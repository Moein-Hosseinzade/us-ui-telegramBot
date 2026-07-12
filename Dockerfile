FROM golang:1.22-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app
COPY go.mod main.go ./
RUN go mod tidy && \
    CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o monitor .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/monitor .
EXPOSE 8080
ENTRYPOINT ["/app/monitor"]
