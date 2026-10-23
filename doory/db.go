// DATABASE_URL handling (D4: SQLite by default, Postgres optional).
// Forms: postgres://... | postgresql://... | sqlite:////abs | sqlite:///rel
// | bare path (defaults to a local SQLite file).
//
// Per-connection state is set per new connection, never post-open, because
// database/sql pools hand out arbitrary connections:
//   - SQLite: foreign_keys ON (Django enforces FKs), busy_timeout, WAL — the
//     D4 single-writer hazard list — via DSN _pragma params on every conn.
//   - Postgres: search_path=public via pgx OptionAfterConnect; pg_dump'd
//     migration scripts reset the session search_path to ” (fully qualified
//     names only) and that must never leak into later pool connections.
package doory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	pgxstd "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// openPostgres opens a pgx-backed *sql.DB with the search_path pinned on
// every new connection (OptionAfterConnect).
func openPostgres(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgxstd.OpenDB(*cfg, pgxstd.OptionAfterConnect(func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET search_path = public, pg_catalog")
		return err
	})), nil
}

const sqlitePragmas = "_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"

// OpenDB opens the database named by DATABASE_URL (Django's env.db parse).
func OpenDB(databaseURL string) (*sql.DB, string, error) {
	var driver, dsn string
	switch {
	case strings.HasPrefix(databaseURL, "postgres://"),
		strings.HasPrefix(databaseURL, "postgresql://"):
		db, err := openPostgres(databaseURL)
		if err != nil {
			return nil, "pgx", fmt.Errorf("open pgx: %w", err)
		}
		return db, "pgx", nil
	case strings.HasPrefix(databaseURL, "sqlite:////"): // sqlite:////abs/path
		driver, dsn = "sqlite", withPragmas(strings.TrimPrefix(databaseURL, "sqlite:///"))
	case strings.HasPrefix(databaseURL, "sqlite:///"): // sqlite:///rel/path
		driver, dsn = "sqlite", withPragmas(strings.TrimPrefix(databaseURL, "sqlite:///"))
	case strings.HasPrefix(databaseURL, "sqlite://"):
		driver, dsn = "sqlite", withPragmas(strings.TrimPrefix(databaseURL, "sqlite://"))
	default:
		driver, dsn = "sqlite", withPragmas(databaseURL)
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, driver, fmt.Errorf("open %s: %w", driver, err)
	}
	if driver == "sqlite" {
		// SQLite: one writer at a time (D4); WAL keeps readers unblocked.
		db.SetMaxOpenConns(1)
	}
	return db, driver, nil
}

// withPragmas turns a sqlite path into a file: DSN carrying the pragmas.
func withPragmas(path string) string {
	path = strings.TrimPrefix(path, "file:")
	if strings.Contains(path, "?") {
		return "file:" + path + "&" + sqlitePragmas
	}
	return "file:" + path + "?" + sqlitePragmas
}
