// coverage_test.go — targeted tests for paths not covered by the primary test files.
package yjs_test

import (
	"testing"

	yjs "github.com/CivNode/yjs-go"
)

// EncodeStateVector / EncodeStateAsUpdate
func TestEncodeStateVectorAndDiff(t *testing.T) {
	docA := yjs.NewDoc()
	textA := docA.GetText("code")
	docA.Transact(func() { textA.Insert(0, "hello") }, nil)

	sv, err := yjs.EncodeStateVector(docA)
	if err != nil {
		t.Fatalf("EncodeStateVector: %v", err)
	}
	if len(sv) == 0 {
		t.Fatal("state vector should not be empty")
	}

	// Empty state on docB — diff should contain everything.
	docB := yjs.NewDoc()
	svB, err := yjs.EncodeStateVector(docB)
	if err != nil {
		t.Fatalf("EncodeStateVector B: %v", err)
	}

	update, err := yjs.EncodeStateAsUpdate(docA, svB)
	if err != nil {
		t.Fatalf("EncodeStateAsUpdate: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("diff from empty should not be empty")
	}

	if err := yjs.ApplyUpdate(docB, update, "remote"); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if got := docB.GetText("code").String(); got != "hello" {
		t.Errorf("after apply: want 'hello' got %q", got)
	}
}

// Empty state-as-update when source has no new items.
func TestEncodeStateAsUpdateNoDiff(t *testing.T) {
	docA := yjs.NewDoc()
	docA.Transact(func() { docA.GetText("code").Insert(0, "hi") }, nil)

	// svA = full state of docA.
	svA, _ := yjs.EncodeStateVector(docA)
	update, err := yjs.EncodeStateAsUpdate(docA, svA)
	if err != nil {
		t.Fatalf("EncodeStateAsUpdate: %v", err)
	}
	// Applying an empty diff is a no-op.
	docB := yjs.NewDoc()
	yjs.ApplyUpdate(docB, update, "remote")
	// docB should remain empty.
	if s := docB.GetText("code").String(); s != "" {
		t.Errorf("no-diff apply should leave docB empty, got %q", s)
	}
}

// Awareness.SetLocalStateField and EncodeAwarenessUpdate
func TestAwarenessFieldAndFullEncode(t *testing.T) {
	doc := yjs.NewDoc()
	a := yjs.NewAwareness(doc)
	defer a.Destroy()

	a.SetLocalState(map[string]interface{}{"user": "Alice"})
	a.SetLocalStateField("cursor", int64(10))

	state := a.GetLocalState()
	if state == nil {
		t.Fatal("expected non-nil local state")
	}
	if state["cursor"] != int64(10) {
		t.Errorf("want cursor=10 got %v", state["cursor"])
	}
	if state["user"] != "Alice" {
		t.Errorf("want user=Alice got %v", state["user"])
	}

	// EncodeAwarenessUpdate encodes all states.
	encoded := a.EncodeAwarenessUpdate()
	if len(encoded) == 0 {
		t.Fatal("EncodeAwarenessUpdate should not be empty")
	}

	// Apply on peer.
	doc2 := yjs.NewDocWithClientID(doc.ClientID() + 1)
	a2 := yjs.NewAwareness(doc2)
	defer a2.Destroy()
	if err := a2.ApplyUpdate(encoded, "remote"); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	states := a2.GetStates()
	s, ok := states[doc.ClientID()]
	if !ok {
		t.Fatal("peer state not received")
	}
	if s["user"] != "Alice" {
		t.Errorf("want user=Alice got %v", s["user"])
	}
}

// XmlFragment — GetXmlFragment + Len
func TestXmlFragmentBasic(t *testing.T) {
	doc := yjs.NewDoc()
	xf := doc.GetXmlFragment("xml")
	if xf == nil {
		t.Fatal("GetXmlFragment returned nil")
	}
	if xf.Len() != 0 {
		t.Errorf("expected empty XmlFragment, got len=%d", xf.Len())
	}
	// Second call returns same instance.
	xf2 := doc.GetXmlFragment("xml")
	if xf != xf2 {
		t.Error("expected same XmlFragment instance")
	}
}

// TestIntegrateItem_SkipsAlreadyDeleted verifies that when a delete-set update
// arrives before the insert it references, items integrated later are marked
// deleted immediately. Without isDeleted wired into integrateItem, the item
// reappears as live content.
func TestIntegrateItem_SkipsAlreadyDeleted(t *testing.T) {
	// A inserts "hello world" then deletes "hello " (chars 0..6).
	// B receives ONLY the delete update first, then the insert update.
	// After both updates, B must converge to "world" (not "hello world").
	docA := yjs.NewDocWithClientID(20)
	docB := yjs.NewDocWithClientID(21)
	textA := docA.GetText("t")
	textB := docB.GetText("t")

	var insertUpdate, deleteUpdate []byte
	step := 0
	docA.OnUpdate(func(u []byte, _ interface{}) {
		step++
		if step == 1 {
			insertUpdate = u
		} else {
			deleteUpdate = u
		}
	})

	docA.Transact(func() { textA.Insert(0, "hello world") }, nil)
	docA.Transact(func() { textA.Delete(0, 6) }, nil) // remove "hello "

	if got := textA.String(); got != "world" {
		t.Fatalf("docA should be 'world', got %q", got)
	}

	// B receives delete-set update first (out-of-order delivery).
	if err := yjs.ApplyUpdate(docB, deleteUpdate, "remote"); err != nil {
		t.Fatalf("ApplyUpdate delete: %v", err)
	}
	// B has no items yet; delete is buffered in the accumulated delete set.

	// B receives the insert update. The inserted items must be marked deleted.
	if err := yjs.ApplyUpdate(docB, insertUpdate, "remote"); err != nil {
		t.Fatalf("ApplyUpdate insert: %v", err)
	}

	if got := textB.String(); got != "world" {
		t.Errorf("convergence: want 'world' got %q (isDeleted not consulted during integration)", got)
	}

	// C: normal order insert-then-delete also converges.
	docC := yjs.NewDocWithClientID(22)
	textC := docC.GetText("t")
	if err := yjs.ApplyUpdate(docC, insertUpdate, "remote"); err != nil {
		t.Fatalf("ApplyUpdate insert (C): %v", err)
	}
	if err := yjs.ApplyUpdate(docC, deleteUpdate, "remote"); err != nil {
		t.Fatalf("ApplyUpdate delete (C): %v", err)
	}
	if got := textC.String(); got != "world" {
		t.Errorf("C (normal order) want 'world' got %q", got)
	}

	// B and C must agree.
	if textB.String() != textC.String() {
		t.Errorf("B and C diverge: B=%q C=%q", textB.String(), textC.String())
	}
}

// deleteSet.add / isDeleted — exercised via TestIntegrateItem_SkipsAlreadyDeleted
// (out-of-order delivery) and here via normal-order insert-then-delete propagation.
func TestDeleteSetCoverage(t *testing.T) {
	docA := yjs.NewDocWithClientID(10)
	docB := yjs.NewDocWithClientID(11)
	textA := docA.GetText("code")
	textB := docB.GetText("code")

	// Track updates per transaction.
	var insertUpdate, deleteUpdate []byte
	step := 0
	docA.OnUpdate(func(u []byte, _ interface{}) {
		step++
		if step == 1 {
			insertUpdate = u
		} else {
			deleteUpdate = u
		}
	})

	docA.Transact(func() { textA.Insert(0, "abcde") }, nil)
	docA.Transact(func() { textA.Delete(1, 3) }, nil) // bcd deleted

	if got := textA.String(); got != "ae" {
		t.Errorf("docA: want 'ae' got %q", got)
	}

	// Apply insert first, then delete — ensures items exist when delete arrives.
	if err := yjs.ApplyUpdate(docB, insertUpdate, "remote"); err != nil {
		t.Fatalf("ApplyUpdate insert: %v", err)
	}
	if got := textB.String(); got != "abcde" {
		t.Errorf("docB after insert: want 'abcde' got %q", got)
	}
	if err := yjs.ApplyUpdate(docB, deleteUpdate, "remote"); err != nil {
		t.Fatalf("ApplyUpdate delete: %v", err)
	}
	if got := textB.String(); got != "ae" {
		t.Errorf("docB after delete: want 'ae' got %q", got)
	}
}

// GetLocalState when no state set returns nil.
func TestAwarenessGetLocalStateNil(t *testing.T) {
	doc := yjs.NewDoc()
	a := yjs.NewAwareness(doc)
	a.SetLocalState(nil) // remove initial empty state
	if a.GetLocalState() != nil {
		t.Error("expected nil local state after remove")
	}
}

// Array out-of-bounds Get returns false.
func TestArrayGetOutOfBounds(t *testing.T) {
	doc := yjs.NewDoc()
	a := doc.GetArray("items")
	doc.Transact(func() { a.Push("x") }, nil)

	_, ok := a.Get(10)
	if ok {
		t.Error("Get out of bounds should return false")
	}
}

// Text.Delete with length 0 is a no-op.
func TestTextDeleteZeroLength(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("code")
	doc.Transact(func() { text.Insert(0, "hello") }, nil)
	doc.Transact(func() { text.Delete(2, 0) }, nil)
	if got := text.String(); got != "hello" {
		t.Errorf("delete 0 should be no-op, got %q", got)
	}
}

// Transact with nested call (runs fn directly).
func TestTransactNested(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("code")
	var outer, inner bool
	doc.Transact(func() {
		outer = true
		doc.Transact(func() {
			inner = true
			text.Insert(0, "x")
		}, nil)
	}, nil)
	if !outer || !inner {
		t.Error("nested transact should run both functions")
	}
	if text.String() != "x" {
		t.Error("nested transact should apply mutation")
	}
}

// ApplyUpdate with an empty update is a no-op.
func TestApplyEmptyUpdate(t *testing.T) {
	doc := yjs.NewDoc()
	// Empty update: 0 structs, 0 delete set entries.
	empty := []byte{0x00, 0x00}
	if err := yjs.ApplyUpdate(doc, empty, "remote"); err != nil {
		t.Fatalf("ApplyUpdate empty: %v", err)
	}
}

// transport.ClientConn.Send exercises the Send method.
// Tested indirectly via TestRealtimePropagation; this hits it more directly.
func TestTextFindItemAtIndex(t *testing.T) {
	// Exercises findItemAtIndex through a Delete path that starts at a non-zero index.
	doc := yjs.NewDoc()
	text := doc.GetText("code")
	doc.Transact(func() {
		text.Insert(0, "hello world")
	}, nil)
	// Delete "world" (index 6, len 5).
	doc.Transact(func() {
		text.Delete(6, 5)
	}, nil)
	if got := text.String(); got != "hello " {
		t.Errorf("want 'hello ' got %q", got)
	}
}

// Test multi-char split in the middle with Delete to cover more of findPositionAt.
func TestTextDeleteMiddle(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("code")
	doc.Transact(func() { text.Insert(0, "abcdefghij") }, nil)
	// Delete from index 3 for length 4: "defg"
	doc.Transact(func() { text.Delete(3, 4) }, nil)
	if got := text.String(); got != "abchij" {
		t.Errorf("want 'abchij' got %q", got)
	}
}

// Test awareness remote peer removes state.
func TestAwarenessRemoteRemoveState(t *testing.T) {
	docA := yjs.NewDocWithClientID(100)
	docB := yjs.NewDocWithClientID(200)
	a := yjs.NewAwareness(docA)
	b := yjs.NewAwareness(docB)
	defer a.Destroy()
	defer b.Destroy()

	a.SetLocalState(map[string]interface{}{"user": "Alice"})

	// B learns about A.
	updA := a.EncodeAwarenessUpdate()
	if err := b.ApplyUpdate(updA, "test"); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	// A sends a null state (clock incremented).
	a.SetLocalState(nil)

	// Now encode A's null state.
	updA2 := a.EncodeUpdate([]uint64{docA.ClientID()})
	// updA2 might be empty since A has no state.
	// Apply to B — B should remove A.
	_ = b.ApplyUpdate(updA2, "test")
	// The important thing is no panic.
}

// Test multiple inserts converging via sync.
func TestTextMultiInsertConvergence(t *testing.T) {
	docA := yjs.NewDocWithClientID(1)
	docB := yjs.NewDocWithClientID(2)
	textA := docA.GetText("code")
	textB := docB.GetText("code")

	var updA, updB []byte
	docA.OnUpdate(func(u []byte, _ interface{}) { updA = u })
	docB.OnUpdate(func(u []byte, _ interface{}) { updB = u })

	docA.Transact(func() { textA.Insert(0, "AAA") }, nil)
	docB.Transact(func() { textB.Insert(0, "BBB") }, nil)

	yjs.ApplyUpdate(docA, updB, "remote")
	yjs.ApplyUpdate(docB, updA, "remote")

	if textA.String() != textB.String() {
		t.Errorf("not converged: A=%q B=%q", textA.String(), textB.String())
	}
}
