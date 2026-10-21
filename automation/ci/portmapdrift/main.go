// Command portmapdrift rejects inconsistencies between the tracked upstream
// denominator and docs/parity/PORT_MAP.md:
//
//  1. a tracked upstream file covered by no row;
//  2. a row whose upstream path does not resolve in the mirror;
//  3. a row with a status outside the vocabulary;
//  4. duplicate rows;
//  5. a Go source claiming '// Ports backend/...' without a matching row.
//
// This is a review-obligation gate, never an acceptance gate: it forces the
// map to describe the mirror, not the other way round.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"doory-go/internal/parity"
)

var portsClaim = regexp.MustCompile(`//\s*Ports\s+(backend/[^\s*]*)`)

func main() {
	root, err := parity.RepoRoot()
	if err != nil {
		fatal(err)
	}
	mirror := filepath.Join(root, ".upstream", "current")
	tracked, err := parity.TrackedBackendFiles(mirror)
	if err != nil {
		fatal(err)
	}
	rows, err := parity.ParsePortMap(filepath.Join(root, "docs", "parity", "PORT_MAP.md"))
	if err != nil {
		fatal(err)
	}

	var problems []string

	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.Upstream] {
			problems = append(problems, "duplicate row: "+r.Upstream)
		}
		seen[r.Upstream] = true
		if !parity.ValidStatuses[r.Status] {
			problems = append(problems, fmt.Sprintf("row %s: status %q outside vocabulary", r.Upstream, r.Status))
		}
		if !r.RowExists(mirror, tracked) {
			problems = append(problems, "row upstream path missing from mirror: "+r.Upstream)
		}
	}

	for _, f := range tracked {
		if len(parity.CoveredBy(rows, f)) == 0 {
			problems = append(problems, "tracked file with no PORT_MAP row: "+f)
		}
	}

	// Go port claims must name a row that covers the claimed path.
	var goFiles []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch rel {
			case ".upstream", ".oracle", "node_modules", "dist":
				return filepath.SkipDir
			}
			// Port claims live in port code only; tooling may mention the
			// convention in doc comments without making a claim.
			if rel == "automation" || strings.HasPrefix(rel, "test/") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})
	sort.Strings(goFiles)
	for _, gf := range goFiles {
		data, err := os.ReadFile(gf)
		if err != nil {
			continue
		}
		for _, m := range portsClaim.FindAllStringSubmatch(string(data), -1) {
			claimed := strings.TrimSuffix(m[1], ".")
			rel, _ := filepath.Rel(root, gf)
			matched := false
			for _, r := range rows {
				if r.Covers(claimed) {
					matched = true
					break
				}
			}
			if !matched {
				problems = append(problems, fmt.Sprintf("%s: '// Ports %s' has no PORT_MAP row", rel, claimed))
			}
		}
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "port-map-drift:", p)
		}
		fmt.Fprintf(os.Stderr, "port-map-drift: %d problem(s)\n", len(problems))
		os.Exit(1)
	}
	fmt.Printf("port-map-drift: ok (%d rows, %d tracked files, %d go sources checked)\n", len(rows), len(tracked), len(goFiles))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "port-map-drift:", err)
	os.Exit(1)
}
