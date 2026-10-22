package yjs

// TextEvent carries information about a change to a Text type.
//
// In v0.1.x the event carries only Origin. Detailed delta computation
// (retain/insert/delete operations per the Yjs delta format) is planned
// for v0.2 once the observer infrastructure matures.
type TextEvent struct {
	// Origin is the transaction origin passed to Transact.
	Origin interface{}
}

// Text is a CRDT text sequence. Internally it is a doubly-linked list of Items,
// each holding a string fragment.
type Text struct {
	doc       *Doc
	name      string
	start     *Item // first item in the list (possibly deleted)
	observers []func(*TextEvent)
	itemID    *ID // carrying ContentType item (nil for roots); see map.go
}

func newText(doc *Doc, name string) *Text {
	return &Text{doc: doc, name: name}
}

func (t *Text) sharedTypeName() string { return t.name }

// Insert inserts s at the given UTF-16 code-unit index (Yjs uses UTF-16 length).
// In practice for ASCII content, index equals the rune offset.
func (t *Text) Insert(index uint64, s string) {
	if s == "" {
		return
	}
	doc := t.doc
	doc.mu.Lock()
	tx := doc.currentTx
	doc.mu.Unlock()

	if tx == nil {
		panic("yjs: Text.Insert must be called inside Transact")
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	clock := doc.store.getState(doc.clientID)
	left, splitRight := t.findPositionAt(index)

	var originLeft *ID
	if left != nil {
		// The origin is the ID of the specific character to the left of the
		// insertion point. For a multi-char item at clock C, the char at offset k
		// has virtual ID {client, C+k}.
		oid := ID{left.ID.Client, left.ID.Clock + left.Length - 1}
		originLeft = &oid
	}

	item := &Item{
		ID:          ID{doc.clientID, clock},
		OriginLeft:  originLeft,
		OriginRight: nil,
		Parent:      t,
		Kind:        contentString,
		Content:     s,
		Length:      uint64(len([]rune(s))),
	}
	doc.store.addItem(item)

	// Link: left <-> item <-> splitRight
	item.Left = left
	item.Right = splitRight
	if left != nil {
		left.Right = item
	} else {
		t.start = item
	}
	if splitRight != nil {
		splitRight.Left = item
	}

	tx.addItem(item)
}

// Delete removes length UTF-16 code units starting at index.
func (t *Text) Delete(index, length uint64) {
	if length == 0 {
		return
	}
	doc := t.doc
	doc.mu.Lock()
	tx := doc.currentTx
	doc.mu.Unlock()
	if tx == nil {
		panic("yjs: Text.Delete must be called inside Transact")
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	// Walk and delete items from index to index+length.
	// Items that straddle either boundary are split in-place by splitItemAt.
	var pos uint64
	remaining := length
	for item := t.start; item != nil && remaining > 0; item = item.Right {
		if item.Deleted {
			continue
		}
		end := pos + item.Length

		if end <= index {
			pos = end
			continue
		}

		if pos < index {
			// This item straddles the start of the deletion. Split it first.
			offset := index - pos
			_, right := t.splitItemAt(item, offset)
			if right != nil && !right.Deleted {
				toDelete := right.Length
				if toDelete > remaining {
					// Split again at the end of the delete range.
					_, _ = t.splitItemAt(right, remaining)
					toDelete = remaining
				}
				right.Deleted = true
				tx.addDeletedItem(right)
				remaining -= toDelete
			}
			pos = end
			continue
		}

		// pos >= index: this item is fully within the delete range.
		toDelete := item.Length
		if toDelete > remaining {
			// Split: only delete the first `remaining` chars.
			_, _ = t.splitItemAt(item, remaining)
			toDelete = remaining
		}
		item.Deleted = true
		tx.addDeletedItem(item)
		remaining -= toDelete
		pos = end
	}
}

// String returns the current text content by walking the item list and
// concatenating non-deleted string items.
func (t *Text) String() string {
	doc := t.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()

	var out []rune
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted || item.Kind != contentString {
			continue
		}
		out = append(out, []rune(item.strContent())...)
	}
	return string(out)
}

// Len returns the number of visible UTF-16 code units.
func (t *Text) Len() uint64 {
	doc := t.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()

	var n uint64
	for item := t.start; item != nil; item = item.Right {
		if !item.Deleted {
			n += item.Length
		}
	}
	return n
}

// Observe registers a callback that fires after each transaction that changes this Text.
func (t *Text) Observe(fn func(*TextEvent)) {
	doc := t.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()
	t.observers = append(t.observers, fn)
}

func (t *Text) fireObservers(tx *transaction) {
	if len(t.observers) == 0 {
		return
	}
	ev := &TextEvent{Origin: tx.origin}
	for _, fn := range t.observers {
		fn(ev)
	}
}

// findPositionAt finds the insertion point for index.
// Returns (left, right) where left is the last live item before the index and
// right is the first live item at or after the index.
//
// When index falls in the middle of a multi-character item, that item is split:
// left gets the prefix, right gets the suffix, and the split items are linked.
func (t *Text) findPositionAt(index uint64) (left *Item, right *Item) {
	if index == 0 {
		// Insert at beginning.
		for item := t.start; item != nil; item = item.Right {
			if !item.Deleted {
				return nil, item
			}
		}
		return nil, nil
	}

	var pos uint64
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		end := pos + item.Length
		if end == index {
			// Insertion point is exactly after this item.
			// Find the next live item as the right neighbor.
			right = item.Right
			for right != nil && right.Deleted {
				right = right.Right
			}
			return item, right
		}
		if end > index {
			// index falls inside this item. Split it.
			offset := index - pos // characters into this item
			leftItem, rightItem := t.splitItemAt(item, offset)
			return leftItem, rightItem
		}
		pos = end
		left = item
	}
	// index >= total length: append at end.
	return left, nil
}

// splitItemAt splits item at the given character offset within it.
// The left part stays as item (with truncated content and length),
// a new right item is created with the remainder.
// Returns (left, right).
func (t *Text) splitItemAt(item *Item, offset uint64) (*Item, *Item) {
	if offset == 0 {
		left := item.Left
		for left != nil && left.Deleted {
			left = left.Left
		}
		return left, item
	}

	str := item.strContent()
	runes := []rune(str)

	leftStr := string(runes[:offset])
	rightStr := string(runes[offset:])

	// Modify the left item in-place.
	item.Content = leftStr
	item.Length = uint64(len([]rune(leftStr)))

	// Create the right item, continuing the same client's clock sequence.
	// Its clock = item.clock + leftLen (the ID of the first char of the right side).
	rightItem := &Item{
		ID: ID{
			Client: item.ID.Client,
			Clock:  item.ID.Clock + item.Length, // clock after the left portion
		},
		Left:    item,
		Right:   item.Right,
		Parent:  item.Parent,
		Kind:    contentString,
		Content: rightStr,
		Length:  uint64(len([]rune(rightStr))),
	}
	// The right item's origin is the last char of the left item.
	leftLastClock := item.ID.Clock + item.Length - 1
	oid := ID{item.ID.Client, leftLastClock}
	rightItem.OriginLeft = &oid

	// Link the chain.
	if item.Right != nil {
		item.Right.Left = rightItem
	}
	item.Right = rightItem

	// Register the right item in the store so clock lookups work.
	if t.doc != nil {
		t.doc.store.addItem(rightItem)
	}

	return item, rightItem
}
