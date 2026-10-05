#!/usr/bin/env python3
"""import-ios-backup.py — import WhatsApp history from an iPhone backup's ChatStorage.sqlite.

WhatsApp gives linked devices only part of the history; the phone's own database (in a local,
unencrypted iPhone backup: domain AppDomainGroup-group.net.whatsapp.WhatsApp.shared, file
ChatStorage.sqlite) has all of it, with the real message IDs. This script reads that copy and
sends the messages to the bridge (POST /api/import with keep_ids), which:
  - inserts messages it does not have,
  - fills in empty fields of messages it has (never overwrites a value),
  - stores extra data (starred, quoted message, media details) in message_metadata.

Run on the bridge host. Dry run by default; prints counts only, never message content.

  python3 scripts/import-ios-backup.py --chatstorage /path/ChatStorage.sqlite            # dry run
  python3 scripts/import-ios-backup.py --chatstorage /path/ChatStorage.sqlite --apply    # import
Options: --chat JID (only this chat), --db (default whatsapp-bridge/store/messages.db),
         --api (default http://127.0.0.1:8080/api/import)
"""

import argparse
import json
import os
import sqlite3
import sys
import urllib.request
from collections import Counter, defaultdict
from datetime import datetime, timezone

APPLE_EPOCH = 978307200  # 2001-01-01 in Unix time
SKIP_SERVERS = ("status", "broadcast", "lid.status")

# ZWAMESSAGE.ZMESSAGETYPE -> (media_type, placeholder when there is no text); None = not imported
TYPES = {
    0: ("", ""),            # text
    7: ("", ""),            # text with link preview
    1: ("image", ""),
    2: ("video", ""),
    3: ("audio", ""),       # voice message / audio
    8: ("document", ""),
    11: ("video", ""),      # GIF
    15: ("image", "[sticker]"),
    4: ("", "[contact]"),
    5: ("", "[location]"),
}


def apple_time(v: float) -> str:
    return datetime.fromtimestamp(v + APPLE_EPOCH, tz=timezone.utc).astimezone().isoformat()


def user_part(jid: str) -> str:
    return (jid or "").split("@")[0]


def main() -> int:
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--chatstorage", required=True)
    ap.add_argument("--db", default=os.path.join(here, "..", "whatsapp-bridge", "store", "messages.db"))
    ap.add_argument("--api", default="http://127.0.0.1:8080/api/import")
    ap.add_argument("--chat", help="import only this chat JID (as in the backup or in the bridge)")
    ap.add_argument("--source", default="ios-backup")
    ap.add_argument("--apply", action="store_true", help="import (default: dry run)")
    ap.add_argument("--batch", type=int, default=1000)
    args = ap.parse_args()

    # --- the bridge's current state (read-only) ---
    ours = sqlite3.connect(f"file:{os.path.abspath(args.db)}?mode=ro", uri=True)
    our_chats = {jid for (jid,) in ours.execute("SELECT jid FROM chats")}
    pn_to_lid, lid_to_pn = {}, {}
    for lid, pn in ours.execute("SELECT lid, pn FROM lid_names WHERE lid != '' AND pn != ''"):
        pn_to_lid[pn], lid_to_pn[lid] = lid, pn
    stored = defaultdict(set)  # message id -> chat JIDs it is stored under
    empty_content = set()      # (id, chat) with no text
    for mid, chat, content in ours.execute("SELECT id, chat_jid, COALESCE(content, '') FROM messages"):
        stored[mid].add(chat)
        if not content:
            empty_content.add((mid, chat))
    own = ours.execute("""SELECT sender FROM messages WHERE is_from_me = 1 AND sender NOT LIKE '%@%' AND sender != ''
                          GROUP BY sender ORDER BY count(*) DESC LIMIT 1""").fetchone()
    own = own[0] if own else ""
    ours.close()

    def alternate(jid: str) -> str:
        u, _, server = jid.partition("@")
        if server == "s.whatsapp.net" and u in pn_to_lid:
            return pn_to_lid[u] + "@lid"
        if server == "lid" and u in lid_to_pn:
            return lid_to_pn[u] + "@s.whatsapp.net"
        return ""

    def target_chat(jid: str) -> str:
        if jid in our_chats:
            return jid
        alt = alternate(jid)
        return alt if alt in our_chats else jid

    # --- the phone's database (read-only) ---
    src = sqlite3.connect(f"file:{os.path.abspath(args.chatstorage)}?immutable=1", uri=True)
    chats = {}
    for pk, jid, name in src.execute("SELECT Z_PK, ZCONTACTJID, ZPARTNERNAME FROM ZWACHATSESSION"):
        if not jid or jid.partition("@")[2] in SKIP_SERVERS:
            continue
        chats[pk] = (jid, name or "")
    if args.chat:
        chats = {pk: c for pk, c in chats.items() if args.chat in (c[0], target_chat(c[0]), alternate(c[0]))}
        if not chats:
            print(f"chat {args.chat} not found in the backup")
            return 1
    members = dict(src.execute("SELECT Z_PK, ZMEMBERJID FROM ZWAGROUPMEMBER"))
    media = {r[0]: r[1:] for r in src.execute(
        "SELECT Z_PK, ZMEDIALOCALPATH, ZTITLE, ZFILESIZE, ZMOVIEDURATION, ZLATITUDE, ZLONGITUDE, ZVCARDNAME FROM ZWAMEDIAITEM")}
    stanza = dict(src.execute("SELECT Z_PK, ZSTANZAID FROM ZWAMESSAGE WHERE ZSTANZAID IS NOT NULL"))

    stats = Counter()
    skipped_types = Counter()
    per_chat = defaultdict(list)  # target chat -> messages
    new_chats = {}
    marks = ",".join("?" * len(chats))
    rows = src.execute(f"""
        SELECT ZCHATSESSION, ZSTANZAID, ZMESSAGEDATE, ZISFROMME, ZMESSAGETYPE, ZTEXT, ZFROMJID, ZGROUPMEMBER,
               ZMEDIAITEM, ZPARENTMESSAGE, ZSTARRED
        FROM ZWAMESSAGE WHERE ZCHATSESSION IN ({marks}) ORDER BY ZCHATSESSION, ZMESSAGEDATE""", list(chats))
    for chat_pk, mid, date, from_me, mtype, text, from_jid, member, media_pk, parent, starred in rows:
        stats["messages_in_backup"] += 1
        if not mid or date is None:
            stats["skipped_no_id"] += 1
            continue
        if mtype not in TYPES:
            skipped_types[mtype] += 1
            continue
        jid, name = chats[chat_pk]
        media_type, placeholder = TYPES[mtype]
        m = media.get(media_pk) or (None,) * 7
        local_path, title, size, duration, lat, lon, vcard = m
        content = (text or "").strip() or ((title or "").strip() if media_type and media_type != "document" else "")
        if mtype == 4 and vcard:
            content = f"[contact: {vcard}]"
        elif mtype == 5 and lat is not None and lon is not None:
            content = f"[location: {lat:.6f}, {lon:.6f}]"
        content = content or placeholder

        # Store where the message already is (either JID of the contact), else in the matching chat
        logical = {target_chat(jid), jid, alternate(jid)} - {""}
        existing = stored.get(mid, set()) & logical
        chat_jid = next(iter(existing)) if existing else target_chat(jid)
        if existing:
            if (mid, chat_jid) in empty_content and content:
                stats["would_fill_text"] += 1
            else:
                stats["already_stored"] += 1
        else:
            stats["new"] += 1
            if chat_jid not in our_chats:
                new_chats[chat_jid] = name

        if from_me:
            sender = own
        elif jid.endswith("@g.us"):
            sender = user_part(members.get(member) or from_jid)
        else:
            sender = user_part(jid)

        meta = {}
        if starred:
            meta["starred"] = True
        if parent and stanza.get(parent):
            meta["quoted_id"] = stanza[parent]
        if media_type or mtype in (4, 5):
            md = {k: v for k, v in (("local_path", local_path), ("title", title), ("size", size),
                                    ("duration", duration)) if v not in (None, "", 0)}
            if md:
                meta["media"] = md
            if content and content != placeholder:
                meta["text"] = content  # captions: restored if the bridge re-stores the message without them
        per_chat[chat_jid].append({
            "id": mid, "timestamp": apple_time(date), "sender": sender, "is_from_me": bool(from_me),
            "content": content, "media_type": media_type,
            "filename": (title or "") if media_type == "document" else "",
            "metadata": meta or None,
        })
    src.close()

    msgs = [m for ms in per_chat.values() for m in ms]
    print(f"backup chats: {len(chats)} (status/broadcast skipped); target chats: {len(per_chat)}, "
          f"of which new: {len(new_chats)}")
    print(f"messages in backup: {stats['messages_in_backup']}; importable: {len(msgs)}")
    print(f"  new: {stats['new']}, already stored: {stats['already_stored']}, "
          f"stored without text that the backup has: {stats['would_fill_text']}")
    if msgs:
        print(f"  date range: {min(m['timestamp'] for m in msgs)[:10]} – {max(m['timestamp'] for m in msgs)[:10]}")
    print(f"  with metadata: {sum(1 for m in msgs if m['metadata'])}")
    if skipped_types:
        print("skipped message types (system events, calls, deleted, unknown): "
              + ", ".join(f"{t}: {n}" for t, n in skipped_types.most_common()))
    if not args.apply:
        print("dry run: nothing imported (use --apply)")
        return 0

    totals = Counter()
    for chat_jid, ms in per_chat.items():
        for i in range(0, len(ms), args.batch):
            body = json.dumps({"chat_jid": chat_jid, "chat_name": new_chats.get(chat_jid, ""), "source": args.source,
                               "keep_ids": True, "messages": ms[i:i + args.batch]}).encode()
            req = urllib.request.Request(args.api, data=body, headers={"Content-Type": "application/json"})
            try:
                with urllib.request.urlopen(req, timeout=300) as r:
                    res = json.load(r)
            except Exception as e:  # report and stop; the import can be re-run safely
                print(f"import failed in {chat_jid}: {e}")
                return 2
            if not res.get("success"):
                print(f"import failed in {chat_jid}: {res.get('message')}")
                return 2
            for k in ("inserted", "filled", "with_metadata", "duplicates_removed", "already_present"):
                totals[k] += res.get(k, 0)
        totals["chats"] += 1
        if totals["chats"] % 50 == 0:
            print(f"  … {totals['chats']}/{len(per_chat)} chats")
    print("imported: " + ", ".join(f"{k} {v}" for k, v in totals.items()))
    return 0


if __name__ == "__main__":
    sys.exit(main())
