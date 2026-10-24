// The element content commands (M13): import_elements, sync_element_packs and
// seed_elements.
//
//   import_elements      import every *.svg below a directory into a pack
//   sync_element_packs   install the packs listed in backend/data/element_packs.json
//                        (npm tarballs and pinned files verified against the
//                        manifest, plus Doory's generated packs)
//   seed_elements        generate and import Doory's own artwork (the shapes of
//                        generate_shapes.py and the graphics sets)

package doory

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// elementBuiltinPacks is seed.BUILTIN_PACKS: (slug, name, kind, description).
var elementBuiltinPacks = [][4]string{
	{"basic-shapes", "Basic Shapes", "shape",
		"Geometric shapes: polygons, stars, hearts, blobs, waves, badges."},
	{"lines-dividers", "Lines & Dividers", "line",
		"Lines, arrows, dividers, flourishes and brackets."},
	{"frames-masks", "Frames & Masks", "frame", "Photo masks and decorative frames."},
	{"arrows-callouts", "Arrows & Callouts", "shape",
		"Arrows, chevrons, speech bubbles and banners."},
}

// runImportElements ports apps/elements/management/commands/import_elements.py.
func runImportElements(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "pack", kind: "string"},
		{name: "name", kind: "string"},
		{name: "kind", kind: "string", choices: elementKindValues},
		{name: "license", kind: "string"},
		{name: "source-url", kind: "string", dest: "source_url"},
	})
	if err != nil {
		return err
	}
	if len(args.positionals) < 1 {
		return &usageError{message: "the following arguments are required: directory"}
	}
	directory := args.positionals[0]
	st, serr := os.Stat(directory)
	if serr != nil || !st.IsDir() {
		return commandErrorf("%s: not a directory.", directory)
	}
	packSlug := args.str("pack")
	if packSlug == "" {
		return &usageError{message: "the following arguments are required: --pack"}
	}
	name := args.str("name")
	if name == "" {
		name = elementName(packSlug)
	}
	kind := args.str("kind")
	licenseName := args.str("license")
	packID, created, perr := c.App.ensureElementPack(packSlug, name, kind, licenseName,
		args.str("source_url"), "", 0, "", "")
	if perr != nil {
		return commandErrorf("%v", perr)
	}
	if created {
		c.Printf("Created pack \"%s\" (%s).\n", name, packSlug)
	} else {
		c.Printf("Using pack \"%s\" (%s).\n", name, packSlug)
	}
	summary, ierr := c.App.importElementDirectory(packID, packSlug, directory, kind,
		func(outcome, detail string) {
			switch outcome {
			case "skipped":
				c.Errorf("skipped  %s\n", detail)
			default:
				c.Printf("%-9s%s\n", outcome, detail)
			}
		})
	if ierr != nil {
		return commandErrorf("%v", ierr)
	}
	c.Printf("Done: %s.\n", summary)
	return nil
}

// importElementDirectory is importer.import_directory: every *.svg below the
// directory, sorted; invalid files are skipped, never fatal.
func (a *App) importElementDirectory(packID, packSlug, directory, kind string,
	progress func(outcome, detail string),
) (elementImportSummary, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return elementImportSummary{}, err
	}
	var files []string
	walkErr := filepath.WalkDir(root, func(current string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".svg") && d.Type()&fs.ModeSymlink == 0 {
			files = append(files, current)
		}
		return nil
	})
	if walkErr != nil {
		return elementImportSummary{}, walkErr
	}
	sort.Strings(files)
	var summary elementImportSummary
	type pending struct {
		key   string
		name  string
		tags  []string
		data  []byte
		order int
	}
	var entries []elementImportEntry
	for order, path := range files {
		rel, rerr := filepath.Rel(root, path)
		label := filepath.ToSlash(rel)
		if rerr != nil {
			continue
		}
		info, ierr := os.Stat(path)
		if ierr != nil || !info.Mode().IsRegular() {
			summary.Skipped = append(summary.Skipped, [2]string{label, "not a regular file"})
			if progress != nil {
				progress("skipped", label+": not a regular file")
			}
			continue
		}
		if info.Size() > elementMaxSvgBytes {
			summary.Skipped = append(summary.Skipped, [2]string{label, "the file is larger than 5 MB"})
			if progress != nil {
				progress("skipped", label+": the file is larger than 5 MB")
			}
			continue
		}
		key, kerr := elementKey(filepath.ToSlash(rel))
		if kerr != nil {
			summary.Skipped = append(summary.Skipped, [2]string{label, kerr.Error()})
			if progress != nil {
				progress("skipped", label+": "+kerr.Error())
			}
			continue
		}
		data, derr := os.ReadFile(path)
		if derr != nil {
			summary.Skipped = append(summary.Skipped, [2]string{label, derr.Error()})
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		name := elementName(stem)
		directories := strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/")
		if len(directories) == 1 && directories[0] == "." {
			directories = nil
		}
		entries = append(entries, elementImportEntry{
			Key: key, Label: label, Name: name, SVG: data, Kind: kind,
			Tags: elementTagsFrom(directories, name), SortOrder: order * 10,
		})
	}
	if len(entries) > 0 {
		outcomes := map[string]string{}
		imported, berr := a.bulkImportWithOutcomes(packID, packSlug, entries, kind, false, nil, outcomes)
		if berr != nil {
			return summary, berr
		}
		summary.Created += imported.Created
		summary.Updated += imported.Updated
		summary.Unchanged += imported.Unchanged
		summary.Skipped = append(summary.Skipped, imported.Skipped...)
		if progress != nil {
			// One line per file, in file order, like import_directory.
			for _, entry := range entries {
				outcome, ok := outcomes[entry.Key]
				if !ok {
					continue
				}
				progress(outcome, entry.Label)
			}
		}
	}
	return summary, nil
}

// runSyncElementPacks ports apps/elements/management/commands/sync_element_packs.py.
func runSyncElementPacks(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "pack", kind: "list"},
		{name: "list", kind: "bool"},
		{name: "dry-run", kind: "bool", dest: "dry_run"},
		{name: "force", kind: "bool"},
		{name: "prune", kind: "bool"},
		{name: "brand-logos", kind: "bool", dest: "brand_logos"},
		{name: "no-brand-logos", kind: "bool", dest: "no_brand_logos"},
		{name: "concurrency", kind: "int"},
		{name: "from-dir", kind: "string", dest: "from_dir"},
	})
	if err != nil {
		return err
	}
	manifest, merr := loadElementManifest()
	if merr != nil {
		return commandErrorf("%v", merr)
	}
	packs, serr := manifest.selectPacks(args.list("pack"))
	if serr != nil {
		return commandErrorf("%v", serr)
	}
	brandLogos := envBool("DOORY_ELEMENTS_BRAND_LOGOS", false)
	if args.has("brand_logos") {
		brandLogos = args.bool("brand_logos")
	}
	if args.has("no_brand_logos") {
		brandLogos = false
	}
	if args.bool("list") {
		return listElementPacks(c, packs, brandLogos)
	}
	concurrency := args.intOption("concurrency", 4)
	if args.has("concurrency") && concurrency < 1 {
		return commandErrorf("--concurrency needs a positive number.")
	}
	if concurrency > 8 {
		concurrency = 8
	}
	var source packSource
	if fromDir := args.str("from_dir"); fromDir != "" {
		st, terr := os.Stat(fromDir)
		if terr != nil || !st.IsDir() {
			return commandErrorf("%s: not a directory.", fromDir)
		}
		source = directoryPackSource{dir: fromDir}
	} else {
		source = newHTTPPackSource()
	}
	dryRun := args.bool("dry_run")
	line := fmt.Sprintf("Syncing %d pack(s) (%d at a time)", len(packs), concurrency)
	if brandLogos {
		line += ", with brand logos"
	}
	if dryRun {
		line += " - dry run, nothing is written"
	}
	c.Println(line + ".")
	report := c.App.syncElementPacks(packs, source, elementSyncOptions{
		Concurrency: concurrency, Force: args.bool("force"), DryRun: dryRun,
		Prune: args.bool("prune"), BrandLogos: brandLogos,
	}, c.Println)
	created, updated, unchanged, skipped, current, ok := 0, 0, 0, 0, 0, 0
	for _, result := range report.Results {
		created += result.Created
		updated += result.Updated
		unchanged += result.Unchanged
		skipped += len(result.Skipped)
		if result.Status == "up-to-date" {
			current++
		}
		if result.ok() {
			ok++
		}
	}
	wouldBe := ""
	if dryRun {
		wouldBe = "would be "
	}
	summary := fmt.Sprintf("Done: %d of %d pack(s) ok (%d up to date); %d elements %screated, %d updated, "+
		"%d unchanged, %d files skipped.", ok, len(report.Results), current, created, wouldBe, updated,
		unchanged, skipped)
	c.Println(summary)
	if report.Aborted != "" {
		return commandErrorf("%s. Check your network or proxy settings (HTTPS_PROXY), or download the "+
			"releases on a machine with internet access and import them with --from-dir DIR (see "+
			"THIRD_PARTY.md). Packs that finished are kept; run the command again to resume.", report.Aborted)
	}
	if failed := report.failed(); len(failed) > 0 {
		ids := make([]string, 0, len(failed))
		for _, result := range failed {
			ids = append(ids, result.PackID)
		}
		return commandErrorf("%d pack(s) failed: %s. Packs that finished are kept; run the command again "+
			"to retry the others.", len(failed), strings.Join(ids, ", "))
	}
	return nil
}

// listElementPacks is Command._list.
func listElementPacks(c *CLI, packs []elementPackSpec, brandLogos bool) error {
	width := 0
	for _, pack := range packs {
		if len(pack.ID) > width {
			width = len(pack.ID)
		}
	}
	for _, pack := range packs {
		row, err := c.App.loadElementPack(pack.ID)
		state := "not installed"
		if err != nil {
			return commandErrorf("%v", err)
		}
		if row != nil {
			var count int
			_ = c.App.DB.QueryRow(`SELECT COUNT(*) FROM elements_element WHERE pack_id = ?`,
				row.ID).Scan(&count)
			state = fmt.Sprintf("%d elements", count)
			if row.SyncFingerprint != pack.fingerprintFor(brandLogos) {
				state += ", out of date"
			}
		}
		style := pack.Style
		if style == "" {
			style = "-"
		}
		c.Printf("%-*s  %-12s %-8s %-14s %s  [%s]\n", width, pack.ID, pack.Kind, style,
			pack.License.SPDX, pack.sourceRef(), state)
	}
	return nil
}

// runSeedElements ports apps/elements/management/commands/seed_elements.py.
func runSeedElements(c *CLI, argv []string) error {
	if _, err := parseArgs(argv, nil); err != nil {
		return err
	}
	summaries := map[string]elementImportSummary{}
	names := map[string]string{}
	for index, pack := range elementBuiltinPacks {
		slug, name, kind, description := pack[0], pack[1], pack[2], pack[3]
		names[slug] = name
		packID, _, perr := c.App.ensureElementPack(slug, name, kind, "CC0-1.0", "", description,
			(index+1)*10, "", "")
		if perr != nil {
			return commandErrorf("%v", perr)
		}
		files, ferr := generatedShapePackFiles(slug)
		if ferr != nil {
			return commandErrorf("%v", ferr)
		}
		entries := generatedShapeEntries(files)
		summary, ierr := c.App.bulkImport(packID, slug, entries, kind, false, nil)
		if ierr != nil {
			return commandErrorf("%v", ierr)
		}
		summaries[slug] = summary
	}
	total := 0
	for _, pack := range elementBuiltinPacks {
		slug := pack[0]
		summary := summaries[slug]
		c.Printf("%s: %s\n", names[slug], summary)
		for _, skip := range summary.Skipped {
			c.Errorf("  skipped %s: %s\n", skip[0], skip[1])
		}
		total += summary.Created + summary.Updated + summary.Unchanged
	}
	c.Printf("Shapes: %d elements in %d packs.\n", total, len(summaries))

	// The graphics packs come from the same manifest as sync_element_packs.
	manifest, merr := loadElementManifest()
	if merr != nil {
		return commandErrorf("%v", merr)
	}
	var generated []elementPackSpec
	for _, pack := range manifest.Packs {
		if pack.isGenerated() {
			generated = append(generated, pack)
		}
	}
	report := c.App.syncElementPacks(generated, noPackSource{}, elementSyncOptions{}, c.Println)
	if failed := report.failed(); len(failed) > 0 {
		return commandErrorf("%d graphics pack(s) failed: %s", len(failed), failed[0].Error)
	}
	graphics := 0
	for _, result := range report.Results {
		graphics += result.total()
	}
	c.Printf("Done: %d elements in %d packs.\n", total+graphics, len(summaries)+len(report.Results))
	return nil
}

// generatedShapeEntries is importer.import_directory over the pinned artwork:
// one entry per file in sorted order, `sort_order = index * 10`.
func generatedShapeEntries(files map[string][]byte) []elementImportEntry {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]elementImportEntry, 0, len(paths))
	for order, path := range paths {
		key, err := elementKey(path)
		if err != nil {
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		name := elementName(stem)
		directories := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
		if len(directories) == 1 && directories[0] == "." {
			directories = nil
		}
		entries = append(entries, elementImportEntry{
			Key: key, Name: name, SVG: files[path], Tags: elementTagsFrom(directories, name),
			SortOrder: order * 10,
		})
	}
	return entries
}

var _ = regexp.MustCompile
