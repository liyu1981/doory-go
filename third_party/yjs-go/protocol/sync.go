package protocol

import (
	"bytes"
	"fmt"
	"io"
)

// y-protocols sync message types.
const (
	MessageYjsSyncStep1 = uint64(0)
	MessageYjsSyncStep2 = uint64(1)
	MessageYjsUpdate    = uint64(2)
)

// WriteSyncStep1 writes a sync step 1 message: the sender's state vector.
// Format: varuint(0), varBytes(stateVector)
func WriteSyncStep1(w io.Writer, stateVector []byte) error {
	if err := WriteVarUint(w, MessageYjsSyncStep1); err != nil {
		return err
	}
	return WriteVarBytes(w, stateVector)
}

// WriteSyncStep2 writes a sync step 2 message: the update the receiver needs.
// Format: varuint(1), varBytes(update)
func WriteSyncStep2(w io.Writer, update []byte) error {
	if err := WriteVarUint(w, MessageYjsSyncStep2); err != nil {
		return err
	}
	return WriteVarBytes(w, update)
}

// WriteUpdate writes an update message.
// Format: varuint(2), varBytes(update)
func WriteUpdate(w io.Writer, update []byte) error {
	if err := WriteVarUint(w, MessageYjsUpdate); err != nil {
		return err
	}
	return WriteVarBytes(w, update)
}

// SyncMessage is a parsed y-protocols sync message.
type SyncMessage struct {
	Type uint64
	// For step1: StateVector bytes.
	// For step2 / update: Update bytes.
	Payload []byte
}

// ReadSyncMessage reads one sync message from r.
func ReadSyncMessage(r io.Reader) (*SyncMessage, error) {
	msgType, err := ReadVarUint(r)
	if err != nil {
		return nil, fmt.Errorf("sync message type: %w", err)
	}
	payload, err := ReadVarBytes(r)
	if err != nil {
		return nil, fmt.Errorf("sync message payload (type=%d): %w", msgType, err)
	}
	return &SyncMessage{Type: msgType, Payload: payload}, nil
}

// EncodeSyncStep1 returns the binary encoding of a sync step 1 message.
func EncodeSyncStep1(stateVector []byte) []byte {
	var buf bytes.Buffer
	_ = WriteSyncStep1(&buf, stateVector)
	return buf.Bytes()
}

// EncodeSyncStep2 returns the binary encoding of a sync step 2 message.
func EncodeSyncStep2(update []byte) []byte {
	var buf bytes.Buffer
	_ = WriteSyncStep2(&buf, update)
	return buf.Bytes()
}

// EncodeUpdate returns the binary encoding of an update message.
func EncodeUpdate(update []byte) []byte {
	var buf bytes.Buffer
	_ = WriteUpdate(&buf, update)
	return buf.Bytes()
}
