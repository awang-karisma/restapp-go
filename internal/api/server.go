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
	Type       string `json:"type"`        // image, video, audio, ptt, file
	MimeType   string `json:"mime_type"`   // e.g. image/png
	FileName   string `json:"file_name"`   // used for documents
	Caption    string `json:"caption"`     // optional caption/description
	DataBase64 string `json:"data_base64"` // base64 encoded bytes of the media
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
	mux.HandleFunc("/webhook", s.handleSetWebhook)
	mux.HandleFunc("/healthz", s.handleHealth)

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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.whatsapp.IsConnected() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("whatsapp not connected"))
}
