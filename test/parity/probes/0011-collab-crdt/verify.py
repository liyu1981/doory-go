#!/usr/bin/env python
"""Reverse direction of the S0 conformance proof: apply the Go engine's
witness update under the ORACLE engine (pycrdt) and compare documents.

Run from the upstream backend tree with the oracle venv:

  cd .upstream/current/backend
  OUT_DIR=<probe dir> ../../.oracle/venv/bin/python <probe dir>/verify.py

Inputs (written by `CRDT_WRITE_WITNESS=<probe dir>/go_updates go test ./internal/crdt
-run TestGoUpdatesForOracleWitness`):
  go_updates/go_full.update   full state after the Go-side edits
  go_updates/go_final.json    the Go engine's document view

Output:
  go_updates/verdict.json     oracle's document + equality verdict
"""
import json
import os
import sys

import django

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "doory.settings.test")
sys.path.insert(0, os.getcwd())
django.setup()

from pycrdt import Doc, Map, Array  # noqa: E402
from apps.collab import schema as ds  # noqa: E402

OUT = os.environ["OUT_DIR"]
W = os.path.join(OUT, "go_updates")

with open(os.path.join(W, "go_full.update"), "rb") as fh:
    update = fh.read()
with open(os.path.join(W, "go_final.json"), encoding="utf-8") as fh:
    go_view = json.load(fh)

doc = ds.new_doc()
doc.apply_update(update)
oracle_view = ds.ydoc_to_document(doc)


def json_equal(a, b):
    if isinstance(a, bool) or isinstance(b, bool):
        return isinstance(a, bool) and isinstance(b, bool) and a == b
    if isinstance(a, (int, float)) and isinstance(b, (int, float)):
        return a == b
    if isinstance(a, list) and isinstance(b, list):
        return len(a) == len(b) and all(json_equal(x, y) for x, y in zip(a, b))
    if isinstance(a, dict) and isinstance(b, dict):
        return a.keys() == b.keys() and all(json_equal(v, b[k]) for k, v in a.items())
    return type(a) is type(b) and a == b


verdict = {
    "applied": True,
    "documents_equal": json_equal(oracle_view, go_view),
    "oracle_document": oracle_view,
    "go_document": go_view,
}
with open(os.path.join(W, "verdict.json"), "w", encoding="utf-8") as fh:
    json.dump(verdict, fh, ensure_ascii=False, separators=(",", ":"))

if not verdict["documents_equal"]:
    print("MISMATCH: oracle and Go documents differ")
    print(json.dumps(oracle_view, ensure_ascii=False, indent=1)[:2000])
    print("---")
    print(json.dumps(go_view, ensure_ascii=False, indent=1)[:2000])
    sys.exit(1)
print("verdict: pycrdt applied the Go update; documents equal")
