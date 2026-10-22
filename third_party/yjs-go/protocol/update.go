package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sort"
)

// Update-v1 binary layout (matches Yjs lib0/encoding exactly):
//
// update ::=
//   varuint(numClients)
//   per-client:
//     varuint(numStructs)
//     varuint(clientID)      [via writeClient = writeVarUint]
//     varuint(firstClock)
//     per-struct: encodedStruct
//   deleteSet:
//     varuint(numDsClients)
//     per-client:
//       varuint(clientID)
//       varuint(numRanges)
//       per-range:
//         varuint(clock)   [v1: absolute]
//         varuint(len)     [v1: exact]
//
// Info byte per item:
//   bits 0-4: content ref (0=GC, 1=deleted, 3=binary, 4=string, 5=embed,
//             6=format, 7=type, 8=any, 9=doc, 10=skip)
//   bit 5 (0x20): parentSub present
//   bit 6 (0x40): rightOrigin present
//   bit 7 (0x80): originLeft present
//
// Parent info (only when bits 6+7 are both 0):
//   varuint(isYKey): 1->varString(ykey), 0->varuint(client)+varuint(clock)
// Followed by varString(parentSub) when bit 5 set - but only in that same
// no-origin case: with a left origin the key lives in the origin's slot and
// is derived at integration (reference Yjs/yrs behavior; probed against
// pycrdt fixtures in internal/crdt).

// EncodedItem is a fully decoded struct from a v1 update.
type EncodedItem struct {
	ClientID    uint64
	Clock       uint64
	Length      uint64
	IsGC        bool
	IsSkip      bool
	InfoByte    byte
	OriginLeft  *EncodedID
	OriginRight *EncodedID
	// Parent (only when OriginLeft == nil && OriginRight == nil):
	ParentIsYKey bool
	ParentYKey   string
	ParentID     *EncodedID
	ParentSub    *string
	ContentRef   byte
	ContentData  interface{}
}

// EncodedID is a (clientID, clock) pair.
type EncodedID struct {
	Client uint64
	Clock  uint64
}

// EncodedDeleteRange is a [clock, clock+len) deletion range.
type EncodedDeleteRange struct {
	Clock uint64
	Len   uint64
}

// UpdateV1 is the fully decoded content of a Yjs v1 update message.
type UpdateV1 struct {
	Items     []*EncodedItem
	DeleteSet map[uint64][]EncodedDeleteRange
}

// DecodeUpdateV1 decodes a v1 update from raw bytes.
func DecodeUpdateV1(data []byte) (*UpdateV1, error) {
	return readUpdateV1(bytes.NewReader(data))
}

// EncodeUpdateV1 encodes a v1 update to bytes.
func EncodeUpdateV1(u *UpdateV1) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeUpdateV1(&buf, u); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeStateVectorV1 serializes a state vector in v1 format.
// Format: varuint(n), then n*(varuint(clientID), varuint(clock)), sorted descending clientID.
func EncodeStateVectorV1(sv map[uint64]uint64) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteVarUint(&buf, uint64(len(sv))); err != nil {
		return nil, err
	}
	type kv struct{ k, v uint64 }
	pairs := make([]kv, 0, len(sv))
	for k, v := range sv {
		pairs = append(pairs, kv{k, v})
	}
	// Sort descending by clientID (matches Yjs).
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k > pairs[j].k })
	for _, p := range pairs {
		if err := WriteVarUint(&buf, p.k); err != nil {
			return nil, err
		}
		if err := WriteVarUint(&buf, p.v); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// DecodeStateVectorV1 parses a state vector from v1 bytes.
func DecodeStateVectorV1(data []byte) (map[uint64]uint64, error) {
	r := bytes.NewReader(data)
	n, err := ReadVarUint(r)
	if err != nil {
		return nil, fmt.Errorf("state vector count: %w", err)
	}
	sv := make(map[uint64]uint64, n)
	for i := uint64(0); i < n; i++ {
		client, err := ReadVarUint(r)
		if err != nil {
			return nil, fmt.Errorf("state vector client[%d]: %w", i, err)
		}
		clock, err := ReadVarUint(r)
		if err != nil {
			return nil, fmt.Errorf("state vector clock[%d]: %w", i, err)
		}
		sv[client] = clock
	}
	return sv, nil
}

func readUpdateV1(r io.Reader) (*UpdateV1, error) {
	u := &UpdateV1{DeleteSet: make(map[uint64][]EncodedDeleteRange)}

	numClients, err := ReadVarUint(r)
	if err != nil {
		return nil, fmt.Errorf("update numClients: %w", err)
	}

	for ci := uint64(0); ci < numClients; ci++ {
		numStructs, err := ReadVarUint(r)
		if err != nil {
			return nil, fmt.Errorf("numStructs[%d]: %w", ci, err)
		}
		clientID, err := ReadVarUint(r)
		if err != nil {
			return nil, fmt.Errorf("clientID[%d]: %w", ci, err)
		}
		clock, err := ReadVarUint(r)
		if err != nil {
			return nil, fmt.Errorf("clock[%d]: %w", ci, err)
		}

		for si := uint64(0); si < numStructs; si++ {
			item, n, err := readEncodedItem(r, clientID, clock)
			if err != nil {
				return nil, fmt.Errorf("item[%d/%d]: %w", ci, si, err)
			}
			clock += n
			u.Items = append(u.Items, item)
		}
	}

	numDsClients, err := ReadVarUint(r)
	if err != nil {
		return nil, fmt.Errorf("ds numClients: %w", err)
	}
	for di := uint64(0); di < numDsClients; di++ {
		clientID, err := ReadVarUint(r)
		if err != nil {
			return nil, fmt.Errorf("ds clientID[%d]: %w", di, err)
		}
		numRanges, err := ReadVarUint(r)
		if err != nil {
			return nil, fmt.Errorf("ds numRanges[%d]: %w", di, err)
		}
		for ri := uint64(0); ri < numRanges; ri++ {
			clock, err := ReadVarUint(r)
			if err != nil {
				return nil, fmt.Errorf("ds clock[%d/%d]: %w", di, ri, err)
			}
			length, err := ReadVarUint(r)
			if err != nil {
				return nil, fmt.Errorf("ds len[%d/%d]: %w", di, ri, err)
			}
			u.DeleteSet[clientID] = append(u.DeleteSet[clientID], EncodedDeleteRange{clock, length})
		}
	}

	return u, nil
}

func readEncodedItem(r io.Reader, clientID, clock uint64) (*EncodedItem, uint64, error) {
	infoBuf := make([]byte, 1)
	if _, err := io.ReadFull(r, infoBuf); err != nil {
		return nil, 0, fmt.Errorf("info byte: %w", err)
	}
	info := infoBuf[0]
	contentRef := info & 0x1f

	switch contentRef {
	case 0: // GC
		length, err := ReadVarUint(r)
		if err != nil {
			return nil, 0, fmt.Errorf("GC len: %w", err)
		}
		return &EncodedItem{ClientID: clientID, Clock: clock, Length: length, IsGC: true, InfoByte: info}, length, nil
	case 10: // Skip
		length, err := ReadVarUint(r)
		if err != nil {
			return nil, 0, fmt.Errorf("Skip len: %w", err)
		}
		return &EncodedItem{ClientID: clientID, Clock: clock, Length: length, IsSkip: true, InfoByte: info}, length, nil
	}

	item := &EncodedItem{ClientID: clientID, Clock: clock, InfoByte: info, ContentRef: contentRef}
	hasOriginLeft := (info & 0x80) != 0
	hasOriginRight := (info & 0x40) != 0
	hasParentSub := (info & 0x20) != 0

	if hasOriginLeft {
		eid, err := readEncodedID(r)
		if err != nil {
			return nil, 0, fmt.Errorf("originLeft: %w", err)
		}
		item.OriginLeft = eid
	}
	if hasOriginRight {
		eid, err := readEncodedID(r)
		if err != nil {
			return nil, 0, fmt.Errorf("originRight: %w", err)
		}
		item.OriginRight = eid
	}
	if !hasOriginLeft && !hasOriginRight {
		isYKey, err := ReadVarUint(r)
		if err != nil {
			return nil, 0, fmt.Errorf("parentInfo flag: %w", err)
		}
		if isYKey == 1 {
			ykey, err := ReadVarString(r)
			if err != nil {
				return nil, 0, fmt.Errorf("parentYKey: %w", err)
			}
			item.ParentIsYKey = true
			item.ParentYKey = ykey
		} else {
			eid, err := readEncodedID(r)
			if err != nil {
				return nil, 0, fmt.Errorf("parentID: %w", err)
			}
			item.ParentID = eid
		}
		if hasParentSub {
			// The map key is encoded only with the parent (no origin case);
			// with a left origin it is inherited from the origin item.
			sub, err := ReadVarString(r)
			if err != nil {
				return nil, 0, fmt.Errorf("parentSub: %w", err)
			}
			item.ParentSub = &sub
		}
	}

	length, err := readItemContent(r, item)
	if err != nil {
		return nil, 0, fmt.Errorf("content (ref=%d): %w", contentRef, err)
	}
	item.Length = length
	return item, length, nil
}

func readEncodedID(r io.Reader) (*EncodedID, error) {
	client, err := ReadVarUint(r)
	if err != nil {
		return nil, err
	}
	clock, err := ReadVarUint(r)
	if err != nil {
		return nil, err
	}
	return &EncodedID{client, clock}, nil
}

func readItemContent(r io.Reader, item *EncodedItem) (uint64, error) {
	switch item.ContentRef {
	case 1: // ContentDeleted
		n, err := ReadVarUint(r)
		if err != nil {
			return 0, err
		}
		item.ContentData = n
		return n, nil
	case 4: // ContentString
		s, err := ReadVarString(r)
		if err != nil {
			return 0, err
		}
		item.ContentData = s
		return uint64(utf16Len(s)), nil
	case 8: // ContentAny — an array of any-values; length is written first.
		count, err := ReadVarUint(r)
		if err != nil {
			return 0, err
		}
		arr := make([]interface{}, count)
		for i := uint64(0); i < count; i++ {
			v, err := ReadAny(r)
			if err != nil {
				return 0, fmt.Errorf("any[%d]: %w", i, err)
			}
			arr[i] = v
		}
		// Expose as a slice of length count; or as the single value when count==1
		// for ease of use by callers (the common case for Map.set).
		if count == 1 {
			item.ContentData = arr[0]
		} else {
			item.ContentData = arr
		}
		return count, nil
	case 3: // ContentBinary
		b, err := ReadVarBytes(r)
		if err != nil {
			return 0, err
		}
		item.ContentData = b
		return 1, nil
	case 5: // ContentEmbed (JSON string)
		s, err := ReadVarString(r)
		if err != nil {
			return 0, err
		}
		item.ContentData = s
		return 1, nil
	case 6: // ContentFormat (key + any; does not advance clock)
		key, err := ReadVarString(r)
		if err != nil {
			return 0, err
		}
		val, err := ReadAny(r)
		if err != nil {
			return 0, err
		}
		item.ContentData = map[string]interface{}{"key": key, "value": val}
		return 0, nil
	case 7: // ContentType (nested Y type)
		typeRef, err := ReadVarUint(r)
		if err != nil {
			return 0, err
		}
		item.ContentData = typeRef
		return 1, nil
	case 2: // ContentJSON (legacy)
		s, err := ReadVarString(r)
		if err != nil {
			return 0, err
		}
		item.ContentData = s
		return 1, nil
	case 9: // ContentDoc
		guid, err := ReadVarString(r)
		if err != nil {
			return 0, err
		}
		opts, err := ReadAny(r)
		if err != nil {
			return 0, err
		}
		item.ContentData = map[string]interface{}{"guid": guid, "opts": opts}
		return 1, nil
	default:
		return 0, fmt.Errorf("unknown content ref %d", item.ContentRef)
	}
}

// utf16Len returns the UTF-16 length of s (number of code units).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++ // supplementary character = surrogate pair
		}
	}
	return n
}

// ReadAny reads a lib0-encoded "any" value from r.
func ReadAny(r io.Reader) (interface{}, error) {
	tagBuf := make([]byte, 1)
	if _, err := io.ReadFull(r, tagBuf); err != nil {
		return nil, fmt.Errorf("any tag: %w", err)
	}
	switch tagBuf[0] {
	case 127: // undefined
		return nil, nil
	case 126: // null
		return nil, nil
	case 125: // integer
		n, err := ReadVarUint(r)
		if err != nil {
			return nil, err
		}
		return int64(n), nil
	case 124: // float32
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		return math.Float32frombits(binary.BigEndian.Uint32(b)), nil
	case 123: // float64
		b := make([]byte, 8)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
	case 122: // bigint
		n, err := ReadVarUint(r)
		if err != nil {
			return nil, err
		}
		return n, nil
	case 121: // false
		return false, nil
	case 120: // true
		return true, nil
	case 119: // string
		return ReadVarString(r)
	case 118: // object
		n, err := ReadVarUint(r)
		if err != nil {
			return nil, err
		}
		obj := make(map[string]interface{}, n)
		for i := uint64(0); i < n; i++ {
			k, err := ReadVarString(r)
			if err != nil {
				return nil, err
			}
			v, err := ReadAny(r)
			if err != nil {
				return nil, err
			}
			obj[k] = v
		}
		return obj, nil
	case 117: // array
		n, err := ReadVarUint(r)
		if err != nil {
			return nil, err
		}
		arr := make([]interface{}, n)
		for i := uint64(0); i < n; i++ {
			v, err := ReadAny(r)
			if err != nil {
				return nil, err
			}
			arr[i] = v
		}
		return arr, nil
	case 116: // Uint8Array
		return ReadVarBytes(r)
	default:
		return nil, fmt.Errorf("unknown any tag 0x%02x", tagBuf[0])
	}
}

// WriteAny writes a lib0-encoded "any" value to w.
func WriteAny(w io.Writer, v interface{}) error {
	if v == nil {
		_, err := w.Write([]byte{127})
		return err
	}
	switch val := v.(type) {
	case bool:
		if val {
			_, err := w.Write([]byte{120})
			return err
		}
		_, err := w.Write([]byte{121})
		return err
	case string:
		if _, err := w.Write([]byte{119}); err != nil {
			return err
		}
		return WriteVarString(w, val)
	case int:
		if _, err := w.Write([]byte{125}); err != nil {
			return err
		}
		return WriteVarUint(w, uint64(val))
	case int64:
		if _, err := w.Write([]byte{125}); err != nil {
			return err
		}
		return WriteVarUint(w, uint64(val))
	case uint64:
		if _, err := w.Write([]byte{122}); err != nil {
			return err
		}
		return WriteVarUint(w, val)
	case float32:
		if _, err := w.Write([]byte{124}); err != nil {
			return err
		}
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, math.Float32bits(val))
		_, err := w.Write(b)
		return err
	case float64:
		if _, err := w.Write([]byte{123}); err != nil {
			return err
		}
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, math.Float64bits(val))
		_, err := w.Write(b)
		return err
	case map[string]interface{}:
		if _, err := w.Write([]byte{118}); err != nil {
			return err
		}
		if err := WriteVarUint(w, uint64(len(val))); err != nil {
			return err
		}
		for k, v2 := range val {
			if err := WriteVarString(w, k); err != nil {
				return err
			}
			if err := WriteAny(w, v2); err != nil {
				return err
			}
		}
		return nil
	case []interface{}:
		if _, err := w.Write([]byte{117}); err != nil {
			return err
		}
		if err := WriteVarUint(w, uint64(len(val))); err != nil {
			return err
		}
		for _, v2 := range val {
			if err := WriteAny(w, v2); err != nil {
				return err
			}
		}
		return nil
	case []byte:
		if _, err := w.Write([]byte{116}); err != nil {
			return err
		}
		return WriteVarBytes(w, val)
	default:
		_, err := w.Write([]byte{127})
		return err
	}
}

func writeUpdateV1(w io.Writer, u *UpdateV1) error {
	// Group by clientID.
	type clientGroup struct {
		clientID uint64
		items    []*EncodedItem
	}
	clientMap := make(map[uint64]*clientGroup)
	for _, item := range u.Items {
		g := clientMap[item.ClientID]
		if g == nil {
			g = &clientGroup{clientID: item.ClientID}
			clientMap[item.ClientID] = g
		}
		g.items = append(g.items, item)
	}

	groups := make([]*clientGroup, 0, len(clientMap))
	for _, g := range clientMap {
		groups = append(groups, g)
	}
	// Sort descending by clientID.
	sort.Slice(groups, func(i, j int) bool { return groups[i].clientID > groups[j].clientID })

	if err := WriteVarUint(w, uint64(len(groups))); err != nil {
		return err
	}
	for _, g := range groups {
		if err := WriteVarUint(w, uint64(len(g.items))); err != nil {
			return err
		}
		if err := WriteVarUint(w, g.clientID); err != nil {
			return err
		}
		firstClock := uint64(0)
		if len(g.items) > 0 {
			firstClock = g.items[0].Clock
		}
		if err := WriteVarUint(w, firstClock); err != nil {
			return err
		}
		for _, item := range g.items {
			if err := writeEncodedItem(w, item); err != nil {
				return err
			}
		}
	}

	// Delete set.
	type dsEntry struct {
		clientID uint64
		ranges   []EncodedDeleteRange
	}
	dsEntries := make([]dsEntry, 0, len(u.DeleteSet))
	for cid, ranges := range u.DeleteSet {
		dsEntries = append(dsEntries, dsEntry{cid, ranges})
	}
	sort.Slice(dsEntries, func(i, j int) bool { return dsEntries[i].clientID > dsEntries[j].clientID })
	if err := WriteVarUint(w, uint64(len(dsEntries))); err != nil {
		return err
	}
	for _, entry := range dsEntries {
		if err := WriteVarUint(w, entry.clientID); err != nil {
			return err
		}
		if err := WriteVarUint(w, uint64(len(entry.ranges))); err != nil {
			return err
		}
		for _, r := range entry.ranges {
			if err := WriteVarUint(w, r.Clock); err != nil {
				return err
			}
			if err := WriteVarUint(w, r.Len); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeEncodedItem(w io.Writer, item *EncodedItem) error {
	if item.IsGC {
		if _, err := w.Write([]byte{0}); err != nil {
			return err
		}
		return WriteVarUint(w, item.Length)
	}
	if item.IsSkip {
		if _, err := w.Write([]byte{10}); err != nil {
			return err
		}
		return WriteVarUint(w, item.Length)
	}

	if _, err := w.Write([]byte{item.InfoByte}); err != nil {
		return err
	}
	if item.OriginLeft != nil {
		if err := writeEncodedID(w, item.OriginLeft); err != nil {
			return err
		}
	}
	if item.OriginRight != nil {
		if err := writeEncodedID(w, item.OriginRight); err != nil {
			return err
		}
	}
	if item.OriginLeft == nil && item.OriginRight == nil {
		if item.ParentIsYKey {
			if err := WriteVarUint(w, 1); err != nil {
				return err
			}
			if err := WriteVarString(w, item.ParentYKey); err != nil {
				return err
			}
		} else if item.ParentID != nil {
			if err := WriteVarUint(w, 0); err != nil {
				return err
			}
			if err := writeEncodedID(w, item.ParentID); err != nil {
				return err
			}
		}
		if item.ParentSub != nil {
			// Same rule as the parent: the map key is written only when no
			// origin names it (reference format; the decoder derives it
			// from the left neighbor otherwise).
			if err := WriteVarString(w, *item.ParentSub); err != nil {
				return err
			}
		}
	}
	return writeItemContent(w, item)
}

func writeEncodedID(w io.Writer, eid *EncodedID) error {
	if err := WriteVarUint(w, eid.Client); err != nil {
		return err
	}
	return WriteVarUint(w, eid.Clock)
}

func writeItemContent(w io.Writer, item *EncodedItem) error {
	switch item.ContentRef {
	case 1:
		return WriteVarUint(w, item.Length)
	case 4:
		return WriteVarString(w, item.ContentData.(string))
	case 8: // ContentAny: varuint(count) + count*any
		// If ContentData is already a slice, write each element.
		if arr, ok := item.ContentData.([]interface{}); ok {
			if err := WriteVarUint(w, uint64(len(arr))); err != nil {
				return err
			}
			for _, v := range arr {
				if err := WriteAny(w, v); err != nil {
					return err
				}
			}
			return nil
		}
		// Single value (the common case from Map.set).
		if err := WriteVarUint(w, 1); err != nil {
			return err
		}
		return WriteAny(w, item.ContentData)
	case 3:
		return WriteVarBytes(w, item.ContentData.([]byte))
	case 5: // ContentEmbed — JSON string
		if s, ok := item.ContentData.(string); ok {
			return WriteVarString(w, s)
		}
		return WriteVarString(w, "null")
	case 6: // ContentFormat — key + any value
		if m, ok := item.ContentData.(map[string]interface{}); ok {
			key, _ := m["key"].(string)
			if err := WriteVarString(w, key); err != nil {
				return err
			}
			return WriteAny(w, m["value"])
		}
		return fmt.Errorf("ContentFormat: expected map[string]interface{}")
	case 7:
		return WriteVarUint(w, item.ContentData.(uint64))
	default:
		return fmt.Errorf("unsupported content ref for encoding: %d", item.ContentRef)
	}
}
