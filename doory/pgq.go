// PostgreSQL placeholder rewriting (D4). The port's SQL is written with `?`
// placeholders — Django's ORM emits `%s` and psycopg adapts it, so the Go port
// hides the driver difference the same way instead of carrying two SQL
// dialects. This is a database/sql driver wrapper: every statement is
// rewritten to `$1..$n` (outside string literals and quoted identifiers)
// before pgx sees it.
package doory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// pgxConnConfig is pgx.ConnConfig (aliased so the helper's signature reads).
type pgxConnConfig = pgx.ConnConfig

const pgqDriverName = "pgxq"

// pgqDriverInstance is the registered driver (used when opening through a DSN)
// and the one a connector reports.
var pgqDriverInstance = &pgqDriver{inner: &stdlib.Driver{}}

func init() {
	sql.Register(pgqDriverName, pgqDriverInstance)
}

// openPostgresWith opens a *sql.DB whose statements are placeholder-rewritten,
// over a pgx connector built from the parsed configuration.
func openPostgresWith(config pgxConnConfig, options ...stdlib.OptionOpenDB) *sql.DB {
	connector := stdlib.GetConnector(config, options...)
	return sql.OpenDB(&pgqConnector{inner: connector, driver: pgqDriverInstance})
}

// rewritePlaceholders turns `?` into `$1, $2, ...`, skipping single-quoted
// strings, double-quoted identifiers and Postgres dollar-quoted bodies.
func rewritePlaceholders(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch c {
		case '?':
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
		case '\'':
			// single-quoted literal ('' escapes a quote)
			out.WriteByte(c)
			for i++; i < len(query); i++ {
				out.WriteByte(query[i])
				if query[i] == '\'' {
					if i+1 < len(query) && query[i+1] == '\'' {
						i++
						out.WriteByte(query[i])
						continue
					}
					break
				}
			}
		case '"':
			out.WriteByte(c)
			for i++; i < len(query); i++ {
				out.WriteByte(query[i])
				if query[i] == '"' {
					if i+1 < len(query) && query[i+1] == '"' {
						i++
						out.WriteByte(query[i])
						continue
					}
					break
				}
			}
		case '-':
			// -- line comment: copy to the end of the line untouched
			if i+1 < len(query) && query[i+1] == '-' {
				out.WriteString("--")
				i += 2
				for ; i < len(query) && query[i] != '\n'; i++ {
					out.WriteByte(query[i])
				}
				if i < len(query) {
					out.WriteByte('\n')
				}
				continue
			}
			out.WriteByte(c)
		case '/':
			// /* block comment */ (nested per Postgres) is copied untouched
			if i+1 < len(query) && query[i+1] == '*' {
				depth := 0
				out.WriteString("/*")
				i++
				depth = 1
				for i++; i < len(query); i++ {
					if query[i] == '/' && i+1 < len(query) && query[i+1] == '*' {
						depth++
						out.WriteString("/*")
						i++
						continue
					}
					if query[i] == '*' && i+1 < len(query) && query[i+1] == '/' {
						depth--
						out.WriteString("*/")
						i++
						if depth == 0 {
							break
						}
						continue
					}
					out.WriteByte(query[i])
				}
				continue
			}
			out.WriteByte(c)
		case '$':
			// dollar-quoted string: $tag$ ... $tag$
			j := i + 1
			for j < len(query) && (isTagByte(query[j])) {
				j++
			}
			if j < len(query) && query[j] == '$' {
				tag := query[i : j+1]
				out.WriteString(tag)
				if end := strings.Index(query[j+1:], tag); end >= 0 {
					out.WriteString(query[j+1 : j+1+end+len(tag)])
					i = j + 1 + end + len(tag) - 1
					continue
				}
				out.WriteString(query[j+1:])
				i = len(query)
				continue
			}
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

func isTagByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// ------------------------------------------------------------------ driver --

// pgqDriver wraps pgx's stdlib driver.
type pgqDriver struct{ inner driver.Driver }

func (d *pgqDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &pgqConn{inner: conn}, nil
}

func (d *pgqDriver) OpenConnector(name string) (driver.Connector, error) {
	if connector, ok := d.inner.(driver.DriverContext); ok {
		inner, err := connector.OpenConnector(name)
		if err != nil {
			return nil, err
		}
		return &pgqConnector{inner: inner, driver: d}, nil
	}
	return &pgqConnector{name: name, driver: d}, nil
}

type pgqConnector struct {
	inner  driver.Connector
	name   string
	driver *pgqDriver
}

func (c *pgqConnector) Connect(ctx context.Context) (driver.Conn, error) {
	var (
		conn driver.Conn
		err  error
	)
	if c.inner != nil {
		conn, err = c.inner.Connect(ctx)
	} else {
		conn, err = c.driver.Open(c.name)
	}
	if err != nil {
		return nil, err
	}
	return &pgqConn{inner: conn}, nil
}

func (c *pgqConnector) Driver() driver.Driver { return c.driver }

type pgqConn struct{ inner driver.Conn }

func (c *pgqConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.inner.Prepare(rewritePlaceholders(query))
	if err != nil {
		return nil, err
	}
	return &pgqStmt{inner: stmt}, nil
}

func (c *pgqConn) Close() error              { return c.inner.Close() }
func (c *pgqConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

func (c *pgqConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.inner.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return c.inner.Begin()
}

func (c *pgqConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if preparer, ok := c.inner.(driver.ConnPrepareContext); ok {
		stmt, err := preparer.PrepareContext(ctx, rewritePlaceholders(query))
		if err != nil {
			return nil, err
		}
		return &pgqStmt{inner: stmt}, nil
	}
	return c.Prepare(query)
}

func (c *pgqConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, rewritePlaceholders(query), args)
}

func (c *pgqConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, rewritePlaceholders(query), args)
	if err != nil {
		return nil, err
	}
	return &pgqRows{inner: rows}, nil
}

func (c *pgqConn) Ping(ctx context.Context) error {
	if pinger, ok := c.inner.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

type pgqStmt struct{ inner driver.Stmt }

func (s *pgqStmt) Close() error  { return s.inner.Close() }
func (s *pgqStmt) NumInput() int { return s.inner.NumInput() }

func (s *pgqStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.inner.Exec(args)
}

func (s *pgqStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := s.inner.Query(args)
	if err != nil {
		return nil, err
	}
	return &pgqRows{inner: rows}, nil
}

func (s *pgqStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if execer, ok := s.inner.(driver.StmtExecContext); ok {
		return execer.ExecContext(ctx, args)
	}
	return nil, driver.ErrSkip
}

func (s *pgqStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if queryer, ok := s.inner.(driver.StmtQueryContext); ok {
		rows, err := queryer.QueryContext(ctx, args)
		if err != nil {
			return nil, err
		}
		return &pgqRows{inner: rows}, nil
	}
	return nil, driver.ErrSkip
}

// pgqRows converts timestamps to the Django sqlite storage text the port's
// scanners expect ("2026-10-24 12:00:00.123456", UTC): pgx returns time.Time,
// SQLite returns that text, and one code path must read both (D4).
type pgqRows struct{ inner driver.Rows }

func (r *pgqRows) Columns() []string { return r.inner.Columns() }
func (r *pgqRows) Close() error      { return r.inner.Close() }

func (r *pgqRows) Next(dest []driver.Value) error {
	if err := r.inner.Next(dest); err != nil {
		return err
	}
	for i, value := range dest {
		if stamp, ok := value.(time.Time); ok {
			dest[i] = stamp.UTC().Format("2006-01-02 15:04:05.000000")
		}
	}
	return nil
}

var _ driver.DriverContext = (*pgqDriver)(nil)
