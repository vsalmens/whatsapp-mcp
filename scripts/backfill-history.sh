#!/usr/bin/env bash
# backfill-history.sh — fetch older history for one or more chats from your phone.
#
#   bash scripts/backfill-history.sh [-t YYYY-MM-DD] CHAT_JID [CHAT_JID ...]
#   bash scripts/backfill-history.sh -f chats.txt          # one JID per line
#
# Requests 50 messages at a time until the target date is reached or the phone stops
# returning older messages. Keep WhatsApp open in the foreground on the phone.
# Environment: API (default http://127.0.0.1:8080/api/history), PAUSE (default 20 s).
set -uo pipefail
API="${API:-http://127.0.0.1:8080/api/history}"
PAUSE="${PAUSE:-20}"
TARGET="1970-01-01"
CHATS=()
while getopts "t:f:" opt; do
  case $opt in
    t) TARGET="$OPTARG" ;;
    f) while IFS= read -r l; do [[ -n "$l" && "$l" != \#* ]] && CHATS+=("$l"); done < "$OPTARG" ;;
    *) exit 2 ;;
  esac
done
shift $((OPTIND - 1)); CHATS+=("$@")
(( ${#CHATS[@]} )) || { echo "usage: $0 [-t YYYY-MM-DD] [-f file] CHAT_JID..."; exit 2; }

for chat in "${CHATS[@]}"; do
  echo "== $chat"
  prev=""; stalled=0
  while true; do
    resp=$(curl -s -X POST "$API" -H 'Content-Type: application/json' -d "{\"chat_jid\":\"$chat\",\"count\":50}")
    oldest=$(printf '%s' "$resp" | python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("oldest_known",""))
except Exception: print("")')
    [[ -n "$oldest" ]] || { echo "  error: $resp"; break; }
    echo "  $(date +%H:%M:%S)  oldest known: $oldest"
    [[ "$oldest" < "$TARGET" ]] && { echo "  -> reached $TARGET"; break; }
    if [[ "$oldest" == "$prev" ]]; then
      (( ++stalled >= 3 )) && { echo "  -> no more history (or the phone is not answering)"; break; }
    else stalled=0; fi
    prev="$oldest"; sleep "$PAUSE"
  done
done
