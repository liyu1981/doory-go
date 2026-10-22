#!/usr/bin/env python
"""Spike S0 evidence: fixtures from the ORACLE engine (pycrdt/yrs) for the Go
engine conformance test (internal/crdt).

Run from the upstream backend tree with the oracle venv:

  cd .upstream/current/backend
  ../../.oracle/venv/bin/python <repo>/test/parity/probes/0011-collab-crdt/generate.py

Outputs (raw evidence, never edited afterwards):
  <doc>.doc.json            the DoryDocument fed to document_to_ydoc
  <doc>.snapshot.bin        document_to_ydoc(doc).get_update()
  <doc>.sv.bin              get_state() after building
  <doc>.roundtrip.json      ydoc_to_document() - the oracle's own JSON view
  edits/NNN.update          incremental transaction updates (TransactionEvent.update)
  edits/final.json          ydoc_to_document after all edits
  edits/final.sv.bin        state vector after all edits
  conc/left.update, conc/right.update   concurrent streams from one snapshot
  conc/merged.json          converged ydoc_to_document (both applied)
  conc/merged.sv.bin        converged state vector
  conc/merge_updates.bin    merge_updates(*applied batches) - compaction path
  meta.json                 pins (pycrdt version, upstream sha, date)
"""
import json
import os
import subprocess
import sys

import django

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "doory.settings.test")
sys.path.insert(0, os.getcwd())
django.setup()

from pycrdt import Array, Doc, Map, Text, merge_updates  # noqa: E402
from apps.collab import schema as ds  # noqa: E402

OUT = os.environ["OUT_DIR"]
REPO = os.environ["REPO_DIR"]

DOCS = {
    "basic": {
        "title": "Spike Design",
        "background": "#ffffff",
        "pages": [
            {
                "id": "11111111-1111-4111-8111-111111111111",
                "name": "Cover",
                "width": 794,
                "height": 1123,
                "objects": [
                    {
                        "id": "aaaaaaa1-0000-4000-8000-000000000001",
                        "type": "text", "x": 40, "y": 60, "width": 300,
                        "text": "Hello, parity", "fontSize": 24, "zIndex": 1,
                    },
                    {
                        "id": "aaaaaaa1-0000-4000-8000-000000000002",
                        "type": "rect", "x": 10.5, "y": 200.25, "width": 120, "height": 80,
                        "fill": "#ff0044", "locked": False, "opacity": 0.75,
                        "props": {"shadow": {"blur": 4, "dx": 0, "dy": 2, "color": "#00000033"}},
                    },
                ],
            },
            {
                "id": "22222222-2222-4222-8222-222222222222",
                "name": "Second",
                "objects": [],
            },
        ],
    },
    "unicode": {
        "title": "Ünïcødé — تعمیر — 🎨",
        "pages": [
            {
                "id": "33333333-3333-4333-8333-333333333333",
                "name": "مixture 💡",
                "objects": [
                    {
                        "id": "bbbbbbb1-0000-4000-8000-000000000001",
                        "type": "text", "text": "café ☕ zwölf", "x": 0, "y": 0,
                        "combining": "a\u0301e\u0301",
                    }
                ],
            }
        ],
    },
    "numbers": {
        "counter": 42,
        "pi": 3.14159,
        "big": 9007199254740991,
        "neg": -17,
        "yes": True,
        "no": False,
        "nothing": None,
        "pages": [
            {"id": "44444444-4444-4444-8444-444444444444", "name": "n", "objects": []}
        ],
    },
    "empty": {"pages": []},
}


def save(name, data):
    path = os.path.join(OUT, name)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    if isinstance(data, (bytes, bytearray)):
        with open(path, "wb") as fh:
            fh.write(data)
    elif isinstance(data, str):
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(data)
    else:
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(data, fh, ensure_ascii=False, sort_keys=False, separators=(",", ":"))


def build_docs():
    for name, doc in DOCS.items():
        ydoc = ds.document_to_ydoc(doc)
        save(f"{name}.doc.json", doc)
        save(f"{name}.snapshot.bin", ydoc.get_update())
        save(f"{name}.sv.bin", ydoc.get_state())
        save(f"{name}.roundtrip.json", ds.ydoc_to_document(ydoc))


def edit_stream():
    ydoc = ds.document_to_ydoc(DOCS["basic"])
    save("edits/base.snapshot.bin", ydoc.get_update())  # the base these edits build on (same doc!)
    updates = []
    holder = {}
    def cb(event):
        holder["u"] = event.update
    subscription = ydoc.observe(cb)
    meta = ydoc.get(ds.ROOT_META, type=Map)
    order = ydoc.get(ds.ROOT_PAGE_ORDER, type=Array)
    pages = ydoc.get(ds.ROOT_PAGES, type=Map)
    page = pages[order[0]]
    objects = page[ds.KEY_OBJECTS]
    object_order = page[ds.KEY_OBJECT_ORDER]

    steps = [
        lambda: meta.__setitem__("title", "Edited title"),
        lambda: objects[order[0] and "aaaaaaa1-0000-4000-8000-000000000001"].update({"x": 55}),
        lambda: object_order.append("ccccccc1-0000-4000-8000-000000000003"),
        lambda: objects.__setitem__("ccccccc1-0000-4000-8000-000000000003",
                                    {"type": "text", "text": "appended", "x": 9, "y": 9}),
        lambda: objects["ccccccc1-0000-4000-8000-000000000003"].update({"y": 10, "zIndex": 2}),
        lambda: objects.__delitem__("aaaaaaa1-0000-4000-8000-000000000002"),
        lambda: object_order.pop(1),
        lambda: order.append("55555555-5555-4555-8555-555555555555"),
        lambda: pages.__setitem__("55555555-5555-4555-8555-555555555555",
                                  {"id": "55555555-5555-4555-8555-555555555555", "name": "Third",
                                   "objectOrder": [], "objects": {}}),
        lambda: meta.__setitem__("nested", {"deep": {"value": [1, 2, {"x": "y"}]}}),
        lambda: meta.__delitem__("background"),
        lambda: objects["aaaaaaa1-0000-4000-8000-000000000001"].update({"text": "Hello, 𝒸ollab ✓"}),
    ]
    for i, step in enumerate(steps):
        with ydoc.transaction():
            step()
        updates.append(holder["u"])
        save(f"edits/{i:03d}.update", holder["u"])
    ydoc.unobserve(subscription)
    save("edits/final.json", ds.ydoc_to_document(ydoc))
    save("edits/final.sv.bin", ydoc.get_state())
    save("edits/merged_apply.bin", ydoc.get_update())
    merged = merge_updates(*updates)
    save("edits/merge_updates.bin", merged)


def concurrent():
    base = ds.document_to_ydoc(DOCS["basic"]).get_update()
    left, right = Doc(), Doc()
    left.get(ds.ROOT_META, type=Map); left.get(ds.ROOT_PAGE_ORDER, type=Array); left.get(ds.ROOT_PAGES, type=Map)
    right.get(ds.ROOT_META, type=Map); right.get(ds.ROOT_PAGE_ORDER, type=Array); right.get(ds.ROOT_PAGES, type=Map)
    left.apply_update(base)
    right.apply_update(base)
    lmeta, rmeta = left.get(ds.ROOT_META, type=Map), right.get(ds.ROOT_META, type=Map)
    lorder, rorder = left.get(ds.ROOT_PAGE_ORDER, type=Array), right.get(ds.ROOT_PAGE_ORDER, type=Array)
    lpages, rpages = left.get(ds.ROOT_PAGES, type=Map), right.get(ds.ROOT_PAGES, type=Map)

    with left.transaction():
        lmeta["title"] = "left title"
        lorder.append("66666666-6666-4666-8666-666666666666")
        lpages["66666666-6666-4666-8666-666666666666"] = {
            "id": "66666666-6666-4666-8666-666666666666", "name": "from left",
            "objectOrder": [], "objects": {}}
    left_update = left.get_state_as_update() if hasattr(left, "get_state_as_update") else None
    with right.transaction():
        rmeta["title"] = "right title"
        rmeta["edited_by"] = "right"
        rorder.insert(0, "77777777-7777-4777-8777-777777777777")
        rpages["77777777-7777-4777-8777-777777777777"] = {
            "id": "77777777-7777-4777-8777-777777777777", "name": "from right",
            "objectOrder": [], "objects": {}}

    # incremental updates relative to base: full states minus base is what a client would send as its diff;
    # encode from the peer's state vector like the sync protocol does.
    save("conc/left.update", left.get_update() if left_update is None else left_update)
    save("conc/right.update", right.get_update())

    for peer, upd in ((right, open(os.path.join(OUT, "conc/left.update"), "rb").read()),
                      (left, open(os.path.join(OUT, "conc/right.update"), "rb").read())):
        peer.apply_update(upd)
    save("conc/merged.json", ds.ydoc_to_document(right))
    save("conc/merged.sv.bin", right.get_state())
    save("conc/merged_from_left.json", ds.ydoc_to_document(left))


def main():
    os.makedirs(OUT, exist_ok=True)
    build_docs()
    edit_stream()
    concurrent()
    import pycrdt
    sha = subprocess.check_output(["git", "-C", os.path.join(REPO, ".upstream/current"), "rev-parse", "HEAD"]).decode().strip()
    save("meta.json", {
        "spike": "S0 yrs/yjs engine conformance",
        "engine": "pycrdt (yrs)",
        "pycrdt_version": getattr(pycrdt, "__version__", "unknown"),
        "upstream_commit": sha,
        "generator": "test/parity/probes/0011-collab-crdt/generate.py",
    })
    print("fixtures written to", OUT)


if __name__ == "__main__":
    main()
