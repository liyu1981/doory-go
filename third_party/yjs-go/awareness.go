package yjs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/CivNode/yjs-go/protocol"
)

// ChangeHandler is called when awareness states change.
// added/updated/removed are slices of clientIDs that changed.
type ChangeHandler func(added, updated, removed []uint64, origin interface{})

// awarenessState holds one client's state and clock.
type awarenessState struct {
	state map[string]interface{}
	clock uint64
}

// Awareness implements the Yjs awareness protocol:
// ephemeral shared state (cursors, user info) keyed by clientID.
// The binary format is: varuint(count), (varuint(clientID), varuint(clock), varstring(JSON(state)))*.
type Awareness struct {
	mu       sync.Mutex
	doc      *Doc
	states   map[uint64]awarenessState
	handlers []ChangeHandler
}

// NewAwareness creates an Awareness instance bound to doc.
func NewAwareness(doc *Doc) *Awareness {
	a := &Awareness{
		doc:    doc,
		states: make(map[uint64]awarenessState),
	}
	// Initialize local state with empty object and clock 0.
	a.states[doc.ClientID()] = awarenessState{state: map[string]interface{}{}, clock: 0}
	return a
}

// Destroy cleans up the awareness instance (removes local state).
func (a *Awareness) Destroy() {
	a.SetLocalState(nil)
}

// SetLocalState sets this client's local state. Pass nil to remove.
func (a *Awareness) SetLocalState(state map[string]interface{}) {
	a.mu.Lock()
	clientID := a.doc.ClientID()
	cur, exists := a.states[clientID]
	var clock uint64
	if exists {
		clock = cur.clock + 1
	}

	var added, updated, removed []uint64
	if state == nil {
		delete(a.states, clientID)
		removed = append(removed, clientID)
	} else {
		a.states[clientID] = awarenessState{state: state, clock: clock}
		if !exists || cur.state == nil {
			added = append(added, clientID)
		} else {
			updated = append(updated, clientID)
		}
	}
	handlers := make([]ChangeHandler, len(a.handlers))
	copy(handlers, a.handlers)
	a.mu.Unlock()

	for _, h := range handlers {
		h(added, updated, removed, "local")
	}
}

// SetLocalStateField sets a single field in the local state map.
func (a *Awareness) SetLocalStateField(field string, value interface{}) {
	a.mu.Lock()
	clientID := a.doc.ClientID()
	cur := a.states[clientID]
	state := make(map[string]interface{})
	for k, v := range cur.state {
		state[k] = v
	}
	state[field] = value
	a.mu.Unlock()
	a.SetLocalState(state)
}

// GetLocalState returns this client's current state, or nil if removed.
func (a *Awareness) GetLocalState() map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.states[a.doc.ClientID()]
	if !ok {
		return nil
	}
	return s.state
}

// GetStates returns a snapshot of all known client states.
func (a *Awareness) GetStates() map[uint64]map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[uint64]map[string]interface{}, len(a.states))
	for id, s := range a.states {
		out[id] = s.state
	}
	return out
}

// OnChange registers a handler for awareness state changes.
func (a *Awareness) OnChange(h ChangeHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers = append(a.handlers, h)
}

// EncodeUpdate encodes the awareness states for the given client IDs as binary.
// Binary format: varuint(count), then (varuint(clientID), varuint(clock), varstring(JSON))*.
func (a *Awareness) EncodeUpdate(clientIDs []uint64) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()

	var buf bytes.Buffer
	valid := make([]uint64, 0, len(clientIDs))
	for _, id := range clientIDs {
		if _, ok := a.states[id]; ok {
			valid = append(valid, id)
		}
	}

	if err := protocol.WriteVarUint(&buf, uint64(len(valid))); err != nil {
		return nil
	}
	for _, id := range valid {
		s := a.states[id]
		stateJSON, err := json.Marshal(s.state)
		if err != nil {
			stateJSON = []byte("null")
		}
		if err := protocol.WriteVarUint(&buf, id); err != nil {
			return nil
		}
		if err := protocol.WriteVarUint(&buf, s.clock); err != nil {
			return nil
		}
		if err := protocol.WriteVarString(&buf, string(stateJSON)); err != nil {
			return nil
		}
	}
	return buf.Bytes()
}

// EncodeAwarenessUpdate encodes all current states (all known clients).
func (a *Awareness) EncodeAwarenessUpdate() []byte {
	a.mu.Lock()
	ids := make([]uint64, 0, len(a.states))
	for id := range a.states {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	return a.EncodeUpdate(ids)
}

// ApplyUpdate applies a remote awareness update binary.
func (a *Awareness) ApplyUpdate(data []byte, origin interface{}) error {
	r := bytes.NewReader(data)
	count, err := protocol.ReadVarUint(r)
	if err != nil {
		return fmt.Errorf("awareness count: %w", err)
	}

	var added, updated, removed []uint64

	a.mu.Lock()
	for i := uint64(0); i < count; i++ {
		clientID, err := protocol.ReadVarUint(r)
		if err != nil {
			a.mu.Unlock()
			return fmt.Errorf("awareness clientID[%d]: %w", i, err)
		}
		clock, err := protocol.ReadVarUint(r)
		if err != nil {
			a.mu.Unlock()
			return fmt.Errorf("awareness clock[%d]: %w", i, err)
		}
		stateStr, err := protocol.ReadVarString(r)
		if err != nil {
			a.mu.Unlock()
			return fmt.Errorf("awareness state[%d]: %w", i, err)
		}

		var state map[string]interface{}
		if stateStr != "null" && stateStr != "" {
			if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
				// Treat malformed state as null.
				state = nil
			}
		}

		cur, exists := a.states[clientID]
		currClock := uint64(0)
		if exists {
			currClock = cur.clock
		}

		// Only apply if clock is newer (or clock equals and state is null = remove).
		if clock > currClock || (clock == currClock && state == nil && exists) {
			// Do not let a remote peer remove the local client's state.
			if clientID == a.doc.ClientID() && a.states[clientID].state != nil {
				continue
			}
			if state == nil {
				delete(a.states, clientID)
				if exists {
					removed = append(removed, clientID)
				}
			} else {
				a.states[clientID] = awarenessState{state: state, clock: clock}
				if !exists {
					added = append(added, clientID)
				} else {
					updated = append(updated, clientID)
				}
			}
		}
	}
	handlers := make([]ChangeHandler, len(a.handlers))
	copy(handlers, a.handlers)
	a.mu.Unlock()

	if len(added) > 0 || len(updated) > 0 || len(removed) > 0 {
		for _, h := range handlers {
			h(added, updated, removed, origin)
		}
	}
	return nil
}
