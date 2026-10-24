// Package crdt wraps github.com/CivNode/yjs-go as the collab engine.
//
// Substitution (spike S0): upstream runs pycrdt (the Python binding for the
// Rust yrs engine). yjs-go is a pure-Go implementation of the same Yjs v1
// wire format. Like the CDP chromium driver (M9), the substitution is
// recorded and CONFORMANCE-PROVEN against oracle-generated fixtures:
// test/parity/probes/0011-collab-crdt (pycrdt 0.14.10, pinned upstream
// commit) — snapshots, state vectors, incremental edit streams, concurrent
// merges and compaction all cross-checked in both directions.
//
// The engine surface mirrors what apps/collab uses of pycrdt: Doc with
// apply_update/get_state/get_update, the three root types (meta Map,
// pageOrder Array, pages Map) and plain-data conversion.
// Ports backend/apps/collab/crdt.py
package crdt

import (
	"encoding/json"
	"fmt"

	yjsgo "github.com/CivNode/yjs-go"
)

// Doc is the engine document (yjs-go's Doc).
type Doc = yjsgo.Doc

// NewDoc ports schema.new_doc: an empty document with the three roots
// declared, so a client's mismatching root is ignored.
func NewDoc() *Doc {
	d := yjsgo.NewDoc()
	_ = d.GetMap("meta")
	_ = d.GetArray("pageOrder")
	_ = d.GetMap("pages")
	return d
}

// ApplyUpdate ports pycrdt Doc.apply_update (v1 updates, no origin).
func ApplyUpdate(d *Doc, update []byte) error { return yjsgo.ApplyUpdate(d, update, nil) }

// StateVector ports Doc.get_state (the encoded state vector).
func StateVector(d *Doc) ([]byte, error) { return yjsgo.EncodeStateVector(d) }

// StateAsUpdate ports Doc.get_update with an optional remote state vector
// (nil: the full document state).
func StateAsUpdate(d *Doc, sv []byte) ([]byte, error) { return yjsgo.EncodeStateAsUpdate(d, sv) }

// Snapshot is Doc.get_update() — the full state (build_snapshot path).
func Snapshot(d *Doc) ([]byte, error) { return yjsgo.EncodeStateAsUpdate(d, nil) }

// ApplyToFresh ports engine.merge_state / merge_updates: one compacted
// update out of a snapshot and appended batches (deleted content collected).
func ApplyToFresh(snapshot []byte, updates ...[]byte) ([]byte, error) {
	d := NewDoc()
	if err := ApplyUpdate(d, snapshot); err != nil {
		return nil, err
	}
	for _, u := range updates {
		if err := ApplyUpdate(d, u); err != nil {
			return nil, err
		}
	}
	return Snapshot(d)
}

// --------------------------------------------------- document conversion ---

const (
	rootMeta      = "meta"
	rootPageOrder = "pageOrder"
	rootPages     = "pages"
	keyObjects    = "objects"
	keyObjOrder   = "objectOrder"
)

// jsonValue converts engine values (map[string]any / []any / primitives as
// produced by yjs-go) to plain JSON-decodable data.
func jsonValue(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, string, float64, int, int64, uint64:
		return v, nil
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			jv, err := jsonValue(val)
			if err != nil {
				return nil, err
			}
			out[k] = jv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			jv, err := jsonValue(val)
			if err != nil {
				return nil, err
			}
			out = append(out, jv)
		}
		return out, nil
	default:
		// Numbers arrive from yjs-go as the decoded `any` tags; normalize
		// via JSON so json.Unmarshal semantics apply.
		b, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("crdt: unsupported engine value %T", v)
		}
		var out any
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
}

// ToDocument ports the read side of schema.read_document (spike level:
// no anomaly repair - the defensive reader lands with the schema.py port):
// meta props, then pages in pageOrder with their objects in objectOrder.
func ToDocument(d *Doc) (map[string]any, error) {
	meta := d.GetMap(rootMeta)
	order := d.GetArray(rootPageOrder)
	pages := d.GetMap(rootPages)

	doc := map[string]any{}
	for _, k := range meta.Keys() {
		v, _ := meta.Get(k)
		jv, err := jsonValue(v)
		if err != nil {
			return nil, err
		}
		doc[k] = jv
	}
	var pageList []any
	for _, pid := range order.ToSlice() {
		pidStr, _ := pid.(string)
		pv, ok := pages.Get(pidStr)
		if !ok {
			continue
		}
		pageMap, ok := pv.(*yjsgo.Map)
		if !ok {
			if m, isMap := pv.(map[string]any); isMap {
				_ = m
			}
			continue
		}
		page := map[string]any{}
		for _, k := range pageMap.Keys() {
			if k == keyObjects || k == keyObjOrder {
				continue
			}
			v, _ := pageMap.Get(k)
			jv, err := jsonValue(v)
			if err != nil {
				return nil, err
			}
			page[k] = jv
		}
		page["id"] = pidStr

		var objects []any
		if ov, ok := pageMap.Get(keyObjOrder); ok {
			if objOrder, isArr := ov.(*yjsgo.Array); isArr {
				objectsMap, _ := pageMap.Get(keyObjects)
				objMap, _ := objectsMap.(*yjsgo.Map)
				for _, oidv := range objOrder.ToSlice() {
					oid, _ := oidv.(string)
					if objv, ok := objMap.Get(oid); ok {
						if om, isM := objv.(*yjsgo.Map); isM {
							obj := map[string]any{}
							for _, k := range om.Keys() {
								v, _ := om.Get(k)
								jv, err := jsonValue(v)
								if err != nil {
									return nil, err
								}
								obj[k] = jv
							}
							objects = append(objects, obj)
						}
					}
				}
			}
		}
		if objects == nil {
			objects = []any{}
		}
		page[keyObjects] = objects
		pageList = append(pageList, page)
	}
	if pageList == nil {
		pageList = []any{}
	}
	doc["pages"] = pageList
	return doc, nil
}
