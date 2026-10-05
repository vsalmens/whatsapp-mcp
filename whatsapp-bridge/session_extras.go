package main

// session_extras.go — restart into QR mode after the phone logs this device out.
//
// Upstream only logs the LoggedOut event and keeps the process running without a session, so
// no QR code appears until someone restarts the bridge. whatsmeow has already deleted the
// session at that point; exiting lets launchd (KeepAlive) start a fresh process that shows a QR code.

import (
	"os"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func startSessionExtras(client *whatsmeow.Client, logger waLog.Logger) {
	client.AddEventHandler(func(evt any) {
		if lo, ok := evt.(*events.LoggedOut); ok {
			logger.Warnf("extras: logged out by the phone (reason %s); exiting so the service restarts and shows a QR code", lo.Reason)
			go func() {
				time.Sleep(time.Second) // let the log line and pending writes finish
				os.Exit(1)
			}()
		}
	})
}
