#!/usr/bin/env bash
# update-whatsmeow.sh — update whatsmeow to the latest commit, build, and roll back on failure.
#
#   bash scripts/update-whatsmeow.sh            # update + build
#   RESTART=1 bash scripts/update-whatsmeow.sh  # also restart the launchd bridge service
#
# Run it when the bridge log shows "client outdated (405)", or regularly (e.g. monthly).
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BRIDGE="$REPO/whatsapp-bridge"
LABEL_PREFIX="${LABEL_PREFIX:-local.whatsapp-mcp}"
BRIDGE_LABEL="${BRIDGE_LABEL:-$LABEL_PREFIX.bridge}"
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
cd "$BRIDGE"
cp go.mod go.mod.prev && cp go.sum go.sum.prev
before=$(grep 'go.mau.fi/whatsmeow ' go.mod | awk '{print $2}')
if go get go.mau.fi/whatsmeow@main && go mod tidy && go build -o whatsapp-bridge.new .; then
  after=$(grep 'go.mau.fi/whatsmeow ' go.mod | awk '{print $2}')
  mv whatsapp-bridge.new whatsapp-bridge && rm -f go.mod.prev go.sum.prev
  echo "✓ whatsmeow $before → $after"
  if [[ "${RESTART:-0}" == 1 ]]; then
    launchctl kickstart -k "gui/$(id -u)/$BRIDGE_LABEL" && echo "✓ bridge restarted"
  fi
else
  mv go.mod.prev go.mod && mv go.sum.prev go.sum && rm -f whatsapp-bridge.new
  echo "✗ update or build failed — go.mod/go.sum restored, running binary untouched"
  exit 1
fi
