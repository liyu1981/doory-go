// The paired-scenario runner: boots the pinned oracle and the Go port in
// isolation, replays identical request sequences against both, and compares
// with the strongest stable comparator (exact status + headers + body bytes,
// with a small set of recorded normalization rules — PLAN.md §4).
package runner

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Scenario is one paired scenario TOML.
type Scenario struct {
	Name        string            `toml:"name"`
	Description string            `toml:"description"`
	Driver      string            `toml:"driver"`
	Tags        []string          `toml:"tags"`
	Covers      []string          `toml:"covers"`
	Dialects    []string          `toml:"dialects"`
	Env         map[string]string `toml:"env"` // applied to BOTH sides
	Steps       []Step            `toml:"steps"`
}

// Step is one request replayed against both sides.
type Step struct {
	Method  string            `toml:"method"`
	Path    string            `toml:"path"`
	Headers map[string]string `toml:"headers"`
	Body    string            `toml:"body"`
	// CookieStable asserts the side keeps its own cookie value across this
	// step (Django renews csrf expiry with the same secret; see get_token's
	// CSRF_COOKIE_NEEDS_UPDATE). Checked per side, never across sides:
	// values are per-request randomness (shape rules cover the format).
	CookieStable bool `toml:"cookie_stable"`
}

// Load parses a scenario file.
func Load(path string) (*Scenario, error) {
	var sc Scenario
	if _, err := toml.DecodeFile(path, &sc); err != nil {
		return nil, err
	}
	if sc.Name == "" || sc.Driver == "" {
		return nil, fmt.Errorf("%s: missing name/driver", path)
	}
	return &sc, nil
}

// Discover expands files and directories into scenario paths (sorted).
func Discover(paths []string, fastOnly bool) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		err = filepath.Walk(p, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !fi.IsDir() && filepath.Ext(path) == ".toml" {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if fastOnly {
		var kept []string
		for _, p := range out {
			sc, err := Load(p)
			if err != nil {
				return nil, err
			}
			if hasTag(sc, "fast") {
				kept = append(kept, p)
			}
		}
		out = kept
	}
	return out, nil
}

func hasTag(sc *Scenario, tag string) bool {
	for _, t := range sc.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
