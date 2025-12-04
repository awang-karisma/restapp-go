package whatsapp

import (
	"bytes"
	"encoding/json"
	"io"
	"log"

	"go.mau.fi/whatsmeow/types/events"
)

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
