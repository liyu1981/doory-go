# yjs-go

A Go implementation of the [Yjs](https://yjs.dev/) CRDT protocol.

Yjs is a conflict-free replicated data type (CRDT) framework. Two peers can edit shared data simultaneously without coordination and converge to the same state when they exchange updates. This package implements the core algorithm, the binary v1 wire format, the awareness protocol, and a WebSocket client that speaks the same protocol as [y-websocket](https://github.com/yjs/y-websocket).

**Status: alpha.** The API is usable and the binary format is wire-compatible with the official Yjs implementation. Breaking changes are possible before v1.0.

## Current limitations

The following features are absent or incomplete in v0.1.x:

- **update-v2 not implemented.** `update-v1` is the only encode/decode format. All interop tests run against the v1 wire format, which is what `y-websocket` uses by default.
- **`XmlFragment` has only `Len()`.** No child insertion, no attribute support, no mutations. Do not use `XmlFragment` for XML-tree scenarios; that surface will be fleshed out in a future release.
- **Content types 2 (JSON legacy) and 9 (ContentDoc, nested docs) are decode-only stubs.** They are not produced by any of the primary CRDT types (Text/Map/Array) and are only present for wire-format completeness.
- **`Text.Delete` straddle guard is dead code.** A defensive branch inside `Delete` (for items that straddle the deletion start) is never reached in practice because `findPositionAt` pre-splits items at the target boundary. It is left in place for safety but not covered by tests. This is the remaining coverage gap after v0.1.1.

## Install

```
go get github.com/CivNode/yjs-go
```

Requires Go 1.22 or later. The only non-stdlib dependency is `nhooyr.io/websocket` for the transport package.

## Quick start

```go
package main

import (
    "fmt"
    yjs "github.com/CivNode/yjs-go"
)

func main() {
    // Two documents on different peers.
    docA := yjs.NewDoc()
    docB := yjs.NewDoc()

    // Capture updates so we can exchange them.
    var updA, updB []byte
    docA.OnUpdate(func(u []byte, _ interface{}) { updA = u })
    docB.OnUpdate(func(u []byte, _ interface{}) { updB = u })

    textA := docA.GetText("code")
    textB := docB.GetText("code")

    // Each peer writes concurrently.
    docA.Transact(func() { textA.Insert(0, "Hello") }, nil)
    docB.Transact(func() { textB.Insert(0, "World") }, nil)

    // Exchange updates — order doesn't matter.
    yjs.ApplyUpdate(docA, updB, "remote")
    yjs.ApplyUpdate(docB, updA, "remote")

    // Both peers converge to the same string.
    fmt.Println(textA.String()) // e.g. "WorldHello" or "HelloWorld" — deterministic
    fmt.Println(textB.String()) // same as textA
}
```

## Shared types

### Text

```go
text := doc.GetText("name")
doc.Transact(func() {
    text.Insert(0, "hello")
    text.Delete(0, 2) // removes "he"
}, nil)
fmt.Println(text.String()) // "llo"
```

### Map

```go
m := doc.GetMap("meta")
doc.Transact(func() {
    m.Set("author", "Alice")
    m.Set("version", int64(3))
}, nil)
v, ok := m.Get("author") // "Alice", true
m.Delete("version")
keys := m.Keys() // ["author"]
```

### Array

```go
a := doc.GetArray("items")
doc.Transact(func() {
    a.Push("one", "two", "three")
}, nil)
a.Insert(1, "one-and-a-half")
a.Delete(0, 1)
fmt.Println(a.ToSlice()) // ["one-and-a-half", "two", "three"]
```

### Awareness

Awareness carries ephemeral per-peer state (cursor positions, user info) that is not persisted.

```go
aw := yjs.NewAwareness(doc)
defer aw.Destroy()

aw.SetLocalState(map[string]interface{}{
    "user": "Alice",
    "cursor": int64(42),
})

aw.OnChange(func(added, updated, removed []uint64, origin interface{}) {
    fmt.Printf("peers changed: +%v ~%v -%v\n", added, updated, removed)
})

// Encode and send to a peer.
encoded := aw.EncodeAwarenessUpdate()
// On the receiving end:
aw2.ApplyUpdate(encoded, "remote")
```

## WebSocket transport

`yjs-go/transport` implements the y-websocket protocol so Go clients can join rooms hosted by any y-websocket-compatible relay.

```go
import "github.com/CivNode/yjs-go/transport"

doc := yjs.NewDoc()
conn, err := transport.Connect(ctx, doc, "ws://localhost:1234", "my-room", "")
if err != nil {
    log.Fatal(err)
}
defer conn.Close()

// Local mutations are broadcast automatically.
doc.Transact(func() { doc.GetText("code").Insert(0, "hello") }, nil)

// Subscribe to remote updates.
conn.OnUpdate(func(update []byte) {
    fmt.Printf("remote update: %d bytes\n", len(update))
})
```

The `transport.NewRelay()` type is an `http.Handler` you can use in tests or embed in your own server:

```go
relay := transport.NewRelay()
http.Handle("/", relay)
http.ListenAndServe(":1234", nil)
```

## Sync protocol

`yjs-go/protocol` has helpers for the y-protocols sync message framing:

```go
import "github.com/CivNode/yjs-go/protocol"

// Encode
step1 := protocol.EncodeSyncStep1(stateVector)
step2 := protocol.EncodeSyncStep2(update)
upd   := protocol.EncodeUpdate(update)

// Decode
msg, err := protocol.ReadSyncMessage(r)
// msg.Type: 0=step1, 1=step2, 2=update
// msg.Payload: stateVector or update bytes
```

## Encoding helpers

```go
sv, _  := yjs.EncodeStateVector(doc)             // compact state vector
upd, _ := yjs.EncodeStateAsUpdate(doc, remoteSV) // diff since remoteSV
yjs.ApplyUpdate(doc, upd, "remote")               // integrate an update

// Snapshot the full document state and restore it on a new Doc.
snap, _ := doc.Snapshot()
doc2, _ := yjs.RestoreDoc(snap)
```

## Benchmarks

On an AMD Ryzen 9 3950X (amd64, Go 1.22):

```
BenchmarkTextInsert1k           870    1.29 ms/op    456 KB/op   10070 allocs/op
BenchmarkTextInsert10k            9  113.6 ms/op    4908 KB/op  100101 allocs/op
BenchmarkTextInsert100k           1  14.35 s/op    54896 KB/op 1000189 allocs/op
BenchmarkTextAppend1k           416    2.87 ms/op   1002 KB/op   25017 allocs/op
BenchmarkMapUpdate1k            798    1.50 ms/op   1153 KB/op   30529 allocs/op
BenchmarkSnapshotSize/100     25305   47.36 µs/op     46 KB/op    1057 allocs/op
BenchmarkSnapshotSize/1000      931    1.38 ms/op    446 KB/op   10073 allocs/op
BenchmarkSnapshotSize/10000       9  116.4 ms/op    4907 KB/op  100104 allocs/op
BenchmarkApplyUpdate           2845  421.4 µs/op     354 KB/op   11048 allocs/op
BenchmarkEncodeStateVector  4612528  259.6 ns/op      160 B/op       5 allocs/op
```

Run them yourself:

```
go test -bench=. -benchmem ./...
```

## Interop testing

The `transport` package includes tests that connect to a real Node.js y-websocket server. They are skipped by default:

```
cd testdata/interop && npm install
YJS_GO_INTEROP=1 go test ./transport/... -run TestInterop -v
```

## Contributing

Issues and pull requests welcome. Please include tests for any change. Run `go test ./...` before submitting.

## License

MIT. See [LICENSE](LICENSE).
