package main

// media_extras.go — fixed media download for the lharries/whatsapp-mcp bridge.
//
// Upstream bug: extractDirectPathFromURL() strips the query string (?ccb=...&oh=...&oe=...)
// from the media URL. whatsmeow builds the download URL as "https://<host>" + directPath +
// "&hash=...", i.e. it expects the signed query to be present. Without it the URL is
// malformed and the media host answers 403.
//
// This file adds POST /api/download2 {"message_id": "...", "chat_jid": "..."}, which:
//   1) builds the direct path including the signed query and downloads the media
//   2) if the media has expired on WhatsApp's servers (403/404/410), asks the phone to
//      re-upload it (media retry), waits for the new path and tries again
//   3) stores the file under store/<chat>/<message ID><ext> and returns its path
//
// Started from startExtras() (lid_history.go); no extra wiring needed.
// API signatures checked against whatsmeow 8b41cfe (2026-09-29).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const mediaRetryTimeout = 45 * time.Second

var mediaRetryWaiters sync.Map // types.MessageID -> chan *events.MediaRetry

func startMediaExtras(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	client.AddEventHandler(func(evt any) {
		if mr, ok := evt.(*events.MediaRetry); ok {
			if ch, ok := mediaRetryWaiters.Load(mr.MessageID); ok {
				select {
				case ch.(chan *events.MediaRetry) <- mr:
				default:
				}
			}
		}
	})

	http.HandleFunc("/api/download2", func(w http.ResponseWriter, r *http.Request) {
		handleDownload2(client, store, logger, w, r)
	})
	logger.Infof("extras: /api/download2 (fixed media download + media retry) enabled")
}

// directPathWithQuery keeps the signed query string that whatsmeow needs.
func directPathWithQuery(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return ""
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

func isExpiredMediaErr(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

// senderJID converts a stored sender (user part only, or a full JID) into a JID.
func senderJID(ctx context.Context, client *whatsmeow.Client, raw string, chat types.JID) types.JID {
	if strings.Contains(raw, "@") {
		if j, err := types.ParseJID(raw); err == nil {
			return j
		}
	}
	if raw == "" {
		return chat
	}
	// User part only: LID or phone number?
	lid := types.NewJID(raw, types.HiddenUserServer)
	if pn, err := client.Store.LIDs.GetPNForLID(ctx, lid); err == nil && !pn.IsEmpty() {
		return lid
	}
	return types.NewJID(raw, types.DefaultUserServer)
}

func handleDownload2(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeExtrasJSON(w, http.StatusMethodNotAllowed, map[string]any{"success": false, "message": "POST only"})
		return
	}
	var req struct {
		MessageID string `json:"message_id"`
		ChatJID   string `json:"chat_jid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MessageID == "" || req.ChatJID == "" {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "message_id and chat_jid are required"})
		return
	}

	var (
		mediaType, filename, rawURL, sender sql.NullString
		mediaKey, fileSHA256, fileEncSHA256 []byte
		fileLength                          sql.NullInt64
		isFromMe                            bool
		ts                                  time.Time
	)
	err := store.db.QueryRow(`
		SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, sender, is_from_me, timestamp
		FROM messages WHERE id = ? AND chat_jid = ?`, req.MessageID, req.ChatJID).
		Scan(&mediaType, &filename, &rawURL, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength, &sender, &isFromMe, &ts)
	if err == sql.ErrNoRows {
		writeExtrasJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "message not found in the database"})
		return
	} else if err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "database error: " + err.Error()})
		return
	}
	if mediaType.String == "" {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "message has no media"})
		return
	}

	// Upstream names media files by the time the message was stored (e.g. audio_20261002_080052.ogg),
	// so messages stored in the same second of a history sync share one name and the cache check
	// below returned another message's file. Name the file by message ID instead; old
	// timestamp-named files are simply no longer looked up.
	name := mediaFileName(req.MessageID, mediaType.String, filename.String)
	chatDir := filepath.Join("store", strings.ReplaceAll(req.ChatJID, ":", "_"))
	localPath := filepath.Join(chatDir, name)
	absPath, _ := filepath.Abs(localPath)

	if _, err := os.Stat(localPath); err == nil {
		writeExtrasJSON(w, http.StatusOK, map[string]any{"success": true, "path": absPath, "media_type": mediaType.String, "filename": name,
			"message_id": req.MessageID, "original_filename": filename.String, "cached": true})
		return
	}

	if rawURL.String == "" || len(mediaKey) == 0 || len(fileEncSHA256) == 0 {
		writeExtrasJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "message": "media keys missing from the database (old message without metadata)"})
		return
	}

	var waType whatsmeow.MediaType
	switch mediaType.String {
	case "image":
		waType = whatsmeow.MediaImage
	case "video":
		waType = whatsmeow.MediaVideo
	case "audio":
		waType = whatsmeow.MediaAudio
	case "document":
		waType = whatsmeow.MediaDocument
	default:
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "unknown media type: " + mediaType.String})
		return
	}

	dl := &MediaDownloader{
		URL:           rawURL.String,
		DirectPath:    directPathWithQuery(rawURL.String),
		MediaKey:      mediaKey,
		FileLength:    uint64(fileLength.Int64),
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waType,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	data, err := client.Download(ctx, dl)
	retried := false
	if err != nil && isExpiredMediaErr(err) {
		// Media has expired on WhatsApp's servers: ask the phone to re-upload it
		retried = true
		chat, perr := types.ParseJID(req.ChatJID)
		if perr != nil {
			writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "invalid chat_jid"})
			return
		}
		info := &types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     chat,
				IsFromMe: isFromMe,
				IsGroup:  chat.Server == types.GroupServer,
			},
			ID:        types.MessageID(req.MessageID),
			Timestamp: ts,
		}
		if isFromMe && client.Store.ID != nil {
			info.Sender = client.Store.ID.ToNonAD()
		} else {
			info.Sender = senderJID(ctx, client, sender.String, chat)
		}

		ch := make(chan *events.MediaRetry, 1)
		mediaRetryWaiters.Store(info.ID, ch)
		defer mediaRetryWaiters.Delete(info.ID)

		if rerr := client.SendMediaRetryReceipt(ctx, info, mediaKey); rerr != nil {
			writeExtrasJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "failed to send media retry request: " + rerr.Error()})
			return
		}
		logger.Infof("extras: media retry requested for message %s", req.MessageID)

		select {
		case evt := <-ch:
			notif, derr := whatsmeow.DecryptMediaRetryNotification(evt, mediaKey)
			if derr != nil {
				writeExtrasJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "failed to decrypt media retry response: " + derr.Error()})
				return
			}
			if notif.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS || notif.GetDirectPath() == "" {
				writeExtrasJSON(w, http.StatusGone, map[string]any{"success": false, "message": fmt.Sprintf("the phone could not restore the media (result: %s); it has probably been deleted from the phone too", notif.GetResult().String())})
				return
			}
			dl.DirectPath = notif.GetDirectPath()
			data, err = client.Download(ctx, dl)
			if err == nil {
				_, _ = store.db.Exec(`UPDATE messages SET url = ? WHERE id = ? AND chat_jid = ?`,
					"https://mmg.whatsapp.net"+dl.DirectPath, req.MessageID, req.ChatJID)
			}
		case <-time.After(mediaRetryTimeout):
			writeExtrasJSON(w, http.StatusGatewayTimeout, map[string]any{"success": false, "message": "the phone did not answer the media retry request in time (is it online with WhatsApp open?)"})
			return
		case <-ctx.Done():
			writeExtrasJSON(w, http.StatusGatewayTimeout, map[string]any{"success": false, "message": "timeout"})
			return
		}
	}
	if err != nil {
		logger.Warnf("extras: media download failed (%s): %v", req.MessageID, err)
		writeExtrasJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": fmt.Sprintf("download of message %s failed: %v", req.MessageID, err), "message_id": req.MessageID, "retried": retried})
		return
	}

	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "failed to create directory: " + err.Error()})
		return
	}
	if err := os.WriteFile(localPath, data, 0o600); err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "failed to save file: " + err.Error()})
		return
	}
	writeExtrasJSON(w, http.StatusOK, map[string]any{
		"success": true, "path": absPath, "media_type": mediaType.String, "filename": name,
		"message_id": req.MessageID, "original_filename": filename.String, "bytes": len(data), "retried": retried,
	})
}

// mediaFileName returns "<message ID><ext>": unique per chat directory and stable across downloads.
// The extension comes from the stored file name (documents keep theirs) or the media type.
func mediaFileName(messageID, mediaType, storedName string) string {
	ext := strings.ToLower(filepath.Ext(filepath.Base(storedName)))
	if len(ext) < 2 || len(ext) > 10 || strings.ContainsAny(ext, " /\\") {
		ext = ""
	}
	if ext == "" {
		ext = map[string]string{"image": ".jpg", "video": ".mp4", "audio": ".ogg"}[mediaType]
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, messageID)
	return safe + ext
}
