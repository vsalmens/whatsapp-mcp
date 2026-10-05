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
//    messages parsed from the phone's "Export chat" text (parsed by the MCP server); these get
//    IDs starting with importIDPrefix. With "keep_ids": true the messages carry their real
//    WhatsApp IDs (e.g. from the phone's own database in a local backup), so messages already
//    stored are skipped. Every inserted message is listed in message_imports.

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
		// Extra per-message data from imports (starred, quoted message, media details, text, ...)
		// as JSON. Upstream never touches this table, so it survives the bridge re-storing a message.
		`CREATE TABLE IF NOT EXISTS message_metadata (
			message_id TEXT,
			chat_jid TEXT,
			source TEXT,
			data TEXT,
			updated_at TIMESTAMP,
			PRIMARY KEY (message_id, chat_jid, source)
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
		KeepIDs  bool   `json:"keep_ids"`
		Source   string `json:"source"`
		Messages []struct {
			ID        string          `json:"id"`
			Timestamp time.Time       `json:"timestamp"`
			Sender    string          `json:"sender"`
			IsFromMe  bool            `json:"is_from_me"`
			Content   string          `json:"content"`
			MediaType string          `json:"media_type"`
			Filename  string          `json:"filename"`
			Metadata  json.RawMessage `json:"metadata"` // stored in message_metadata under source
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
		prefixed := strings.HasPrefix(m.ID, importIDPrefix)
		if m.ID == "" || m.Timestamp.IsZero() || prefixed == req.KeepIDs {
			writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "every message needs a timestamp and an id: " +
				"real WhatsApp IDs with keep_ids, otherwise IDs starting with " + importIDPrefix})
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
	filled, withMetadata := 0, 0
	for _, m := range req.Messages {
		var res sql.Result
		nullable := func(v string) any {
			if v == "" {
				return nil
			}
			return v
		}
		if len(m.Metadata) > 0 && string(m.Metadata) != "null" {
			if _, err = tx.Exec(`INSERT INTO message_metadata (message_id, chat_jid, source, data, updated_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(message_id, chat_jid, source) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
				m.ID, req.ChatJID, req.Source, string(m.Metadata), now); err != nil {
				break
			}
			withMetadata++
		}
		// An existing message is never overwritten; only its empty fields are filled in
		res, err = tx.Exec(`UPDATE messages SET
				content = CASE WHEN COALESCE(content, '') = '' THEN ? ELSE content END,
				media_type = COALESCE(NULLIF(media_type, ''), ?),
				filename = COALESCE(NULLIF(filename, ''), ?),
				sender = COALESCE(NULLIF(sender, ''), ?)
			WHERE id = ? AND chat_jid = ? AND (
				(COALESCE(content, '') = '' AND ? != '') OR (COALESCE(media_type, '') = '' AND ? != '') OR
				(COALESCE(filename, '') = '' AND ? != '') OR (COALESCE(sender, '') = '' AND ? != ''))`,
			m.Content, nullable(m.MediaType), nullable(m.Filename), nullable(m.Sender), m.ID, req.ChatJID,
			m.Content, m.MediaType, m.Filename, m.Sender)
		if err != nil {
			break
		}
		if n, _ := res.RowsAffected(); n > 0 {
			filled++
			continue
		}
		res, err = tx.Exec(`INSERT OR IGNORE INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, req.ChatJID, m.Sender, m.Content, m.Timestamp, m.IsFromMe, nullable(m.MediaType), nullable(m.Filename))
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
	// Imported copies made redundant by real messages (also older chat-export rows)
	removed, err := removeImportDuplicates(store, req.ChatJID)
	if err != nil {
		logger.Warnf("extras: duplicate cleanup failed for %s: %v", req.ChatJID, err)
	}
	logger.Infof("extras: imported %d of %d messages into %s (%d filled in, %d duplicates removed)",
		inserted, len(req.Messages), req.ChatJID, filled, removed)
	writeExtrasJSON(w, http.StatusOK, map[string]any{
		"success": true, "chat_jid": req.ChatJID, "received": len(req.Messages), "inserted": inserted, "filled": filled, "with_metadata": withMetadata, "duplicates_removed": removed,
		"already_present": len(req.Messages) - inserted - filled,
	})
}

// logicalChatJIDs returns the chat JID plus the same contact's other JID (phone number <-> LID)
// from lid_names, so one logical 1:1 chat stored under both is treated as one.
func logicalChatJIDs(store *MessageStore, chatJID string) []string {
	jids := []string{chatJID}
	user, server, ok := strings.Cut(chatJID, "@")
	if !ok {
		return jids
	}
	var alt string
	switch server {
	case types.DefaultUserServer:
		_ = store.db.QueryRow(`SELECT lid || '@lid' FROM lid_names WHERE pn = ? AND lid != '' LIMIT 1`, user).Scan(&alt)
	case types.HiddenUserServer:
		_ = store.db.QueryRow(`SELECT pn || '@s.whatsapp.net' FROM lid_names WHERE lid = ? AND pn != ''`, user).Scan(&alt)
	}
	if alt != "" && alt != chatJID {
		jids = append(jids, alt)
	}
	return jids
}

// unmarkImported records that the bridge itself has now stored these messages (upstream's
// INSERT OR REPLACE overwrote any imported copy), so they no longer count as imported.
func unmarkImported(store *MessageStore, chatJID string, ids []string) {
	for i := 0; i < len(ids); i += 500 {
		batch := ids[i:min(i+500, len(ids))]
		args := []any{chatJID}
		for _, id := range batch {
			args = append(args, id)
		}
		_, _ = store.db.Exec(`DELETE FROM message_imports WHERE chat_jid = ? AND message_id IN (?`+strings.Repeat(", ?", len(batch)-1)+`)`, args...)
	}
}

// removeImportDuplicates deletes imported copies of messages the bridge has stored itself.
// Only rows listed in message_imports (or with import- IDs) are ever deleted:
//   - an imported row whose ID the bridge stored under the contact's other JID (PN <-> LID);
//   - a chat-export row (import- ID, no real ID) when a real message in the same logical chat
//     has the same minute, direction and text.
func removeImportDuplicates(store *MessageStore, chatJID string) (int64, error) {
	jids := logicalChatJIDs(store, chatJID)
	in := "?" + strings.Repeat(", ?", len(jids)-1)
	args := func(times int) []any {
		var a []any
		for range times {
			for _, j := range jids {
				a = append(a, j)
			}
		}
		return a
	}
	var total int64
	res, err := store.db.Exec(`
		DELETE FROM messages WHERE chat_jid IN (`+in+`)
		  AND EXISTS (SELECT 1 FROM message_imports i WHERE i.message_id = messages.id AND i.chat_jid = messages.chat_jid)
		  AND EXISTS (SELECT 1 FROM messages r WHERE r.id = messages.id AND r.chat_jid IN (`+in+`) AND r.chat_jid != messages.chat_jid
		              AND NOT EXISTS (SELECT 1 FROM message_imports i WHERE i.message_id = r.id AND i.chat_jid = r.chat_jid))`,
		args(2)...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	total += n
	res, err = store.db.Exec(`
		DELETE FROM messages WHERE chat_jid IN (`+in+`) AND id LIKE '`+importIDPrefix+`%'
		  AND EXISTS (SELECT 1 FROM messages r WHERE r.chat_jid IN (`+in+`) AND r.id NOT LIKE '`+importIDPrefix+`%'
		              AND r.is_from_me = messages.is_from_me
		              AND CAST(julianday(r.timestamp) * 1440 AS INTEGER) = CAST(julianday(messages.timestamp) * 1440 AS INTEGER)
		              AND TRIM(COALESCE(r.content, '')) = TRIM(COALESCE(messages.content, '')))`,
		args(2)...)
	if err != nil {
		return total, err
	}
	n, _ = res.RowsAffected()
	total += n
	if total > 0 {
		_, err = store.db.Exec(`DELETE FROM message_imports WHERE chat_jid IN (`+in+`)
			AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.id = message_imports.message_id AND m.chat_jid = message_imports.chat_jid)`,
			args(1)...)
	}
	return total, err
}

// restoreTextFromMetadata puts back message text that an import supplied (message_metadata
// "text") when upstream has since re-stored the message without it (INSERT OR REPLACE).
func restoreTextFromMetadata(store *MessageStore, chatJID string) (int64, error) {
	res, err := store.db.Exec(`
		UPDATE messages SET content = (
			SELECT json_extract(md.data, '$.text') FROM message_metadata md
			WHERE md.message_id = messages.id AND md.chat_jid = messages.chat_jid
			  AND COALESCE(json_extract(md.data, '$.text'), '') != '' LIMIT 1)
		WHERE chat_jid = ? AND COALESCE(content, '') = ''
		  AND EXISTS (SELECT 1 FROM message_metadata md WHERE md.message_id = messages.id AND md.chat_jid = messages.chat_jid
		              AND COALESCE(json_extract(md.data, '$.text'), '') != '')`, chatJID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
