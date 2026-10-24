// The CLI driver: run management commands against both sides of a booted pair
// and compare what the commands are observable through — exit code, stdout,
// stderr, the database rows and the media files (PLAN.md §4 cli-driver).
//
// Django prints "manage.py" in usage text and the Go binary prints "doory";
// that program name, the absolute per-side MEDIA_ROOT/EXPORT_STORAGE_ROOT
// prefix and Django's global option block (--settings, --pythonpath, … which a
// single binary does not have, D13) are normalized. Everything the command
// itself writes is compared byte for byte.

package runner

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// StepCommand is one management-command step.
type StepCommand struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
}

// cliResult is one side's run of a command.
type cliResult struct {
	Exit   int
	Stdout string
	Stderr string
}

// runCommandStep executes the command on both sides and reports the differences.
func runCommandStep(i int, oracle, goSide *Side, sc *Scenario, step StepCommand) []string {
	var problems []string
	oracleRes, err := runSideCommand(oracle, sc, step, "oracle")
	if err != nil {
		return []string{fmt.Sprintf("step %02d: cli(oracle): %v", i+1, err)}
	}
	goRes, err := runSideCommand(goSide, sc, step, "go")
	if err != nil {
		return []string{fmt.Sprintf("step %02d: cli(go): %v", i+1, err)}
	}
	if oracleRes.Exit != goRes.Exit {
		problems = append(problems, fmt.Sprintf("step %02d: cli exit: oracle=%d go=%d", i+1,
			oracleRes.Exit, goRes.Exit))
	}
	if norm := normalizeCLIOutput(oracleRes.Stdout, oracle); norm != normalizeCLIOutput(goRes.Stdout, goSide) {
		problems = append(problems, fmt.Sprintf("step %02d: cli stdout differs:\n      oracle: %q\n      go:     %q",
			i+1, normalizeCLIOutput(oracleRes.Stdout, oracle), normalizeCLIOutput(goRes.Stdout, goSide)))
	}
	if norm := normalizeCLIOutput(oracleRes.Stderr, oracle); norm != normalizeCLIOutput(goRes.Stderr, goSide) {
		problems = append(problems, fmt.Sprintf("step %02d: cli stderr differs:\n      oracle: %q\n      go:     %q",
			i+1, norm, normalizeCLIOutput(goRes.Stderr, goSide)))
	}
	if diff := compareDatabaseDigests(i, oracle, goSide); len(diff) > 0 {
		problems = append(problems, diff...)
	}
	if diff := compareMediaTrees(i, oracle, goSide); len(diff) > 0 {
		problems = append(problems, diff...)
	}
	return problems
}

// runSideCommand runs one command for one side with the scenario environment.
func runSideCommand(side *Side, sc *Scenario, step StepCommand, sideName string) (cliResult, error) {
	var cmd *exec.Cmd
	env := append(os.Environ(),
		"DATABASE_URL="+side.DatabaseURL,
		"MEDIA_ROOT="+side.MediaRoot,
		"EXPORT_STORAGE_ROOT="+side.ExportRoot,
	)
	for k, v := range sc.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range step.Env {
		env = append(env, k+"="+v)
	}
	if sideName == "oracle" {
		// Scenario paths are repo-relative; run the oracle from the repo root
		// with manage.py addressed absolutely (its own directory still lands
		// on sys.path, so Django imports work).
		manage := filepath.Join(side.BackendDir, "manage.py")
		args := append([]string{manage, step.Command}, step.Args...)
		cmd = exec.Command(side.Python, args...)
		cmd.Dir = side.RepoRoot
		env = append(env,
			"PYTHONDONTWRITEBYTECODE=1",
			"DJANGO_SETTINGS_MODULE=doory.settings.prod",
			"DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789",
			"SECURE_SSL_REDIRECT=false",
			"SECURE_COOKIES=false",
		)
	} else {
		cmd = exec.Command(side.Bin, append([]string{step.Command}, step.Args...)...)
		cmd.Dir = side.RepoRoot
		env = append(env, "SECURE_COOKIES=false")
	}
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			return cliResult{}, err
		}
	}
	return cliResult{Exit: exit, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

var (
	cliProgramName = regexp.MustCompile(`\bmanage\.py\b`)
	// Django's argparse adds these to every command (the Go binary has no
	// settings module, pythonpath, traceback flag or check framework, D13).
	cliDjangoGlobals = regexp.MustCompile(`\s*\[--settings SETTINGS\]|\s*\[--pythonpath PYTHONPATH\]|` +
		`\s*\[--traceback\]|\s*\[--no-color\]|\s*\[--force-color\]|\s*\[--skip-checks\]|` +
		`\s*\[--version\]|\s*\[-v \{0,1,2,3\}\]`)
)

// normalizeCLIOutput strips the framework-level, per-side noise: the program
// name and the Django global-option block in usage text (D13), plus this
// side's absolute media/storage roots (each side has its own workdir).
func normalizeCLIOutput(text string, side *Side) string {
	text = cliProgramName.ReplaceAllString(text, "doory")
	text = strings.ReplaceAll(text, side.MediaRoot, "<MEDIA_ROOT>")
	text = strings.ReplaceAll(text, side.ExportRoot, "<EXPORT_STORAGE_ROOT>")
	text = strings.ReplaceAll(text, side.WorkDir, "<WORKDIR>")
	return normalizeUsageBlock(text)
}

// normalizeUsageBlock compares what a usage block SAYS, not how argparse
// wraps it: the Django global options are removed and the usage lines are
// joined with single spaces (argparse wraps at 80 columns and indents the
// continuation under the program name).
func normalizeUsageBlock(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "usage:") {
			out = append(out, cliDjangoGlobals.ReplaceAllString(line, ""))
			continue
		}
		joined := line
		for i+1 < len(lines) && strings.HasPrefix(lines[i+1], " ") &&
			!strings.Contains(lines[i+1], ": error:") {
			i++
			joined += " " + strings.TrimSpace(lines[i])
		}
		joined = cliDjangoGlobals.ReplaceAllString(joined, "")
		out = append(out, strings.Join(strings.Fields(joined), " "))
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- database ---

// secretColumns hold per-side randomness by design (hashed passwords, random
// tokens, encrypted blobs): their VALUES are compared through behavior
// scenarios, not by the state digest.
var secretColumns = map[string]bool{
	"password": true, "password_hash": true, "secret_encrypted": true, "token": true,
	"snapshot": true, "data": true,
	// Django's session rows hold a signed+compressed payload (D5: the Go port
	// stores its own encoding); only the row count and expiry shape compare.
	"session_key": true, "session_data": true,
}

// frameworkTables are the web framework's own bookkeeping, absent or shaped by
// the framework rather than the application: Django's migration log (the Go
// migrator keeps schema_migrations), its content-type/permission cache and
// auth group tables. D13 records the exclusion.
var frameworkTables = map[string]bool{
	"django_migrations": true, "schema_migrations": true,
	"django_content_type": true, "auth_permission": true,
	"auth_group": true, "auth_group_permissions": true,
	"accounts_user_groups": true, "accounts_user_user_permissions": true,
}

var (
	uuidValue = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)
	tsValue   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?( ?(\+00:00|Z|[+-]\d{4})| \+0000 UTC)?$`)
)

// databaseDigest renders every table as (row count, sha256 of its normalized,
// sorted rows). Rows keep their content; per-side uuids, timestamps and
// secret columns are placeholders so two databases that differ only in
// randomness hash equal.
func databaseDigest(db *sql.DB) ([]string, map[string]int, map[string][]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'
	      AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, nil, nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		tables = append(tables, name)
	}
	rows.Close()

	lines := make([]string, 0, len(tables))
	counts := map[string]int{}
	tableRows := map[string][]string{}
	for _, table := range tables {
		if frameworkTables[table] {
			continue
		}
		columns, err := tableColumns(db, table)
		if err != nil {
			return nil, nil, nil, err
		}
		query := fmt.Sprintf(`SELECT %s FROM "%s"`, quotedColumns(columns), table)
		data, err := db.Query(query)
		if err != nil {
			return nil, nil, nil, err
		}
		var rendered []string
		for data.Next() {
			values := make([]any, len(columns))
			ptrs := make([]any, len(columns))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := data.Scan(ptrs...); err != nil {
				data.Close()
				return nil, nil, nil, err
			}
			parts := make([]string, len(columns))
			for i, column := range columns {
				parts[i] = normalizeDBValue(column, values[i])
			}
			rendered = append(rendered, strings.Join(parts, "|"))
		}
		data.Close()
		if err := data.Err(); err != nil {
			return nil, nil, nil, err
		}
		sort.Strings(rendered)
		counts[table] = len(rendered)
		tableRows[table] = rendered
		sum := sha256.Sum256([]byte(strings.Join(rendered, "\n")))
		lines = append(lines, fmt.Sprintf("%s rows=%d sha=%s", table, len(rendered), hex.EncodeToString(sum[:8])))
	}
	return lines, counts, tableRows, nil
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			def     any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &def, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

func quotedColumns(columns []string) string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = `"` + column + `"`
	}
	return strings.Join(quoted, ", ")
}

// normalizeDBValue replaces per-side randomness with placeholders.
func normalizeDBValue(column string, value any) string {
	if secretColumns[column] {
		return "<secret>"
	}
	switch v := value.(type) {
	case nil:
		return "<null>"
	case time.Time:
		// The sqlite driver maps DATETIME columns to time.Time.
		return "<ts>"
	case []byte:
		if len(v) == 0 {
			return "<empty>"
		}
		sum := sha256.Sum256(v)
		return "blob:" + hex.EncodeToString(sum[:6])
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
	case string:
		if uuidValue.MatchString(v) {
			return "<uuid>"
		}
		if tsValue.MatchString(v) {
			return "<ts>"
		}
		if strings.Contains(v, "\"id\":\"") || strings.Contains(v, "\"id\": \"") {
			// JSON documents carry uuids/timestamps inside; normalize them.
			return uuidInText.ReplaceAllString(tsInText.ReplaceAllString(v, "<ts>"), "<uuid>")
		}
		return v
	}
	return fmt.Sprintf("%v", value)
}

var (
	uuidInText = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	tsInText   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|\+00:00)?`)
)

// compareDatabaseDigests reports tables whose normalized state differs, and
// any foreign-key violation (a delete command must not leave dangling rows).
func compareDatabaseDigests(i int, oracle, goSide *Side) []string {
	oracleDigest, oracleCounts, oracleRows, err := sideDatabaseDigest(oracle)
	if err != nil {
		return []string{fmt.Sprintf("step %02d: oracle db digest: %v", i+1, err)}
	}
	goDigest, goCounts, goRows, err := sideDatabaseDigest(goSide)
	if err != nil {
		return []string{fmt.Sprintf("step %02d: go db digest: %v", i+1, err)}
	}
	var problems []string
	oByTable := map[string]string{}
	gByTable := map[string]string{}
	for idx, line := range oracleDigest {
		oByTable[strings.Fields(line)[0]] = line
		_ = idx
	}
	for _, line := range goDigest {
		gByTable[strings.Fields(line)[0]] = line
	}
	for table, oLine := range oByTable {
		gLine, ok := gByTable[table]
		if !ok {
			problems = append(problems, fmt.Sprintf("step %02d: db table %q missing on go side", i+1, table))
			continue
		}
		if oLine != gLine {
			detail := firstRowDelta(oracleRows[table], goRows[table])
			problems = append(problems, fmt.Sprintf("step %02d: db table %q differs (oracle rows=%d go rows=%d)%s",
				i+1, table, oracleCounts[table], goCounts[table], detail))
		}
	}
	for table := range gByTable {
		if _, ok := oByTable[table]; !ok {
			problems = append(problems, fmt.Sprintf("step %02d: db table %q only on go side", i+1, table))
		}
	}
	for _, side := range []*Side{oracle, goSide} {
		db, err := sql.Open("sqlite", sqlitePath(side.DatabaseURL))
		if err != nil {
			continue
		}
		rows, qerr := db.Query(`PRAGMA foreign_key_check`)
		if qerr == nil {
			for rows.Next() {
				var table string
				var rowid int64
				var parent string
				var fkid int
				if err := rows.Scan(&table, &rowid, &parent, &fkid); err == nil {
					problems = append(problems, fmt.Sprintf("step %02d: %s: dangling %s row %d -> %s",
						i+1, side.Name, table, rowid, parent))
				}
			}
			rows.Close()
		}
		db.Close()
	}
	return problems
}

// sqlitePath turns sqlite:///<path> into the file path the driver opens.
func sqlitePath(dsn string) string {
	return strings.TrimPrefix(dsn, "sqlite:///")
}

func sideDatabaseDigest(side *Side) ([]string, map[string]int, map[string][]string, error) {
	db, err := sql.Open("sqlite", sqlitePath(side.DatabaseURL))
	if err != nil {
		return nil, nil, nil, err
	}
	defer db.Close()
	return databaseDigest(db)
}

// firstRowDelta names the first row present on only one side (or the first
// pair that differs), so a digest mismatch is debuggable from the report.
func firstRowDelta(oracle, goRows []string) string {
	oSet := map[string]bool{}
	gSet := map[string]bool{}
	for _, row := range oracle {
		oSet[row] = true
	}
	for _, row := range goRows {
		gSet[row] = true
	}
	for _, row := range oracle {
		if !gSet[row] {
			return fmt.Sprintf("\n      first oracle-only row: %s", row)
		}
	}
	for _, row := range goRows {
		if !oSet[row] {
			return fmt.Sprintf("\n      first go-only row: %s", row)
		}
	}
	return ""
}

// ------------------------------------------------------------------- media ---

// mediaTreeDigest lists every stored file (relative path with uuids
// normalized, plus its sha256), so a seed/import command's file writes are
// compared as well as its rows.
func mediaTreeDigest(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	var entries []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		sum := sha256.Sum256(content)
		entries = append(entries, fmt.Sprintf("%s sha=%s", uuidInText.ReplaceAllString(rel, "<uuid>"),
			hex.EncodeToString(sum[:8])))
		return nil
	})
	sort.Strings(entries)
	return entries, err
}

func compareMediaTrees(i int, oracle, goSide *Side) []string {
	var problems []string
	for _, pair := range []struct {
		label string
		o, g  string
	}{
		{"media", oracle.MediaRoot, goSide.MediaRoot},
		{"exports", oracle.ExportRoot, goSide.ExportRoot},
	} {
		oEntries := mustMediaDigest(pair.o)
		gEntries := mustMediaDigest(pair.g)
		if strings.Join(oEntries, "\n") != strings.Join(gEntries, "\n") {
			problems = append(problems, fmt.Sprintf("step %02d: %s tree differs:\n      oracle: %v\n      go:     %v",
				i+1, pair.label, oEntries, gEntries))
		}
	}
	return problems
}

func mustMediaDigest(root string) []string {
	entries, _ := mediaTreeDigest(root)
	return entries
}
