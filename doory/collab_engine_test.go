package doory

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"doory-go/internal/crdt"
	"doory-go/internal/yproto"
)

// engine fixture file (engine_fixtures.py, oracle apply_client_updates).
type engineFixtures struct {
	Batch struct {
		Base        string     `json:"base"`
		Updates     []string   `json:"updates"`
		InitialSize int        `json:"initial_size"`
		Client      uint64     `json:"client"`
		Introduced  [][]uint64 `json:"introduced"`
		Outcomes    []struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
			Detail string `json:"detail"`
		} `json:"outcomes"`
		SV           map[string]uint64 `json:"sv"`
		Size         int               `json:"size"`
		Merged       string            `json:"merged"`
		AppliedCount int               `json:"applied_count"`
		PagesGone    bool              `json:"pages_gone"`
		Poisoned     bool              `json:"poisoned"`
	} `json:"batch"`
	Noop struct {
		MergedState string `json:"merged_state"`
		Update      string `json:"update"`
		InitialSize int    `json:"initial_size"`
		Outcomes    []struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
			Detail string `json:"detail"`
		} `json:"outcomes"`
		SV        map[string]uint64 `json:"sv"`
		Size      int               `json:"size"`
		PagesGone bool              `json:"pages_gone"`
	} `json:"noop"`
}

func loadEngineFixtures(t *testing.T) engineFixtures {
	t.Helper()
	dir := os.Getenv("CRDT_FIXTURE_DIR")
	if dir == "" {
		dir = filepath.Join("..", "test", "parity", "probes", "0011-collab-crdt")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "engine_fixtures.json"))
	if err != nil {
		t.Fatalf("engine fixtures missing (run engine_fixtures.py): %v", err)
	}
	var fx engineFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func b64d(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestApplyClientUpdatesConformance replays the oracle's batch verdicts.
func TestApplyClientUpdatesConformance(t *testing.T) {
	fx := loadEngineFixtures(t)
	doc := crdt.NewDoc()
	if err := crdt.ApplyUpdate(doc, b64d(t, fx.Batch.Base)); err != nil {
		t.Fatal(err)
	}
	stateBytes, err := crdt.StateVector(doc)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := yproto.DecodeStateVector(stateBytes, 0)
	if err != nil {
		t.Fatal(err)
	}
	size, err := documentSize(doc)
	if err != nil {
		t.Fatal(err)
	}
	if size != fx.Batch.InitialSize {
		t.Fatalf("initial size: got %d want %d", size, fx.Batch.InitialSize)
	}
	var updates [][]byte
	for _, u := range fx.Batch.Updates {
		updates = append(updates, b64d(t, u))
	}
	introduced := []map[uint64]bool{}
	for _, set := range fx.Batch.Introduced {
		m := map[uint64]bool{}
		for _, id := range set {
			m[id] = true
		}
		introduced = append(introduced, m)
	}
	result, err := ApplyClientUpdates(doc, sv, updates, size, NewUpdateBudget(), introduced)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outcomes) != len(fx.Batch.Outcomes) {
		t.Fatalf("outcome count: got %d want %d", len(result.Outcomes), len(fx.Batch.Outcomes))
	}
	for i, got := range result.Outcomes {
		want := fx.Batch.Outcomes[i]
		if got.Kind != want.Kind || got.Reason != want.Reason || got.Detail != want.Detail {
			t.Fatalf("outcome[%d]: got %+v want %+v", i, got, want)
		}
	}
	wantSV := map[uint64]uint64{}
	for k, v := range fx.Batch.SV {
		var client uint64
		for _, c := range k {
			client = client*10 + uint64(c-'0')
		}
		wantSV[client] = v
	}
	if !svEqual(result.SV, wantSV) {
		t.Fatalf("state vector: got %v want %v", result.SV, wantSV)
	}
	if result.Size != fx.Batch.Size {
		t.Fatalf("size: got %d want %d", result.Size, fx.Batch.Size)
	}
	if len(result.Applied) != fx.Batch.AppliedCount {
		t.Fatalf("applied: got %d want %d", len(result.Applied), fx.Batch.AppliedCount)
	}
	if result.PagesGone != fx.Batch.PagesGone || result.Poisoned != fx.Batch.Poisoned {
		t.Fatalf("flags: got pages_gone=%v poisoned=%v want %v/%v",
			result.PagesGone, result.Poisoned, fx.Batch.PagesGone, fx.Batch.Poisoned)
	}
	// The merged update applied ON TOP OF THE BASE must rebuild the same
	// state (merge_updates stores a delta relative to the snapshot).
	mergedDoc := crdt.NewDoc()
	if err := crdt.ApplyUpdate(mergedDoc, b64d(t, fx.Batch.Base)); err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdate(mergedDoc, result.Merged); err != nil {
		t.Fatalf("merged apply: %v", err)
	}
	mergedSVBytes, _ := crdt.StateVector(mergedDoc)
	mergedSV, err := yproto.DecodeStateVector(mergedSVBytes, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !svEqual(mergedSV, wantSV) {
		t.Fatalf("merged state vector: got %v want %v", mergedSV, wantSV)
	}
}

// TestNoopReplayConformance: the oracle's noop/rejected verdict when an
// already-covered update is replayed.
func TestNoopReplayConformance(t *testing.T) {
	fx := loadEngineFixtures(t)
	doc := crdt.NewDoc()
	if err := crdt.ApplyUpdate(doc, b64d(t, fx.Noop.MergedState)); err != nil {
		t.Fatal(err)
	}
	stateBytes, _ := crdt.StateVector(doc)
	sv, err := yproto.DecodeStateVector(stateBytes, 0)
	if err != nil {
		t.Fatal(err)
	}
	size, err := documentSize(doc)
	if err != nil {
		t.Fatal(err)
	}
	if size != fx.Noop.InitialSize {
		t.Fatalf("initial size: got %d want %d", size, fx.Noop.InitialSize)
	}
	result, err := ApplyClientUpdates(doc, sv, [][]byte{b64d(t, fx.Noop.Update)}, size,
		NewUpdateBudget(), []map[uint64]bool{{fx.Batch.Client: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outcomes) != len(fx.Noop.Outcomes) {
		t.Fatalf("outcome count: got %d want %d", len(result.Outcomes), len(fx.Noop.Outcomes))
	}
	for i, got := range result.Outcomes {
		want := fx.Noop.Outcomes[i]
		if got.Kind != want.Kind || got.Reason != want.Reason || got.Detail != want.Detail {
			t.Fatalf("outcome[%d]: got %+v want %+v", i, got, want)
		}
	}
	if result.PagesGone != fx.Noop.PagesGone {
		t.Fatalf("pages_gone: got %v want %v", result.PagesGone, fx.Noop.PagesGone)
	}
	_ = reflect.DeepEqual
}
