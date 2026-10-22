package yjs

import "sort"

// StructStore holds all items per client, sorted by clock.
// This matches Yjs StructStore exactly.
type StructStore struct {
	// clients maps clientID -> sorted slice of items (by clock ascending).
	clients map[uint64][]*Item
}

func newStructStore() *StructStore {
	return &StructStore{clients: make(map[uint64][]*Item)}
}

// getState returns the current clock (= highest clock + length) for clientID.
func (s *StructStore) getState(clientID uint64) uint64 {
	items := s.clients[clientID]
	if len(items) == 0 {
		return 0
	}
	last := items[len(items)-1]
	return last.ID.Clock + last.Length
}

// stateVector returns a map of clientID -> state for all clients.
func (s *StructStore) stateVector() map[uint64]uint64 {
	sv := make(map[uint64]uint64, len(s.clients))
	for client := range s.clients {
		sv[client] = s.getState(client)
	}
	return sv
}

// addItem inserts an item into the store. Items must be added in clock order.
func (s *StructStore) addItem(item *Item) {
	items := s.clients[item.ID.Client]
	// Keep each client's list sorted by clock (reference Yjs): pending
	// retries integrate out of clock order, and getItem's binary search plus
	// getState's tail read both assume sorted structs.
	idx := sort.Search(len(items), func(i int) bool {
		return items[i].ID.Clock > item.ID.Clock
	})
	items = append(items, nil)
	copy(items[idx+1:], items[idx:])
	items[idx] = item
	s.clients[item.ID.Client] = items
}

// getItem returns the item that contains the given ID, or nil.
func (s *StructStore) getItem(id ID) *Item {
	items := s.clients[id.Client]
	if len(items) == 0 {
		return nil
	}
	// Binary search for the item whose clock <= id.Clock < clock+length.
	idx := sort.Search(len(items), func(i int) bool {
		return items[i].ID.Clock > id.Clock
	})
	if idx == 0 {
		return nil
	}
	item := items[idx-1]
	if item.ID.Clock+item.Length > id.Clock {
		return item
	}
	return nil
}

// deleteSet records which items have been deleted, keyed by clientID.
type deleteSet struct {
	clients map[uint64][]deleteRange
}

type deleteRange struct {
	clock uint64
	len   uint64
}

func newDeleteSet() *deleteSet {
	return &deleteSet{clients: make(map[uint64][]deleteRange)}
}

func (ds *deleteSet) add(clientID, clock, length uint64) {
	ds.clients[clientID] = append(ds.clients[clientID], deleteRange{clock, length})
}

func (ds *deleteSet) isDeleted(id ID) bool {
	ranges := ds.clients[id.Client]
	for _, r := range ranges {
		if id.Clock >= r.clock && id.Clock < r.clock+r.len {
			return true
		}
	}
	return false
}
