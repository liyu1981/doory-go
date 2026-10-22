package yjs

// applyDeleteRange applies a remote delete range to the document.
// It splits items at the range boundaries and marks covered items deleted.
// Must be called with d.mu held.
func applyDeleteRange(d *Doc, tx *transaction, clientID, clock, length uint64) {
	endClock := clock + length

	// Split at the start boundary if needed.
	splitAtClock(d, clientID, clock)
	// Split at the end boundary if needed.
	splitAtClock(d, clientID, endClock)

	// Mark all items within [clock, endClock) as deleted.
	// After splitting, items should align exactly to the range boundaries.
	items := d.store.clients[clientID]
	for _, it := range items {
		if it.ID.Clock >= endClock {
			break
		}
		if it.ID.Clock >= clock && !it.Deleted {
			it.Deleted = true
			tx.addDeletedItem(it)
		}
	}
}

// splitAtClock splits the item that spans clientID:splitClock into two items
// at the given clock boundary.
// No-op if no item spans that clock or if the clock is at an item boundary.
// Must be called with d.mu held.
func splitAtClock(d *Doc, clientID, splitClock uint64) {
	it := d.store.getItem(ID{clientID, splitClock})
	if it == nil {
		return
	}
	// If the item already starts at splitClock, no split needed.
	if it.ID.Clock == splitClock {
		return
	}

	offset := splitClock - it.ID.Clock

	// Only string content supports splitting.
	if it.Kind != contentString {
		return
	}

	str := it.strContent()
	runes := []rune(str)
	if offset >= uint64(len(runes)) {
		return
	}

	leftStr := string(runes[:offset])
	rightStr := string(runes[offset:])

	// Modify left item in-place.
	it.Content = leftStr
	it.Length = offset

	// Create right item.
	rightItem := &Item{
		ID: ID{
			Client: clientID,
			Clock:  splitClock,
		},
		Left:    it,
		Right:   it.Right,
		Parent:  it.Parent,
		Kind:    contentString,
		Content: rightStr,
		Length:  uint64(len([]rune(rightStr))),
	}
	oid := ID{clientID, splitClock - 1}
	rightItem.OriginLeft = &oid

	if it.Right != nil {
		it.Right.Left = rightItem
	}
	it.Right = rightItem

	// Register in store at the correct position.
	insertIntoStore(d.store, rightItem)

	// If this was the start of a text/array, no pointer update needed —
	// the linked list handles traversal.
}

// insertIntoStore inserts newItem into the store at the correct sorted position.
// Must be called with d.mu held.
func insertIntoStore(s *StructStore, newItem *Item) {
	clientID := newItem.ID.Client
	items := s.clients[clientID]

	// Find insertion position (items are sorted by clock ascending).
	pos := 0
	for pos < len(items) && items[pos].ID.Clock < newItem.ID.Clock {
		pos++
	}

	// Insert at pos.
	items = append(items, nil)
	copy(items[pos+1:], items[pos:])
	items[pos] = newItem
	s.clients[clientID] = items
}
