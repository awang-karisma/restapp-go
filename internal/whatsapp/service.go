package whatsapp

import (
	"context"
	"database/sql"
	"errors"
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
	waLog "go.mau.fi/whatsmeow/util/log"
)

type Service struct {
	client     *whatsmeow.Client
	webhookMu  sync.RWMutex
	webhookURL string
	httpClient *http.Client

	mediaMu    sync.RWMutex
	mediaStore map[string]storedMedia
	db         *sql.DB
	dialect    string

	qrMu        sync.RWMutex
	lastQRCode  string
	qrChannelOn bool
}

// MediaMessageInput captures the data needed to send a media message.
type MediaMessageInput struct {
	To       string
	Kind     string // image, video, audio, ptt, file/document
	MimeType string
	FileName string
	Caption  string
	Data     []byte
	// StickerAnimated is set internally when sending stickers to mark animated WebP.
	StickerAnimated bool
	// StickerMeta carries optional sticker pack metadata (used only for stickers).
	StickerMeta *StickerMetadata
	// SkipStickerExif disables EXIF injection for stickers when true.
	SkipStickerExif bool
	// StickerLottie marks a Lottie/ZIP sticker that skips WebP handling.
	StickerLottie bool
}

// NewService builds the WhatsApp service using a shared DB handle and dialect.
func NewService(ctx context.Context, db *sql.DB, dialect string, cfg config.Config) (*Service, error) {
	dbLog := waLog.Stdout("Database", "INFO", true)

	container := sqlstore.NewWithDB(db, dialect, dbLog)
	if err := container.Upgrade(ctx); err != nil {
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
		mediaStore: make(map[string]storedMedia),
		db:         db,
		dialect:    dialect,
		webhookURL: strings.TrimSpace(cfg.WebhookURL),
	}
	client.AddEventHandler(svc.eventHandler)

	if err := svc.ensureMediaTable(ctx); err != nil {
		return nil, err
	}

	return svc, nil
}

func (s *Service) Connect(ctx context.Context) error {
	if s.client.Store.ID == nil {
		log.Println("No existing WhatsApp session, starting new login...")
		qrChan, _ := s.client.GetQRChannel(ctx)
		s.setQRChannelState(true)
		if err := s.client.Connect(); err != nil {
			return err
		}

		for evt := range qrChan {
			switch evt.Event {
			case "code":
				log.Println("Scan this QR with your phone's WhatsApp:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				s.setLastQRCode(evt.Code)
			default:
				log.Println("Login event:", evt.Event)
			}
		}
		s.setQRChannelState(false)
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

// QRCode returns the latest QR code string if not logged in.
func (s *Service) QRCode() (string, error) {
	if s.client.Store.ID != nil {
		return "", errors.New("session already exists")
	}
	s.qrMu.RLock()
	defer s.qrMu.RUnlock()
	if s.lastQRCode == "" {
		if s.qrChannelOn {
			return "", errors.New("qr pending, not ready")
		}
		return "", errors.New("qr not available")
	}
	return s.lastQRCode, nil
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

	input.MimeType = strings.ToLower(strings.TrimSpace(input.MimeType))
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

	if kind == "sticker" {
		isWebP, isAnimated := sniffWebP(input.Data)
		if input.MimeType != "image/webp" && input.MimeType != "image/x-webp" {
			return whatsmeow.SendResponse{}, errors.New("sticker mime type must be image/webp; convert GIF/PNG to WebP before sending")
		}
		if !isWebP {
			return whatsmeow.SendResponse{}, errors.New("sticker payload is not valid WebP; conversion is required (future: 3rd-party HTTP API)")
		}

		if !input.SkipStickerExif {
			writeDebugSticker("before", input.Data)

			if input.StickerMeta != nil {
				meta := input.StickerMeta.normalizeDefaults()
				input.StickerMeta = &meta
			} else {
				meta := defaultStickerMeta()
				input.StickerMeta = &meta
			}
			rewritten, err := injectStickerMetadataEXIF(input.Data, *input.StickerMeta)
			if err != nil {
				return whatsmeow.SendResponse{}, err
			}
			writeDebugSticker("after", rewritten)
			input.Data = rewritten
		}

		input.StickerAnimated = isAnimated
		input.MimeType = "image/webp"
		input.StickerLottie = false
	}

	if kind == "sticker_lottie" {
		if !isZip(input.Data) {
			return whatsmeow.SendResponse{}, errors.New("lottie sticker must be a zip archive containing animation.json")
		}
		if input.MimeType == "" {
			input.MimeType = "application/zip"
		}
		input.StickerAnimated = true
		input.StickerLottie = true
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

// FetchMedia downloads and decrypts media for a previously received message ID.
func (s *Service) FetchMedia(ctx context.Context, id string) (MediaFetchResult, error) {
	s.mediaMu.RLock()
	entry, ok := s.mediaStore[id]
	s.mediaMu.RUnlock()
	if !ok {
		var err error
		entry, err = s.loadMediaFromDB(id)
		if err != nil {
			return MediaFetchResult{}, errors.New("media not found or not cached")
		}
	}

	switch entry.Kind {
	case "image":
		data, err := s.client.Download(ctx, entry.Msg.GetImageMessage())
		return MediaFetchResult{Data: data, MimeType: entry.Msg.GetImageMessage().GetMimetype(), Kind: entry.Kind}, err
	case "video":
		data, err := s.client.Download(ctx, entry.Msg.GetVideoMessage())
		return MediaFetchResult{Data: data, MimeType: entry.Msg.GetVideoMessage().GetMimetype(), Kind: entry.Kind}, err
	case "audio", "ptt":
		data, err := s.client.Download(ctx, entry.Msg.GetAudioMessage())
		return MediaFetchResult{Data: data, MimeType: entry.Msg.GetAudioMessage().GetMimetype(), Kind: entry.Kind}, err
	case "document":
		doc := entry.Msg.GetDocumentMessage()
		data, err := s.client.Download(ctx, doc)
		name := doc.GetFileName()
		if name == "" {
			name = doc.GetTitle()
		}
		return MediaFetchResult{Data: data, MimeType: doc.GetMimetype(), FileName: name, Kind: entry.Kind}, err
	case "sticker":
		data, err := s.client.Download(ctx, entry.Msg.GetStickerMessage())
		return MediaFetchResult{Data: data, MimeType: entry.Msg.GetStickerMessage().GetMimetype(), Kind: entry.Kind}, err
	default:
		return MediaFetchResult{}, errors.New("unsupported media kind")
	}
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
	case "sticker", "sticker_lottie":
		return whatsmeow.MediaImage, nil
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
	case "sticker", "sticker_lottie":
		return &waE2E.Message{
			StickerMessage: &waE2E.StickerMessage{
				Mimetype:      proto.String(input.MimeType),
				URL:           proto.String(uploadResp.URL),
				DirectPath:    proto.String(uploadResp.DirectPath),
				MediaKey:      uploadResp.MediaKey,
				FileEncSHA256: uploadResp.FileEncSHA256,
				FileSHA256:    uploadResp.FileSHA256,
				FileLength:    proto.Uint64(uploadResp.FileLength),
				IsAnimated:    proto.Bool(input.StickerAnimated),
			},
		}, nil
	default:
		return nil, errors.New("unsupported media type: " + kind)
	}
}

func (s *Service) getWebhookURL() string {
	s.webhookMu.RLock()
	defer s.webhookMu.RUnlock()
	return s.webhookURL
}

func (s *Service) setLastQRCode(code string) {
	s.qrMu.Lock()
	s.lastQRCode = strings.TrimSpace(code)
	s.qrMu.Unlock()
}

func (s *Service) setQRChannelState(on bool) {
	s.qrMu.Lock()
	s.qrChannelOn = on
	if !on {
		s.lastQRCode = s.lastQRCode // keep last code until session exists or replaced
	}
	s.qrMu.Unlock()
}
