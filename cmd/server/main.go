package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"

	"github.com/awang-karisma/restapp-go/internal/api"
	"github.com/awang-karisma/restapp-go/internal/config"
	"github.com/awang-karisma/restapp-go/internal/whatsapp"
)

func main() {
	ctx := context.Background()
	cfg := config.Load()

	db, err := sql.Open(cfg.DatabaseDriver, cfg.DatabaseDSN)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	if cfg.Dialect() == "sqlite3" {
		if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
			log.Fatalf("failed to enable sqlite foreign keys: %v", err)
		}
	}

	waService, err := whatsapp.NewService(ctx, db, cfg.Dialect(), cfg)
	if err != nil {
		log.Fatalf("failed to init whatsapp client: %v", err)
	}

	if err := waService.Connect(ctx); err != nil {
		log.Fatalf("failed to connect to whatsapp: %v", err)
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
			log.Fatalf("HTTP server error: %v", err)
		}
	case <-stop:
		log.Println("Shutdown signal received, closing down...")
	}

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := apiServer.Shutdown(ctxShutdown); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	waService.Disconnect()
	log.Println("Bye.")
}
