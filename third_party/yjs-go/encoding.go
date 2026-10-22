package yjs

import (
	"sort"

	"github.com/CivNode/yjs-go/protocol"
)

// encodeUpdateV1 encodes a list of newly inserted items and deleted items
// into a Yjs v1 update binary.
func encodeUpdateV1(items []*Item, deletedItems []*Item) []byte {
	u := &protocol.UpdateV1{
		DeleteSet: make(map[uint64][]protocol.EncodedDeleteRange),
	}

	// Convert inserted items to EncodedItems.
	for _, item := range items {
		ei := itemToEncoded(item)
		if ei != nil {
			u.Items = append(u.Items, ei)
		}
	}

	// Build delete set from deleted items.
	for _, item := range deletedItems {
		u.DeleteSet[item.ID.Client] = append(u.DeleteSet[item.ID.Client],
			protocol.EncodedDeleteRange{Clock: item.ID.Clock, Len: item.Length})
	}

	data, err := protocol.EncodeUpdateV1(u)
	if err != nil {
		// Encoding failure is a programmer error; return empty update.
		return emptyUpdateV1()
	}
	return data
}

// emptyUpdateV1 returns a valid but empty v1 update (0 clients, 0 ds clients).
func emptyUpdateV1() []byte {
	data, _ := protocol.EncodeUpdateV1(&protocol.UpdateV1{
		DeleteSet: make(map[uint64][]protocol.EncodedDeleteRange),
	})
	return data
}

// itemToEncoded converts an in-memory Item to a protocol.EncodedItem for serialization.
func itemToEncoded(item *Item) *protocol.EncodedItem {
	if item == nil {
		return nil
	}

	infoByte := byte(contentKindToRef(item.Kind))
	var originLeft *protocol.EncodedID
	var originRight *protocol.EncodedID

	if item.OriginLeft != nil {
		infoByte |= 0x80
		originLeft = &protocol.EncodedID{Client: item.OriginLeft.Client, Clock: item.OriginLeft.Clock}
	}
	if item.OriginRight != nil {
		infoByte |= 0x40
		originRight = &protocol.EncodedID{Client: item.OriginRight.Client, Clock: item.OriginRight.Clock}
	}
	if item.ParentSub != nil {
		infoByte |= 0x20
	}

	ei := &protocol.EncodedItem{
		ClientID:    item.ID.Client,
		Clock:       item.ID.Clock,
		Length:      item.Length,
		InfoByte:    infoByte,
		OriginLeft:  originLeft,
		OriginRight: originRight,
		ContentRef:  contentKindToRef(item.Kind),
		ContentData: item.Content,
	}
	if item.Kind == contentType {
		// ContentData for a type item is the typeRef (writeItemContent case 7).
		if ref, ok := typeRefOfContent(item.Content); ok {
			ei.ContentData = ref
		}
	}

	if item.ParentSub != nil {
		s := *item.ParentSub
		ei.ParentSub = &s
	}

	// Parent info: only encoded when both origins are nil. Roots are named
	// (ParentIsYKey); a nested parent is named by its carrying item's ID.
	if originLeft == nil && originRight == nil {
		if st, ok := item.Parent.(SharedType); ok {
			if nid, hasItem := nodeItemID(item.Parent); hasItem {
				ei.ParentID = &protocol.EncodedID{Client: nid.Client, Clock: nid.Clock}
			} else {
				ei.ParentIsYKey = true
				ei.ParentYKey = st.sharedTypeName()
			}
		}
	}

	return ei
}

func contentKindToRef(k contentKind) byte {
	return byte(k)
}

// decodeUpdateV1 decodes a v1 update binary into items and a delete set.
// Returns items ready for integration and the parsed delete set.
func decodeUpdateV1(data []byte) ([]*Item, *deleteSet, error) {
	u, err := protocol.DecodeUpdateV1(data)
	if err != nil {
		return nil, nil, err
	}

	var items []*Item
	for _, ei := range u.Items {
		if ei.IsGC || ei.IsSkip {
			continue
		}
		item := encodedToItem(ei)
		if item != nil {
			items = append(items, item)
		}
	}

	ds := newDeleteSet()
	for clientID, ranges := range u.DeleteSet {
		for _, r := range ranges {
			ds.add(clientID, r.Clock, r.Len)
		}
	}

	return items, ds, nil
}

// encodedToItem converts a protocol.EncodedItem back to an in-memory Item.
// The item's Parent is nil here; it is resolved during integration.
func encodedToItem(ei *protocol.EncodedItem) *Item {
	if ei == nil || ei.IsGC || ei.IsSkip {
		return nil
	}

	item := &Item{
		ID:     ID{ei.ClientID, ei.Clock},
		Length: ei.Length,
		Kind:   contentKind(ei.ContentRef),
	}

	if ei.OriginLeft != nil {
		oid := ID{ei.OriginLeft.Client, ei.OriginLeft.Clock}
		item.OriginLeft = &oid
	}
	if ei.OriginRight != nil {
		oid := ID{ei.OriginRight.Client, ei.OriginRight.Clock}
		item.OriginRight = &oid
	}
	if ei.ParentSub != nil {
		s := *ei.ParentSub
		item.ParentSub = &s
	}

	// Store the parent name for resolution during integration.
	if ei.ParentIsYKey {
		item.parentName = ei.ParentYKey
	}
	if ei.ParentID != nil {
		pid := ID{ei.ParentID.Client, ei.ParentID.Clock}
		item.parentID = &pid
	}

	switch ei.ContentRef {
	case 4: // string
		if s, ok := ei.ContentData.(string); ok {
			item.Content = s
		}
	case 8: // any
		item.Content = ei.ContentData
	case 1: // deleted
		item.Deleted = true
		if n, ok := ei.ContentData.(uint64); ok {
			item.Length = n
		}
	case 3: // binary
		item.Content = ei.ContentData
	default:
		item.Content = ei.ContentData
	}

	return item
}

// EncodeStateVector returns the binary v1 state vector for document d.
func EncodeStateVector(d *Doc) ([]byte, error) {
	d.mu.Lock()
	sv := d.store.stateVector()
	d.mu.Unlock()
	return protocol.EncodeStateVectorV1(sv)
}

// EncodeStateAsUpdate encodes the full document state as a v1 update,
// optionally filtered to only items after encodedStateVector.
func EncodeStateAsUpdate(d *Doc, encodedStateVector []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Parse the remote state vector.
	remoteSV := make(map[uint64]uint64)
	if len(encodedStateVector) > 0 {
		sv, err := protocol.DecodeStateVectorV1(encodedStateVector)
		if err != nil {
			return nil, err
		}
		remoteSV = sv
	}

	u := &protocol.UpdateV1{
		DeleteSet: make(map[uint64][]protocol.EncodedDeleteRange),
	}

	// Collect all items whose clock >= remoteSV[client].
	for clientID, items := range d.store.clients {
		remoteState := remoteSV[clientID]
		// Find first item at or after remoteState.
		startIdx := sort.Search(len(items), func(i int) bool {
			return items[i].ID.Clock+items[i].Length > remoteState
		})
		for i := startIdx; i < len(items); i++ {
			ei := itemToEncoded(items[i])
			if ei != nil {
				u.Items = append(u.Items, ei)
			}
			if items[i].Deleted {
				// The delete set carries deletions (items encode without a
				// deleted flag); a snapshot without it would resurrect them.
				u.DeleteSet[clientID] = append(u.DeleteSet[clientID], protocol.EncodedDeleteRange{
					Clock: items[i].ID.Clock,
					Len:   items[i].Length,
				})
			}
		}
	}

	return protocol.EncodeUpdateV1(u)
}
