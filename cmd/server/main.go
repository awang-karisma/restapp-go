package main

// @title WhatsApp Relay API
// @version 1.0
// @description REST API for sending/receiving WhatsApp messages via whatsmeow.
// @BasePath /
//go:generate swag init -g main.go -o ../../docs -d .,../../internal/api

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"

	docs "github.com/awang-karisma/restapp-go/docs" // swagger docs
	"github.com/awang-karisma/restapp-go/internal/api"
	"github.com/awang-karisma/restapp-go/internal/config"
	"github.com/awang-karisma/restapp-go/internal/whatsapp"
)

func main() {
	ctx := context.Background()

	// Load environment variables from .env if present (noop when missing).
	_ = godotenv.Load()

	// Configure slog globally.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := config.Load()

	docs.SwaggerInfo.Title = "WhatsApp Relay API"
	docs.SwaggerInfo.Version = "1.0.0"
	docs.SwaggerInfo.BasePath = "/"
	docs.SwaggerInfo.Description = "REST API for sending/receiving WhatsApp messages via whatsmeow."

	db, err := sql.Open(cfg.DatabaseDriver, cfg.DatabaseDSN)
	if err != nil {
		slog.Error("failed to open database", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	if cfg.Dialect() == "sqlite3" {
		if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
			slog.Error("failed to enable sqlite foreign keys", "err", err)
			os.Exit(1)
		}
	}

	waService, err := whatsapp.NewService(ctx, db, cfg.Dialect(), cfg)
	if err != nil {
		slog.Error("failed to init whatsapp client", "err", err)
		os.Exit(1)
	}

	if err := waService.Connect(ctx); err != nil {
		slog.Error("failed to connect to whatsapp", "err", err)
		os.Exit(1)
	}

	apiServer := api.NewServer(cfg, waService)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- apiServer.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil {
			slog.Error("HTTP server error", "err", err)
			os.Exit(1)
		}
	case <-stop:
		slog.Info("Shutdown signal received, closing down...")
	}

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := apiServer.Shutdown(ctxShutdown); err != nil {
		slog.Error("HTTP server shutdown error", "err", err)
	}

	waService.Disconnect()
	slog.Info("Bye.")
}
