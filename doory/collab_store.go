// Database side of the rooms (apps/collab/store.py): the durable Y state
// (snapshot + appended update rows), flush/replacement CAS and the
// compaction helpers. Plain synchronous functions.

// Ports backend/apps/collab/store.py
// Ports backend/apps/collab/models.py
package doory

import (
	"database/sql"
	"strings"
	"sync/atomic"
	"time"
)

// heartbeatsBehindStale ports HEARTBEAT_STALE (30s) for cutoff maths.
const heartbeatsStale = 30 * time.Second

// savingFromRoom ports signals.saving_from_room: Design saves made by a room
// itself must not echo back to rooms (the notify hooks check this).
var savingFromRoom atomic.Bool

func beginSavingFromRoom() func() {
	savingFromRoom.Store(true)
	return func() { savingFromRoom.Store(false) }
}

// CollabLoadedState is store.LoadedState.
type CollabLoadedState struct {
	Snapshot      []byte
	Updates       [][]byte
	Version       int64
	FlushedVersion int64
	BaseRevision  int64
	Title         string
	Revision      int64
}

// collabStaleCutoff is store.stale_cutoff: heartbeats older than this belong
// to processes that are gone.
func collabStaleCutoff() string {
	return formatDjangoDateTime(time.Now().Add(-heartbeatsStale))
}

// resetAllowed ports _reset_allowed: a stale state is only rebuilt while
// nobody else is connected (their rooms would lose history).
func (a *App) resetAllowed(designID, instance string) bool {
	var n int
	_ = a.DB.QueryRow(
		`SELECT COUNT(*) FROM collab_roomconnection WHERE design_id = ? AND heartbeat_at > ?
		 AND instance <> ?`,
		designID, collabStaleCutoff(), instance).Scan(&n)
	return n == 0
}

// CollabLoadOrInit ports store.load_or_init: the stored Y state of a design,
// rebuilt from Design.document when it is missing or stale. ok=false for an
// unknown design (DoesNotExist upstream).
func (a *App) CollabLoadOrInit(designID, instance string,
	build func(document map[string]any) ([]byte, error),
) (state CollabLoadedState, ok bool, err error) {
	tx, err := a.DB.Begin()
	if err != nil {
		return state, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var design designRow
	design, err = scanDesign(tx.QueryRow(`SELECT `+designCols+` FROM designs_design WHERE id = ?`,
		strings.ToLower(designID)))
	if err == sql.ErrNoRows {
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	var (
		snapshot       []byte
		version        int64
		flushedVersion int64
		baseRevision   int64
		haveState      bool
	)
	err = tx.QueryRow(`SELECT snapshot, version, flushed_version, base_revision
	      FROM collab_ystate WHERE design_id = ?`, design.ID).
		Scan(&snapshot, &version, &flushedVersion, &baseRevision)
	haveState = err == nil
	if err != nil && err != sql.ErrNoRows {
		return state, false, err
	}
	usable := haveState
	if usable && baseRevision != design.Revision {
		// reset_allowed, on THIS transaction's connection (a nested DB call
		// would need a second connection — SQLite pools one).
		var others int
		if qerr := tx.QueryRow(
			`SELECT COUNT(*) FROM collab_roomconnection WHERE design_id = ? AND heartbeat_at > ?
			 AND instance <> ?`, designID, collabStaleCutoff(), instance).Scan(&others); qerr != nil {
			return state, false, qerr
		}
		usable = others == 0
	}
	if usable {
		updates, uerr := loadYUpdatesTx(tx, design.ID)
		if uerr != nil {
			return state, false, uerr
		}
		return CollabLoadedState{
			Snapshot: snapshot, Updates: updates, Version: version,
			FlushedVersion: flushedVersion, BaseRevision: baseRevision,
			Title: design.Title, Revision: design.Revision,
		}, true, nil
	}
	// Rebuild from Design.document.
	var docRaw string
	if derr := tx.QueryRow(`SELECT document FROM designs_design WHERE id = ?`,
		design.ID).Scan(&docRaw); derr != nil {
		return state, false, derr
	}
	parsed, perr := parseJSONValue([]byte(docRaw))
	if perr != nil {
		return state, false, perr
	}
	document, isMap := parsed.(map[string]any)
	if !isMap {
		document = map[string]any{}
	}
	built, berr := build(document)
	if berr != nil {
		return state, false, berr
	}
	if _, derr := tx.Exec(`DELETE FROM collab_yupdate WHERE design_id = ?`, design.ID); derr != nil {
		return state, false, derr
	}
	now := formatDjangoDateTime(time.Now())
	if !haveState {
		if _, ierr := tx.Exec(
			`INSERT INTO collab_ystate (id, created_at, updated_at, snapshot, version,
			 flushed_version, base_revision, design_id, last_editor_id)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			newUUID4HexDashed(), now, now, built, int64(0), int64(0), design.Revision,
			design.ID, nil); ierr != nil {
			return state, false, ierr
		}
	} else {
		if _, uerr := tx.Exec(
			`UPDATE collab_ystate SET snapshot=?, version=?, flushed_version=?,
			 base_revision=?, updated_at=? WHERE design_id=?`,
			built, int64(0), int64(0), design.Revision, now, design.ID); uerr != nil {
			return state, false, uerr
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		return state, false, cerr
	}
	return CollabLoadedState{
		Snapshot: built, Updates: nil, Version: 0, FlushedVersion: 0,
		BaseRevision: design.Revision, Title: design.Title, Revision: design.Revision,
	}, true, nil
}

func loadYUpdatesTx(tx *sql.Tx, designID string) ([][]byte, error) {
	rows, err := tx.Query(
		`SELECT data FROM collab_yupdate WHERE design_id = ? ORDER BY version`, designID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CollabAppendUpdate ports store.append_update: durably append one batch;
// returns the new version.
func (a *App) CollabAppendUpdate(designID string, data []byte, editorID string) (int64, error) {
	tx, err := a.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var version int64
	var lastEditor any
	if err := tx.QueryRow(`SELECT version, last_editor_id FROM collab_ystate WHERE design_id = ?`,
		designID).Scan(&version, &lastEditor); err != nil {
		return 0, err
	}
	version++
	if editorID != "" {
		lastEditor = editorID
	}
	now := formatDjangoDateTime(time.Now())
	if _, err := tx.Exec(`UPDATE collab_ystate SET version=?, last_editor_id=?, updated_at=?
	      WHERE design_id=?`, version, lastEditor, now, designID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`INSERT INTO collab_yupdate (id, created_at, updated_at, design_id, version, data)
		 VALUES (?,?,?,?,?,?)`, newUUID4HexDashed(), now, now, designID, version, data); err != nil {
		return 0, err
	}
	return version, tx.Commit()
}

// CollabCompactionInput is store.CompactionInput.
type CollabCompactionInput struct {
	Snapshot []byte
	RowIDs   []string
	Updates  [][]byte
}

// CollabReadForCompaction ports store.read_for_compaction.
func (a *App) CollabReadForCompaction(designID string) *CollabCompactionInput {
	var snapshot []byte
	if err := a.DB.QueryRow(`SELECT snapshot FROM collab_ystate WHERE design_id = ?`,
		designID).Scan(&snapshot); err != nil {
		return nil
	}
	rows, err := a.DB.Query(`SELECT id, data FROM collab_yupdate WHERE design_id = ? ORDER BY version`,
		designID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	in := &CollabCompactionInput{Snapshot: snapshot}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil
		}
		in.RowIDs = append(in.RowIDs, id)
		in.Updates = append(in.Updates, data)
	}
	if len(in.RowIDs) == 0 {
		return nil
	}
	return in
}

// CollabApplyCompaction ports store.apply_compaction: replace the snapshot by
// `merged` and drop the rows it folded in, unless someone compacted meanwhile.
func (a *App) CollabApplyCompaction(designID string, in *CollabCompactionInput, merged []byte) (bool, error) {
	tx, err := a.DB.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var current []byte
	if err := tx.QueryRow(`SELECT snapshot FROM collab_ystate WHERE design_id = ?`,
		designID).Scan(&current); err != nil {
		return false, nil
	}
	if !bytesEqual(current, in.Snapshot) {
		return false, nil
	}
	for _, id := range in.RowIDs {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM collab_yupdate WHERE id = ?`, id).Scan(&n); err != nil || n == 0 {
			return false, nil
		}
	}
	now := formatDjangoDateTime(time.Now())
	if _, err := tx.Exec(`UPDATE collab_ystate SET snapshot=?, updated_at=? WHERE design_id=?`,
		merged, now, designID); err != nil {
		return false, err
	}
	for _, id := range in.RowIDs {
		if _, err := tx.Exec(`DELETE FROM collab_yupdate WHERE id = ?`, id); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CollabFlushResult is store.FlushResult.
type CollabFlushResult struct {
	Saved    bool
	Revision int64
	Reason   string
}

// CollabFlushDocument ports store.flush_document: write the (validated)
// document to the design when it is newer than what was saved.
func (a *App) CollabFlushDocument(designID string, document map[string]any, version int64,
	editorID string,
) (CollabFlushResult, error) {
	tx, err := a.DB.Begin()
	if err != nil {
		return CollabFlushResult{Reason: "gone"}, err
	}
	defer func() { _ = tx.Rollback() }()
	var (
		baseRevision   int64
		flushedVersion int64
		lastEditor     sql.NullString
		stateFound     bool
	)
	err = tx.QueryRow(`SELECT base_revision, flushed_version, last_editor_id
	      FROM collab_ystate WHERE design_id = ?`, designID).
		Scan(&baseRevision, &flushedVersion, &lastEditor)
	stateFound = err == nil
	if err != nil && err != sql.ErrNoRows {
		return CollabFlushResult{Reason: "gone"}, err
	}
	design, derr := scanDesign(tx.QueryRow(`SELECT `+designCols+
		` FROM designs_design WHERE id = ? AND trashed_at IS NULL`, designID))
	if !stateFound || derr == sql.ErrNoRows {
		return CollabFlushResult{Reason: "gone"}, nil
	}
	if derr != nil {
		return CollabFlushResult{Reason: "gone"}, derr
	}
	if version <= flushedVersion {
		return CollabFlushResult{Saved: false, Revision: design.Revision, Reason: "up_to_date"}, nil
	}
	if design.Revision != baseRevision {
		return CollabFlushResult{Saved: false, Revision: design.Revision, Reason: "external_change"}, nil
	}
	info, ferr := describeDocument(document)
	if ferr != nil {
		return CollabFlushResult{}, ferr
	}
	docRaw := collabJSON(document)
	newRevision := design.Revision + 1
	now := formatDjangoDateTime(time.Now())
	if _, err := tx.Exec(
		`UPDATE designs_design SET document=?, width=?, height=?, page_count=?, preset=?,
		 revision=?, updated_at=? WHERE id=?`,
		string(docRaw), info.Width, info.Height, info.PageCount, info.Preset,
		newRevision, now, designID); err != nil {
		return CollabFlushResult{}, err
	}
	if _, err := tx.Exec(
		`UPDATE collab_ystate SET base_revision=?, flushed_version=?, updated_at=?
		 WHERE design_id=?`, newRevision, version, now, designID); err != nil {
		return CollabFlushResult{}, err
	}
	// saving_from_room: the notify hooks (signals port) must not echo this save.
	done := beginSavingFromRoom()
	defer done()
	if err := tx.Commit(); err != nil {
		return CollabFlushResult{}, err
	}
	// record_auto_version_if_due with the editor (or the state's last one).
	designForVersion := design
	designForVersion.Revision = newRevision
	var editor *UserRow
	editorUse := editorID
	if editorUse == "" {
		editorUse = lastEditor.String
	}
	if editorUse != "" {
		if u, uerr := getUserByID(a.DB, editorUse); uerr == nil && u != nil {
			editor = u
		}
	}
	a.maybeRecordAutoVersion(&designForVersion, docRaw, editor)
	return CollabFlushResult{Saved: true, Revision: newRevision}, nil
}

// CollabReplaceTicket is store.ReplaceTicket.
type CollabReplaceTicket struct {
	DesignID string
	Document string // stored JSON text (parsed by the caller)
	Revision int64
	Title    string
}

// CollabReadReplacement ports store.read_replacement: the design's current
// document when it moved on without the room (a restore); nil when in sync.
func (a *App) CollabReadReplacement(designID string) *CollabReplaceTicket {
	var stateRevision int64
	if err := a.DB.QueryRow(`SELECT base_revision FROM collab_ystate WHERE design_id = ?`,
		designID).Scan(&stateRevision); err != nil {
		return nil
	}
	design, err := scanDesign(a.DB.QueryRow(
		`SELECT `+designCols+` FROM designs_design WHERE id = ? AND trashed_at IS NULL`, designID))
	if err != nil {
		return nil
	}
	if design.Revision == stateRevision {
		return nil
	}
	return &CollabReplaceTicket{
		DesignID: design.ID, Document: design.Document,
		Revision: design.Revision, Title: design.Title,
	}
}

// CollabCommitReplacement ports store.commit_replacement: claim the
// replacement of the Y state described by the ticket and append its delta.
// Returns the new version, or ok=false when another process claimed it first
// (or the design moved on again).
func (a *App) CollabCommitReplacement(ticket *CollabReplaceTicket, delta []byte) (version int64, ok bool, err error) {
	tx, err := a.DB.Begin()
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var baseRevision int64
	if err := tx.QueryRow(`SELECT base_revision FROM collab_ystate WHERE design_id = ?`,
		ticket.DesignID).Scan(&baseRevision); err != nil {
		return 0, false, nil
	}
	var designRevision int64
	if err := tx.QueryRow(`SELECT revision FROM designs_design WHERE id = ?`,
		ticket.DesignID).Scan(&designRevision); err != nil {
		return 0, false, nil
	}
	if baseRevision == ticket.Revision || designRevision != ticket.Revision {
		return 0, false, nil
	}
	var cur int64
	if err := tx.QueryRow(`SELECT version FROM collab_ystate WHERE design_id = ?`,
		ticket.DesignID).Scan(&cur); err != nil {
		return 0, false, err
	}
	version = cur + 1
	now := formatDjangoDateTime(time.Now())
	if _, err := tx.Exec(`UPDATE collab_ystate SET version=?, base_revision=?, flushed_version=?,
	      updated_at=? WHERE design_id=?`, version, ticket.Revision, version, now, ticket.DesignID); err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(`INSERT INTO collab_yupdate (id, created_at, updated_at, design_id, version, data)
	      VALUES (?,?,?,?,?,?)`, newUUID4HexDashed(), now, now, ticket.DesignID, version, delta); err != nil {
		return 0, false, err
	}
	return version, true, tx.Commit()
}

// CollabLoadCurrent ports store.load_current: snapshot and update rows as
// they are now (a room that fell behind re-reads them).
func (a *App) CollabLoadCurrent(designID string) ([]byte, [][]byte, bool) {
	var snapshot []byte
	if err := a.DB.QueryRow(`SELECT snapshot FROM collab_ystate WHERE design_id = ?`,
		designID).Scan(&snapshot); err != nil {
		return nil, nil, false
	}
	updates, err := loadYUpdatesTxWrap(a.DB, designID)
	if err != nil {
		return nil, nil, false
	}
	return snapshot, updates, true
}

func loadYUpdatesTxWrap(db *sql.DB, designID string) ([][]byte, error) {
	rows, err := db.Query(`SELECT data FROM collab_yupdate WHERE design_id = ? ORDER BY version`, designID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CollabCurrentVersion port store.current_version.
func (a *App) CollabCurrentVersion(designID string) (int64, bool) {
	var v int64
	if err := a.DB.QueryRow(`SELECT version FROM collab_ystate WHERE design_id = ?`,
		designID).Scan(&v); err != nil {
		return 0, false
	}
	return v, true
}

// CollabDesignsNeedingCompaction ports store.designs_needing_compaction.
func (a *App) CollabDesignsNeedingCompaction(threshold int) []string {
	rows, err := a.DB.Query(`SELECT design_id FROM collab_yupdate GROUP BY design_id
	      HAVING COUNT(id) >= ?`, threshold)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil
		}
		out = append(out, id)
	}
	return out
}

// CollabPurgeStalePresence ports store.purge_stale_presence: presence rows of
// processes that stopped without cleaning up; returns how many were removed.
func (a *App) CollabPurgeStalePresence() int64 {
	cutoff := formatDjangoDateTime(time.Now().Add(-heartbeatsStale * 4))
	res, err := a.DB.Exec(`DELETE FROM collab_roomconnection WHERE heartbeat_at < ?`, cutoff)
	if err != nil {
		return 0
	}
	_, _ = a.DB.Exec(`DELETE FROM collab_collabinstance WHERE heartbeat_at < ?`, cutoff)
	n, _ := res.RowsAffected()
	return n
}

// CollabUnflushedDesigns ports store.unflushed_designs: designs whose Y state
// is ahead of their document.
func (a *App) CollabUnflushedDesigns() []string {
	rows, err := a.DB.Query(`SELECT s.design_id FROM collab_ystate s
	      JOIN designs_design d ON d.id = s.design_id
	      WHERE s.version > s.flushed_version AND d.trashed_at IS NULL`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil
		}
		out = append(out, id)
	}
	return out
}

// CollabResetState ports store.reset_state: throw the stored Y state away and
// rebuild it from Design.document (the state could not be read).
func (a *App) CollabResetState(designID string) error {
	var docRaw string
	if err := a.DB.QueryRow(`SELECT document FROM designs_design WHERE id = ?`,
		designID).Scan(&docRaw); err != nil {
		return err
	}
	parsed, err := parseJSONValue([]byte(docRaw))
	if err != nil {
		return err
	}
	document, _ := parsed.(map[string]any)
	if document == nil {
		document = map[string]any{}
	}
	snapshot, err := buildSnapshot(document)
	if err != nil {
		return err
	}
	var revision int64
	if err := a.DB.QueryRow(`SELECT revision FROM designs_design WHERE id = ?`,
		designID).Scan(&revision); err != nil {
		return err
	}
	if _, err := a.DB.Exec(`DELETE FROM collab_yupdate WHERE design_id = ?`, designID); err != nil {
		return err
	}
	now := formatDjangoDateTime(time.Now())
	var n int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM collab_ystate WHERE design_id = ?`, designID).Scan(&n)
	if n == 0 {
		_, err = a.DB.Exec(`INSERT INTO collab_ystate (id, created_at, updated_at, snapshot,
		      version, flushed_version, base_revision, design_id, last_editor_id)
		      VALUES (?,?,?,?,?,?,?,?,?)`, newUUID4HexDashed(), now, now, snapshot,
			int64(0), int64(0), revision, designID, nil)
		return err
	}
	_, err = a.DB.Exec(`UPDATE collab_ystate SET snapshot=?, version=?, flushed_version=?,
	      base_revision=?, updated_at=? WHERE design_id=?`,
		snapshot, int64(0), int64(0), revision, now, designID)
	return err
}
