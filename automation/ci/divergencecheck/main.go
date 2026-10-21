// Command divergencecheck enforces the divergence ledger contract:
//
//  1. every record in docs/parity/DIVERGENCES.md has a unique global id
//     D<N>, a SCRUTINIZED line, a status, and a remove-when condition;
//  2. docs/parity/DIVERGENCE-IDS.txt lists exactly the ids in the ledger,
//     sorted and unique;
//  3. every '// doory divergence (D<N>):' marker in Go sources references a
//     known id.
//
// This is the consistency half of the ratchet; the guard half (scanning for
// unrecorded workarounds) lands with the divguard baseline in M1.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"doory-go/internal/parity"
)

var (
	recordHead = regexp.MustCompile(`^## (D\d+) — (.+)$`)
	marker     = regexp.MustCompile(`//\s*doory divergence \(D(\d+)\):`)
)

type record struct {
	id, title                       string
	scrutinized, status, removeWhen string
}

func main() {
	root, err := parity.RepoRoot()
	if err != nil {
		fatal(err)
	}
	var problems []string

	// 1. Parse ledger records.
	data, err := os.ReadFile(filepath.Join(root, "docs", "parity", "DIVERGENCES.md"))
	if err != nil {
		fatal(err)
	}
	var records []record
	var cur *record
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := recordHead.FindStringSubmatch(line); m != nil {
			cur = &record{id: m[1], title: m[2]}
			if seen[cur.id] {
				problems = append(problems, "duplicate divergence id: "+cur.id)
			}
			seen[cur.id] = true
			records = append(records, *cur)
			continue
		}
		if cur == nil {
			continue
		}
		// Update the last record in place.
		cur = &records[len(records)-1]
		switch {
		case strings.HasPrefix(line, "- SCRUTINIZED:"):
			cur.scrutinized = strings.TrimSpace(strings.TrimPrefix(line, "- SCRUTINIZED:"))
		case strings.HasPrefix(line, "- status:"):
			cur.status = strings.TrimSpace(strings.TrimPrefix(line, "- status:"))
		case strings.HasPrefix(line, "- remove-when:"):
			cur.removeWhen = strings.TrimSpace(strings.TrimPrefix(line, "- remove-when:"))
		}
	}
	for _, r := range records {
		if r.scrutinized == "" {
			problems = append(problems, r.id+": missing SCRUTINIZED line")
		}
		if r.status != "active" && r.status != "retired" {
			problems = append(problems, fmt.Sprintf("%s: status %q must be active|retired", r.id, r.status))
		}
		if r.removeWhen == "" {
			problems = append(problems, r.id+": missing remove-when condition")
		}
	}

	// 2. IDS file must match the ledger exactly.
	idsData, err := os.ReadFile(filepath.Join(root, "docs", "parity", "DIVERGENCE-IDS.txt"))
	if err != nil {
		fatal(err)
	}
	var ids []string
	for _, line := range strings.Split(string(idsData), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ids = append(ids, line)
		}
	}
	// Ids are global and numeric: D1 < D2 < ... < D10 (lexicographic order
	// would put D10 between D1 and D2).
	last := -1
	for _, id := range ids {
		n, ok := parseD(id)
		if !ok {
			problems = append(problems, "DIVERGENCE-IDS.txt: malformed id "+id)
			continue
		}
		if n <= last {
			problems = append(problems, "DIVERGENCE-IDS.txt: ids not in strictly increasing numeric order at "+id)
		}
		last = n
	}
	if len(ids) != len(records) {
		problems = append(problems, fmt.Sprintf("id count mismatch: %d ids vs %d records", len(ids), len(records)))
	}
	for i, r := range records {
		if i >= len(ids) || ids[i] != r.id {
			problems = append(problems, "DIVERGENCE-IDS.txt out of sync with ledger at "+r.id)
			break
		}
	}

	// 3. Call-site markers must reference known ids.
	markFiles := 0
	markCount := 0
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
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		ms := marker.FindAllStringSubmatch(string(b), -1)
		if len(ms) > 0 {
			markFiles++
			markCount += len(ms)
			for _, m := range ms {
				id := "D" + strconv.Itoa(toInt(m[1]))
				if !seen[id] {
					problems = append(problems, rel+": marker references unknown divergence "+id)
				}
			}
		}
		return nil
	})

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "divergence-consistency:", p)
		}
		fmt.Fprintf(os.Stderr, "divergence-consistency: %d problem(s)\n", len(problems))
		os.Exit(1)
	}
	fmt.Printf("divergence-consistency: ok (%d records, %d call-site markers in %d files)\n",
		len(records), markCount, markFiles)
}

func toInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// parseD parses a "D<N>" id into N.
func parseD(id string) (int, bool) {
	if !strings.HasPrefix(id, "D") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "divergence-consistency:", err)
	os.Exit(1)
}
