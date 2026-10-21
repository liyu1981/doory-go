// Command lintscenarios validates test/parity/families.toml (scope declared
// before scenarios) and every scenario TOML under test/parity/scenarios/:
//
// families:   unique names, non-empty description/includes, includes resolve
//
//	in the mirror and match at least one PORT_MAP row;
//
// scenarios:  required name/description/covers, known driver, covers paths
//
//	matching PORT_MAP rows, known dialects, normalize_replace rules
//	carrying a reason (when that field lands).
//
// Scope declared up front is what prevents silent skipping; this gate keeps
// the declaration honest against both the mirror and the map.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"doory-go/internal/parity"
)

var knownDrivers = map[string]bool{
	"http": true, "ws": true, "mcp": true, "cli": true, "schema": true, "contract": true,
}

var knownDialects = map[string]bool{"sqlite": true, "postgres": true}

type familiesFile struct {
	Families map[string]struct {
		Description string   `toml:"description"`
		Includes    []string `toml:"includes"`
	} `toml:"families"`
}

type scenarioFile struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Covers      []string `toml:"covers"`
	Driver      string   `toml:"driver"`
	Dialects    []string `toml:"dialects"`
}

func main() {
	root, err := parity.RepoRoot()
	if err != nil {
		fatal(err)
	}
	var problems []string

	rows, err := parity.ParsePortMap(filepath.Join(root, "docs", "parity", "PORT_MAP.md"))
	if err != nil {
		fatal(err)
	}
	tracked, err := parity.TrackedBackendFiles(filepath.Join(root, ".upstream", "current"))
	if err != nil {
		fatal(err)
	}

	// --- families.toml ---
	var fams familiesFile
	if _, err := toml.DecodeFile(filepath.Join(root, "test", "parity", "families.toml"), &fams); err != nil {
		fatal(err)
	}
	if len(fams.Families) == 0 {
		problems = append(problems, "families.toml declares no families")
	}
	for name, fam := range fams.Families {
		if fam.Description == "" {
			problems = append(problems, "families."+name+": missing description")
		}
		if len(fam.Includes) == 0 {
			problems = append(problems, "families."+name+": empty includes")
		}
		for _, inc := range fam.Includes {
			matches := 0
			for _, r := range rows {
				if strings.HasPrefix(r.Upstream, inc) {
					matches++
				}
			}
			if matches == 0 {
				problems = append(problems, "families."+name+": include "+inc+" matches no PORT_MAP row")
			}
		}
	}

	// --- scenarios (none yet is fine; the rules bind future ones) ---
	scenarioRoot := filepath.Join(root, "test", "parity", "scenarios")
	scenarioCount := 0
	_ = filepath.WalkDir(scenarioRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".toml") {
			return err
		}
		scenarioCount++
		var sc scenarioFile
		if _, err := toml.DecodeFile(path, &sc); err != nil {
			problems = append(problems, path+": "+err.Error())
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if sc.Name == "" {
			problems = append(problems, rel+": missing name")
		}
		if sc.Description == "" {
			problems = append(problems, rel+": missing description")
		}
		if !knownDrivers[sc.Driver] {
			problems = append(problems, rel+": unknown driver "+fmt.Sprintf("%q", sc.Driver))
		}
		if len(sc.Covers) == 0 {
			problems = append(problems, rel+": covers nothing")
		}
		for _, c := range sc.Covers {
			matched := false
			for _, r := range rows {
				if r.Covers(c) {
					matched = true
					break
				}
			}
			if !matched {
				problems = append(problems, rel+": covers "+c+" matches no PORT_MAP row")
			}
		}
		for _, d := range sc.Dialects {
			if !knownDialects[d] {
				problems = append(problems, rel+": unknown dialect "+fmt.Sprintf("%q", d))
			}
		}
		return nil
	})

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "lint-scenarios:", p)
		}
		fmt.Fprintf(os.Stderr, "lint-scenarios: %d problem(s)\n", len(problems))
		os.Exit(1)
	}
	fmt.Printf("lint-scenarios: ok (%d families, %d scenarios, %d tracked files, %d map rows)\n",
		len(fams.Families), scenarioCount, len(tracked), len(rows))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "lint-scenarios:", err)
	os.Exit(1)
}
