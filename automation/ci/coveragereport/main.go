// Command coveragereport builds the coverage table embedded in AGENTS.md.
//
// Inputs (all generated/reviewed artifacts, never hand-maintained):
//   - docs/parity/PORT_MAP.md          every tracked backend file with its
//     review status and the Go symbol(s);
//   - test/parity/scenarios/**/*.toml  the scenarios and the files they cover
//     (`covers = [...]`);
//   - ledger/test-mapping-*.json       per-upstream-test-file dispositions.
//
// The report counts a family row as "covered" only when a scenario COVERS the
// file path; rows marked reviewed (✅/🟡) without scenario coverage are listed
// as weak (review-only), and untested files stay visibly uncovered. The table
// is written between the COVERAGE markers in AGENTS.md.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"doory-go/internal/parity"
)

const (
	startMarker = "<!-- COVERAGE:START -->"
	endMarker   = "<!-- COVERAGE:END -->"
)

// scenarioCoverage is one scenario's declared scope.
type scenarioCoverage struct {
	Name   string
	File   string
	Tags   []string
	Covers []string
}

// readScenarios parses the `name`, `tags` and `covers` fields of every
// scenario TOML (the shapes are fixed, so no TOML dependency is needed).
func readScenarios(root string) ([]scenarioCoverage, error) {
	var out []scenarioCoverage
	dir := filepath.Join(root, "test", "parity", "scenarios")
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".toml" {
			return err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scenario := scenarioCoverage{File: path}
		inCovers := false
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "name = ") && scenario.Name == "" {
				scenario.Name = strings.Trim(strings.TrimPrefix(trimmed, "name = "), `"`)
			}
			if strings.HasPrefix(trimmed, "tags = ") {
				for _, part := range strings.Split(strings.Trim(trimmed[len("tags = "):], "[]"), ",") {
					tag := strings.Trim(strings.TrimSpace(part), `"`)
					if tag != "" {
						scenario.Tags = append(scenario.Tags, tag)
					}
				}
			}
			if strings.HasPrefix(trimmed, "covers = [") {
				inCovers = !strings.Contains(trimmed, "]")
				scenario.Covers = append(scenario.Covers, splitTOMLStrings(trimmed[len("covers = ["):])...)
				continue
			}
			if inCovers {
				scenario.Covers = append(scenario.Covers, splitTOMLStrings(trimmed)...)
				if strings.Contains(trimmed, "]") {
					inCovers = false
				}
			}
		}
		out = append(out, scenario)
		return nil
	})
	return out, err
}

// splitTOMLStrings picks the quoted strings out of one TOML array fragment.
func splitTOMLStrings(fragment string) []string {
	var out []string
	parts := strings.Split(fragment, `"`)
	for i := 1; i < len(parts); i += 2 {
		if value := strings.TrimSpace(parts[i]); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// coversPath reports whether a scenario's `covers` entry names the file
// (exact path or a directory prefix with a trailing slash).
func coversPath(covers []string, upstream string) bool {
	for _, cover := range covers {
		if strings.HasSuffix(cover, "/") {
			if strings.HasPrefix(upstream, cover) {
				return true
			}
			continue
		}
		if cover == upstream {
			return true
		}
	}
	return false
}

// dispositions counts the per-test-file disposition state per family.
func dispositions(root string) (map[string][3]int, error) {
	matches, err := filepath.Glob(filepath.Join(root, "ledger", "test-mapping-*.json"))
	if err != nil || len(matches) == 0 {
		return nil, err
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		return nil, err
	}
	var doc struct {
		Families map[string]map[string]struct {
			Disposition string `json:"disposition"`
		} `json:"families"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := map[string][3]int{}
	for family, files := range doc.Families {
		var counts [3]int // ported, partial, pending
		for _, entry := range files {
			switch entry.Disposition {
			case "ported":
				counts[0]++
			case "partial":
				counts[1]++
			default:
				counts[2]++
			}
		}
		out[family] = counts
	}
	return out, nil
}

// familyOf maps a PORT_MAP row to its family by the first path segment after
// backend/apps/ (or "kernel"/"ops" for the root files).
func familyOf(upstream string) string {
	parts := strings.Split(strings.TrimPrefix(upstream, "backend/"), "/")
	if len(parts) > 1 && parts[0] == "apps" {
		return parts[1]
	}
	switch parts[0] {
	case "doory", "manage.py", "conftest.py", "healthcheck.py", "tests", "requirements.txt",
		"requirements-dev.txt":
		return "kernel"
	}
	return "ops"
}

func main() {
	root, err := parity.RepoRoot()
	if err != nil {
		fatal(err)
	}
	rows, err := parity.ParsePortMap(filepath.Join(root, "docs", "parity", "PORT_MAP.md"))
	if err != nil {
		fatal(err)
	}
	scenarios, err := readScenarios(root)
	if err != nil {
		fatal(err)
	}
	dispositionsByFamily, err := dispositions(root)
	if err != nil {
		fatal(err)
	}

	type familyStats struct{ rows, covered, reviewed, uncovered int }
	stats := map[string]*familyStats{}
	weak := map[string]bool{}
	for _, row := range rows {
		family := familyOf(row.Upstream)
		entry := stats[family]
		if entry == nil {
			entry = &familyStats{}
			stats[family] = entry
		}
		entry.rows++
		reviewed := row.Status == "✅" || row.Status == "🟡"
		if reviewed {
			entry.reviewed++
		}
		isCovered := false
		for _, scenario := range scenarios {
			if coversPath(scenario.Covers, row.Upstream) {
				isCovered = true
				break
			}
		}
		if isCovered {
			entry.covered++
			continue
		}
		entry.uncovered++
		if reviewed {
			weak[row.Upstream] = true
		}
	}

	families := make([]string, 0, len(stats))
	for family := range stats {
		families = append(families, family)
	}
	sort.Strings(families)

	var b strings.Builder
	b.WriteString(startMarker + "\n")
	b.WriteString("<!-- generated by `pnpm coverage` from PORT_MAP.md, the scenarios' `covers` and the\n")
	b.WriteString("     test-mapping ledger; never hand-edit -->\n\n")
	b.WriteString("| family | rows | scenario-covered | reviewed | uncovered | test files (ported/partial/pending) |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, family := range families {
		entry := stats[family]
		counts := dispositionsByFamily[family]
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %d | %d | %d/%d/%d |\n", family, entry.rows,
			entry.covered, entry.reviewed, entry.uncovered, counts[0], counts[1], counts[2]))
	}
	covered, reviewed, total := 0, 0, 0
	for _, entry := range stats {
		covered += entry.covered
		reviewed += entry.reviewed
		total += entry.rows
	}
	percentage := 0.0
	if total > 0 {
		percentage = 100 * float64(covered) / float64(total)
	}
	b.WriteString(fmt.Sprintf("\n**Totals:** %d of %d PORT_MAP rows (%.0f%%) are covered by at least one "+
		"scenario; %d reviewed rows have no scenario that covers them (review-only: they do NOT count "+
		"as verified).\n", covered, total, percentage, reviewed-covered))
	if len(weak) > 0 {
		paths := make([]string, 0, len(weak))
		for path := range weak {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		b.WriteString("\nReview-only rows (weak evidence, not counted as verified):\n\n")
		for _, path := range paths {
			b.WriteString("- `" + path + "`\n")
		}
	}
	b.WriteString(endMarker + "\n")

	agentsPath := filepath.Join(root, "AGENTS.md")
	agents, err := os.ReadFile(agentsPath)
	if err != nil {
		fatal(err)
	}
	text := string(agents)
	start := strings.Index(text, startMarker)
	end := strings.Index(text, endMarker)
	if start < 0 || end < start {
		text = strings.TrimRight(text, "\n") + "\n\n## Coverage report\n\n" + b.String()
	} else {
		text = text[:start] + b.String() + text[end+len(endMarker)+1:]
	}
	if err := os.WriteFile(agentsPath, []byte(text), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("coveragereport: %d rows, %d covered (%d review-only), %d families\n", total, covered,
		reviewed-covered, len(families))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "coveragereport:", err)
	os.Exit(1)
}
