// Package envconfig loads the optional .env file and implements the
// `doory config init|upgrade` commands.
//
// The binary reads configuration from the process environment (the upstream
// contract). On top of that it may fill variables that are *absent* from the
// process environment from a dotenv file: ~/.config/doory/.env by default, or
// a path passed with `--env-file`. Process environment variables always win.
//
// The canonical template is .env.example at the repository root; its copy in
// this directory is embedded into the binary (envconfig_test keeps the two
// files identical).
package envconfig

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Template is the embedded .env.example (the canonical env template).
//
//go:generate cp ../../.env.example env.example
//go:embed env.example
var Template string

// templateVersion is parsed from the `# template-version: N` marker.
var templateVersion = versionOf([]byte(Template))

// TemplateVersion is the version of the embedded template.
func TemplateVersion() int { return templateVersion }

var (
	versionMarker = regexp.MustCompile(`(?im)^\s*#\s*template-version\s*:\s*([0-9]+)\s*$`)
	keyPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// DefaultPath is the user-level env file: $XDG_CONFIG_HOME/doory/.env, or
// ~/.config/doory/.env when XDG_CONFIG_HOME is unset. It returns "" when no
// home directory can be determined.
func DefaultPath() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "doory", ".env")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "doory", ".env")
}

// ExpandPath expands a leading ~ to the user's home directory.
func ExpandPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// ResolvePath returns the explicit path when one was given, otherwise the
// default user path. The bool says whether the path was explicit (an explicit
// missing file is an error; a missing default file is not).
func ResolvePath(explicit string) (string, bool) {
	if path := strings.TrimSpace(explicit); path != "" {
		return ExpandPath(path), true
	}
	return DefaultPath(), false
}

// Load resolves and loads the env file, filling only variables that are absent
// from the process environment. It returns the path it read ("" when no path
// is available). A missing default file is silently ignored; a missing
// explicit path is an error.
func Load(explicit string) (string, error) {
	path, isExplicit := ResolvePath(explicit)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !isExplicit {
			return "", nil
		}
		return path, fmt.Errorf("read env file %s: %w", path, err)
	}
	for key, value := range parseEnv(data) {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return path, fmt.Errorf("set %s from env file %s: %w", key, path, err)
		}
	}
	// doory divergence (D23): upstream reads only the process environment;
	// the port loads an optional dotenv file without overriding it.
	return path, nil
}

// Init writes the embedded template as <dir>/.env. It refuses to overwrite an
// existing file unless force is true (in which case the old file is kept as
// .env.bak).
func Init(dir string, force bool) (string, error) {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	path := filepath.Join(ExpandPath(dir), ".env")
	if data, err := os.ReadFile(path); err == nil {
		if !force {
			return path, fmt.Errorf("%s already exists (use --force to overwrite)", path)
		}
		if err := os.WriteFile(path+".bak", data, 0o600); err != nil {
			return path, fmt.Errorf("write backup %s.bak: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return path, fmt.Errorf("check %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(Template), 0o600); err != nil {
		return path, fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// UpgradeResult reports what Upgrade changed.
type UpgradeResult struct {
	Path       string
	BackupPath string   // "" when the file was already up to date
	OldVersion int      // 0 when the file carries no template-version marker
	NewVersion int      // the embedded template version
	Changed    bool     // whether the file was rewritten
	Added      []string // active template keys the old file did not have
	Documented []string // commented template keys the old file did not have
	Preserved  []string // active old keys whose values were kept
	Extra      []string // active old keys not present in the template
}

// Upgrade merges the newest template into an existing .env file: every active
// value in the old file is kept, settings that are new in the template are
// added, and active settings that left the template are kept in a trailing
// section. The old file is backed up as .env.bak before a rewrite.
func Upgrade(path string) (*UpgradeResult, error) {
	path = ExpandPath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	merged, stats := merge(data, []byte(Template))
	result := &UpgradeResult{
		Path:       path,
		OldVersion: versionOf(data),
		NewVersion: templateVersion,
		Changed:    stats.changed,
		Added:      stats.added,
		Documented: stats.documented,
		Preserved:  stats.preserved,
		Extra:      stats.extra,
	}
	if !result.Changed {
		return result, nil
	}
	backup := path + ".bak"
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return nil, fmt.Errorf("write backup %s: %w", backup, err)
	}
	if err := os.WriteFile(path, []byte(merged), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	result.BackupPath = backup
	return result, nil
}

// ------------------------------------------------------------------ parsing --

type envLine struct {
	raw        string
	assignment bool
	key        string
	value      string // raw right-hand side (quotes and inline comments intact)
	commented  bool
}

// parseLines splits a dotenv file into lines, retaining the raw text for the
// template merge. CRLF is normalised to LF.
func parseLines(data []byte) []envLine {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	rawLines := strings.Split(text, "\n")
	lines := make([]envLine, 0, len(rawLines))
	for _, raw := range rawLines {
		line, _ := parseLine(raw)
		lines = append(lines, line)
	}
	return lines
}

func parseLine(raw string) (envLine, bool) {
	line := envLine{raw: raw}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return line, false
	}
	if strings.HasPrefix(trimmed, "#") {
		line.commented = true
		trimmed = strings.TrimSpace(trimmed[1:])
	}
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimSpace(trimmed[len("export "):])
	}
	eq := strings.IndexByte(trimmed, '=')
	if eq <= 0 {
		return line, false
	}
	key := strings.TrimSpace(trimmed[:eq])
	if !keyPattern.MatchString(key) {
		return line, false
	}
	line.assignment = true
	line.key = key
	line.value = strings.TrimSpace(trimmed[eq+1:])
	return line, true
}

// parseEnv returns the active assignments (last value wins).
func parseEnv(data []byte) map[string]string {
	values := map[string]string{}
	for _, line := range parseLines(data) {
		if !line.assignment || line.commented {
			continue
		}
		values[line.key] = parseValue(line.value)
	}
	return values
}

// parseValue decodes one right-hand side: quotes, escapes, and an unquoted
// inline comment introduced by whitespace + '#'.
func parseValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	switch raw[0] {
	case '"', '\'':
		if value, ok := unquote(raw, raw[0]); ok {
			return value
		}
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
			return strings.TrimSpace(raw[:i])
		}
	}
	return raw
}

func unquote(raw string, quote byte) (string, bool) {
	var out strings.Builder
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		if c == quote {
			return out.String(), true
		}
		if quote == '"' && c == '\\' && i+1 < len(raw) {
			i++
			switch raw[i] {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			default:
				out.WriteByte(raw[i])
			}
			continue
		}
		out.WriteByte(c)
	}
	return "", false
}

func versionOf(data []byte) int {
	match := versionMarker.FindSubmatch(data)
	if match == nil {
		return 0
	}
	version, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0
	}
	return version
}

// ---------------------------------------------------------------- merging --

type mergeStats struct {
	added      []string
	documented []string
	preserved  []string
	extra      []string
	changed    bool
}

func merge(oldData, templateData []byte) (string, mergeStats) {
	oldLines := parseLines(oldData)
	templateLines := parseLines(templateData)

	// Only active values are "configured"; a commented-out setting follows the
	// template (comments are documentation, not configuration). The full key
	// set still counts as "known" so an upgrade does not report every existing
	// commented placeholder as new.
	oldActive := map[string]envLine{}
	oldKnown := map[string]bool{}
	oldLast := map[string]int{}
	for i, line := range oldLines {
		if !line.assignment {
			continue
		}
		oldKnown[line.key] = true
		if !line.commented {
			oldActive[line.key] = line
			oldLast[line.key] = i
		}
	}
	templateKeys := map[string]bool{}
	for _, line := range templateLines {
		if line.assignment {
			templateKeys[line.key] = true
		}
	}

	var stats mergeStats
	seen := map[string]bool{}
	out := make([]string, 0, len(templateLines)+8)
	for _, line := range templateLines {
		if line.assignment {
			if old, ok := oldActive[line.key]; ok {
				// Keep the operator's line (value, quoting and inline comment).
				out = append(out, old.raw)
				stats.preserved = append(stats.preserved, line.key)
				seen[line.key] = true
				continue
			}
			if !oldKnown[line.key] {
				if line.commented {
					stats.documented = append(stats.documented, line.key)
				} else {
					stats.added = append(stats.added, line.key)
				}
			}
		}
		out = append(out, line.raw)
	}

	var extras []string
	for i, line := range oldLines {
		if !line.assignment || line.commented {
			continue
		}
		if templateKeys[line.key] || oldLast[line.key] != i || seen[line.key] {
			continue
		}
		extras = append(extras, line.raw)
		stats.extra = append(stats.extra, line.key)
	}
	if len(extras) > 0 {
		out = append(out, "", "# ---- Kept from the previous file (not in this template) ----")
		out = append(out, extras...)
	}

	text := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	stats.changed = text != normalizeNewlines(string(oldData))
	return text, stats
}

func normalizeNewlines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	return text + "\n"
}
