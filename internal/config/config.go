package config

import (
	"os"
	"strings"
)

// Config captures runtime settings for the application.
type Config struct {
	HTTPAddr       string
	DatabaseDriver string
	DatabaseDSN    string
	WebhookURL     string
}

// Load builds Config from environment variables with sensible defaults.
func Load() Config {
	return Config{
		HTTPAddr:       getenv("HTTP_ADDR", ":8080"),
		DatabaseDriver: strings.ToLower(getenv("DATABASE_DRIVER", "sqlite3")),
		DatabaseDSN:    getenv("DATABASE_DSN", "file:whatsapp.db?_foreign_keys=on"),
		WebhookURL:     getenv("WEBHOOK_URL", ""),
	}
}

// Dialect normalizes the configured driver to a whatsmeow dialect value.
func (c Config) Dialect() string {
	switch c.DatabaseDriver {
	case "postgres", "pgx":
		return "postgres"
	default:
		return "sqlite3"
	}
}

func getenv(key, def string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value != "" {
		return value
	}
	return def
}
