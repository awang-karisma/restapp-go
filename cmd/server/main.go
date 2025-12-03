package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/awang-karisma/restapp-go/internal/api"
	"github.com/awang-karisma/restapp-go/internal/config"
	"github.com/awang-karisma/restapp-go/internal/whatsapp"
)

func main() {
	ctx := context.Background()
	cfg := config.Load()

	waService, err := whatsapp.NewService(ctx, cfg)
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
