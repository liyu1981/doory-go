// Conformance for ARRAY INDEX operations over decoded multi-value batches.
//
// The decoder materializes one Yjs ContentAny insert batch as one item holding
// N values (PATCHES.md item 10), so `Array.Insert`/`Array.Delete` must count
// VALUES and split a batch when the position falls inside it — otherwise a
// server-side write (schema rewrite, repair) lands on the wrong element.
//
// Fixtures: test/parity/probes/0015-array-index (pycrdt/yrs). Each record ships
// the base snapshot, the operation and the oracle's values/Len/state vector.
// The engine must reproduce all three after performing the SAME operation.
package crdt

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	yjsgo "github.com/CivNode/yjs-go"

	"doory-go/internal/yproto"
)

type arrayIndexCase struct {
	Name        string          `json:"name"`
	BaseHistory [][]interface{} `json:"base_history"`
	Op          struct {
		Kind   string        `json:"kind"`
		Index  uint64        `json:"index"`
		Length uint64        `json:"length"`
		Values []interface{} `json:"values"`
	} `json:"op"`
	Base     string        `json:"base"`
	SVBefore string        `json:"sv_before"`
	Values   []interface{} `json:"values"`
	Len      uint64        `json:"len"`
	SVAfter  string        `json:"sv_after"`
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", ".."))
	return filepath.Join(root, "test", "parity", "probes", "0015-array-index", name)
}

func TestArrayIndexOpsOverBatches(t *testing.T) {
	raw, err := os.ReadFile(fixturePath(t, "fixtures.json"))
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}
	var cases []arrayIndexCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parsing fixtures: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no fixtures")
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			base, err := hex.DecodeString(tc.Base)
			if err != nil {
				t.Fatalf("base hex: %v", err)
			}
			doc := NewDoc()
			if err := ApplyUpdate(doc, base); err != nil {
				t.Fatalf("apply base: %v", err)
			}
			arr := doc.GetArray("pageOrder")

			// The base must already reproduce the oracle's starting length.
			if got := arr.Len(); got != uint64(countValues(tc.BaseHistory)) {
				t.Fatalf("base Len = %d, want %d", got, countValues(tc.BaseHistory))
			}
			before, err := StateVector(doc)
			if err != nil {
				t.Fatalf("sv before: %v", err)
			}
			if got, want := hex.EncodeToString(before), tc.SVBefore; got != want {
				t.Fatalf("sv before = %s, want %s", got, want)
			}
			beforeClocks, err := yproto.DecodeStateVector(before, 64)
			if err != nil {
				t.Fatalf("decode sv before: %v", err)
			}

			doc.Transact(func() {
				switch tc.Op.Kind {
				case "insert":
					arr.Insert(tc.Op.Index, tc.Op.Values...)
				case "delete":
					arr.Delete(tc.Op.Index, tc.Op.Length)
				default:
					t.Fatalf("unknown op %q", tc.Op.Kind)
				}
			}, nil)

			if got := arr.ToSlice(); !reflect.DeepEqual(got, tc.Values) {
				t.Fatalf("values = %v, want %v", got, tc.Values)
			}
			if got := arr.Len(); got != tc.Len {
				t.Fatalf("Len = %d, want %d", got, tc.Len)
			}
			// Clock accounting (the oracle's SV bytes cannot match: its insert
			// came from the doc's own client, ours from the replica's client).
			// Splitting an existing batch must NOT consume clock, and an insert
			// must add exactly one clock tick per value.
			after, err := StateVector(doc)
			if err != nil {
				t.Fatalf("sv after: %v", err)
			}
			afterClocks, err := yproto.DecodeStateVector(after, 64)
			if err != nil {
				t.Fatalf("decode sv after: %v", err)
			}
			for client, clock := range beforeClocks {
				if afterClocks[client] != clock {
					t.Fatalf("client %d clock moved %d -> %d (splits must not consume clock)",
						client, clock, afterClocks[client])
				}
			}
			var added int64
			for client, clock := range afterClocks {
				added += int64(clock) - int64(beforeClocks[client])
			}
			wantAdded := int64(0)
			if tc.Op.Kind == "insert" {
				wantAdded = int64(len(tc.Op.Values))
			}
			if added != wantAdded {
				t.Fatalf("clock ticks added = %d, want %d", added, wantAdded)
			}
			// The document must also survive a snapshot round trip.
			snap, err := Snapshot(doc)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			fresh := NewDoc()
			if err := ApplyUpdate(fresh, snap); err != nil {
				t.Fatalf("apply snapshot: %v", err)
			}
			if got := fresh.GetArray("pageOrder").ToSlice(); !reflect.DeepEqual(got, tc.Values) {
				t.Fatalf("roundtrip values = %v, want %v", got, tc.Values)
			}
		})
	}
}

func countValues(history [][]interface{}) int {
	n := 0
	for _, batch := range history {
		n += len(batch)
	}
	return n
}

// TestGetAndLenCountValues pins the read side over a decoded batch (a batch is
// ONE item with N values) independently of the fixtures.
func TestGetAndLenCountValues(t *testing.T) {
	doc := NewDoc()
	arr := doc.GetArray("pageOrder")
	doc.Transact(func() { arr.Push("a") }, nil)
	// Round-trip so the value comes back as a decoder batch is not guaranteed
	// for Push; Get/Len must agree with ToSlice either way.
	doc.Transact(func() { arr.Push("b", "c") }, nil)
	if got, want := arr.Len(), uint64(3); got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
	if v, ok := arr.Get(2); !ok || v != "c" {
		t.Fatalf("Get(2) = %v, %v", v, ok)
	}
	if _, ok := arr.Get(3); ok {
		t.Fatal("Get(3) should miss")
	}
	doc.Transact(func() { arr.Insert(1, "Z") }, nil)
	if got, want := arr.ToSlice(), []interface{}{"a", "Z", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after insert = %v, want %v", got, want)
	}
	doc.Transact(func() { arr.Delete(1, 2) }, nil)
	if got, want := arr.ToSlice(), []interface{}{"a", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after delete = %v, want %v", got, want)
	}
}

var _ = yjsgo.NewDoc
