package yjs

// integrateItem inserts item into d's store and the appropriate shared type's
// linked list, using the Yjs LSST conflict-resolution algorithm.
// Must be called with d.mu held. Returns false when the item's parent has not
// arrived yet: the caller must pend it and retry after later items integrate
// (the reference pendingStructs behavior - a peer's multi-client full-state
// update groups clients in arbitrary order).
func integrateItem(d *Doc, item *Item) bool {
	// If we already have this item, skip (idempotent).
	if d.store.getItem(item.ID) != nil {
		return true
	}

	// Resolve parent name to a SharedType if not already set.
	if item.Parent == nil && item.parentName != "" {
		st, ok := d.share[item.parentName]
		if !ok {
			// Create the shared type lazily so remote items can land.
			switch item.Kind {
			case contentString:
				st = newText(d, item.parentName)
			case contentAny:
				if item.ParentSub != nil {
					st = newMap(d, item.parentName)
				} else {
					st = newArray(d, item.parentName)
				}
			default:
				st = newText(d, item.parentName)
			}
			d.share[item.parentName] = st
		}
		item.Parent = st
	}

	// Nested types: a decoded ContentType item's content IS the shared node
	// (reference Yjs). Materialize it by typeRef before slot insertion, so the
	// node exists for children that name this item as their parent.
	if item.Kind == contentType {
		if _, isNode := typeRefOfContent(item.Content); !isNode {
			// Decoded content carries the typeRef (uint64) until materialized.
			typeRef, _ := item.Content.(uint64)
			var node interface{}
			switch typeRef {
			case 0: // Y.Array
				node = newArray(d, "")
			case 1: // Y.Map
				node = newMap(d, "")
			case 2: // Y.Text
				node = newText(d, "")
			}
			if node != nil {
				item.Content = node
				setNodeItemID(node, item.ID)
			}
		}
	}

	// Non-root parents named by item ID (no origin to derive from): the
	// parent is that item's content - the materialized nested node.
	if item.Parent == nil && item.parentID != nil {
		if parentItem := d.store.getItem(*item.parentID); parentItem != nil {
			switch parentItem.Content.(type) {
			case *Map, *Array, *Text:
				item.Parent = parentItem.Content
			}
		}
	}

	// If parent is still unresolved but we have a left-origin neighbor,
	// inherit the parent from that neighbor. This covers Array/Text items
	// after the first element, which encode their OriginLeft but omit the
	// parent key in the wire format. A map item inherits its key the same
	// way (reference Yjs/yrs: the key is written only when no origin).
	if item.Parent == nil && item.OriginLeft != nil {
		leftNeighbor := d.store.getItem(*item.OriginLeft)
		if leftNeighbor != nil && leftNeighbor.Parent != nil {
			item.Parent = leftNeighbor.Parent
			if item.ParentSub == nil {
				item.ParentSub = leftNeighbor.ParentSub
			}
		}
	}

	// Insert-at-the-head items have only a right origin: inherit the parent
	// (and the map key) from the right neighbor the same way.
	if item.Parent == nil && item.OriginRight != nil {
		rightNeighbor := d.store.getItem(*item.OriginRight)
		if rightNeighbor != nil && rightNeighbor.Parent != nil {
			item.Parent = rightNeighbor.Parent
			if item.ParentSub == nil {
				item.ParentSub = rightNeighbor.ParentSub
			}
		}
	}

	// Resolve parent.
	parent, parentSub := resolveParent(d, item)
	if parent == nil {
		return false // pend and retry once the parent arrives
	}
	item.Parent = parent

	d.store.addItem(item)

	// Consult the document's accumulated delete set. If the item's starting ID
	// is already deleted, mark it now. Partial coverage (where only some chars
	// of a multi-char item are deleted) is handled by applyDeleteRange after
	// all items are integrated.
	if d.ds.isDeleted(item.ID) {
		item.Deleted = true
	}

	// Find left neighbor: the last item with ID == item.OriginLeft.
	var left *Item
	if item.OriginLeft != nil {
		left = d.store.getItem(*item.OriginLeft)
	}

	// Find right neighbor: the first item with ID == item.OriginRight.
	var right *Item
	if item.OriginRight != nil {
		right = d.store.getItem(*item.OriginRight)
	}

	// LSST: scan right from left to find the correct insertion position.
	// We need to place this item relative to any concurrent items that may
	// have been inserted in the same position.
	o := itemAfter(parent, left, parentSub) // first candidate after left
	for o != nil && o != right {
		// o is a concurrent item. Compare their origins for tie-breaking.
		if lssIsBefore(d, item, o) {
			// item should go before o — stop scanning.
			break
		}
		left = o
		o = o.Right
	}

	// Insert item between left and right.
	item.Left = left
	item.Right = o

	if left != nil {
		left.Right = item
	} else {
		setFirstItem(parent, item, parentSub)
	}
	if o != nil {
		o.Left = item
	}
	return true
}

// lssIsBefore implements the LSST tie-breaking rule:
// "item" comes before "other" when their origins agree and item.ID < other.ID.
func lssIsBefore(d *Doc, item, other *Item) bool {
	// If other's origin-left is further right than item's origin-left,
	// other started further right so item goes first.
	itemOriginClock := uint64(0)
	if item.OriginLeft != nil {
		itemOriginClock = item.OriginLeft.Clock + 1
	}
	otherOriginClock := uint64(0)
	if other.OriginLeft != nil {
		otherOriginClock = other.OriginLeft.Clock + 1
	}
	if itemOriginClock != otherOriginClock {
		return itemOriginClock > otherOriginClock
	}
	// Same origin: tie-break by ID (higher clientID wins = goes left;
	// compareIDs provides a total order using client then clock).
	return compareIDs(item.ID, other.ID) > 0
}

// itemAfter returns the first live item to the right of left (or the first
// item in the type if left is nil) that belongs to the given parentSub (for Map).
func itemAfter(parent interface{}, left *Item, parentSub *string) *Item {
	if left != nil {
		return left.Right
	}
	return firstItem(parent, parentSub)
}

// firstItem returns the first item in a shared type's list.
func firstItem(parent interface{}, parentSub *string) *Item {
	switch p := parent.(type) {
	case *Text:
		return p.start
	case *Array:
		return p.start
	case *Map:
		if parentSub != nil {
			return p.start[*parentSub]
		}
	case *XmlFragment:
		return p.start
	}
	return nil
}

// setFirstItem sets the first item in a shared type's list.
func setFirstItem(parent interface{}, item *Item, parentSub *string) {
	switch p := parent.(type) {
	case *Text:
		p.start = item
	case *Array:
		p.start = item
	case *Map:
		if parentSub != nil {
			if p.start == nil {
				p.start = make(map[string]*Item)
			}
			p.start[*parentSub] = item
		}
	case *XmlFragment:
		p.start = item
	}
}

// resolveParent finds the shared type that owns the item, using the item's
// parent key or parentSub. Returns (sharedType, parentSub).
func resolveParent(d *Doc, item *Item) (interface{}, *string) {
	if item.Parent != nil {
		return item.Parent, item.ParentSub
	}
	return nil, item.ParentSub
}
