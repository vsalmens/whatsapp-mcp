package main

// revokes_extras.go — records "delete for everyone" (revoke) events.
//
// WhatsApp sends a deletion as a protocol message of type REVOKE whose key names the
// original message; in a history sync the deleted message itself comes back as a stub
// (MessageStubType REVOKE) under its own key, without content. Upstream drops both, so
// the archive kept showing messages their senders had withdrawn. This file stores them in
//   - message_revokes   chat_jid, message_id, revoked_by, revoked_at, source (live | history)
// The messages row is left as it is: the archive keeps what was once sent, and readers
// decide how to show it (export_chat_text replaces the text, message_details flags it).
// For a stub from history the time is the original message's; the exact deletion time is
// not in the sync.
//
// Started from startExtras() (lid_history.go).
// API signatures checked against whatsmeow 8b41cfe (2026-09-29).

import (
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func startRevokeExtras(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	if _, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS message_revokes (
		chat_jid   TEXT NOT NULL,
		message_id TEXT NOT NULL,
		revoked_by TEXT,
		revoked_at TIMESTAMP,
		source     TEXT,
		PRIMARY KEY (chat_jid, message_id)
	)`); err != nil {
		logger.Errorf("extras: failed to create message_revokes table: %v", err)
		return
	}

	self := func() string {
		if client.Store.ID != nil {
			return client.Store.ID.ToNonAD().String()
		}
		return "me"
	}

	// Who a history-sync key stands for: self, the participant in groups, the other party in 1:1 chats
	whoFromKey := func(key *waCommon.MessageKey, chatJID string) string {
		if key == nil {
			return ""
		}
		if key.GetFromMe() {
			return self()
		}
		if p := key.GetParticipant(); p != "" {
			return p
		}
		if rj := key.GetRemoteJID(); rj != "" {
			return rj
		}
		return chatJID
	}

	// The first record of a deletion stands (history batches repeat; a live event came first anyway).
	save := func(chatJID, id, by string, at time.Time, source string) bool {
		if chatJID == "" || id == "" {
			return false
		}
		res, err := store.db.Exec(`INSERT OR IGNORE INTO message_revokes (chat_jid, message_id, revoked_by, revoked_at, source)
			VALUES (?, ?, ?, ?, ?)`, chatJID, id, by, at, source)
		if err != nil {
			logger.Warnf("extras: failed to store deletion (%s): %v", id, err)
			return false
		}
		n, _ := res.RowsAffected()
		return n > 0
	}

	client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {

		case *events.Message:
			pm := v.Message.GetProtocolMessage()
			if pm == nil || pm.GetType() != waE2E.ProtocolMessage_REVOKE {
				return
			}
			by := v.Info.Sender.ToNonAD().String() // in a group an admin may delete another member's message
			if v.Info.IsFromMe {
				by = self()
			}
			if save(v.Info.Chat.String(), pm.GetKey().GetID(), by, v.Info.Timestamp, "live") {
				logger.Infof("extras: message %s deleted for everyone", pm.GetKey().GetID())
			}

		case *events.HistorySync:
			if v.Data == nil {
				return
			}
			n := 0
			for _, conv := range v.Data.GetConversations() {
				chatJID := conv.GetID()
				if parsed, err := types.ParseJID(chatJID); err == nil {
					chatJID = parsed.String()
				}
				for _, hsm := range conv.GetMessages() {
					wm := hsm.GetMessage()
					if wm == nil {
						continue
					}
					at := time.Unix(int64(wm.GetMessageTimestamp()), 0)
					if wm.GetMessageStubType() == waWeb.WebMessageInfo_REVOKE {
						// The deleted message itself, now a stub under its own key
						if save(chatJID, wm.GetKey().GetID(), whoFromKey(wm.GetKey(), chatJID), at, "history") {
							n++
						}
						continue
					}
					// A deletion kept as a protocol message pointing to the original
					if pm := wm.GetMessage().GetProtocolMessage(); pm != nil && pm.GetType() == waE2E.ProtocolMessage_REVOKE && pm.GetKey().GetID() != "" {
						if save(chatJID, pm.GetKey().GetID(), whoFromKey(wm.GetKey(), chatJID), at, "history") {
							n++
						}
					}
				}
			}
			if n > 0 {
				logger.Infof("extras: stored %d deletions from history sync", n)
			}
		}
	})

	logger.Infof("extras: deletion tracking enabled")
}
