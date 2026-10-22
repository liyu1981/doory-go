# Vendored yjs-go (third_party/) — patch record

Pinned at **upstream v0.1.6** (`github.com/CivNode/yjs-go`, MIT) with the
patches below, all driven by the S0 conformance fixtures
(`test/parity/probes/0011-collab-crdt/`, pycrdt 0.14.10 / pinned upstream
commit) and checked in both directions by `internal/crdt`:

- oracle fixtures -> Go: snapshots + state vectors byte-equal, 12-update
  edit stream (document + state vector + merge paths), concurrent streams
  converge in both orders;
- Go -> oracle: `go_updates/verdict.json` — pycrdt applies the Go witness
  update and the documents are equal.

Keep this list in sync with the code (each patch says "probed against
pycrdt fixtures" at the site).

## Wire format (reference Yjs/yrs behavior)

1. **parentSub with a left origin** (`protocol/update.go`): the map key is
   encoded only when neither origin is present (with a left origin it lives
   in the origin's slot and is derived at integration). Both the decoder
   and the encoder had it unconditional.
2. **Map.set replacement form** (`map.go`): a slot replacement carries
   `originLeft = old item` and omits parent+key on the wire; only a fresh
   key writes the first-write form.
3. **parentSub/parent inheritance at integration** (`integrate.go`): derive
   the parent (and map key) from the left neighbor, and — for
   insert-at-head items — from the RIGHT neighbor.

## Nested types (ContentType items)

4. **Materialization** (`integrate.go`): a decoded ref-7 item's content is
   the shared node (typeRef 0=Array 1=Map 2=Text); children resolve their
   parent through the carrying item.
5. **parentID plumbing** (`item.go`, `encoding.go`): `Item.parentID` is
   retained from the wire (it was dropped), and re-encoding a nested parent
   writes its carrying item's ID (`nodeItemID`, set on materialize and on
   local insert).
6. **Local node values** (`map.go`, `array.go`, `item.go`):
   `NewNestedMap/Array/Text` + Kind=contentType on Set/Insert. Builder
   discipline (documented there): insert the node into its parent BEFORE
   populating it — children must follow the type item in clock order (there
   is no local pending path for our writes).

## Lifecycle

7. **Pending buffer** (`integrate.go`, `doc.go`): items whose parent has
   not arrived pend and retry across the update (reference
   pendingStructs). A multi-client full-state update groups clients in
   arbitrary order — pycrdt emits edits before the base.
8. **Sorted struct store** (`store.go`): `addItem` inserts by clock
   (binary search + state-vector tail both assume sorted); pending retries
   integrate out of clock order.
9. **Snapshot delete set** (`encoding.go`): `EncodeStateAsUpdate` now
   emits delete ranges for deleted items — without it a snapshot
   resurrected deletions.
10. **Multi-value ContentAny in arrays** (`array.go`): `ToSlice/Get/Len`
    count VALUES (an insert batch is one item holding N values).
11. **Map view walk** (`map.go`): `Get/Keys` walk past a deleted slot head
    to the first live item (remote replaces insert after the head).

## Known gaps (candidates for upstreaming + follow-ups)

- `transport` package fetches its own module dependency (`nhooyr.io/websocket`)
  and is not part of the main module build; tests for the core and
  protocol packages run green via `go test github.com/CivNode/yjs-go/...`.
- Array index operations (`Insert/Delete/findItemAtIndex`) still count
  ITEMS, not values, when a decoded multi-value batch is present — the Go
  collab port must either patch these or go through value-based paths
  (scenario coverage will catch it).
- No out-of-order delivery across separate updates for items whose parent
  is missing forever (stranded items stay pending; nothing times them out).
