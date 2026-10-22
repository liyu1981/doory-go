package protocol_test

import (
	"bytes"
	"testing"

	"github.com/CivNode/yjs-go/protocol"
)

func TestSyncStep1RoundTrip(t *testing.T) {
	sv := []byte{0x01, 0x02, 0x03}
	msg := protocol.EncodeSyncStep1(sv)

	r := bytes.NewReader(msg)
	parsed, err := protocol.ReadSyncMessage(r)
	if err != nil {
		t.Fatalf("ReadSyncMessage: %v", err)
	}
	if parsed.Type != protocol.MessageYjsSyncStep1 {
		t.Errorf("want type 0 got %d", parsed.Type)
	}
	if !bytes.Equal(parsed.Payload, sv) {
		t.Errorf("want %v got %v", sv, parsed.Payload)
	}
}

func TestSyncStep2RoundTrip(t *testing.T) {
	update := []byte{0xAB, 0xCD}
	msg := protocol.EncodeSyncStep2(update)

	r := bytes.NewReader(msg)
	parsed, err := protocol.ReadSyncMessage(r)
	if err != nil {
		t.Fatalf("ReadSyncMessage: %v", err)
	}
	if parsed.Type != protocol.MessageYjsSyncStep2 {
		t.Errorf("want type 1 got %d", parsed.Type)
	}
	if !bytes.Equal(parsed.Payload, update) {
		t.Errorf("want %v got %v", update, parsed.Payload)
	}
}

func TestSyncUpdateRoundTrip(t *testing.T) {
	update := []byte{0xFF, 0x00, 0x42}
	msg := protocol.EncodeUpdate(update)

	r := bytes.NewReader(msg)
	parsed, err := protocol.ReadSyncMessage(r)
	if err != nil {
		t.Fatalf("ReadSyncMessage: %v", err)
	}
	if parsed.Type != protocol.MessageYjsUpdate {
		t.Errorf("want type 2 got %d", parsed.Type)
	}
	if !bytes.Equal(parsed.Payload, update) {
		t.Errorf("want %v got %v", update, parsed.Payload)
	}
}

func TestSyncEmptyPayload(t *testing.T) {
	msg := protocol.EncodeSyncStep1([]byte{})
	r := bytes.NewReader(msg)
	parsed, err := protocol.ReadSyncMessage(r)
	if err != nil {
		t.Fatalf("ReadSyncMessage: %v", err)
	}
	if len(parsed.Payload) != 0 {
		t.Errorf("want empty payload, got %v", parsed.Payload)
	}
}
