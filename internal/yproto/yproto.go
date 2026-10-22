// Package yproto ports apps/collab/protocol.py: the Yjs wire protocol
// framing (y-protocols/y-websocket byte for byte), state vectors, awareness
// updates and the bounded update scanner that gates untrusted updates before
// they reach the engine.
//
// Every message from a client is parsed here: the engine is native code, and
// a crafted update (deep nesting, references to history the document does not
// have) can crash the process or grow memory without bound. ProtocolError
// closes the connection with 4400 (apps/collab consumers).
package yproto

import (
	"encoding/json"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// Message types (MESSAGE_SYNC etc. in protocol.py).
const (
	MessageSync           = 0
	MessageAwareness      = 1
	MessageAuth           = 2
	MessageQueryAwareness = 3
)

// Sync message kinds.
const (
	SyncStep1  = 0
	SyncStep2  = 1
	SyncUpdate = 2
)

const (
	maxVarUintBytes       = 10
	MaxStateVectorEntries = 100_000
	MaxAnyDepth           = 32
)

// AllowedTypeRefs: Y.Array, Y.Map, Y.Text; the XML types are not used by Doory.
var AllowedTypeRefs = map[uint64]bool{0: true, 1: true, 2: true}

// Content references of an update item (lib0 / yjs structs/*).
const (
	refDeleted = 1
	refJSON    = 2
	refBinary  = 3
	refString  = 4
	refEmbed   = 5
	refFormat  = 6
	refType    = 7
	refAny     = 8
	refDoc     = 9
	refSkip    = 10
)

// lib0 `any` tags.
const (
	anyArray  = 117
	anyObject = 118
	anyString = 119
	anyBytes  = 116
	anyVarint = 125
)

var anyFixed = map[byte]int{127: 0, 126: 0, 124: 4, 123: 8, 122: 8, 121: 0, 120: 0}

// ProtocolError is protocol.ProtocolError: a frame or update is malformed or
// breaks a rule (closes the connection with 4400).
type ProtocolError struct{ Message string }

func (e *ProtocolError) Error() string { return e.Message }

func perrf(format string, args ...any) error {
	return &ProtocolError{Message: fmt.Sprintf(format, args...)}
}

// ----------------------------------------------------------------- reader --

// Reader is protocol.Reader: a bounds-checked cursor over bytes (lib0 decoding).
type Reader struct {
	Data []byte
	Pos  int
}

func NewReader(data []byte) *Reader { return &Reader{Data: data} }

func (r *Reader) Remaining() int { return len(r.Data) - r.Pos }

func (r *Reader) AtEnd() bool { return r.Pos == len(r.Data) }

func (r *Reader) Uint8() byte {
	if r.Pos >= len(r.Data) {
		panic(&ProtocolError{Message: "Unexpected end of data."})
	}
	b := r.Data[r.Pos]
	r.Pos++
	return b
}

func (r *Reader) VarUint() uint64 {
	var result uint64
	var shift uint
	for i := 0; i < maxVarUintBytes; i++ {
		b := r.Uint8()
		result |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return result
		}
		shift += 7
	}
	panic(&ProtocolError{Message: "Integer too long."})
}

func (r *Reader) SkipVarint() {
	for i := 0; i < maxVarUintBytes; i++ {
		if r.Uint8() < 0x80 {
			return
		}
	}
	panic(&ProtocolError{Message: "Integer too long."})
}

func (r *Reader) Take(count int) []byte {
	if count < 0 || count > r.Remaining() {
		panic(&ProtocolError{Message: "Unexpected end of data."})
	}
	chunk := r.Data[r.Pos : r.Pos+count]
	r.Pos += count
	return chunk
}

func (r *Reader) Skip(count int) {
	if count < 0 || count > r.Remaining() {
		panic(&ProtocolError{Message: "Unexpected end of data."})
	}
	r.Pos += count
}

func (r *Reader) VarBytes() []byte { return r.Take(int(r.VarUint())) }

func (r *Reader) VarString() string {
	raw := r.VarBytes()
	if !utf8.Valid(raw) {
		panic(&ProtocolError{Message: "Invalid UTF-8 text."})
	}
	return string(raw)
}

// SkipVarString skips a string and returns its length in UTF-16 code units
// (the clock length of a text item).
func (r *Reader) SkipVarString() int {
	raw := r.VarBytes()
	if !utf8.Valid(raw) {
		panic(&ProtocolError{Message: "Invalid UTF-8 text."})
	}
	return len(utf16.Encode([]rune(string(raw))))
}

// --------------------------------------------------------------- writers ---

// WriteVarUint is protocol.write_varuint.
func WriteVarUint(value uint64) []byte {
	var out []byte
	for value > 0x7f {
		out = append(out, 0x80|byte(value&0x7f))
		value >>= 7
	}
	out = append(out, byte(value))
	return out
}

func WriteVarBytes(data []byte) []byte {
	return append(WriteVarUint(uint64(len(data))), data...)
}

func WriteVarString(text string) []byte { return WriteVarBytes([]byte(text)) }

// ----------------------------------------------------------------- frames --

// Frame is one decoded client frame (protocol.Frame).
type Frame struct {
	Kind    int    // SyncStep1/SyncStep2/SyncUpdate, or -1 for non-sync frames
	Payload []byte // sync payload or awareness update
	Query   bool   // MESSAGE_QUERY_AWARENESS
}

// ParseFrame ports protocol.parse_frame: anything but sync, awareness and
// query-awareness raises ProtocolError.
func ParseFrame(data []byte) (frame Frame, err error) {
	defer func() {
		if p := recover(); p != nil {
			if pe, ok := p.(*ProtocolError); ok {
				frame, err = Frame{}, pe
				return
			}
			panic(p)
		}
	}()
	r := NewReader(data)
	message := r.VarUint()
	if message == MessageSync {
		kind := r.VarUint()
		if kind != SyncStep1 && kind != SyncStep2 && kind != SyncUpdate {
			return Frame{}, &ProtocolError{Message: "Unknown sync message."}
		}
		payload := r.VarBytes()
		if !r.AtEnd() {
			return Frame{}, &ProtocolError{Message: "Trailing bytes after a sync message."}
		}
		return Frame{Kind: int(kind), Payload: payload}, nil
	}
	if message == MessageAwareness {
		payload := r.VarBytes()
		if !r.AtEnd() {
			return Frame{}, &ProtocolError{Message: "Trailing bytes after an awareness message."}
		}
		return Frame{Kind: -1, Payload: payload}, nil
	}
	if message == MessageQueryAwareness {
		if !r.AtEnd() {
			return Frame{}, &ProtocolError{Message: "Trailing bytes after a query message."}
		}
		return Frame{Kind: -1, Query: true}, nil
	}
	return Frame{}, perrf("Unknown message type %d.", message)
}

// SyncFrame is protocol.sync_frame.
func SyncFrame(kind int, payload []byte) []byte {
	out := WriteVarUint(MessageSync)
	out = append(out, WriteVarUint(uint64(kind))...)
	return append(out, WriteVarBytes(payload)...)
}

// AwarenessFrame is protocol.awareness_frame.
func AwarenessFrame(update []byte) []byte {
	out := WriteVarUint(MessageAwareness)
	return append(out, WriteVarBytes(update)...)
}

// PermissionDeniedFrame is protocol.permission_denied_frame.
func PermissionDeniedFrame(reason string) []byte {
	out := WriteVarUint(MessageAuth)
	out = append(out, WriteVarUint(0)...)
	return append(out, WriteVarString(reason)...)
}

// ------------------------------------------------------- state vectors ----

// DecodeStateVector ports protocol.decode_state_vector: client -> next clock.
// `limit` (0: none) bounds the number of clients of a vector a peer sent.
func DecodeStateVector(data []byte, limit uint64) (map[uint64]uint64, error) {
	var vector map[uint64]uint64
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				if pe, ok := p.(*ProtocolError); ok {
					err = pe
					return
				}
				panic(p)
			}
		}()
		r := NewReader(data)
		count := r.VarUint()
		if (limit != 0 && count > limit) || count*2 > uint64(r.Remaining()) {
			return &ProtocolError{Message: "Invalid state vector."}
		}
		vector = map[uint64]uint64{}
		for i := uint64(0); i < count; i++ {
			client := r.VarUint()
			vector[client] = r.VarUint()
		}
		if !r.AtEnd() {
			return &ProtocolError{Message: "Trailing bytes after a state vector."}
		}
		return nil
	}()
	return vector, err
}

// ----------------------------------------------------------- awareness ----

// AwarenessEntry is protocol.AwarenessEntry.
type AwarenessEntry struct {
	ClientID uint64
	Clock    uint64
	State    string // JSON text; "null" removes the client
}

// DecodeAwareness ports protocol.decode_awareness.
func DecodeAwareness(data []byte, maxEntries uint64, maxStateBytes int) (entries []AwarenessEntry, err error) {
	defer func() {
		if p := recover(); p != nil {
			if pe, ok := p.(*ProtocolError); ok {
				entries, err = nil, pe
				return
			}
			panic(p)
		}
	}()
	r := NewReader(data)
	count := r.VarUint()
	if count > maxEntries || count > uint64(r.Remaining()) {
		return nil, &ProtocolError{Message: "Too many awareness states."}
	}
	for i := uint64(0); i < count; i++ {
		clientID := r.VarUint()
		clock := r.VarUint()
		raw := r.VarBytes()
		if len(raw) > maxStateBytes {
			return nil, &ProtocolError{Message: "Awareness state too large."}
		}
		if !utf8.Valid(raw) {
			return nil, &ProtocolError{Message: "Invalid UTF-8 text."}
		}
		entries = append(entries, AwarenessEntry{ClientID: clientID, Clock: clock, State: string(raw)})
	}
	if !r.AtEnd() {
		return nil, &ProtocolError{Message: "Trailing bytes after an awareness update."}
	}
	return entries, nil
}

// EncodeAwareness ports protocol.encode_awareness.
func EncodeAwareness(entries []AwarenessEntry) []byte {
	out := WriteVarUint(uint64(len(entries)))
	for _, e := range entries {
		out = append(out, WriteVarUint(e.ClientID)...)
		out = append(out, WriteVarUint(e.Clock)...)
		out = append(out, WriteVarString(e.State)...)
	}
	return out
}

// ParseAwarenessState ports protocol.parse_awareness_state: the state object
// of an awareness entry, or nil for a removal.
func ParseAwarenessState(text string) (map[string]any, error) {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil, &ProtocolError{Message: "Awareness state is not JSON."}
	}
	if value == nil {
		return nil, nil
	}
	if obj, ok := value.(map[string]any); ok {
		return obj, nil
	}
	return nil, &ProtocolError{Message: "Awareness state must be an object or null."}
}
