package main

// lid_history.go — extras for the lharries/whatsapp-mcp bridge.
//
// 1) LID names: resolves @lid pseudonyms to phone numbers via whatsmeow's LID
//    map, looks up the contact name and stores it in the lid_names table of
//    messages.db. Also fixes chat names of 1:1 chats that only show a raw ID.
//    Runs 20 s after start-up and every 10 minutes after that.
//
// 2) On-demand history: POST /api/history (history_wait_extras.go) asks the
//    user's own phone for older messages of one chat. The request is a peer
//    message sent ONLY to the user's own devices.
//
// Wiring: add this line after the startRESTServer(...) call in main.go:
//     startExtras(client, messageStore, logger)
//
// API signatures checked against whatsmeow 8b41cfe (2026-09-29).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const lidRefreshInterval = 10 * time.Minute

// SelfChatName is the display name used for the user's own "message yourself" chat.
const SelfChatName = "Me (note to self)"

func startExtras(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	if _, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS lid_names (
		lid TEXT PRIMARY KEY,
		pn TEXT,
		name TEXT,
		updated_at TIMESTAMP
	)`); err != nil {
		logger.Errorf("extras: failed to create lid_names table: %v", err)
	}

	startOnDemandHistoryLogging(client, store, logger) // history_ondemand_extras.go

	// One-time repair of chats whose last message time was moved back by history batches
	if n, err := fixChatLastMessageTime(store, ""); err != nil {
		logger.Warnf("extras: failed to fix chat last message times: %v", err)
	} else if n > 0 {
		logger.Infof("extras: fixed last message time of %d chats", n)
	}

	http.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		handleHistoryRequest(client, store, logger, w, r)
	})

	http.HandleFunc("/api/refresh_names", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeExtrasJSON(w, http.StatusMethodNotAllowed, map[string]any{"success": false, "message": "POST only"})
			return
		}
		resolved, renamed := refreshLIDNames(client, store, logger)
		writeExtrasJSON(w, http.StatusOK, map[string]any{
			"success":       true,
			"resolved_lids": resolved,
			"renamed_chats": renamed,
		})
	})

	go func() {
		time.Sleep(20 * time.Second) // let the connection and initial sync settle
		for {
			if client.IsConnected() && client.Store.ID != nil {
				resolved, renamed := refreshLIDNames(client, store, logger)
				logger.Infof("extras: LID names refreshed (%d resolved, %d chat names fixed)", resolved, renamed)
			}
			time.Sleep(lidRefreshInterval)
		}
	}()

	logger.Infof("extras: LID names and /api/history enabled")

	startMediaExtras(client, store, logger)    // media_extras.go
	startReactionExtras(client, store, logger) // reactions_extras.go
}

// contactDisplayName returns the best available name from the contact store.
func contactDisplayName(ctx context.Context, client *whatsmeow.Client, jid types.JID) string {
	c, err := client.Store.Contacts.GetContact(ctx, jid)
	if err != nil || !c.Found {
		return ""
	}
	for _, n := range []string{c.FullName, c.FirstName, c.BusinessName, c.PushName} {
		if strings.TrimSpace(n) != "" {
			return n
		}
	}
	return ""
}

// resolveLID maps a LID user part to a phone number JID and a display name.
// ok is true if either was found.
func resolveLID(ctx context.Context, client *whatsmeow.Client, user string) (pn types.JID, name string, ok bool) {
	lid := types.NewJID(user, types.HiddenUserServer)

	if mapped, err := client.Store.LIDs.GetPNForLID(ctx, lid); err == nil && !mapped.IsEmpty() {
		pn = mapped
		ok = true
		if client.Store.ID != nil && mapped.User == client.Store.ID.User {
			return pn, SelfChatName, true
		}
		name = contactDisplayName(ctx, client, mapped)
	}

	// The push name may be stored directly under the LID (group members)
	if name == "" {
		if n := contactDisplayName(ctx, client, lid); n != "" {
			name = n
			ok = true
		}
	}
	return pn, name, ok
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// refreshLIDNames resolves all known LID candidates and updates names.
func refreshLIDNames(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) (resolved int, renamed int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Collect candidates first and write afterwards (no locking while iterating)
	candidates := map[string]bool{}
	rows, err := store.db.Query(`
		SELECT jid FROM chats WHERE jid LIKE '%@lid'
		UNION
		SELECT DISTINCT sender FROM messages WHERE sender IS NOT NULL AND sender != ''`)
	if err != nil {
		logger.Warnf("extras: failed to query LID candidates: %v", err)
		return 0, 0
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			continue
		}
		user := s
		if i := strings.Index(s, "@"); i >= 0 {
			if !strings.HasSuffix(s, "@"+types.HiddenUserServer) {
				continue // regular phone number or group JID
			}
			user = s[:i]
		}
		if isAllDigits(user) {
			candidates[user] = true
		}
	}
	rows.Close()

	now := time.Now()
	for user := range candidates {
		pn, name, ok := resolveLID(ctx, client, user)
		if !ok {
			continue
		}
		resolved++

		pnUser := ""
		if !pn.IsEmpty() {
			pnUser = pn.User
		}
		if _, err := store.db.Exec(`
			INSERT INTO lid_names (lid, pn, name, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(lid) DO UPDATE SET pn = excluded.pn, name = excluded.name, updated_at = excluded.updated_at`,
			user, pnUser, name, now); err != nil {
			logger.Warnf("extras: failed to write lid_names (%s): %v", user, err)
			continue
		}

		if name == "" {
			continue
		}
		// Only replace names that are raw IDs; never touch manually set names
		res, err := store.db.Exec(`
			UPDATE chats SET name = ?
			WHERE jid = ? AND (name IS NULL OR name = '' OR name = ? OR name = ?)`,
			name, user+"@"+types.HiddenUserServer, user, user+"@"+types.HiddenUserServer)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				renamed += int(n)
			}
		}
	}
	renamed += renamePNChats(ctx, client, store, logger)
	return resolved, renamed
}

// renamePNChats names phone-number chats that still show a raw number. Upstream names a chat
// only when it is first stored, often before the phone has synced contacts, and never again.
// Like the LID pass, it only replaces names that are raw IDs, never manually set names.
func renamePNChats(ctx context.Context, client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) (renamed int) {
	rows, err := store.db.Query(`
		SELECT jid FROM chats
		WHERE jid LIKE '%@' || ? AND (name IS NULL OR name = '' OR name = jid OR name = substr(jid, 1, instr(jid, '@') - 1))`,
		types.DefaultUserServer)
	if err != nil {
		logger.Warnf("extras: failed to query unnamed chats: %v", err)
		return 0
	}
	var jids []types.JID
	for rows.Next() {
		var s string
		if rows.Scan(&s) != nil {
			continue
		}
		if jid, err := types.ParseJID(s); err == nil {
			jids = append(jids, jid)
		}
	}
	rows.Close()

	for _, jid := range jids {
		name := contactDisplayName(ctx, client, jid)
		if client.Store.ID != nil && jid.User == client.Store.ID.User {
			name = SelfChatName
		}
		if name == "" {
			continue
		}
		res, err := store.db.Exec(`
			UPDATE chats SET name = ?
			WHERE jid = ? AND (name IS NULL OR name = '' OR name = jid OR name = ?)`,
			name, jid.String(), jid.User)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				renamed += int(n)
			}
		}
	}
	return renamed
}

func writeExtrasJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
