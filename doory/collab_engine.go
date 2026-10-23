// Collab engine (apps/collab/engine.py): vetting and applying client
// updates, snapshots, compaction and conversion to the design document.
//
// Everything here runs on plain data; the engine itself is
// internal/crdt (yjs-go, conformance-proven against pycrdt), the wire
// scanner is internal/yproto and the document mapping internal/yschema.
package doory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"doory-go/internal/crdt"
	"doory-go/internal/yproto"
	"doory-go/internal/yschema"
)

// engine.py constants.
const (
	smallUpdate = 1024          // bytes
	slack       = 256 * 1024    // bytes the size estimate may exceed the limit by
	// Budgets the writers of one room may use beyond the scanner's per-update checks.
	maxNewClients         = 4
	maxOriginlessPerEntry = 32
	maxTrackedEntries     = 1_000_000
	maxRightOnlyPerUpdate = 64
	maxInsideRefsUpdate   = 256
)

// slowApplySeconds ports REALTIME_SLOW_APPLY_SECONDS (env, default 2.0): an
// update the engine needed longer for poisons the room's state.
func slowApplySeconds() float64 {
	if v, err := strconv.ParseFloat(envStr("REALTIME_SLOW_APPLY_SECONDS", "2.0"), 64); err == nil {
		return v
	}
	return 2.0
}

// maxDocumentBytes ports MAX_DESIGN_DOCUMENT_BYTES (12 MiB).
func maxDocumentBytes() int { return MaxDesignDocumentBytes }

// CollabOutcome is engine.Outcome: what happened to one update.
type CollabOutcome struct {
	Kind   string // "applied" | "noop" | "rejected"
	Reason string
	Detail string
}

// BatchResult is engine.BatchResult.
type BatchResult struct {
	Outcomes []CollabOutcome
	SV       map[uint64]uint64
	Size     int
	Merged   []byte
	Applied  [][]byte
	PagesGone bool
	Poisoned  bool
}

// UpdateBudget is engine.UpdateBudget: what the writers of one room may do to
// its document beyond the scanner checks, keyed like Python's hashed
// (parent, key) entries (memory only; counts are what matter).
type UpdateBudget struct {
	originless map[string]int
}

// NewUpdateBudget ports engine.UpdateBudget().
func NewUpdateBudget() *UpdateBudget {
	return &UpdateBudget{originless: map[string]int{}}
}

// Check ports UpdateBudget.check: why `scan` may not be applied (or "").
func (b *UpdateBudget) Check(scan yproto.UpdateScan, introduced map[uint64]bool) string {
	union := len(introduced) + len(scan.NewClients)
	for id := range introduced {
		if scan.NewClients[id] {
			union--
		}
	}
	if union > maxNewClients {
		return "too many new clients"
	}
	if scan.RightOnly > maxRightOnlyPerUpdate {
		return "too many insertions at the start of an array"
	}
	if scan.InsideRefs > maxInsideRefsUpdate {
		return "too many references into the middle of a long run of values"
	}
	if len(scan.Originless) > 0 {
		fresh := 0
		for key := range scan.Originless {
			if _, ok := b.originless[key]; !ok {
				fresh++
			}
		}
		if len(b.originless)+fresh > maxTrackedEntries {
			return "too many map entries started in this room"
		}
		for key, count := range scan.Originless {
			if b.originless[key]+count > maxOriginlessPerEntry {
				return "too many first writes of one map entry"
			}
		}
	}
	return ""
}

// Commit ports UpdateBudget.commit.
func (b *UpdateBudget) Commit(scan yproto.UpdateScan, introduced map[uint64]bool) {
	for id := range introduced {
		scan.NewClients[id] = true
	}
	for key, count := range scan.Originless {
		b.originless[key] += count
	}
}

// collabRoots is schema.ROOTS as the scanner's allowlist.
func collabRoots() map[string]bool {
	return map[string]bool{
		yschema.RootMeta: true, yschema.RootPageOrder: true, yschema.RootPages: true,
	}
}

// documentSize ports engine.document_size: the JSON size of the document the
// state holds (compact separators, raw UTF-8 and raw <>& like Python's
// json.dumps(ensure_ascii=False)).
func documentSize(doc *crdt.Doc) (int, error) {
	out, _, err := readCollabDocument(doc)
	if err != nil {
		return 0, err
	}
	return len(collabJSON(out)), nil
}

// collabJSON marshals compactly with raw UTF-8 and unescaped HTML (Python
// json.dumps(ensure_ascii=False, separators=(",", ":"))).
func collabJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1] // Encode appends a newline; dumps does not
	}
	return b
}

// readCollabDocument wraps yschema.ReadDocument (the bounded reader turns
// deep nesting into bad_props entries).
func readCollabDocument(doc *crdt.Doc) (map[string]any, *yschema.Anomalies, error) {
	out, anomalies := yschema.ReadDocument(doc)
	for _, path := range anomalies.BadProps {
		if len(path) > 1 {
			return out, anomalies, unrepresentableErr
		}
	}
	return out, anomalies, nil
}

var unrepresentableErr = fmt.Errorf("The property is nested too deeply.")

// ApplyClientUpdates ports engine.apply_client_updates: vet and apply updates
// from a writer, in order. A rejected update leaves the document untouched.
func ApplyClientUpdates(doc *crdt.Doc, sv map[uint64]uint64, updates [][]byte, size int,
	budget *UpdateBudget, introduced []map[uint64]bool,
) (result BatchResult, err error) {
	limit := maxDocumentBytes()
	var outcomes []CollabOutcome
	var applied [][]byte
	poisoned := false
	svBeforeBytes, serr0 := crdt.StateVector(doc)
	if serr0 != nil {
		return result, serr0
	}
	for position, update := range updates {
		if poisoned {
			outcomes = append(outcomes, CollabOutcome{Kind: "noop"})
			continue
		}
		scan, serr := yproto.ScanUpdate(update, sv, collabRoots(), yproto.MaxAnyDepth)
		if serr != nil {
			outcomes = append(outcomes, CollabOutcome{Kind: "rejected", Reason: "bad_frame", Detail: serr.Error()})
			continue
		}
		var owned map[uint64]bool
		if budget != nil && introduced != nil && position < len(introduced) {
			owned = introduced[position]
		}
		if budget != nil && owned != nil {
			if why := budget.Check(scan, owned); why != "" {
				outcomes = append(outcomes, CollabOutcome{Kind: "rejected", Reason: "bad_frame", Detail: why})
				continue
			}
		}
		estimate := size + len(update)
		if estimate > limit && (len(update) >= smallUpdate || estimate > limit+slack) {
			measured, ok := wouldExceed(doc, update, limit)
			if !ok {
				outcomes = append(outcomes, CollabOutcome{Kind: "rejected", Reason: "bad_frame",
					Detail: "The update does not apply cleanly."})
				continue
			}
			exceeds, newSize := measured.exceeds, measured.size
			if exceeds {
				outcomes = append(outcomes, CollabOutcome{Kind: "rejected", Reason: "document_too_large"})
				continue
			}
			estimate = newSize
		}
		started := time.Now()
		if aerr := crdt.ApplyUpdate(doc, update); aerr != nil {
			outcomes = append(outcomes, CollabOutcome{Kind: "rejected", Reason: "bad_frame", Detail: aerr.Error()})
			continue
		}
		if time.Since(started).Seconds() > slowApplySeconds() {
			outcomes = append(outcomes, CollabOutcome{Kind: "rejected", Reason: "update_too_costly"})
			poisoned = true
			continue
		}
		if budget != nil && owned != nil {
			budget.Commit(scan, owned)
		}
		stateBytes, serr := crdt.StateVector(doc)
		if serr != nil {
			return result, serr
		}
		newSV, derr := yproto.DecodeStateVector(stateBytes, 0)
		if derr != nil {
			return result, derr
		}
		if svEqual(newSV, sv) && !scan.HasDeletes {
			outcomes = append(outcomes, CollabOutcome{Kind: "noop"})
			continue
		}
		sv, size = newSV, estimate
		outcomes = append(outcomes, CollabOutcome{Kind: "applied"})
		applied = append(applied, update)
	}
	var merged []byte
	switch {
	case len(applied) > 1:
		// merge_updates semantics for storage: one update covering everything
		// the batch added (the delta from the state before the batch).
		merged, err = crdt.StateAsUpdate(doc, svBeforeBytes)
		if err != nil {
			return result, err
		}
	case len(applied) == 1:
		merged = applied[0]
	}
	gone := len(applied) > 0 && !poisoned && yschema.HasNoPages(doc)
	return BatchResult{
		Outcomes: outcomes, SV: sv, Size: size, Merged: merged,
		Applied: applied, PagesGone: gone, Poisoned: poisoned,
	}, nil
}

func svEqual(a, b map[uint64]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// wouldExceed ports _would_exceed: apply to a scratch copy and measure.
func wouldExceed(doc *crdt.Doc, update []byte, limit int) (out struct {
	exceeds bool
	size    int
}, ok bool) {
	defer func() {
		if p := recover(); p != nil {
			ok = false
		}
	}()
	snapshot, err := crdt.Snapshot(doc)
	if err != nil {
		return out, false
	}
	scratch := crdt.NewDoc()
	if err := crdt.ApplyUpdate(scratch, snapshot); err != nil {
		return out, false
	}
	if err := crdt.ApplyUpdate(scratch, update); err != nil {
		return out, false
	}
	size, err := documentSize(scratch)
	if err != nil {
		return out, false
	}
	return struct {
		exceeds bool
		size    int
	}{size > limit, size}, true
}

// Conversion is engine.Conversion: a validated DoryDocument from a Y state.
type Conversion struct {
	Document  map[string]any
	Anomalies *yschema.Anomalies
	Problem   string
}

// Convert ports engine.convert: encoded Y state -> validated DoryDocument.
func Convert(state []byte) Conversion {
	doc := crdt.NewDoc()
	if err := crdt.ApplyUpdate(doc, state); err != nil {
		return Conversion{Problem: fmt.Sprintf("ValueError: %v", err)}
	}
	out, anomalies, uerr := readCollabDocument(doc)
	if uerr != nil {
		return Conversion{Problem: "The document is nested too deeply."}
	}
	if derr := validateDocument(out, maxDocumentBytes()); derr != nil {
		return Conversion{Document: out, Anomalies: anomalies, Problem: derr.Code}
	}
	return Conversion{Document: out, Anomalies: anomalies}
}

// RepairDelta ports engine.repair: delete anomalies from the live document;
// returns the delta (nil when there was nothing to repair).
func RepairDelta(doc *crdt.Doc) ([]byte, error) {
	_, anomalies, err := readCollabDocument(doc)
	if err != nil {
		return nil, err
	}
	if anomalies.Empty() {
		return nil, nil
	}
	before, err := crdt.StateVector(doc)
	if err != nil {
		return nil, err
	}
	doc.Transact(func() {
		yschema.Repair(doc, anomalies)
	}, nil)
	return crdt.StateAsUpdate(doc, before)
}

// MakeReplacement ports engine.make_replacement: the update that turns the
// live document into `document` inside the same history (version restore).
func MakeReplacement(doc *crdt.Doc, document map[string]any) ([]byte, error) {
	snapshot, err := crdt.Snapshot(doc)
	if err != nil {
		return nil, err
	}
	scratch := crdt.NewDoc()
	if err := crdt.ApplyUpdate(scratch, snapshot); err != nil {
		return nil, err
	}
	before, err := crdt.StateVector(scratch)
	if err != nil {
		return nil, err
	}
	var werr error
	scratch.Transact(func() {
		werr = yschema.WriteDocument(scratch, document)
	}, nil)
	if werr != nil {
		return nil, werr
	}
	return crdt.StateAsUpdate(scratch, before)
}
