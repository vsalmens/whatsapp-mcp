"""chat_import.py — parse WhatsApp "Export chat" text files (iPhone and Android, any common locale).

Recognised line headers (an optional U+200E mark may precede them):
    [24.11.2025 klo 9.19.38] Name: text        iPhone, Finnish
    [24/11/2025, 09:19:38] Name: text          iPhone, en-GB
    [11/24/25, 9:19:38 AM] Name: text          iPhone, en-US
    24.11.2025 klo 9.19 - Name: text           Android, Finnish
    24/11/2025, 09:19 - Name: text             Android, en-GB
    2025-11-24 09:19 - Name: text              Android, ISO
Lines without a header continue the previous message. Header lines without "Name: " are system
messages (encryption notice, group changes) and are skipped.

The day/month order is detected from the file (a value above 12 decides it); if every date is
ambiguous, day-first is assumed unless `date_order` says otherwise. Export times are the phone's
local time; they are interpreted in the server's local time zone unless `tz` is given.
"""

import hashlib
import io
import os
import re
import zipfile
from dataclasses import dataclass, field
from datetime import datetime
from typing import Dict, List, Optional, Tuple

_HEADER = re.compile(
    r"^‎?\[?(?P<date>\d{1,4}[./-]\d{1,2}[./-]\d{2,4})\.?,?\s+(?:klo\s+)?"
    r"(?P<time>\d{1,2}[:.]\d{2}(?:[:.]\d{2})?)(?:\s*(?P<ampm>[AaPp]\.?\s?[Mm]\.?))?"
    r"(?:\]\s*|\s+-\s+)(?P<rest>.*)$"
)
_SENDER = re.compile(r"^(?P<sender>[^:]{1,80}?):\s(?P<text>.*)$", re.S)


@dataclass
class ExportMessage:
    timestamp: datetime
    sender: str
    text: str


@dataclass
class ParsedExport:
    messages: List[ExportMessage] = field(default_factory=list)
    date_order: str = "dmy"
    system_lines: int = 0
    unparsed_lines: int = 0

    def senders(self) -> Dict[str, int]:
        out: Dict[str, int] = {}
        for m in self.messages:
            out[m.sender] = out.get(m.sender, 0) + 1
        return dict(sorted(out.items(), key=lambda kv: -kv[1]))


def read_export_text(path: str) -> str:
    """Text of a .txt export, or of the chat .txt inside a .zip export (media are ignored)."""
    if path.lower().endswith(".zip"):
        with zipfile.ZipFile(path) as z:
            names = [n for n in z.namelist() if n.lower().endswith(".txt")]
            if not names:
                raise ValueError("the zip file contains no .txt chat export")
            # The chat itself is the largest text file ("_chat.txt" on iPhone)
            name = max(names, key=lambda n: z.getinfo(n).file_size)
            return io.TextIOWrapper(z.open(name), encoding="utf-8-sig", errors="replace").read()
    with open(path, "r", encoding="utf-8-sig", errors="replace") as f:
        return f.read()


def _normalise(line: str) -> str:
    return line.replace(" ", " ").replace(" ", " ").rstrip("\r")


def _detect_date_order(dates: List[str]) -> Optional[str]:
    for d in dates:
        a, b, _ = re.split(r"[./-]", d)
        if len(a) == 4:
            return "ymd"
        if int(a) > 12:
            return "dmy"
        if int(b) > 12:
            return "mdy"
    return None


def _to_datetime(date: str, time: str, ampm: Optional[str], order: str) -> datetime:
    p = [int(x) for x in re.split(r"[./-]", date)]
    if order == "ymd":
        y, m, d = p
    elif order == "mdy":
        m, d, y = p
    else:
        d, m, y = p
    if y < 100:
        y += 2000
    t = [int(x) for x in re.split(r"[:.]", time)]
    hh, mm, ss = t[0], t[1], (t[2] if len(t) > 2 else 0)
    if ampm:
        pm = ampm.lower().startswith("p")
        if hh == 12:
            hh = 0
        if pm:
            hh += 12
    return datetime(y, m, d, hh, mm, ss)


def parse_export(text: str, date_order: Optional[str] = None, tz=None) -> ParsedExport:
    lines = [_normalise(l) for l in text.split("\n")]
    heads = [(i, _HEADER.match(l)) for i, l in enumerate(lines)]
    order = date_order or _detect_date_order([m.group("date") for _, m in heads if m]) or "dmy"
    out = ParsedExport(date_order=order)
    current: Optional[Tuple[datetime, str, List[str]]] = None

    def flush():
        if current:
            ts, sender, parts = current
            out.messages.append(ExportMessage(ts, sender, "\n".join(parts).strip()))

    for i, m in heads:
        line = lines[i]
        if m:
            try:
                ts = _to_datetime(m.group("date"), m.group("time"), m.group("ampm"), order)
            except ValueError:
                m = None
        if m:
            sm = _SENDER.match(m.group("rest"))
            flush()
            current = None
            if not sm:
                out.system_lines += 1
                continue
            ts = ts.replace(tzinfo=tz) if tz else ts.astimezone()
            current = (ts, sm.group("sender").strip().lstrip("‎~ ").strip(), [sm.group("text").lstrip("‎")])
        elif current is not None:
            current[2].append(line)
        elif line.strip():
            out.unparsed_lines += 1
    flush()
    return out


def import_ids(chat_jid: str, messages: List[ExportMessage], from_me: List[bool]) -> List[str]:
    """Deterministic IDs ("import-<hash>"), so importing the same export twice adds nothing.
    Identical messages in the same minute are told apart by their order of occurrence."""
    seen: Dict[str, int] = {}
    ids = []
    for m, me in zip(messages, from_me):
        key = f"{chat_jid}|{m.timestamp.isoformat()}|{'me' if me else m.sender}|{m.text}"
        n = seen.get(key, 0)
        seen[key] = n + 1
        ids.append("import-" + hashlib.sha1(f"{key}|{n}".encode()).hexdigest()[:24])
    return ids


def guess_chat_name(file_name: str) -> str:
    """Contact or group name from the export's file name, e.g. "WhatsApp Chat with Pentti.txt",
    "WhatsApp-keskustelu henkilön Pentti kanssa.txt", "WhatsApp Chat - Pentti.zip"."""
    base = os.path.splitext(os.path.basename(file_name))[0]
    for pat in (r"WhatsApp[- ]keskustelu henkilön (.+) kanssa", r"WhatsApp[- ]keskustelu ryhmässä (.+)",
                r"WhatsApp[- ]keskustelu[: -]+(.+)", r"WhatsApp Chat with (.+)", r"WhatsApp Chat - (.+)",
                r"WhatsApp[- ]Chat[: -]+(.+)"):
        m = re.match(pat, base, re.I)
        if m:
            return re.sub(r"\s*\(\d+\)$", "", m.group(1)).strip()
    return base
