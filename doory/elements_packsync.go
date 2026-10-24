// The sync_element_packs pipeline (apps/elements/packsync.py): plan -> fetch ->
// normalise -> import, idempotent and resumable.
//
// A pack's elements are imported in batches that each commit, and the pack is
// only marked as synced (its sync_fingerprint) once every batch went through,
// so an interrupted run continues where it stopped and an up-to-date pack is
// skipped without any download.

package doory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	elementTrademarkNotice = "Logos are trademarks of their respective owners; " +
		"use them according to their brand guidelines."
	elementPruneChunk = 500
)

// elementSyncOptions is packsync.SyncOptions.
type elementSyncOptions struct {
	Concurrency int
	Force       bool
	DryRun      bool
	Prune       bool
	BrandLogos  bool
}

// elementPackResult is packsync.PackResult.
type elementPackResult struct {
	PackID     string
	Status     string // synced | up-to-date | dry-run | failed | not-attempted
	SourceRef  string
	Created    int
	Updated    int
	Unchanged  int
	Skipped    [][2]string
	Duplicates int
	Stale      int
	Pruned     int
	LogosUnpub int
	Warnings   []string
	Error      string
	Seconds    float64
}

func (r elementPackResult) ok() bool {
	return r.Status == "synced" || r.Status == "up-to-date" || r.Status == "dry-run"
}

func (r elementPackResult) total() int { return r.Created + r.Updated + r.Unchanged }

// elementSyncReport is packsync.SyncReport.
type elementSyncReport struct {
	Results []elementPackResult
	Aborted string
}

func (r *elementSyncReport) failed() []elementPackResult {
	var out []elementPackResult
	for _, result := range r.Results {
		if result.Status == "failed" || result.Status == "not-attempted" {
			out = append(out, result)
		}
	}
	return out
}

// elementPackRow is the pack columns the pipeline reads and writes.
type elementPackRow struct {
	ID              string
	Slug            string
	SyncFingerprint string
}

// loadElementPack reads the pack row for a slug (nil when absent).
func (a *App) loadElementPack(slug string) (*elementPackRow, error) {
	var row elementPackRow
	err := a.DB.QueryRow(`SELECT id, slug, sync_fingerprint FROM elements_elementpack WHERE slug = ?`,
		slug).Scan(&row.ID, &row.Slug, &row.SyncFingerprint)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// isNoRows reports sql.ErrNoRows.
func isNoRows(err error) bool { return err != nil && strings.Contains(err.Error(), "no rows") }

// elementPackHasElements reports whether the pack already has rows.
func (a *App) elementPackHasElements(packID string) bool {
	var count int
	if err := a.DB.QueryRow(`SELECT COUNT(*) FROM elements_element WHERE pack_id = ?`, packID).
		Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// elementPackUpToDate is packsync.is_up_to_date.
func (a *App) elementPackUpToDate(pack elementPackSpec, brandLogos bool) bool {
	row, err := a.loadElementPack(pack.ID)
	if err != nil || row == nil {
		return false
	}
	if row.SyncFingerprint != pack.fingerprintFor(brandLogos) {
		return false
	}
	return a.elementPackHasElements(row.ID)
}

// elementAttribution is packsync._attribution.
func elementAttribution(pack elementPackSpec, brandLogos bool) string {
	if !(brandLogos && pack.hasBrand()) {
		return pack.Attribution
	}
	return strings.TrimSpace(pack.Attribution + " " + elementTrademarkNotice)
}

// unpublishElements is packsync._unpublish.
func (a *App) unpublishElements(packID string, keys []string) (int, error) {
	total := 0
	for start := 0; start < len(keys); start += elementPruneChunk {
		end := start + elementPruneChunk
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := []any{formatDjangoDateTime(time.Now()), packID}
		for _, key := range chunk {
			args = append(args, key)
		}
		result, err := a.DB.Exec(`UPDATE elements_element SET is_published = FALSE, updated_at = ?
		     WHERE pack_id = ? AND is_published = TRUE AND key IN (`+placeholders+`)`, args...)
		if err != nil {
			return total, err
		}
		affected, _ := result.RowsAffected()
		total += int(affected)
	}
	return total, nil
}

// publishedElementKeys lists the pack's published keys.
func (a *App) publishedElementKeys(packID string) ([]string, error) {
	rows, err := a.DB.Query(`SELECT key FROM elements_element WHERE pack_id = ? AND is_published = TRUE`,
		packID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// importElementPack is packsync.import_pack: import already-fetched files.
func (a *App) importElementPack(pack elementPackSpec, files map[string][]byte,
	options elementSyncOptions, log func(string),
) (elementPackResult, error) {
	result := elementPackResult{PackID: pack.ID, SourceRef: pack.sourceRef()}
	started := time.Now()
	entries, skipped, duplicates := buildElementEntries(pack, files, &result.Warnings,
		options.BrandLogos)
	result.Skipped, result.Duplicates = skipped, duplicates
	if len(entries) == 0 {
		return result, packSourceErrorf("the release contains no usable artwork (check the manifest's file patterns)")
	}
	if options.DryRun {
		existing, err := a.loadElementPack(pack.ID)
		if err != nil {
			return result, err
		}
		if existing != nil {
			summary, berr := a.bulkImport(existing.ID, existing.Slug, entries, pack.Kind, true, nil)
			if berr != nil {
				return result, berr
			}
			result.Created, result.Updated, result.Unchanged = summary.Created, summary.Updated, summary.Unchanged
			result.Skipped = append(result.Skipped, summary.Skipped...)
		} else {
			result.Created = len(entries)
		}
		result.Status = "dry-run"
		result.Seconds = time.Since(started).Seconds()
		return result, nil
	}
	packID, _, err := a.ensureElementPack(pack.ID, pack.Name, pack.Kind, pack.License.SPDX,
		pack.Homepage, pack.Description, pack.SortOrder, pack.License.URL,
		elementAttribution(pack, options.BrandLogos))
	if err != nil {
		return result, err
	}
	summary, berr := a.bulkImport(packID, pack.ID, entries, pack.Kind, false,
		func(partial elementImportSummary) {
			log(fmt.Sprintf("    %s: %d/%d", pack.ID,
				partial.Created+partial.Updated+partial.Unchanged, len(entries)))
		})
	if berr != nil {
		return result, berr
	}
	produced := map[string]bool{}
	for _, entry := range entries {
		produced[entry.Key] = true
	}
	publishedKeys, err := a.publishedElementKeys(packID)
	if err != nil {
		return result, err
	}
	var stale []string
	for _, key := range publishedKeys {
		if !produced[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	var logos []string
	if pack.hasBrand() && !options.BrandLogos {
		brandRe, _ := compileElementPattern(pack.Naming.Brand)
		var remaining []string
		for _, key := range stale {
			if brandRe.match(key) {
				logos = append(logos, key)
				continue
			}
			remaining = append(remaining, key)
		}
		stale = remaining
	}
	result.Stale = len(stale)
	if len(logos) > 0 {
		unpublished, uerr := a.unpublishElements(packID, logos)
		if uerr != nil {
			return result, uerr
		}
		result.LogosUnpub = unpublished
	}
	if options.Prune && len(stale) > 0 {
		pruned, perr := a.unpublishElements(packID, stale)
		if perr != nil {
			return result, perr
		}
		result.Pruned = pruned
	}
	now := formatDjangoDateTime(time.Now())
	if _, uerr := a.DB.Exec(`UPDATE elements_elementpack SET source_ref = ?, sync_fingerprint = ?,
	     synced_at = ?, updated_at = ? WHERE id = ?`,
		pack.sourceRef(), pack.fingerprintFor(options.BrandLogos), now, now, packID); uerr != nil {
		return result, uerr
	}
	result.Status = "synced"
	result.Created, result.Updated, result.Unchanged = summary.Created, summary.Updated, summary.Unchanged
	result.Skipped = append(result.Skipped, summary.Skipped...)
	result.Seconds = time.Since(started).Seconds()
	return result, nil
}

// syncElementPacks is packsync.sync_packs.
func (a *App) syncElementPacks(packs []elementPackSpec, source packSource,
	options elementSyncOptions, log func(string),
) *elementSyncReport {
	report := &elementSyncReport{}
	total := len(packs)
	width := len(fmt.Sprintf("%d", total))
	var todo []elementPackSpec
	for index, pack := range packs {
		if !options.Force && a.elementPackUpToDate(pack, options.BrandLogos) {
			report.Results = append(report.Results, elementPackResult{PackID: pack.ID,
				Status: "up-to-date", SourceRef: pack.sourceRef()})
			log(fmt.Sprintf("[%*d/%d] %s: up to date (%s), nothing to download",
				width, index+1, total, pack.ID, pack.sourceRef()))
			continue
		}
		todo = append(todo, pack)
	}
	isTodo := map[string]bool{}
	for _, pack := range todo {
		isTodo[pack.ID] = true
	}
	for index, pack := range packs {
		if !isTodo[pack.ID] {
			continue
		}
		prefix := fmt.Sprintf("[%*d/%d] %s", width, index+1, total, pack.ID)
		if report.Aborted != "" {
			report.Results = append(report.Results, elementPackResult{PackID: pack.ID, Status: "not-attempted"})
			continue
		}
		log(fmt.Sprintf("%s: %s ...", prefix, pack.sourceRef()))
		files, err := a.elementPackFiles(pack, source, pack.isGenerated())
		if err != nil {
			var unavailable *packSourceUnavailable
			if asPackUnavailable(err, &unavailable) {
				report.Aborted = err.Error()
				report.Results = append(report.Results, elementPackResult{PackID: pack.ID, Status: "failed",
					Error: err.Error()})
				log(fmt.Sprintf("%s: %v. Stopping; run again to resume.", prefix, err))
				continue
			}
			report.Results = append(report.Results, elementPackResult{PackID: pack.ID, Status: "failed",
				Error: err.Error()})
			log(fmt.Sprintf("%s: FAILED: %v", prefix, err))
			continue
		}
		result, ierr := a.importElementPack(pack, files, options, func(line string) { log(line) })
		if ierr != nil {
			var unavailable *packSourceUnavailable
			if asPackUnavailable(ierr, &unavailable) {
				report.Aborted = ierr.Error()
			}
			report.Results = append(report.Results, elementPackResult{PackID: pack.ID, Status: "failed",
				Error: ierr.Error()})
			log(fmt.Sprintf("%s: FAILED: %v", prefix, ierr))
			continue
		}
		report.Results = append(report.Results, result)
		line := fmt.Sprintf("%s: %d elements (%d created, %d updated, %d unchanged", prefix, result.total(),
			result.Created, result.Updated, result.Unchanged)
		if result.Duplicates > 0 {
			line += fmt.Sprintf(", %d aliases merged", result.Duplicates)
		}
		if len(result.Skipped) > 0 {
			line += fmt.Sprintf(", %d skipped", len(result.Skipped))
		}
		line += fmt.Sprintf(") in %.1fs", result.Seconds)
		if result.Status == "dry-run" {
			line += " [dry run]"
		}
		log(line)
		for _, warning := range result.Warnings {
			log("    ! " + warning)
		}
		for i, skip := range result.Skipped {
			if i == 5 {
				break
			}
			log(fmt.Sprintf("    skipped %s: %s", skip[0], skip[1]))
		}
		if len(result.Skipped) > 5 {
			log(fmt.Sprintf("    ... and %d more skipped", len(result.Skipped)-5))
		}
		if result.LogosUnpub > 0 {
			log(fmt.Sprintf("    %d brand logo(s) unpublished (brand logos are off).", result.LogosUnpub))
		}
		if result.Stale > 0 {
			if options.Prune {
				log(fmt.Sprintf("    %d element(s) are no longer part of this pack; %d unpublished.",
					result.Stale, result.Pruned))
			} else {
				log(fmt.Sprintf("    %d element(s) are no longer part of this pack "+
					"(run with --prune to unpublish them).", result.Stale))
			}
		}
	}
	return report
}

// elementPackStaleFiles is used by the importer's directory cleanup: the svg
// files of a pack that no longer belong to it (bulk_import's replaced files).
func (a *App) removeElementFile(storage string) {
	if storage == "" {
		return
	}
	_ = os.Remove(filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(storage)))
}
