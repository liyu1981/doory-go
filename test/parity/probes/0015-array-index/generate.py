"""Array INDEX ops over decoded multi-value batches: oracle fixtures (pycrdt/yrs).

The vendored yjs-go decoder materializes a Yjs ContentAny insert batch as ONE
item holding N values (third_party/yjs-go/PATCHES.md item 10). Server-side
array index operations (`Insert`/`Delete`) must therefore count VALUES, and a
position strictly inside a batch has to split it — exactly what Yjs's
`Item.splitItem` does. This fixture pins the oracle's answer for a set of index
operations on top of such batches.

Output (test/parity/probes/0015-array-index/):
  fixtures.json   one record per case: base snapshot (hex), the operation,
                  the oracle's resulting values, value count and state vector
  meta.txt        pins

Case shape:
  {"name": ..., "base_history": [[values...], ...], "op": {"kind":"insert"|"delete", ...},
   "base": "<snapshot hex>", "sv_before": "<hex>", "values": [...], "len": n,
   "sv_after": "<hex>"}

The Go side (internal/crdt/array_index_test.go) applies `base_history` to a
fresh doc, then performs the SAME operation through the Go Array API and must
reproduce `values`, `len` and `sv_after`.
"""
import json
import os
import subprocess
import sys

REPO = os.environ["REPO_DIR"]
OUT = os.environ["OUT_DIR"]
BACKEND = os.path.join(REPO, ".upstream/current/backend")
sys.path.insert(0, BACKEND)

from pycrdt import Array, Doc  # noqa: E402

ROOT = "pageOrder"

CASES = [
    # (name, base history, operation)
    ("insert-inside-batch", [["a", "b", "c"]], {"kind": "insert", "index": 2, "values": ["X"]}),
    ("insert-at-batch-start", [["a", "b", "c"]], {"kind": "insert", "index": 0, "values": ["X"]}),
    ("insert-at-batch-end", [["a", "b", "c"]], {"kind": "insert", "index": 3, "values": ["X"]}),
    ("insert-multi-values-inside-batch", [["a", "b", "c"]], {"kind": "insert", "index": 1, "values": ["X", "Y"]}),
    ("delete-one-inside-batch", [["a", "b", "c"]], {"kind": "delete", "index": 1, "length": 1}),
    ("delete-across-batch", [["a", "b", "c"]], {"kind": "delete", "index": 1, "length": 2}),
    ("delete-tail-of-batch", [["a", "b", "c"]], {"kind": "delete", "index": 2, "length": 1}),
    ("insert-between-two-batches", [["a", "b"], ["c", "d"]], {"kind": "insert", "index": 2, "values": ["Z"]}),
    ("delete-spanning-two-batches", [["a", "b"], ["c", "d"]], {"kind": "delete", "index": 1, "length": 2}),
]


def build(history):
    """A history entry is one INSERT BATCH: pycrdt's extend() writes the values
    as a single ContentAny item (one Yjs insert), which is the decode shape the
    Go port must index over value-by-value."""
    doc = Doc()
    arr = doc.get(ROOT, type=Array)
    for values in history:
        with doc.transaction():
            arr.extend(values)
    return doc, arr


def main():
    os.makedirs(OUT, exist_ok=True)
    records = []
    for name, history, op in CASES:
        doc, arr = build(history)
        base = doc.get_update()
        sv_before = doc.get_state()
        with doc.transaction():
            if op["kind"] == "insert":
                if len(op["values"]) == 1:
                    arr.insert(op["index"], op["values"][0])
                else:
                    # several new values at one position, one transaction
                    tail = list(arr)[op["index"]:]
                    del arr[op["index"]:]
                    arr.extend(op["values"] + tail)
            else:
                del arr[op["index"]:op["index"] + op["length"]]
        records.append(
            {
                "name": name,
                "base_history": history,
                "op": op,
                "base": base.hex(),
                "sv_before": sv_before.hex(),
                "values": list(arr),
                "len": len(arr),
                "sv_after": doc.get_state().hex(),
            }
        )
    with open(os.path.join(OUT, "fixtures.json"), "w") as fh:
        json.dump(records, fh, indent=1, ensure_ascii=False)
    import pycrdt

    sha = subprocess.check_output(
        ["git", "-C", os.path.join(REPO, ".upstream/current"), "rev-parse", "HEAD"]
    ).decode().strip()
    with open(os.path.join(OUT, "meta.txt"), "w") as fh:
        fh.write(
            "probe=0015-array-index\n"
            f"upstream_commit={sha}\n"
            f"engine=pycrdt (yrs) {getattr(pycrdt, '__version__', 'unknown')}\n"
            "note=array Insert/Delete over decoded multi-value ContentAny batches; never edit\n"
        )
    print(f"array index fixtures: {len(records)} cases")


if __name__ == "__main__":
    main()
