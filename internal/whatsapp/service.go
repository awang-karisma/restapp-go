package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"google.golang.org/protobuf/proto"

	"github.com/awang-karisma/restapp-go/internal/config"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type Service struct {
	client     *whatsmeow.Client
	webhookMu  sync.RWMutex
	webhookURL string
	httpClient *http.Client
}

type incomingWebhookPayload struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Timestamp int64  `json:"timestamp"`
	Text      string `json:"text"`
}

// MediaMessageInput captures the data needed to send a media message.
type MediaMessageInput struct {
	To       string
	Kind     string // image, video, audio, ptt, file/document
	MimeType string
	FileName string
	Caption  string
	Data     []byte
}

func NewService(ctx context.Context, cfg config.Config) (*Service, error) {
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(ctx, "sqlite3", cfg.DatabaseDSN, dbLog)
	if err != nil {
		return nil, err
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}

	clientLog := waLog.Stdout("Client", "INFO", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	svc := &Service{
		client:     client,
		httpClient: http.DefaultClient,
	}
	client.AddEventHandler(svc.eventHandler)

	return svc, nil
}

func (s *Service) Connect(ctx context.Context) error {
	if s.client.Store.ID == nil {
		log.Println("No existing WhatsApp session, starting new login...")
		qrChan, _ := s.client.GetQRChannel(ctx)
		if err := s.client.Connect(); err != nil {
			return err
		}

		for evt := range qrChan {
			switch evt.Event {
			case "code":
				log.Println("Scan this QR with your phone's WhatsApp:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			default:
				log.Println("Login event:", evt.Event)
			}
		}
		return nil
	}

	log.Println("Existing WhatsApp session found, connecting...")
	return s.client.Connect()
}

func (s *Service) Disconnect() {
	s.client.Disconnect()
}

func (s *Service) IsConnected() bool {
	return s.client.IsConnected()
}

func (s *Service) SendText(ctx context.Context, to, message string) (whatsmeow.SendResponse, error) {
	if !s.client.IsConnected() {
		return whatsmeow.SendResponse{}, errors.New("whatsapp client not connected")
	}

	to = strings.TrimSpace(to)
	if to == "" {
		return whatsmeow.SendResponse{}, errors.New("missing destination")
	}

	var jid types.JID
	jid, err := types.ParseJID(to)
	if err != nil {
		jid = types.NewJID(to, types.DefaultUserServer)
	}

	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	msg := &waE2E.Message{Conversation: proto.String(message)}
	return s.client.SendMessage(sendCtx, jid, msg, whatsmeow.SendRequestExtra{})
}

// SendMedia uploads and sends the provided media to the given destination.
func (s *Service) SendMedia(ctx context.Context, input MediaMessageInput) (whatsmeow.SendResponse, error) {
	if !s.client.IsConnected() {
		return whatsmeow.SendResponse{}, errors.New("whatsapp client not connected")
	}

	to := strings.TrimSpace(input.To)
	if to == "" {
		return whatsmeow.SendResponse{}, errors.New("missing destination")
	}
	if len(input.Data) == 0 {
		return whatsmeow.SendResponse{}, errors.New("missing media data")
	}
	if input.MimeType == "" {
		return whatsmeow.SendResponse{}, errors.New("missing mime type")
	}

	kind := strings.ToLower(strings.TrimSpace(input.Kind))
	mediaType, err := mediaTypeForKind(kind)
	if err != nil {
		return whatsmeow.SendResponse{}, err
	}

	var jid types.JID
	jid, err = types.ParseJID(to)
	if err != nil {
		jid = types.NewJID(to, types.DefaultUserServer)
	}

	uploadResp, err := s.client.Upload(ctx, input.Data, mediaType)
	if err != nil {
		return whatsmeow.SendResponse{}, err
	}

	message, err := buildMediaMessage(kind, input, uploadResp)
	if err != nil {
		return whatsmeow.SendResponse{}, err
	}

	return s.client.SendMessage(ctx, jid, message, whatsmeow.SendRequestExtra{})
}

func (s *Service) SetWebhook(url string) {
	s.webhookMu.Lock()
	s.webhookURL = strings.TrimSpace(url)
	s.webhookMu.Unlock()
}

func (s *Service) eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		go s.forwardToWebhook(v)
	}
}

func (s *Service) forwardToWebhook(msg *events.Message) {
	url := s.getWebhookURL()
	if url == "" {
		return
	}

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

	resp, err := s.httpClient.Post(url, "application/json", bytes.NewReader(body))
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

func (s *Service) getWebhookURL() string {
	s.webhookMu.RLock()
	defer s.webhookMu.RUnlock()
	return s.webhookURL
}

func mediaTypeForKind(kind string) (whatsmeow.MediaType, error) {
	switch kind {
	case "image":
		return whatsmeow.MediaImage, nil
	case "video":
		return whatsmeow.MediaVideo, nil
	case "audio", "ptt":
		return whatsmeow.MediaAudio, nil
	case "file", "document":
		return whatsmeow.MediaDocument, nil
	default:
		return "", errors.New("unsupported media type: " + kind)
	}
}

func buildMediaMessage(kind string, input MediaMessageInput, uploadResp whatsmeow.UploadResponse) (*waE2E.Message, error) {
	switch kind {
	case "image":
		return &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				Caption:       proto.String(input.Caption),
				Mimetype:      proto.String(input.MimeType),
				URL:           proto.String(uploadResp.URL),
				DirectPath:    proto.String(uploadResp.DirectPath),
				MediaKey:      uploadResp.MediaKey,
				FileEncSHA256: uploadResp.FileEncSHA256,
				FileSHA256:    uploadResp.FileSHA256,
				FileLength:    proto.Uint64(uploadResp.FileLength),
			},
		}, nil
	case "video":
		return &waE2E.Message{
			VideoMessage: &waE2E.VideoMessage{
				Caption:       proto.String(input.Caption),
				Mimetype:      proto.String(input.MimeType),
				URL:           proto.String(uploadResp.URL),
				DirectPath:    proto.String(uploadResp.DirectPath),
				MediaKey:      uploadResp.MediaKey,
				FileEncSHA256: uploadResp.FileEncSHA256,
				FileSHA256:    uploadResp.FileSHA256,
				FileLength:    proto.Uint64(uploadResp.FileLength),
			},
		}, nil
	case "audio", "ptt":
		ptt := kind == "ptt"
		return &waE2E.Message{
			AudioMessage: &waE2E.AudioMessage{
				Mimetype:      proto.String(input.MimeType),
				URL:           proto.String(uploadResp.URL),
				DirectPath:    proto.String(uploadResp.DirectPath),
				MediaKey:      uploadResp.MediaKey,
				FileEncSHA256: uploadResp.FileEncSHA256,
				FileSHA256:    uploadResp.FileSHA256,
				FileLength:    proto.Uint64(uploadResp.FileLength),
				PTT:           proto.Bool(ptt),
			},
		}, nil
	case "file", "document":
		name := input.FileName
		if strings.TrimSpace(name) == "" {
			name = "file"
		}
		return &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				Caption:       proto.String(input.Caption),
				Mimetype:      proto.String(input.MimeType),
				Title:         proto.String(name),
				FileName:      proto.String(name),
				URL:           proto.String(uploadResp.URL),
				DirectPath:    proto.String(uploadResp.DirectPath),
				MediaKey:      uploadResp.MediaKey,
				FileEncSHA256: uploadResp.FileEncSHA256,
				FileSHA256:    uploadResp.FileSHA256,
				FileLength:    proto.Uint64(uploadResp.FileLength),
			},
		}, nil
	default:
		return nil, errors.New("unsupported media type: " + kind)
	}
}
