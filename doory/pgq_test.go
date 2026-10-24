// Unit evidence for the PostgreSQL placeholder rewrite (D4): the port writes
// `?` placeholders and the pgq driver translates them to `$N` for pgx.
package doory

import "testing"

func TestRewritePlaceholders(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM t WHERE a = ? AND b = ?":    "SELECT * FROM t WHERE a = $1 AND b = $2",
		"INSERT INTO t (a) VALUES (?)":             "INSERT INTO t (a) VALUES ($1)",
		"SELECT '?' AS q, ? AS v":                  "SELECT '?' AS q, $1 AS v",
		`SELECT "we?ird" FROM t WHERE x = ?`:       `SELECT "we?ird" FROM t WHERE x = $1`,
		"SELECT 'it''s ?' , ?":                     "SELECT 'it''s ?' , $1",
		"SELECT $$ ? $$, ?":                        "SELECT $$ ? $$, $1",
		"SELECT $tag$ ? $tag$, ?":                  "SELECT $tag$ ? $tag$, $1",
		"UPDATE t SET a = ? WHERE b = ? AND c = ?": "UPDATE t SET a = $1 WHERE b = $2 AND c = $3",
		"SELECT 1 -- ? comment\n":                  "SELECT 1 -- ? comment\n",
	}
	for input, want := range cases {
		if got := rewritePlaceholders(input); got != want {
			t.Errorf("rewrite(%q) = %q, want %q", input, got, want)
		}
	}
}
