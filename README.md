# Server Monitor & VPN Panel Bot

A single Go bot (Telegram + web dashboard) that monitors an Ubuntu server and
manages VPN users across **x-ui (Xray)** and **s-ui (sing-box)** panels.

## Features

**Monitoring**
- `/status` `/cpu` `/ram` `/uptime` `/net` — live server metrics
- Web dashboard on `:8080` (HTTP basic auth)

**User management**
- `/adduser` — button wizard: create a user on **x-ui**, **s-ui**, or **both**,
  with multi-select inbounds, traffic & expiry presets
- `/users` — paginated 2-column list → per-user manage view:
  - 🟢 Enable / 🔴 Disable (both panels)
  - 🔄 Renew (fresh expiry + reset traffic + enable)
  - 🗑 Delete
  - 🔗 Subscription link
- `/inbounds` — list x-ui inbounds

**Backups**
- `/backup` — send the x-ui + s-ui SQLite DBs to Telegram now
- Automatic daily backup at `BACKUP_HOUR` UTC

## How it talks to the panels

- **x-ui (3x-ui v3)** — via its HTTP API using a Bearer **API token**
  (create one in the panel; it bypasses CSRF). Handles all DB reconciliation
  correctly, then xray is reloaded via a host script.
- **s-ui** — DB writes for enable/disable/delete/renew, then a graceful
  **core reload** through the s-ui API (`restartSb`, session login). Not a
  service restart.

Both panel databases are bind-mounted read/write. s-ui runs in WAL mode, so the
**whole `s-ui/db` directory** is mounted (not just the file) to share the WAL.

## Setup

1. Install Docker + Docker Compose.
2. `cp .env.example .env` and fill in your values (see below).
3. Provide a host script `/usr/local/bin/xui-reload.sh` that reloads xray
   (e.g. runs `x-ui restart-xray`).
4. `docker-compose up -d --build`

### Configuration (.env)

| Var | Description |
|-----|-------------|
| `TELEGRAM_TOKEN` | Bot token from @BotFather |
| `ADMIN_CHAT_ID` | Comma-separated admin IDs; first is primary |
| `WEB_USER` / `WEB_PASS` | Dashboard basic-auth creds |
| `XUI_DB_PATH` / `SUI_DB_PATH` | Panel DB paths (host) |
| `XUI_PANEL_URL` | `https://host:port/basePath` |
| `XUI_API_TOKEN` | x-ui API token (Bearer) |
| `SUI_API_URL` | `https://domain:2096/app` |
| `SUI_USER` / `SUI_PASS` | s-ui panel login |
| `SUB_BASE_URL` | Subscription aggregator base URL |
| `BACKUP_HOUR` | Daily backup hour (UTC) |

Also adjust `extra_hosts` in `docker-compose.yml` to map your s-ui cert domain
to the host IP.

## Security

- **Never commit `.env`** — it holds the bot token, API token, and panel
  passwords. It is gitignored.
- Keep this repository **private**.
- The bot only responds to chat IDs listed in `ADMIN_CHAT_ID`.
