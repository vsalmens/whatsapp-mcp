package main

// version_extras.go — which build is running. Set at build time by deploy/post-receive and
// scripts/setup-macos-service.sh:
//   go build -ldflags "-X main.buildVersion=<major.minor.N> -X main.buildCommit=<commit> -X main.buildTime=<time>"
// major.minor comes from the VERSION file, N is the number of commits (grows with every deploy).
// Printed as the first log line when the process starts (also while waiting for a QR scan),
// logged again when the extras start, and served by GET /api/version.

import (
	"fmt"
	"net/http"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

var (
	buildVersion = "dev"
	buildCommit  = "dev"
	buildTime    = "unknown"
	startedAt    = time.Now()
)

func init() {
	fmt.Printf("%s whatsapp-bridge %s (%s, built %s)\n", startedAt.Format("2006-01-02 15:04:05"), buildVersion, buildCommit, buildTime)
}

func startVersionExtras(logger waLog.Logger) {
	logger.Infof("extras: bridge %s (%s, built %s)", buildVersion, buildCommit, buildTime)
	http.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeExtrasJSON(w, http.StatusOK, map[string]any{
			"version": buildVersion, "commit": buildCommit, "built": buildTime, "started": startedAt.Format(time.RFC3339),
		})
	})
}
