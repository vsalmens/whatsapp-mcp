#!/usr/bin/env bash
# setup-macos-service.sh — run the WhatsApp bridge and MCP server as macOS launchd services.
#
# Run on the always-on machine, from anywhere inside the repo, after the repo (including
# whatsapp-bridge/store/ if you are moving an existing link) is in place and no other
# bridge is running.
#
#   bash scripts/setup-macos-service.sh
#
# Environment (all optional):
#   LABEL_PREFIX  launchd label prefix              (default: local.whatsapp-mcp)
#   BRIDGE_LABEL  full bridge label                 (default: <prefix>.bridge)
#   MCP_LABEL     full MCP server label             (default: <prefix>.mcp)
#   MCP_PORT      MCP HTTP port                     (default: 8001)
#   BIND_IP       IP the MCP server listens on      (default: this machine's Tailscale IPv4)
#   BIND_NAME     extra allowed Host name           (default: this machine's Tailscale DNS name)
#
# What it does:
#   1) makes sure the bridge REST API listens on 127.0.0.1 only
#   2) records this machine as the owner of the device keys (store/.host, see host_guard.go)
#   3) builds the bridge and syncs the Python environment
#   4) installs two LaunchAgents: $BRIDGE_LABEL and $MCP_LABEL
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BRIDGE="$REPO/whatsapp-bridge"
SERVER="$REPO/whatsapp-mcp-server"
LABEL_PREFIX="${LABEL_PREFIX:-local.whatsapp-mcp}"
BRIDGE_LABEL="${BRIDGE_LABEL:-$LABEL_PREFIX.bridge}"
MCP_LABEL="${MCP_LABEL:-$LABEL_PREFIX.mcp}"
MCP_PORT="${MCP_PORT:-8001}"
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
UV="$(command -v uv)"     || { echo "✗ uv not found (brew install uv)"; exit 1; }
GO="$(command -v go)"     || { echo "✗ go not found (brew install go)"; exit 1; }
LA="$HOME/Library/LaunchAgents"
LOGS="$HOME/Library/Logs"

die(){ echo "✗ $*"; exit 1; }
[[ -f "$BRIDGE/main.go" && -f "$SERVER/main.py" ]] || die "repo layout not found under $REPO"
pgrep -x whatsapp-bridge >/dev/null && die "a whatsapp-bridge process is already running on this machine — stop it first"

# --- Network address for the MCP server ---
TSCLI="$(command -v tailscale || echo /Applications/Tailscale.app/Contents/MacOS/Tailscale)"
BIND_IP="${BIND_IP:-$("$TSCLI" ip -4 2>/dev/null | head -1 || true)}"
BIND_NAME="${BIND_NAME:-$("$TSCLI" status --json 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))' 2>/dev/null || true)}"
[[ -n "$BIND_IP" ]] || die "no Tailscale IPv4 found; install/log in to Tailscale or set BIND_IP (127.0.0.1 for local-only)"
echo "• MCP will listen on $BIND_IP:$MCP_PORT ${BIND_NAME:+($BIND_NAME)}"

# --- 1) Bridge REST API on localhost only (upstream listens on all interfaces) ---
if grep -q 'fmt.Sprintf(":%d", port)' "$BRIDGE/main.go"; then
  sed -i '' 's|fmt.Sprintf(":%d", port)|fmt.Sprintf("127.0.0.1:%d", port)|' "$BRIDGE/main.go"
  echo "✓ bridge REST API bound to 127.0.0.1"
fi

# --- Sanity checks for the committed MCP changes ---
grep -q 'MCP_TRANSPORT' "$SERVER/main.py" || die "main.py has no HTTP mode (MCP_TRANSPORT); use the extras version of main.py"
grep -q 'whatsapp_extras' "$SERVER/main.py" || die "main.py does not register whatsapp_extras"

# --- 2) Owner of the device keys ---
mkdir -p "$BRIDGE/store" && chmod 700 "$BRIDGE/store"
hostname -s | tr '[:upper:]' '[:lower:]' > "$BRIDGE/store/.host"
chmod 600 "$BRIDGE/store/.host"
echo "✓ store/.host = $(cat "$BRIDGE/store/.host")"

# --- 3) Build ---
( cd "$BRIDGE" && "$GO" build -o whatsapp-bridge . ) && echo "✓ bridge built"
( cd "$SERVER" && "$UV" sync -q ) && echo "✓ Python environment synced"

# --- 4) LaunchAgents ---
mkdir -p "$LA" "$LOGS"
ALLOWED="$BIND_IP:*${BIND_NAME:+,$BIND_NAME:*}"

cat > "$LA/$BRIDGE_LABEL.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$BRIDGE_LABEL</string>
  <key>WorkingDirectory</key><string>$BRIDGE</string>
  <key>ProgramArguments</key><array><string>$BRIDGE/whatsapp-bridge</string></array>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key><string>$LOGS/whatsapp-bridge.log</string>
  <key>StandardErrorPath</key><string>$LOGS/whatsapp-bridge.log</string>
</dict></plist>
PLIST

cat > "$LA/$MCP_LABEL.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$MCP_LABEL</string>
  <key>ProgramArguments</key><array>
    <string>$UV</string><string>--directory</string><string>$SERVER</string><string>run</string><string>main.py</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>MCP_TRANSPORT</key><string>http</string>
    <key>MCP_HOST</key><string>$BIND_IP</string>
    <key>MCP_PORT</key><string>$MCP_PORT</string>
    <key>MCP_ALLOWED_HOSTS</key><string>$ALLOWED</string>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key><string>$LOGS/whatsapp-mcp.log</string>
  <key>StandardErrorPath</key><string>$LOGS/whatsapp-mcp.log</string>
</dict></plist>
PLIST

for label in "$BRIDGE_LABEL" "$MCP_LABEL"; do
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$LA/$label.plist"
done
echo "✓ LaunchAgents loaded ($BRIDGE_LABEL, $MCP_LABEL)"

cat <<TXT

Check:
  tail -f $LOGS/whatsapp-bridge.log   # should connect without a QR code if store/ was copied
  tail -f $LOGS/whatsapp-mcp.log      # "Uvicorn running on http://$BIND_IP:$MCP_PORT"

Claude Desktop config (any machine on the tailnet):
  "whatsapp": {
    "command": "npx",
    "args": ["-y", "mcp-remote@<version>", "http://${BIND_NAME:-$BIND_IP}:$MCP_PORT/mcp", "--allow-http"]
  }
Claude Code:
  claude mcp add --transport http whatsapp http://${BIND_NAME:-$BIND_IP}:$MCP_PORT/mcp
TXT
