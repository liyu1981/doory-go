// Room processing: the inbox batch loop, welcome/frame handling, the update
// commit path and awareness (apps/collab/room.py _process.._announce_gone).

// Ports backend/apps/collab/room.py
package doory

import (
	"time"

	"doory-go/internal/crdt"
	"doory-go/internal/yproto"
)

// process ports Room._process: handle a batch in order; consecutive writer
// updates share one apply and one database write. Returns true to stop.
func (room *collabRoom) process(items []roomItem) bool {
	var pending []pendingUpdate
	for _, item := range items {
		// Only SYNC frames (step2/update) are batched as document updates —
		// upstream's guard is `isinstance(payload, protocol.SyncFrame) and
		// payload.kind != SYNC_STEP1`. An awareness frame has no sync kind
		// (Go: Frame.Kind == -1): treating it as an update applied awareness
		// bytes to the engine and skipped the presence relay (scenario
		// 21-collab-peers caught it: the peer received nothing).
		if item.kind == "frame" && item.frame != nil &&
			item.frame.Kind >= yproto.SyncStep1 && item.frame.Kind <= yproto.SyncUpdate &&
			item.frame.Kind != yproto.SyncStep1 {
			member := item.member
			if member == nil || member.closing {
				continue
			}
			if !member.CanWrite {
				room.denyWrite(member)
				continue
			}
			pending = append(pending, pendingUpdate{member: member, data: item.frame.Payload})
			continue
		}
		if len(pending) > 0 {
			room.commit(pending)
			pending = nil
		}
		if room.dispatch(item) {
			return true
		}
	}
	if len(pending) > 0 {
		room.commit(pending)
	}
	return false
}

type pendingUpdate struct {
	member *CollabMember
	data   []byte
}

func (room *collabRoom) dispatch(item roomItem) bool {
	switch item.kind {
	case "welcome":
		if item.member != nil {
			room.welcome(item.member)
		}
	case "frame":
		if item.member != nil && !item.member.closing {
			room.handleFrame(item.member, item.frame)
		}
	case "awareness_gone":
		if len(item.payload) >= 16 {
			room.announceGone(beUint64(item.payload[:8]), beUint64(item.payload[8:16]))
		}
	case "member_gone":
		if item.member != nil {
			room.leave(item.member)
		}
	case "remote":
		room.remoteMessage(item.message)
	case "tick":
		room.tick()
	case "flush":
		room.flushNow()
	case "flush_done":
		room.flushDone(item.message)
	case "repair":
		room.repair()
	case "shutdown":
		room.teardown(true)
		return true
	}
	return false
}

func beUint64(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// denyWrite ports Room._deny_write: read-only connections are told once.
func (room *collabRoom) denyWrite(member *CollabMember) {
	room.registry.addCounter("dropped_read_only")
	if !member.notifiedReadOnly {
		member.notifiedReadOnly = true
		room.sendBytes(member, yproto.PermissionDeniedFrame("read_only"))
	}
}

// welcome ports Room._welcome: sync step1 (our state vector) + awareness.
func (room *collabRoom) welcome(member *CollabMember) {
	if member.closing {
		return
	}
	vector, err := crdt.StateVector(room.doc)
	if err != nil {
		room.fail("internal_error")
		return
	}
	room.sendBytes(member, yproto.SyncFrame(yproto.SyncStep1, vector))
	if entries := room.awarenessEntries(); len(entries) > 0 {
		room.sendBytes(member, yproto.AwarenessFrame(yproto.EncodeAwareness(entries)))
	}
}

// handleFrame ports Room._frame: only step1 reaches here (updates are
// batched by process).
func (room *collabRoom) handleFrame(member *CollabMember, frame *yproto.Frame) {
	if frame == nil {
		return
	}
	switch {
	case frame.Kind >= yproto.SyncStep1 && frame.Kind <= yproto.SyncUpdate:
		if frame.Kind != yproto.SyncStep1 {
			// Step2/update frames are batched before dispatch; anything else
			// should not reach here.
			return
		}
		if _, derr := yproto.DecodeStateVector(frame.Payload, yproto.MaxStateVectorEntries); derr != nil {
			room.kick(member, 4400, "bad_frame")
			return
		}
		diff, serr := crdt.StateAsUpdate(room.doc, frame.Payload)
		if serr != nil {
			room.kick(member, 4400, "bad_frame")
			return
		}
		room.sendBytes(member, yproto.SyncFrame(yproto.SyncStep2, diff))
	case frame.Query:
		if entries := room.awarenessEntries(); len(entries) > 0 {
			room.sendBytes(member, yproto.AwarenessFrame(yproto.EncodeAwareness(entries)))
		}
	default:
		// Awareness update (Kind -1 with payload).
		if frame.Payload != nil {
			room.clientAwareness(member, frame.Payload)
		}
	}
}

// commit ports Room._commit: vet + apply one batch, persist, relay.
func (room *collabRoom) commit(pending []pendingUpdate) {
	updates := make([][]byte, 0, len(pending))
	introduced := make([]map[uint64]bool, 0, len(pending))
	for _, p := range pending {
		updates = append(updates, p.data)
		introduced = append(introduced, p.member.introduced)
	}
	result, err := ApplyClientUpdates(room.doc, room.sv, updates, room.size, room.budget, introduced)
	if err != nil {
		room.fail("internal_error")
		return
	}
	room.sv, room.size = result.SV, result.Size
	var applied []pendingUpdate
	for i, p := range pending {
		if i >= len(result.Outcomes) {
			break
		}
		outcome := result.Outcomes[i]
		switch outcome.Kind {
		case "rejected":
			room.registry.addCounter("rejected_" + outcome.Reason)
			if outcome.Reason == "update_too_costly" {
				BlockUser(p.member.UserID)
			}
			code := 4400
			if outcome.Reason == "document_too_large" {
				code = 4413
			}
			room.kick(p.member, code, outcome.Reason)
		case "applied":
			applied = append(applied, p)
		}
	}
	if len(applied) > 0 && result.Merged != nil {
		room.persist(applied, result.Merged, applied[len(applied)-1].member.UserID)
	}
	if result.Poisoned {
		room.fail("update_too_costly")
		return
	}
	if result.PagesGone {
		room.enqueue(roomItem{kind: "repair"})
	}
}

// persist ports Room._persist: append durably, then relay to the others.
func (room *collabRoom) persist(applied []pendingUpdate, merged []byte, editor string) {
	version, err := room.app.CollabAppendUpdate(room.designID, merged, editor)
	if err != nil {
		room.fail("persist_failed")
		return
	}
	room.versions.add(version)
	room.dirty = true
	room.rowsSinceCompact++
	if editor != "" {
		room.lastEditorID = editor
	}
	for _, p := range applied {
		room.sendBytesOthers(p.member, yproto.SyncFrame(yproto.SyncUpdate, p.data))
	}
	// Cross-process publish (collab.update) is a no-op single-process.
	if room.rowsSinceCompact >= envInt("REALTIME_COMPACT_AFTER_UPDATES", 200) {
		room.compact()
	}
}

func nilStrEditor(editor string) any {
	if editor == "" {
		return nil
	}
	return editor
}

// awarenessEntries ports Room._awareness_entries.
func (room *collabRoom) awarenessEntries() []yproto.AwarenessEntry {
	out := []yproto.AwarenessEntry{}
	for client, slot := range room.awareness {
		out = append(out, yproto.AwarenessEntry{ClientID: client, Clock: slot.Clock, State: slot.State})
	}
	return out
}

// identity ports Room._identity: the server's identity overlay (id, name,
// avatar, palette colour, role).
func identityFor(member *CollabMember, state map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range state {
		out[k] = v
	}
	color, _ := state["color"].(string)
	if color == "" || !collabColorPattern.MatchString(color) {
		color = collabColorFor(member.UserID)
	}
	out["user"] = map[string]any{
		"id": member.UserID, "name": member.Name, "avatarUrl": member.AvatarURL,
		"color": color, "role": member.Role,
	}
	return out
}

// clientAwareness ports Room._client_awareness.
func (room *collabRoom) clientAwareness(member *CollabMember, payload []byte) {
	entries, err := yproto.DecodeAwareness(payload, 64, 2048)
	if err != nil {
		room.kick(member, 4400, "bad_frame")
		return
	}
	now := time.Now()
	accepted := []yproto.AwarenessEntry{}
	for _, entry := range entries {
		slot := room.awareness[entry.ClientID]
		if slot != nil && slot.Owner != member.ID {
			continue
		}
		if slot == nil && !member.controlled[entry.ClientID] && len(member.controlled) >= collabMaxControlledClients {
			continue
		}
		state, serr := yproto.ParseAwarenessState(entry.State)
		if serr != nil {
			room.kick(member, 4400, "bad_frame")
			return
		}
		if state == nil {
			if slot != nil && entry.Clock >= slot.Clock {
				delete(room.awareness, entry.ClientID)
				delete(member.controlled, entry.ClientID)
				accepted = append(accepted, yproto.AwarenessEntry{
					ClientID: entry.ClientID, Clock: entry.Clock, State: "null",
				})
			}
			continue
		}
		if slot != nil && entry.Clock <= slot.Clock {
			continue
		}
		text := roomText(identityFor(member, state))
		if len([]byte(text)) > collabMaxAwarenessBytes {
			room.registry.addCounter("awareness_dropped")
			continue
		}
		member.controlled[entry.ClientID] = true
		room.awareness[entry.ClientID] = &CollabAwarenessSlot{
			Clock: entry.Clock, State: text, Owner: member.ID, Seen: now,
		}
		accepted = append(accepted, yproto.AwarenessEntry{
			ClientID: entry.ClientID, Clock: entry.Clock, State: text,
		})
	}
	if len(accepted) > 0 {
		room.sendBytesOthers(member, yproto.AwarenessFrame(yproto.EncodeAwareness(accepted)))
	}
}

// announceGone ports Room._announce_gone.
func (room *collabRoom) announceGone(client, clock uint64) {
	encoded := yproto.EncodeAwareness([]yproto.AwarenessEntry{
		{ClientID: client, Clock: clock, State: "null"},
	})
	room.sendBytesOthers(nil, yproto.AwarenessFrame(encoded))
}
