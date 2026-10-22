package yproto

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixture file: oracle scan_update verdicts (scan_fixtures.py, pycrdt).
type scanFixtures struct {
	AllowedRoots []string `json:"allowed_roots"`
	MaxAnyDepth  int      `json:"max_any_depth"`
	Cases        []struct {
		Name            string            `json:"name"`
		Update          string            `json:"update"`
		Known           map[string]uint64 `json:"known"`
		Accepted        bool              `json:"accepted"`
		Error           string            `json:"error"`
		Structs         int               `json:"structs"`
		Types           int               `json:"types"`
		HasItems        bool              `json:"has_items"`
		HasDeletes      bool              `json:"has_deletes"`
		Extents         map[string]uint64 `json:"extents"`
		NewClients      []uint64          `json:"new_clients"`
		OriginlessEntry int               `json:"originless_entries"`
		OriginlessTotal int               `json:"originless_total"`
		RightOnly       int               `json:"right_only"`
		InsideRefs      int               `json:"inside_refs"`
	} `json:"cases"`
}

func TestScanConformance(t *testing.T) {
	dir := os.Getenv("CRDT_FIXTURE_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "test", "parity", "probes", "0011-collab-crdt")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "scan_fixtures.json"))
	if err != nil {
		t.Fatalf("scan fixtures missing (run scan_fixtures.py): %v", err)
	}
	var fx scanFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	roots := map[string]bool{}
	for _, r := range fx.AllowedRoots {
		roots[r] = true
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no scan cases")
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			update, err := base64.StdEncoding.DecodeString(c.Update)
			if err != nil {
				t.Fatal(err)
			}
			known := map[uint64]uint64{}
			for k, v := range c.Known {
				var client uint64
				if _, err := fmtSscan(k, &client); err != nil {
					t.Fatal(err)
				}
				known[client] = v
			}
			scan, serr := ScanUpdate(update, known, roots, fx.MaxAnyDepth)
			if !c.Accepted {
				if serr == nil {
					t.Fatalf("oracle rejected with %q; Go accepted", c.Error)
				}
				pe, _ := serr.(*ProtocolError)
				if pe == nil || pe.Message != c.Error {
					t.Fatalf("error mismatch:\n got %v\nwant %q", serr, c.Error)
				}
				return
			}
			if serr != nil {
				t.Fatalf("oracle accepted; Go rejected: %v", serr)
			}
			if scan.Structs != c.Structs || scan.Types != c.Types ||
				scan.HasItems != c.HasItems || scan.HasDeletes != c.HasDeletes ||
				scan.RightOnly != c.RightOnly || scan.InsideRefs != c.InsideRefs {
				t.Fatalf("counters differ: got %+v want structs=%d types=%d items=%v deletes=%v right=%d inside=%d",
					scan, c.Structs, c.Types, c.HasItems, c.HasDeletes, c.RightOnly, c.InsideRefs)
			}
			// extents + new clients
			wantExtents := map[uint64]uint64{}
			for k, v := range c.Extents {
				var client uint64
				if _, err := fmtSscan(k, &client); err != nil {
					t.Fatal(err)
				}
				wantExtents[client] = v
			}
			if !reflect.DeepEqual(scan.Extents, wantExtents) {
				t.Fatalf("extents differ: got %v want %v", scan.Extents, wantExtents)
			}
			gotNew := []uint64{}
			for c := range scan.NewClients {
				gotNew = append(gotNew, c)
			}
			sortU64s(gotNew)
			wantNew := c.NewClients
			if wantNew == nil {
				wantNew = []uint64{}
			}
			sortU64s(wantNew)
			if !reflect.DeepEqual(gotNew, wantNew) {
				t.Fatalf("new clients differ: got %v want %v", gotNew, wantNew)
			}
			if len(scan.Originless) != c.OriginlessEntry || originlessTotal(scan) != c.OriginlessTotal {
				t.Fatalf("originless differ: got entries=%d total=%d want entries=%d total=%d",
					len(scan.Originless), originlessTotal(scan), c.OriginlessEntry, c.OriginlessTotal)
			}
		})
	}
}

func originlessTotal(s UpdateScan) int {
	total := 0
	for _, v := range s.Originless {
		total += v
	}
	return total
}

func sortU64s(v []uint64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func fmtSscan(s string, out *uint64) (int, error) {
	v, err := strconvParseUint(s)
	if err != nil {
		return 0, err
	}
	*out = v
	return 1, nil
}

func strconvParseUint(s string) (uint64, error) {
	var v uint64
	if s == "" {
		return 0, nil
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errNotDigits
		}
		v = v*10 + uint64(r-'0')
	}
	return v, nil
}

var errNotDigits = &ProtocolError{Message: "not digits"}
