// Permanent deletion of designs (designs/services.py purge_designs + the
// pre_delete signal that removes thumbnails).
//
// Django's collector deletes every row that references a design before the
// design itself: comments (children before parents), live-room presence, the
// Y state/updates, share links, versions, then the design. The pre_delete
// signal queues removal of the design thumbnail and its versions' thumbnails.
// SQLite/Postgres both enforce these FKs here, so a partial delete is a hard
// error — the earlier port deleted only the versions and answered 500 as soon
// as a share link or comment existed.

package doory

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"
)

// purgeDesigns permanently deletes the given designs (cascade + files) and
// returns how many design rows were removed.
func (a *App) purgeDesigns(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	deleted := 0
	for _, id := range ids {
		thumbnails, terr := a.designThumbnails(tx, id)
		if terr != nil {
			return deleted, terr
		}
		// Order matters: children first (FKs are DEFERRABLE but the delete
		// itself must not leave dangling references), matching the collector.
		steps := []string{
			`DELETE FROM sharing_comment WHERE design_id = ? AND parent_id IS NOT NULL`, // replies
			`DELETE FROM sharing_comment WHERE design_id = ?`,
			`DELETE FROM collab_roomconnection WHERE design_id = ?`,
			`DELETE FROM collab_ystate WHERE design_id = ?`,
			`DELETE FROM collab_yupdate WHERE design_id = ?`,
			`DELETE FROM sharing_sharelink WHERE design_id = ?`,
			`DELETE FROM designs_designversion WHERE design_id = ?`,
			`DELETE FROM designs_design WHERE id = ?`,
		}
		for _, stmt := range steps {
			if _, derr := tx.Exec(stmt, id); derr != nil {
				return deleted, derr
			}
		}
		deleted++
		for _, name := range thumbnails {
			a.deleteMediaFile(name)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// designThumbnails lists the design's own thumbnail and its versions'
// thumbnails (the pre_delete signal's file list).
func (a *App) designThumbnails(tx *sql.Tx, designID string) ([]string, error) {
	var names []string
	var own string
	if err := tx.QueryRow(`SELECT thumbnail FROM designs_design WHERE id = ?`, designID).Scan(&own); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if own != "" {
		names = append(names, own)
	}
	rows, err := tx.Query(`SELECT thumbnail FROM designs_designversion
	      WHERE design_id = ? AND thumbnail != ''`, designID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// deleteMediaFile removes one stored file (best effort: Django queues file
// deletion on commit and ignores a storage error).
func (a *App) deleteMediaFile(name string) {
	if name == "" || a.Settings.MediaRoot == "" {
		return
	}
	_ = os.Remove(filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(name)))
}

// purgeExpiredTrash ports services.purge_expired_trash: designs trashed longer
// ago than `days` (retention) are deleted permanently.
func (a *App) purgeExpiredTrash(days int, now time.Time) (int, error) {
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := a.DB.Query(
		`SELECT id FROM designs_design WHERE trashed_at IS NOT NULL AND trashed_at < ?`,
		formatDjangoDateTime(cutoff))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	deleted, derr := a.purgeDesigns(ids)
	a.notifyDesignsRecheck(ids)
	return deleted, derr
}
