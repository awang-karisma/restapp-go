package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

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

func (s *Service) cacheMedia(id, kind string, message *waE2E.Message) {
	clone := proto.Clone(message).(*waE2E.Message)
	s.mediaMu.Lock()
	s.mediaStore[id] = storedMedia{Msg: clone, Kind: kind}
	s.mediaMu.Unlock()

	if err := s.persistMedia(id, kind, message); err != nil {
		slog.Warn("persist media failed", "id", id, "err", err)
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
	case "sticker", "sticker_lottie":
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
	case "sticker", "sticker_lottie":
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
