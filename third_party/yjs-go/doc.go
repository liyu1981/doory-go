package yjs

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
)

// UpdateHandler is called whenever the local document produces a new update.
type UpdateHandler func(update []byte, origin interface{})

// indexedHandler pairs a handler with its registration ID so it can be removed.
type indexedHandler struct {
	id uint64
	fn UpdateHandler
}

// Doc is the root Yjs document. It owns a StructStore, manages transactions,
// and holds named shared types (Text, Map, Array, XmlFragment).
type Doc struct {
	mu sync.Mutex

	clientID uint64
	store    *StructStore
	share    map[string]SharedType // named types

	// pending holds decoded items whose parent has not arrived yet; they
	// integrate on a later update that brings the missing ancestors.
	pending []*Item

	// ds accumulates all delete ranges ever applied to this document.
	// integrateItem consults it so that items arriving after their delete
	// are immediately marked deleted rather than reintegrated as live.
	ds *deleteSet

	// updateHandlers is the indexed set of registered update handlers.
	// Each entry pairs a handler with an ID used by the unsubscribe closure.
	updateHandlers []indexedHandler

	// nextHandlerID is a monotonically increasing counter for handler IDs.
	nextHandlerID uint64

	// current transaction (nil when idle)
	currentTx *transaction
}

// NewDoc creates a new document with a random clientID.
//
// ClientIDs are 32-bit unsigned integers (matching Yjs/lib0 behavior) so they
// remain within JavaScript's safe-integer range for cross-runtime interop.
func NewDoc() *Doc {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback: non-zero constant if crypto/rand fails.
		b = [4]byte{0xca, 0xfe, 0x01, 0x02}
	}
	id := uint64(binary.LittleEndian.Uint32(b[:]))
	if id == 0 {
		id = 1
	}
	return &Doc{
		clientID: id,
		store:    newStructStore(),
		share:    make(map[string]SharedType),
		ds:       newDeleteSet(),
	}
}

// NewDocWithClientID creates a document with a specific clientID. Useful for tests.
func NewDocWithClientID(clientID uint64) *Doc {
	if clientID == 0 {
		panic("yjs: clientID must be non-zero")
	}
	return &Doc{
		clientID: clientID,
		store:    newStructStore(),
		share:    make(map[string]SharedType),
		ds:       newDeleteSet(),
	}
}

// ClientID returns this document's unique peer identifier.
func (d *Doc) ClientID() uint64 { return d.clientID }

// GetText returns the named Text type, creating it if it does not exist.
func (d *Doc) GetText(name string) *Text {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.share[name]; ok {
		return t.(*Text)
	}
	t := newText(d, name)
	d.share[name] = t
	return t
}

// GetMap returns the named Map type, creating it if it does not exist.
func (d *Doc) GetMap(name string) *Map {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m, ok := d.share[name]; ok {
		return m.(*Map)
	}
	m := newMap(d, name)
	d.share[name] = m
	return m
}

// GetArray returns the named Array type, creating it if it does not exist.
func (d *Doc) GetArray(name string) *Array {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.share[name]; ok {
		return a.(*Array)
	}
	a := newArray(d, name)
	d.share[name] = a
	return a
}

// GetXmlFragment returns the named XmlFragment type, creating it if it does not exist.
// GetXmlFragment returns a shared XmlFragment rooted at the given name.
//
// In v0.1.x the returned fragment supports only Len(); see the XmlFragment
// godoc. Use GetText, GetMap, or GetArray for functional shared types
// until XmlFragment mutation lands in v0.2.
func (d *Doc) GetXmlFragment(name string) *XmlFragment {
	d.mu.Lock()
	defer d.mu.Unlock()
	if x, ok := d.share[name]; ok {
		return x.(*XmlFragment)
	}
	x := newXmlFragment(d, name)
	d.share[name] = x
	return x
}

// OnUpdate registers a handler that is called after each transaction with the
// binary v1 update bytes that should be broadcast to peers.
//
// It returns an unsubscribe function. Call it to remove the handler; subsequent
// transactions will not invoke it. This prevents handler leaks across
// connect/disconnect cycles when the same Doc is reused.
func (d *Doc) OnUpdate(h UpdateHandler) func() {
	d.mu.Lock()
	id := d.nextHandlerID
	d.nextHandlerID++
	d.updateHandlers = append(d.updateHandlers, indexedHandler{id: id, fn: h})
	d.mu.Unlock()

	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for i, entry := range d.updateHandlers {
			if entry.id == id {
				d.updateHandlers = append(d.updateHandlers[:i], d.updateHandlers[i+1:]...)
				return
			}
		}
	}
}

// Transact runs fn inside a transaction. All structural changes must happen
// inside a transaction. Transactions batch items, then on commit produce the
// binary update and fire OnUpdate handlers.
func (d *Doc) Transact(fn func(), origin interface{}) {
	d.mu.Lock()
	if d.currentTx != nil {
		// Nested transact: just run fn.
		d.mu.Unlock()
		fn()
		return
	}
	tx := &transaction{doc: d, origin: origin}
	d.currentTx = tx
	d.mu.Unlock()

	fn()

	d.mu.Lock()
	d.currentTx = nil
	pending := tx.pendingItems
	pendingDs := tx.pendingDeletes
	d.mu.Unlock()

	if len(pending) == 0 && len(pendingDs) == 0 {
		return
	}

	// Encode the transaction as a v1 update and fire handlers.
	update := encodeUpdateV1(pending, pendingDs)
	d.mu.Lock()
	snapshot := make([]indexedHandler, len(d.updateHandlers))
	copy(snapshot, d.updateHandlers)
	d.mu.Unlock()
	for _, entry := range snapshot {
		entry.fn(update, origin)
	}

	// Fire type-level observers.
	tx.fireObservers()
}

// ApplyUpdate applies a v1 binary update received from a remote peer.
func ApplyUpdate(d *Doc, update []byte, origin interface{}) error {
	items, ds, err := decodeUpdateV1(update)
	if err != nil {
		return err
	}

	d.mu.Lock()
	tx := &transaction{doc: d, origin: origin}
	d.currentTx = tx
	// Accumulate incoming delete ranges into the document-level delete set
	// before integrating items. This ensures that integrateItem can consult
	// d.ds and immediately mark items deleted if their ID is already covered
	// by a delete range that arrived out of order.
	for clientID, ranges := range ds.clients {
		for _, r := range ranges {
			d.ds.add(clientID, r.clock, r.len)
		}
	}
	d.mu.Unlock()

	// Integrate items, retrying the ones whose parents have not arrived yet
	// (reference pendingStructs): a multi-client full-state update groups
	// clients in arbitrary order, so children may precede their parents.
	queue := append(append([]*Item{}, d.pending...), items...)
	d.pending = nil
	progress := true
	for progress && len(queue) > 0 {
		progress = false
		next := queue[:0]
		for _, item := range queue {
			d.mu.Lock()
			ok := integrateItem(d, item)
			d.mu.Unlock()
			if ok {
				progress = true
			} else {
				next = append(next, item)
			}
		}
		queue = next
	}
	d.pending = append(d.pending, queue...)

	// Apply the full accumulated delete set. Using d.ds (not just the current
	// update's ds) ensures that delete ranges which arrived before the items
	// they reference are applied once those items are integrated.
	// applyDeleteRange is idempotent: already-deleted items are skipped.
	d.mu.Lock()
	fullDS := d.ds
	d.mu.Unlock()
	for clientID, ranges := range fullDS.clients {
		for _, r := range ranges {
			d.mu.Lock()
			applyDeleteRange(d, tx, clientID, r.clock, r.len)
			d.mu.Unlock()
		}
	}

	d.mu.Lock()
	d.currentTx = nil
	d.mu.Unlock()

	tx.fireObservers()
	return nil
}

// SharedType is implemented by Text, Map, Array, XmlFragment.
type SharedType interface {
	sharedTypeName() string
}

// transaction accumulates changes within a Transact call.
type transaction struct {
	doc            *Doc
	origin         interface{}
	pendingItems   []*Item
	pendingDeletes []*Item
	changedTypes   map[SharedType]struct{}
}

func (tx *transaction) addItem(item *Item) {
	tx.pendingItems = append(tx.pendingItems, item)
	if tx.changedTypes == nil {
		tx.changedTypes = make(map[SharedType]struct{})
	}
	if st, ok := item.Parent.(SharedType); ok {
		tx.changedTypes[st] = struct{}{}
	}
}

func (tx *transaction) addDeletedItem(item *Item) {
	tx.pendingDeletes = append(tx.pendingDeletes, item)
	if tx.changedTypes == nil {
		tx.changedTypes = make(map[SharedType]struct{})
	}
	if st, ok := item.Parent.(SharedType); ok {
		tx.changedTypes[st] = struct{}{}
	}
}

// Snapshot returns an opaque byte slice encoding the full document state.
// Pass the result to RestoreDoc to reconstruct a Doc with the same state.
func (d *Doc) Snapshot() ([]byte, error) {
	return EncodeStateAsUpdate(d, nil)
}

// RestoreDoc reconstructs a Doc from a snapshot produced by Doc.Snapshot.
func RestoreDoc(snapshot []byte) (*Doc, error) {
	d := NewDoc()
	if err := ApplyUpdate(d, snapshot, nil); err != nil {
		return nil, err
	}
	return d, nil
}

func (tx *transaction) fireObservers() {
	if tx.changedTypes == nil {
		return
	}
	for t := range tx.changedTypes {
		switch v := t.(type) {
		case *Text:
			v.fireObservers(tx)
		case *Map:
			v.fireObservers(tx)
		case *Array:
			v.fireObservers(tx)
		}
	}
}
