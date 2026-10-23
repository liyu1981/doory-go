package yschema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"doory-go/internal/crdt"
)

func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CRDT_FIXTURE_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "test", "parity", "probes", "0011-collab-crdt")
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatalf("fixture dir missing (run generate.py): %v", err)
	}
	return dir
}

func readFixtureJSON(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func readFixtureBin(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertDocEqual(t *testing.T, got map[string]any, wantRel string) {
	t.Helper()
	want := readFixtureJSON(t, wantRel)
	if !JSONEqual(any(got), any(want)) {
		gb, _ := json.MarshalIndent(got, "", " ")
		wb, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("%s: document differs from oracle\n got: %s\nwant: %s", wantRel, gb, wb)
	}
}

// TestWriteReadRoundTrip: DoryDocument -> Y state -> DoryDocument equals the
// oracle's own ydoc_to_document output for every fixture document.
func TestWriteReadRoundTrip(t *testing.T) {
	for _, name := range []string{"basic", "unicode", "numbers", "empty"} {
		t.Run(name, func(t *testing.T) {
			doc := readFixtureJSON(t, name+".doc.json")
			state, err := DocumentToState(doc)
			if err != nil {
				t.Fatalf("document to state: %v", err)
			}
			d := NewDoc()
			if err := crdt.ApplyUpdate(d, state); err != nil {
				t.Fatalf("apply: %v", err)
			}
			out, anomalies := ReadDocument(d)
			// The "empty" document has no pages: no_pages is flagged (the
			// oracle flags it too) - every other fixture must be clean.
			if !anomalies.Empty() && !anomalies.NoPages {
				t.Fatalf("unexpected anomalies: %+v", anomalies)
			}
			assertDocEqual(t, out, name+".roundtrip.json")
		})
	}
}

// TestReadOracleSnapshots: the READ side over the oracle's own snapshots.
func TestReadOracleSnapshots(t *testing.T) {
	for _, name := range []string{"basic", "unicode", "numbers", "empty"} {
		t.Run(name, func(t *testing.T) {
			d := NewDoc()
			if err := crdt.ApplyUpdate(d, readFixtureBin(t, name+".snapshot.bin")); err != nil {
				t.Fatal(err)
			}
			out, anomalies := ReadDocument(d)
			// The "empty" document has no pages: no_pages is flagged (the
			// oracle flags it too) - every other fixture must be clean.
			if !anomalies.Empty() && !anomalies.NoPages {
				t.Fatalf("unexpected anomalies: %+v", anomalies)
			}
			assertDocEqual(t, out, name+".roundtrip.json")
		})
	}
}

// TestEditStreamAndConcurrentStates: the reader over the replayed edit stream
// and both converged concurrent states.
func TestEditStreamAndConcurrentStates(t *testing.T) {
	t.Run("edits/final", func(t *testing.T) {
		d := NewDoc()
		if err := crdt.ApplyUpdate(d, readFixtureBin(t, "edits/base.snapshot.bin")); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(fixtureDir(t), "edits"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".update" {
				if err := crdt.ApplyUpdate(d, readFixtureBin(t, "edits/"+e.Name())); err != nil {
					t.Fatalf("%s: %v", e.Name(), err)
				}
			}
		}
		// The edit stream creates a page as a plain JSON object (pycrdt
		// dict assignment); the oracle flags it unlisted and skips it -
		// the documents must agree either way.
		out, _ := ReadDocument(d)
		assertDocEqual(t, out, "edits/final.json")
		if HasNoPages(d) {
			t.Fatal("pages are present; HasNoPages must be false")
		}
	})
	for _, side := range []string{"merged", "merged_from_left"} {
		t.Run("conc/"+side, func(t *testing.T) {
			d := NewDoc()
			for _, f := range []string{"conc/left.update", "conc/right.update"} {
				if err := crdt.ApplyUpdate(d, readFixtureBin(t, f)); err != nil {
					t.Fatalf("%s: %v", f, err)
				}
			}
			out, anomalies := ReadDocument(d)
			// The conc fixtures store the ADDED pages as plain JSON
			// objects (pycrdt Map.__setitem__ with a dict = Any::Object);
			// the oracle flags them unlisted and skips them too - the
			// documents must agree either way.
			assertDocEqual(t, out, "conc/"+side+".json")
			_ = anomalies
		})
	}
}

// TestWriteDocumentReplaces: writing a second document over a live doc
// replaces the roots (the make_replacement path).
func TestWriteDocumentReplaces(t *testing.T) {
	doc := readFixtureJSON(t, "basic.doc.json")
	d := NewDoc()
	var werr error
	d.Transact(func() { werr = WriteDocument(d, doc) }, nil)
	if werr != nil {
		t.Fatal(werr)
	}
	second := readFixtureJSON(t, "numbers.doc.json")
	d.Transact(func() { werr = WriteDocument(d, second) }, nil)
	if werr != nil {
		t.Fatal(werr)
	}
	out, anomalies := ReadDocument(d)
	if !anomalies.Empty() {
		t.Fatalf("unexpected anomalies: %+v", anomalies)
	}
	assertDocEqual(t, out, "numbers.roundtrip.json")
}
