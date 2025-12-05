package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
	To                 string `json:"to"`
	Type               string `json:"type"`        // image, video, audio, ptt, file, sticker, sticker_lottie
	MimeType           string `json:"mime_type"`   // e.g. image/png
	FileName           string `json:"file_name"`   // used for documents
	Caption            string `json:"caption"`     // optional caption/description
	DataBase64         string `json:"data_base64"` // base64 encoded bytes of the media
	StickerPackID      string `json:"sticker_pack_id"`
	StickerPackName    string `json:"sticker_pack_name"`
	StickerPackPub     string `json:"sticker_pack_publisher"`
	AndroidAppStore    string `json:"android_app_store_link"`
	IOSAppStore        string `json:"ios_app_store_link"`
	DisableStickerEXIF bool   `json:"disable_sticker_exif"`
}

type sendResponse struct {
	ID         string   `json:"ID"`
	MessageIDs []string `json:"MessageIDs"`
}

type sendReactionRequest struct {
	To        string `json:"to"`
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji"`
	Sender    string `json:"sender,omitempty"`
}

type deleteMessageRequest struct {
	To        string `json:"to"`
	MessageID string `json:"message_id"`
	Sender    string `json:"sender,omitempty"` // optional original sender (for admin delete)
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
	mux.HandleFunc("/send-reaction", s.handleSendReaction)
	mux.HandleFunc("/delete-message", s.handleDeleteMessage)
	mux.HandleFunc("/send-media", s.handleSendMedia)
	mux.HandleFunc("/media", s.handleFetchMedia)
	mux.HandleFunc("/webhook", s.handleSetWebhook)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/qr", s.handleQR)
	mux.Handle("/swagger/", httpSwagger.WrapHandler)

	return s
}

func (s *Server) ListenAndServe() error {
	slog.Info("HTTP API listening", "addr", s.srv.Addr)

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
		slog.Error("SendMessage error", "err", err)
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

// handleSendReaction godoc
// @Summary Send a reaction
// @Description Sends a reaction to an existing WhatsApp message
// @Tags messages
// @Accept json
// @Produce json
// @Param request body sendReactionRequest true "Reaction payload"
// @Success 200 {object} sendResponse "Reaction accepted"
// @Failure 400 {string} string "Invalid input"
// @Failure 503 {string} string "WhatsApp not connected"
// @Failure 500 {string} string "Send failed"
// @Router /send-reaction [post]
func (s *Server) handleSendReaction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req sendReactionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	req.To = strings.TrimSpace(req.To)
	req.MessageID = strings.TrimSpace(req.MessageID)
	req.Emoji = strings.TrimSpace(req.Emoji)

	if req.To == "" || req.MessageID == "" || req.Emoji == "" {
		http.Error(w, "`to`, `message_id`, and `emoji` are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	resp, err := s.whatsapp.SendReaction(ctx, req.To, req.MessageID, req.Emoji, req.Sender)
	if err != nil {
		slog.Error("SendReaction error", "err", err)
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not connected") {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "send reaction failed: "+err.Error(), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleDeleteMessage godoc
// @Summary Delete a message
// @Description Sends a revoke/delete for a message (works for text and media)
// @Tags messages
// @Accept json
// @Produce json
// @Param request body deleteMessageRequest true "Delete payload"
// @Success 200 {object} sendResponse "Delete accepted"
// @Failure 400 {string} string "Invalid input"
// @Failure 503 {string} string "WhatsApp not connected"
// @Failure 500 {string} string "Delete failed"
// @Router /delete-message [post]
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req deleteMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	req.To = strings.TrimSpace(req.To)
	req.MessageID = strings.TrimSpace(req.MessageID)
	req.Sender = strings.TrimSpace(req.Sender)

	if req.To == "" || req.MessageID == "" {
		http.Error(w, "`to` and `message_id` are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	resp, err := s.whatsapp.SendDelete(ctx, req.To, req.MessageID, req.Sender)
	if err != nil {
		slog.Error("SendDelete error", "err", err)
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not connected") {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "delete failed: "+err.Error(), status)
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
		slog.Error("FetchMedia error", "id", id, "err", err)
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
// @Summary Send media (image/video/audio/ptt/document/sticker/sticker_lottie)
// @Description Supports JSON (base64) or multipart/form-data uploads
// @Tags media
// @Accept json
// @Accept mpfd
// @Produce json
// @Param request body sendMediaRequest false "Send media payload (JSON/base64)"
// @Param sticker_pack_id query string false "Sticker pack ID (sticker only)"
// @Param sticker_pack_name query string false "Sticker pack name (sticker only)"
// @Param sticker_pack_publisher query string false "Sticker pack publisher (sticker only)"
// @Param android_app_store_link query string false "Android store link (sticker only)"
// @Param ios_app_store_link query string false "iOS store link (sticker only)"
// @Param sticker_pack_id formData string false "Sticker pack ID (sticker only, multipart)"
// @Param sticker_pack_name formData string false "Sticker pack name (sticker only, multipart)"
// @Param sticker_pack_publisher formData string false "Sticker pack publisher (sticker only, multipart)"
// @Param android_app_store_link formData string false "Android store link (sticker only, multipart)"
// @Param ios_app_store_link formData string false "iOS store link (sticker only, multipart)"
// @Param disable_sticker_exif formData bool false "Disable WebP EXIF embedding for sticker (multipart)"
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
		meta     *whatsapp.StickerMetadata
		skipExif bool
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
		kind = normalizeKind(r.FormValue("type"))
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
		meta = buildStickerMetadataFromForm(r)
		skipExif = parseBool(r.FormValue("disable_sticker_exif"))
	} else {
		var req sendMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		req.To = strings.TrimSpace(req.To)
		req.Type = normalizeKind(req.Type)
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
		if kind == "sticker" {
			meta = buildStickerMetadataFromJSON(req)
			skipExif = req.DisableStickerEXIF
		}
	}

	if to == "" || kind == "" || mimeType == "" || len(data) == 0 {
		http.Error(w, "`to`, `type`, `mime_type`, and file content are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resp, err := s.whatsapp.SendMedia(ctx, whatsapp.MediaMessageInput{
		To:              to,
		Kind:            kind,
		MimeType:        mimeType,
		FileName:        fileName,
		Caption:         caption,
		Data:            data,
		StickerMeta:     meta,
		SkipStickerExif: skipExif,
	})
	if err != nil {
		slog.Error("SendMedia error", "err", err)
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

func parseBool(val string) bool {
	b, err := strconv.ParseBool(val)
	if err != nil {
		return false
	}
	return b
}

func normalizeKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "sticker-lottie":
		return "sticker_lottie"
	default:
		return k
	}
}

func buildStickerMetadataFromForm(r *http.Request) *whatsapp.StickerMetadata {
	meta := &whatsapp.StickerMetadata{
		PackID:      strings.TrimSpace(r.FormValue("sticker_pack_id")),
		PackName:    strings.TrimSpace(r.FormValue("sticker_pack_name")),
		PackPub:     strings.TrimSpace(r.FormValue("sticker_pack_publisher")),
		AndroidLink: strings.TrimSpace(r.FormValue("android_app_store_link")),
		IOSLink:     strings.TrimSpace(r.FormValue("ios_app_store_link")),
	}

	if meta.PackName == "" {
		meta.PackName = "WhatsApp Bot"
	}
	if meta.PackPub == "" {
		meta.PackPub = "karisma.id"
	}
	if meta.PackID == "" {
		meta.PackID = "bot-pack"
	}
	return meta
}

func buildStickerMetadataFromJSON(req sendMediaRequest) *whatsapp.StickerMetadata {
	meta := &whatsapp.StickerMetadata{
		PackID:      strings.TrimSpace(req.StickerPackID),
		PackName:    strings.TrimSpace(req.StickerPackName),
		PackPub:     strings.TrimSpace(req.StickerPackPub),
		AndroidLink: strings.TrimSpace(req.AndroidAppStore),
		IOSLink:     strings.TrimSpace(req.IOSAppStore),
	}

	if meta.PackName == "" {
		meta.PackName = "WhatsApp Bot"
	}
	if meta.PackPub == "" {
		meta.PackPub = "karisma.id"
	}
	if meta.PackID == "" {
		meta.PackID = "bot-pack"
	}
	return meta
}
