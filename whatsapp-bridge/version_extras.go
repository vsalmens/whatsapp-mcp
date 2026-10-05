package main

// version_extras.go — which build is running. Set at build time by deploy/post-receive and
// scripts/setup-macos-service.sh:
//   go build -ldflags "-X main.buildCommit=<commit> -X main.buildTime=<time>"
// Logged at start-up and served by GET /api/version.

import (
	"net/http"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

var (
	buildCommit = "dev"
	buildTime   = "unknown"
	startedAt   = time.Now()
)

func startVersionExtras(logger waLog.Logger) {
	logger.Infof("extras: bridge build %s (built %s)", buildCommit, buildTime)
	http.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeExtrasJSON(w, http.StatusOK, map[string]any{
			"commit": buildCommit, "built": buildTime, "started": startedAt.Format(time.RFC3339),
		})
	})
}
