package main

// history_ondemand_extras.go — diagnostics for on-demand history sync requests.
//
// Logs the phone's response to HISTORY_SYNC_ON_DEMAND requests (a peer protocol message
// carrying a response code, e.g. REQUEST_SUCCESS or DECLINED_SHARING_HISTORY) and the
// ON_DEMAND history batches that follow, so a request that gets no messages can be told
// apart: rejected by the phone, never answered, or answered but not stored.

import (
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func startOnDemandHistoryLogging(client *whatsmeow.Client, logger waLog.Logger) {
	client.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.Message:
			pm := v.Message.GetProtocolMessage()
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
			logger.Infof("extras: peer data response type=%s request_id=%s results=%d history_codes=[%s]",
				resp.GetPeerDataOperationRequestType(), resp.GetStanzaID(),
				len(resp.GetPeerDataOperationResult()), strings.Join(codes, ","))

		case *events.HistorySync:
			if v.Data == nil {
				return
			}
			convs := v.Data.GetConversations()
			logger.Infof("extras: history sync type=%s conversations=%d", v.Data.GetSyncType(), len(convs))
			if v.Data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
				return
			}
			for _, conv := range convs {
				logger.Infof("extras: on-demand history %s: %d messages", conv.GetID(), len(conv.GetMessages()))
			}
		}
	})
}
