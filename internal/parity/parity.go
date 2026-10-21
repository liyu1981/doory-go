// Package parity holds the shared machinery for the file denominator:
// which upstream files are tracked, how PORT_MAP.md rows are parsed, and
// what counts as coverage. Used by portmapgen, portmapdrift, lintscenarios
// and the future familygaps report.
package parity

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RepoRoot walks up from the working directory until it finds the repo root
// (the directory holding both go.mod and .upstream/current).
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if hasFile(dir, "go.mod") && isDir(filepath.Join(dir, ".upstream", "current")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repo root (go.mod + .upstream/current) not found above %s", dir)
		}
		dir = parent
	}
}

func hasFile(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !st.IsDir()
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// TrackedBackendFiles lists the upstream backend source files that form the
// port denominator, as mirror-root-relative paths (backend/...).
//
// Tracked: every *.py except __init__.py (Go has no package barrels; noted as
// n/a in the map), every *.html template, every *.json under backend/data
// (seeded content whose digest is part of ops parity), and the ops files the
// deploy/env contract lives in (Dockerfile, entrypoint, env example,
// requirements pins).
func TrackedBackendFiles(mirrorRoot string) ([]string, error) {
	var files []string
	backendRoot := filepath.Join(mirrorRoot, "backend")
	opsFiles := map[string]bool{
		"backend/Dockerfile":           true,
		"backend/docker-entrypoint.sh": true,
		"backend/.env.example":         true,
		"backend/requirements.txt":     true,
		"backend/requirements-dev.txt": true,
	}
	err := filepath.WalkDir(backendRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == "__pycache__" || name == ".venv" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(mirrorRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case opsFiles[rel]:
			files = append(files, rel)
		case strings.HasSuffix(name, ".py"):
			if name != "__init__.py" {
				files = append(files, rel)
			}
		case strings.HasSuffix(name, ".html"):
			files = append(files, rel)
		case strings.HasSuffix(name, ".json") && strings.HasPrefix(rel, "backend/data/"):
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// PortMapRow is one table row of PORT_MAP.md.
type PortMapRow struct {
	Upstream string // backend/... path; trailing slash means directory prefix
	Go       string
	Status   string
}

// ValidStatuses is the status vocabulary (PiG's set).
var ValidStatuses = map[string]bool{
	"✅": true, "🟡": true, "⬜": true, "⏸": true, "🔴": true, "n/a": true,
}

// ParsePortMap parses every markdown table row of the form
// | `upstream` | go | status | out of the file.
func ParsePortMap(path string) ([]PortMapRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []PortMapRow
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		// | `upstream` | go | status |  -> ["", " `upstream` ", " go ", " status ", ""]
		if len(cells) != 5 {
			return nil, fmt.Errorf("%s:%d: expected 3 cells, got %d", path, i+1, len(cells)-2)
		}
		up := strings.TrimSpace(cells[1])
		if !strings.HasPrefix(up, "`") || !strings.HasSuffix(up, "`") {
			continue // separator or header row
		}
		rows = append(rows, PortMapRow{
			Upstream: strings.Trim(up, "`"),
			Go:       strings.TrimSpace(cells[2]),
			Status:   strings.TrimSpace(cells[3]),
		})
	}
	return rows, nil
}

// Covers reports whether a PORT_MAP row covers a tracked file: an exact path
// match, or a directory-prefix row (trailing slash) containing the file.
func (r PortMapRow) Covers(file string) bool {
	if strings.HasSuffix(r.Upstream, "/") {
		return strings.HasPrefix(file, r.Upstream)
	}
	return r.Upstream == file
}

// CoveredBy returns the rows covering a tracked file.
func CoveredBy(rows []PortMapRow, file string) []PortMapRow {
	var out []PortMapRow
	for _, r := range rows {
		if r.Covers(file) {
			out = append(out, r)
		}
	}
	return out
}

// RowExists reports whether a row's upstream path resolves in the mirror:
// a file, or a directory prefix containing at least one tracked file.
func (r PortMapRow) RowExists(mirrorRoot string, tracked []string) bool {
	if strings.HasSuffix(r.Upstream, "/") {
		for _, f := range tracked {
			if strings.HasPrefix(f, r.Upstream) {
				return true
			}
		}
		return false
	}
	return isDir(mirrorRoot) && hasFile(mirrorRoot, r.Upstream)
}
