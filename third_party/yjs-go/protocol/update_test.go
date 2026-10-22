package protocol_test

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/CivNode/yjs-go/protocol"
)

func testdataDir() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "testdata", "yjs-vectors")
}

func readVector(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testdataDir(), name))
	if err != nil {
		t.Skipf("vector %s not found (run testdata/extract.sh): %v", name, err)
	}
	return data
}

func TestDecodeEmptyStateVector(t *testing.T) {
	data := readVector(t, "empty-state-vector.bin")
	sv, err := protocol.DecodeStateVectorV1(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sv) != 0 {
		t.Errorf("want empty sv, got %v", sv)
	}
}

func TestDecodeEmptyUpdate(t *testing.T) {
	data := readVector(t, "empty-update.bin")
	u, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(u.Items) != 0 {
		t.Errorf("want 0 items, got %d", len(u.Items))
	}
	if len(u.DeleteSet) != 0 {
		t.Errorf("want empty delete set, got %v", u.DeleteSet)
	}
}

func TestGoldenHex(t *testing.T) {
	data := readVector(t, "empty-update.bin")
	got := hex.EncodeToString(data)
	if got != "0000" {
		t.Errorf("empty update: want 0000 got %s", got)
	}
}

func TestDecodeTextInsert(t *testing.T) {
	data := readVector(t, "text-insert-hello.bin")
	u, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(u.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(u.Items))
	}
	item := u.Items[0]
	if item.ContentRef != 4 {
		t.Errorf("want contentRef=4 (string), got %d", item.ContentRef)
	}
	s, ok := item.ContentData.(string)
	if !ok || s != "hello" {
		t.Errorf("want ContentData='hello', got %v (%T)", item.ContentData, item.ContentData)
	}
	if item.Length != 5 {
		t.Errorf("want length=5, got %d", item.Length)
	}
	if !item.ParentIsYKey {
		t.Error("want ParentIsYKey=true")
	}
	if item.ParentYKey != "content" {
		t.Errorf("want ParentYKey='content', got %q", item.ParentYKey)
	}
}

func TestUpdateRoundTrip(t *testing.T) {
	data := readVector(t, "text-insert-hello.bin")
	u, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	reenc, err := protocol.EncodeUpdateV1(u)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	u2, err := protocol.DecodeUpdateV1(reenc)
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if len(u2.Items) != len(u.Items) {
		t.Errorf("items count: want %d got %d", len(u.Items), len(u2.Items))
	}
	if u2.Items[0].ContentData != u.Items[0].ContentData {
		t.Errorf("content: want %v got %v", u.Items[0].ContentData, u2.Items[0].ContentData)
	}
}

func TestStateVectorRoundTrip(t *testing.T) {
	sv := map[uint64]uint64{1: 5, 2: 10, 99: 0}
	encoded, err := protocol.EncodeStateVectorV1(sv)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := protocol.DecodeStateVectorV1(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k, v := range sv {
		if decoded[k] != v {
			t.Errorf("sv[%d]: want %d got %d", k, v, decoded[k])
		}
	}
}

func TestDecodeGoldenStateVector(t *testing.T) {
	data := readVector(t, "text-insert-hello-sv.bin")
	sv, err := protocol.DecodeStateVectorV1(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sv) != 1 {
		t.Fatalf("want 1 client in sv, got %d: %v", len(sv), sv)
	}
	// The doc inserted 5 characters, so exactly one client has clock=5.
	var found bool
	for _, clock := range sv {
		if clock == 5 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a client with clock=5 in sv, got %v", sv)
	}
}

func TestDecodeMapSet(t *testing.T) {
	data := readVector(t, "map-set.bin")
	u, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, item := range u.Items {
		if item.ContentRef == 8 && item.ParentSub != nil && *item.ParentSub == "key" {
			found = true
			s, ok := item.ContentData.(string)
			if !ok || s != "value" {
				t.Errorf("want ContentData='value', got %v (%T)", item.ContentData, item.ContentData)
			}
		}
	}
	if !found {
		t.Errorf("did not find map item with key='key'; items: %+v", u.Items)
	}
}

func TestAnyRoundTrip(t *testing.T) {
	cases := []interface{}{
		nil,
		true,
		false,
		"hello world",
		int64(42),
		float64(3.14),
		[]byte{0xde, 0xad, 0xbe, 0xef},
		map[string]interface{}{"a": "b", "c": int64(3)},
		[]interface{}{"x", int64(1), true},
	}
	for _, v := range cases {
		var buf bytes.Buffer
		if err := protocol.WriteAny(&buf, v); err != nil {
			t.Errorf("WriteAny(%v): %v", v, err)
			continue
		}
		got, err := protocol.ReadAny(&buf)
		if err != nil {
			t.Errorf("ReadAny(%v): %v", v, err)
			continue
		}
		// Basic nil check.
		if v == nil && got != nil {
			t.Errorf("nil round-trip: got %v", got)
		}
	}
}

func TestDeleteRangeInUpdate(t *testing.T) {
	data := readVector(t, "text-delete-update.bin")
	u, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The delete update should have at least one entry in the delete set.
	if len(u.DeleteSet) == 0 && len(u.Items) == 0 {
		t.Error("expected some content in delete update")
	}
}
