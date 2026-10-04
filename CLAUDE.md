# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is
A fork of lharries/whatsapp-mcp with extras (see EXTRAS.md):
- `whatsapp-bridge/` — Go program built on whatsmeow (the WhatsApp protocol library). Keeps a
  SQLite store and exposes a REST API on 127.0.0.1:8080.
- `whatsapp-mcp-server/` — Python MCP server that reads the store and calls the bridge.
- Extras live in separate files (`*_extras.go`, `lid_history.go`, `host_guard.go`,
  `whatsapp_extras.py`) so upstream merges stay easy. Keep it that way: prefer new files
  and small hooks over editing upstream files.

## Hard rules
- **Never read, copy, modify or commit `whatsapp-bridge/store/`.** It holds the message
  database and the WhatsApp device keys.
- **Never start the bridge on a machine that is not the bridge host** with an existing store.
  `host_guard.go` enforces this; do not weaken it.
- Never commit logs, `.env` files or anything containing message content, phone numbers,
  host names or personal data. Keep code, comments and docs in English.
- Do not enable or add message-sending tools unless explicitly asked.
- Do not add the WhatsApp MCP server to Claude Code in this repository (message content and
  shell access should never share a session).

## Workflow
- Build before committing: `cd whatsapp-bridge && go build ./...`
- Python check: `cd whatsapp-mcp-server && uv run python -m py_compile main.py whatsapp.py whatsapp_extras.py`
- Shell check: `for f in scripts/*.sh deploy/post-receive; do bash -n "$f"; done`
- Commit messages: imperative, short subject line.
- Upstream: `git fetch upstream`; review PRs listed in the weekly "Upstream activity" issues and
  cherry-pick useful fixes with attribution.
- whatsmeow updates arrive as the `deps/whatsmeow` PR. Check CI, read the whatsmeow changes,
  adapt extras if APIs changed, then merge.
- Deployment target and machine-specific details are in `CLAUDE.local.md` (not committed).
