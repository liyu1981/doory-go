// Template field filling (apps/designs/fill.py): values -> update_object /
// replace_image operations on every object tagged with the key, applied as
// one all-or-nothing edit. With as_copy (the default) the template is left
// alone and a new design is created.
//
// Ports backend/apps/designs/fill.py (fill_design; bulk_fill lives in
// design_bulk_fill.go)
package doory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fillTextMax       = 20_000
	fillLabel         = "Before filling fields"
	fillLockSeconds   = 180
	fillColorPattern  = `^#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`
	fillDefaultName   = "{title} - {n}"
	fillPlaceholderRe = `\{([a-z][a-z0-9_]{0,39})\}`
)

var fillColorRe = regexp.MustCompile(fillColorPattern)
var fillPlaceholderPattern = regexp.MustCompile(fillPlaceholderRe)

type fillProblem struct {
	Fields map[string]string
}

func (p *fillProblem) Error() string {
	keys := make([]string, 0, len(p.Fields))
	for key := range p.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+": "+p.Fields[key])
	}
	return strings.Join(parts, "; ")
}

type fillImage struct {
	Src    string
	Width  float64
	Height float64
	Asset  string
	Name   string
	Focal  [2]float64
}

type fillOp struct {
	Key      string
	ObjectID string
	Kind     string // text, color, image
	Text     string
	Color    string
	Image    *fillImage
}

type fillResult struct {
	Design   *designRow
	Filled   []string
	Skipped  []string
	Overflow []string
}

func (r *fillResult) summary() map[string]any {
	return map[string]any{
		"design_id": dashedUUID(r.Design.ID),
		"title":     r.Design.Title,
		"filled":    r.Filled,
		"skipped":   r.Skipped,
		"overflow":  r.Overflow,
	}
}

// fillValueLock is the process-local equivalent of one_fill_at_a_time.
var (
	fillLockMu sync.Mutex
	fillLocks  = map[string]time.Time{}
)

func acquireFillLock(userID string) bool {
	fillLockMu.Lock()
	defer fillLockMu.Unlock()
	now := time.Now()
	if until, ok := fillLocks[userID]; ok && now.Before(until) {
		return false
	}
	fillLocks[userID] = now.Add(fillLockSeconds * time.Second)
	return true
}

func releaseFillLock(userID string) {
	fillLockMu.Lock()
	delete(fillLocks, userID)
	fillLockMu.Unlock()
}

func fillValidation(w http.ResponseWriter, problems map[string]string) {
	fields := make([]FieldError, 0, len(problems))
	keys := make([]string, 0, len(problems))
	for key := range problems {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fields = append(fields, SimpleField(key, problems[key], "invalid"))
	}
	writeDRFValidation(w, fields)
}

func (a *App) designFill(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	if d.TrashedAt.Valid {
		writeDRF(w, http.StatusConflict, "design_trashed",
			"This design is in the trash. Restore it before changing it.")
		return
	}
	var body struct {
		Values map[string]any `json:"values"`
		AsCopy *bool          `json:"as_copy"`
		Name   *string        `json:"name"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	if body.Values == nil || len(body.Values) == 0 {
		writeDRFValidation(w, []FieldError{SimpleField("values", "This field is required.", "required")})
		return
	}
	asCopy := true
	if body.AsCopy != nil {
		asCopy = *body.AsCopy
	}
	name := ""
	if body.Name != nil {
		cleaned := cleanText(*body.Name)
		if strings.TrimSpace(*body.Name) == "" {
			writeDRFValidation(w, []FieldError{SimpleField("name", "This field may not be blank.", "blank")})
			return
		}
		if cleaned == "" {
			writeDRFValidation(w, []FieldError{SimpleField("name", "The title cannot be empty.", "invalid")})
			return
		}
		if len([]rune(cleaned)) > 200 {
			writeDRFValidation(w, []FieldError{SimpleField("name",
				"Ensure this field has no more than 200 characters.", "max_length")})
			return
		}
		name = cleaned
	}

	if !acquireFillLock(u.ID) {
		writeDRF(w, http.StatusTooManyRequests, "throttled",
			"A fill is already running for your account. Try again when it has finished.")
		return
	}
	defer releaseFillLock(u.ID)

	result, fillErr, docErr := a.fillDesign(d, u, body.Values, asCopy, name, nil)
	if fillErr != nil {
		fillValidation(w, fillErr.Fields)
		return
	}
	if docErr != nil {
		writeDRF(w, docErr.Status, docErr.Code, docErr.Message)
		return
	}
	status := http.StatusCreated
	if !asCopy {
		status = http.StatusOK
	}
	writeJSON(w, status, result.summary())
}

func (a *App) fillDesign(template *designRow, owner *UserRow, values map[string]any,
	asCopy bool, name string, folder *string) (*fillResult, *fillProblem, *DocumentError) {
	document, err := parseJSONValue([]byte(template.Document))
	if err != nil {
		return nil, nil, &DocumentError{Code: "invalid_document", Status: http.StatusBadRequest,
			Message: "The design has no pages."}
	}
	doc, ok := document.(map[string]any)
	if !ok {
		return nil, nil, &DocumentError{Code: "invalid_document", Status: http.StatusBadRequest,
			Message: "The design has no pages."}
	}
	raw, problem, docErr := a.rawFillOperations(doc, owner, values)
	if problem != nil || docErr != nil {
		return nil, problem, docErr
	}
	next := cloneJSONValue(doc).(map[string]any)
	if err := applyFillOperations(next, raw.ops); err != nil {
		editErr := err.(*fillApplyError)
		key := "values"
		if editErr.Index >= 0 && editErr.Index < len(raw.ops) {
			key = raw.ops[editErr.Index].Key
		}
		return nil, &fillProblem{Fields: map[string]string{key: editErr.Message}}, nil
	}
	if verr := validateDocument(next, maxDesignDocumentBytes); verr != nil {
		return nil, nil, verr
	}
	overflow := fillOverflow(next, raw.filled)
	if asCopy {
		title := name
		if title == "" {
			title = "Copy of " + template.Title
		}
		title = truncateRunes(title, 200)
		folderArg := folder
		if folderArg == nil && template.FolderID.Valid && template.FolderID.String != "" {
			id := dashedUUID(template.FolderID.String)
			folderArg = &id
		}
		rawDoc := pythonStoredJSON(encodeCompactJSON(next))
		parsed, _ := parseJSONValue(rawDoc)
		copy, err := a.createDesign(owner, title, parsed, rawDoc, folderArg)
		if err != nil {
			if lr, ok := err.(*LimitReachedError); ok {
				return nil, nil, &DocumentError{Code: "limit_reached", Status: http.StatusConflict,
					Message: lr.Error()}
			}
			return nil, nil, &DocumentError{Code: "server_error", Status: http.StatusInternalServerError,
				Message: "Something went wrong on our side."}
		}
		return &fillResult{Design: copy, Filled: raw.filled, Skipped: raw.skipped, Overflow: overflow}, nil, nil
	}

	updated, docErr := a.saveFilledDesign(template, owner, next, name)
	if docErr != nil {
		return nil, nil, docErr
	}
	return &fillResult{Design: updated, Filled: raw.filled, Skipped: raw.skipped, Overflow: overflow}, nil, nil
}

type fillRaw struct {
	ops     []fillOp
	filled  []string
	skipped []string
}

func (a *App) rawFillOperations(document map[string]any, owner *UserRow, values map[string]any) (*fillRaw, *fillProblem, *DocumentError) {
	entries := listFields(document)
	byKey := map[string]fieldEntry{}
	for _, entry := range entries {
		if entry.Key != nil {
			byKey[*entry.Key] = entry
		}
	}
	problems := map[string]string{}
	raw := &fillRaw{filled: []string{}, skipped: []string{}}
	// Field order keeps filled/skipped stable even though a Go JSON object
	// loses the request's key order.
	for _, entry := range entries {
		if entry.Key == nil {
			continue
		}
		key := *entry.Key
		value, present := values[key]
		if !present || fillBlank(value) {
			raw.skipped = append(raw.skipped, key)
			continue
		}
		ops, err := a.fieldFillOperations(entry, value, owner)
		if err != nil {
			problems[key] = err.Error()
			continue
		}
		raw.ops = append(raw.ops, ops...)
		raw.filled = append(raw.filled, key)
	}
	for key := range values {
		if _, known := byKey[key]; !known {
			problems[key] = "This design has no field with this key (see list_fields)."
		}
	}
	if len(problems) > 0 {
		return nil, &fillProblem{Fields: problems}, nil
	}
	return raw, nil, nil
}

func (a *App) fieldFillOperations(entry fieldEntry, value any, owner *UserRow) ([]fillOp, error) {
	ids := make([]string, 0, len(entry.ObjectIDs))
	for _, raw := range entry.ObjectIDs {
		if id, ok := raw.(string); ok {
			ids = append(ids, id)
		}
	}
	var base fillOp
	switch entry.Type {
	case "text":
		text, err := fillTextValue(value)
		if err != nil {
			return nil, err
		}
		if len([]rune(text)) > fillTextMax {
			return nil, fmt.Errorf("String should have at most %d characters", fillTextMax)
		}
		base = fillOp{Kind: "text", Text: text}
	case "color":
		color, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("A color value is a #hex string.")
		}
		color = strings.TrimSpace(color)
		if !fillColorRe.MatchString(color) {
			return nil, fmt.Errorf("A color value is a #hex string, e.g. #ff6600.")
		}
		base = fillOp{Kind: "color", Color: color}
	default:
		image, err := a.resolveFillImage(owner, value)
		if err != nil {
			return nil, err
		}
		base = fillOp{Kind: "image", Image: &image}
	}
	out := make([]fillOp, 0, len(ids))
	for _, id := range ids {
		op := base
		op.Key = *entry.Key
		op.ObjectID = id
		out = append(out, op)
	}
	return out, nil
}

func fillBlank(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) == ""
	}
	return false
}

func fillTextValue(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case float64:
		return pyNum(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	default:
		return "", fmt.Errorf("A text value is a string.")
	}
}

var fillImageKeys = map[string]bool{"asset_id": true, "element_id": true, "url": true, "focal": true}

func (a *App) resolveFillImage(owner *UserRow, value any) (fillImage, error) {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		lower := strings.ToLower(text)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			asset, err := a.importRemoteImage(owner, text, "")
			if err != nil {
				return fillImage{}, err
			}
			return fillImageFromAsset(asset), nil
		}
		if asset, err := a.fillAssetByIDOrName(owner, text); err == nil && asset != nil {
			return fillImageFromAsset(asset), nil
		}
		return fillImage{}, fmt.Errorf("There is no picture named %q in your uploads.", text)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return fillImage{}, fmt.Errorf("An image value is a link, an upload's name or {asset_id | element_id | url}.")
	}
	for key := range obj {
		if !fillImageKeys[key] {
			keys := []string{key}
			return fillImage{}, fmt.Errorf("Unknown image setting %q.", keys[0])
		}
	}
	present := 0
	for _, key := range []string{"asset_id", "element_id", "url"} {
		if value, ok := obj[key]; ok && value != nil {
			present++
		}
	}
	if present != 1 {
		return fillImage{}, fmt.Errorf("Send exactly one of asset_id, element_id and url.")
	}
	source := fillImage{Focal: [2]float64{0.5, 0.5}}
	if focal, ok := obj["focal"]; ok {
		pair, ok := focal.([]any)
		if !ok || len(pair) != 2 {
			return fillImage{}, fmt.Errorf("focal must be [x, y].")
		}
		for i, raw := range pair {
			n, ok := asNumber(raw)
			if !ok || n < 0 || n > 1 {
				return fillImage{}, fmt.Errorf("focal must be [x, y] between 0 and 1.")
			}
			source.Focal[i] = n
		}
	}
	if id, ok := obj["asset_id"].(string); ok {
		asset, err := a.fillAsset(owner, id)
		if err != nil || asset == nil {
			return fillImage{}, fmt.Errorf("There is no picture %q in your uploads (see list_assets).", id)
		}
		out := fillImageFromAsset(asset)
		out.Focal = source.Focal
		return out, nil
	}
	if id, ok := obj["element_id"].(string); ok {
		element, err := a.fillElement(id)
		if err != nil || element == nil {
			return fillImage{}, fmt.Errorf("There is no element %q (see search_elements).", id)
		}
		out := *element
		out.Focal = source.Focal
		return out, nil
	}
	if rawURL, ok := obj["url"].(string); ok {
		asset, err := a.importRemoteImage(owner, rawURL, "")
		if err != nil {
			return fillImage{}, err
		}
		out := fillImageFromAsset(asset)
		out.Focal = source.Focal
		return out, nil
	}
	return fillImage{}, fmt.Errorf("Send exactly one of asset_id, element_id and url.")
}

func fillImageFromAsset(asset *assetRow) fillImage {
	return fillImage{Src: mediaURL + asset.File, Width: float64(asset.Width), Height: float64(asset.Height),
		Asset: dashedUUID(asset.ID), Name: asset.Name}
}

func (a *App) fillAssetByIDOrName(owner *UserRow, text string) (*assetRow, error) {
	if shape, ok := uuidShapeString(text); ok {
		return a.fillAsset(owner, shape)
	}
	row := a.DB.QueryRow(`SELECT `+assetCols+` FROM assets_asset
		WHERE owner_id = ? AND LOWER(name) = LOWER(?) AND kind IN ('image','svg')
		ORDER BY created_at DESC LIMIT 1`, owner.ID, text)
	asset, err := scanAsset(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &asset, nil
}

func (a *App) fillAsset(owner *UserRow, rawID string) (*assetRow, error) {
	id := hexUUID(strings.TrimSpace(rawID))
	if id == "" {
		return nil, nil
	}
	asset, err := a.getAssetByID(id)
	if err != nil || asset == nil || asset.OwnerID != owner.ID || !pictureKinds[asset.Kind] {
		return nil, nil
	}
	return asset, nil
}

func (a *App) fillElement(rawID string) (*fillImage, error) {
	id := hexUUID(strings.TrimSpace(rawID))
	if id == "" {
		return nil, nil
	}
	var name, svg string
	var width, height int64
	err := a.DB.QueryRow(`SELECT e.name, e.svg, e.width, e.height FROM elements_element e
		JOIN elements_elementpack p ON p.id = e.pack_id
		WHERE e.id = ? AND e.is_published = TRUE AND p.is_published = TRUE`, id).
		Scan(&name, &svg, &width, &height)
	if err != nil {
		return nil, err
	}
	return &fillImage{Src: mediaURL + svg, Width: float64(width), Height: float64(height), Name: name}, nil
}

func uuidShapeString(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !uuidShape.MatchString(strings.ToLower(raw)) {
		return "", false
	}
	return raw, true
}

// ---- document operations (the fill subset of doc/ops.py) -----------------

type fillApplyError struct {
	Index   int
	Message string
}

func (e *fillApplyError) Error() string { return e.Message }

func applyFillOperations(document map[string]any, ops []fillOp) error {
	for index, op := range ops {
		obj, err := fillFindObject(document, op.ObjectID)
		if err != nil {
			return &fillApplyError{Index: index, Message: err.Error()}
		}
		kind := objectKind(obj)
		switch op.Kind {
		case "text":
			if kind != "text" {
				return &fillApplyError{Index: index, Message: "Only text objects have text."}
			}
			obj["text"] = op.Text
			delete(obj, "styles")
			refitFillTextHeight(obj)
		case "color":
			if kind == "line" {
				obj["stroke"] = op.Color
			} else {
				obj["fill"] = op.Color
			}
		case "image":
			if kind != "image" && kind != "placeholder" {
				return &fillApplyError{Index: index,
					Message: "Only pictures and photo placeholders can be replaced with a picture."}
			}
			replaceFillImage(obj, *op.Image)
		}
	}
	return nil
}

func fillFindObject(document map[string]any, id string) (map[string]any, error) {
	pages, _ := document["pages"].([]any)
	for _, rawPage := range pages {
		page, ok := rawPage.(map[string]any)
		if !ok {
			continue
		}
		objects, _ := page["objects"].([]any)
		for _, raw := range objects {
			obj, ok := raw.(map[string]any)
			if ok && pyStr(obj["id"]) == id {
				return obj, nil
			}
		}
	}
	if group := fillGroupOf(pages, id); group != "" {
		return nil, fmt.Errorf("Object %q is inside the group %q: ungroup it first (`ungroup`), "+
			"or change the whole group.", id, group)
	}
	return nil, fmt.Errorf("There is no object with id %q.", id)
}

func fillGroupOf(pages []any, id string) string {
	var holds func([]any) bool
	holds = func(objects []any) bool {
		for _, raw := range objects {
			obj, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if pyStr(obj["id"]) == id {
				return true
			}
			if children, ok := obj["objects"].([]any); ok && holds(children) {
				return true
			}
		}
		return false
	}
	for _, rawPage := range pages {
		page, ok := rawPage.(map[string]any)
		if !ok {
			continue
		}
		objects, _ := page["objects"].([]any)
		for _, raw := range objects {
			obj, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if children, ok := obj["objects"].([]any); ok && holds(children) {
				return pyStr(obj["id"])
			}
		}
	}
	return ""
}

func cloneJSONValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneJSONValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneJSONValue(item)
		}
		return out
	default:
		return value
	}
}

// ---- text / image builders (doc/builders.py subset) ----------------------

func refitFillTextHeight(obj map[string]any) {
	if fit := readFit(obj["fit"]); fit != nil {
		applyFillFit(obj, fit)
		return
	}
	size := fillFloat(obj["fontSize"], 40)
	lines := estimateTextLines(pyStr(obj["text"]), fillFloat(obj["width"], 200), size,
		int(fillFloat(obj["fontWeight"], 400)))
	before := fillBox(obj)
	obj["height"] = round3(textHeight(lines, size, fillFloat(obj["lineHeight"], 1.16)))
	after := fillBox(obj)
	fillPlace(obj, before.X, before.Y+(before.H-after.H)/2)
}

func applyFillFit(obj map[string]any, fit map[string]any) {
	if fit == nil {
		delete(obj, "fit")
		delete(obj, "fitFontSize")
		return
	}
	size, ok := validFontSize(obj["fontSize"])
	if !ok {
		delete(obj, "fitFontSize")
		return
	}
	obj["fit"] = fit
	width := posFloat(obj["width"], 200)
	leading := posFloat(obj["lineHeight"], 1.16)
	height := posFloat(obj["height"], textHeight(1, size, leading))
	weight := int(posFloat(obj["fontWeight"], 400))
	var minSize *float64
	if v, ok := asNumber(fit["minFontSize"]); ok {
		minSize = &v
	}
	obj["fitFontSize"] = fitFontSize(pyStr(obj["text"]), width, height, size, weight, leading, minSize)
}

type fillBoxRect struct{ X, Y, W, H float64 }

func fillFloat(value any, def float64) float64 {
	if n, ok := asNumber(value); ok {
		return n
	}
	return def
}

func fillOrigin(value any) float64 {
	switch pyStr(value) {
	case "center":
		return 0.5
	case "right", "bottom":
		return 1
	default:
		return 0
	}
}

func fillBox(obj map[string]any) fillBoxRect {
	if slot, ok := obj["slot"].(map[string]any); ok && fillFloat(slot["width"], 0) > 0 && fillFloat(slot["height"], 0) > 0 {
		w, h := fillFloat(slot["width"], 0), fillFloat(slot["height"], 0)
		return fillBoxRect{X: fillFloat(slot["cx"], 0) - w/2, Y: fillFloat(slot["cy"], 0) - h/2, W: w, H: h}
	}
	w := fillFloat(obj["width"], 0) * math.Abs(fillFloat(obj["scaleX"], 1))
	h := fillFloat(obj["height"], 0) * math.Abs(fillFloat(obj["scaleY"], 1))
	return fillBoxRect{
		X: fillFloat(obj["left"], 0) - w*fillOrigin(obj["originX"]),
		Y: fillFloat(obj["top"], 0) - h*fillOrigin(obj["originY"]), W: w, H: h,
	}
}

func fillPlace(obj map[string]any, x, y float64) {
	current := fillBox(obj)
	dx, dy := x-current.X, y-current.Y
	obj["left"] = round3(fillFloat(obj["left"], 0) + dx)
	obj["top"] = round3(fillFloat(obj["top"], 0) + dy)
	if slot, ok := obj["slot"].(map[string]any); ok {
		slot["cx"] = builderNum(fillFloat(slot["cx"], 0) + dx)
		slot["cy"] = builderNum(fillFloat(slot["cy"], 0) + dy)
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func replaceFillImage(obj map[string]any, source fillImage) {
	wasImage := strings.ToLower(pyStr(obj["type"])) == "image"
	_, hasSlot := obj["slot"].(map[string]any)
	isSlot := truthy(obj["isPlaceholder"]) || hasSlot
	if isSlot {
		slot := fillSlotOf(obj)
		obj["type"] = "image"
		obj["strokeUniform"] = true
		obj["slot"] = slot
		for key, value := range fillCoverSlot(slot, source.Width, source.Height, source.Focal) {
			obj[key] = value
		}
		for _, key := range []string{"isPlaceholder", "clipPath", "maskShape", "maskRadius", "rx", "ry",
			"points", "path", "fillRule", "shapeKind", "shapeId", "sides", "starPoints", "innerRatio"} {
			delete(obj, key)
		}
		if !wasImage {
			delete(obj, "fill")
		}
	} else {
		scaleX := math.Abs(fillFloat(obj["scaleX"], 1))
		scaleY := math.Abs(fillFloat(obj["scaleY"], 1))
		frameW := math.Max(fillFloat(obj["width"], 1)*scaleX, 1)
		frameH := math.Max(fillFloat(obj["height"], 1)*scaleY, 1)
		ox, oy := fillOrigin(obj["originX"]), fillOrigin(obj["originY"])
		cx := fillFloat(obj["left"], 0) + (0.5-ox)*frameW
		cy := fillFloat(obj["top"], 0) + (0.5-oy)*frameH
		obj["originX"], obj["originY"] = "center", "center"
		obj["left"], obj["top"] = builderNum(cx), builderNum(cy)
		for key, value := range fillCoverFit(frameW, frameH, source.Width, source.Height, source.Focal) {
			obj[key] = value
		}
		delete(obj, "clipPath")
	}
	obj["src"] = source.Src
	for _, key := range []string{"docSrc", "elementId", "colorMap", "isPlaceholder"} {
		delete(obj, key)
	}
	if source.Asset != "" {
		obj["assetId"] = source.Asset
	} else {
		delete(obj, "assetId")
	}
	if source.Name != "" {
		if name, _ := obj["name"].(string); name == "" || name == "Image placeholder" {
			obj["name"] = source.Name
		}
	}
}

func fillSlotOf(obj map[string]any) map[string]any {
	if slot, ok := obj["slot"].(map[string]any); ok &&
		fillFloat(slot["width"], 0) > 0 && fillFloat(slot["height"], 0) > 0 {
		out := map[string]any{}
		for key, value := range slot {
			out[key] = value
		}
		return out
	}
	scaleX, scaleY := math.Abs(fillFloat(obj["scaleX"], 1)), math.Abs(fillFloat(obj["scaleY"], 1))
	w := math.Max(fillFloat(obj["width"], 1)*scaleX, 1)
	h := math.Max(fillFloat(obj["height"], 1)*scaleY, 1)
	ox, oy := fillOrigin(obj["originX"]), fillOrigin(obj["originY"])
	cx := fillFloat(obj["left"], 0) + (0.5-ox)*w
	cy := fillFloat(obj["top"], 0) + (0.5-oy)*h
	slot := map[string]any{
		"cx": builderNum(cx), "cy": builderNum(cy),
		"width": builderNum(w), "height": builderNum(h), "angle": builderNum(fillFloat(obj["angle"], 0)),
	}
	if src, ok := obj["src"].(string); ok && src != "" && strings.ToLower(pyStr(obj["type"])) == "image" {
		slot["src"] = src
	}
	return slot
}

func fillCoverFit(frameW, frameH, imgW, imgH float64, focal [2]float64) map[string]any {
	natW, natH := math.Max(imgW, 1), math.Max(imgH, 1)
	fx, fy := math.Max(0, math.Min(1, focal[0])), math.Max(0, math.Min(1, focal[1]))
	scale := math.Max(frameW/natW, frameH/natH)
	visW := math.Min(natW, frameW/scale)
	visH := math.Min(natH, frameH/scale)
	return map[string]any{
		"cropX": builderNum(math.Max(0, (natW-visW)*fx)),
		"cropY": builderNum(math.Max(0, (natH-visH)*fy)),
		"width": builderNum(visW), "height": builderNum(visH),
		"scaleX": builderNum(scale), "scaleY": builderNum(scale),
	}
}

func fillCoverSlot(slot map[string]any, imgW, imgH float64, focal [2]float64) map[string]any {
	natW, natH := math.Max(imgW, 1), math.Max(imgH, 1)
	slotW, slotH := math.Max(fillFloat(slot["width"], 1), 1), math.Max(fillFloat(slot["height"], 1), 1)
	angle := fillFloat(slot["angle"], 0)
	fx, fy := math.Max(0, math.Min(1, focal[0])), math.Max(0, math.Min(1, focal[1]))
	scale := math.Max(slotW/natW, slotH/natH)
	dx := (0.5 - fx) * (natW*scale - slotW)
	dy := (0.5 - fy) * (natH*scale - slotH)
	rad := angle * math.Pi / 180
	cx := fillFloat(slot["cx"], 0) + dx*math.Cos(rad) - dy*math.Sin(rad)
	cy := fillFloat(slot["cy"], 0) + dx*math.Sin(rad) + dy*math.Cos(rad)
	return map[string]any{
		"originX": "center", "originY": "center", "left": builderNum(cx), "top": builderNum(cy),
		"width": builderNum(natW), "height": builderNum(natH),
		"scaleX": builderNum(scale), "scaleY": builderNum(scale), "angle": builderNum(angle),
		"cropX": 0, "cropY": 0,
	}
}

// ---- saving / versions ----------------------------------------------------

func (a *App) saveFilledDesign(template *designRow, owner *UserRow, document map[string]any, name string) (*designRow, *DocumentError) {
	info, err := describeDocument(document)
	if err != nil {
		return nil, &DocumentError{Code: "invalid_document", Status: http.StatusBadRequest, Message: err.Error()}
	}
	a.recordVersionBeforeFill(template, owner)
	now := formatDjangoDateTime(time.Now())
	docText := string(pythonStoredJSON(encodeCompactJSON(document)))
	updates := []string{"document = ?", "width = ?", "height = ?", "page_count = ?", "preset = ?",
		"revision = revision + 1", "updated_at = ?"}
	args := []any{docText, info.Width, info.Height, info.PageCount, info.Preset, now}
	if name != "" {
		updates = append(updates, "title = ?")
		args = append(args, name)
	}
	args = append(args, template.ID, owner.ID)
	if _, err := a.DB.Exec("UPDATE designs_design SET "+strings.Join(updates, ", ")+
		" WHERE id = ? AND owner_id = ?", args...); err != nil {
		return nil, &DocumentError{Code: "server_error", Status: http.StatusInternalServerError,
			Message: "Something went wrong on our side."}
	}
	updated, err := a.getDesignByID(template.ID)
	if err != nil || updated == nil {
		return nil, &DocumentError{Code: "server_error", Status: http.StatusInternalServerError,
			Message: "Something went wrong on our side."}
	}
	a.notifyDesignRecheck(updated.ID)
	return updated, nil
}

func (a *App) recordVersionBeforeFill(d *designRow, owner *UserRow) {
	var latest sqlNullString
	_ = a.DB.QueryRow(`SELECT created_at FROM designs_designversion WHERE design_id = ?
		ORDER BY created_at DESC LIMIT 1`, d.ID).Scan(&latest)
	if latest.Valid {
		if t, err := parseDjangoDateTime(latest.String); err == nil && time.Since(t) < editVersionGap {
			return
		}
	}
	pageCount := int64(0)
	if doc, err := parseJSONValue([]byte(d.Document)); err == nil {
		if m, ok := doc.(map[string]any); ok {
			if pages, ok := m["pages"].([]any); ok {
				pageCount = int64(len(pages))
			}
		}
	}
	_ = a.recordVersion(d, d.Document, pageCount, owner, true, fillLabel)
	a.pruneAutoVersions(d.ID)
}

func fillOverflow(document map[string]any, filled []string) []string {
	out := []string{}
	for _, entry := range listFields(document) {
		if entry.Key != nil && entry.Overflow && containsText(filled, *entry.Key) {
			out = append(out, *entry.Key)
		}
	}
	return out
}

func containsText(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
