package main

// version_extras.go — which build is running. Set at build time by deploy/post-receive and
// scripts/setup-macos-service.sh:
//   go build -ldflags "-X main.buildCommit=<commit> -X main.buildTime=<time>"
// Printed as the first log line when the process starts (also while waiting for a QR scan),
// logged again when the extras start, and served by GET /api/version.

import (
	"fmt"
	"net/http"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

var (
	buildCommit = "dev"
	buildTime   = "unknown"
	startedAt   = time.Now()
)

func init() {
	fmt.Printf("%s whatsapp-bridge build %s (built %s)\n", startedAt.Format("2006-01-02 15:04:05"), buildCommit, buildTime)
}

func startVersionExtras(logger waLog.Logger) {
	logger.Infof("extras: bridge build %s (built %s)", buildCommit, buildTime)
	http.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeExtrasJSON(w, http.StatusOK, map[string]any{
			"commit": buildCommit, "built": buildTime, "started": startedAt.Format(time.RFC3339),
		})
	})
}
