package protocol_test

// Tests for readItemContent cases not reachable via EncodeUpdateV1
// (because writeItemContent doesn't implement all content types).
// We construct minimal binary blobs manually.

import (
	"bytes"
	"testing"

	"github.com/CivNode/yjs-go/protocol"
)

// buildMinimalItem constructs a minimal update binary with one item.
// prefix bytes go after the common struct-store header (count=1, clientID, firstClock).
// item header: infoByte, parentInfo=1, parentKey, then content bytes.
func buildMinimalItem(contentRef byte, parentKey string, contentBytes []byte) []byte {
	var buf bytes.Buffer
	// Struct store: 1 client, clientID=1, firstClock=0, numStructs=1.
	protocol.WriteVarUint(&buf, 1)           // num clients
	protocol.WriteVarUint(&buf, 1)           // clientID
	protocol.WriteVarUint(&buf, 0)           // firstClock
	protocol.WriteVarUint(&buf, 1)           // numStructs
	buf.WriteByte(contentRef)                // infoByte (no origins)
	protocol.WriteVarUint(&buf, 1)           // parentInfo = 1 (isYKey)
	protocol.WriteVarString(&buf, parentKey) // parentYKey
	buf.Write(contentBytes)                  // content-specific bytes
	// Delete set: 0 clients.
	protocol.WriteVarUint(&buf, 0)
	return buf.Bytes()
}

// TestDecodeContentEmbed2 tests embed content (contentRef=5) via raw binary.
func TestDecodeContentEmbed2(t *testing.T) {
	var content bytes.Buffer
	protocol.WriteVarString(&content, `{"type":"image","url":"x"}`)
	data := buildMinimalItem(5, "root", content.Bytes())

	decoded, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("DecodeUpdateV1 embed: %v", err)
	}
	if len(decoded.Items) != 1 {
		t.Fatalf("want 1 item got %d", len(decoded.Items))
	}
	it := decoded.Items[0]
	if it.ContentRef != 5 {
		t.Errorf("want contentRef=5 got %d", it.ContentRef)
	}
	if it.ContentData != `{"type":"image","url":"x"}` {
		t.Errorf("unexpected embed content: %v", it.ContentData)
	}
}

// TestDecodeContentFormat tests format content (contentRef=6) via raw binary.
func TestDecodeContentFormat(t *testing.T) {
	var content bytes.Buffer
	protocol.WriteVarString(&content, "bold") // key
	writeAnyBool(&content, true)              // value = true (any tag 120)
	data := buildMinimalItem(6, "root", content.Bytes())

	decoded, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("DecodeUpdateV1 format: %v", err)
	}
	if len(decoded.Items) != 1 {
		t.Fatalf("want 1 item got %d", len(decoded.Items))
	}
	it := decoded.Items[0]
	if it.ContentRef != 6 {
		t.Errorf("want contentRef=6 got %d", it.ContentRef)
	}
	m, ok := it.ContentData.(map[string]interface{})
	if !ok {
		t.Fatalf("want map for format content, got %T", it.ContentData)
	}
	if m["key"] != "bold" {
		t.Errorf("want key='bold' got %v", m["key"])
	}
}

// TestDecodeContentJSONLegacy tests JSON legacy content (contentRef=2).
func TestDecodeContentJSONLegacy(t *testing.T) {
	var content bytes.Buffer
	protocol.WriteVarString(&content, `{"x":1}`)
	data := buildMinimalItem(2, "root", content.Bytes())

	decoded, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("DecodeUpdateV1 JSON legacy: %v", err)
	}
	if len(decoded.Items) != 1 {
		t.Fatalf("want 1 item got %d", len(decoded.Items))
	}
	if decoded.Items[0].ContentRef != 2 {
		t.Errorf("want contentRef=2 got %d", decoded.Items[0].ContentRef)
	}
}

// TestDecodeContentDoc tests ContentDoc (contentRef=9) via raw binary.
func TestDecodeContentDoc(t *testing.T) {
	var content bytes.Buffer
	protocol.WriteVarString(&content, "my-guid") // guid
	writeAnyNull(&content)                       // opts = null
	data := buildMinimalItem(9, "root", content.Bytes())

	decoded, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		t.Fatalf("DecodeUpdateV1 ContentDoc: %v", err)
	}
	if len(decoded.Items) != 1 {
		t.Fatalf("want 1 item got %d", len(decoded.Items))
	}
	it := decoded.Items[0]
	if it.ContentRef != 9 {
		t.Errorf("want contentRef=9 got %d", it.ContentRef)
	}
	m, ok := it.ContentData.(map[string]interface{})
	if !ok {
		t.Fatalf("want map for ContentDoc, got %T", it.ContentData)
	}
	if m["guid"] != "my-guid" {
		t.Errorf("want guid='my-guid' got %v", m["guid"])
	}
}

// TestDecodeUnknownContentRef tests that an unknown content ref returns an error.
func TestDecodeUnknownContentRef(t *testing.T) {
	var content bytes.Buffer
	// Content ref 99 is unknown.
	data := buildMinimalItem(99, "root", content.Bytes())
	_, err := protocol.DecodeUpdateV1(data)
	if err == nil {
		t.Error("expected error for unknown content ref")
	}
}

// TestDecodeParentByID tests an item with a parentID (not parentYKey).
func TestDecodeParentByID(t *testing.T) {
	var buf bytes.Buffer
	// Struct store: 1 client, clientID=1, firstClock=0, numStructs=1.
	protocol.WriteVarUint(&buf, 1) // num clients
	protocol.WriteVarUint(&buf, 1) // clientID
	protocol.WriteVarUint(&buf, 0) // firstClock
	protocol.WriteVarUint(&buf, 1) // numStructs
	buf.WriteByte(4)               // infoByte: contentRef=4 (string), no origins
	protocol.WriteVarUint(&buf, 0) // parentInfo = 0 (parentID, not ykey)
	// parent ID = {client=1, clock=0}
	protocol.WriteVarUint(&buf, 1) // parent clientID
	protocol.WriteVarUint(&buf, 0) // parent clock
	// content: varstring "hi"
	protocol.WriteVarString(&buf, "hi")
	// Delete set: 0 clients.
	protocol.WriteVarUint(&buf, 0)

	decoded, err := protocol.DecodeUpdateV1(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeUpdateV1: %v", err)
	}
	if len(decoded.Items) != 1 {
		t.Fatalf("want 1 item got %d", len(decoded.Items))
	}
	it := decoded.Items[0]
	if it.ParentIsYKey {
		t.Error("expected ParentIsYKey=false")
	}
	if it.ParentID == nil || it.ParentID.Client != 1 {
		t.Errorf("unexpected parentID: %v", it.ParentID)
	}
}

// writeAnyBool writes a boolean any-value (tag 120=true, 121=false).
func writeAnyBool(buf *bytes.Buffer, v bool) {
	if v {
		buf.WriteByte(120)
	} else {
		buf.WriteByte(121)
	}
}

// writeAnyNull writes a null any-value (tag 126).
func writeAnyNull(buf *bytes.Buffer) {
	buf.WriteByte(126)
}
