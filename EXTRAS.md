# whatsapp-mcp extras

Additions on top of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp), kept as
separate files so that upstream changes and fixes from other forks merge with few conflicts.

| Area | What changes |
|---|---|
| **Names** | `@lid` pseudonyms resolved to contact names (`lid_names` table); your own chat shows as *Me (note to self)*; sender lookup no longer mis-matches legacy group JIDs |
| **History** | On-demand history from your phone (`/api/history`, tool `request_older_messages`), anchored on the oldest or newest stored message; reports what the phone answered (received / exhausted / no response) and retries other anchors; `scripts/backfill-history.sh` for bulk runs |
| **Chat exports** | WhatsApp gives linked devices only part of a chat's history on demand (the phone answers "more remain on the phone"); that limit is recorded per chat and explained to the user. Older history can be imported from the phone's *Export chat* file via a configured folder (tools `list_chat_exports`, `import_chat_export`) |
| **Media** | Files named by message ID (upstream names collide); fixed downloads (the signed query string is kept — upstream strips it and gets 403), automatic *media retry* via the phone for expired media; images returned inline, document text / local transcription / raw file on request |
| **Reactions** | Stored live and from history syncs, including the newer *message add-on* format; current state (`reactions`) plus an append-only change log (`reaction_events`) |
| **Edits** | Edited messages are versioned (`message_edits`: version 0 = original, 1.. = edits with edit timestamps); reactions on edit wrappers are attached to the original message |
| **Bulk reading** | Tool `export_chat_text` exports a chat as compact chronological text in chunks — for summaries and overall analyses |
| **Remote use** | MCP server can run over streamable HTTP (`MCP_TRANSPORT=http`) with Host-header checks; macOS launchd setup in `scripts/setup-macos-service.sh` |
| **Safety** | Bridge REST API bound to `127.0.0.1`; `host_guard.go` refuses to run the same device keys on a second machine; send tools can stay disabled |
| **Maintenance** | CI, a weekly whatsmeow update PR, a weekly upstream-activity digest, a deploy hook with rollback |

## Architecture

```
Client machines                                     Bridge host (always on)
───────────────                                     ───────────────────────
Claude Desktop ── mcp-remote ──┐                    launchd: whatsapp-bridge  ⇄ WhatsApp (linked device)
                               ├── Tailscale ──▶    launchd: MCP server (HTTP :8001)
Claude Code ───── HTTP ────────┘                    store/  (SQLite messages + device keys — never committed)
```

The MCP server has **no authentication of its own**. Expose it only on a private network
(e.g. a personal Tailscale tailnet; restrict the port with ACLs if others share the tailnet) or
on `127.0.0.1`.

## Files

| File | Purpose |
|---|---|
| `whatsapp-bridge/lid_history.go` | `startExtras()` entry point, LID names, `/api/refresh_names` |
| `whatsapp-bridge/history_wait_extras.go` | `/api/history`: waits for the phone's answer, anchor and LID/PN fallbacks; chat last-message-time repair |
| `whatsapp-bridge/device_props_extras.go` | Linking: asks the phone for the full history, registers as a desktop client, neutral device name (`WHATSAPP_DEVICE_NAME`) |
| `whatsapp-bridge/import_extras.go` | `/api/import` for chat exports; `history_limits` and `message_imports` tables |
| `whatsapp-mcp-server/chat_import.py` | Parser for *Export chat* text files (iPhone/Android, common locales) |
| `whatsapp-bridge/history_ondemand_extras.go` | Logs on-demand answers (end-of-history flags) and hands them to waiting requests |
| `whatsapp-bridge/media_extras.go` | `/api/download2` with media retry |
| `whatsapp-bridge/reactions_extras.go` | Reactions, reaction change log, edit versioning, debug helpers |
| `whatsapp-bridge/host_guard.go` | Device-key owner check (`store/.host`) |
| `whatsapp-mcp-server/whatsapp_extras.py` | MCP tools and fixes listed above |
| `scripts/setup-macos-service.sh` | Build + launchd services (bridge, MCP over HTTP) |
| `scripts/backfill-history.sh` | Bulk history requests |
| `scripts/update-whatsmeow.sh` | Local whatsmeow update with rollback |
| `deploy/post-receive` | `git push` deploy with build check and rollback |

Wiring in upstream files (already applied in this branch):
- `main.go`: `startExtras(client, messageStore, logger)` after `startRESTServer(...)`, REST server on `127.0.0.1`
- `main.py`: `whatsapp_extras.register(mcp)` and an HTTP mode selected by `MCP_TRANSPORT`
- `pyproject.toml`: `mcp[cli]>=1.20,<2` (streamable HTTP) and `pypdf`

## Setup on an always-on Mac

```bash
brew install go uv node            # node provides npx for mcp-remote on clients
bash scripts/setup-macos-service.sh
```
Moving an existing link: copy `whatsapp-bridge/store/` from the old machine first, stop the
bridge there, then run the script (it records the new owner in `store/.host`).

Optional local transcription: `brew install whisper-cpp ffmpeg` and a model at
`~/models/ggml-large-v3-turbo.bin` (or set `WHISPER_MODEL`, `WHISPER_LANG`).

### Clients

Claude Desktop:
```json
"whatsapp": {
  "command": "npx",
  "args": ["-y", "mcp-remote@<version>", "http://<bridge-host>:8001/mcp", "--allow-http"]
}
```
Claude Code:
```bash
claude mcp add --transport http whatsapp http://<bridge-host>:8001/mcp
```
When an agent with shell access can read WhatsApp messages, anyone who can message you can put
text into its context. Keep permission prompts on and the send tools disabled.

## Database additions (`messages.db`)

| Table | Contents |
|---|---|
| `lid_names` | `lid`, `pn` (phone number), `name`, `updated_at` |
| `reactions` | current reaction per (`chat_jid`, `message_id`, `sender`) |
| `reaction_events` | every observed reaction / change / removal (`emoji = ''`), with `source` = live/history |
| `message_edits` | `version` 0 = original, 1.. = edits, with `edited_at` |

Change history for reactions is only complete for changes the bridge sees live; history
syncs contain only the current reaction per person.

## Debugging

```bash
WA_DEBUG_MSG=<message id> WA_DEBUG_TEXT="<substring>" ./whatsapp-bridge
```
logs the raw structure of matching messages (and a ±20 min window around the target) when they
arrive in a history sync. Every history batch is summarised in the log (count, time range).
**Debug output contains message content — keep logs private and delete them afterwards.**

## Staying current with the WhatsApp protocol

Protocol changes land in [whatsmeow](https://github.com/tulir/whatsmeow), not in the bridge.
- **Weekly PR** (`.github/workflows/update-whatsmeow.yml`) bumps whatsmeow; a failing run is an
  early warning that the extras need adapting.
- **Locally**: `bash scripts/update-whatsmeow.sh` (rolls back on build failure). Run it at once if
  the bridge log shows `client outdated (405)`.
- **Upstream digest** (`.github/workflows/upstream-watch.yml`) opens an issue listing new upstream
  commits and PRs, so fixes from others can be cherry-picked:
  ```bash
  git remote add upstream https://github.com/lharries/whatsapp-mcp.git
  git fetch upstream pull/<N>/head:pr-<N> && git cherry-pick <commit>
  ```

## Credits

- [Luke Harries](https://github.com/lharries) and contributors — the original bridge and MCP server (MIT).
- [Diego Penna Moreira](https://github.com/lharries/whatsapp-mcp/pull/307) — analysis of the media 403 bug and the LID contact lookup in upstream PR #307.
- [Tulir Asokan](https://github.com/tulir) and the whatsmeow contributors — the WhatsApp protocol library (MPL-2.0). Consider [sponsoring whatsmeow](https://github.com/sponsors/tulir).

## License

The extras are released under the MIT License (see `LICENSE-EXTRAS`). The upstream code keeps
its own MIT license (`LICENSE`). whatsmeow is used as a library under MPL-2.0.

### Importing chat exports

WhatsApp keeps part of every chat's history on the phone only: `request_older_messages` then
returns `phone_sent_nothing` (and `phone_limit_known` for a week afterwards, `force=True` asks
again) with a `user_message` explaining it. To add that history:

1. Configure an export folder for the MCP server, e.g. a Google Drive folder synced to the
   bridge host: `EXPORT_DIR=... EXPORT_DIR_LABEL="Google Drive › whatsapp-export" MY_NAME="Your Name"`
   when running `scripts/setup-macos-service.sh` (sets `WHATSAPP_EXPORT_DIR`,
   `WHATSAPP_EXPORT_DIR_LABEL`, `WHATSAPP_MY_NAME`). On macOS, cloud folders are privacy-protected:
   give the Python that runs the MCP server Full Disk Access.
2. On the phone: chat → Export chat → Without media → save to that folder (.txt or .zip).
3. Ask Claude to import it: `list_chat_exports`, then `import_chat_export` (dry run first).

Only messages older than the oldest stored WhatsApp message are imported, with IDs
`import-<hash>`, so repeating an import adds nothing. They have no reactions or media.
