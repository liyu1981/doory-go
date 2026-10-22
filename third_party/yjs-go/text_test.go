package yjs_test

import (
	"testing"
	"testing/quick"

	yjs "github.com/CivNode/yjs-go"
)

func TestTextInsertAndString(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("content")

	doc.Transact(func() {
		text.Insert(0, "hello")
	}, nil)

	if got := text.String(); got != "hello" {
		t.Errorf("want %q got %q", "hello", got)
	}
}

func TestTextInsertMiddle(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("content")

	doc.Transact(func() { text.Insert(0, "helo") }, nil)
	doc.Transact(func() { text.Insert(2, "l") }, nil)

	if got := text.String(); got != "hello" {
		t.Errorf("want %q got %q", "hello", got)
	}
}

func TestTextDelete(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("content")

	doc.Transact(func() { text.Insert(0, "hello world") }, nil)
	doc.Transact(func() { text.Delete(5, 6) }, nil)

	if got := text.String(); got != "hello" {
		t.Errorf("want %q got %q", "hello", got)
	}
}

func TestTextLength(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("content")

	doc.Transact(func() { text.Insert(0, "hi") }, nil)
	if text.Len() != 2 {
		t.Errorf("want 2 got %d", text.Len())
	}
	doc.Transact(func() { text.Delete(0, 1) }, nil)
	if text.Len() != 1 {
		t.Errorf("after delete want 1 got %d", text.Len())
	}
}

func TestTextObserve(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("content")

	events := 0
	text.Observe(func(ev *yjs.TextEvent) {
		events++
	})

	doc.Transact(func() { text.Insert(0, "hello") }, nil)
	if events != 1 {
		t.Errorf("expected 1 event, got %d", events)
	}

	doc.Transact(func() { text.Delete(0, 5) }, nil)
	if events != 2 {
		t.Errorf("expected 2 events, got %d", events)
	}
}

func TestTextUnicode(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("content")

	// Multi-byte UTF-8 characters.
	doc.Transact(func() { text.Insert(0, "helo") }, nil)
	doc.Transact(func() { text.Insert(2, "l") }, nil)

	if got := text.String(); got != "hello" {
		t.Errorf("want %q got %q", "hello", got)
	}
}

// TestTextDeleteMidItem verifies that deleting characters from the middle of a
// single multi-character item (a straddling delete) produces the correct result.
// This locks in the behaviour after removing the redundant findPositionAt call
// from Text.Delete.
func TestTextDeleteMidItem(t *testing.T) {
	tests := []struct {
		name   string
		insert string
		index  uint64
		length uint64
		want   string
	}{
		{
			name:   "delete middle of single item",
			insert: "abcdef",
			index:  2,
			length: 2,
			want:   "abef",
		},
		{
			name:   "delete from start of item",
			insert: "hello",
			index:  0,
			length: 2,
			want:   "llo",
		},
		{
			name:   "delete tail of item",
			insert: "hello",
			index:  3,
			length: 2,
			want:   "hel",
		},
		{
			name:   "delete entire item",
			insert: "hello",
			index:  0,
			length: 5,
			want:   "",
		},
		{
			name:   "delete single char mid item",
			insert: "abcde",
			index:  3,
			length: 1,
			want:   "abce",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := yjs.NewDocWithClientID(42)
			text := doc.GetText("t")
			doc.Transact(func() { text.Insert(0, tc.insert) }, nil)
			doc.Transact(func() { text.Delete(tc.index, tc.length) }, nil)
			if got := text.String(); got != tc.want {
				t.Errorf("Delete(%d,%d) on %q: got %q want %q",
					tc.index, tc.length, tc.insert, got, tc.want)
			}
		})
	}
}

// TestTextConvergence checks that two Docs applying the same operations in
// different orders converge to the same state after exchanging updates.
func TestTextConvergence(t *testing.T) {
	err := quick.Check(func(a, b string) bool {
		if len(a) == 0 || len(b) == 0 {
			return true
		}
		// Keep strings short for readability; quick.Check generates arbitrary ones.
		if len(a) > 100 {
			a = a[:100]
		}
		if len(b) > 100 {
			b = b[:100]
		}

		docA := yjs.NewDoc()
		docB := yjs.NewDoc()

		textA := docA.GetText("content")
		textB := docB.GetText("content")

		// docA inserts a, docB inserts b concurrently.
		var updateFromA []byte
		var updateFromB []byte

		docA.OnUpdate(func(update []byte, _ interface{}) {
			updateFromA = update
		})
		docB.OnUpdate(func(update []byte, _ interface{}) {
			updateFromB = update
		})

		docA.Transact(func() { textA.Insert(0, a) }, nil)
		docB.Transact(func() { textB.Insert(0, b) }, nil)

		// Exchange updates.
		if err := yjs.ApplyUpdate(docA, updateFromB, "remote"); err != nil {
			return false
		}
		if err := yjs.ApplyUpdate(docB, updateFromA, "remote"); err != nil {
			return false
		}

		// Both docs must converge.
		return textA.String() == textB.String()
	}, nil)
	if err != nil {
		t.Error(err)
	}
}
