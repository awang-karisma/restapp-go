# WhatsApp Relay Service

Lightweight REST wrapper around the WhatsMeow client to send/receive WhatsApp messages, forward webhooks, and fetch media on demand.

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
- `POST /send-text` — send a text message.
- `POST /send-media` — send image/video/audio/ptt/document/sticker. Accepts JSON (base64 `data`) or multipart form. Sticker-specific fields: `kind=sticker` with WebP data; `sticker_pack_id/name/publisher` and optional `emojis` (JSON or repeated form field) set pack metadata; `disable_sticker_exif=true` to skip embedding; `kind=sticker_lottie` for Lottie ZIP stickers. Other media use `mime_type` and optional `filename`/`caption`.
- `POST /send-reaction` — add/remove a reaction on a message (empty emoji removes).
- `POST /delete-message` — revoke/delete a message (text or media); optional sender for admin deletes in groups.
- `GET /media?id=<message-id>` — download previously received media (requires cached/persisted metadata)
- `POST /webhook` — set/change webhook URL
- `GET /qr?type=image|json` — fetch current QR (image by default; json returns base64). Returns an error if already logged in.
- `GET /healthz` — health check
- `GET /swagger/` — Swagger UI (spec at `/swagger/doc.json`)

Incoming webhooks include media metadata (type, mime, id) but do not download media; use `/media` with the `media_id` to fetch when needed.

### Webhook payloads
The service POSTs a JSON payload to your configured webhook URL. Core fields:
- `id`, `from`, `to`, `author`, `timestamp`, `isGroupMsg`, `isQuoted`, `type`
- Message fields (per type): `text`, `caption`, `media_id`, `mime_type`, `file_name`, `ptt`
- Protocol fields: `deleted_id` (delete), `edited_id`/`edit_type` (edit), ephemeral settings fields, history sync fields
- Reactions: `reaction` (emoji), `reaction_target_id`, `reaction_remove` (true when reaction was removed)
- Group events: `group_id`, `participants` (array), `type` values `group_participants_added`, `group_participants_removed`, `group_promote`, `group_demote`, `group_joined`

Examples:
- Text: `{"type":"text","text":"hi","from":"123@s.whatsapp.net","to":"456@s.whatsapp.net","timestamp":...}`
- Image (media cached for later fetch): `{"type":"image","media_id":"<msg-id>","mime_type":"image/jpeg","caption":"pic","from":"...","to":"..."}`
- Group promote: `{"type":"group_promote","group_id":"123-456@g.us","participants":["789@s.whatsapp.net"],"from":"admin@g.us","to":"123-456@g.us"}`
- Reaction: `{"type":"reaction","reaction":"👍","reaction_target_id":"<msg-id>","from":"123@s.whatsapp.net","to":"456@s.whatsapp.net"}`

## Swagger docs
Generated with `swag` annotations. To regenerate after handler changes:
```bash
go install github.com/swaggo/swag/cmd/swag@latest
PATH=$(go env GOPATH)/bin:$PATH go generate ./cmd/server
```

## Project layout
- `cmd/server` — entrypoint wiring config/env and HTTP server.
- `internal/api` — HTTP handlers (send, webhook, media fetch, QR, swagger).
- `internal/whatsapp` — WhatsApp client service, webhook dispatch, media store, sticker helpers (EXIF/Lottie), QR handling.
- `internal/config` — configuration loading.

## Persistence notes
Media metadata is persisted (SQLite/Postgres) so `/media` can fetch after restarts. Actual media bytes are only downloaded on demand.***
