// Command schemadigest reads canonical schema digests and (optionally)
// compares two databases — the schema-driver of the parity harness.
//
//	schemadigest -db a.sqlite            print digest JSON
//	schemadigest -a a.sqlite -b b.sqlite  compare; exit 1 on differences
//
// For Postgres: -driver pgx (digest from pg_catalog; same-dialect compare).
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"doory-go/internal/schema"
)

func main() {
	driver := flag.String("driver", "sqlite", "sqlite | pgx")
	dbA := flag.String("db", "", "database (dsn) for digest printing")
	dbB := flag.String("b", "", "second database: compare instead of print")
	aFlag := flag.String("a", "", "first database for compare mode")
	flag.Parse()

	open := func(dsn string) *sql.DB {
		db, err := sql.Open(*driver, dsn)
		if err != nil {
			fatal(err)
		}
		return db
	}

	digest := func(dsn string) *schema.Schema {
		db := open(dsn)
		defer db.Close()
		var (
			d   *schema.Schema
			err error
		)
		switch *driver {
		case "sqlite":
			d, err = schema.DigestSQLite(db)
		case "pgx":
			d, err = schema.DigestPostgres(db)
		default:
			fatal(fmt.Errorf("unknown driver %q", *driver))
		}
		if err != nil {
			fatal(fmt.Errorf("digest %s: %w", dsn, err))
		}
		return d
	}

	switch {
	case *dbB != "" || *aFlag != "":
		first := *aFlag
		if first == "" {
			first = *dbA
		}
		if first == "" {
			fatal(fmt.Errorf("-a (or -db) required for compare"))
		}
		A := digest(first)
		B := digest(*dbB)
		problems := schema.Compare(A, B)
		if len(problems) > 0 {
			for _, p := range problems {
				fmt.Fprintln(os.Stderr, "schema-parity:", p)
			}
			fmt.Fprintf(os.Stderr, "schemadigest: %d difference(s) (%s vs %s)\n", len(problems), first, *dbB)
			os.Exit(1)
		}
		fmt.Printf("schemadigest: ok (%d tables, %s, %s == %s)\n", len(A.Tables), A.Dialect, first, *dbB)
	case *dbA != "":
		d := digest(*dbA)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(d); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("need -db (print) or -a/-b (compare)"))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "schemadigest:", err)
	os.Exit(1)
}
