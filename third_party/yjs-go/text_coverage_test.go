package yjs_test

import (
	"testing"

	yjs "github.com/CivNode/yjs-go"
)

// TestTextDeletePastEnd verifies that deleting beyond the text length
// deletes only what is available and does not panic.
func TestTextDeletePastEnd(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("t")
	doc.Transact(func() { text.Insert(0, "hello") }, nil)

	// Delete starting at index 3 with length 100 (far past end).
	doc.Transact(func() { text.Delete(3, 100) }, nil)

	if got := text.String(); got != "hel" {
		t.Errorf("want %q got %q", "hel", got)
	}
}

// TestTextDeleteZeroWithinNonEmpty verifies that Delete(n, 0) is a no-op.
func TestTextDeleteZeroWithinNonEmpty(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("t")
	doc.Transact(func() { text.Insert(0, "hello") }, nil)

	doc.Transact(func() { text.Delete(2, 0) }, nil)

	if got := text.String(); got != "hello" {
		t.Errorf("want %q got %q", "hello", got)
	}
}

// TestTextDeleteUnicodeMultiChar exercises deletion of multi-rune sequences
// including characters outside the BMP (surrogate-pair territory in UTF-16).
func TestTextDeleteUnicodeMultiChar(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("t")
	// U+1F600 GRINNING FACE is a 4-byte UTF-8 sequence (2 UTF-16 code units).
	// Insert three emoji followed by ASCII.
	doc.Transact(func() { text.Insert(0, "\U0001F600\U0001F601\U0001F602abc") }, nil)

	// Delete the three emoji (3 runes = 3 characters in our Length counting).
	doc.Transact(func() { text.Delete(0, 3) }, nil)

	if got := text.String(); got != "abc" {
		t.Errorf("want %q got %q", "abc", got)
	}
}

// TestTextFindPositionOutOfRange exercises findPositionAt with an index beyond
// the total text length, which should return (last item, nil).
func TestTextFindPositionOutOfRange(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("t")
	doc.Transact(func() { text.Insert(0, "abc") }, nil)

	// Insert at a position beyond the text length; this exercises the
	// "index >= total length: append at end" branch in findPositionAt.
	doc.Transact(func() { text.Insert(100, "xyz") }, nil)

	if got := text.String(); got != "abcxyz" {
		t.Errorf("want %q got %q", "abcxyz", got)
	}
}

// TestTextDeleteAtStart verifies deletion at index 0 (straddle-start branch).
func TestTextDeleteAtStart(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("t")
	doc.Transact(func() { text.Insert(0, "hello world") }, nil)
	doc.Transact(func() { text.Delete(0, 5) }, nil)

	if got := text.String(); got != " world" {
		t.Errorf("want %q got %q", " world", got)
	}
}

// TestTextDeleteStraddleItemBoundary inserts two separate fragments so the
// deletion must cross an item boundary.
func TestTextDeleteStraddleItemBoundary(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("t")
	doc.Transact(func() { text.Insert(0, "hello") }, nil)
	doc.Transact(func() { text.Insert(5, " world") }, nil)

	// Delete across the item boundary: last 2 of "hello" + first 2 of " world".
	doc.Transact(func() { text.Delete(3, 4) }, nil)

	if got := text.String(); got != "helorld" {
		t.Errorf("want %q got %q", "helorld", got)
	}
}

// TestTextDeleteStraddleShorterThanItem exercises the branch where we straddle
// the start of deletion and the right-split item is longer than the remaining
// deletion length, triggering the second split inside the straddle handler.
func TestTextDeleteStraddleShorterThanItem(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("t")
	// Single item "abcdefgh".
	doc.Transact(func() { text.Insert(0, "abcdefgh") }, nil)
	// Delete 2 chars at index 2 (straddle at offset=2; right half is "cdefgh"=6
	// chars; remaining is 2, so toDelete > remaining triggers the second split).
	doc.Transact(func() { text.Delete(2, 2) }, nil)

	if got := text.String(); got != "abefgh" {
		t.Errorf("want %q got %q", "abefgh", got)
	}
}

// TestTextDeleteItemLongerThanRemaining exercises the "fully within range but
// item.Length > remaining" branch where an item spans beyond the deletion end.
func TestTextDeleteItemLongerThanRemaining(t *testing.T) {
	doc := yjs.NewDoc()
	text := doc.GetText("t")
	// Two separate items: "abc" then "defgh".
	doc.Transact(func() { text.Insert(0, "abc") }, nil)
	doc.Transact(func() { text.Insert(3, "defgh") }, nil)

	// Delete 4 chars at 0: consumes all of "abc" (len=3, remaining drops to 1),
	// then enters "defgh" which has length 5 > remaining 1, triggering the split.
	doc.Transact(func() { text.Delete(0, 4) }, nil)

	if got := text.String(); got != "efgh" {
		t.Errorf("want %q got %q", "efgh", got)
	}
}
