# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.25-alpine AS builder
WORKDIR /app

# Install build deps (sqlite headers) if needed.
RUN apk add --no-cache build-base sqlite-dev

# Cache modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build binary
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o /app/bin/server ./cmd/server

# Runtime stage
FROM alpine:3.20
WORKDIR /app

# ca-certs for HTTPS calls
RUN apk add --no-cache ca-certificates sqlite-libs

# Copy binary
COPY --from=builder /app/bin/server /app/server

# Default envs (override via runtime env or .env bind)
ENV HTTP_ADDR=:8080 \
    DATABASE_DRIVER=sqlite3 \
    DATABASE_DSN=file:whatsapp.db?_foreign_keys=on

EXPOSE 8080

CMD ["/app/server"]
