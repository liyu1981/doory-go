// Management commands: the deterministic, database-only ones
// (M13 / family 13 "commands + content").
//
// Each Run* function is BaseCommand.handle for one upstream command; the
// stdout text is the command's own, because the cli-driver compares it.

package doory

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// runPurgeTrash ports apps/designs/management/commands/purge_trash.py.
func runPurgeTrash(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{{name: "days", kind: "int"}})
	if err != nil {
		return err
	}
	days := args.intOption("days", 30)
	if days < 0 {
		return commandErrorf("--days must be zero or greater.")
	}
	deleted, derr := c.App.purgeExpiredTrash(days, time.Now().UTC())
	if derr != nil {
		return commandErrorf("%v", derr)
	}
	c.Printf("Purged %d design(s) trashed more than %d day(s) ago.\n", deleted, days)
	return nil
}

// runPurgeExports ports apps/exports/management/commands/purge_exports.py.
func runPurgeExports(c *CLI, argv []string) error {
	if _, err := parseArgs(argv, nil); err != nil {
		return err
	}
	removed := c.App.purgeExpiredExports(time.Now().UTC())
	c.Printf("Removed %d expired exported image(s).\n", removed)
	return nil
}

// runReencryptAISecrets ports apps/ai/management/commands/reencrypt_ai_secrets.py.
func runReencryptAISecrets(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{{name: "dry-run", kind: "bool", dest: "dry_run"}})
	if err != nil {
		return err
	}
	dryRun := args.bool("dry_run")
	report, rerr := c.App.reencryptProviderSecrets(dryRun)
	if rerr != nil {
		return commandErrorf("%v", rerr)
	}
	actionWord := "Re-encrypted"
	if dryRun {
		actionWord = "Would re-encrypt"
	}
	c.Printf("%s %d provider secret(s). %d failed.\n", actionWord, report.Reencrypted, report.Failed)
	if report.Failed > 0 {
		return commandErrorf("%d provider secret(s) could not be decrypted; enter them again.", report.Failed)
	}
	return nil
}

// runCollabMaintenance ports apps/collab/management/commands/collab_maintenance.py.
func runCollabMaintenance(c *CLI, argv []string) error {
	if _, err := parseArgs(argv, nil); err != nil {
		return err
	}
	c.App.RunCollabMaintenance(c.Stdout)
	return nil
}

// runSeedGallery ports apps/gallery/management/commands/seed_gallery.py: the
// standard template categories, in display order.
func runSeedGallery(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{{name: "update", kind: "bool"}})
	if err != nil {
		return err
	}
	report, serr := c.App.seedGalleryCategories(args.bool("update"))
	if serr != nil {
		return commandErrorf("%v", serr)
	}
	c.Printf("Template categories: %d created, %d updated, %d unchanged.\n",
		report.Created, report.Updated, report.Unchanged)
	return nil
}

// galleryCategories is seed_gallery.CATEGORIES: (slug, name, Lucide icon).
var galleryCategories = [][3]string{
	{"social-media", "Social Media", "share-2"},
	{"presentations", "Presentations", "presentation"},
	{"posters", "Posters", "image"},
	{"flyers", "Flyers", "file-text"},
	{"invitations", "Invitations", "mail"},
	{"cards", "Cards", "credit-card"},
	{"resumes", "Resumes", "file-user"},
	{"logos", "Logos", "shapes"},
	{"marketing", "Marketing", "megaphone"},
	{"education", "Education", "graduation-cap"},
	{"business", "Business", "briefcase"},
	{"photo-collage", "Photo Collage", "layout-grid"},
	{"youtube", "YouTube", "circle-play"},
	{"print", "Print", "printer"},
}

// SeedReport is the created/updated/unchanged triple the seed commands print.
type SeedReport struct {
	Created   int
	Updated   int
	Unchanged int
}

// seedGalleryCategories ports Command.handle of seed_gallery.
func (a *App) seedGalleryCategories(update bool) (SeedReport, error) {
	var report SeedReport
	tx, err := a.DB.Begin()
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback() }()
	now := formatDjangoDateTime(time.Now())
	for i, row := range galleryCategories {
		slug, name, icon := row[0], row[1], row[2]
		order := (i + 1) * 10
		var id string
		qerr := tx.QueryRow(`SELECT id FROM gallery_templatecategory WHERE slug = ?`, slug).Scan(&id)
		switch {
		case qerr != nil:
			id = newUUID4Hex()
			if _, ierr := tx.Exec(`INSERT INTO gallery_templatecategory
			     (id, created_at, updated_at, slug, name, icon, sort_order)
			     VALUES (?, ?, ?, ?, ?, ?, ?)`, id, now, now, slug, name, icon, order); ierr != nil {
				return report, ierr
			}
			report.Created++
		case update:
			var curName, curIcon string
			var curOrder int
			if serr := tx.QueryRow(`SELECT name, icon, sort_order FROM gallery_templatecategory WHERE id = ?`,
				id).Scan(&curName, &curIcon, &curOrder); serr != nil {
				return report, serr
			}
			if curName != name || curIcon != icon || curOrder != order {
				if _, uerr := tx.Exec(`UPDATE gallery_templatecategory SET name = ?, icon = ?, sort_order = ?,
				     updated_at = ? WHERE id = ?`, name, icon, order, now, id); uerr != nil {
					return report, uerr
				}
				report.Updated++
				continue
			}
			report.Unchanged++
		default:
			report.Unchanged++
		}
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, nil
}

// SecretsReencryptReport is reencrypt_ai_secrets.Command.handle's tally.
type SecretsReencryptReport struct {
	Reencrypted int
	Failed      int
}

// reencryptProviderSecrets ports Command.handle: every provider that stores a
// secret is decrypted with the configured keys and written back under the
// primary key. A secret no key can read is counted and reported.
func (a *App) reencryptProviderSecrets(dryRun bool) (SecretsReencryptReport, error) {
	var report SecretsReencryptReport
	rows, err := a.DB.Query(`SELECT id, name, secret_encrypted FROM ai_provider WHERE secret_encrypted != ''`)
	if err != nil {
		return report, err
	}
	type providerSecret struct {
		id     string
		name   string
		secret []byte
	}
	var providers []providerSecret
	for rows.Next() {
		var p providerSecret
		if err := rows.Scan(&p.id, &p.name, &p.secret); err != nil {
			rows.Close()
			return report, err
		}
		providers = append(providers, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	for _, p := range providers {
		plain, derr := a.Settings.aiDecrypt(p.secret)
		if derr != nil {
			report.Failed++
			continue
		}
		encrypted, eerr := a.Settings.aiEncrypt(plain)
		if eerr != nil {
			report.Failed++
			continue
		}
		if !dryRun {
			if _, uerr := a.DB.Exec(`UPDATE ai_provider SET secret_encrypted = ?, updated_at = ?
			     WHERE id = ?`, encrypted, formatDjangoDateTime(time.Now()), p.id); uerr != nil {
				return report, uerr
			}
		}
		report.Reencrypted++
	}
	return report, nil
}

// runSeedSizePresets ports apps/presets/management/commands/seed_size_presets.py.
func runSeedSizePresets(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{{name: "update", kind: "bool"}})
	if err != nil {
		return err
	}
	catalogue, cerr := builtinPresetCatalogue()
	if cerr != nil {
		return commandErrorf("%v", cerr)
	}
	report, werr := c.App.writePresetCatalogue(catalogue, "extend", args.bool("update"))
	if werr != nil {
		return commandErrorf("%v", werr)
	}
	c.Printf("Built-in size presets: %s.\n", report.String())
	return nil
}

// runImportSizePresets ports apps/presets/management/commands/import_size_presets.py.
func runImportSizePresets(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "mode", kind: "string", choices: []string{"extend", "replace"}},
		{name: "dry-run", kind: "bool", dest: "dry_run"},
	})
	if err != nil {
		return err
	}
	if len(args.positionals) < 1 {
		return &usageError{message: "the following arguments are required: paths"}
	}
	files, ferr := jsonFiles(args.positionals)
	if ferr != nil {
		return ferr
	}
	if len(files) == 0 {
		return commandErrorf("No .json files found.")
	}
	merged := &presetCatalogue{}
	for _, path := range files {
		catalogue, cerr := c.App.readPresetCatalogue(path)
		if cerr != nil {
			return commandErrorf("%v", cerr)
		}
		merged.Groups = append(merged.Groups, catalogue.Groups...)
		merged.Presets = append(merged.Presets, catalogue.Presets...)
		if uerr := checkPresetUnique(merged); uerr != nil {
			return commandErrorf("%v", uerr)
		}
	}
	mode := args.str("mode")
	if mode == "" {
		mode = "extend"
	}
	dryRun := args.bool("dry_run")
	report, werr := c.App.writePresetCatalogueTx(merged, mode, true, dryRun)
	if werr != nil {
		return commandErrorf("%v", werr)
	}
	if dryRun {
		c.Printf("Dry run, nothing changed. Would be: %s.\n", report.String())
		return nil
	}
	c.Printf("Size presets: %s.\n", report.String())
	return nil
}

// jsonFiles ports the shared _json_files helper: a directory contributes
// every *.json below it (sorted, symlinks skipped); a missing path is a
// CommandError.
func jsonFiles(paths []string) ([]string, error) {
	var files []string
	for _, raw := range paths {
		st, err := os.Stat(raw)
		if err != nil {
			return nil, commandErrorf("%s: no such file or directory.", raw)
		}
		if !st.IsDir() {
			files = append(files, raw)
			continue
		}
		var found []string
		_ = filepath.WalkDir(raw, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if filepath.Ext(path) == ".json" && d.Type()&fs.ModeSymlink == 0 {
				found = append(found, path)
			}
			return nil
		})
		sort.Strings(found)
		files = append(files, found...)
	}
	return files, nil
}
