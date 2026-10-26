// Migrations: dialect-specific DDL generated from the oracle itself
// (automation/ci/schemagen), embedded and applied by `doory migrate`.
//
// Migration runs on ONE pinned connection: pg_dump'd scripts reset the
// session search_path to ” and sqlite PRAGMAs are per-connection; a pool
// would leak that state. The digest gate (schemadigest) — not the
// generator — owns the truth.
package doory

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// MigrateFromEnv applies the embedded migrations for DATABASE_URL's dialect.
// The file naming picks the dialect (D4): *.sqlite.sql / *.postgres.sql.
func MigrateFromEnv() error {
	s := FromEnv()
	db, driver, err := OpenDB(s.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	return Migrate(db, driver)
}

// Migrate applies embedded migrations in filename order, recording applied
// files in schema_migrations. Django's django_migrations table is NOT
// reproduced: migration *history* is not observable through the API — only
// the final schema is a parity contract (schema-driver scope, PLAN §4).
func Migrate(db *sql.DB, driver string) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	suffix := "." + driverFileSuffix(driver) + ".sql"
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("no migrations for dialect %q", driver)
	}

	mark := `INSERT INTO schema_migrations (version) VALUES (?)`
	count := `SELECT count(*) FROM schema_migrations WHERE version = ?`
	create := `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`
	if driver == "pgx" {
		mark = `INSERT INTO schema_migrations (version) VALUES ($1)`
		count = `SELECT count(*) FROM schema_migrations WHERE version = $1`
		create = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (now() AT TIME ZONE 'utc')
		)`
	}
	if _, err := conn.ExecContext(ctx, create); err != nil {
		return fmt.Errorf("schema_migrations: %w", err)
	}

	for _, name := range files {
		var exists int
		if err := conn.QueryRowContext(ctx, count, name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := execScript(ctx, conn, driver, string(body)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := conn.ExecContext(ctx, mark, name); err != nil {
			return fmt.Errorf("%s (record): %w", name, err)
		}
	}
	// Django's post_migrate receivers create the content types and the
	// add/change/delete/view permissions; the admin and auth permission
	// checks read them.
	if err := seedAuthRows(ctx, conn); err != nil {
		return err
	}
	return nil
}

func driverFileSuffix(driver string) string {
	if driver == "pgx" {
		return "postgres"
	}
	return "sqlite"
}

// execScript runs a multi-statement DDL script on the pinned connection.
// sqlite drivers accept the whole script in one Exec; pgx's stdlib does not —
// split on statement boundaries (schema DDL contains no function bodies at
// this stage, so a line-based split suffices; revisit when a migration needs
// one). pg_dump scripts end with `set_config('search_path', ”)` effects:
// restore the default before the caller's bookkeeping queries.
func execScript(ctx context.Context, conn *sql.Conn, driver string, script string) error {
	if driver != "pgx" {
		_, err := conn.ExecContext(ctx, script)
		return err
	}
	for _, stmt := range splitStatements(script) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w in statement: %.120s", err, stmt)
		}
	}
	_, err := conn.ExecContext(ctx, `SET search_path = public, pg_catalog`)
	return err
}

func splitStatements(script string) []string {
	var out []string
	var cur []string
	flush := func() {
		stmt := strings.TrimSpace(strings.Join(cur, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
		cur = nil
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue // comment-only line
		}
		cur = append(cur, line)
		if strings.HasSuffix(trimmed, ";") {
			flush()
		}
	}
	flush()
	return out
}
