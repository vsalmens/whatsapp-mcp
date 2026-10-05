package main

// import_extras.go — chat exports from the phone, and the phone's on-demand history limit.
//
// WhatsApp lets a linked device fetch only part of a chat's history on demand; older messages
// stay on the phone (the phone answers COMPLETE_ON_DEMAND_SYNC_BUT_MORE_MSG_REMAIN_ON_PRIMARY,
// which WhatsApp Web shows as "Older messages can be viewed in WhatsApp on your phone").
//
// 1) history_limits: the oldest message the phone would serve per chat, recorded by
//    /api/history (history_wait_extras.go) so later requests can answer without asking again.
// 2) POST /api/import {"chat_jid": "...", "source": "file name", "messages": [...]} stores
//    messages parsed from the phone's "Export chat" text (parsed by the MCP server).
//    Imported messages get IDs starting with importIDPrefix and are listed in message_imports.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// importIDPrefix marks imported messages; WhatsApp message IDs never contain a hyphen.
const importIDPrefix = "import-"

// historyLimitTTL is how long a recorded limit is trusted before the phone is asked again.
const historyLimitTTL = 7 * 24 * time.Hour

func startImportExtras(store *MessageStore, logger waLog.Logger) {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS history_limits (
			chat_jid TEXT PRIMARY KEY,
			oldest_available TIMESTAMP,
			response_code TEXT,
			checked_at TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS message_imports (
			message_id TEXT,
			chat_jid TEXT,
			source TEXT,
			imported_at TIMESTAMP,
			PRIMARY KEY (message_id, chat_jid)
		)`,
	} {
		if _, err := store.db.Exec(q); err != nil {
			logger.Errorf("extras: failed to create import tables: %v", err)
		}
	}
	http.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
		handleImport(store, logger, w, r)
	})
	logger.Infof("extras: /api/import and history limits enabled")
}

type historyLimit struct {
	ChatJID         string
	OldestAvailable time.Time
	ResponseCode    string
	CheckedAt       time.Time
}

func recordHistoryLimit(store *MessageStore, chatJID string, oldest time.Time, code string) error {
	_, err := store.db.Exec(`
		INSERT INTO history_limits (chat_jid, oldest_available, response_code, checked_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_jid) DO UPDATE SET oldest_available = excluded.oldest_available,
			response_code = excluded.response_code, checked_at = excluded.checked_at`,
		chatJID, oldest, code, time.Now())
	return err
}

// knownHistoryLimit returns a recent limit for any of the chat JIDs whose boundary is still the
// oldest WhatsApp message stored (i.e. nothing older has arrived since it was recorded).
func knownHistoryLimit(store *MessageStore, chats []string, oldestStored time.Time) (historyLimit, bool) {
	for _, c := range chats {
		var l historyLimit
		err := store.db.QueryRow(`SELECT chat_jid, oldest_available, response_code, checked_at FROM history_limits WHERE chat_jid = ?`, c).
			Scan(&l.ChatJID, &l.OldestAvailable, &l.ResponseCode, &l.CheckedAt)
		if err != nil {
			continue
		}
		if time.Since(l.CheckedAt) < historyLimitTTL && !oldestStored.Before(l.OldestAvailable) {
			return l, true
		}
	}
	return historyLimit{}, false
}

func handleImport(store *MessageStore, logger waLog.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeExtrasJSON(w, http.StatusMethodNotAllowed, map[string]any{"success": false, "message": "POST only"})
		return
	}
	var req struct {
		ChatJID  string `json:"chat_jid"`
		ChatName string `json:"chat_name"`
		Source   string `json:"source"`
		Messages []struct {
			ID        string    `json:"id"`
			Timestamp time.Time `json:"timestamp"`
			Sender    string    `json:"sender"`
			IsFromMe  bool      `json:"is_from_me"`
			Content   string    `json:"content"`
		} `json:"messages"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "missing chat_jid or invalid request"})
		return
	}
	if _, err := types.ParseJID(req.ChatJID); err != nil {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "invalid chat_jid: " + err.Error()})
		return
	}
	for _, m := range req.Messages {
		if !strings.HasPrefix(m.ID, importIDPrefix) || m.Timestamp.IsZero() {
			writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "every message needs an id starting with " + importIDPrefix + " and a timestamp"})
			return
		}
	}

	tx, err := store.db.Begin()
	if err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "database error: " + err.Error()})
		return
	}
	defer tx.Rollback()

	// The chat normally exists already; create it if not (keeps a manually set name)
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM chats WHERE jid = ?`, req.ChatJID).Scan(&exists); err == nil && exists == 0 {
		var last time.Time
		for _, m := range req.Messages {
			if m.Timestamp.After(last) {
				last = m.Timestamp
			}
		}
		name := req.ChatName
		if name == "" {
			name = req.ChatJID
		}
		if _, err := tx.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`, req.ChatJID, name, last); err != nil {
			writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "database error: " + err.Error()})
			return
		}
	}

	now := time.Now()
	inserted := 0
	for _, m := range req.Messages {
		var res sql.Result
		res, err = tx.Exec(`INSERT OR IGNORE INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, ?)`,
			m.ID, req.ChatJID, m.Sender, m.Content, m.Timestamp, m.IsFromMe)
		if err != nil {
			break
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
			_, err = tx.Exec(`INSERT OR IGNORE INTO message_imports (message_id, chat_jid, source, imported_at) VALUES (?, ?, ?, ?)`,
				m.ID, req.ChatJID, req.Source, now)
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "database error: " + err.Error()})
		return
	}
	if _, err := fixChatLastMessageTime(store, req.ChatJID); err != nil {
		logger.Warnf("extras: failed to fix last message time of %s: %v", req.ChatJID, err)
	}
	logger.Infof("extras: imported %d of %d messages into %s", inserted, len(req.Messages), req.ChatJID)
	writeExtrasJSON(w, http.StatusOK, map[string]any{
		"success": true, "chat_jid": req.ChatJID, "received": len(req.Messages), "inserted": inserted,
		"already_present": len(req.Messages) - inserted,
	})
}
