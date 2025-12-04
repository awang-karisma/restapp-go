# WhatsApp Relay Service

Lightweight REST wrapper around the WhatsMeow client to send/receive WhatsApp messages, forward webhooks, and fetch media on demand.

## Prerequisites
- Go 1.21+ (module uses Go 1.25.x in `go.mod`)
- SQLite (built-in) or Postgres if you set `DATABASE_DRIVER=postgres`
- WhatsApp QR login (first run will prompt a terminal QR)

## Configuration
Copy `.env.example` to `.env` and adjust as needed:
- `HTTP_ADDR` — listen address, default `:8080`
- `DATABASE_DRIVER` — `sqlite3` (default) or `postgres`
- `DATABASE_DSN` — SQLite DSN (`file:whatsapp.db?_foreign_keys=on`) or Postgres URL
- `WEBHOOK_URL` — optional initial webhook to receive incoming events

Environment variables are read at startup; no flags are required.

## Run
```bash
go run ./cmd/server
```
On first run, scan the QR printed in the terminal with WhatsApp on your phone.

## API Overview
- `POST /send-text` — send a text message
- `POST /send-media` — send image/video/audio/ptt/document (JSON+base64 or multipart)
- `GET /media?id=<message-id>` — download previously received media (requires cached/persisted metadata)
- `POST /webhook` — set/change webhook URL
- `GET /healthz` — health check
- `GET /swagger/` — Swagger UI (spec at `/swagger/doc.json`)

Incoming webhooks include media metadata (type, mime, id) but do not download media; use `/media` with the `media_id` to fetch when needed.

## Swagger docs
Generated with `swag` annotations. To regenerate after handler changes:
```bash
go install github.com/swaggo/swag/cmd/swag@latest
PATH=$(go env GOPATH)/bin:$PATH go generate ./cmd/server
```

## Persistence notes
Media metadata is persisted (SQLite/Postgres) so `/media` can fetch after restarts. Actual media bytes are only downloaded on demand.***
