// Package schema: canonical schema digests and DDL extraction.
//
// The schema-driver contract (PLAN.md §4): the Go schema must produce the
// same table/column/index/constraint digest as the pinned Django oracle on
// the same dialect. DDL is derived FROM the oracle's own database
// (sqlite_master / pg_catalog), so the generated migration is upstream's
// schema verbatim; the digest comparison then proves the Go-migrated
// database matches — per dialect.
package schema

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Table is one table's canonical shape.
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
	Indexes []Index  `json:"indexes"`
	Foreign []FK     `json:"foreign_keys"`
}

type Column struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // declared type, as written
	NotNull bool   `json:"notnull"`
	Default string `json:"default"` // "" when absent
	PKOrd   int    `json:"pk"`      // 0 when not part of the PK
}

type Index struct {
	Name   string   `json:"name"`
	Unique bool     `json:"unique"`
	Origin string   `json:"origin"` // c/u/w (created, unique-constraint, partial)
	Cols   []string `json:"columns"`
}

type FK struct {
	Name     string `json:"name,omitempty"`
	Table    string `json:"table"`
	From     string `json:"from"`
	To       string `json:"to"`
	OnUpdate string `json:"on_update"`
	OnDelete string `json:"on_delete"`
}

// Schema is a whole database's canonical digest.
type Schema struct {
	Dialect string  `json:"dialect"`
	Tables  []Table `json:"tables"`
}

// bookkeeping tables are NOT part of the schema contract: Go records its
// applied files in schema_migrations, Django in django_migrations — neither
// is observable through the API (schema-driver scope, PLAN §4).
var bookkeepingTables = `('schema_migrations', 'django_migrations')`

// DigestSQLite reads the canonical digest of a SQLite database.
func DigestSQLite(db *sql.DB) (*Schema, error) {
	s := &Schema{Dialect: "sqlite"}
	names, err := tableNames(db, `SELECT name FROM sqlite_master
		WHERE type='table' AND name NOT LIKE 'sqlite_%'
		AND name NOT IN `+bookkeepingTables+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		t := Table{Name: name}
		rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY cid`, name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c Column
			var def sql.NullString
			if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &def, &c.PKOrd); err != nil {
				rows.Close()
				return nil, err
			}
			c.Default = def.String
			t.Columns = append(t.Columns, c)
		}
		rows.Close()
		if rows.Err() != nil {
			return nil, rows.Err()
		}

		idxRows, err := db.Query(`SELECT name, "unique", origin FROM pragma_index_list(?) ORDER BY name`, name)
		if err != nil {
			return nil, err
		}
		for idxRows.Next() {
			var ix Index
			if err := idxRows.Scan(&ix.Name, &ix.Unique, &ix.Origin); err != nil {
				idxRows.Close()
				return nil, err
			}
			colRows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, ix.Name)
			if err != nil {
				idxRows.Close()
				return nil, err
			}
			for colRows.Next() {
				var col sql.NullString
				if err := colRows.Scan(&col); err != nil {
					colRows.Close()
					idxRows.Close()
					return nil, err
				}
				ix.Cols = append(ix.Cols, col.String) // NULL for expressions
			}
			colRows.Close()
			t.Indexes = append(t.Indexes, ix)
		}
		idxRows.Close()
		if idxRows.Err() != nil {
			return nil, idxRows.Err()
		}

		fkRows, err := db.Query(`SELECT id, "table", "from", "to", on_update, on_delete FROM pragma_foreign_key_list(?) ORDER BY id`, name)
		if err != nil {
			return nil, err
		}
		for fkRows.Next() {
			var id int
			var fk FK
			var to sql.NullString
			if err := fkRows.Scan(&id, &fk.Table, &fk.From, &to, &fk.OnUpdate, &fk.OnDelete); err != nil {
				fkRows.Close()
				return nil, err
			}
			fk.To = to.String
			t.Foreign = append(t.Foreign, fk)
		}
		fkRows.Close()
		if fkRows.Err() != nil {
			return nil, fkRows.Err()
		}
		s.Tables = append(s.Tables, t)
	}
	return s, nil
}

// DigestPostgres reads the canonical digest of a Postgres database from
// pg_catalog (dialect comparison is only ever within-dialect — D4).
func DigestPostgres(db *sql.DB) (*Schema, error) {
	s := &Schema{Dialect: "postgres"}
	rows, err := db.Query(`
		SELECT c.relname,
		       a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname NOT IN ` + bookkeepingTables + `
		ORDER BY c.relname, a.attnum`)
	if err != nil {
		return nil, err
	}
	type colKey struct{ table, col string }
	cols := map[string][]Column{}
	for rows.Next() {
		var table string
		var c Column
		var defaultExpr string
		if err := rows.Scan(&table, &c.Name, &c.Type, &c.NotNull, &defaultExpr); err != nil {
			rows.Close()
			return nil, err
		}
		c.Default = defaultExpr
		cols[table] = append(cols[table], c)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	// Primary key ordinals properly (multi-column pks).
	pkRows, err := db.Query(`
		SELECT c.relname, a.attname, k.ordinality
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ordinality) ON true
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE i.indisprimary AND n.nspname = 'public'`)
	if err != nil {
		return nil, err
	}
	pk := map[colKey]int{}
	for pkRows.Next() {
		var table, col string
		var ord int
		if err := pkRows.Scan(&table, &col, &ord); err != nil {
			pkRows.Close()
			return nil, err
		}
		pk[colKey{table, col}] = ord
	}
	pkRows.Close()
	for table, cs := range cols {
		for i := range cs {
			cs[i].PKOrd = pk[colKey{table, cs[i].Name}]
		}
	}

	// Indexes (non-pk, user-visible; pg indexes incl. unique).
	idxRows, err := db.Query(`
		SELECT c.relname, i.relname, ix.indisunique, pg_get_indexdef(ix.indexrelid)
		FROM pg_index ix
		JOIN pg_class c ON c.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND NOT ix.indisprimary
		ORDER BY c.relname, i.relname`)
	if err != nil {
		return nil, err
	}
	indexes := map[string][]Index{}
	type idxDef struct {
		unique bool
		cols   []string
	}
	for idxRows.Next() {
		var table, name, def string
		var unique bool
		if err := idxRows.Scan(&table, &name, &unique, &def); err != nil {
			idxRows.Close()
			return nil, err
		}
		ix := Index{Name: name, Unique: unique, Origin: "c"}
		// Columns from pg_index instead of parsing the definition.
		ix.Cols = nil
		indexes[table] = append(indexes[table], ix)
	}
	idxRows.Close()

	// Index columns per index.
	colIdxRows, err := db.Query(`
		SELECT i.relname, a.attname
		FROM pg_index ix
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN LATERAL unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ordinality) ON true
		JOIN pg_attribute a ON a.attrelid = ix.indrelid AND a.attnum = k.attnum
		JOIN pg_class c ON c.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND NOT ix.indisprimary
		ORDER BY i.relname, k.ordinality`)
	if err != nil {
		return nil, err
	}
	idxCols := map[string][]string{}
	for colIdxRows.Next() {
		var name string
		var col sql.NullString
		if err := colIdxRows.Scan(&name, &col); err != nil {
			colIdxRows.Close()
			return nil, err
		}
		idxCols[name] = append(idxCols[name], col.String)
	}
	colIdxRows.Close()
	for table, list := range indexes {
		for i := range list {
			list[i].Cols = idxCols[list[i].Name]
		}
		indexes[table] = list
	}

	// Foreign keys.
	fkRows, err := db.Query(`
		SELECT cl.relname, con.conname, pcl.relname, a.attname, pa.attname,
		       con.confupdtype, con.confdeltype
		FROM pg_constraint con
		JOIN pg_class cl ON cl.oid = con.conrelid
		JOIN pg_class pcl ON pcl.oid = con.confrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		JOIN unnest(con.conkey, con.confkey) AS k(attnum, attnum2) ON true
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		JOIN pg_attribute pa ON pa.attrelid = con.confrelid AND pa.attnum = k.attnum2
		WHERE con.contype = 'f' AND n.nspname = 'public'
		ORDER BY cl.relname, con.conname`)
	if err != nil {
		return nil, err
	}
	fks := map[string][]FK{}
	actions := func(code string) string {
		switch code {
		case "a":
			return "NO ACTION"
		case "r":
			return "RESTRICT"
		case "c":
			return "CASCADE"
		case "n":
			return "SET NULL"
		case "d":
			return "SET DEFAULT"
		}
		return code
	}
	for fkRows.Next() {
		var table, conname, ptable, from, to string
		var upd, del string
		if err := fkRows.Scan(&table, &conname, &ptable, &from, &to, &upd, &del); err != nil {
			fkRows.Close()
			return nil, err
		}
		fks[table] = append(fks[table], FK{
			Name: conname, Table: ptable, From: from, To: to,
			OnUpdate: actions(upd), OnDelete: actions(del),
		})
	}
	fkRows.Close()

	for table := range cols {
		t := Table{Name: table, Columns: cols[table], Indexes: indexes[table], Foreign: fks[table]}
		s.Tables = append(s.Tables, t)
	}
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	return s, nil
}

func tableNames(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ExtractSQLiteDDL returns the CREATE statements of a SQLite database
// (tables + explicit indexes), deterministically ordered by name. Django's
// migration bookkeeping table (django_migrations) is skipped: Go keeps its
// own schema_migrations, and neither is part of the schema contract.
func ExtractSQLiteDDL(db *sql.DB) ([]string, error) {
	// Order: tables first (SQLite accepts forward REFERENCES at CREATE),
	// then indexes (an index name can sort before its table), then triggers.
	rows, err := db.Query(`SELECT sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		AND name NOT IN ` + bookkeepingTables + `
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'trigger' THEN 2 ELSE 1 END, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stmts []string
	for rows.Next() {
		var sqlText string
		if err := rows.Scan(&sqlText); err != nil {
			return nil, err
		}
		stmts = append(stmts, strings.TrimSuffix(sqlText, ";")+";")
	}
	return stmts, rows.Err()
}

// Compare returns human-readable differences between two digests (empty
// means equal). Only same-dialect comparison is meaningful (D4).
func Compare(a, b *Schema) []string {
	if a.Dialect != b.Dialect {
		return []string{fmt.Sprintf("dialect mismatch: %s vs %s", a.Dialect, b.Dialect)}
	}
	var problems []string
	am := map[string]Table{}
	for _, t := range a.Tables {
		am[t.Name] = t
	}
	bm := map[string]Table{}
	for _, t := range b.Tables {
		bm[t.Name] = t
	}
	for name := range bm {
		if _, ok := am[name]; !ok {
			problems = append(problems, "table only in b: "+name)
		}
	}
	for name, at := range am {
		bt, ok := bm[name]
		if !ok {
			problems = append(problems, "table only in a: "+name)
			continue
		}
		problems = append(problems, diffTable(at, bt)...)
	}
	sort.Strings(problems)
	return problems
}

func diffTable(a, b Table) []string {
	var problems []string
	prefix := "table " + a.Name + ": "
	aj, _ := json.Marshal(a.Columns)
	bj, _ := json.Marshal(b.Columns)
	if string(aj) != string(bj) {
		problems = append(problems, prefix+"columns differ\n  a: "+string(aj)+"\n  b: "+string(bj))
	}
	ai, _ := json.Marshal(sortedIndexes(a.Indexes))
	bi, _ := json.Marshal(sortedIndexes(b.Indexes))
	if string(ai) != string(bi) {
		problems = append(problems, prefix+"indexes differ\n  a: "+string(ai)+"\n  b: "+string(bi))
	}
	af, _ := json.Marshal(sortedFKs(a.Foreign))
	bf, _ := json.Marshal(sortedFKs(b.Foreign))
	if string(af) != string(bf) {
		problems = append(problems, prefix+"foreign keys differ\n  a: "+string(af)+"\n  b: "+string(bf))
	}
	return problems
}

func sortedIndexes(ix []Index) []Index {
	out := append([]Index(nil), ix...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedFKs(fk []FK) []FK {
	out := append([]FK(nil), fk...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].From < out[j].From
	})
	return out
}
