// The bounded update scanner (apps/collab/protocol.py scan_update): rejects
// malformed or abusive Y updates WITHOUT applying them.
package yproto

import "sort"

// UpdateScan is protocol.UpdateScan: what one update would do.
type UpdateScan struct {
	Structs    int
	Types      int
	HasItems   bool
	HasDeletes bool
	// Extents: per client, the clock end this update reaches.
	Extents map[uint64]uint64
	// NewClients: clients the document has not heard of before; an honest
	// connection introduces one (its own).
	NewClients map[uint64]bool
	// Originless: items that start a map entry without naming a
	// predecessor, per (parent, key) — the engine walks an entry's whole
	// history for such items.
	Originless map[string]int
	// RightOnly: items that name a right neighbour but no left one.
	RightOnly int
	// InsideRefs: references into a multi-value struct of this same update.
	InsideRefs int
}

// key for Originless counts: parent reference + entry key, without inventing
// names for the map (the room hashes this string the way Python hashes the
// tuple — counts only, never names).
type entryKey struct {
	parentClient uint64
	parentClock  uint64
	root         string
	sub          string
}

func (k entryKey) String() string {
	return k.root + "\x00" + u64(k.parentClient) + ":" + u64(k.parentClock) + "\x00" + k.sub
}

func u64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// ScanUpdate ports protocol.scan_update: check a Y update (v1) against the
// state it will be applied to (`known` is that document's state vector).
// Rejects: malformed data, values nested deeper than maxAnyDepth, root types
// other than allowedRoots, XML types and sub-documents, Skip structs, and any
// reference to history the document does not have (such an update would sit
// in the engine's pending buffer forever).
func ScanUpdate(update []byte, known map[uint64]uint64, allowedRoots map[string]bool, maxAnyDepth int) (scan UpdateScan, err error) {
	scan = UpdateScan{
		NewClients: map[uint64]bool{},
		Originless: map[string]int{},
	}
	defer func() {
		if p := recover(); p != nil {
			if pe, ok := p.(*ProtocolError); ok {
				scan, err = UpdateScan{}, pe
				return
			}
			panic(p)
		}
	}()
	r := NewReader(update)
	blocks := map[uint64][2]uint64{}
	var references []parentRef
	type span struct{ first, length uint64 }
	longStructs := map[uint64][]span{}

	numClients := boundedCount(r)
	for ci := uint64(0); ci < numClients; ci++ {
		structs := boundedCount(r)
		client := r.VarUint()
		start := r.VarUint()
		prev, dup := blocks[client]
		_ = prev
		if dup || start > known[client] || structs == 0 {			panic(&ProtocolError{Message: "The update has a gap in its history."})
		}
		if _, seen := known[client]; !seen {
			scan.NewClients[client] = true
		}
		clock := start
		for si := uint64(0); si < structs; si++ {
			length := scanStruct(r, &scan, &references, allowedRoots, maxAnyDepth, client)
			if length > 1 {
				longStructs[client] = append(longStructs[client], span{clock, length})
			}
			clock += length
		}
		blocks[client] = [2]uint64{start, clock}
	}
	extents := map[uint64]uint64{}
	for client, block := range blocks {
		knownEnd := known[client]
		if block[1] > knownEnd {
			extents[client] = block[1]
		} else {
			extents[client] = knownEnd
		}
	}
	scan.Extents = extents

	reaches := func(client, clock uint64) bool {
		end := extents[client]
		if known[client] > end {
			end = known[client]
		}
		return clock < end
	}
	for _, ref := range references {
		if !reaches(ref.client, ref.clock) {
			panic(&ProtocolError{Message: "The update refers to history the document does not have."})
		}
	}
	for _, ref := range references {
		spans := longStructs[ref.client]
		if len(spans) == 0 {
			continue
		}
		// bisect_right(spans, (clock, +inf)) - 1
		idx := sort.Search(len(spans), func(i int) bool {
			s := spans[i]
			return s.first > ref.clock || (s.first == ref.clock && s.length > ^uint64(0))
		})
		if idx == 0 {
			continue
		}
		s := spans[idx-1]
		if s.first <= ref.clock && ref.clock < s.first+s.length {
			scan.InsideRefs++
		}
	}
	numDsClients := boundedCount(r)
	for di := uint64(0); di < numDsClients; di++ {
		client := r.VarUint()
		numRanges := boundedCount(r)
		for ri := uint64(0); ri < numRanges; ri++ {
			clock := r.VarUint()
			length := r.VarUint()
			scan.HasDeletes = true
			if length == 0 || !reaches(client, clock+length-1) {
				panic(&ProtocolError{Message: "The update deletes history the document does not have."})
			}
		}
	}
	if !r.AtEnd() {
		panic(&ProtocolError{Message: "Trailing bytes after a Y update."})
	}
	return scan, nil
}

func boundedCount(r *Reader) uint64 {
	count := r.VarUint()
	if count > uint64(r.Remaining()) {
		panic(&ProtocolError{Message: "Invalid count."})
	}
	return count
}

type parentRef struct {
	client uint64
	clock  uint64
	root   string
	sub    string
}

// scanStruct ports protocol._scan_struct: returns the clock ticks it occupies.
func scanStruct(r *Reader, scan *UpdateScan, references *[]parentRef,
	allowedRoots map[string]bool, maxAnyDepth int, client uint64,
) uint64 {
	info := r.Uint8()
	ref := info & 0x1f
	scan.Structs++
	if ref == 0 { // GC
		length := r.VarUint()
		if length == 0 {
			panic(&ProtocolError{Message: "Empty struct."})
		}
		return length
	}
	if ref == refSkip {
		panic(&ProtocolError{Message: "The update has a gap in its history."})
	}
	scan.HasItems = true
	hasOrigin, hasRightOrigin := info&0x80 != 0, info&0x40 != 0
	if hasRightOrigin && !hasOrigin {
		scan.RightOnly++
	}
	pr := parentRef{client: client}
	if hasOrigin {
		pr.client, pr.clock = r.VarUint(), r.VarUint()
		*references = append(*references, pr)
	}
	if hasRightOrigin {
		c2, cl2 := r.VarUint(), r.VarUint()
		*references = append(*references, parentRef{client: c2, clock: cl2})
	}
	if !hasOrigin && !hasRightOrigin {
		if r.VarUint() == 1 {
			root := r.VarString()
			if !allowedRoots[root] {
				panic(&ProtocolError{Message: "The update writes to an unknown root type."})
			}
			pr.root = root
		} else {
			pr.client, pr.clock = r.VarUint(), r.VarUint()
			*references = append(*references, pr)
		}
		if info&0x20 != 0 {
			key := r.VarBytes()
			scan.Originless[entryKey{pr.client, pr.clock, pr.root, string(key)}.String()]++
		}
	}
	return scanContent(r, scan, ref, maxAnyDepth)
}

// scanContent ports protocol._scan_content.
func scanContent(r *Reader, scan *UpdateScan, ref byte, maxAnyDepth int) uint64 {
	switch ref {
	case refDeleted:
		return positive(r.VarUint())
	case refJSON:
		count := positive(boundedCount(r))
		for i := uint64(0); i < count; i++ {
			r.VarBytes()
		}
		return count
	case refBinary:
		r.VarBytes()
		return 1
	case refString:
		return positive(uint64(r.SkipVarString()))
	case refEmbed:
		r.VarBytes()
		return 1
	case refFormat:
		r.VarBytes()
		r.VarBytes()
		return 1
	case refType:
		if !AllowedTypeRefs[r.VarUint()] {
			panic(&ProtocolError{Message: "The update creates a type that is not allowed."})
		}
		scan.Types++
		return 1
	case refAny:
		count := positive(boundedCount(r))
		for i := uint64(0); i < count; i++ {
			skipAny(r, maxAnyDepth)
		}
		return count
	default:
		panic(&ProtocolError{Message: "Unsupported content in the update."})
	}
}

func positive(value uint64) uint64 {
	if value == 0 {
		panic(&ProtocolError{Message: "Empty struct."})
	}
	return value
}

// skipAny ports protocol._skip_any: skip one lib0 `any` value with an
// explicit stack — no recursion, depth and counts bounded.
func skipAny(r *Reader, maxDepth int) {
	remaining := []int{1}
	objects := []bool{false}
	for len(remaining) > 0 {
		if remaining[len(remaining)-1] == 0 {
			remaining = remaining[:len(remaining)-1]
			objects = objects[:len(objects)-1]
			continue
		}
		remaining[len(remaining)-1]--
		if objects[len(objects)-1] {
			r.VarBytes()
		}
		tag := r.Uint8()
		if n, ok := anyFixed[tag]; ok {
			if n > 0 {
				r.Skip(n)
			}
			continue
		}
		switch tag {
		case anyArray, anyObject:
			count := r.VarUint()
			if count > uint64(r.Remaining()) {
				panic(&ProtocolError{Message: "Invalid collection length."})
			}
			if len(remaining) >= maxDepth {
				panic(&ProtocolError{Message: "A value is nested too deeply."})
			}
			remaining = append(remaining, int(count))
			objects = append(objects, tag == anyObject)
		case anyVarint:
			r.SkipVarint()
		case anyString, anyBytes:
			r.VarBytes()
		default:
			panic(&ProtocolError{Message: "Unknown value type."})
		}
	}
}
