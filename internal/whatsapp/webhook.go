package whatsapp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/types"
)

type incomingWebhookPayload struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Author     string `json:"author"`
	GroupID    string `json:"group_id,omitempty"`
	IsGroupMsg bool   `json:"isGroupMsg"`
	IsQuoted   bool   `json:"isQuoted"`
	Timestamp  int64  `json:"timestamp"`
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Caption    string `json:"caption,omitempty"`
	MediaID    string `json:"media_id,omitempty"`
	MimeType   string `json:"mime_type,omitempty"`
	FileName   string `json:"file_name,omitempty"`
	PTT        bool   `json:"ptt,omitempty"`
	DeletedID  string `json:"deleted_id,omitempty"`
	EditedID   string `json:"edited_id,omitempty"`
	EditType   string `json:"edit_type,omitempty"`
	Reaction         string `json:"reaction,omitempty"`
	ReactionTargetID string `json:"reaction_target_id,omitempty"`
	ReactionRemove   bool   `json:"reaction_remove,omitempty"`
	// Ephemeral/disappearing settings
	EphemeralExpiration       int64  `json:"ephemeral_expiration,omitempty"`
	EphemeralSettingTimestamp int64  `json:"ephemeral_setting_timestamp,omitempty"`
	EphemeralInitiator        string `json:"ephemeral_initiator,omitempty"`
	EphemeralTrigger          string `json:"ephemeral_trigger,omitempty"`
	// History sync notification
	HistorySyncType     string `json:"history_sync_type,omitempty"`
	HistorySyncProgress uint32 `json:"history_sync_progress,omitempty"`

	Participants []string `json:"participants,omitempty"`
}

func (s *Service) eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		go s.forwardToWebhook(v)
	case *events.GroupInfo:
		go s.forwardGroupInfoToWebhook(v)
	case *events.JoinedGroup:
		go s.forwardJoinedGroupToWebhook(v)
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
		slog.Error("failed to marshal webhook payload", "err", err)
		return
	}

	resp, err := s.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Error("webhook POST error", "err", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 300 {
		slog.Warn("webhook responded with non-2xx", "status", resp.StatusCode)
	}
}

func (s *Service) forwardGroupInfoToWebhook(info *events.GroupInfo) {
	url := s.getWebhookURL()
	if url == "" {
		return
	}

	payloads := s.buildGroupInfoPayloads(info)
	for _, payload := range payloads {
		body, err := json.Marshal(payload)
		if err != nil {
			slog.Error("failed to marshal group webhook payload", "err", err)
			continue
		}

		resp, err := s.httpClient.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			slog.Error("group webhook POST error", "err", err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 300 {
			slog.Warn("group webhook responded with non-2xx", "status", resp.StatusCode)
		}
	}
}

func (s *Service) forwardJoinedGroupToWebhook(info *events.JoinedGroup) {
	url := s.getWebhookURL()
	if url == "" {
		return
	}

	self := s.selfJID()
	payload := incomingWebhookPayload{
		ID:         string(info.CreateKey),
		From:       jidToString(info.Sender),
		To:         info.GroupInfo.JID.String(),
		Author:     jidToString(info.Sender),
		GroupID:    info.GroupInfo.JID.String(),
		IsGroupMsg: true,
		Type:       "group_joined",
		Timestamp:  time.Now().Unix(),
	}
	if self != "" {
		payload.Participants = []string{self}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal joined group webhook payload", "err", err)
		return
	}

	resp, err := s.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Error("joined group webhook POST error", "err", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		slog.Warn("joined group webhook responded with non-2xx", "status", resp.StatusCode)
	}
}

func (s *Service) buildGroupInfoPayloads(info *events.GroupInfo) []incomingWebhookPayload {
	base := incomingWebhookPayload{
		From:       jidToString(info.Sender),
		To:         info.JID.String(),
		Author:     jidToString(info.Sender),
		GroupID:    info.JID.String(),
		IsGroupMsg: true,
		Timestamp:  info.Timestamp.Unix(),
	}

	var payloads []incomingWebhookPayload

	if len(info.Join) > 0 {
		p := base
		p.Type = "group_participants_added"
		p.Participants = jidsToStrings(info.Join)
		payloads = append(payloads, p)
	}

	if len(info.Leave) > 0 {
		p := base
		p.Type = "group_participants_removed"
		p.Participants = jidsToStrings(info.Leave)
		payloads = append(payloads, p)
	}

	if len(info.Promote) > 0 {
		p := base
		p.Type = "group_promote"
		p.Participants = jidsToStrings(info.Promote)
		payloads = append(payloads, p)
	}

	if len(info.Demote) > 0 {
		p := base
		p.Type = "group_demote"
		p.Participants = jidsToStrings(info.Demote)
		payloads = append(payloads, p)
	}

	return payloads
}

func jidsToStrings(ids []types.JID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

func jidToString(jid *types.JID) string {
	if jid == nil {
		return ""
	}
	return jid.String()
}

func (s *Service) selfJID() string {
	if s.client != nil && s.client.Store != nil && s.client.Store.ID != nil {
		return s.client.Store.ID.String()
	}
	return ""
}

func (s *Service) buildWebhookPayload(msg *events.Message) incomingWebhookPayload {
	isQuoted := isQuotedMessage(msg.Message)

	payload := incomingWebhookPayload{
		ID:         msg.Info.ID,
		From:       msg.Info.Sender.String(),
		To:         msg.Info.Chat.String(),
		Author:     msg.Info.Sender.String(),
		GroupID:    msg.Info.Chat.String(),
		IsGroupMsg: msg.Info.IsGroup,
		IsQuoted:   isQuoted,
		Timestamp:  msg.Info.Timestamp.Unix(),
	}

	message := msg.Message
	if message == nil && msg.RawMessage != nil {
		message = msg.RawMessage
	}

	if message == nil {
		payload.Type = "unknown"
		return payload
	}

	switch {
	case message.GetProtocolMessage() != nil:
		pm := message.GetProtocolMessage()
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			payload.Type = "delete"
			if pm.GetKey() != nil {
				payload.DeletedID = pm.GetKey().GetID()
			}
		case waE2E.ProtocolMessage_EPHEMERAL_SETTING:
			payload.Type = "ephemeral_setting"
			payload.EphemeralExpiration = int64(pm.GetEphemeralExpiration())
			payload.EphemeralSettingTimestamp = pm.GetEphemeralSettingTimestamp()
			if dm := pm.GetDisappearingMode(); dm != nil {
				payload.EphemeralInitiator = dm.GetInitiator().String()
				payload.EphemeralTrigger = dm.GetTrigger().String()
			}
		case waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION:
			payload.Type = "history_sync_notification"
			if hs := pm.GetHistorySyncNotification(); hs != nil {
				payload.HistorySyncType = hs.GetSyncType().String()
				payload.HistorySyncProgress = hs.GetProgress()
			}
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			payload.Type = "edit"
			if pm.GetKey() != nil {
				payload.EditedID = pm.GetKey().GetID()
			}
		default:
			payload.Type = "protocol"
		}
	case message.GetImageMessage() != nil:
		payload.Type = "image"
		payload.Caption = message.GetImageMessage().GetCaption()
		payload.MediaID = msg.Info.ID
		payload.MimeType = message.GetImageMessage().GetMimetype()
		s.cacheMedia(msg.Info.ID, "image", message)
	case message.GetVideoMessage() != nil:
		payload.Type = "video"
		payload.Caption = message.GetVideoMessage().GetCaption()
		payload.MediaID = msg.Info.ID
		payload.MimeType = message.GetVideoMessage().GetMimetype()
		s.cacheMedia(msg.Info.ID, "video", message)
	case message.GetAudioMessage() != nil:
		payload.Type = "audio"
		if message.GetAudioMessage().GetPTT() {
			payload.Type = "ptt"
			payload.PTT = true
		}
		payload.MediaID = msg.Info.ID
		payload.MimeType = message.GetAudioMessage().GetMimetype()
		s.cacheMedia(msg.Info.ID, payload.Type, message)
	case message.GetDocumentMessage() != nil:
		payload.Type = "document"
		payload.Caption = message.GetDocumentMessage().GetCaption()
		payload.MediaID = msg.Info.ID
		payload.MimeType = message.GetDocumentMessage().GetMimetype()
		payload.FileName = message.GetDocumentMessage().GetFileName()
		s.cacheMedia(msg.Info.ID, "document", message)
	case message.GetStickerMessage() != nil:
		payload.Type = "sticker"
		payload.MediaID = msg.Info.ID
		payload.MimeType = message.GetStickerMessage().GetMimetype()
		s.cacheMedia(msg.Info.ID, "sticker", message)
	case message.GetReactionMessage() != nil:
		payload.Type = "reaction"
		rm := message.GetReactionMessage()
		payload.Reaction = rm.GetText()
		if key := rm.GetKey(); key != nil {
			payload.ReactionTargetID = key.GetID()
		}
		if payload.Reaction == "" {
			payload.ReactionRemove = true
		}
	default:
		payload.Type = "text"
		text := ""
		if message != nil {
			text = message.GetConversation()
			if text == "" && message.GetExtendedTextMessage() != nil {
				text = message.GetExtendedTextMessage().GetText()
			}
		}
		payload.Text = text
	}

	// Edits can also be indicated via the message info edit flag even when the protocol wrapper
	// was already unwrapped by the library.
	if payload.Type == "text" && msg.Info.Edit != "" {
		payload.Type = "edit"
		payload.EditedID = string(msg.Info.ID)
		payload.EditType = string(msg.Info.Edit)
	}

	// Advanced privacy: limit sharing changes can be present in context info without protocol wrapper.
	// (Intentionally not emitting limit_sharing events)

	if payload.Type == "delete" || payload.Type == "ephemeral_setting" || payload.Type == "history_sync_notification" {
		return payload
	}

	return payload
}

func isQuotedMessage(msg *waE2E.Message) bool {
	ctx := extractContextInfo(msg)
	return ctx != nil && ctx.QuotedMessage != nil
}

func extractContextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	if msg == nil {
		return nil
	}

	if em := msg.GetEphemeralMessage(); em != nil && em.GetMessage() != nil {
		return extractContextInfo(em.GetMessage())
	}
	if v1 := msg.GetViewOnceMessage(); v1 != nil && v1.GetMessage() != nil {
		return extractContextInfo(v1.GetMessage())
	}
	if v2 := msg.GetViewOnceMessageV2(); v2 != nil && v2.GetMessage() != nil {
		return extractContextInfo(v2.GetMessage())
	}

	switch {
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetContextInfo()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetContextInfo()
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetContextInfo()
	default:
		return nil
	}
}
