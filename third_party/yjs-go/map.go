package yjs

// MapEvent carries information about a change to a Map type.
//
// In v0.1.x the event carries only Origin. Per-key change details
// (action, old value, new value) are planned for v0.2.
type MapEvent struct {
	// Origin is the transaction origin passed to Transact.
	Origin interface{}
}

// Map is a CRDT key-value store. Values are arbitrary Go values encoded as
// Yjs ContentAny. Only the most-recent item per key (by clock) is live.
type Map struct {
	doc       *Doc
	name      string
	start     map[string]*Item // current live item per key
	observers []func(*MapEvent)
	// itemID is the ID of the ContentType item that carries this node as a
	// value (nil for roots); parents named by ID encode it as ParentID.
	itemID *ID
}

func newMap(doc *Doc, name string) *Map {
	return &Map{doc: doc, name: name, start: make(map[string]*Item)}
}

// NewNestedMap creates an unnamed Y.Map for use as a VALUE (a ContentType
// item). Insert it into its parent BEFORE populating it: children must follow
// the type item in clock order (no pending buffer - see PLAN.md M11 spike).
func NewNestedMap(doc *Doc) *Map { return newMap(doc, "") }

func (m *Map) sharedTypeName() string { return m.name }

// Set sets key to value in the map.
func (m *Map) Set(key string, value interface{}) {
	doc := m.doc
	doc.mu.Lock()
	tx := doc.currentTx
	doc.mu.Unlock()
	if tx == nil {
		panic("yjs: Map.Set must be called inside Transact")
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	clock := doc.store.getState(doc.clientID)
	sub := key
	kind := contentAny
	if _, ok := typeRefOfContent(value); ok {
		kind = contentType
	}
	// Delete the old item if present, and name it as the new item's left
	// origin (reference Yjs/yrs map-set form: a slot REPLACEMENT carries the
	// origin and omits parent+key on the wire; only a fresh key writes the
	// first-write form). Probed against pycrdt fixtures (internal/crdt).
	item := &Item{
		ID:       ID{doc.clientID, clock},
		Parent:   m,
		ParentSub: &sub,
		Kind:     kind,
		Content:  value,
		Length:   1,
	}
	if old, ok := m.start[key]; ok {
		old.Deleted = true
		oid := old.ID
		item.OriginLeft = &oid
	}
	setNodeItemID(value, item.ID)
	doc.store.addItem(item)
	m.start[key] = item
	tx.addItem(item)
}

// Get returns the value for key, or (nil, false) if absent. The slot head in
// `start` may be a deleted item (a remote replace inserts after it, per the
// reference wire format): walk the list for the first live item of this key.
func (m *Map) Get(key string) (interface{}, bool) {
	doc := m.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()

	head, ok := m.start[key]
	if !ok {
		return nil, false
	}
	for item := head; item != nil; item = item.Right {
		if item.ParentSub != nil && *item.ParentSub == key && !item.Deleted {
			return item.Content, true
		}
	}
	return nil, false
}

// Delete removes key from the map.
func (m *Map) Delete(key string) {
	doc := m.doc
	doc.mu.Lock()
	tx := doc.currentTx
	doc.mu.Unlock()
	if tx == nil {
		panic("yjs: Map.Delete must be called inside Transact")
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	item, ok := m.start[key]
	if !ok || item.Deleted {
		return
	}
	item.Deleted = true
	tx.addDeletedItem(item)
}

// Keys returns all non-deleted keys.
func (m *Map) Keys() []string {
	doc := m.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()

	var keys []string
	for k, head := range m.start {
		// A deleted head with a live replacement (remote replace) still
		// has a value: walk the list like Get does.
		for item := head; item != nil; item = item.Right {
			if item.ParentSub != nil && *item.ParentSub == k && !item.Deleted {
				keys = append(keys, k)
				break
			}
		}
	}
	return keys
}

// Observe registers a handler for changes to this Map.
func (m *Map) Observe(fn func(*MapEvent)) {
	doc := m.doc
	doc.mu.Lock()
	defer doc.mu.Unlock()
	m.observers = append(m.observers, fn)
}

func (m *Map) fireObservers(tx *transaction) {
	if len(m.observers) == 0 {
		return
	}
	ev := &MapEvent{Origin: tx.origin}
	for _, fn := range m.observers {
		fn(ev)
	}
}
