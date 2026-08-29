package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/mattn/go-sqlite3"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	psutilnet "github.com/shirou/gopsutil/v3/net"
)

var (
	botToken    = os.Getenv("TELEGRAM_TOKEN")
	adminChatID int64          // primary admin (startup + daily backup destination)
	adminIDs    = map[int64]bool{} // all admins allowed to control the bot
	webUser     = os.Getenv("WEB_USER")
	webPass     = os.Getenv("WEB_PASS")
	xuiDBPath   = os.Getenv("XUI_DB_PATH")
	suiDBPath   = os.Getenv("SUI_DB_PATH")
	xuiPanelURL = strings.TrimRight(os.Getenv("XUI_PANEL_URL"), "/") // e.g. https://IP:32500/basePath
	xuiAPIToken = os.Getenv("XUI_API_TOKEN")
	suiAPIURL   = strings.TrimRight(os.Getenv("SUI_API_URL"), "/") // e.g. https://IP:2096/app
	suiUser     = os.Getenv("SUI_USER")
	suiPass     = os.Getenv("SUI_PASS")
	backupHour  = getEnvInt("BACKUP_HOUR", 3) // daily DB backup hour, UTC
	subBaseURL  = getEnvStr("SUB_BASE_URL", "https://subui.sparkycloud.ir:61111")
	startTime   = time.Now()
)

func getEnvStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// subLink returns the subscription URL for a user (redirects to /sub/<hash>).
func subLink(name string) string {
	return strings.TrimRight(subBaseURL, "/") + "/get_hash/" + url.PathEscape(name)
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// httpClient talks to the local x-ui panel over HTTPS. The panel serves a cert
// for its public domain, but we reach it by IP, so skip verification — traffic
// never leaves the host.
var httpClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// panelMu serializes panel-mutating operations (create/enable/disable/delete/
// renew on x-ui or s-ui) so a reload/restart triggered by one action can't
// overlap with another action's API call to the same panel.
var panelMu sync.Mutex

// doWithRetry sends req and, on a transient network error (not an HTTP-level
// failure), retries once after a short delay. This covers the case where the
// panel is momentarily unreachable because a previous action just restarted
// it via reloadXray/suiRestartCore.
func doWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				req.Body = body
			}
			time.Sleep(2 * time.Second)
		}
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func init() {
	// ADMIN_CHAT_ID may be a comma-separated list. The first is the "primary"
	// (startup notice + daily backups); all listed IDs may control the bot.
	for _, part := range strings.Split(os.Getenv("ADMIN_CHAT_ID"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			log.Fatalf("Invalid ADMIN_CHAT_ID entry %q: %v", part, err)
		}
		adminIDs[id] = true
		if adminChatID == 0 {
			adminChatID = id
		}
	}
	if adminChatID == 0 {
		log.Fatalf("ADMIN_CHAT_ID is required")
	}
	if xuiDBPath == "" {
		xuiDBPath = "/etc/x-ui/x-ui.db"
	}
	if suiDBPath == "" {
		suiDBPath = "/usr/local/s-ui/db/s-ui.db"
	}
}

// --- Metrics ---

type Metrics struct {
	Uptime    string
	CPUPct    float64
	RAMUsed   uint64
	RAMTotal  uint64
	RAMPct    float64
	NetSent   uint64
	NetRecv   uint64
	GoVersion string
	Timestamp string
}

func getMetrics() Metrics {
	cpuPcts, _ := cpu.Percent(500*time.Millisecond, false)
	cpuPct := 0.0
	if len(cpuPcts) > 0 {
		cpuPct = math.Round(cpuPcts[0]*10) / 10
	}
	vmStat, _ := mem.VirtualMemory()
	netStats, _ := psutilnet.IOCounters(false)
	var sent, recv uint64
	if len(netStats) > 0 {
		sent = netStats[0].BytesSent
		recv = netStats[0].BytesRecv
	}
	uptime := time.Since(startTime)
	return Metrics{
		Uptime:    fmt.Sprintf("%dd %dh %dm", int(uptime.Hours())/24, int(uptime.Hours())%24, int(uptime.Minutes())%60),
		CPUPct:    cpuPct,
		RAMUsed:   vmStat.Used / 1024 / 1024,
		RAMTotal:  vmStat.Total / 1024 / 1024,
		RAMPct:    math.Round(vmStat.UsedPercent*10) / 10,
		NetSent:   sent / 1024 / 1024,
		NetRecv:   recv / 1024 / 1024,
		GoVersion: runtime.Version(),
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
	}
}

func formatStatus(m Metrics) string {
	return fmt.Sprintf(
		"📊 *Server Status*\n\n🕐 *Uptime:* `%s`\n🖥 *CPU:* `%.1f%%`\n💾 *RAM:* `%d MB / %d MB (%.1f%%)`\n🌐 *Net ↑:* `%d MB` | *↓:* `%d MB`\n\n🕰 _%s_",
		m.Uptime, m.CPUPct, m.RAMUsed, m.RAMTotal, m.RAMPct, m.NetSent, m.NetRecv, m.Timestamp,
	)
}

// --- Helpers ---

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randPassword(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func placeholders(n int) string {
	s := make([]string, n)
	for i := range s {
		s[i] = "?"
	}
	return strings.Join(s, ",")
}

func int64sToArgs(ids []int64) []interface{} {
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// --- x-ui ---

type XUIUser struct {
	Email     string
	UUID      string
	Password  string
	SubID     string
	Protocol  string
	ExpiryMs  int64
	TotalGB   int64
	InboundID int64
}

// xuiInboundProtocol reads the protocol of an inbound (read-only query).
func xuiInboundProtocol(inboundID int64) string {
	db, err := sql.Open("sqlite3", xuiDBPath+"?_busy_timeout=5000")
	if err != nil {
		return ""
	}
	defer db.Close()
	var proto string
	db.QueryRow(`SELECT protocol FROM inbounds WHERE id=?`, inboundID).Scan(&proto)
	return proto
}

// addXUIUser creates a client through the x-ui panel HTTP API using a Bearer
// API token. The panel handles everything the DB needs (settings JSON,
// client_traffics, and the clients/client_inbounds reconciliation via
// SyncInbound) — replicating that by hand drifts out of sync. We then reload
// xray so the client is served immediately.
func addXUIUser(email string, inboundIDs []int64, days int, gbLimit int64) (*XUIUser, error) {
	if xuiPanelURL == "" || xuiAPIToken == "" {
		return nil, fmt.Errorf("XUI_PANEL_URL / XUI_API_TOKEN not configured")
	}
	if len(inboundIDs) == 0 {
		return nil, fmt.Errorf("no inbound selected")
	}

	uuid := newUUID()
	subID := randHex(8)
	pass := randPassword(16)
	protocol := xuiInboundProtocol(inboundIDs[0])

	var expiryMs int64
	if days > 0 {
		expiryMs = time.Now().Add(time.Duration(days) * 24 * time.Hour).UnixMilli()
	}
	var totalBytes int64
	if gbLimit > 0 {
		totalBytes = gbLimit * 1024 * 1024 * 1024
	}

	// Set id, password and auth so validation passes regardless of protocol
	// (vless/vmess use id; trojan/shadowsocks use password; hysteria uses auth).
	client := map[string]any{
		"id":         uuid,
		"password":   pass,
		"auth":       pass,
		"email":      email,
		"enable":     true,
		"totalGB":    totalBytes,
		"expiryTime": expiryMs,
		"subId":      subID,
		"tgId":       0,
		"limitIp":    0,
		"reset":      0,
		"security":   "auto",
		"flow":       "",
	}
	payload := map[string]any{
		"client":     client,
		"inboundIds": inboundIDs,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", xuiPanelURL+"/panel/api/clients/add", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+xuiAPIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := doWithRetry(httpClient, req)
	if err != nil {
		return nil, fmt.Errorf("x-ui API request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var r struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	json.Unmarshal(respBody, &r)
	if !r.Success {
		return nil, fmt.Errorf("x-ui API (%d): %s", resp.StatusCode, strings.TrimSpace(r.Msg+" "+string(respBody)))
	}

	return &XUIUser{
		Email: email, UUID: uuid, Password: pass, SubID: subID,
		Protocol: protocol, ExpiryMs: expiryMs, TotalGB: gbLimit, InboundID: inboundIDs[0],
	}, nil
}

func reloadXray() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/local/bin/xui-reload.sh").CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("xui-reload: timed out after 30s — %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		return fmt.Errorf("xui-reload: %w — %s", err, string(out))
	}
	log.Printf("xray reloaded: %s", strings.TrimSpace(string(out)))
	return nil
}

type Inbound struct {
	ID       int64
	Remark   string
	Port     int
	Protocol string
}

func listXUIInbounds() ([]Inbound, error) {
	db, err := sql.Open("sqlite3", xuiDBPath+"?_busy_timeout=5000&cache=shared")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, remark, port, protocol FROM inbounds WHERE enable=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Inbound
	for rows.Next() {
		var ib Inbound
		rows.Scan(&ib.ID, &ib.Remark, &ib.Port, &ib.Protocol)
		out = append(out, ib)
	}
	return out, nil
}

func listXUIUsers() (string, error) {
	db, err := sql.Open("sqlite3", xuiDBPath+"?_busy_timeout=5000&cache=shared")
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT c.email, c.enable, c.expiry_time, c.total_gb,
		       COALESCE(ct.up+ct.down,0), i.remark
		FROM clients c
		LEFT JOIN client_inbounds ci ON ci.client_id=c.id
		LEFT JOIN inbounds i ON i.id=ci.inbound_id
		LEFT JOIN client_traffics ct ON ct.email=c.email
		ORDER BY c.id DESC LIMIT 20`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	sb.WriteString("👥 *x-ui Users (last 20)*\n\n")
	for rows.Next() {
		var email, remark string
		var enable int
		var expiryMs, totalBytes, usedBytes int64
		rows.Scan(&email, &enable, &expiryMs, &totalBytes, &usedBytes, &remark)
		status := "✅"
		if enable == 0 {
			status = "🔴"
		}
		expStr := "never"
		if expiryMs > 0 {
			exp := time.UnixMilli(expiryMs)
			if exp.Before(time.Now()) {
				expStr = "expired"
				status = "⏰"
			} else {
				expStr = exp.Format("2006-01-02")
			}
		}
		totalStr := "∞"
		if totalBytes > 0 {
			totalStr = fmt.Sprintf("%.0f GB", float64(totalBytes)/1e9)
		}
		sb.WriteString(fmt.Sprintf("%s `%s` — %s | used: `%.2f/%s` | exp: `%s`\n",
			status, email, remark, float64(usedBytes)/1e9, totalStr, expStr))
	}
	return sb.String(), nil
}

// --- s-ui ---

type SUIInbound struct {
	ID   int64
	Tag  string
	Type string
}

func listSUIInbounds() ([]SUIInbound, error) {
	db, err := sql.Open("sqlite3", suiDBPath+"?_busy_timeout=5000&cache=shared")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, tag, type FROM inbounds ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SUIInbound
	for rows.Next() {
		var ib SUIInbound
		rows.Scan(&ib.ID, &ib.Tag, &ib.Type)
		out = append(out, ib)
	}
	return out, nil
}

type SUIUser struct {
	Name     string
	Password string
	UUID     string
	Expiry   int64
	VolumeGB int64
}

func addSUIUser(name string, inboundIDs []int64, days int, gbLimit int64) (*SUIUser, error) {
	// s-ui runs in WAL mode; the whole db directory is bind-mounted so the
	// -wal/-shm sidecars are shared with the s-ui process. Just set a busy
	// timeout so we wait for s-ui's write lock instead of failing instantly.
	// Do NOT force journal_mode here — that would race with s-ui.
	db, err := sql.Open("sqlite3", suiDBPath+"?_busy_timeout=15000")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	uuid := newUUID()
	password := randPassword(12)
	var expiry int64
	if days > 0 {
		expiry = time.Now().Unix() + int64(days)*86400
	}
	var volumeBytes int64
	if gbLimit > 0 {
		volumeBytes = gbLimit * 1024 * 1024 * 1024
	}

	inboundsJSON, _ := json.Marshal(inboundIDs)

	// Build links from inbound server settings
	type linkEntry struct {
		Remark string `json:"remark"`
		Type   string `json:"type"`
		URI    string `json:"uri"`
	}
	var links []linkEntry
	var domain string
	db.QueryRow(`SELECT value FROM settings WHERE key='webDomain'`).Scan(&domain)

	ibRows, err := db.Query(`SELECT id, tag, type, options FROM inbounds WHERE id IN (`+placeholders(len(inboundIDs))+`)`, int64sToArgs(inboundIDs)...)
	if err == nil {
		defer ibRows.Close()
		for ibRows.Next() {
			var ibID int64
			var tag, ibType string
			var optBlob []byte
			ibRows.Scan(&ibID, &tag, &ibType, &optBlob)
			var ibCfg map[string]interface{}
			json.Unmarshal(optBlob, &ibCfg)
			port := 0
			if p, ok := ibCfg["listen_port"].(float64); ok {
				port = int(p)
			}
			var uri string
			switch ibType {
			case "hysteria2":
				uri = fmt.Sprintf("hysteria2://%s@%s:%d#%s-%s", password, domain, port, tag, name)
			case "vless":
				uri = fmt.Sprintf("vless://%s@%s:%d#%s-%s", uuid, domain, port, tag, name)
			case "anytls":
				uri = fmt.Sprintf("anytls://%s@%s:%d#%s-%s", password, domain, port, tag, name)
			default:
				uri = fmt.Sprintf("%s://%s@%s:%d#%s-%s", ibType, password, domain, port, tag, name)
			}
			links = append(links, linkEntry{Remark: tag, Type: "local", URI: uri})
		}
	}

	linksJSON, _ := json.Marshal(links)
	configJSON := fmt.Sprintf(`{"mixed":{"username":%q,"password":%q},"socks":{"username":%q,"password":%q},"http":{"username":%q,"password":%q},"vmess":{"name":%q,"uuid":%q,"alterId":0},"vless":{"name":%q,"uuid":%q,"flow":"xtls-rprx-vision"},"anytls":{"name":%q,"password":%q},"trojan":{"name":%q,"password":%q},"hysteria":{"name":%q,"auth_str":%q},"hysteria2":{"name":%q,"password":%q},"tuic":{"name":%q,"uuid":%q,"password":%q}}`,
		name, password, name, password, name, password,
		name, uuid, name, uuid,
		name, password, name, password,
		name, password, name, password,
		name, uuid, password)

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO clients
		(enable,name,config,inbounds,links,volume,expiry,down,up,desc,"group",
		 delay_start,auto_reset,reset_days,next_reset,total_up,total_down,remark,created_at,online_at)
		VALUES (1,?,CAST(? AS BLOB),CAST(? AS BLOB),CAST(? AS BLOB),?,?,0,0,'','',0,0,0,0,0,0,'',cast(strftime('%s','now') as integer),0)`,
		name, configJSON, string(inboundsJSON), string(linksJSON), volumeBytes, expiry)
	if err != nil {
		return nil, fmt.Errorf("insert s-ui client: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil, fmt.Errorf("insert affected 0 rows — DB may be locked by s-ui")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	log.Printf("s-ui user created: %s (rows affected: %d)", name, affected)
	return &SUIUser{Name: name, Password: password, UUID: uuid, Expiry: expiry, VolumeGB: gbLimit}, nil
}

// --- User management (list / enable / disable / delete across both panels) ---

type AggUser struct {
	Name       string
	InXUI      bool
	InSUI      bool
	XUIEnabled bool
	SUIEnabled bool
	Used       int64 // combined up+down bytes
}

func (u AggUser) enabledAnywhere() bool {
	return (u.InXUI && u.XUIEnabled) || (u.InSUI && u.SUIEnabled)
}

// listAllUsers aggregates unique users across both panels (x-ui email / s-ui name),
// mirroring how sub.python gathers them.
func listAllUsers() ([]AggUser, error) {
	m := map[string]*AggUser{}
	var order []string
	get := func(name string) *AggUser {
		if u, ok := m[name]; ok {
			return u
		}
		u := &AggUser{Name: name}
		m[name] = u
		order = append(order, name)
		return u
	}

	if xdb, err := sql.Open("sqlite3", xuiDBPath+"?_busy_timeout=5000"); err == nil {
		defer xdb.Close()
		rows, qerr := xdb.Query(`SELECT c.email, c.enable, COALESCE(SUM(ct.up+ct.down),0)
			FROM clients c LEFT JOIN client_traffics ct ON ct.email=c.email GROUP BY c.email`)
		if qerr == nil {
			for rows.Next() {
				var email string
				var en int
				var used int64
				rows.Scan(&email, &en, &used)
				if strings.TrimSpace(email) == "" {
					continue
				}
				u := get(email)
				u.InXUI = true
				u.XUIEnabled = en == 1
				u.Used += used
			}
			rows.Close()
		}
	}

	if sdb, err := sql.Open("sqlite3", suiDBPath+"?_busy_timeout=5000"); err == nil {
		defer sdb.Close()
		rows, qerr := sdb.Query(`SELECT name, enable, up+down FROM clients`)
		if qerr == nil {
			for rows.Next() {
				var name string
				var en int
				var used int64
				rows.Scan(&name, &en, &used)
				if strings.TrimSpace(name) == "" {
					continue
				}
				u := get(name)
				u.InSUI = true
				u.SUIEnabled = en == 1
				u.Used += used
			}
			rows.Close()
		}
	}

	sort.Slice(order, func(i, j int) bool { return strings.ToLower(order[i]) < strings.ToLower(order[j]) })
	out := make([]AggUser, 0, len(order))
	for _, n := range order {
		out = append(out, *m[n])
	}
	return out, nil
}

func findUser(name string) (*AggUser, error) {
	users, err := listAllUsers()
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].Name == name {
			return &users[i], nil
		}
	}
	return nil, fmt.Errorf("user not found")
}

// --- x-ui API actions ---

func xuiAPIPost(path string, body any) error {
	if xuiPanelURL == "" || xuiAPIToken == "" {
		return fmt.Errorf("x-ui API not configured")
	}
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest("POST", xuiPanelURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+xuiAPIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := doWithRetry(httpClient, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var r struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	json.Unmarshal(rb, &r)
	if !r.Success {
		return fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(r.Msg))
	}
	return nil
}

func xuiSetEnable(email string, enable bool) error {
	action := "/panel/api/clients/bulkDisable"
	if enable {
		action = "/panel/api/clients/bulkEnable"
	}
	return xuiAPIPost(action, map[string]any{"emails": []string{email}})
}

func xuiDelete(email string) error {
	return xuiAPIPost("/panel/api/clients/del/"+url.PathEscape(email), nil)
}

// --- s-ui API (login + graceful core reload) ---

// suiLoggedInClient logs into the s-ui panel and returns a cookie-bearing client.
func suiLoggedInClient() (*http.Client, error) {
	if suiAPIURL == "" || suiUser == "" || suiPass == "" {
		return nil, fmt.Errorf("s-ui credentials not set (SUI_API_URL/SUI_USER/SUI_PASS)")
	}
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{
		Timeout:   20 * time.Second,
		Jar:       jar,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := cl.PostForm(suiAPIURL+"/api/login", url.Values{"user": {suiUser}, "pass": {suiPass}})
	if err != nil {
		return nil, fmt.Errorf("s-ui login: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var r struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	json.Unmarshal(rb, &r)
	if !r.Success {
		return nil, fmt.Errorf("s-ui login failed: %s", strings.TrimSpace(r.Msg))
	}
	return cl, nil
}

// suiRestartCore triggers s-ui's graceful sing-box core reload (NOT a service restart).
func suiRestartCore() error {
	cl, err := suiLoggedInClient()
	if err != nil {
		return err
	}
	resp, err := cl.Post(suiAPIURL+"/api/restartSb", "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return nil
}

func suiSetEnable(name string, enable bool) error {
	db, err := sql.Open("sqlite3", suiDBPath+"?_busy_timeout=15000")
	if err != nil {
		return err
	}
	defer db.Close()
	en := 0
	if enable {
		en = 1
	}
	if _, err := db.Exec(`UPDATE clients SET enable=? WHERE name=?`, en, name); err != nil {
		return err
	}
	return suiRestartCore()
}

func suiDelete(name string) error {
	db, err := sql.Open("sqlite3", suiDBPath+"?_busy_timeout=15000")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM clients WHERE name=?`, name); err != nil {
		return err
	}
	return suiRestartCore()
}

// xuiRenew resets usage and sets a fresh absolute expiry (now + days; 0 = never).
// It re-sends the client's current fields via the update API so nothing else
// changes, then zeroes traffic (which also re-enables).
func xuiRenew(name string, days int) error {
	db, err := sql.Open("sqlite3", xuiDBPath+"?_busy_timeout=5000")
	if err != nil {
		return err
	}
	var uuid, subID, flow, security, comment, password, auth sql.NullString
	var limitIP, totalGB, tgID, reset int64
	err = db.QueryRow(`SELECT uuid, sub_id, flow, security, comment, password, auth, limit_ip, total_gb, tg_id, reset
		FROM clients WHERE email=?`, name).
		Scan(&uuid, &subID, &flow, &security, &comment, &password, &auth, &limitIP, &totalGB, &tgID, &reset)
	db.Close()
	if err != nil {
		return fmt.Errorf("read client: %w", err)
	}

	var expiryMs int64
	if days > 0 {
		expiryMs = time.Now().Add(time.Duration(days) * 24 * time.Hour).UnixMilli()
	}
	sec := security.String
	if sec == "" {
		sec = "auto"
	}
	client := map[string]any{
		"id": uuid.String, "password": password.String, "auth": auth.String,
		"email": name, "subId": subID.String, "flow": flow.String, "security": sec,
		"comment": comment.String, "limitIp": limitIP, "totalGB": totalGB,
		"expiryTime": expiryMs, "tgId": tgID, "reset": reset, "enable": true,
	}
	if err := xuiAPIPost("/panel/api/clients/update/"+url.PathEscape(name), client); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if err := xuiAPIPost("/panel/api/clients/resetTraffic/"+url.PathEscape(name), nil); err != nil {
		return fmt.Errorf("resetTraffic: %w", err)
	}
	return nil
}

// suiRenew resets usage and sets a fresh absolute expiry (now + days; 0 = never).
func suiRenew(name string, days int) error {
	db, err := sql.Open("sqlite3", suiDBPath+"?_busy_timeout=15000")
	if err != nil {
		return err
	}
	defer db.Close()
	var expiry int64
	if days > 0 {
		expiry = time.Now().Unix() + int64(days)*86400
	}
	if _, err := db.Exec(`UPDATE clients SET up=0, down=0, expiry=?, enable=1 WHERE name=?`, expiry, name); err != nil {
		return err
	}
	return suiRestartCore()
}

// --- Wizard state ---

const (
	panelXUI  = "xui"
	panelSUI  = "sui"
	panelBoth = "both"
)

type wizardState struct {
	panel  string
	name   string
	days   int
	gb     int64
	xuiIDs []int64 // selected x-ui inbound ids
	suiIDs []int64 // selected s-ui inbound ids
	step   string
}

func toggleID(list []int64, id int64) []int64 {
	for i, v := range list {
		if v == id {
			return append(list[:i], list[i+1:]...)
		}
	}
	return append(list, id)
}

var sessions = map[int64]*wizardState{}

// renewPending maps a chat to the username awaiting a renewal-days reply.
var renewPending = map[int64]string{}

// --- Bot ---

func runBot(ctx context.Context) {
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("Bot init error: %v", err)
	}
	log.Printf("Bot authorized as @%s", bot.Self.UserName)

	go scheduleDailyBackup(bot)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := bot.GetUpdatesChan(u)

	hello := tgbotapi.NewMessage(adminChatID, "✅ *Monitoring bot started.*\nSend /help to see commands.")
	hello.ParseMode = "Markdown"
	bot.Send(hello)

	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Message != nil {
				if !adminIDs[update.Message.Chat.ID] {
					bot.Send(tgbotapi.NewMessage(update.Message.Chat.ID, "⛔ Unauthorized."))
					continue
				}
				handleMessage(bot, update.Message)
			}
			if update.CallbackQuery != nil {
				if !adminIDs[update.CallbackQuery.Message.Chat.ID] {
					continue
				}
				handleCallback(bot, update.CallbackQuery)
			}
		}
	}
}

func send(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	bot.Send(msg)
}

func sendKB(bot *tgbotapi.BotAPI, chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = kb
	bot.Send(msg)
}

func editKB(bot *tgbotapi.BotAPI, chatID int64, msgID int, text string, kb tgbotapi.InlineKeyboardMarkup) {
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, msgID, text, kb)
	edit.ParseMode = "Markdown"
	bot.Send(edit)
}

func editText(bot *tgbotapi.BotAPI, chatID int64, msgID int, text string) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ParseMode = "Markdown"
	bot.Send(edit)
}

func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	// Awaiting renewal days for a user?
	if name, ok := renewPending[chatID]; ok && !strings.HasPrefix(text, "/") {
		delete(renewPending, chatID)
		days, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || days < 0 {
			send(bot, chatID, "❌ Please send a valid number of days (0 = never). Renew cancelled.")
			return
		}
		send(bot, chatID, "⏳ Renewing...")
		go doUserRenew(bot, chatID, name, days)
		return
	}

	if w, ok := sessions[chatID]; ok && !strings.HasPrefix(text, "/") {
		handleWizardText(bot, chatID, w, text)
		return
	}
	delete(sessions, chatID)

	m := getMetrics()
	switch strings.ToLower(msg.Command()) {
	case "start", "help":
		send(bot, chatID,
			"🤖 *Server Monitor Bot*\n\n"+
				"*Monitoring*\n"+
				"/status — full status\n/cpu — CPU\n/ram — RAM\n/uptime — uptime\n/net — network\n\n"+
				"*User Management*\n"+
				"/adduser — add VPN user (with buttons)\n"+
				"/users — list & manage users\n"+
				"/inbounds — list x-ui inbounds\n\n"+
				"*Backup*\n"+
				fmt.Sprintf("/backup — send x-ui + s-ui DB now\n_(auto daily at %02d:00 UTC)_", backupHour))
	case "status":
		send(bot, chatID, formatStatus(m))
	case "cpu":
		send(bot, chatID, fmt.Sprintf("🖥 *CPU:* `%.1f%%`", m.CPUPct))
	case "ram":
		send(bot, chatID, fmt.Sprintf("💾 *RAM:* `%d/%d MB (%.1f%%)`", m.RAMUsed, m.RAMTotal, m.RAMPct))
	case "uptime":
		send(bot, chatID, fmt.Sprintf("🕐 *Uptime:* `%s`", m.Uptime))
	case "net":
		send(bot, chatID, fmt.Sprintf("🌐 *Network*\n↑ `%d MB`\n↓ `%d MB`", m.NetSent, m.NetRecv))
	case "inbounds":
		ibs, err := listXUIInbounds()
		if err != nil {
			send(bot, chatID, "❌ "+err.Error())
			return
		}
		var sb strings.Builder
		sb.WriteString("📡 *x-ui Inbounds*\n\n")
		for _, ib := range ibs {
			sb.WriteString(fmt.Sprintf("ID `%d` — *%s* (%s) :`%d`\n", ib.ID, ib.Remark, ib.Protocol, ib.Port))
		}
		send(bot, chatID, sb.String())
	case "users":
		showUsersList(bot, chatID, 0, 0)
	case "adduser":
		startWizard(bot, chatID)
	case "backup":
		go sendBackups(bot, chatID, "Manual")
	default:
		send(bot, chatID, "❓ Unknown command. Send /help")
	}
}

// --- Wizard ---

func startWizard(bot *tgbotapi.BotAPI, chatID int64) {
	sessions[chatID] = &wizardState{}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🇽 x-ui (Xray)", "panel:xui"),
			tgbotapi.NewInlineKeyboardButtonData("🇸 s-ui (sing-box)", "panel:sui"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔀 Both panels", "panel:both"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel"),
		),
	)
	sendKB(bot, chatID, "👤 *Add New VPN User*\n\nStep 1 — Choose the panel:", kb)
}

func handleCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID
	data := cb.Data

	bot.Request(tgbotapi.NewCallback(cb.ID, ""))

	if data == "cancel" {
		delete(sessions, chatID)
		editText(bot, chatID, msgID, "❌ Cancelled.")
		return
	}

	// --- User management callbacks (stateless, no wizard session needed) ---
	switch {
	case data == "noop":
		return
	case strings.HasPrefix(data, "users:page:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(data, "users:page:"))
		showUsersList(bot, chatID, msgID, page)
		return
	case strings.HasPrefix(data, "usr:"):
		showUserManage(bot, chatID, msgID, strings.TrimPrefix(data, "usr:"))
		return
	case strings.HasPrefix(data, "uact:delask:"):
		showDeleteConfirm(bot, chatID, msgID, strings.TrimPrefix(data, "uact:delask:"))
		return
	case strings.HasPrefix(data, "uact:enable:"):
		editText(bot, chatID, msgID, "⏳ Enabling...")
		go doUserAction(bot, chatID, msgID, "enable", strings.TrimPrefix(data, "uact:enable:"))
		return
	case strings.HasPrefix(data, "uact:disable:"):
		editText(bot, chatID, msgID, "⏳ Disabling...")
		go doUserAction(bot, chatID, msgID, "disable", strings.TrimPrefix(data, "uact:disable:"))
		return
	case strings.HasPrefix(data, "uact:delete:"):
		editText(bot, chatID, msgID, "⏳ Deleting...")
		go doUserAction(bot, chatID, msgID, "delete", strings.TrimPrefix(data, "uact:delete:"))
		return
	case strings.HasPrefix(data, "uact:renew:"):
		name := strings.TrimPrefix(data, "uact:renew:")
		delete(sessions, chatID) // avoid wizard collision
		renewPending[chatID] = name
		editText(bot, chatID, msgID, fmt.Sprintf("🔄 Renew *%s*\n\nSend the number of *days* for the new period (0 = never expire):", name))
		return
	}

	w, ok := sessions[chatID]
	if !ok {
		bot.Send(tgbotapi.NewMessage(chatID, "Session expired. Use /adduser to start again."))
		return
	}

	switch {
	case strings.HasPrefix(data, "panel:"):
		w.panel = strings.TrimPrefix(data, "panel:")
		w.step = "name"
		editText(bot, chatID, msgID,
			fmt.Sprintf("✅ Panel: *%s*\n\nStep 2 — Type the *username* for the new user:", strings.ToUpper(w.panel)))

	case strings.HasPrefix(data, "traffic:"):
		val := strings.TrimPrefix(data, "traffic:")
		if val == "custom" {
			w.step = "traffic_custom"
			editText(bot, chatID, msgID, "📦 Type the traffic limit in *GB* (or `0` for unlimited):")
			return
		}
		gb, _ := strconv.ParseInt(val, 10, 64)
		w.gb = gb
		showExpiryKB(bot, chatID, msgID, w)

	case strings.HasPrefix(data, "expiry:"):
		val := strings.TrimPrefix(data, "expiry:")
		if val == "custom" {
			w.step = "expiry_custom"
			editText(bot, chatID, msgID, "📅 Type the number of *days* until expiry (or `0` for never):")
			return
		}
		days, _ := strconv.Atoi(val)
		w.days = days
		showInboundKB(bot, chatID, msgID, w)

	case data == "ib:done":
		if len(w.xuiIDs)+len(w.suiIDs) == 0 {
			bot.Request(tgbotapi.NewCallback(cb.ID, "⚠️ Select at least one inbound"))
			return
		}
		showConfirmKB(bot, chatID, msgID, w)

	case strings.HasPrefix(data, "ibx:"):
		// x-ui inbound toggle
		ibID, _ := strconv.ParseInt(strings.TrimPrefix(data, "ibx:"), 10, 64)
		w.xuiIDs = toggleID(w.xuiIDs, ibID)
		showInboundKB(bot, chatID, msgID, w)

	case strings.HasPrefix(data, "ibs:"):
		// s-ui inbound toggle
		ibID, _ := strconv.ParseInt(strings.TrimPrefix(data, "ibs:"), 10, 64)
		w.suiIDs = toggleID(w.suiIDs, ibID)
		showInboundKB(bot, chatID, msgID, w)

	case data == "confirm":
		delete(sessions, chatID)
		editText(bot, chatID, msgID, "⏳ Creating user...")
		go createUser(bot, chatID, w)
	}
}

func handleWizardText(bot *tgbotapi.BotAPI, chatID int64, w *wizardState, text string) {
	switch w.step {
	case "name":
		name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(text), " ", "_"))
		if name == "" {
			send(bot, chatID, "❌ Name cannot be empty.")
			return
		}
		w.name = name
		showTrafficKB(bot, chatID, 0, w)

	case "traffic_custom":
		gb, err := strconv.ParseInt(text, 10, 64)
		if err != nil || gb < 0 {
			send(bot, chatID, "❌ Enter a valid number.")
			return
		}
		w.gb = gb
		showExpiryKB(bot, chatID, 0, w)

	case "expiry_custom":
		days, err := strconv.Atoi(text)
		if err != nil || days < 0 {
			send(bot, chatID, "❌ Enter a valid number.")
			return
		}
		w.days = days
		showInboundKB(bot, chatID, 0, w)
	}
}

func summary(w *wizardState) string {
	gbStr := "∞ Unlimited"
	if w.gb > 0 {
		gbStr = fmt.Sprintf("%d GB", w.gb)
	}
	expStr := "Never"
	if w.days > 0 {
		expStr = fmt.Sprintf("%d days", w.days)
	}
	return fmt.Sprintf("👤 `%s` | 📦 `%s` | 📅 `%s`", w.name, gbStr, expStr)
}

func showTrafficKB(bot *tgbotapi.BotAPI, chatID int64, msgID int, w *wizardState) {
	text := fmt.Sprintf("👤 User: `%s`\n\nStep 3 — Choose *traffic limit*:", w.name)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("5 GB", "traffic:5"),
			tgbotapi.NewInlineKeyboardButtonData("10 GB", "traffic:10"),
			tgbotapi.NewInlineKeyboardButtonData("20 GB", "traffic:20"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("50 GB", "traffic:50"),
			tgbotapi.NewInlineKeyboardButtonData("100 GB", "traffic:100"),
			tgbotapi.NewInlineKeyboardButtonData("∞ Unlimited", "traffic:0"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Custom", "traffic:custom"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel"),
		),
	)
	if msgID != 0 {
		editKB(bot, chatID, msgID, text, kb)
	} else {
		sendKB(bot, chatID, text, kb)
	}
}

func showExpiryKB(bot *tgbotapi.BotAPI, chatID int64, msgID int, w *wizardState) {
	text := summary(w) + "\n\nStep 4 — Choose *expiry*:"
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("7 days", "expiry:7"),
			tgbotapi.NewInlineKeyboardButtonData("14 days", "expiry:14"),
			tgbotapi.NewInlineKeyboardButtonData("30 days", "expiry:30"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("60 days", "expiry:60"),
			tgbotapi.NewInlineKeyboardButtonData("90 days", "expiry:90"),
			tgbotapi.NewInlineKeyboardButtonData("∞ Never", "expiry:0"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✏️ Custom", "expiry:custom"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel"),
		),
	)
	if msgID != 0 {
		editKB(bot, chatID, msgID, text, kb)
	} else {
		sendKB(bot, chatID, text, kb)
	}
}

func showInboundKB(bot *tgbotapi.BotAPI, chatID int64, msgID int, w *wizardState) {
	var rows [][]tgbotapi.InlineKeyboardButton

	xuiSel := map[int64]bool{}
	for _, id := range w.xuiIDs {
		xuiSel[id] = true
	}
	suiSel := map[int64]bool{}
	for _, id := range w.suiIDs {
		suiSel[id] = true
	}
	mark := func(on bool) string {
		if on {
			return "✅"
		}
		return "☐"
	}
	text := summary(w) + "\n\nStep 5 — Select *inbounds* (tap to toggle, then Done):"

	// x-ui inbounds (for xui and both)
	if w.panel == panelXUI || w.panel == panelBoth {
		ibs, err := listXUIInbounds()
		if err != nil || len(ibs) == 0 {
			if w.panel == panelXUI {
				send(bot, chatID, "❌ No x-ui inbounds found.")
				return
			}
		}
		for _, ib := range ibs {
			label := fmt.Sprintf("%s 🇽 %s (%s :%d)", mark(xuiSel[ib.ID]), ib.Remark, ib.Protocol, ib.Port)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("ibx:%d", ib.ID)),
			))
		}
	}

	// s-ui inbounds (for sui and both)
	if w.panel == panelSUI || w.panel == panelBoth {
		ibs, err := listSUIInbounds()
		if err != nil || len(ibs) == 0 {
			if w.panel == panelSUI {
				send(bot, chatID, "❌ No s-ui inbounds found.")
				return
			}
		}
		for _, ib := range ibs {
			label := fmt.Sprintf("%s 🇸 %s (%s)", mark(suiSel[ib.ID]), ib.Tag, ib.Type)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("ibs:%d", ib.ID)),
			))
		}
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("✔️ Done — Confirm", "ib:done"),
		tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel"),
	))
	kb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	if msgID != 0 {
		editKB(bot, chatID, msgID, text, kb)
	} else {
		sendKB(bot, chatID, text, kb)
	}
}

func showConfirmKB(bot *tgbotapi.BotAPI, chatID int64, msgID int, w *wizardState) {
	gbStr := "∞ Unlimited"
	if w.gb > 0 {
		gbStr = fmt.Sprintf("%d GB", w.gb)
	}
	expStr := "Never"
	if w.days > 0 {
		expStr = fmt.Sprintf("%d days", w.days)
	}

	ibLines := ""
	if w.panel == panelXUI || w.panel == panelBoth {
		ibLines += fmt.Sprintf("🇽 x-ui inbounds: `%v`\n", w.xuiIDs)
	}
	if w.panel == panelSUI || w.panel == panelBoth {
		ibLines += fmt.Sprintf("🇸 s-ui inbounds: `%v`\n", w.suiIDs)
	}

	text := fmt.Sprintf(
		"📋 *Confirm New User*\n\n"+
			"🔹 Panel: `%s`\n"+
			"👤 Name: `%s`\n"+
			"📦 Traffic: `%s`\n"+
			"📅 Expiry: `%s`\n"+
			"%s\n"+
			"Create this user?",
		strings.ToUpper(w.panel), w.name, gbStr, expStr, ibLines,
	)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Confirm & Create", "confirm"),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel"),
		),
	)
	if msgID != 0 {
		editKB(bot, chatID, msgID, text, kb)
	} else {
		sendKB(bot, chatID, text, kb)
	}
}

func limitStr(gb int64) string {
	if gb > 0 {
		return fmt.Sprintf("%d GB", gb)
	}
	return "Unlimited"
}

// createXUIPart creates the client in x-ui and returns a result block for the
// summary message (or an error line).
func createXUIPart(w *wizardState) string {
	user, err := addXUIUser(w.name, w.xuiIDs, w.days, w.gb)
	if err != nil {
		return "❌ *x-ui:* `" + err.Error() + "`"
	}
	if err := reloadXray(); err != nil {
		log.Printf("reloadXray: %v", err)
	}
	expStr := "Never"
	if user.ExpiryMs > 0 {
		expStr = time.UnixMilli(user.ExpiryMs).Format("2006-01-02")
	}
	credLine := fmt.Sprintf("🔑 UUID: `%s`", user.UUID)
	switch user.Protocol {
	case "trojan", "shadowsocks":
		credLine = fmt.Sprintf("🔒 Password: `%s`", user.Password)
	case "hysteria", "hysteria2":
		credLine = fmt.Sprintf("🔒 Auth: `%s`", user.Password)
	}
	return fmt.Sprintf(
		"✅ *🇽 x-ui*\n%s\n📋 SubID: `%s`\n📦 Limit: `%s`\n📅 Expires: `%s`",
		credLine, user.SubID, limitStr(user.TotalGB), expStr,
	)
}

// createSUIPart creates the client in s-ui and returns a result block.
func createSUIPart(w *wizardState) string {
	user, err := addSUIUser(w.name, w.suiIDs, w.days, w.gb)
	if err != nil {
		return "❌ *s-ui:* `" + err.Error() + "`"
	}
	expStr := "Never"
	if user.Expiry > 0 {
		expStr = time.Unix(user.Expiry, 0).Format("2006-01-02")
	}
	return fmt.Sprintf(
		"✅ *🇸 s-ui*\n🔑 UUID: `%s`\n🔒 Password: `%s`\n📦 Limit: `%s`\n📅 Expires: `%s`",
		user.UUID, user.Password, limitStr(user.VolumeGB), expStr,
	)
}

func createUser(bot *tgbotapi.BotAPI, chatID int64, w *wizardState) {
	panelMu.Lock()
	defer panelMu.Unlock()
	var parts []string
	if (w.panel == panelXUI || w.panel == panelBoth) && len(w.xuiIDs) > 0 {
		parts = append(parts, createXUIPart(w))
	}
	if (w.panel == panelSUI || w.panel == panelBoth) && len(w.suiIDs) > 0 {
		parts = append(parts, createSUIPart(w))
	}
	if len(parts) == 0 {
		send(bot, chatID, "❌ No inbounds were selected.")
		return
	}
	header := fmt.Sprintf("👤 *User `%s` created*\n\n", w.name)
	footer := fmt.Sprintf("\n\n🔗 *Sub link:*\n`%s`", subLink(w.name))
	send(bot, chatID, header+strings.Join(parts, "\n\n")+footer)
}

// --- User management UI ---

const usersPerPage = 12 // 6 rows × 2 columns

func gb(bytes int64) string {
	return fmt.Sprintf("%.2f GB", float64(bytes)/1e9)
}

func showUsersList(bot *tgbotapi.BotAPI, chatID int64, msgID, page int) {
	users, err := listAllUsers()
	if err != nil {
		send(bot, chatID, "❌ "+err.Error())
		return
	}
	if len(users) == 0 {
		send(bot, chatID, "No users found.")
		return
	}
	pages := (len(users) + usersPerPage - 1) / usersPerPage
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * usersPerPage
	end := start + usersPerPage
	if end > len(users) {
		end = len(users)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	var curRow []tgbotapi.InlineKeyboardButton
	for _, u := range users[start:end] {
		dot := "🟢"
		if !u.enabledAnywhere() {
			dot = "🔴"
		}
		tag := ""
		if u.InXUI {
			tag += "🇽"
		}
		if u.InSUI {
			tag += "🇸"
		}
		label := fmt.Sprintf("%s%s %s", dot, tag, u.Name)
		curRow = append(curRow, tgbotapi.NewInlineKeyboardButtonData(label, "usr:"+u.Name))
		if len(curRow) == 2 {
			rows = append(rows, curRow)
			curRow = nil
		}
	}
	if len(curRow) > 0 {
		rows = append(rows, curRow)
	}
	var nav []tgbotapi.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("users:page:%d", page-1)))
	}
	nav = append(nav, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d/%d", page+1, pages), "noop"))
	if page < pages-1 {
		nav = append(nav, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("users:page:%d", page+1)))
	}
	rows = append(rows, nav)

	text := fmt.Sprintf("👥 *Users* — %d total\n🇽 x-ui · 🇸 s-ui · 🟢 active · 🔴 disabled\n\nTap a user to manage:", len(users))
	kb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	if msgID != 0 {
		editKB(bot, chatID, msgID, text, kb)
	} else {
		sendKB(bot, chatID, text, kb)
	}
}

func showUserManage(bot *tgbotapi.BotAPI, chatID int64, msgID int, name string) {
	u, err := findUser(name)
	if err != nil {
		editText(bot, chatID, msgID, "❌ "+err.Error())
		return
	}
	panelLine := func(in, en bool) string {
		if !in {
			return "—"
		}
		if en {
			return "🟢 active"
		}
		return "🔴 disabled"
	}
	text := fmt.Sprintf(
		"👤 *%s*\n\n"+
			"🇽 x-ui: %s\n"+
			"🇸 s-ui: %s\n"+
			"📊 Used: `%s`\n"+
			"🔗 Sub: `%s`\n\nChoose an action:",
		u.Name, panelLine(u.InXUI, u.XUIEnabled), panelLine(u.InSUI, u.SUIEnabled), gb(u.Used), subLink(u.Name),
	)

	var rows [][]tgbotapi.InlineKeyboardButton
	if u.enabledAnywhere() {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔴 Disable", "uact:disable:"+name),
		))
	}
	// offer enable if disabled on any panel it belongs to
	if (u.InXUI && !u.XUIEnabled) || (u.InSUI && !u.SUIEnabled) {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🟢 Enable", "uact:enable:"+name),
		))
	}
	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Renew", "uact:renew:"+name),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑 Delete", "uact:delask:"+name),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Back to list", "users:page:0"),
		),
	)
	kb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	if msgID != 0 {
		editKB(bot, chatID, msgID, text, kb)
	} else {
		sendKB(bot, chatID, text, kb)
	}
}

func doUserAction(bot *tgbotapi.BotAPI, chatID int64, msgID int, action, name string) {
	panelMu.Lock()
	defer panelMu.Unlock()
	u, err := findUser(name)
	if err != nil {
		editText(bot, chatID, msgID, "❌ "+err.Error())
		return
	}

	var errs []string
	switch action {
	case "enable", "disable":
		enable := action == "enable"
		if u.InXUI {
			if e := xuiSetEnable(name, enable); e != nil {
				errs = append(errs, "x-ui: "+e.Error())
			} else {
				reloadXray()
			}
		}
		if u.InSUI {
			if e := suiSetEnable(name, enable); e != nil {
				errs = append(errs, "s-ui: "+e.Error())
			}
		}
	case "delete":
		if u.InXUI {
			if e := xuiDelete(name); e != nil {
				errs = append(errs, "x-ui: "+e.Error())
			} else {
				reloadXray()
			}
		}
		if u.InSUI {
			if e := suiDelete(name); e != nil {
				errs = append(errs, "s-ui: "+e.Error())
			}
		}
	}

	if len(errs) > 0 {
		send(bot, chatID, "⚠️ *"+name+"* — some actions failed:\n`"+strings.Join(errs, "\n")+"`")
	}
	if action == "delete" {
		editText(bot, chatID, msgID, fmt.Sprintf("🗑 *%s* deleted.", name))
		return
	}
	// refresh the manage view with new status
	showUserManage(bot, chatID, msgID, name)
}

func doUserRenew(bot *tgbotapi.BotAPI, chatID int64, name string, days int) {
	panelMu.Lock()
	defer panelMu.Unlock()
	u, err := findUser(name)
	if err != nil {
		send(bot, chatID, "❌ "+err.Error())
		return
	}
	var errs []string
	if u.InXUI {
		if e := xuiRenew(name, days); e != nil {
			errs = append(errs, "x-ui: "+e.Error())
		} else {
			reloadXray()
		}
	}
	if u.InSUI {
		if e := suiRenew(name, days); e != nil {
			errs = append(errs, "s-ui: "+e.Error())
		}
	}
	period := "never expires"
	if days > 0 {
		period = fmt.Sprintf("%d days (until %s)", days, time.Now().Add(time.Duration(days)*24*time.Hour).Format("2006-01-02"))
	}
	if len(errs) > 0 {
		send(bot, chatID, fmt.Sprintf("⚠️ *%s* renew — some panels failed:\n`%s`", name, strings.Join(errs, "\n")))
		return
	}
	send(bot, chatID, fmt.Sprintf(
		"🔄 *%s renewed*\n\n📅 New period: %s\n📊 Traffic reset to 0\n🟢 Enabled\n\n🔗 Sub: `%s`",
		name, period, subLink(name),
	))
}

func showDeleteConfirm(bot *tgbotapi.BotAPI, chatID int64, msgID int, name string) {
	text := fmt.Sprintf("🗑 Delete *%s* from all panels?\n\nThis removes the user everywhere. This cannot be undone.", name)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Yes, delete", "uact:delete:"+name),
			tgbotapi.NewInlineKeyboardButtonData("⬅️ No, back", "usr:"+name),
		),
	)
	editKB(bot, chatID, msgID, text, kb)
}

// --- Backups (x-ui + s-ui databases sent to Telegram) ---

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func sendBackups(bot *tgbotapi.BotAPI, chatID int64, label string) {
	ts := time.Now().UTC().Format("2006-01-02_15-04")
	dbs := []struct{ src, name string }{
		{xuiDBPath, "xui_" + ts + ".db"},
		{suiDBPath, "sui_" + ts + ".db"},
	}
	send(bot, chatID, fmt.Sprintf("📦 *%s backup starting…*", label))
	allOk := true
	for _, d := range dbs {
		if _, err := os.Stat(d.src); err != nil {
			send(bot, chatID, "⚠️ Skipping `"+d.src+"` — not found.")
			continue
		}
		tmp := filepath.Join(os.TempDir(), d.name)
		if err := copyFile(d.src, tmp); err != nil {
			send(bot, chatID, "❌ copy `"+d.src+"`: "+err.Error())
			allOk = false
			continue
		}
		f, err := os.Open(tmp)
		if err != nil {
			allOk = false
			continue
		}
		doc := tgbotapi.NewDocument(chatID, tgbotapi.FileReader{Name: d.name, Reader: f})
		doc.Caption = fmt.Sprintf("`%s` — %s UTC", d.name, time.Now().UTC().Format("2006-01-02 15:04"))
		doc.ParseMode = "Markdown"
		if _, err := bot.Send(doc); err != nil {
			log.Printf("send backup %s: %v", d.name, err)
			allOk = false
		}
		f.Close()
		os.Remove(tmp)
	}
	if allOk {
		send(bot, chatID, "✅ *Backup complete.*")
	}
}

func scheduleDailyBackup(bot *tgbotapi.BotAPI) {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), backupHour, 0, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		log.Printf("Next daily backup at %s UTC", next.Format("2006-01-02 15:04"))
		time.Sleep(time.Until(next))
		sendBackups(bot, adminChatID, "Daily")
	}
}

// --- Web Dashboard ---

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Server Monitor</title>
<style>
  :root{--bg:#0d1117;--surface:#161b22;--border:#21262d;--text:#e6edf3;--muted:#8b949e;--accent:#58a6ff;--good:#3fb950;--warn:#d29922;--crit:#f85149;--font:'Courier New',monospace}
  *{box-sizing:border-box;margin:0;padding:0}
  body{background:var(--bg);color:var(--text);font-family:var(--font);min-height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;padding:2rem}
  h1{font-size:1.4rem;letter-spacing:.15em;color:var(--accent);margin-bottom:2rem;text-transform:uppercase}
  .grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:1rem;width:100%;max-width:900px}
  .card{background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:1.25rem 1.5rem}
  .card-label{font-size:.7rem;letter-spacing:.12em;color:var(--muted);text-transform:uppercase;margin-bottom:.5rem}
  .card-value{font-size:1.6rem;font-weight:bold}
  .card-sub{font-size:.75rem;color:var(--muted);margin-top:.3rem}
  .bar{background:var(--border);border-radius:4px;height:6px;margin-top:.75rem;overflow:hidden}
  .bar-fill{height:100%;border-radius:4px}
  .good{color:var(--good)}.warn{color:var(--warn)}.crit{color:var(--crit)}
  .bar-fill.good{background:var(--good)}.bar-fill.warn{background:var(--warn)}.bar-fill.crit{background:var(--crit)}
  .ts{margin-top:2rem;font-size:.7rem;color:var(--muted)}
  .refresh-btn{margin-top:1.5rem;background:none;border:1px solid var(--border);color:var(--accent);padding:.5rem 1.5rem;border-radius:6px;cursor:pointer;font-family:var(--font);font-size:.8rem;text-transform:uppercase}
</style>
</head>
<body>
<h1>⬡ Server Monitor</h1>
<div class="grid">
  <div class="card"><div class="card-label">Uptime</div><div class="card-value" style="font-size:1.2rem">{{.Uptime}}</div></div>
  <div class="card"><div class="card-label">CPU</div><div class="card-value {{cpuClass .CPUPct}}">{{printf "%.1f" .CPUPct}}<span style="font-size:1rem">%</span></div><div class="bar"><div class="bar-fill {{cpuClass .CPUPct}}" style="width:{{printf "%.1f" .CPUPct}}%"></div></div></div>
  <div class="card"><div class="card-label">RAM</div><div class="card-value {{ramClass .RAMPct}}">{{printf "%.1f" .RAMPct}}<span style="font-size:1rem">%</span></div><div class="card-sub">{{.RAMUsed}} MB / {{.RAMTotal}} MB</div><div class="bar"><div class="bar-fill {{ramClass .RAMPct}}" style="width:{{printf "%.1f" .RAMPct}}%"></div></div></div>
  <div class="card"><div class="card-label">Network I/O</div><div class="card-value" style="font-size:1.1rem">↑ {{.NetSent}} MB</div><div class="card-sub">↓ {{.NetRecv}} MB</div></div>
</div>
<div class="ts">{{.Timestamp}} · Go {{.GoVersion}}</div>
<button class="refresh-btn" onclick="location.reload()">↻ Refresh</button>
</body>
</html>`

func cpuClass(p float64) string {
	if p >= 85 {
		return "crit"
	} else if p >= 60 {
		return "warn"
	}
	return "good"
}
func ramClass(p float64) string {
	if p >= 85 {
		return "crit"
	} else if p >= 70 {
		return "warn"
	}
	return "good"
}

var tmpl = template.Must(template.New("dash").Funcs(template.FuncMap{"cpuClass": cpuClass, "ramClass": ramClass}).Parse(dashboardHTML))

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != webUser || p != webPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Monitor"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func runWeb() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		m := getMetrics()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmpl.Execute(w, m)
	}))
	mux.HandleFunc("/api/metrics", basicAuth(func(w http.ResponseWriter, r *http.Request) {
		m := getMetrics()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"uptime":%q,"cpu":%.1f,"ram_used":%d,"ram_total":%d,"ram_pct":%.1f,"net_sent":%d,"net_recv":%d,"ts":%q}`,
			m.Uptime, m.CPUPct, m.RAMUsed, m.RAMTotal, m.RAMPct, m.NetSent, m.NetRecv, m.Timestamp)
	}))
	log.Printf("Web dashboard on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func main() {
	ctx := context.Background()
	go runBot(ctx)
	runWeb()
}
