package config

import (
	"os"
	"strings"
)

// Config captures runtime settings for the application.
type Config struct {
	HTTPAddr    string
	DatabaseDSN string
}

// Load builds Config from environment variables with sensible defaults.
func Load() Config {
	return Config{
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
		DatabaseDSN: getenv("DATABASE_DSN", "file:whatsapp.db?_foreign_keys=on"),
	}
}

func getenv(key, def string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value != "" {
		return value
	}
	return def
}
