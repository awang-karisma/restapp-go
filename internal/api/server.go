package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/awang-karisma/restapp-go/internal/config"
	"github.com/awang-karisma/restapp-go/internal/whatsapp"
	"github.com/skip2/go-qrcode"
	httpSwagger "github.com/swaggo/http-swagger"
)

type Server struct {
	srv      *http.Server
	whatsapp *whatsapp.Service
}

type sendTextRequest struct {
	To      string `json:"to"`
	Message string `json:"message"`
}

type setWebhookRequest struct {
	URL string `json:"url"`
}

type sendMediaRequest struct {
	To         string `json:"to"`
	Type       string `json:"type"`        // image, video, audio, ptt, file, sticker
	MimeType   string `json:"mime_type"`   // e.g. image/png
	FileName   string `json:"file_name"`   // used for documents
	Caption    string `json:"caption"`     // optional caption/description
	DataBase64 string `json:"data_base64"` // base64 encoded bytes of the media
}

type sendResponse struct {
	ID         string   `json:"ID"`
	MessageIDs []string `json:"MessageIDs"`
}

type qrResponse struct {
	QRBase64 string `json:"qr_base64"`
}

func NewServer(cfg config.Config, svc *whatsapp.Service) *Server {
	mux := http.NewServeMux()

	s := &Server{
		whatsapp: svc,
		srv: &http.Server{
			Addr:    cfg.HTTPAddr,
			Handler: mux,
		},
	}

	mux.HandleFunc("/send-text", s.handleSendText)
	mux.HandleFunc("/send-media", s.handleSendMedia)
	mux.HandleFunc("/media", s.handleFetchMedia)
	mux.HandleFunc("/webhook", s.handleSetWebhook)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/qr", s.handleQR)
	mux.Handle("/swagger/", httpSwagger.WrapHandler)

	return s
}

func (s *Server) ListenAndServe() error {
	log.Printf("HTTP API listening on %s", s.srv.Addr)

	if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// handleQR godoc
// @Summary Get login QR code
// @Description Returns the current login QR code if not logged in. Defaults to PNG image. Use `type=json` to get base64.
// @Tags auth
// @Produce json
// @Produce png
// @Param type query string false "image|json" default(image)
// @Success 200 {object} qrResponse "Base64 QR when type=json"
// @Failure 409 {string} string "Session already exists"
// @Failure 404 {string} string "QR not available"
// @Router /qr [get]
func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if kind == "" {
		kind = "image"
	}

	code, err := s.whatsapp.QRCode()
	if err != nil {
		if strings.Contains(err.Error(), "exists") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	png, err := qrcode.Encode(code, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "failed to render qr", http.StatusInternalServerError)
		return
	}

	if kind == "json" {
		resp := qrResponse{QRBase64: base64.StdEncoding.EncodeToString(png)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// handleSendText godoc
// @Summary Send a text message
// @Description Sends a plain text WhatsApp message
// @Tags messages
// @Accept json
// @Produce json
// @Param request body sendTextRequest true "Send text payload"
// @Success 200 {object} sendResponse "Message accepted"
// @Failure 400 {string} string "Invalid input"
// @Failure 503 {string} string "WhatsApp not connected"
// @Failure 500 {string} string "Send failed"
// @Router /send-text [post]
func (s *Server) handleSendText(w http.ResponseWriter, r *http.Request) {
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

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	resp, err := s.whatsapp.SendText(ctx, req.To, req.Message)
	if err != nil {
		log.Printf("SendMessage error: %v", err)
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not connected") {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "send failed: "+err.Error(), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleFetchMedia godoc
// @Summary Fetch media by message ID
// @Description Downloads and decrypts media for a previously received message ID
// @Tags media
// @Produce octet-stream
// @Param id query string true "Message ID from webhook payload"
// @Success 200 {file} binary "Media bytes"
// @Failure 404 {string} string "Media not found in cache/DB"
// @Failure 500 {string} string "Download/decrypt failed"
// @Router /media [get]
func (s *Server) handleFetchMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "`id` is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	result, err := s.whatsapp.FetchMedia(ctx, id)
	if err != nil {
		log.Printf("FetchMedia error for %s: %v", id, err)
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, "fetch media failed: "+err.Error(), status)
		return
	}

	if result.MimeType != "" {
		w.Header().Set("Content-Type", result.MimeType)
	}
	if result.FileName != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", result.FileName))
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Data)
}

// handleSendMedia godoc
// @Summary Send media (image/video/audio/ptt/document/sticker)
// @Description Supports JSON (base64) or multipart/form-data uploads
// @Tags media
// @Accept json
// @Accept mpfd
// @Produce json
// @Param request body sendMediaRequest false "Send media payload (JSON/base64)"
// @Success 200 {object} sendResponse "Media accepted"
// @Failure 400 {string} string "Invalid input"
// @Failure 503 {string} string "WhatsApp not connected"
// @Failure 500 {string} string "Send failed"
// @Router /send-media [post]
func (s *Server) handleSendMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var (
		to       string
		kind     string
		mimeType string
		fileName string
		caption  string
		data     []byte
	)

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/") {
		const maxUploadSize = 25 << 20 // 25 MB
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
		if err := r.ParseMultipartForm(maxUploadSize); err != nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}

		to = strings.TrimSpace(r.FormValue("to"))
		kind = strings.ToLower(strings.TrimSpace(r.FormValue("type")))
		mimeType = strings.TrimSpace(r.FormValue("mime_type"))
		fileName = strings.TrimSpace(r.FormValue("file_name"))
		caption = r.FormValue("caption")

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "`file` is required in multipart form", http.StatusBadRequest)
			return
		}
		defer file.Close()

		data, err = io.ReadAll(file)
		if err != nil {
			http.Error(w, "failed to read uploaded file", http.StatusBadRequest)
			return
		}

		if mimeType == "" {
			mimeType = header.Header.Get("Content-Type")
		}
		if mimeType == "" {
			sniff := data
			if len(sniff) > 512 {
				sniff = sniff[:512]
			}
			mimeType = http.DetectContentType(sniff)
		}
		if fileName == "" {
			fileName = header.Filename
		}
	} else {
		var req sendMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		req.To = strings.TrimSpace(req.To)
		req.Type = strings.ToLower(strings.TrimSpace(req.Type))
		req.MimeType = strings.TrimSpace(req.MimeType)
		req.FileName = strings.TrimSpace(req.FileName)

		if req.To == "" || req.Type == "" || req.MimeType == "" || req.DataBase64 == "" {
			http.Error(w, "`to`, `type`, `mime_type`, and `data_base64` are required", http.StatusBadRequest)
			return
		}

		decoded, err := base64.StdEncoding.DecodeString(req.DataBase64)
		if err != nil {
			http.Error(w, "invalid base64 data", http.StatusBadRequest)
			return
		}
		to, kind, mimeType, fileName, caption, data = req.To, req.Type, req.MimeType, req.FileName, req.Caption, decoded
	}

	if to == "" || kind == "" || mimeType == "" || len(data) == 0 {
		http.Error(w, "`to`, `type`, `mime_type`, and file content are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resp, err := s.whatsapp.SendMedia(ctx, whatsapp.MediaMessageInput{
		To:       to,
		Kind:     kind,
		MimeType: mimeType,
		FileName: fileName,
		Caption:  caption,
		Data:     data,
	})
	if err != nil {
		log.Printf("SendMedia error: %v", err)
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not connected") {
			status = http.StatusServiceUnavailable
		} else if strings.Contains(err.Error(), "unsupported media type") || strings.Contains(err.Error(), "missing") {
			status = http.StatusBadRequest
		}
		http.Error(w, "send media failed: "+err.Error(), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSetWebhook godoc
// @Summary Configure webhook URL
// @Description Sets the HTTP webhook URL for incoming WhatsApp messages
// @Tags webhook
// @Accept json
// @Produce json
// @Param request body setWebhookRequest true "Webhook URL payload"
// @Success 200 {object} map[string]string "Status and webhook URL"
// @Failure 400 {string} string "Invalid input"
// @Router /webhook [post]
func (s *Server) handleSetWebhook(w http.ResponseWriter, r *http.Request) {
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

	s.whatsapp.SetWebhook(url)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","webhook_url":%q}`, url)
}

// handleHealth godoc
// @Summary Health check
// @Description Returns OK when WhatsApp client is connected
// @Tags health
// @Produce plain
// @Success 200 {string} string "ok"
// @Failure 503 {string} string "whatsapp not connected"
// @Router /healthz [get]
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.whatsapp.IsConnected() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("whatsapp not connected"))
}
