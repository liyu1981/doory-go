// The collab_maintenance housekeeping pass (apps/collab/management/commands/
// collab_maintenance.py): save designs whose real-time state is ahead of their
// document (after a crash), fold long update logs into their snapshot, and
// remove presence rows of processes that are gone. Safe to run at any time.

package doory

import (
	"fmt"
	"io"

	"doory-go/internal/crdt"
)

// CollabMaintenanceResult is the command's summary line.
type CollabMaintenanceResult struct {
	Saved     int
	Skipped   int
	Failed    int
	Compacted int
	Removed   int64
}

// RunCollabMaintenance ports Command.handle. `out` receives the same summary
// the management command writes to stdout; nil discards it.
func (a *App) RunCollabMaintenance(out io.Writer) CollabMaintenanceResult {
	res := CollabMaintenanceResult{}
	res.Removed = a.CollabPurgeStalePresence()

	for _, designID := range a.CollabUnflushedDesigns() {
		if a.CollabRoomIsLive(designID) {
			res.Skipped++
			continue
		}
		switch a.collabMaintenanceSave(designID) {
		case "saved":
			res.Saved++
		case "skipped":
			res.Skipped++
		default:
			res.Failed++
		}
	}

	threshold := envInt("REALTIME_COMPACT_AFTER_UPDATES", 200)
	for _, designID := range a.CollabDesignsNeedingCompaction(threshold) {
		if a.CollabRoomIsLive(designID) {
			continue
		}
		source := a.CollabReadForCompaction(designID)
		if source == nil {
			continue
		}
		merged, err := crdt.ApplyToFresh(source.Snapshot, source.Updates...)
		if err != nil {
			continue
		}
		if ok, cerr := a.CollabApplyCompaction(designID, source, merged); cerr == nil && ok {
			res.Compacted++
		}
	}

	if out != nil {
		fmt.Fprintf(out, "saved %d, skipped %d, failed %d; compacted %d; removed %d stale connection rows\n",
			res.Saved, res.Skipped, res.Failed, res.Compacted, res.Removed)
	}
	return res
}

// collabMaintenanceSave ports Command.save: rebuild the document from the
// stored state and flush it unless the room already did.
func (a *App) collabMaintenanceSave(designID string) string {
	snapshot, updates, ok := a.CollabLoadCurrent(designID)
	if !ok {
		return "skipped"
	}
	version, ok := a.CollabCurrentVersion(designID)
	if !ok {
		return "skipped"
	}
	state, err := crdt.ApplyToFresh(snapshot, updates...)
	if err != nil {
		return "failed"
	}
	conversion := Convert(state)
	if conversion.Document == nil {
		return "failed"
	}
	result, err := a.CollabFlushDocument(designID, conversion.Document, version, "")
	if err != nil {
		return "failed"
	}
	if result.Saved {
		return "saved"
	}
	return "skipped"
}
