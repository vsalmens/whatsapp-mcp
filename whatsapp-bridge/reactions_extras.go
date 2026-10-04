package main

// reactions_extras.go — stores WhatsApp reactions (emoji) and message edits.
//
// The upstream bridge drops reactions and edits because they carry no text content.
// This file listens to
//   - live reactions (events.Message with a ReactionMessage) and live edits
//   - reactions in history syncs (WebMessageInfo.Reactions, MessageAddOns[REACTION] and
//     standalone ReactionMessage entries) and edited messages (protocol wrappers)
// and stores them in:
//   - reactions        current reaction per message + reactor (empty emoji = removed)
//   - reaction_events  append-only log of every observed reaction, change and removal
//   - message_edits    version history: 0 = original, 1.. = edits with timestamps
//
// Debugging: set WA_DEBUG_MSG=<message ID> and/or WA_DEBUG_TEXT=<substring> to log the
// raw structure of matching messages (and a ±20 min window) when they arrive in a
// history sync. Note: debug output contains message content.
//
// Started from startExtras() (lid_history.go).
// API signatures checked against whatsmeow 8b41cfe (2026-09-29).

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func startReactionExtras(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	if _, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS reactions (
		chat_jid   TEXT NOT NULL,
		message_id TEXT NOT NULL,
		sender     TEXT NOT NULL,
		emoji      TEXT NOT NULL,
		timestamp  TIMESTAMP,
		PRIMARY KEY (chat_jid, message_id, sender)
	)`); err != nil {
		logger.Errorf("extras: failed to create reactions table: %v", err)
		return
	}
	_, _ = store.db.Exec(`CREATE INDEX IF NOT EXISTS idx_reactions_msg ON reactions (chat_jid, message_id)`)
	// Reaction change log (append-only): every observed reaction, change and removal (emoji = '')
	if _, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS reaction_events (
		chat_jid   TEXT NOT NULL,
		message_id TEXT NOT NULL,
		sender     TEXT NOT NULL,
		emoji      TEXT NOT NULL,
		timestamp  TIMESTAMP NOT NULL,
		source     TEXT,
		PRIMARY KEY (chat_jid, message_id, sender, timestamp, emoji)
	)`); err != nil {
		logger.Errorf("extras: failed to create reaction_events table: %v", err)
	}
	// Message version history: version 0 = original, 1.. = edits with timestamps
	if _, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS message_edits (
		chat_jid   TEXT NOT NULL,
		message_id TEXT NOT NULL,
		version    INTEGER NOT NULL,
		content    TEXT,
		edited_at  TIMESTAMP,
		PRIMARY KEY (chat_jid, message_id, version)
	)`); err != nil {
		logger.Errorf("extras: failed to create message_edits table: %v", err)
	}

	me := func() string {
		if client.Store.ID != nil {
			return client.Store.ID.ToNonAD().String()
		}
		return "me"
	}

	saveReaction := func(chatJID, targetID, sender, emoji string, ts time.Time, source string) {
		if chatJID == "" || targetID == "" || sender == "" {
			return
		}
		// Change log: the same event is stored only once (history batches repeat)
		_, _ = store.db.Exec(`INSERT OR IGNORE INTO reaction_events (chat_jid, message_id, sender, emoji, timestamp, source)
			VALUES (?, ?, ?, ?, ?, ?)`, chatJID, targetID, sender, emoji, ts, source)
		var err error
		if emoji == "" {
			_, err = store.db.Exec(`DELETE FROM reactions WHERE chat_jid = ? AND message_id = ? AND sender = ?`,
				chatJID, targetID, sender)
		} else {
			_, err = store.db.Exec(`
				INSERT INTO reactions (chat_jid, message_id, sender, emoji, timestamp) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(chat_jid, message_id, sender) DO UPDATE SET
					emoji = excluded.emoji, timestamp = excluded.timestamp
				WHERE excluded.timestamp >= reactions.timestamp OR reactions.timestamp IS NULL`,
				chatJID, targetID, sender, emoji, ts)
		}
		if err != nil {
			logger.Warnf("extras: failed to store reaction: %v", err)
		}
	}

	// Reactor from a history-sync key: self, the participant in groups, the other party in 1:1 chats
	reactorFromKey := func(key *waCommon.MessageKey, chatJID string) string {
		if key == nil {
			return ""
		}
		if key.GetFromMe() {
			return me()
		}
		if p := key.GetParticipant(); p != "" {
			return p
		}
		if rj := key.GetRemoteJID(); rj != "" {
			return rj
		}
		return chatJID
	}

	msToTime := func(ms int64, fallback time.Time) time.Time {
		if ms > 0 {
			return time.UnixMilli(ms)
		}
		return fallback
	}

	// Edited messages: WhatsApp sends an edit as a protocol message that points to the original ID.
	// Upstream drops these, so we version and update the original message ourselves.
	saveEdit := func(chatJID, origID string, edited *waE2E.Message, editedAt time.Time) bool {
		text := plainMsgText(edited)
		if origID == "" || text == "" {
			return false
		}
		var curContent sql.NullString
		var origTS time.Time
		if err := store.db.QueryRow(`SELECT content, timestamp FROM messages WHERE id = ? AND chat_jid = ?`,
			origID, chatJID).Scan(&curContent, &origTS); err != nil {
			return false // original message is not in the database (yet)
		}
		var maxVer sql.NullInt64
		var lastContent sql.NullString
		_ = store.db.QueryRow(`SELECT MAX(version) FROM message_edits WHERE chat_jid = ? AND message_id = ?`,
			chatJID, origID).Scan(&maxVer)
		if !maxVer.Valid {
			// First edit: keep the original version with its original timestamp
			if _, err := store.db.Exec(`INSERT OR IGNORE INTO message_edits (chat_jid, message_id, version, content, edited_at) VALUES (?, ?, 0, ?, ?)`,
				chatJID, origID, curContent.String, origTS); err != nil {
				logger.Warnf("extras: failed to store original version (%s): %v", origID, err)
				return false
			}
			maxVer = sql.NullInt64{Int64: 0, Valid: true}
		}
		_ = store.db.QueryRow(`SELECT content FROM message_edits WHERE chat_jid = ? AND message_id = ? AND version = ?`,
			chatJID, origID, maxVer.Int64).Scan(&lastContent)
		if lastContent.String == text {
			return false // same version already stored (repeated history batch)
		}
		if _, err := store.db.Exec(`INSERT INTO message_edits (chat_jid, message_id, version, content, edited_at) VALUES (?, ?, ?, ?, ?)`,
			chatJID, origID, maxVer.Int64+1, text, editedAt); err != nil {
			logger.Warnf("extras: failed to store edit (%s): %v", origID, err)
			return false
		}
		_, _ = store.db.Exec(`UPDATE messages SET content = ? WHERE id = ? AND chat_jid = ?`, text, origID, chatJID)
		return true
	}

	client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {

		case *events.Message:
			if pm := v.Message.GetProtocolMessage(); pm != nil && pm.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT {
				if saveEdit(v.Info.Chat.String(), pm.GetKey().GetID(), pm.GetEditedMessage(),
					msToTime(pm.GetTimestampMS(), v.Info.Timestamp)) {
					logger.Infof("extras: message %s edited", pm.GetKey().GetID())
				}
				return
			}
			rm := v.Message.GetReactionMessage()
			if rm == nil {
				return
			}
			sender := v.Info.Sender.ToNonAD().String()
			if v.Info.IsFromMe {
				sender = me()
			}
			saveReaction(v.Info.Chat.String(), rm.GetKey().GetID(), sender, rm.GetText(),
				msToTime(rm.GetSenderTimestampMS(), v.Info.Timestamp), "live")

		case *events.HistorySync:
			if v.Data == nil {
				return
			}
			n, edits := 0, 0
			for _, conv := range v.Data.GetConversations() {
				chatJID := conv.GetID()
				if parsed, err := types.ParseJID(chatJID); err == nil {
					chatJID = parsed.String()
				}
				var minTS, maxTS uint64
				dbgFound := false
				dbgID := os.Getenv("WA_DEBUG_MSG")
				dbgText := os.Getenv("WA_DEBUG_TEXT")
				var dbgTS time.Time
				if dbgID != "" {
					_ = store.db.QueryRow(`SELECT timestamp FROM messages WHERE id = ?`, dbgID).Scan(&dbgTS)
				}
				for _, hsm := range conv.GetMessages() {
					wm := hsm.GetMessage()
					if wm == nil {
						continue
					}
					if t := wm.GetMessageTimestamp(); t > 0 {
						if minTS == 0 || t < minTS {
							minTS = t
						}
						if t > maxTS {
							maxTS = t
						}
					}
					targetID := wm.GetKey().GetID()
					if dbgID != "" && targetID == dbgID {
						dbgFound = true
					}
					if !dbgTS.IsZero() || dbgText != "" {
						t := time.Unix(int64(wm.GetMessageTimestamp()), 0)
						txt := debugMsgText(wm.GetMessage())
						if !dbgTS.IsZero() && t.Sub(dbgTS) > -20*time.Minute && t.Sub(dbgTS) < 20*time.Minute {
							short := []rune(txt)
							if len(short) > 70 {
								short = short[:70]
							}
							logger.Infof("extras DEBUG window: id=%s time=%s fromMe=%v participant=%s stub=%v fields=[%s] reactions=%d addons=%d text=%q",
								targetID, t.Format("2.1. 15:04:05"), wm.GetKey().GetFromMe(), wm.GetParticipant(),
								wm.GetMessageStubType(), debugMsgKinds(wm.GetMessage()),
								len(wm.GetReactions()), len(wm.GetMessageAddOns()), string(short))
						}
						if dbgText != "" && strings.Contains(txt, dbgText) {
							logger.Infof("extras DEBUG TEXT %s: %s", targetID, protojson.Format(wm))
						}
					}
					// Debugging: WA_DEBUG_MSG=<message ID> dumps the raw message structure to the log
					if dbgID != "" && dbgID == targetID {
						logger.Infof("extras DEBUG %s: %s", targetID, protojson.Format(wm))
					}
					// Edited message in history: a protocol wrapper with its own ID pointing to the original.
					// Reactions hang on the wrapper but belong to the original message.
					if pm := wm.GetMessage().GetProtocolMessage(); pm != nil && pm.GetEditedMessage() != nil && pm.GetKey().GetID() != "" {
						origID := pm.GetKey().GetID()
						if saveEdit(chatJID, origID, pm.GetEditedMessage(),
							msToTime(pm.GetTimestampMS(), time.Unix(int64(wm.GetMessageTimestamp()), 0))) {
							edits++
						}
						targetID = origID
					}
					// Viestiin liitetyt reaktiot
					for _, r := range wm.GetReactions() {
						saveReaction(chatJID, targetID, reactorFromKey(r.GetKey(), chatJID), r.GetText(),
							msToTime(r.GetSenderTimestampMS(), time.Unix(int64(wm.GetMessageTimestamp()), 0)), "history")
						n++
					}
					// Uudempi muoto: reaktiot "add-oneina" (MessageAddOns, tyyppi REACTION)
					for _, ao := range wm.GetMessageAddOns() {
						if ao.GetMessageAddOnType() != waWeb.MessageAddOn_REACTION {
							continue
						}
						rm := ao.GetMessageAddOn().GetReactionMessage()
						if rm == nil {
							continue
						}
						tid := rm.GetKey().GetID()
						if tid == "" {
							tid = targetID
						}
						saveReaction(chatJID, tid, reactorFromKey(ao.GetMessageAddOnKey(), chatJID), rm.GetText(),
							msToTime(rm.GetSenderTimestampMS(), msToTime(ao.GetSenderTimestampMS(), time.Unix(int64(wm.GetMessageTimestamp()), 0))), "history")
						n++
					}
					// Erillinen reaktioviesti historiassa
					if rm := wm.GetMessage().GetReactionMessage(); rm != nil {
						sender := reactorFromKey(wm.GetKey(), chatJID)
						if p := wm.GetParticipant(); p != "" && !wm.GetKey().GetFromMe() {
							sender = p
						}
						saveReaction(chatJID, rm.GetKey().GetID(), sender, rm.GetText(),
							msToTime(rm.GetSenderTimestampMS(), time.Unix(int64(wm.GetMessageTimestamp()), 0)), "history")
						n++
					}
				}
				dbgInfo := ""
				if dbgID != "" {
					dbgInfo = fmt.Sprintf(", debug message %s included: %v", dbgID, dbgFound)
				}
				logger.Infof("extras: history batch %s: %d messages, %s – %s%s",
					chatJID, len(conv.GetMessages()),
					time.Unix(int64(minTS), 0).Format("2.1. 15:04"), time.Unix(int64(maxTS), 0).Format("2.1. 15:04"),
					dbgInfo)
			}
			if n > 0 || edits > 0 {
				logger.Infof("extras: stored %d reactions and %d edits from history sync", n, edits)
			}
		}
	})

	logger.Infof("extras: reaction and edit tracking enabled")
}

// --- debugging helpers ---

func debugMsgKinds(m *waE2E.Message) string {
	if m == nil {
		return "nil"
	}
	var names []string
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		names = append(names, string(fd.Name()))
		return true
	})
	return strings.Join(names, ",")
}

func debugMsgText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if t := m.GetConversation(); t != "" {
		return t
	}
	if t := m.GetExtendedTextMessage().GetText(); t != "" {
		return t
	}
	if em := m.GetEditedMessage().GetMessage(); em != nil {
		return "[edited] " + debugMsgText(em)
	}
	if pm := m.GetProtocolMessage(); pm != nil && pm.GetEditedMessage() != nil {
		return "[edit→" + pm.GetKey().GetID() + "] " + debugMsgText(pm.GetEditedMessage())
	}
	return ""
}

// plainMsgText returns the message text (including captions) as upstream stores it in content.
func plainMsgText(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if t := m.GetConversation(); t != "" {
		return t
	}
	if t := m.GetExtendedTextMessage().GetText(); t != "" {
		return t
	}
	if t := m.GetImageMessage().GetCaption(); t != "" {
		return t
	}
	if t := m.GetVideoMessage().GetCaption(); t != "" {
		return t
	}
	return m.GetDocumentMessage().GetCaption()
}
