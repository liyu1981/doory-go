package yjs

// contentKind distinguishes the payload of an Item.
type contentKind uint8

const (
	contentString  contentKind = 4 // ContentString ref in Yjs
	contentAny     contentKind = 8 // ContentAny
	contentType    contentKind = 7 // ContentType (nested type)
	contentDeleted contentKind = 1 // ContentDeleted (GC'd)
	contentBinary  contentKind = 3 // ContentBinary
	contentEmbed   contentKind = 5 // ContentEmbed
	contentFormat  contentKind = 6 // ContentFormat
)

// Item is a node in the doubly-linked list that forms a Yjs type's content.
// Each item corresponds to one Insert operation with its causal context
// (originLeft, originRight) for LSST-based conflict resolution.
type Item struct {
	ID ID

	// Left and Right are the previous/next live items in insertion order.
	Left  *Item
	Right *Item

	// OriginLeft and OriginRight are the IDs of the items that were to the
	// left and right of this item when it was created (causal context for
	// the LSST conflict resolution algorithm).
	OriginLeft  *ID
	OriginRight *ID

	// Parent is the YType that owns this item.
	Parent interface{} // *Text | *Map | *Array | etc.

	// ParentSub is the map key when this item lives inside a Map.
	ParentSub *string

	// Content: one of string, []interface{}, etc.
	Kind    contentKind
	Content interface{} // string for contentString, interface{} for contentAny, etc.
	Length  uint64

	Deleted bool

	// parentName holds the shared type name from a decoded update, used during
	// integration to look up the actual SharedType from the Doc.
	parentName string

	// parentID names a non-root parent by item ID (wire: ParentIsYKey false,
	// no origin). For nested types the parent item's Content is the shared node.
	parentID *ID
}

// typeRefOfContent maps a shared node to the Yjs ContentType ref
// (0=Array, 1=Map, 2=Text - the set apps/collab's scanner allows).
func typeRefOfContent(content interface{}) (uint64, bool) {
	switch content.(type) {
	case *Array:
		return 0, true
	case *Map:
		return 1, true
	case *Text:
		return 2, true
	default:
		return 0, false
	}
}

// setNodeItemID records the carrying item on a nested node when it is
// inserted as a value (for ParentID encoding and parent resolution).
func setNodeItemID(content interface{}, id ID) {
	switch n := content.(type) {
	case *Array:
		n.itemID = &id
	case *Map:
		n.itemID = &id
	case *Text:
		n.itemID = &id
	}
}

// nodeItemID returns the carrying item's ID for a nested node (roots: nil).
func nodeItemID(content interface{}) (*ID, bool) {
	switch n := content.(type) {
	case *Array:
		return n.itemID, n.itemID != nil
	case *Map:
		return n.itemID, n.itemID != nil
	case *Text:
		return n.itemID, n.itemID != nil
	default:
		return nil, false
	}
}

// NewNestedArray / NewNestedText: see NewNestedMap (map.go).
func NewNestedArray(doc *Doc) *Array { return newArray(doc, "") }
func NewNestedText(doc *Doc) *Text   { return newText(doc, "") }

// content returns the string value for contentString items.
func (it *Item) strContent() string {
	if it.Kind == contentString {
		return it.Content.(string)
	}
	return ""
}
