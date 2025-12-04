package whatsapp

import (
	"bytes"
	"context"
	"database/sql"
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

	mediaMu    sync.RWMutex
	mediaStore map[string]storedMedia
	db         *sql.DB
	dialect    string

	qrMu        sync.RWMutex
	lastQRCode  string
	qrChannelOn bool
}

type incomingWebhookPayload struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Timestamp int64  `json:"timestamp"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Caption   string `json:"caption,omitempty"`
	MediaID   string `json:"media_id,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	PTT       bool   `json:"ptt,omitempty"`
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

type storedMedia struct {
	Msg  *waE2E.Message
	Kind string
}

type dbMediaRecord struct {
	ID            string
	Kind          string
	MimeType      string
	FileName      string
	Caption       string
	DirectPath    string
	URL           string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
	IsPTT         bool
}

// MediaFetchResult is returned when downloading media on demand.
type MediaFetchResult struct {
	Data     []byte
	MimeType string
	FileName string
	Kind     string
}

const createIncomingMediaTableSQLite = `
CREATE TABLE IF NOT EXISTS incoming_media (
	message_id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	mime_type TEXT,
	file_name TEXT,
	caption TEXT,
	direct_path TEXT,
	url TEXT,
	media_key BLOB,
	file_sha256 BLOB,
	file_enc_sha256 BLOB,
	file_length INTEGER,
	is_ptt BOOLEAN NOT NULL DEFAULT 0
);`

const createIncomingMediaTablePostgres = `
CREATE TABLE IF NOT EXISTS incoming_media (
	message_id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	mime_type TEXT,
	file_name TEXT,
	caption TEXT,
	direct_path TEXT,
	url TEXT,
	media_key BYTEA,
	file_sha256 BYTEA,
	file_enc_sha256 BYTEA,
	file_length BIGINT,
	is_ptt BOOLEAN NOT NULL DEFAULT FALSE
);`

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

	payload := s.buildWebhookPayload(msg)

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

func (s *Service) buildWebhookPayload(msg *events.Message) incomingWebhookPayload {
	payload := incomingWebhookPayload{
		ID:        msg.Info.ID,
		From:      msg.Info.Sender.String(),
		To:        msg.Info.Chat.String(),
		Timestamp: msg.Info.Timestamp.Unix(),
	}

	switch {
	case msg.Message.GetImageMessage() != nil:
		payload.Type = "image"
		payload.Caption = msg.Message.GetImageMessage().GetCaption()
		payload.MediaID = msg.Info.ID
		payload.MimeType = msg.Message.GetImageMessage().GetMimetype()
		s.cacheMedia(msg.Info.ID, "image", msg.Message)
	case msg.Message.GetVideoMessage() != nil:
		payload.Type = "video"
		payload.Caption = msg.Message.GetVideoMessage().GetCaption()
		payload.MediaID = msg.Info.ID
		payload.MimeType = msg.Message.GetVideoMessage().GetMimetype()
		s.cacheMedia(msg.Info.ID, "video", msg.Message)
	case msg.Message.GetAudioMessage() != nil:
		payload.Type = "audio"
		if msg.Message.GetAudioMessage().GetPTT() {
			payload.Type = "ptt"
			payload.PTT = true
		}
		payload.MediaID = msg.Info.ID
		payload.MimeType = msg.Message.GetAudioMessage().GetMimetype()
		s.cacheMedia(msg.Info.ID, payload.Type, msg.Message)
	case msg.Message.GetDocumentMessage() != nil:
		payload.Type = "document"
		payload.Caption = msg.Message.GetDocumentMessage().GetCaption()
		payload.MediaID = msg.Info.ID
		payload.MimeType = msg.Message.GetDocumentMessage().GetMimetype()
		payload.FileName = msg.Message.GetDocumentMessage().GetFileName()
		s.cacheMedia(msg.Info.ID, "document", msg.Message)
	case msg.Message.GetStickerMessage() != nil:
		payload.Type = "sticker"
		payload.MediaID = msg.Info.ID
		payload.MimeType = msg.Message.GetStickerMessage().GetMimetype()
		s.cacheMedia(msg.Info.ID, "sticker", msg.Message)
	default:
		payload.Type = "text"
		text := msg.Message.GetConversation()
		if text == "" && msg.Message.GetExtendedTextMessage() != nil {
			text = msg.Message.GetExtendedTextMessage().GetText()
		}
		payload.Text = text
	}

	return payload
}

func (s *Service) cacheMedia(id, kind string, message *waE2E.Message) {
	clone := proto.Clone(message).(*waE2E.Message)
	s.mediaMu.Lock()
	s.mediaStore[id] = storedMedia{Msg: clone, Kind: kind}
	s.mediaMu.Unlock()

	if err := s.persistMedia(id, kind, message); err != nil {
		log.Printf("persist media %s failed: %v", id, err)
	}
}

func (s *Service) persistMedia(id, kind string, msg *waE2E.Message) error {
	record, err := buildMediaRecord(id, kind, msg)
	if err != nil {
		return err
	}

	if s.dialect == "postgres" {
		_, err = s.db.Exec(
			`INSERT INTO incoming_media (message_id, kind, mime_type, file_name, caption, direct_path, url, media_key, file_sha256, file_enc_sha256, file_length, is_ptt)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			 ON CONFLICT (message_id) DO UPDATE SET
			   kind=EXCLUDED.kind,
			   mime_type=EXCLUDED.mime_type,
			   file_name=EXCLUDED.file_name,
			   caption=EXCLUDED.caption,
			   direct_path=EXCLUDED.direct_path,
			   url=EXCLUDED.url,
			   media_key=EXCLUDED.media_key,
			   file_sha256=EXCLUDED.file_sha256,
			   file_enc_sha256=EXCLUDED.file_enc_sha256,
			   file_length=EXCLUDED.file_length,
			   is_ptt=EXCLUDED.is_ptt`,
			record.ID, record.Kind, record.MimeType, record.FileName, record.Caption, record.DirectPath, record.URL,
			record.MediaKey, record.FileSHA256, record.FileEncSHA256, record.FileLength, record.IsPTT,
		)
		return err
	}

	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO incoming_media
		(message_id, kind, mime_type, file_name, caption, direct_path, url, media_key, file_sha256, file_enc_sha256, file_length, is_ptt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.Kind, record.MimeType, record.FileName, record.Caption, record.DirectPath, record.URL,
		record.MediaKey, record.FileSHA256, record.FileEncSHA256, record.FileLength, record.IsPTT,
	)
	return err
}

func buildMediaRecord(id, kind string, msg *waE2E.Message) (dbMediaRecord, error) {
	rec := dbMediaRecord{ID: id, Kind: kind}
	switch kind {
	case "image":
		im := msg.GetImageMessage()
		if im == nil {
			return rec, errors.New("missing image payload")
		}
		rec.MimeType = im.GetMimetype()
		rec.Caption = im.GetCaption()
		rec.DirectPath = im.GetDirectPath()
		rec.URL = im.GetURL()
		rec.MediaKey = im.GetMediaKey()
		rec.FileSHA256 = im.GetFileSHA256()
		rec.FileEncSHA256 = im.GetFileEncSHA256()
		rec.FileLength = im.GetFileLength()
	case "video":
		vm := msg.GetVideoMessage()
		if vm == nil {
			return rec, errors.New("missing video payload")
		}
		rec.MimeType = vm.GetMimetype()
		rec.Caption = vm.GetCaption()
		rec.DirectPath = vm.GetDirectPath()
		rec.URL = vm.GetURL()
		rec.MediaKey = vm.GetMediaKey()
		rec.FileSHA256 = vm.GetFileSHA256()
		rec.FileEncSHA256 = vm.GetFileEncSHA256()
		rec.FileLength = vm.GetFileLength()
	case "audio", "ptt":
		am := msg.GetAudioMessage()
		if am == nil {
			return rec, errors.New("missing audio payload")
		}
		rec.MimeType = am.GetMimetype()
		rec.DirectPath = am.GetDirectPath()
		rec.URL = am.GetURL()
		rec.MediaKey = am.GetMediaKey()
		rec.FileSHA256 = am.GetFileSHA256()
		rec.FileEncSHA256 = am.GetFileEncSHA256()
		rec.FileLength = am.GetFileLength()
		rec.IsPTT = am.GetPTT()
	case "document":
		dm := msg.GetDocumentMessage()
		if dm == nil {
			return rec, errors.New("missing document payload")
		}
		rec.MimeType = dm.GetMimetype()
		rec.Caption = dm.GetCaption()
		rec.FileName = dm.GetFileName()
		if rec.FileName == "" {
			rec.FileName = dm.GetTitle()
		}
		rec.DirectPath = dm.GetDirectPath()
		rec.URL = dm.GetURL()
		rec.MediaKey = dm.GetMediaKey()
		rec.FileSHA256 = dm.GetFileSHA256()
		rec.FileEncSHA256 = dm.GetFileEncSHA256()
		rec.FileLength = dm.GetFileLength()
	case "sticker":
		sm := msg.GetStickerMessage()
		if sm == nil {
			return rec, errors.New("missing sticker payload")
		}
		rec.MimeType = sm.GetMimetype()
		rec.DirectPath = sm.GetDirectPath()
		rec.URL = sm.GetURL()
		rec.MediaKey = sm.GetMediaKey()
		rec.FileSHA256 = sm.GetFileSHA256()
		rec.FileEncSHA256 = sm.GetFileEncSHA256()
		rec.FileLength = sm.GetFileLength()
	default:
		return rec, errors.New("unsupported media kind")
	}
	return rec, nil
}

func (s *Service) ensureMediaTable(ctx context.Context) error {
	switch s.dialect {
	case "postgres":
		_, err := s.db.ExecContext(ctx, createIncomingMediaTablePostgres)
		return err
	default:
		_, err := s.db.ExecContext(ctx, createIncomingMediaTableSQLite)
		return err
	}
}

func (s *Service) loadMediaFromDB(id string) (storedMedia, error) {
	var rec dbMediaRecord
	var fileLength sql.NullInt64
	query := `SELECT kind, mime_type, file_name, caption, direct_path, url, media_key, file_sha256, file_enc_sha256, file_length, is_ptt
		FROM incoming_media WHERE message_id = ?`
	if s.dialect == "postgres" {
		query = strings.Replace(query, "?", "$1", 1)
	}

	err := s.db.QueryRow(
		query, id,
	).Scan(&rec.Kind, &rec.MimeType, &rec.FileName, &rec.Caption, &rec.DirectPath, &rec.URL,
		&rec.MediaKey, &rec.FileSHA256, &rec.FileEncSHA256, &fileLength, &rec.IsPTT)
	if err != nil {
		return storedMedia{}, err
	}
	if fileLength.Valid && fileLength.Int64 > 0 {
		rec.FileLength = uint64(fileLength.Int64)
	}
	rec.ID = id

	msg, err := buildMessageFromRecord(rec)
	if err != nil {
		return storedMedia{}, err
	}
	entry := storedMedia{Msg: msg, Kind: rec.Kind}

	s.mediaMu.Lock()
	s.mediaStore[id] = entry
	s.mediaMu.Unlock()

	return entry, nil
}

func buildMessageFromRecord(rec dbMediaRecord) (*waE2E.Message, error) {
	switch rec.Kind {
	case "image":
		return &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				URL:           proto.String(rec.URL),
				DirectPath:    proto.String(rec.DirectPath),
				MediaKey:      rec.MediaKey,
				FileSHA256:    rec.FileSHA256,
				FileEncSHA256: rec.FileEncSHA256,
				FileLength:    proto.Uint64(rec.FileLength),
				Mimetype:      proto.String(rec.MimeType),
				Caption:       proto.String(rec.Caption),
			},
		}, nil
	case "video":
		return &waE2E.Message{
			VideoMessage: &waE2E.VideoMessage{
				URL:           proto.String(rec.URL),
				DirectPath:    proto.String(rec.DirectPath),
				MediaKey:      rec.MediaKey,
				FileSHA256:    rec.FileSHA256,
				FileEncSHA256: rec.FileEncSHA256,
				FileLength:    proto.Uint64(rec.FileLength),
				Mimetype:      proto.String(rec.MimeType),
				Caption:       proto.String(rec.Caption),
			},
		}, nil
	case "audio", "ptt":
		return &waE2E.Message{
			AudioMessage: &waE2E.AudioMessage{
				URL:           proto.String(rec.URL),
				DirectPath:    proto.String(rec.DirectPath),
				MediaKey:      rec.MediaKey,
				FileSHA256:    rec.FileSHA256,
				FileEncSHA256: rec.FileEncSHA256,
				FileLength:    proto.Uint64(rec.FileLength),
				Mimetype:      proto.String(rec.MimeType),
				PTT:           proto.Bool(rec.IsPTT),
			},
		}, nil
	case "document":
		name := rec.FileName
		return &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				URL:           proto.String(rec.URL),
				DirectPath:    proto.String(rec.DirectPath),
				MediaKey:      rec.MediaKey,
				FileSHA256:    rec.FileSHA256,
				FileEncSHA256: rec.FileEncSHA256,
				FileLength:    proto.Uint64(rec.FileLength),
				Mimetype:      proto.String(rec.MimeType),
				Caption:       proto.String(rec.Caption),
				FileName:      proto.String(name),
				Title:         proto.String(name),
			},
		}, nil
	case "sticker":
		return &waE2E.Message{
			StickerMessage: &waE2E.StickerMessage{
				URL:           proto.String(rec.URL),
				DirectPath:    proto.String(rec.DirectPath),
				MediaKey:      rec.MediaKey,
				FileSHA256:    rec.FileSHA256,
				FileEncSHA256: rec.FileEncSHA256,
				FileLength:    proto.Uint64(rec.FileLength),
				Mimetype:      proto.String(rec.MimeType),
			},
		}, nil
	default:
		return nil, errors.New("unsupported media kind")
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
	case "sticker":
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
	case "sticker":
		return nil, errors.New("sticker sending not supported via media endpoint")
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
