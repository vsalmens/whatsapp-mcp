package main

// media_import_extras.go — store media files from an import (e.g. a local iPhone backup).
//
// POST /api/import_media?chat_jid=...&message_id=...&name=<original file name>  (body: file bytes)
// The message must already be stored with a media type. The file is saved where /api/download2
// looks for it (store/<chat>/<message ID><ext>), so download_media then works without the phone.
// An existing file is never replaced; an empty filename column is filled with the original name.

import (
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	waLog "go.mau.fi/whatsmeow/util/log"
)

const maxImportMediaBytes = 200 << 20

func startMediaImportExtras(store *MessageStore, logger waLog.Logger) {
	http.HandleFunc("/api/import_media", func(w http.ResponseWriter, r *http.Request) {
		handleImportMedia(store, logger, w, r)
	})
}

func handleImportMedia(store *MessageStore, logger waLog.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeExtrasJSON(w, http.StatusMethodNotAllowed, map[string]any{"success": false, "message": "POST only"})
		return
	}
	q := r.URL.Query()
	chatJID, messageID, name := q.Get("chat_jid"), q.Get("message_id"), filepath.Base(q.Get("name"))
	if chatJID == "" || messageID == "" {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "chat_jid and message_id are required"})
		return
	}
	var mediaType, filename sql.NullString
	err := store.db.QueryRow(`SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?`, messageID, chatJID).
		Scan(&mediaType, &filename)
	if err == sql.ErrNoRows {
		writeExtrasJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "message not found"})
		return
	} else if err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "database error: " + err.Error()})
		return
	}
	if mediaType.String == "" {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "message has no media"})
		return
	}
	if filename.String == "" && name != "." && name != "" {
		_, _ = store.db.Exec(`UPDATE messages SET filename = ? WHERE id = ? AND chat_jid = ? AND COALESCE(filename, '') = ''`,
			name, messageID, chatJID)
		filename.String = name
	}

	file := mediaFileName(messageID, mediaType.String, filename.String)
	chatDir := filepath.Join("store", strings.ReplaceAll(chatJID, ":", "_"))
	localPath := filepath.Join(chatDir, file)
	if _, err := os.Stat(localPath); err == nil {
		writeExtrasJSON(w, http.StatusOK, map[string]any{"success": true, "status": "exists", "filename": file})
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportMediaBytes))
	if err != nil || len(data) == 0 {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "missing or too large file body"})
		return
	}
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "failed to create directory: " + err.Error()})
		return
	}
	// Write to a temporary name first so a failed write never leaves a partial file behind
	tmp := localPath + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "failed to save file: " + err.Error()})
		return
	}
	if err := os.Rename(tmp, localPath); err != nil {
		_ = os.Remove(tmp)
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "failed to save file: " + err.Error()})
		return
	}
	writeExtrasJSON(w, http.StatusOK, map[string]any{"success": true, "status": "stored", "filename": file, "bytes": len(data)})
}
