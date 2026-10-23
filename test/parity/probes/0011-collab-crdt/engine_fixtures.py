#!/usr/bin/env python
"""Engine fixtures: the ORACLE's apply_client_updates verdicts for the real
edit-stream batch (with the budget switched on) and a no-op re-apply, for the
Go port in doory/collab_engine.go.

Run from the upstream backend tree with the oracle venv:

  cd .upstream/current/backend
  OUT_DIR=<probe dir> <venv>/python <probe dir>/engine_fixtures.py

Output: engine_fixtures.json
"""
import base64
import json
import os
import sys

import django

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "doory.settings.test")
sys.path.insert(0, os.getcwd())
django.setup()

from apps.collab import engine, protocol, schema as ds  # noqa: E402

OUT = os.environ["OUT_DIR"]


def b64(b: bytes) -> str:
    return base64.b64encode(b).decode()


def main():
    base = open(os.path.join(OUT, "edits/base.snapshot.bin"), "rb").read()
    updates = [
        open(os.path.join(OUT, "edits", f"{i:03d}.update"), "rb").read() for i in range(12)
    ]
    doc = engine.open_doc(base, [])
    client = next(iter(doc.sv))  # the base/edits client
    introduced = [{client} for _ in updates]
    result = engine.apply_client_updates(doc.doc, dict(doc.sv), updates, doc.size,
                                         engine.UpdateBudget(), introduced)

    batch = {
        "base": b64(base),
        "updates": [b64(u) for u in updates],
        "initial_size": doc.size,
        "client": client,
        "introduced": [[client] for _ in updates],
        "outcomes": [
            {"kind": o.kind, "reason": o.reason, "detail": o.detail} for o in result.outcomes
        ],
        "sv": {str(k): v for k, v in result.sv.items()},
        "size": result.size,
        "merged": b64(result.merged) if result.merged else None,
        "applied_count": len(result.applied),
        "pages_gone": result.pages_gone,
        "poisoned": result.poisoned,
    }

    # No-op: re-apply the last update to the resulting document (its state
    # already covers it; no deletes in that update).
    doc2 = engine.open_doc(result.merged, [])
    noop = engine.apply_client_updates(doc2.doc, dict(doc2.sv), [updates[-1]], doc2.size,
                                       engine.UpdateBudget(), [{client}])
    noop_case = {
        "merged_state": batch["merged"],
        "update": b64(updates[-1]),
        "initial_size": doc2.size,
        "outcomes": [
            {"kind": o.kind, "reason": o.reason, "detail": o.detail} for o in noop.outcomes
        ],
        "sv": {str(k): v for k, v in noop.sv.items()},
        "size": noop.size,
        "pages_gone": noop.pages_gone,
    }

    out = {
        "generated_by": "test/parity/probes/0011-collab-crdt/engine_fixtures.py",
        "max_design_document_bytes": ds.MAX_VALUE_DEPTH and __import__("django.conf", fromlist=["settings"]).settings.MAX_DESIGN_DOCUMENT_BYTES,
        "batch": batch,
        "noop": noop_case,
    }
    with open(os.path.join(OUT, "engine_fixtures.json"), "w", encoding="utf-8") as fh:
        json.dump(out, fh, ensure_ascii=False, separators=(",", ":"))
    print("engine fixtures written:",
          [o["kind"] for o in batch["outcomes"]], "noop:", [o["kind"] for o in noop_case["outcomes"]])


if __name__ == "__main__":
    main()
