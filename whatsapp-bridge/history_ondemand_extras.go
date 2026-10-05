package main

// history_ondemand_extras.go — diagnostics for on-demand history sync requests.
//
// Logs the phone's response to HISTORY_SYNC_ON_DEMAND requests (a peer protocol message
// carrying a response code, e.g. REQUEST_SUCCESS or DECLINED_SHARING_HISTORY) and the
// ON_DEMAND history batches that follow, so a request that gets no messages can be told
// apart: rejected by the phone, never answered, or answered but not stored.

import (
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func startOnDemandHistoryLogging(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.Message:
			pm := v.Message.GetProtocolMessage()
			if n := pm.GetHistorySyncNotification(); n != nil {
				logHistorySyncNotification(n, logger)
			}
			if pm.GetType() != waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_RESPONSE_MESSAGE {
				return
			}
			resp := pm.GetPeerDataOperationRequestResponseMessage()
			var codes []string
			for _, res := range resp.GetPeerDataOperationResult() {
				if r := res.GetFullHistorySyncOnDemandRequestResponse(); r != nil {
					codes = append(codes, r.GetResponseCode().String())
				}
			}
			if len(codes) > 0 {
				deliverPeerCode(resp.GetStanzaID(), codes[0])
			}
			logger.Infof("extras: peer data response type=%s request_id=%s results=%d history_codes=[%s]",
				resp.GetPeerDataOperationRequestType(), resp.GetStanzaID(),
				len(resp.GetPeerDataOperationResult()), strings.Join(codes, ","))

		case *events.HistorySync:
			if v.Data == nil {
				return
			}
			convs := v.Data.GetConversations()
			logger.Infof("extras: history sync type=%s conversations=%d", v.Data.GetSyncType(), len(convs))
			// Upstream has already stored the batch (its handler is registered first)
			for _, conv := range convs {
				if _, err := fixChatLastMessageTime(store, conv.GetID()); err != nil {
					logger.Warnf("extras: failed to fix last message time of %s: %v", conv.GetID(), err)
				}
				// The bridge now holds these messages itself: drop imported copies of them
				var ids []string
				for _, m := range conv.GetMessages() {
					if id := m.GetMessage().GetKey().GetID(); id != "" {
						ids = append(ids, id)
					}
				}
				unmarkImported(store, conv.GetID(), ids)
				if _, err := restoreTextFromMetadata(store, conv.GetID()); err != nil {
					logger.Warnf("extras: restoring imported text failed for %s: %v", conv.GetID(), err)
				}
				if n, err := removeImportDuplicates(store, conv.GetID()); err != nil {
					logger.Warnf("extras: duplicate cleanup failed for %s: %v", conv.GetID(), err)
				} else if n > 0 {
					logger.Infof("extras: removed %d imported duplicates in %s", n, conv.GetID())
				}
			}
			if v.Data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND {
				handleOnDemandSync(v, logger)
			}
		}
	})
}

// logHistorySyncNotification logs the technical fields of a history sync notification (no message
// content), before whatsmeow downloads it. A notification without a direct path cannot be
// downloaded by whatsmeow ("no url present"); this shows what the phone sent instead.
func logHistorySyncNotification(n *waE2E.HistorySyncNotification, logger waLog.Logger) {
	var oldest string
	if ts := n.GetOldestMsgInChunkTimestampSec(); ts > 0 {
		oldest = time.Unix(ts, 0).Format(time.RFC3339)
	}
	logger.Infof("extras: history sync notification type=%s chunk=%d progress=%d direct_path=%v enc_handle=%d bytes "+
		"inline_payload=%d bytes file_length=%d oldest_in_chunk=%s original_msg=%s session=%s access_complete=%v on_demand_request=%s",
		n.GetSyncType(), n.GetChunkOrder(), n.GetProgress(), n.GetDirectPath() != "", len(n.GetEncHandle()),
		len(n.GetInitialHistBootstrapInlinePayload()), n.GetFileLength(), valueOr(oldest, "-"),
		valueOr(n.GetOriginalMessageID(), "-"), valueOr(n.GetPeerDataRequestSessionID(), "-"),
		n.GetMessageAccessStatus().GetCompleteAccessGranted(), valueOr(n.GetFullHistorySyncOnDemandRequestMetadata().GetRequestID(), "-"))
}
