// Package yschema ports apps/collab/schema.py: the mapping between a
// DoryDocument (plain JSON) and its Y document — the three root types
// `meta` (Map), `pageOrder` (Array) and `pages` (Map of page-id -> Map) —
// plus the defensive reader (anomalies), the repair pass and has_no_pages.
//
// write_changes/sync_order (the REST-save diff engine, apps/designs/edits.py)
// are ported with the designs slice; this file covers what the room needs to
// open, snapshot, save and repair a design.
//
// Builder discipline (third_party/yjs-go/PATCHES.md): a nested node is
// inserted into its parent BEFORE it is populated, so children follow the
// type item in clock order.
package yschema

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"

	yjsgo "github.com/CivNode/yjs-go"

	"doory-go/internal/crdt"
)

// Root types and keys (schema.ROOTS etc.).
const (
	RootMeta      = "meta"
	RootPageOrder = "pageOrder"
	RootPages     = "pages"
	KeyObjects    = "objects"
	KeyObjOrder   = "objectOrder"

	// RestoredPageID is schema.RESTORED_PAGE_ID: the editor puts the same
	// page back when a design ends up without one.
	RestoredPageID = "restored-page"

	MaxIDLength    = 64
	MaxValueDepth  = 64
	MaxSafeInteger = 1 << 53
)

var (
	reservedPageKeys = map[string]bool{KeyObjects: true, KeyObjOrder: true}
	skipID           = map[string]bool{"id": true}
	skipDoc          = map[string]bool{"pages": true}
)

// Unrepresentable is schema.UnrepresentableState: the Y state cannot be
// turned into a document (for example a property nested too deeply).
type Unrepresentable struct{ Msg string }

func (e *Unrepresentable) Error() string { return e.Msg }

func unrepresentable(format string, args ...any) error {
	return &Unrepresentable{Msg: fmt.Sprintf(format, args...)}
}

// NewDoc ports schema.new_doc: an empty document with the three roots defined,
// so a client's mismatching root is ignored.
func NewDoc() *crdt.Doc { return crdt.NewDoc() }

// ------------------------------------------------------------------ clean ---

// clean ports _clean: a JSON value in the form the Y encoder accepts
// (finite numbers, safe integers, string keys).
func clean(value any, depth int) any {
	if depth > MaxValueDepth {
		panic(unrepresentable("The document is nested too deeply."))
	}
	switch v := value.(type) {
	case nil, bool, string:
		return v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		return v
	case int:
		return v
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, clean(item, depth+1))
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			out[key] = clean(item, depth+1)
		}
		return out
	default:
		// json.Number and friends: accept via a JSON round trip.
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		var out any
		if json.Unmarshal(b, &out) != nil {
			return nil
		}
		return out
	}
}

func validID(candidate any) bool {
	s, ok := candidate.(string)
	return ok && len(s) > 0 && len(s) <= MaxIDLength
}

// uniqueIDs ports _unique_ids: a missing, invalid or repeated id is replaced
// by a fresh UUID.
func uniqueIDs(items []any) []string {
	seen := map[string]bool{}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		obj, _ := item.(map[string]any)
		candidate, _ := obj["id"].(string)
		if !validID(candidate) || seen[candidate] {
			candidate = freshUUID()
		}
		seen[candidate] = true
		ids = append(ids, candidate)
	}
	return ids
}

// freshUUID is str(uuid.uuid4()) for id replacement (crypto/rand v4).
func freshUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ------------------------------------------------------------ write path ---

// WriteDocument ports write_document: write `document` into the roots of the
// doc, replacing whatever they hold (the caller opens the transaction).
func WriteDocument(d *crdt.Doc, document map[string]any) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if u, ok := p.(*Unrepresentable); ok {
				err = u
				return
			}
			panic(p)
		}
	}()
	meta := d.GetMap(RootMeta)
	order := d.GetArray(RootPageOrder)
	pages := d.GetMap(RootPages)
	for _, key := range meta.Keys() {
		meta.Delete(key)
	}
	for _, key := range pages.Keys() {
		pages.Delete(key)
	}
	if order.Len() > 0 {
		order.Delete(0, order.Len())
	}
	for key, value := range document {
		if key == "pages" {
			continue
		}
		meta.Set(key, clean(value, 0))
	}
	var pageItems []any
	if raw, ok := document["pages"].([]any); ok {
		for _, page := range raw {
			if _, isMap := page.(map[string]any); isMap {
				pageItems = append(pageItems, page)
			}
		}
	}
	pageIDs := uniqueIDs(pageItems)
	if len(pageIDs) > 0 {
		order.Push(anyStrings(pageIDs)...)
	}
	for i, page := range pageItems {
		writePage(d, pages, page.(map[string]any), pageIDs[i])
	}
	return nil
}

// writePage builds one page map: insert the node first, then populate it.
func writePage(d *crdt.Doc, pages *yjsgo.Map, page map[string]any, pageID string) {
	node := yjsgo.NewNestedMap(d)
	pages.Set(pageID, node)
	objects := page[KeyObjects]
	objectItems := []any{}
	if raw, ok := objects.([]any); ok {
		for _, item := range raw {
			if _, isMap := item.(map[string]any); isMap {
				objectItems = append(objectItems, item)
			}
		}
	}
	objectIDs := uniqueIDs(objectItems)
	props := map[string]any{}
	for key, value := range page {
		if reservedPageKeys[key] || key == "id" {
			continue
		}
		props[key] = clean(value, 0)
	}
	props["id"] = pageID
	node.Set(KeyObjOrder, yjsgo.NewNestedArray(d))
	node.Set(KeyObjects, yjsgo.NewNestedMap(d))
	for key, value := range props {
		node.Set(key, value)
	}
	ov, _ := node.Get(KeyObjOrder)
	orderNode, _ := ov.(*yjsgo.Array)
	if len(objectIDs) > 0 {
		orderNode.Push(anyStrings(objectIDs)...)
	}
	ov, _ = node.Get(KeyObjects)
	objectsNode, _ := ov.(*yjsgo.Map)
	for i, item := range objectItems {
		writeObject(d, objectsNode, item.(map[string]any), objectIDs[i])
	}
}

func writeObject(d *crdt.Doc, objects *yjsgo.Map, obj map[string]any, objectID string) {
	node := yjsgo.NewNestedMap(d)
	objects.Set(objectID, node)
	props := map[string]any{}
	for key, value := range obj {
		if key == "id" {
			continue
		}
		props[key] = clean(value, 0)
	}
	props["id"] = objectID
	for key, value := range props {
		node.Set(key, value)
	}
}

// DocumentToState ports document_to_ydoc(...).get_update() — the Y state of a
// stored document (build_snapshot).
func DocumentToState(document map[string]any) ([]byte, error) {
	d := NewDoc()
	var werr error
	d.Transact(func() {
		werr = WriteDocument(d, document)
	}, nil)
	if werr != nil {
		return nil, werr
	}
	return crdt.Snapshot(d)
}

func anyStrings(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

// -------------------------------------------------------------- read path ---

// Anomalies is schema.Anomalies: what read_document found wrong; Repair
// removes exactly this.
type Anomalies struct {
	PageOrderRemove    []int
	PagesUnlisted      []string
	ObjectOrderRemove  map[string][]int
	ObjectsUnlisted    map[string][]string
	BadProps           [][]string
	NoPages            bool
}

// Empty reports whether any anomaly was found (Python: __bool__).
func (a *Anomalies) Empty() bool {
	if a == nil {
		return true
	}
	if a.NoPages || len(a.PageOrderRemove) > 0 || len(a.PagesUnlisted) > 0 ||
		len(a.ObjectOrderRemove) > 0 || len(a.ObjectsUnlisted) > 0 || len(a.BadProps) > 0 {
		return false
	}
	return true
}

// number ports _number: integral floats become ints when safe.
func number(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	if v == math.Trunc(v) && math.Abs(v) <= MaxSafeInteger {
		return int64(v)
	}
	return v
}

// arrayValues ports array_values: the entries of a shared array as plain JSON.
func arrayValues(a *yjsgo.Array) []any {
	return a.ToSlice()
}

// toJSON ports to_json: a Y value as plain JSON (shared types flattened,
// integral floats int); nesting bounded.
func toJSON(value any, depth int) (any, error) {
	if depth > MaxValueDepth {
		return nil, unrepresentable("The property is nested too deeply.")
	}
	switch v := value.(type) {
	case nil, bool, string:
		return v, nil
	case float64:
		return number(v), nil
	case float32:
		return number(float64(v)), nil
	case int:
		return v, nil
	case int64:
		return v, nil
	case uint64:
		return v, nil
	case *yjsgo.Text:
		return v.String(), nil
	case *yjsgo.Map:
		out := map[string]any{}
		for _, key := range v.Keys() {
			val, ok := v.Get(key)
			if !ok {
				continue
			}
			jv, err := toJSON(val, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = jv
		}
		return out, nil
	case *yjsgo.Array:
		return plain(arrayValues(v), depth+1)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			jv, err := toJSON(item, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, jv)
		}
		return out, nil
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			jv, err := toJSON(item, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = jv
		}
		return out, nil
	default:
		return nil, nil
	}
}

// plain ports _plain: plain JSON from a shared array (integral floats int,
// nesting bounded).
func plain(values []any, depth int) ([]any, error) {
	if depth > MaxValueDepth {
		return nil, unrepresentable("The property is nested too deeply.")
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		switch v := value.(type) {
		case nil, bool, string:
			out = append(out, v)
		case float64:
			out = append(out, number(v))
		case []any:
			nested, err := plain(v, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, nested)
		case map[string]any:
			jv, err := toJSON(v, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, jv)
		default:
			jv, err := toJSON(v, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, jv)
		}
	}
	return out, nil
}

// ordered ports _ordered: ids in order, first occurrence wins, dangling ids
// skipped. Returns (ids to use, indices to remove, entries not listed).
func ordered(order []any, keys []string) (use []string, remove []int, unlisted []string) {
	keySet := map[string]bool{}
	for _, k := range keys {
		keySet[k] = true
	}
	seen := map[string]bool{}
	listed := map[string]bool{}
	if order != nil {
		for index, raw := range order {
			id, isStr := raw.(string)
			if !isStr || seen[id] || !keySet[id] {
				remove = append(remove, index)
				continue
			}
			seen[id] = true
			use = append(use, id)
			listed[id] = true
		}
	}
	for _, k := range keys {
		if !listed[k] {
			unlisted = append(unlisted, k)
		}
	}
	sortStrings(unlisted)
	return use, remove, unlisted
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// props ports _props: the readable entries of a map as JSON.
func props(source *yjsgo.Map, path []string, skip map[string]bool, anomalies *Anomalies) map[string]any {
	result := map[string]any{}
	if source == nil {
		return result
	}
	for _, key := range source.Keys() {
		if skip[key] {
			continue
		}
		value, ok := source.Get(key)
		if !ok {
			continue
		}
		jv, err := toJSON(value, 0)
		if err != nil {
			anomalies.BadProps = append(anomalies.BadProps, append(append([]string{}, path...), key))
			continue
		}
		result[key] = jv
	}
	return result
}

// ReadDocument ports read_document: the DoryDocument held by the doc and the
// anomalies found on the way.
func ReadDocument(d *crdt.Doc) (map[string]any, *Anomalies) {
	anomalies := &Anomalies{
		ObjectOrderRemove: map[string][]int{},
		ObjectsUnlisted:   map[string][]string{},
	}
	meta := d.GetMap(RootMeta)
	document := props(meta, []string{RootMeta}, skipDoc, anomalies)
	orderNode := d.GetArray(RootPageOrder)
	pagesNode := d.GetMap(RootPages)
	pageKeyList := pagesNode.Keys()
	pageIDs, remove, unlisted := ordered(arrayValues(orderNode), pageKeyList)
	anomalies.PageOrderRemove = remove
	anomalies.PagesUnlisted = unlisted
	var pages []any
	for _, pageID := range pageIDs {
		pv, ok := pagesNode.Get(pageID)
		if !ok {
			anomalies.PagesUnlisted = append(anomalies.PagesUnlisted, pageID)
			continue
		}
		pageMap, isMap := pv.(*yjsgo.Map)
		if !isMap {
			anomalies.PagesUnlisted = append(anomalies.PagesUnlisted, pageID)
			continue
		}
		page := props(pageMap, []string{RootPages, pageID}, reservedPageKeys, anomalies)
		page["id"] = pageID
		var objectsMap *yjsgo.Map
		if ov, ok := pageMap.Get(KeyObjects); ok {
			objectsMap, _ = ov.(*yjsgo.Map)
		}
		var objectKeyList []string
		if objectsMap != nil {
			objectKeyList = objectsMap.Keys()
		}
		var orderValues []any
		if ov, ok := pageMap.Get(KeyObjOrder); ok {
			if arr, isArr := ov.(*yjsgo.Array); isArr {
				orderValues = arrayValues(arr)
			}
		}
		objectIDs, objRemove, objUnlisted := ordered(orderValues, objectKeyList)
		if len(objRemove) > 0 {
			anomalies.ObjectOrderRemove[pageID] = objRemove
		}
		if len(objUnlisted) > 0 {
			anomalies.ObjectsUnlisted[pageID] = objUnlisted
		}
		var objects []any
		for _, objectID := range objectIDs {
			if objectsMap == nil {
				continue
			}
			ov, ok := objectsMap.Get(objectID)
			if !ok {
				continue
			}
			objectMap, isMap := ov.(*yjsgo.Map)
			if !isMap {
				continue
			}
			item := props(objectMap, []string{RootPages, pageID, KeyObjects, objectID}, skipID, anomalies)
			item["id"] = objectID
			objects = append(objects, item)
		}
		if objects == nil {
			objects = []any{}
		}
		page["objects"] = objects
		pages = append(pages, page)
	}
	if pages == nil {
		pages = []any{}
	}
	document["pages"] = pages
	anomalies.NoPages = len(pages) == 0
	return document, anomalies
}

// HasNoPages ports has_no_pages: true when no page is left.
func HasNoPages(d *crdt.Doc) bool {
	pagesNode := d.GetMap(RootPages)
	orderNode := d.GetArray(RootPageOrder)
	use, _, _ := ordered(arrayValues(orderNode), pagesNode.Keys())
	for _, id := range use {
		if pv, ok := pagesNode.Get(id); ok {
			if _, isMap := pv.(*yjsgo.Map); isMap {
				return false
			}
		}
	}
	return true
}

// --------------------------------------------------------------- repair ----

// removeEntries ports _remove_entries: delete the entries at indices as runs
// of neighbours, right to left.
func removeEntries(array *yjsgo.Array, indices []int) {
	if len(indices) == 0 || array == nil {
		return
	}
	sorted := append([]int{}, indices...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	uniq := sorted[:0]
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			uniq = append(uniq, v)
		}
	}
	type run struct{ start, stop int }
	var runs []run
	for _, index := range uniq {
		if n := len(runs); n > 0 && runs[n-1].stop == index {
			runs[n-1].stop = index + 1
			continue
		}
		runs = append(runs, run{index, index + 1})
	}
	for i := len(runs) - 1; i >= 0; i-- {
		array.Delete(uint64(runs[i].start), uint64(runs[i].stop-runs[i].start))
	}
}

// Repair ports schema.repair: delete what anomalies lists (duplicate and
// dangling ids, unlisted entries, unreadable properties). Runs inside the
// caller's transaction.
func Repair(d *crdt.Doc, anomalies *Anomalies) {
	if anomalies == nil || anomalies.Empty() {
		return
	}
	pagesNode := d.GetMap(RootPages)
	orderNode := d.GetArray(RootPageOrder)
	removeEntries(orderNode, anomalies.PageOrderRemove)
	for _, key := range anomalies.PagesUnlisted {
		pagesNode.Delete(key)
	}
	for pageID, indices := range anomalies.ObjectOrderRemove {
		pv, ok := pagesNode.Get(pageID)
		if !ok {
			continue
		}
		pageMap, isMap := pv.(*yjsgo.Map)
		if !isMap {
			continue
		}
		if ov, ok := pageMap.Get(KeyObjOrder); ok {
			if arr, isArr := ov.(*yjsgo.Array); isArr {
				removeEntries(arr, indices)
			}
		}
	}
	for pageID, keys := range anomalies.ObjectsUnlisted {
		pv, ok := pagesNode.Get(pageID)
		if !ok {
			continue
		}
		pageMap, isMap := pv.(*yjsgo.Map)
		if !isMap {
			continue
		}
		ov, ok := pageMap.Get(KeyObjects)
		if !ok {
			continue
		}
		objectsMap, isMap := ov.(*yjsgo.Map)
		if !isMap {
			continue
		}
		for _, key := range keys {
			objectsMap.Delete(key)
		}
	}
	for _, path := range anomalies.BadProps {
		if len(path) == 0 {
			continue
		}
		root := d.GetMap(path[0])
		var target *yjsgo.Map = root
		for _, step := range path[1 : len(path)-1] {
			tv, ok := target.Get(step)
			if !ok {
				target = nil
				break
			}
			next, isMap := tv.(*yjsgo.Map)
			if !isMap {
				target = nil
				break
			}
			target = next
		}
		if target == nil {
			continue
		}
		last := path[len(path)-1]
		if _, ok := target.Get(last); ok {
			target.Delete(last)
		}
	}
	if anomalies.NoPages {
		blank := map[string]any{
			"background": map[string]any{"type": "solid", "color": "#ffffff"},
			"objects":    []any{},
		}
		writePage(d, pagesNode, blank, RestoredPageID)
		orderNode.Push(RestoredPageID)
	}
}

// JSONEqual ports schema.json_equal: equality of JSON values (1 == 1.0, but
// True is not 1).
func JSONEqual(left, right any) bool {
	switch l := left.(type) {
	case bool:
		r, ok := right.(bool)
		return ok && l == r
	case float64:
		switch r := right.(type) {
		case float64:
			return l == r
		case int64:
			return l == float64(r)
		case int:
			return l == float64(r)
		}
		return false
	case int64:
		switch r := right.(type) {
		case float64:
			return float64(l) == r
		case int64:
			return l == r
		case int:
			return l == int64(r)
		}
		return false
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for i := range l {
			if !JSONEqual(l[i], r[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for key, lv := range l {
			rv, ok := r[key]
			if !ok || !JSONEqual(lv, rv) {
				return false
			}
		}
		return true
	case string:
		r, ok := right.(string)
		return ok && l == r
	case nil:
		return right == nil
	default:
		return false
	}
}
