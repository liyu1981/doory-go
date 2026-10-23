// Room periodic work and saving (apps/collab/room.py _tick.._teardown):
// idle/awareness expiry, access re-checks, replacement/resync, flush,
// repair, compaction and the fail/shutdown paths.

package doory

import (
	"fmt"
	"time"

	"doory-go/internal/crdt"
	"doory-go/internal/yproto"
	"doory-go/internal/yschema"
)

// closeForReason is room.CLOSE_FOR_REASON.
func closeForReason(reason string) int {
	if code, ok := collabCloseCodes[reason]; ok {
		return code
	}
	return 4403
}

// remoteMessage ports Room._remote — the cross-process layer replaced by the
// in-process registry (only self-origin events can arrive here in a single
// process; the saving_from_room flag keeps room saves from echoing).
func (room *collabRoom) remoteMessage(msg map[string]any) {
	kind, _ := msg["type"].(string)
	switch kind {
	case "collab.title":
		if title, ok := msg["title"].(string); ok {
			room.title = title
			room.sendTextAll(map[string]any{"type": "title", "title": room.title}, false)
		}
	case "collab.recheck":
		room.recheck()
	case "collab.replaced":
		if rev, ok := msg["revision"].(float64); ok {
			room.revision = int64(rev)
		}
		room.sendTextAll(map[string]any{
			"type": "replaced", "reason": "version_restored", "revision": room.revision,
		}, false)
	case "collab.update":
		// Another process's relayed update (unreachable single-process).
	}
}

// tick ports Room._tick.
func (room *collabRoom) tick() {
	now := time.Now()
	idle := time.Duration(envFloat("REALTIME_IDLE_TIMEOUT_SECONDS", 120) * float64(time.Second))
	for _, m := range snapshotMembers(room) {
		if now.Sub(m.lastSeen) > idle {
			room.kick(m, 4408, "idle")
		}
	}
	for client, slot := range room.awareness {
		if slot.Owner == "" && now.Sub(slot.Seen) > collabAwarenessExpiry {
			delete(room.awareness, client)
		}
	}
	recheckEvery := time.Duration(envFloat("REALTIME_RECHECK_SECONDS", 5) * float64(time.Second))
	if now.Sub(room.lastRecheck) >= recheckEvery {
		room.lastRecheck = now
		room.recheck()
	}
	flushEvery := time.Duration(envFloat("REALTIME_FLUSH_INTERVAL_SECONDS", 10) * float64(time.Second))
	if room.dirty && now.Sub(room.lastFlush) >= flushEvery {
		room.lastFlush = now
		room.startFlush()
	}
}

// recheck ports Room._recheck: close members whose access is gone, follow
// title changes, then replacement/resync.
func (room *collabRoom) recheck() {
	checks := []CollabMemberCheck{}
	for _, m := range snapshotMembers(room) {
		if m.closing {
			continue
		}
		checks = append(checks, CollabMemberCheck{
			MemberID: m.ID, UserID: m.UserID, Role: m.Role,
			LinkID: m.LinkID, SessionKey: m.SessionKey,
		})
	}
	report := room.app.CollabCheckAccess(room.designID, checks)
	for memberID, reason := range report.Closed {
		if m, ok := room.members[memberID]; ok {
			room.registry.addCounter("access_" + reason)
			room.kick(m, closeForReason(reason), reason)
		}
	}
	if report.Title != "" && report.Title != room.title {
		room.title = report.Title
		room.sendTextAll(map[string]any{"type": "title", "title": room.title}, false)
	}
	if len(room.members) == 0 {
		return
	}
	room.maybeReplace()
	room.maybeResync()
}

// maybeReplace ports Room._maybe_replace: version restore rolled into the
// room's own history.
func (room *collabRoom) maybeReplace() {
	ticket := room.app.CollabReadReplacement(room.designID)
	if ticket == nil {
		return
	}
	var document map[string]any
	if parsed, err := parseJSONValue([]byte(ticket.Document)); err == nil {
		document, _ = parsed.(map[string]any)
	}
	if document == nil {
		return
	}
	delta, err := makeReplacement(room.doc, document)
	if err != nil {
		return
	}
	version, ok, cerr := room.app.CollabCommitReplacement(ticket, delta)
	if cerr != nil || !ok {
		return
	}
	if err := crdt.ApplyUpdate(room.doc, delta); err != nil {
		return
	}
	opened, oerr := newOpenedCollabDoc(room.doc)
	if oerr != nil {
		return
	}
	room.sv, room.size = opened.sv, opened.size
	room.versions.add(version)
	room.dirty = false
	room.revision = ticket.Revision
	room.registry.addCounter("replaced")
	room.sendBytesOthers(nil, yproto.SyncFrame(yproto.SyncUpdate, delta))
	room.sendTextAll(map[string]any{
		"type": "replaced", "reason": "version_restored", "revision": room.revision,
	}, false)
}

func makeReplacement(doc *crdt.Doc, document map[string]any) ([]byte, error) {
	before, err := crdt.StateVector(doc)
	if err != nil {
		return nil, err
	}
	var werr error
	doc.Transact(func() {
		werr = yschema.WriteDocument(doc, document)
	}, nil)
	if werr != nil {
		return nil, werr
	}
	return crdt.StateAsUpdate(doc, before)
}

// maybeResync ports Room._maybe_resync.
func (room *collabRoom) maybeResync() {
	stored, ok := room.app.CollabCurrentVersion(room.designID)
	if ok && stored > room.versions.contiguous {
		room.behind++
		if room.behind >= 2 {
			room.behind = 0
			room.resync()
		}
	} else {
		room.behind = 0
	}
}

// resync ports Room._resync: re-read the stored state, merge it in and relay
// the difference (a notify was lost).
func (room *collabRoom) resync() {
	snapshot, updates, ok := room.app.CollabLoadCurrent(room.designID)
	version, vok := room.app.CollabCurrentVersion(room.designID)
	if !ok || !vok {
		return
	}
	before, err := crdt.StateVector(room.doc)
	if err != nil {
		return
	}
	if err := crdt.ApplyUpdate(room.doc, snapshot); err != nil {
		room.registry.addCounter("resyncs_failed")
		return
	}
	for _, u := range updates {
		_ = crdt.ApplyUpdate(room.doc, u)
	}
	delta, derr := crdt.StateAsUpdate(room.doc, before)
	if derr != nil {
		return
	}
	opened, oerr := newOpenedCollabDoc(room.doc)
	if oerr != nil {
		return
	}
	room.sv, room.size = opened.sv, opened.size
	room.versions.reset(version)
	room.registry.addCounter("resyncs")
	if len(delta) > 0 {
		room.sendBytesOthers(nil, yproto.SyncFrame(yproto.SyncUpdate, delta))
	}
	room.dirty = true
}

// compact ports Room._compact.
func (room *collabRoom) compact() {
	source := room.app.CollabReadForCompaction(room.designID)
	if source == nil {
		room.rowsSinceCompact = 0
		return
	}
	merged, err := crdt.ApplyToFresh(source.Snapshot, source.Updates...)
	if err != nil {
		room.rowsSinceCompact = 0
		return
	}
	if ok, _ := room.app.CollabApplyCompaction(room.designID, source, merged); ok {
		room.registry.addCounter("compactions")
	}
	room.rowsSinceCompact = 0
}

// ------------------------------------------------------------ flush paths ---

// flushNow ports Room._flush_now (a writer asked to save now).
func (room *collabRoom) flushNow() {
	if room.flushBusy {
		return // the running flush reports through flush_done
	}
	if room.dirty {
		room.startFlush()
		return
	}
	room.sendTextAll(map[string]any{
		"type": "saved", "revision": room.revision, "at": nowISO(),
	}, false)
}

// startFlush ports Room._start_flush: capture state/version, convert and
// persist off the actor, then post the outcome back.
func (room *collabRoom) startFlush() {
	room.flushMu.Lock()
	if room.flushBusy {
		room.flushMu.Unlock()
		return
	}
	room.flushBusy = true
	room.flushMu.Unlock()

	state, err := crdt.Snapshot(room.doc)
	version, editor := room.versions.latest, room.lastEditorID
	room.dirty = false
	if err != nil {
		room.flushBusy = false
		room.dirty = true
		room.registry.addCounter("flush_failed")
		return
	}
	go func() {
		out := map[string]any{"ok": false}
		conversion := Convert(state)
		if conversion.Document == nil {
			room.enqueue(roomItem{kind: "flush_done", message: map[string]any{
				"invalid": true, "problem": conversion.Problem,
				"noPages": conversion.Anomalies != nil && conversion.Anomalies.NoPages,
			}})
			return
		}
		result, ferr := room.app.CollabFlushDocument(room.designID, conversion.Document, version, editor)
		if ferr != nil {
			room.enqueue(roomItem{kind: "flush_done", message: map[string]any{"failed": true}})
			return
		}
		hasAnomalies := conversion.Anomalies != nil && !conversion.Anomalies.Empty()
		room.enqueue(roomItem{kind: "flush_done", message: map[string]any{
			"ok": result.Saved, "revision": result.Revision, "reason": result.Reason,
			"anomalies": hasAnomalies,
		}})
		_ = out
	}()
}

// flushDone ports the outcome handling inside Room._flush.
func (room *collabRoom) flushDone(msg map[string]any) {
	room.flushMu.Lock()
	room.flushBusy = false
	room.flushMu.Unlock()
	if invalid, _ := msg["invalid"].(bool); invalid {
		room.dirty = true
		room.registry.addCounter("flush_invalid")
		problem, _ := msg["problem"].(string)
		if noPages, _ := msg["noPages"].(bool); noPages {
			room.enqueue(roomItem{kind: "repair"})
		}
		if problem != "" && !room.warned[problem] {
			room.warned[problem] = true
			room.sendTextAll(map[string]any{
				"type": "warning", "code": "document_invalid", "message": problem,
			}, true)
		}
		return
	}
	if failed, _ := msg["failed"].(bool); failed {
		room.dirty = true
		room.registry.addCounter("flush_failed")
		return
	}
	room.warned = map[string]bool{}
	if ok, _ := msg["ok"].(bool); ok {
		room.revision = int64(msg["revision"].(float64))
		room.registry.addCounter("flushes")
		room.sendTextAll(map[string]any{
			"type": "saved", "revision": room.revision, "at": nowISO(),
		}, false)
		if anomalies, _ := msg["anomalies"].(bool); anomalies {
			room.enqueue(roomItem{kind: "repair"})
		}
		return
	}
	if reason, _ := msg["reason"].(string); reason == "external_change" {
		room.dirty = true
	}
}

// repair ports Room._repair: delete duplicates/dangling entries and relay the
// delta.
func (room *collabRoom) repair() {
	_, anomalies := yschema.ReadDocument(room.doc)
	if anomalies.Empty() {
		return
	}
	before, err := crdt.StateVector(room.doc)
	if err != nil {
		return
	}
	room.doc.Transact(func() {
		yschema.Repair(room.doc, anomalies)
	}, nil)
	delta, err := crdt.StateAsUpdate(room.doc, before)
	if err != nil || len(delta) == 0 {
		return
	}
	room.registry.addCounter("repairs")
	if opened, oerr := newOpenedCollabDoc(room.doc); oerr == nil {
		room.sv = opened.sv
		room.size = opened.size
	}
	room.persist(nil, delta, "")
}

// fail ports Room._fail: drop the room; connections reconnect and rebuild
// from the database.
func (room *collabRoom) fail(reason string) {
	room.closing = true
	room.closeAll(1011, reason)
	room.teardown(false)
}

// teardown ports Room._teardown.
func (room *collabRoom) teardown(flush bool) {
	room.stopOnce.Do(func() { close(room.stopCh) })
	if flush && room.dirty {
		room.startFlushInline()
	}
	if flush && room.rowsSinceCompact > 0 {
		room.compact()
	}
	room.registry.forget(room)
}

// startFlushInline is the shutdown-time synchronous flush (teardown path).
func (room *collabRoom) startFlushInline() {
	state, err := crdt.Snapshot(room.doc)
	if err != nil {
		return
	}
	conversion := Convert(state)
	if conversion.Document == nil {
		return
	}
	if result, ferr := room.app.CollabFlushDocument(room.designID, conversion.Document,
		room.versions.latest, room.lastEditorID); ferr == nil && result.Saved {
		room.revision = result.Revision
		room.dirty = false
	}
}

// nowISO ports _now_iso: django timezone.now().isoformat() — UTC with
// "+00:00", the fraction omitted when microseconds are zero.
func nowISO() string {
	t := time.Now().UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}

var _ = fmt.Sprintf
