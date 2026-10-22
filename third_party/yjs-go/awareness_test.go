package yjs_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	yjs "github.com/CivNode/yjs-go"
)

func awarenessTestdata() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "testdata", "yjs-vectors")
}

func TestAwarenessSetGet(t *testing.T) {
	doc := yjs.NewDoc()
	a := yjs.NewAwareness(doc)
	defer a.Destroy()

	a.SetLocalState(map[string]interface{}{"name": "Bob", "cursor": 5})
	state := a.GetLocalState()
	if state == nil {
		t.Fatal("expected non-nil local state")
	}
	if state["name"] != "Bob" {
		t.Errorf("want name=Bob got %v", state["name"])
	}
}

func TestAwarenessEncodeDecode(t *testing.T) {
	doc := yjs.NewDoc()
	a := yjs.NewAwareness(doc)
	defer a.Destroy()

	state := map[string]interface{}{"name": "Alice", "x": int64(42)}
	a.SetLocalState(state)

	encoded := a.EncodeUpdate([]uint64{doc.ClientID()})
	if len(encoded) == 0 {
		t.Fatal("encoded awareness update should not be empty")
	}

	// Decode on a fresh awareness instance.
	doc2 := yjs.NewDocWithClientID(doc.ClientID() + 1)
	a2 := yjs.NewAwareness(doc2)
	defer a2.Destroy()

	if err := a2.ApplyUpdate(encoded, "test"); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	states := a2.GetStates()
	remote, ok := states[doc.ClientID()]
	if !ok {
		t.Fatal("remote state not received")
	}
	if remote["name"] != "Alice" {
		t.Errorf("want name=Alice got %v", remote["name"])
	}
}

func TestAwarenessOnChange(t *testing.T) {
	doc := yjs.NewDoc()
	a := yjs.NewAwareness(doc)
	defer a.Destroy()

	changes := 0
	a.OnChange(func(added, updated, removed []uint64, origin interface{}) {
		changes++
	})

	a.SetLocalState(map[string]interface{}{"cursor": int64(1)})
	if changes == 0 {
		t.Error("expected OnChange to fire after SetLocalState")
	}
}

func TestAwarenessRemoveState(t *testing.T) {
	doc := yjs.NewDoc()
	a := yjs.NewAwareness(doc)
	defer a.Destroy()

	a.SetLocalState(map[string]interface{}{"x": int64(1)})
	a.SetLocalState(nil) // remove

	states := a.GetStates()
	if _, ok := states[doc.ClientID()]; ok {
		t.Error("local state should have been removed")
	}
}

func TestAwarenessGoldenDecode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(awarenessTestdata(), "awareness-update.bin"))
	if err != nil {
		t.Skipf("awareness-update.bin not found (run testdata/extract.sh): %v", err)
	}
	stateData, err := os.ReadFile(filepath.Join(awarenessTestdata(), "awareness-update-state.json"))
	if err != nil {
		t.Skipf("awareness-update-state.json not found: %v", err)
	}

	var expectedState map[string]interface{}
	if err := json.Unmarshal(stateData, &expectedState); err != nil {
		t.Fatalf("parse state json: %v", err)
	}

	doc := yjs.NewDocWithClientID(1)
	a := yjs.NewAwareness(doc)
	defer a.Destroy()

	if err := a.ApplyUpdate(data, "test"); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	states := a.GetStates()
	// The golden file was created with clientID=42.
	state, ok := states[42]
	if !ok {
		t.Fatalf("expected state for clientID=42, got states: %v", states)
	}
	if state["name"] != expectedState["name"] {
		t.Errorf("want name=%v got name=%v", expectedState["name"], state["name"])
	}
}

func TestAwarenessUpdateBroadcast(t *testing.T) {
	// Two awareness instances exchange state and converge.
	docA := yjs.NewDocWithClientID(1)
	docB := yjs.NewDocWithClientID(2)
	a := yjs.NewAwareness(docA)
	b := yjs.NewAwareness(docB)
	defer a.Destroy()
	defer b.Destroy()

	a.SetLocalState(map[string]interface{}{"user": "Alice"})
	b.SetLocalState(map[string]interface{}{"user": "Bob"})

	// A sends to B.
	updateA := a.EncodeUpdate([]uint64{docA.ClientID()})
	if err := b.ApplyUpdate(updateA, "test"); err != nil {
		t.Fatalf("b.ApplyUpdate: %v", err)
	}

	// B sends to A.
	updateB := b.EncodeUpdate([]uint64{docB.ClientID()})
	if err := a.ApplyUpdate(updateB, "test"); err != nil {
		t.Fatalf("a.ApplyUpdate: %v", err)
	}

	// Both should see both users.
	aStates := a.GetStates()
	bStates := b.GetStates()

	if len(aStates) < 2 {
		t.Errorf("A should see 2 states, got %d: %v", len(aStates), aStates)
	}
	if len(bStates) < 2 {
		t.Errorf("B should see 2 states, got %d: %v", len(bStates), bStates)
	}
}
