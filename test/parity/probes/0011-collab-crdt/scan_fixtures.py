#!/usr/bin/env python
"""S0 follow-up: scanner fixtures — the ORACLE's scan_update verdicts for the
real updates (replayed against the state they would apply to) plus crafted
negative cases, for the Go port in internal/yproto.

Run from the upstream backend tree with the oracle venv:

  cd .upstream/current/backend
  OUT_DIR=<probe dir> <venv>/python <probe dir>/scan_fixtures.py

Output: scan_fixtures.json — one entry per case:
  {update file, known state vector (hex), accepted, error message, counters}
"""
import base64
import json
import os
import sys

import django

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "doory.settings.test")
sys.path.insert(0, os.getcwd())
django.setup()

from pycrdt import Doc, Map, Array  # noqa: E402
from apps.collab import protocol, schema as ds  # noqa: E402

OUT = os.environ["OUT_DIR"]
ROOTS = frozenset(ds.ROOTS)


def scan(update: bytes, known: dict[int, int]) -> dict:
    try:
        s = protocol.scan_update(update, known, allowed_roots=ROOTS)
    except protocol.ProtocolError as exc:
        return {"accepted": False, "error": str(exc)}
    return {
        "accepted": True,
        "structs": s.structs,
        "types": s.types,
        "has_items": s.has_items,
        "has_deletes": s.has_deletes,
        "extents": {str(k): v for k, v in s.extents.items()},
        "new_clients": sorted(s.new_clients),
        "originless_entries": len(s.originless),
        "originless_total": sum(s.originless.values()),
        "right_only": s.right_only,
        "inside_refs": s.inside_refs,
    }


def new_doc() -> Doc:
    return ds.new_doc()


def b64(b: bytes) -> str:
    return base64.b64encode(b).decode()


def main():
    cases = []

    def add(name, update, known, extra=None):
        entry = {"name": name, "update": b64(update), "known": {str(k): v for k, v in known.items()}}
        entry.update(scan(update, known))
        if extra:
            entry.update(extra)
        cases.append(entry)

    # --- real edit stream: known = state vector before each update
    doc = new_doc()
    doc.apply_update(open(os.path.join(OUT, "edits/base.snapshot.bin"), "rb").read())
    for i in range(12):
        path = os.path.join(OUT, "edits", f"{i:03d}.update")
        update = open(path, "rb").read()
        known = protocol.decode_state_vector(doc.get_state(), limit=None)
        add(f"edits/{i:03d}", update, known)
        doc.apply_update(update)

    # --- concurrent streams against the base state
    base = open(os.path.join(OUT, "basic.snapshot.bin"), "rb").read()
    bdoc = new_doc()
    bdoc.apply_update(base)
    known = protocol.decode_state_vector(bdoc.get_state(), limit=None)
    add("conc/left", open(os.path.join(OUT, "conc/left.update"), "rb").read(), known)
    add("conc/right", open(os.path.join(OUT, "conc/right.update"), "rb").read(), known)

    # --- snapshot re-application (idempotent: no gap)
    add("basic.snapshot", base, known)

    # --- crafted negatives (byte-level manipulation of a real update)
    real = open(os.path.join(OUT, "edits/000.update"), "rb").read()
    add("neg/truncated", real[: len(real) // 2], known)
    add("neg/trailing", real + b"\x00\x00", known)
    add("neg/empty", b"", known)
    add("neg/clients-count-huge", b"\x7f\x7f\x7f\x01", known)

    # unknown root type: rewrite the root name of a no-origin struct
    # (edits/008 writes a page - root pages; swap its parent YKey).
    upd8 = bytearray(open(os.path.join(OUT, "edits/008.update"), "rb").read())
    idx = bytes(upd8).find(b"pages")
    if idx >= 0:
        upd8[idx : idx + 5] = b"hacks"
        doc8 = new_doc()
        doc8.apply_update(open(os.path.join(OUT, "edits/base.snapshot.bin"), "rb").read())
        for i in range(8):
            doc8.apply_update(open(os.path.join(OUT, "edits", f"{i:03d}.update"), "rb").read())
        add("neg/unknown-root", bytes(upd8), protocol.decode_state_vector(doc8.get_state(), limit=None))

    # reference to unknown history: a state vector naming a future clock
    docf = new_doc()
    docf.apply_update(base)
    knownf = dict(protocol.decode_state_vector(docf.get_state(), limit=None))
    victim = next(iter(knownf))
    knownf[victim] = knownf[victim] + 1000
    add("neg/reference-gap", open(os.path.join(OUT, "edits/000.update"), "rb").read(), knownf)

    # deep nesting: build a document with 40 nested any-objects and scan its
    # snapshot (values nested beyond MAX_ANY_DEPTH=32).
    deep = new_doc()
    meta = deep.get("meta", type=Map)
    value = "leaf"
    for _ in range(40):
        value = {"n": value}
    meta["deep"] = value
    add("neg/deep-nesting", deep.get_update(), {victim: 0})

    out = {
        "generated_by": "test/parity/probes/0011-collab-crdt/scan_fixtures.py",
        "pycrdt": getattr(__import__("pycrdt"), "__version__", "unknown"),
        "max_any_depth": protocol.MAX_ANY_DEPTH,
        "allowed_roots": list(sorted(ROOTS)),
        "cases": cases,
    }
    with open(os.path.join(OUT, "scan_fixtures.json"), "w", encoding="utf-8") as fh:
        json.dump(out, fh, ensure_ascii=False, separators=(",", ":"))
    ok = sum(1 for c in cases if c["accepted"])
    print(f"scan fixtures: {len(cases)} cases ({ok} accepted, {len(cases) - ok} rejected)")


if __name__ == "__main__":
    main()
