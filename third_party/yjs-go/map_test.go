package yjs_test

import (
	"testing"
	"testing/quick"

	yjs "github.com/CivNode/yjs-go"
)

func TestMapSetGet(t *testing.T) {
	doc := yjs.NewDoc()
	m := doc.GetMap("data")

	doc.Transact(func() {
		m.Set("name", "Alice")
	}, nil)

	v, ok := m.Get("name")
	if !ok {
		t.Fatal("key 'name' not found")
	}
	if v != "Alice" {
		t.Errorf("want 'Alice' got %v", v)
	}
}

func TestMapDelete(t *testing.T) {
	doc := yjs.NewDoc()
	m := doc.GetMap("data")

	doc.Transact(func() {
		m.Set("x", "y")
	}, nil)
	doc.Transact(func() {
		m.Delete("x")
	}, nil)

	_, ok := m.Get("x")
	if ok {
		t.Error("key 'x' should be deleted")
	}
}

func TestMapKeys(t *testing.T) {
	doc := yjs.NewDoc()
	m := doc.GetMap("data")

	doc.Transact(func() {
		m.Set("a", 1)
		m.Set("b", 2)
	}, nil)
	doc.Transact(func() {
		m.Delete("a")
	}, nil)

	keys := m.Keys()
	if len(keys) != 1 || keys[0] != "b" {
		t.Errorf("want ['b'] got %v", keys)
	}
}

func TestMapObserve(t *testing.T) {
	doc := yjs.NewDoc()
	m := doc.GetMap("data")
	events := 0
	m.Observe(func(ev *yjs.MapEvent) { events++ })

	doc.Transact(func() { m.Set("k", "v") }, nil)
	if events != 1 {
		t.Errorf("want 1 event got %d", events)
	}
	doc.Transact(func() { m.Delete("k") }, nil)
	if events != 2 {
		t.Errorf("want 2 events got %d", events)
	}
}

// TestMapConvergence checks that two Docs setting the same key concurrently
// converge to the same value after exchanging updates.
func TestMapConvergence(t *testing.T) {
	err := quick.Check(func(valA, valB string) bool {
		if len(valA) > 50 {
			valA = valA[:50]
		}
		if len(valB) > 50 {
			valB = valB[:50]
		}

		docA := yjs.NewDoc()
		docB := yjs.NewDoc()
		mapA := docA.GetMap("data")
		mapB := docB.GetMap("data")

		var updA, updB []byte
		docA.OnUpdate(func(u []byte, _ interface{}) { updA = u })
		docB.OnUpdate(func(u []byte, _ interface{}) { updB = u })

		docA.Transact(func() { mapA.Set("key", valA) }, nil)
		docB.Transact(func() { mapB.Set("key", valB) }, nil)

		if err := yjs.ApplyUpdate(docA, updB, "remote"); err != nil {
			return false
		}
		if err := yjs.ApplyUpdate(docB, updA, "remote"); err != nil {
			return false
		}

		vA, okA := mapA.Get("key")
		vB, okB := mapB.Get("key")
		if !okA || !okB {
			return false
		}
		return vA == vB
	}, nil)
	if err != nil {
		t.Error(err)
	}
}

func TestArrayPushAndGet(t *testing.T) {
	doc := yjs.NewDoc()
	a := doc.GetArray("items")

	doc.Transact(func() {
		a.Push("one", "two", "three")
	}, nil)

	if a.Len() != 3 {
		t.Errorf("want 3 got %d", a.Len())
	}
	v, ok := a.Get(0)
	if !ok || v != "one" {
		t.Errorf("want 'one' got %v", v)
	}
	v, ok = a.Get(2)
	if !ok || v != "three" {
		t.Errorf("want 'three' got %v", v)
	}
}

func TestArrayInsert(t *testing.T) {
	doc := yjs.NewDoc()
	a := doc.GetArray("items")

	doc.Transact(func() {
		a.Push("a", "c")
	}, nil)
	doc.Transact(func() {
		a.Insert(1, "b")
	}, nil)

	s := a.ToSlice()
	if len(s) != 3 || s[0] != "a" || s[1] != "b" || s[2] != "c" {
		t.Errorf("want [a b c] got %v", s)
	}
}

func TestArrayDelete(t *testing.T) {
	doc := yjs.NewDoc()
	a := doc.GetArray("items")

	doc.Transact(func() { a.Push("x", "y", "z") }, nil)
	doc.Transact(func() { a.Delete(1, 1) }, nil)

	s := a.ToSlice()
	if len(s) != 2 || s[0] != "x" || s[1] != "z" {
		t.Errorf("want [x z] got %v", s)
	}
}

func TestArrayObserve(t *testing.T) {
	doc := yjs.NewDoc()
	a := doc.GetArray("items")
	events := 0
	a.Observe(func(ev *yjs.ArrayEvent) { events++ })

	doc.Transact(func() { a.Push("v") }, nil)
	if events != 1 {
		t.Errorf("want 1 event got %d", events)
	}
}

// TestArrayConvergence checks concurrent pushes converge.
func TestArrayConvergence(t *testing.T) {
	err := quick.Check(func(a, b string) bool {
		if len(a) > 50 {
			a = a[:50]
		}
		if len(b) > 50 {
			b = b[:50]
		}

		docA := yjs.NewDoc()
		docB := yjs.NewDoc()
		arrA := docA.GetArray("items")
		arrB := docB.GetArray("items")

		var updA, updB []byte
		docA.OnUpdate(func(u []byte, _ interface{}) { updA = u })
		docB.OnUpdate(func(u []byte, _ interface{}) { updB = u })

		docA.Transact(func() { arrA.Push(a) }, nil)
		docB.Transact(func() { arrB.Push(b) }, nil)

		if err := yjs.ApplyUpdate(docA, updB, "remote"); err != nil {
			return false
		}
		if err := yjs.ApplyUpdate(docB, updA, "remote"); err != nil {
			return false
		}

		return arrA.Len() == arrB.Len()
	}, nil)
	if err != nil {
		t.Error(err)
	}
}
