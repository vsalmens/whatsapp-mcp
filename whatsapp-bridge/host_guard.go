package main

// host_guard.go — refuses to run the bridge on the wrong machine with the same device keys.
//
// store/.host records which machine the WhatsApp device keys in store/ belong to.
//  - No keys yet (store/whatsapp.db missing): a new link → this machine becomes the owner.
//  - Keys exist but .host is missing or names another machine: the bridge refuses to start.
// Running the same linked device on two machines can get the link disconnected.
// To move intentionally: update store/.host with the new machine's name (and make sure
// the old machine no longer runs the bridge).

import (
	"fmt"
	"os"
	"strings"
)

// Short, lower-case host name without the network suffix (".local", ".lan", ...),
// so it stays stable even if the DHCP-provided domain changes.
func normalizedHostname() string {
	h, _ := os.Hostname()
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

func init() {
	host := normalizedHostname()
	const db, marker = "store/whatsapp.db", "store/.host"

	_, dbErr := os.Stat(db)
	owner, markerErr := os.ReadFile(marker)

	switch {
	case dbErr != nil:
		// No device keys yet → new link for this machine
		_ = os.MkdirAll("store", 0o700)
		_ = os.WriteFile(marker, []byte(host+"\n"), 0o600)
	case markerErr != nil:
		fmt.Fprintf(os.Stderr, "✗ store/.host is missing, so the owner of these device keys is unknown.\n"+
			"  If this machine (%q) is the right bridge host, run:  echo %s > store/.host\n", host, host)
		os.Exit(1)
	case strings.TrimSpace(string(owner)) != host:
		fmt.Fprintf(os.Stderr, "✗ The device keys in store/ belong to %q, but this machine is %q.\n"+
			"  Running the same WhatsApp device on two machines can break the link. Not starting.\n",
			strings.TrimSpace(string(owner)), host)
		os.Exit(1)
	}
}
