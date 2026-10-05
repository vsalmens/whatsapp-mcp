"""whatsapp_extras.py — extras for the lharries/whatsapp-mcp MCP server.

- Replaces whatsapp.get_sender_name with a version that resolves WhatsApp LID
  pseudonyms via the bridge's lid_names table and never matches group JIDs.
- Adds MCP tools:
    request_older_messages(chat_jid, count=50, from_newest=False)
    get_reactions(chat_jid, query=None, message_id=None, limit=5)
    export_chat_text(chat_jid, after=None, before=None, max_chars=60000, offset=0)
    refresh_contact_names()
- Replaces list_messages with a version that merges a 1:1 chat stored under both the
  contact's phone number and its LID (same contact, two JIDs after WhatsApp's LID migration).
- Replaces download_media with a fixed version (bridge /api/download2) that returns
  images inline and, on request, document text, local transcriptions or raw files.
- Disables structured-output validation (mcp >= 1.10) for upstream tools whose
  return annotations do not match what they return.

Wiring in main.py, before `if __name__ == "__main__":`:
    import whatsapp_extras
    whatsapp_extras.register(mcp)
"""

import os
import re
import shutil
import sqlite3
import subprocess
import tempfile
from datetime import datetime
from typing import Any, Dict, Optional

import requests

import whatsapp as _wa

# Must match SelfChatName in whatsapp-bridge/lid_history.go
SELF_CHAT_NAME = "Me (note to self)"
SELF_NAME = "Me"

_original_get_sender_name = _wa.get_sender_name


def get_sender_name(sender_jid: str) -> str:
    """Display name for a sender. Avoids upstream's LIKE lookup, which also matched legacy
    group JIDs (<creator number>-<timestamp>@g.us) and could show e.g. your own reaction
    under a group's name."""
    if not sender_jid:
        return sender_jid
    user = sender_jid.split("@")[0]
    try:
        conn = sqlite3.connect(_wa.MESSAGES_DB_PATH)
        try:
            row = conn.execute(
                "SELECT name FROM lid_names WHERE lid = ? AND name IS NOT NULL AND name != ''", (user,)
            ).fetchone()
            if row:
                return row[0]
            own = conn.execute(
                "SELECT pn FROM lid_names WHERE name = ? AND pn IS NOT NULL AND pn != ''", (SELF_CHAT_NAME,)
            ).fetchone()
            if own and user == own[0]:
                return SELF_NAME
            row = conn.execute(
                "SELECT name FROM lid_names WHERE pn = ? AND name IS NOT NULL AND name != ''", (user,)
            ).fetchone()
            if row:
                return row[0]
            row = conn.execute(
                "SELECT name FROM chats WHERE jid IN (?, ?) AND name IS NOT NULL AND name != ''",
                (f"{user}@s.whatsapp.net", f"{user}@lid"),
            ).fetchone()
            if row and row[0] != user:
                return row[0]
        finally:
            conn.close()
    except sqlite3.Error:
        return _original_get_sender_name(sender_jid)
    return sender_jid


# format_message calls the module-level get_sender_name, so patching it here is enough
_wa.get_sender_name = get_sender_name


def list_chats(query: Optional[str] = None, limit: int = 20, page: int = 0,
               include_last_message: bool = True, sort_by: str = "last_active"):
    """Fixed version of whatsapp.list_chats.

    Upstream selects messages.* even when the JOIN is left out (include_last_message=False),
    which fails with "no such column" and silently returns [], and joins on timestamp
    equality, which lists a chat twice when two messages share its last timestamp.
    Here the page of chats is selected first and at most one last message is joined per chat.
    """
    where, params = "", []
    if query:
        where = "WHERE (LOWER(name) LIKE LOWER(?) OR jid LIKE ?)"
        params += [f"%{query}%", f"%{query}%"]
    order = "last_message_time DESC, name" if sort_by == "last_active" else "name"
    params += [limit, page * limit]

    if include_last_message:
        last = """LEFT JOIN messages m ON m.rowid = (
                      SELECT rowid FROM messages WHERE chat_jid = c.jid ORDER BY timestamp DESC LIMIT 1)"""
        cols = "m.content, m.sender, m.is_from_me"
    else:
        last, cols = "", "NULL, NULL, NULL"

    sql = f"""
        SELECT c.jid, c.name, c.last_message_time, {cols}
        FROM (SELECT jid, name, last_message_time FROM chats {where}
              ORDER BY {order} LIMIT ? OFFSET ?) c
        {last}
        ORDER BY {"c.last_message_time DESC, c.name" if sort_by == "last_active" else "c.name"}"""
    conn = sqlite3.connect(_wa.MESSAGES_DB_PATH)
    try:
        rows = conn.execute(sql, params).fetchall()
    finally:
        conn.close()
    return [
        _wa.Chat(
            jid=r[0], name=r[1],
            last_message_time=datetime.fromisoformat(r[2]) if r[2] else None,
            last_message=r[3], last_sender=r[4], last_is_from_me=r[5],
        )
        for r in rows
    ]


_wa.list_chats = list_chats


def chat_jids(chat_jid: str) -> list:
    """The chat JID plus the same contact's alternate JID (LID <-> phone number), if known."""
    jids = [chat_jid]
    user, _, server = chat_jid.partition("@")
    try:
        conn = sqlite3.connect(f"file:{_wa.MESSAGES_DB_PATH}?mode=ro", uri=True)
        try:
            if server == "s.whatsapp.net":
                row = conn.execute("SELECT lid FROM lid_names WHERE pn = ? AND lid != '' LIMIT 1", (user,)).fetchone()
                if row:
                    jids.append(f"{row[0]}@lid")
            elif server == "lid":
                row = conn.execute("SELECT pn FROM lid_names WHERE lid = ? AND pn != ''", (user,)).fetchone()
                if row:
                    jids.append(f"{row[0]}@s.whatsapp.net")
        finally:
            conn.close()
    except sqlite3.Error:
        pass
    return jids


def list_messages(after: Optional[str] = None, before: Optional[str] = None,
                  sender_phone_number: Optional[str] = None, chat_jid: Optional[str] = None,
                  query: Optional[str] = None, limit: int = 20, page: int = 0,
                  include_context: bool = True, context_before: int = 1, context_after: int = 1):
    """whatsapp.list_messages, but chat_jid also matches the contact's alternate (LID/PN) JID."""
    where, params = [], []
    if after:
        where.append("messages.timestamp > ?"); params.append(datetime.fromisoformat(after))
    if before:
        where.append("messages.timestamp < ?"); params.append(datetime.fromisoformat(before))
    if sender_phone_number:
        where.append("messages.sender = ?"); params.append(sender_phone_number)
    if chat_jid:
        jids = chat_jids(chat_jid)
        where.append(f"messages.chat_jid IN ({','.join('?' * len(jids))})"); params += jids
    if query:
        where.append("LOWER(messages.content) LIKE LOWER(?)"); params.append(f"%{query}%")
    sql = ("SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, "
           "chats.jid, messages.id, messages.media_type FROM messages JOIN chats ON messages.chat_jid = chats.jid"
           + (" WHERE " + " AND ".join(where) if where else "")
           + " ORDER BY messages.timestamp DESC LIMIT ? OFFSET ?")
    params += [limit, page * limit]
    conn = sqlite3.connect(_wa.MESSAGES_DB_PATH)
    try:
        rows = conn.execute(sql, params).fetchall()
    finally:
        conn.close()
    result = [_wa.Message(timestamp=datetime.fromisoformat(r[0]), sender=r[1], chat_name=r[2], content=r[3],
                          is_from_me=r[4], chat_jid=r[5], id=r[6], media_type=r[7]) for r in rows]
    if include_context and result:
        out = []
        for msg in result:
            ctx = _wa.get_message_context(msg.id, context_before, context_after)
            out += ctx.before + [ctx.message] + ctx.after
        return _wa.format_messages_list(out, show_chat_info=True)
    return _wa.format_messages_list(result, show_chat_info=True)


_wa.list_messages = list_messages


def _post(path: str, payload: Optional[Dict[str, Any]] = None, timeout: int = 40) -> Dict[str, Any]:
    try:
        resp = requests.post(f"{_wa.WHATSAPP_API_BASE_URL}/{path}", json=payload or {}, timeout=timeout)
        try:
            return resp.json()
        except ValueError:
            return {"success": False, "message": f"HTTP {resp.status_code}: {resp.text[:200]}"}
    except requests.RequestException as e:
        return {"success": False, "message": f"Could not reach the bridge: {e}"}


def _disable_structured_output(mcp) -> None:
    """mcp >= 1.10 validates tool return values against their annotations ("structured output").
    Upstream tools are annotated List[Dict] but return dataclasses, which fails validation.
    Restore the old (1.6) behaviour: serialise the return value as-is, without a schema."""
    try:
        for tool in mcp._tool_manager._tools.values():
            tool.fn_metadata.output_schema = None
            tool.fn_metadata.output_model = None
    except Exception:
        pass  # older versions do not have this feature


def register(mcp) -> None:
    try:
        _register_tools(mcp)
    finally:
        _disable_structured_output(mcp)


def _register_tools(mcp) -> None:
    _register_download(mcp)
    _register_list_chats(mcp)
    _register_list_messages(mcp)

    @mcp.tool()
    def request_older_messages(chat_jid: str, count: int = 50, from_newest: bool = False,
                               wait: bool = True) -> Dict[str, Any]:
        """Request older history for one chat from the user's own phone.

        Fetches `count` (max 100; the phone sends at most 50 per answer) messages older
        than the oldest message stored for this chat, waits up to ~15 s for the phone's
        answer and reports what happened:
          status "received"           – messages arrived and are stored (call list_messages)
          status "history_exhausted"  – the phone answered with zero messages for every
                                        anchor tried: it has nothing older for this chat
          status "no_response"        – the phone did not answer (offline or ignored)
          status "rejected"           – the phone answered with an error response_code
        Also returned: phone_responded, response_code, received_count, history_exhausted,
        anchor_used and the individual attempts. If the phone answers with nothing, other
        anchors (and, for 1:1 chats, the contact's LID/phone-number JID) are tried
        automatically. Repeat the call to go further back.
        The request is sent only to the user's own devices, never to other people.

        If the chat has no stored messages at all, the request is sent without an anchor
        (experimental; "anchorless": true).

        Args:
            chat_jid: JID of the chat (e.g. 123456789012345678@g.us or 15551234567@s.whatsapp.net)
            count: number of messages to request (default 50)
            from_newest: if True, re-fetch the most recent `count` messages instead of
                older ones (useful for picking up reactions on recent messages)
            wait: if False, only send the request (status "sent") and return immediately;
                messages arrive asynchronously
        """
        return _post("history", {"chat_jid": chat_jid, "count": count, "from_newest": from_newest,
                                 "wait": wait}, timeout=90)

    @mcp.tool()
    def get_reactions(chat_jid: str, query: Optional[str] = None, message_id: Optional[str] = None,
                      limit: int = 5) -> Dict[str, Any]:
        """Show emoji reactions (with times) and edit history of WhatsApp messages (e.g. votes on a decision proposal).

        Find the target message either by message_id, or by a text `query` matched
        against message content (newest matches first). Without either, returns the
        most recent messages in the chat that have reactions.

        Args:
            chat_jid: JID of the chat
            query: text to search for in the message (optional)
            message_id: exact message ID (optional)
            limit: max number of messages to return (default 5)
        """
        try:
            conn = sqlite3.connect(_wa.MESSAGES_DB_PATH)
            cur = conn.cursor()
            if message_id:
                cur.execute("SELECT id, sender, content, timestamp FROM messages WHERE chat_jid=? AND id=?",
                            (chat_jid, message_id))
            elif query:
                cur.execute("""SELECT id, sender, content, timestamp FROM messages
                               WHERE chat_jid=? AND content LIKE ? ORDER BY timestamp DESC LIMIT ?""",
                            (chat_jid, f"%{query}%", limit))
            else:
                cur.execute("""SELECT m.id, m.sender, m.content, m.timestamp FROM messages m
                               WHERE m.chat_jid=? AND EXISTS (SELECT 1 FROM reactions r
                                 WHERE r.chat_jid=m.chat_jid AND r.message_id=m.id)
                               ORDER BY m.timestamp DESC LIMIT ?""", (chat_jid, limit))
            msgs = cur.fetchall()
            out = []
            for mid, sender, content, ts in msgs:
                cur.execute("SELECT emoji, sender, timestamp FROM reactions WHERE chat_jid=? AND message_id=? ORDER BY timestamp",
                            (chat_jid, mid))
                by_emoji: Dict[str, list] = {}
                for emoji, rsender, rts in cur.fetchall():
                    by_emoji.setdefault(emoji, []).append({"name": _wa.get_sender_name(rsender), "time": rts})
                versions = []
                try:
                    cur.execute("SELECT version, content, edited_at FROM message_edits WHERE chat_jid=? AND message_id=? ORDER BY version",
                                (chat_jid, mid))
                    versions = [{"version": v, "time": t, "text": (c or "")[:300]} for v, c, t in cur.fetchall()]
                except sqlite3.Error:
                    pass
                item = {
                    "message_id": mid,
                    "time": ts,
                    "from": _wa.get_sender_name(sender or ""),
                    "preview": (content or "")[:160],
                    "reactions": {e: {"count": len(n), "by": n} for e, n in by_emoji.items()},
                    "total": sum(len(n) for n in by_emoji.values()),
                }
                try:
                    cur.execute("""SELECT sender, emoji, timestamp FROM reaction_events
                                   WHERE chat_jid=? AND message_id=? ORDER BY timestamp""", (chat_jid, mid))
                    per_sender: Dict[str, list] = {}
                    for rsender, emoji, rts in cur.fetchall():
                        per_sender.setdefault(rsender, []).append({"emoji": emoji or "(poistettu)", "time": rts})
                    changes = {_wa.get_sender_name(snd): ev for snd, ev in per_sender.items() if len(ev) > 1}
                    if changes:
                        item["reaction_changes"] = changes  # only reactors whose reaction changed
                except sqlite3.Error:
                    pass
                if versions:
                    item["edited"] = True
                    item["versions"] = versions  # 0 = original, 1.. = edits
                out.append(item)
            conn.close()
            return {"success": True, "messages": out,
                    "note": "Reactions are recorded from the moment the bridge extras were installed; re-fetch history (request_older_messages from_newest=True) to pick up older ones."}
        except sqlite3.Error as e:
            return {"success": False, "message": f"Database error (is the bridge up to date?): {e}"}

    @mcp.tool()
    def export_chat_text(chat_jid: str, after: Optional[str] = None, before: Optional[str] = None,
                         max_chars: int = 60000, offset: int = 0, include_reactions: bool = True) -> Dict[str, Any]:
        """Export a chat as compact chronological text for reading/analysis in bulk.

        Format: "YYYY-MM-DD HH:MM Name: text ⟨👍2 ❤️1⟩". Use for overall/semantic analyses
        (general mood, themes, recommendations): read the whole period in chunks using
        `offset`/`next_offset`, summarise each chunk, then synthesise.

        Args:
            chat_jid: JID of the chat
            after: ISO date/time lower bound, e.g. "2026-02-01" (optional)
            before: ISO date/time upper bound (optional)
            max_chars: approximate size of one chunk (default 60000 chars)
            offset: message offset for pagination (use next_offset from previous call)
            include_reactions: append reaction summary to each message
        """
        try:
            conn = sqlite3.connect(f"file:{_wa.MESSAGES_DB_PATH}?mode=ro", uri=True)
            jids = chat_jids(chat_jid)
            in_jids = f"({','.join('?' * len(jids))})"
            q = f"SELECT id, timestamp, sender, content, media_type, is_from_me FROM messages WHERE chat_jid IN {in_jids}"
            args: list = list(jids)
            if after:
                q += " AND timestamp >= ?"; args.append(after)
            if before:
                q += " AND timestamp < ?"; args.append(before)
            total = conn.execute(q.replace("SELECT id, timestamp, sender, content, media_type, is_from_me", "SELECT COUNT(*)"), args).fetchone()[0]
            rows = conn.execute(q + " ORDER BY timestamp ASC LIMIT -1 OFFSET ?", args + [offset]).fetchall()
            reacts: Dict[str, str] = {}
            if include_reactions:
                try:
                    for mid, emoji, cnt in conn.execute(
                            f"SELECT message_id, emoji, COUNT(*) FROM reactions WHERE chat_jid IN {in_jids} GROUP BY message_id, emoji",
                            jids):
                        reacts[mid] = (reacts.get(mid, "") + f" {emoji}{cnt}").strip()
                except sqlite3.Error:
                    pass
            conn.close()
            names: Dict[str, str] = {}
            lines, used, n = [], 0, 0
            for mid, ts, sender, content, mtype, from_me in rows:
                if from_me:
                    who = SELF_NAME
                else:
                    if sender not in names:
                        names[sender] = _wa.get_sender_name(sender or "")
                    who = names[sender]
                body = (content or "").replace("\n", " / ")
                if mtype:
                    body = f"[{mtype}] {body}".strip()
                if mid in reacts:
                    body += f" ⟨{reacts[mid]}⟩"
                line = f"{str(ts)[:16]} {who}: {body}"
                if used + len(line) > max_chars and n > 0:
                    break
                lines.append(line); used += len(line) + 1; n += 1
            done = offset + n >= total
            return {"success": True, "chat_jid": chat_jid, "messages_in_range": total,
                    "returned": n, "offset": offset,
                    "next_offset": None if done else offset + n,
                    "text": "\n".join(lines)}
        except sqlite3.Error as e:
            return {"success": False, "message": f"Tietokantavirhe: {e}"}

    @mcp.tool()
    def refresh_contact_names() -> Dict[str, Any]:
        """Resolve WhatsApp LID pseudonyms (...@lid) to contact names now.

        The bridge also does this automatically every 10 minutes.
        """
        return _post("refresh_names", timeout=150)


MAX_IMAGE_BYTES = 3_500_000
TEXT_EXTS = {".txt", ".csv", ".md", ".json", ".log", ".vcf", ".ics", ".xml", ".html"}


def _shrink_image(path: str) -> str:
    """Downscale a large image (macOS sips) so it fits the client's image size limit."""
    if os.path.getsize(path) <= MAX_IMAGE_BYTES or not shutil.which("sips"):
        return path
    out = os.path.join(tempfile.gettempdir(), "wa_preview_" + os.path.basename(path) + ".jpg")
    try:
        subprocess.run(["sips", "-s", "format", "jpeg", "-Z", "1600", path, "--out", out],
                       check=True, capture_output=True, timeout=30)
        return out
    except Exception:
        return path


def _remove_tool(mcp, name: str) -> None:
    try:
        mcp.remove_tool(name)
    except Exception:
        try:
            mcp._tool_manager._tools.pop(name, None)
        except Exception:
            pass


def _register_list_chats(mcp) -> None:
    # main.py imported upstream list_chats by name, so replace the tool, not just the function
    _remove_tool(mcp, "list_chats")

    @mcp.tool()
    def list_chats(query: Optional[str] = None, limit: int = 20, page: int = 0,
                   include_last_message: bool = True, sort_by: str = "last_active"):
        """Get WhatsApp chats matching specified criteria.

        Args:
            query: Optional search term to filter chats by name or JID
            limit: Maximum number of chats to return (default 20)
            page: Page number for pagination (default 0)
            include_last_message: Whether to include the last message in each chat (default True)
            sort_by: Field to sort results by, either "last_active" or "name" (default "last_active")
        """
        return _wa.list_chats(query=query, limit=limit, page=page,
                              include_last_message=include_last_message, sort_by=sort_by)


def _register_list_messages(mcp) -> None:
    # main.py imported upstream list_messages by name, so replace the tool, not just the function
    _remove_tool(mcp, "list_messages")

    @mcp.tool()
    def list_messages(after: Optional[str] = None, before: Optional[str] = None,
                      sender_phone_number: Optional[str] = None, chat_jid: Optional[str] = None,
                      query: Optional[str] = None, limit: int = 20, page: int = 0,
                      include_context: bool = True, context_before: int = 1, context_after: int = 1):
        """Get WhatsApp messages matching specified criteria with optional context.

        A 1:1 chat stored under both the contact's phone number and LID is listed as one chat.

        Args:
            after: Optional ISO-8601 formatted string to only return messages after this date
            before: Optional ISO-8601 formatted string to only return messages before this date
            sender_phone_number: Optional phone number to filter messages by sender
            chat_jid: Optional chat JID to filter messages by chat
            query: Optional search term to filter messages by content
            limit: Maximum number of messages to return (default 20)
            page: Page number for pagination (default 0)
            include_context: Whether to include messages before and after matches (default True)
            context_before: Number of messages to include before each match (default 1)
            context_after: Number of messages to include after each match (default 1)
        """
        return _wa.list_messages(after=after, before=before, sender_phone_number=sender_phone_number,
                                 chat_jid=chat_jid, query=query, limit=limit, page=page,
                                 include_context=include_context, context_before=context_before,
                                 context_after=context_after)


def _media_details(path: str, mtype: str) -> str:
    """MIME type and, for audio/video, duration (via ffprobe when installed)."""
    import mimetypes
    parts = [mimetypes.guess_type(path)[0] or "application/octet-stream"]
    if mtype in ("audio", "video") and shutil.which("ffprobe"):
        try:
            out = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration",
                                  "-of", "default=nw=1:nk=1", path],
                                 capture_output=True, text=True, timeout=20).stdout.strip()
            parts.append(f"{float(out):.1f} s")
        except (ValueError, subprocess.SubprocessError, OSError):
            pass
    return ", ".join(parts)


class TranscriptionUnavailable(Exception):
    pass


def _register_download(mcp) -> None:
    _remove_tool(mcp, "download_media")

    from mcp.server.fastmcp import Image

    @mcp.tool()
    def download_media(message_id: str, chat_jid: str, content: str = "auto"):
        """Download media from a WhatsApp message and return it.

        content:
          "auto" (default) – images are returned as images, small text files as text;
                             other media (audio, video, PDF, documents) return metadata only.
          "text"           – extract text from a PDF/document (only when the user asks).
          "transcribe"     – transcribe a voice message / audio / video (only when asked;
                             runs locally on the bridge machine, may take a while).
          "file"           – return the raw file as an embedded resource (base64), max ~10 MB.
        If the media has expired from WhatsApp's servers, the user's phone is asked to
        re-upload it (up to ~45 s; the phone must be online).

        Args:
            message_id: The ID of the message containing the media
            chat_jid: The JID of the chat containing the message
            content: "auto" | "text" | "transcribe" | "file"
        """
        res = _post("download2", {"message_id": message_id, "chat_jid": chat_jid}, timeout=150)
        if not res.get("success"):
            return res
        path = res.get("path", "")
        mtype = res.get("media_type", "")
        # Never hand back a file that belongs to another message
        if res.get("message_id") != message_id or not os.path.basename(path).startswith(re.sub(r"[^A-Za-z0-9_-]", "_", message_id)):
            return {"success": False, "message_id": message_id,
                    "message": f"bridge returned a file that does not match message {message_id} ({os.path.basename(path)})"}
        if not os.path.exists(path):
            return {"success": False, "message_id": message_id, "message": f"downloaded file is missing: {path}"}
        ext = os.path.splitext(path)[1].lower()
        orig = res.get("original_filename") or ""
        meta = (f"Message {message_id}: {res.get('filename')} ({mtype}, {_media_details(path, mtype)}, "
                f"{res.get('bytes') or os.path.getsize(path)} B)"
                f"{f' — original name {orig}' if mtype == 'document' and orig else ''}"
                f"{' — restored from phone' if res.get('retried') else ''}\nFile (on the bridge host): {path}")
        try:
            if content == "transcribe":
                try:
                    return meta + "\n\n--- transcript ---\n" + _transcribe(path)
                except TranscriptionUnavailable as e:
                    return {"success": False, "message_id": message_id, "path": path,
                            "message": f"transcription unavailable: {e}"}
            if content == "text":
                return meta + "\n\n--- text ---\n" + _extract_text(path)
            if content == "file":
                return _as_resource(path, meta)
            # auto
            if mtype == "image" and os.path.exists(path):
                return [Image(path=_shrink_image(path)), meta]
            if ext in TEXT_EXTS and os.path.getsize(path) < 200_000:
                with open(path, "r", errors="replace") as f:
                    return meta + "\n\n--- content ---\n" + f.read()
            hint = {"audio": "content='transcribe'", "video": "content='transcribe'"}.get(mtype, "content='text' or content='file'")
            return meta + f"\n(Content not fetched. Ask with {hint} if needed.)"
        except Exception as e:
            return {**res, "note": f"File downloaded, but processing failed: {e}"}


def _extract_text(path: str, max_chars: int = 60_000) -> str:
    ext = os.path.splitext(path)[1].lower()
    if ext in TEXT_EXTS:
        with open(path, "r", errors="replace") as f:
            return f.read()[:max_chars]
    if ext == ".pdf":
        try:
            from pypdf import PdfReader
        except ImportError:
            return "(pypdf is missing: run `uv add pypdf` in the server directory)"
        reader = PdfReader(path)
        parts, total = [], 0
        for i, page in enumerate(reader.pages):
            t = page.extract_text() or ""
            parts.append(f"[page {i + 1}]\n{t}")
            total += len(t)
            if total > max_chars:
                parts.append("… (truncated)")
                break
        text = "\n".join(parts).strip()
        return text or "(the PDF has no text layer – probably a scanned image)"
    if ext == ".docx":
        try:
            import zipfile, re
            with zipfile.ZipFile(path) as z:
                xml = z.read("word/document.xml").decode("utf8", "replace")
            xml = re.sub(r"</w:p>", "\n", xml)
            return re.sub(r"<[^>]+>", "", xml)[:max_chars]
        except Exception as e:
            return f"(failed to extract docx text: {e})"
    return f"(text extraction is not supported for {ext} files – try content='file')"


def _transcribe(path: str) -> str:
    """Transcribe locally with whisper.cpp (whisper-cli) + ffmpeg. The result is cached in a .txt file."""
    cache = path + ".transcript.txt"
    if os.path.exists(cache):
        with open(cache, "r", errors="replace") as f:
            return f.read()
    model = os.environ.get("WHISPER_MODEL", os.path.expanduser("~/models/ggml-large-v3-turbo.bin"))
    whisper = shutil.which("whisper-cli") or shutil.which("whisper-cpp")
    if not whisper or not shutil.which("ffmpeg"):
        raise TranscriptionUnavailable("whisper-cli and ffmpeg are required on the bridge host (brew install whisper-cpp ffmpeg)")
    if not os.path.exists(model):
        raise TranscriptionUnavailable(f"whisper model not found: {model} (download a model or set WHISPER_MODEL)")
    with tempfile.TemporaryDirectory() as td:
        wav = os.path.join(td, "in.wav")
        subprocess.run(["ffmpeg", "-y", "-loglevel", "error", "-i", path, "-ar", "16000", "-ac", "1", wav],
                       check=True, timeout=300)
        out = os.path.join(td, "out")
        subprocess.run([whisper, "-m", model, "-l", os.environ.get("WHISPER_LANG", "auto"),
                        "-f", wav, "-otxt", "-of", out, "-np"],
                       check=True, timeout=1800, capture_output=True)
        with open(out + ".txt", "r", errors="replace") as f:
            text = f.read().strip()
    with open(cache, "w") as f:
        f.write(text)
    return text or "(empty transcript)"


def _as_resource(path: str, meta: str):
    import base64, mimetypes
    from mcp.types import EmbeddedResource, BlobResourceContents
    size = os.path.getsize(path)
    if size > 10_000_000:
        return meta + f"\n(File is too large to embed: {size} B)"
    mime = mimetypes.guess_type(path)[0] or "application/octet-stream"
    with open(path, "rb") as f:
        data = base64.b64encode(f.read()).decode()
    return [meta, EmbeddedResource(type="resource", resource=BlobResourceContents(
        uri=f"file://{path}", mimeType=mime, blob=data))]
