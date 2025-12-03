package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	qrterminal "github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// App holds the WhatsApp client and runtime config (like webhook URL).
type App struct {
	Client *whatsmeow.Client

	webhookMu  sync.RWMutex
	webhookURL string
}

// Payload for /send-text
type sendTextRequest struct {
	To      string `json:"to"`      // phone number or full JID string
	Message string `json:"message"` // text to send
}

// Payload sent to your webhook for every incoming message.
type incomingWebhookPayload struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Timestamp int64  `json:"timestamp"`
	Text      string `json:"text"`
}

// Payload for /webhook to set the callback URL.
type setWebhookRequest struct {
	URL string `json:"url"`
}

// eventHandler is registered as the whatsmeow event handler.
func (a *App) eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		// Forward every message we receive to the configured webhook (if any).
		go a.forwardToWebhook(v)
	}
}

// forwardToWebhook posts the received WhatsApp message to the user-specified webhook.
func (a *App) forwardToWebhook(msg *events.Message) {
	a.webhookMu.RLock()
	url := a.webhookURL
	a.webhookMu.RUnlock()

	if url == "" {
		// Webhook not configured, nothing to do.
		return
	}

	// Extract text (very basic: Conversation or ExtendedTextMessage).
	text := msg.Message.GetConversation()
	if text == "" && msg.Message.GetExtendedTextMessage() != nil {
		text = msg.Message.GetExtendedTextMessage().GetText()
	}

	payload := incomingWebhookPayload{
		ID:        msg.Info.ID,
		From:      msg.Info.Sender.String(),
		To:        msg.Info.Chat.String(),
		Timestamp: msg.Info.Timestamp.Unix(),
		Text:      text,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("failed to marshal webhook payload: %v", err)
		return
	}

	// Fire-and-forget HTTP POST to webhook.
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("webhook POST error: %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 300 {
		log.Printf("webhook responded with status %d", resp.StatusCode)
	}
}

// HTTP handler: POST /send-text
func (a *App) handleSendText(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sendTextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	req.To = strings.TrimSpace(req.To)
	if req.To == "" || req.Message == "" {
		http.Error(w, "`to` and `message` are required", http.StatusBadRequest)
		return
	}

	if !a.Client.IsConnected() {
		http.Error(w, "whatsapp client not connected", http.StatusServiceUnavailable)
		return
	}

	// Build JID. If ParseJID fails, treat `To` as phone number.
	var jid types.JID
	jid, err := types.ParseJID(req.To)
	if err != nil {
		jid = types.NewJID(req.To, types.DefaultUserServer)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	message := &waE2E.Message{
		Conversation: proto.String(req.Message),
	}

	resp, err := a.Client.SendMessage(ctx, jid, message, &whatsmeow.SendRequestExtra{})
	if err != nil {
		log.Printf("SendMessage error: %v", err)
		http.Error(w, "send failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HTTP handler: POST /webhook to set or change the webhook URL.
func (a *App) handleSetWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req setWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	url := strings.TrimSpace(req.URL)
	if url == "" {
		http.Error(w, "`url` is required", http.StatusBadRequest)
		return
	}

	a.webhookMu.Lock()
	a.webhookURL = url
	a.webhookMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","webhook_url":%q}`, url)
}

func main() {
	// ----- 1. Setup WhatsApp client (whatsmeow) -----
	ctx := context.Background()

	dbLog := waLog.Stdout("Database", "INFO", true)
	// Uses SQLite at ./whatsapp.db to store session & keys
	container, err := sqlstore.New(ctx, "sqlite3", "file:whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		log.Fatalf("failed to create sqlstore: %v", err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("failed to get device: %v", err)
	}

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	app := &App{Client: client}
	client.AddEventHandler(app.eventHandler)

	// First-time login: show QR in terminal, then wait for scan.
	if client.Store.ID == nil {
		log.Println("No existing WhatsApp session, starting new login...")
		qrChan, _ := client.GetQRChannel(ctx)
		if err := client.Connect(); err != nil {
			log.Fatalf("failed to connect: %v", err)
		}

		for evt := range qrChan {
			switch evt.Event {
			case "code":
				// Render QR as text in the terminal
				log.Println("Scan this QR with your phone's WhatsApp:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			default:
				log.Println("Login event:", evt.Event)
			}
		}
	} else {
		log.Println("Existing WhatsApp session found, connecting...")
		if err := client.Connect(); err != nil {
			log.Fatalf("failed to connect: %v", err)
		}
	}

	// ----- 2. Start HTTP REST API server -----
	mux := http.NewServeMux()
	mux.HandleFunc("/send-text", app.handleSendText)
	mux.HandleFunc("/webhook", app.handleSetWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if client.IsConnected() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("whatsapp not connected"))
		}
	})

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		log.Println("HTTP API listening on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// ----- 3. Graceful shutdown on Ctrl+C -----
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down HTTP server...")
	ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctxShutdown)

	log.Println("Disconnecting WhatsApp client...")
	client.Disconnect()
	log.Println("Bye.")
}
