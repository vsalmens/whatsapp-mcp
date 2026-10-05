package main

// history_wait_extras.go — on-demand history requests that report what the phone answered.
//
// POST /api/history {"chat_jid": "...", "count": 50}
//   optional: "from_newest": true, "anchor_message_id": "...",
//             "wait": false (old fire-and-forget behaviour), "wait_seconds": 15
//
// By default the handler waits for the phone's ON_DEMAND history sync for the chat and returns
// what arrived: phone_responded, received_count, end_of_history_type (the phone's own
// "more messages remain / no more / no access" flag), history_exhausted and anchor_used.
// If the phone answers with zero messages without saying that nothing remains, the request is
// retried with other anchors (the oldest message sent by the user, the next-oldest ones) and,
// for 1:1 chats, with the alternate LID / phone-number JID of the same contact.
// If the phone does not answer at all, the handler says so instead of retrying.
//
// Answers are matched to requests by chat JID (the history sync carries no request ID that
// whatsmeow exposes reliably), so a late answer to an earlier request may be counted once.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	historyDefaultWait = 15 * time.Second
	historyTotalBudget = 45 * time.Second // all attempts together
	historyMaxAnchors  = 5
	historyMaxAttempts = 6
)

// onDemandResult is one conversation of an ON_DEMAND history sync.
type onDemandResult struct {
	ChatJID        string
	Received       int
	Oldest, Newest time.Time
	EndOfHistory   bool
	EndType        string // set only when the phone sent the field
	SessionID      string
	PeerCode       string // response code from a peer data response, if one arrived
}

type historyWaiter struct {
	ch chan onDemandResult
}

var (
	onDemandMu      sync.Mutex
	onDemandWaiters = map[string]map[*historyWaiter]bool{} // chat JID -> waiters
	peerCodeWaiters sync.Map                               // request (stanza) ID -> chan string
)

func addOnDemandWaiter(jids []string) (*historyWaiter, func()) {
	w := &historyWaiter{ch: make(chan onDemandResult, 8)}
	onDemandMu.Lock()
	for _, j := range jids {
		if onDemandWaiters[j] == nil {
			onDemandWaiters[j] = map[*historyWaiter]bool{}
		}
		onDemandWaiters[j][w] = true
	}
	onDemandMu.Unlock()
	return w, func() {
		onDemandMu.Lock()
		for _, j := range jids {
			delete(onDemandWaiters[j], w)
			if len(onDemandWaiters[j]) == 0 {
				delete(onDemandWaiters, j)
			}
		}
		onDemandMu.Unlock()
	}
}

// deliverOnDemand hands a result to every waiter registered under any of the given JIDs (once each).
func deliverOnDemand(res onDemandResult, jids ...string) {
	seen := map[*historyWaiter]bool{}
	onDemandMu.Lock()
	defer onDemandMu.Unlock()
	for _, j := range jids {
		for w := range onDemandWaiters[j] {
			if seen[w] {
				continue
			}
			seen[w] = true
			select {
			case w.ch <- res:
			default:
			}
		}
	}
}

func deliverPeerCode(requestID, code string) {
	if ch, ok := peerCodeWaiters.Load(requestID); ok {
		select {
		case ch.(chan string) <- code:
		default:
		}
	}
}

// onDemandResultFromConversation summarises one conversation of an ON_DEMAND sync.
func onDemandResultFromConversation(conv *waHistorySync.Conversation, notifSession string) onDemandResult {
	res := onDemandResult{
		ChatJID:      conv.GetID(),
		Received:     len(conv.GetMessages()),
		EndOfHistory: conv.GetEndOfHistoryTransfer(),
		SessionID:    notifSession,
	}
	if conv.EndOfHistoryTransferType != nil {
		res.EndType = conv.GetEndOfHistoryTransferType().String()
	}
	for _, m := range conv.GetMessages() {
		ts := m.GetMessage().GetMessageTimestamp()
		if ts == 0 {
			continue
		}
		t := time.Unix(int64(ts), 0)
		if res.Oldest.IsZero() || t.Before(res.Oldest) {
			res.Oldest = t
		}
		if t.After(res.Newest) {
			res.Newest = t
		}
	}
	return res
}

// handleOnDemandSync is called from the history logging handler for every ON_DEMAND sync.
func handleOnDemandSync(v *events.HistorySync, logger waLog.Logger) {
	session := ""
	if v.Notification != nil {
		session = v.Notification.GetPeerDataRequestSessionID()
	}
	for _, conv := range v.Data.GetConversations() {
		res := onDemandResultFromConversation(conv, session)
		logger.Infof("extras: on-demand history %s: %d messages (end_of_history=%v type=%s pn=%s lid=%s session=%s)",
			conv.GetID(), res.Received, res.EndOfHistory, valueOr(res.EndType, "-"),
			valueOr(conv.GetPnJID(), "-"), valueOr(conv.GetLidJID(), "-"), valueOr(session, "-"))
		deliverOnDemand(res, conv.GetID(), conv.GetPnJID(), conv.GetLidJID())
	}
}

func valueOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// alternateChatJID returns the LID for a phone-number 1:1 chat and vice versa (empty if unknown).
func alternateChatJID(ctx context.Context, client *whatsmeow.Client, chat types.JID) types.JID {
	switch chat.Server {
	case types.DefaultUserServer:
		if lid, err := client.Store.LIDs.GetLIDForPN(ctx, chat); err == nil {
			return lid.ToNonAD()
		}
	case types.HiddenUserServer:
		if pn, err := client.Store.LIDs.GetPNForLID(ctx, chat); err == nil {
			return pn.ToNonAD()
		}
	}
	return types.EmptyJID
}

type historyAnchor struct {
	ID       string    `json:"id"`
	StoredIn string    `json:"stored_in"` // chat JID the message is stored under
	TS       time.Time `json:"-"`
	FromMe   bool      `json:"from_me"`
}

func queryAnchors(store *MessageStore, query string, args ...any) ([]historyAnchor, error) {
	rows, err := store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []historyAnchor
	for rows.Next() {
		var a historyAnchor
		if err := rows.Scan(&a.ID, &a.StoredIn, &a.TS, &a.FromMe); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// anchorCandidates lists anchors in the order they are tried: the oldest stored message, the
// oldest message sent by the user (its key is always known to the phone), then the next-oldest.
func anchorCandidates(store *MessageStore, chats []string, fromNewest bool, anchorID string) ([]historyAnchor, error) {
	in, args := "?", []any{chats[0]}
	if len(chats) > 1 {
		in, args = "?, ?", []any{chats[0], chats[1]}
	}
	base := `SELECT id, chat_jid, timestamp, is_from_me FROM messages WHERE chat_jid IN (` + in + `)`
	switch {
	case anchorID != "":
		return queryAnchors(store, base+` AND id = ? LIMIT 1`, append(args, anchorID)...)
	case fromNewest:
		return queryAnchors(store, base+` ORDER BY julianday(timestamp) DESC LIMIT 1`, args...)
	}
	oldest, err := queryAnchors(store, base+` ORDER BY julianday(timestamp) ASC LIMIT ?`, append(args, historyMaxAnchors)...)
	if err != nil || len(oldest) == 0 {
		return oldest, err
	}
	fromMe, err := queryAnchors(store, base+` AND is_from_me = 1 ORDER BY julianday(timestamp) ASC LIMIT 1`, args...)
	if err != nil {
		return nil, err
	}
	out := []historyAnchor{oldest[0]}
	seen := map[string]bool{oldest[0].ID: true}
	for _, a := range append(fromMe, oldest[1:]...) {
		if !seen[a.ID] && len(out) < historyMaxAnchors {
			seen[a.ID] = true
			out = append(out, a)
		}
	}
	return out, nil
}

type historyAttempt struct {
	AnchorID       string `json:"anchor_id"`
	AnchorTime     string `json:"anchor_time"`
	AnchorFromMe   bool   `json:"anchor_from_me"`
	SentAsChat     string `json:"sent_as_chat"`
	RequestID      string `json:"request_id"`
	PhoneResponded bool   `json:"phone_responded"`
	ResponseChat   string `json:"response_chat,omitempty"`
	Received       int    `json:"received_count"`
	EndOfHistory   bool   `json:"end_of_history,omitempty"`
	EndType        string `json:"end_of_history_type,omitempty"`
	PeerCode       string `json:"response_code,omitempty"`
	Oldest         string `json:"oldest_received,omitempty"`
}

// phoneSaysNoMore reports whether the phone's answer rules out getting more with another anchor.
func phoneSaysNoMore(endType string) bool {
	return endType == waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.String() ||
		endType == waHistorySync.Conversation_COMPLETE_ON_DEMAND_SYNC_WITH_MORE_MSG_ON_PRIMARY_BUT_NO_ACCESS.String()
}

func sendHistoryRequest(ctx context.Context, client *whatsmeow.Client, chat types.JID, a historyAnchor, count int) (string, error) {
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: a.FromMe, IsGroup: chat.Server == types.GroupServer},
		ID:            types.MessageID(a.ID),
		Timestamp:     a.TS,
	}
	id := client.GenerateMessageID()
	_, err := client.SendMessage(ctx, client.Store.ID.ToNonAD(), client.BuildHistorySyncRequest(info, count),
		whatsmeow.SendRequestExtra{Peer: true, ID: id})
	return string(id), err
}

func handleHistoryRequest(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeExtrasJSON(w, http.StatusMethodNotAllowed, map[string]any{"success": false, "message": "POST only"})
		return
	}
	var req struct {
		ChatJID         string `json:"chat_jid"`
		Count           int    `json:"count"`
		FromNewest      bool   `json:"from_newest"`
		AnchorMessageID string `json:"anchor_message_id"`
		Wait            *bool  `json:"wait"`
		WaitSeconds     int    `json:"wait_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "missing chat_jid or invalid request"})
		return
	}
	if req.Count <= 0 || req.Count > 100 {
		req.Count = 50 // whatsmeow recommends 50 per request
	}
	wait := req.Wait == nil || *req.Wait
	perWait := historyDefaultWait
	if req.WaitSeconds > 0 && req.WaitSeconds <= 40 {
		perWait = time.Duration(req.WaitSeconds) * time.Second
	}

	if !client.IsConnected() || client.Store.ID == nil {
		writeExtrasJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "message": "bridge is not connected to WhatsApp"})
		return
	}
	chat, err := types.ParseJID(req.ChatJID)
	if err != nil {
		writeExtrasJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "invalid chat_jid: " + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), historyTotalBudget+15*time.Second)
	defer cancel()

	// The same contact may be stored under its phone number and its LID
	targets := []types.JID{chat}
	if alt := alternateChatJID(ctx, client, chat); !alt.IsEmpty() && alt != chat {
		targets = append(targets, alt)
	}
	stored := make([]string, len(targets))
	for i, t := range targets {
		stored[i] = t.String()
	}

	anchors, err := anchorCandidates(store, stored, req.FromNewest, req.AnchorMessageID)
	if err != nil {
		writeExtrasJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "database error: " + err.Error()})
		return
	}
	anchorless := false
	if len(anchors) == 0 {
		if req.AnchorMessageID != "" {
			writeExtrasJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "anchor message not found in this chat"})
			return
		}
		// Experimental: no stored messages, so send without an anchor and let the phone decide
		anchorless = true
		anchors = []historyAnchor{{TS: time.Now()}}
	}

	if !wait {
		a := anchors[0]
		reqID, err := sendHistoryRequest(ctx, client, chat, a, req.Count)
		if err != nil {
			logger.Warnf("extras: history request failed (%s): %v", req.ChatJID, err)
			writeExtrasJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "failed to send the request to the phone: " + err.Error()})
			return
		}
		logger.Infof("extras: requested %d messages before %s in %s (anchor=%s anchorless=%v request_id=%s, not waiting)",
			req.Count, a.TS.Format(time.RFC3339), req.ChatJID, valueOr(a.ID, "-"), anchorless, reqID)
		writeExtrasJSON(w, http.StatusOK, map[string]any{
			"success": true, "status": "sent", "anchorless": anchorless, "request_id": reqID, "requested": req.Count,
			"message":     "Request sent; not waiting for the answer. Messages arrive asynchronously if the phone has them.",
			"anchor_used": map[string]any{"id": a.ID, "timestamp": a.TS.Format(time.RFC3339), "from_me": a.FromMe},
		})
		return
	}

	waiter, done := addOnDemandWaiter(stored)
	defer done()

	var attempts []historyAttempt
	deadline := time.Now().Add(historyTotalBudget)
	noResponse := false
	stop := false
	for _, a := range anchors {
		for _, target := range targets {
			if stop || len(attempts) >= historyMaxAttempts || time.Now().After(deadline) {
				stop = true
				break
			}
			// Drop late answers to earlier attempts
			for len(waiter.ch) > 0 {
				<-waiter.ch
			}
			codeCh := make(chan string, 1)
			reqID, err := sendHistoryRequest(ctx, client, target, a, req.Count)
			if err != nil {
				logger.Warnf("extras: history request failed (%s): %v", target, err)
				writeExtrasJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "failed to send the request to the phone: " + err.Error(), "attempts": attempts})
				return
			}
			peerCodeWaiters.Store(reqID, codeCh)
			logger.Infof("extras: requested %d messages before %s in %s (anchor=%s from_me=%v request_id=%s)",
				req.Count, a.TS.Format(time.RFC3339), target, valueOr(a.ID, "-"), a.FromMe, reqID)

			att := historyAttempt{AnchorID: a.ID, AnchorTime: a.TS.Format(time.RFC3339), AnchorFromMe: a.FromMe,
				SentAsChat: target.String(), RequestID: reqID}
			timeout := perWait
			if rem := time.Until(deadline); rem < timeout {
				timeout = rem
			}
			timer := time.NewTimer(timeout)
		waitLoop:
			for {
				select {
				case code := <-codeCh:
					att.PeerCode = code // a rejection may come without a history sync
					if code != "" && code != "REQUEST_SUCCESS" {
						att.PhoneResponded = true
						break waitLoop
					}
				case res := <-waiter.ch:
					att.PhoneResponded = true
					att.ResponseChat, att.Received = res.ChatJID, res.Received
					att.EndOfHistory, att.EndType = res.EndOfHistory, res.EndType
					if !res.Oldest.IsZero() {
						att.Oldest = res.Oldest.Format(time.RFC3339)
					}
					break waitLoop
				case <-timer.C:
					break waitLoop
				case <-ctx.Done():
					break waitLoop
				}
			}
			timer.Stop()
			peerCodeWaiters.Delete(reqID)
			attempts = append(attempts, att)

			switch {
			case !att.PhoneResponded:
				noResponse, stop = true, true // another anchor will not wake up an offline phone
			case att.Received > 0, phoneSaysNoMore(att.EndType), anchorless, att.PeerCode != "" && att.PeerCode != "REQUEST_SUCCESS":
				stop = true
			}
		}
		if stop {
			break
		}
	}

	last := attempts[len(attempts)-1]
	received := 0
	allZero := true
	for _, a := range attempts {
		received += a.Received
		if !a.PhoneResponded || a.Received > 0 {
			allZero = false
		}
	}
	responseCode := last.PeerCode
	if responseCode == "" {
		responseCode = last.EndType
	}

	var status, msg string
	switch {
	case noResponse && len(attempts) == 1:
		status = "no_response"
		msg = fmt.Sprintf("No response from phone within %d s - phone offline or request ignored.", int(perWait.Seconds()))
	case received > 0:
		status = "received"
		msg = fmt.Sprintf("The phone sent %d messages; they are stored now (list_messages).", received)
	case allZero:
		status = "history_exhausted"
		msg = "The phone answered with zero messages for every anchor tried: it has nothing older to give for this chat"
		if last.EndType != "" {
			msg += " (phone reports " + last.EndType + ")"
		}
		msg += "."
	case noResponse:
		status = "no_response"
		msg = "The phone answered earlier attempts with zero messages and then stopped answering."
	default:
		status = "rejected"
		msg = "The phone rejected the request (response_code " + responseCode + ")."
	}
	logger.Infof("extras: history request %s finished: status=%s attempts=%d received=%d response=%s",
		req.ChatJID, status, len(attempts), received, valueOr(responseCode, "-"))

	writeExtrasJSON(w, http.StatusOK, map[string]any{
		"success":           true,
		"status":            status,
		"message":           msg,
		"phone_responded":   last.PhoneResponded,
		"response_code":     responseCode,
		"received_count":    received,
		"history_exhausted": allZero,
		"anchorless":        anchorless,
		"anchor_used": map[string]any{
			"id": last.AnchorID, "timestamp": last.AnchorTime, "from_me": last.AnchorFromMe, "sent_as_chat": last.SentAsChat,
		},
		"attempts":  attempts,
		"requested": req.Count,
	})
}

// fixChatLastMessageTime sets chats.last_message_time to the newest stored message. Upstream
// sets it from each history batch, so a batch of old messages moves it back in time.
// It only ever moves the time forward.
func fixChatLastMessageTime(store *MessageStore, chatJID string) (int64, error) {
	q := `UPDATE chats SET last_message_time = (
	          SELECT m.timestamp FROM messages m WHERE m.chat_jid = chats.jid ORDER BY julianday(m.timestamp) DESC LIMIT 1)
	      WHERE EXISTS (SELECT 1 FROM messages m WHERE m.chat_jid = chats.jid
	                    AND (chats.last_message_time IS NULL OR julianday(m.timestamp) > julianday(chats.last_message_time)))`
	var res sql.Result
	var err error
	if chatJID == "" {
		res, err = store.db.Exec(q)
	} else {
		res, err = store.db.Exec(q+` AND jid = ?`, chatJID)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
