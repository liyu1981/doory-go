package yjs

// XmlFragment represents a shared XML tree rooted in the document.
//
// In v0.1.x, XmlFragment is a PLACEHOLDER: only Len() is implemented.
// Mutations (child insertion, attribute setting, iteration) are planned
// for v0.2. For current work use Text, Map, or Array.
type XmlFragment struct {
	doc   *Doc
	name  string
	start *Item
}

func newXmlFragment(doc *Doc, name string) *XmlFragment {
	return &XmlFragment{doc: doc, name: name}
}

func (x *XmlFragment) sharedTypeName() string { return x.name }

// Len returns the number of non-deleted child nodes.
func (x *XmlFragment) Len() uint64 {
	x.doc.mu.Lock()
	defer x.doc.mu.Unlock()

	var n uint64
	for item := x.start; item != nil; item = item.Right {
		if !item.Deleted {
			n++
		}
	}
	return n
}
