package yjs

// ArrayEvent carries information about a change to an Array type.
type ArrayEvent struct {
	Origin interface{}
}

// Array is a CRDT ordered sequence of arbitrary values.
type Array struct {
	doc       *Doc
	name      string
	start     *Item
	observers []func(*ArrayEvent)
	itemID    *ID // carrying ContentType item (nil for roots); see map.go
}

func newArray(doc *Doc, name string) *Array {
	return &Array{doc: doc, name: name}
}

func (a *Array) sharedTypeName() string { return a.name }

// Push appends values to the end of the array.
func (a *Array) Push(values ...interface{}) {
	doc := a.doc
	doc.mu.Lock()
	tx := doc.currentTx
	doc.mu.Unlock()
	if tx == nil {
		panic("yjs: Array.Push must be called inside Transact")
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	// Find the last item.
	var last *Item
	for item := a.start; item != nil; item = item.Right {
		last = item
	}

	for _, v := range values {
		clock := doc.store.getState(doc.clientID)
		var originLeft *ID
		if last != nil {
			oid := ID{last.ID.Client, last.ID.Clock + last.Length - 1}
			originLeft = &oid
		}
		item := &Item{
			ID:         ID{doc.clientID, clock},
			OriginLeft: originLeft,
			Parent:     a,
			Kind:       contentAny,
			Content:    v,
			Length:     1,
			Left:       last,
		}
		if last != nil {
			last.Right = item
		} else {
			a.start = item
		}
		doc.store.addItem(item)
		tx.addItem(item)
		last = item
	}
}

// Insert inserts values at the given index.
func (a *Array) Insert(index uint64, values ...interface{}) {
	if len(values) == 0 {
		return
	}
	doc := a.doc
	doc.mu.Lock()
	tx := doc.currentTx
	doc.mu.Unlock()
	if tx == nil {
		panic("yjs: Array.Insert must be called inside Transact")
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	left := a.findItemAtIndex(index)

	var rightItem *Item
	if left != nil {
		rightItem = left.Right
	} else {
		rightItem = a.start
	}

	for i, v := range values {
		clock := doc.store.getState(doc.clientID)
		var originLeft *ID
		var prev *Item
		if i == 0 {
			prev = left
		} else {
			// previous item we just inserted
			prev = doc.store.getItem(ID{doc.clientID, clock - 1})
		}
		if prev != nil {
			oid := ID{prev.ID.Client, prev.ID.Clock + prev.Length - 1}
			originLeft = &oid
		}

		kind := contentAny
		if _, ok := typeRefOfContent(v); ok {
			kind = contentType
		}
		// A position between two items needs BOTH origins: without
		// originRight the integrator only knows what to sit after and a
		// decoded snapshot appends the value instead of placing it
		// (0015-array-index round trip caught it).
		var originRight *ID
		if rightItem != nil {
			oid := rightItem.ID
			originRight = &oid
		}
		item := &Item{
			ID:          ID{doc.clientID, clock},
			OriginLeft:  originLeft,
			OriginRight: originRight,
			Parent:      a,
			Kind:        kind,
			Content:     v,
			Length:      1,
			Left:        prev,
			Right:       rightItem,
		}
		setNodeItemID(v, item.ID)
		if prev != nil {
			prev.Right = item
		} else {
			a.start = item
		}
		if rightItem != nil {
			rightItem.Left = item
		}
		doc.store.addItem(item)
		tx.addItem(item)
		rightItem = item.Right
	}
}

// itemValues returns an item's values as a slice. A ContentAny item may hold
// one scalar (a local Push) or a batch (decoded insert, or a split half).
func itemValues(item *Item) []interface{} {
	if item.Kind == contentAny {
		if values, ok := item.Content.([]interface{}); ok {
			return values
		}
		return []interface{}{item.Content}
	}
	return []interface{}{item.Content}
}

// valueSpan is how many VALUES an item contributes to array indices. Yjs
// counts a ContentAny insert batch as Length values; every other array
// content kind is one value per item.
func valueSpan(item *Item) uint64 {
	if item.Kind == contentAny {
		return uint64(len(itemValues(item)))
	}
	return 1
}

// splitItem splits a ContentAny batch at value offset diff (Yjs
// Item.splitItem) and returns the RIGHT half. The left half stays this item.
// Caller holds doc.mu.
func (a *Array) splitItem(item *Item, diff uint64) *Item {
	if diff == 0 || diff >= item.Length {
		return item
	}
	values, ok := item.Content.([]interface{})
	if !ok || item.Kind != contentAny {
		return item // only value batches split; other kinds are atomic
	}
	right := &Item{
		ID:          ID{Client: item.ID.Client, Clock: item.ID.Clock + diff},
		OriginLeft:  &ID{Client: item.ID.Client, Clock: item.ID.Clock + diff - 1},
		OriginRight: item.OriginRight,
		Parent:      item.Parent,
		ParentSub:   item.ParentSub,
		Kind:        item.Kind,
		Content:     append([]interface{}(nil), values[diff:]...),
		Length:      item.Length - diff,
		Left:        item,
		Right:       item.Right,
	}
	if item.Right != nil {
		right.Right.Left = right
	}
	item.Right = right
	item.Content = append([]interface{}(nil), values[:diff]...)
	item.Length = diff
	a.doc.store.addItem(right)
	return right
}

// Delete removes length items starting at index.
func (a *Array) Delete(index, length uint64) {
	if length == 0 {
		return
	}
	doc := a.doc
	doc.mu.Lock()
	tx := doc.currentTx
	doc.mu.Unlock()
	if tx == nil {
		panic("yjs: Array.Delete must be called inside Transact")
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	// Indices count VALUES: a decoded ContentAny batch holds several. A
	// partial overlap splits the batch so exactly the requested values die.
	end := index + length
	var pos uint64
	for item := a.start; item != nil; {
		if item.Deleted {
			item = item.Right
			continue
		}
		span := valueSpan(item)
		if pos >= end {
			break
		}
		if pos+span <= index {
			pos += span
			item = item.Right
			continue
		}
		var startOff uint64
		if index > pos {
			startOff = index - pos
		}
		endOff := span
		if end < pos+span {
			endOff = end - pos
		}
		target := item
		if startOff > 0 {
			target = a.splitItem(item, startOff) // item keeps [0,startOff)
		}
		if endOff-startOff < target.Length {
			a.splitItem(target, endOff-startOff)
		}
		target.Deleted = true
		tx.addDeletedItem(target)
		pos += span
		item = target.Right
	}
}

// Get returns the value at index, or (nil, false) if out of range. Indices
// count VALUES: one ContentAny item may hold an insert batch.
func (a *Array) Get(index uint64) (interface{}, bool) {
	doc := a.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()

	var pos uint64
	for item := a.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		span := valueSpan(item)
		if index < pos+span {
			offset := index - pos
			if item.Kind == contentAny {
				values := itemValues(item)
				if offset < uint64(len(values)) {
					return values[offset], true
				}
			}
			return item.Content, true
		}
		pos += span
	}
	return nil, false
}

// Len returns the number of non-deleted elements (in values: a ContentAny
// batch contributes its Length).
func (a *Array) Len() uint64 {
	doc := a.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()

	var n uint64
	for item := a.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		n += valueSpan(item)
	}
	return n
}

// ToSlice returns all non-deleted values as a slice. A ContentAny item may
// carry SEVERAL values in one item (Yjs writeContentAny of an insert batch);
// the values spread across its Length.
func (a *Array) ToSlice() []interface{} {
	doc := a.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()

	var out []interface{}
	for item := a.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		if item.Kind == contentAny {
			out = append(out, itemValues(item)...)
			continue
		}
		out = append(out, item.Content)
	}
	return out
}

// Observe registers a handler for changes to this Array.
func (a *Array) Observe(fn func(*ArrayEvent)) {
	doc := a.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()
	a.observers = append(a.observers, fn)
}

func (a *Array) fireObservers(tx *transaction) {
	if len(a.observers) == 0 {
		return
	}
	ev := &ArrayEvent{Origin: tx.origin}
	for _, fn := range a.observers {
		fn(ev)
	}
}

// findItemAtIndex returns the last visible item whose value index is <= the
// given index, or nil if index is 0. A position INSIDE a decoded multi-value
// ContentAny batch splits the batch so the insertion can land between values
// (without this the batch counted as one item and index arithmetic drifted).
func (a *Array) findItemAtIndex(index uint64) *Item {
	if index == 0 {
		return nil
	}
	var pos uint64
	var last *Item
	for item := a.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		span := valueSpan(item)
		if index == pos+span {
			return item
		}
		if index < pos+span {
			a.splitItem(item, index-pos)
			return item
		}
		pos += span
		last = item
	}
	return last
}
