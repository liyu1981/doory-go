// Turning upstream SVG artwork into the form the editor can recolour
// (apps/elements/normalize.py).
//
// The editor recolours by remapping the colours it finds on the leaf shapes
// (Element.colors), so every visible shape must carry explicit fill/stroke
// values (resolved from ancestors and inline styles, written as #rrggbb),
// invisible helpers and metadata are dropped, unreferenced ids and inter-
// element whitespace are removed, and width/height become explicit numbers on
// top of a viewBox (icons: scaled so the longer side equals `size`; `frame`
// adds an invisible rectangle the size of the viewBox).
//
// The output is still treated as hostile: sanitize runs afterwards, on
// everything that gets stored.

package doory

import (
	"encoding/xml"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"doory-go/internal/svg"
)

// normalizeSpec is normalize.NormalizeSpec.
type normalizeSpec struct {
	// Mode is "mono" (one recolourable colour) or "colour" (keep the palette).
	Mode  string
	Color string
	// Size is the longer side of the output width/height in px (nil: keep the
	// artwork's own size).
	Size *float64
	// Frame adds an invisible rectangle the size of the viewBox.
	Frame bool
}

func (s normalizeSpec) color() string {
	if s.Color == "" {
		return elementDefaultColor
	}
	return s.Color
}

var (
	elementShapes     = setOfStrings("path", "rect", "circle", "ellipse", "line", "polyline", "polygon", "text")
	elementContainers = setOfStrings("svg", "g", "a", "switch")
	elementOpaque     = setOfStrings("defs", "clipPath", "mask", "pattern", "marker", "symbol",
		"linearGradient", "radialGradient")
	elementDroppedElements = setOfStrings("title", "desc", "metadata")
)

var (
	elementFillProps   = []string{"fill-rule", "fill-opacity", "clip-rule"}
	elementStrokeProps = []string{"stroke-width", "stroke-linecap", "stroke-linejoin", "stroke-miterlimit",
		"stroke-dasharray", "stroke-dashoffset", "stroke-opacity"}
	elementInherited   = append(append([]string{"fill", "stroke"}, elementFillProps...), elementStrokeProps...)
	elementRootDropped = []string{"class", "style", "version", "x", "y", "role", "focusable", "xml:space",
		"enable-background"}
	elementPaintAttrs = []string{"fill", "stroke", "stop-color", "flood-color", "lighting-color"}
)

var elementAlphaAttr = map[string]string{
	"fill": "fill-opacity", "stroke": "stroke-opacity", "stop-color": "stop-opacity",
}

var (
	urlRefPattern   = regexp.MustCompile(`url\(\s*['"]?#([^'")\s]+)['"]?\s*\)`)
	numberPattern   = regexp.MustCompile(`^\s*([0-9]*\.?[0-9]+)\s*(?:px)?\s*$`)
	hexColorPattern = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	spacePreserving = setOfStrings("text", "tspan", "textPath", "style")
)

func setOfStrings(items ...string) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		out[item] = true
	}
	return out
}

// fmtNumber is normalize.fmt: at most two decimals, no trailing zeros, never -0.
func fmtNumber(value float64) string {
	text := strconv.FormatFloat(value, 'f', 2, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" || text == "-0" {
		return "0"
	}
	return text
}

// ---- colours -----------------------------------------------------------------------------------------------

var rgbFunctionPattern = regexp.MustCompile(
	`(?i)^rgba?\(\s*([\d.]+%?)\s*[,\s]\s*([\d.]+%?)\s*[,\s]\s*([\d.]+%?)\s*(?:[,/]\s*([\d.]+%?)\s*)?\)$`)

// parsedColor is `(#rrggbb, alpha or nil)`.
type parsedColor struct {
	Hex   string
	Alpha *float64
}

// parseCSSColor is normalize.parse_color: a plain CSS colour, or nil.
func parseCSSColor(value string) *parsedColor {
	text := strings.TrimSpace(value)
	if text == "" || strings.HasPrefix(text, "url(") {
		return nil
	}
	lowered := strings.ToLower(text)
	switch lowered {
	case "inherit", "context-fill", "context-stroke":
		return nil
	}
	if strings.HasPrefix(lowered, "rgb") {
		return parseRGBFunction(text)
	}
	if !hexColorPattern.MatchString(text) && !regexp.MustCompile(`^[A-Za-z]+(\(.*\))?$`).MatchString(text) {
		return nil
	}
	return parseNamedColor(text)
}

// parseRGBFunction is normalize._rgb_function.
func parseRGBFunction(text string) *parsedColor {
	match := rgbFunctionPattern.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	channel := func(raw string, scale float64) (float64, bool) {
		if strings.HasSuffix(raw, "%") {
			value, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
			return value / 100 * scale, err == nil
		}
		value, err := strconv.ParseFloat(raw, 64)
		return value, err == nil
	}
	r, ok1 := channel(match[1], 255)
	g, ok2 := channel(match[2], 255)
	b, ok3 := channel(match[3], 255)
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	round := func(value float64) int64 {
		return int64(math.Round(math.Min(255, math.Max(0, value))))
	}
	alpha := 1.0
	if match[4] != "" {
		value, ok := channel(match[4], 1)
		if !ok {
			return nil
		}
		alpha = math.Min(1, math.Max(0, value))
	}
	out := &parsedColor{Hex: fmt.Sprintf("#%02x%02x%02x", round(r), round(g), round(b))}
	if alpha < 1 {
		rounded := math.Round(alpha*1000) / 1000
		out.Alpha = &rounded
	}
	return out
}

// parseNamedColor resolves #hex, named colours and hsl()/hsv() the way
// Pillow's ImageColor does (the oracle uses Pillow here).
func parseNamedColor(text string) *parsedColor {
	lowered := strings.ToLower(strings.TrimSpace(text))
	if hexColorPattern.MatchString(lowered) {
		return colorFromHex(lowered)
	}
	if strings.HasPrefix(lowered, "hsl(") || strings.HasPrefix(lowered, "hsla(") ||
		strings.HasPrefix(lowered, "hsv(") || strings.HasPrefix(lowered, "hsva(") {
		return colorFromHSLOrHSV(lowered)
	}
	if rgb, ok := elementColorNames[lowered]; ok {
		return &parsedColor{Hex: fmt.Sprintf("#%02x%02x%02x", rgb[0], rgb[1], rgb[2])}
	}
	return nil
}

// colorFromHex expands #rgb / #rgba / #rrggbb / #rrggbbaa.
func colorFromHex(text string) *parsedColor {
	body := strings.TrimPrefix(text, "#")
	expand := func(s string) int {
		value, _ := strconv.ParseInt(strings.Repeat(s, 2), 16, 32)
		return int(value)
	}
	var r, g, b int
	alpha := 1.0
	switch len(body) {
	case 3, 4:
		r, g, b = expand(body[0:1]), expand(body[1:2]), expand(body[2:3])
		if len(body) == 4 {
			alpha = float64(expand(body[3:4])) / 255
		}
	case 6, 8:
		value, err := strconv.ParseInt(body, 16, 64)
		if err != nil {
			return nil
		}
		if len(body) == 6 {
			r = int(value >> 16 & 0xFF)
			g = int(value >> 8 & 0xFF)
			b = int(value & 0xFF)
		} else {
			r = int(value >> 24 & 0xFF)
			g = int(value >> 16 & 0xFF)
			b = int(value >> 8 & 0xFF)
			alpha = float64(value&0xFF) / 255
		}
	default:
		return nil
	}
	out := &parsedColor{Hex: fmt.Sprintf("#%02x%02x%02x", r, g, b)}
	if alpha != 1 {
		rounded := math.Round(alpha*1000) / 1000
		out.Alpha = &rounded
	}
	return out
}

// colorFromHSLOrHSV parses hsl()/hsla()/hsv()/hsva() (Pillow's forms).
func colorFromHSLOrHSV(text string) *parsedColor {
	open := strings.IndexByte(text, '(')
	close := strings.LastIndexByte(text, ')')
	if open < 0 || close < open {
		return nil
	}
	kind := text[:open]
	parts := strings.FieldsFunc(text[open+1:close], func(r rune) bool {
		return r == ',' || r == ' '
	})
	if len(parts) < 3 {
		return nil
	}
	parsePct := func(raw string, scale float64) (float64, bool) {
		raw = strings.TrimSpace(raw)
		if strings.HasSuffix(raw, "%") {
			value, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
			return value / 100 * scale, err == nil
		}
		value, err := strconv.ParseFloat(raw, 64)
		return value, err == nil
	}
	hue, ok1 := parsePct(parts[0], 1)
	sat, ok2 := parsePct(parts[1], 1)
	light, ok3 := parsePct(parts[2], 1)
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	alpha := 1.0
	if len(parts) > 3 {
		value, ok := parsePct(parts[3], 1)
		if ok {
			alpha = math.Min(1, math.Max(0, value))
		}
	}
	hue = math.Mod(hue, 360) / 360
	sat = math.Min(1, math.Max(0, sat))
	light = math.Min(1, math.Max(0, light))
	var r, g, b float64
	if strings.HasPrefix(kind, "hsv") {
		r, g, b = hsvToRGB(hue, sat, light)
	} else {
		r, g, b = hslToRGB(hue, sat, light)
	}
	out := &parsedColor{Hex: fmt.Sprintf("#%02x%02x%02x",
		int(math.Round(r*255)), int(math.Round(g*255)), int(math.Round(b*255)))}
	if alpha != 1 {
		rounded := math.Round(alpha*1000) / 1000
		out.Alpha = &rounded
	}
	return out
}

func hueToChannel(p, q, t float64) float64 {
	if t < 0 {
		t += 1
	}
	if t > 1 {
		t -= 1
	}
	switch {
	case t < 1.0/6:
		return p + (q-p)*6*t
	case t < 0.5:
		return q
	case t < 2.0/3:
		return p + (q-p)*(2.0/3-t)*6
	}
	return p
}

func hslToRGB(h, s, l float64) (float64, float64, float64) {
	if s == 0 {
		return l, l, l
	}
	var q float64
	if l < 0.5 {
		q = l * (1 + s)
	} else {
		q = l + s - l*s
	}
	p := 2*l - q
	return hueToChannel(p, q, h+1.0/3), hueToChannel(p, q, h), hueToChannel(p, q, h-1.0/3)
}

func hsvToRGB(h, s, v float64) (float64, float64, float64) {
	i := int(math.Floor(h * 6))
	f := h*6 - float64(i)
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	switch i % 6 {
	case 0:
		return v, t, p
	case 1:
		return q, v, p
	case 2:
		return p, v, t
	case 3:
		return p, q, v
	case 4:
		return t, p, v
	}
	return v, p, q
}

// ---- inline styles -----------------------------------------------------------------------------------------

// elementDeclarations is normalize._declarations.
func elementDeclarations(style string) map[string]string {
	found := map[string]string{}
	for _, part := range strings.Split(style, ";") {
		name, value, ok := strings.Cut(part, ":")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		cleaned := strings.TrimSpace(value)
		cleaned = strings.TrimSpace(strings.TrimSuffix(cleaned, "!important"))
		found[strings.ToLower(strings.TrimSpace(name))] = cleaned
	}
	return found
}

// ---- paint resolution --------------------------------------------------------------------------------------

// elementPainter maps one paint value to what the stored SVG should carry.
type elementPainter struct{ spec normalizeSpec }

// resolve is _Painter.resolve.
func (p elementPainter) resolve(raw *string, fallback string) (string, *float64) {
	value := fallback
	if raw != nil {
		value = *raw
	}
	value = strings.TrimSpace(value)
	lowered := strings.ToLower(value)
	switch lowered {
	case "none", "transparent":
		return "none", nil
	case "currentcolor":
		return p.spec.color(), nil
	}
	if strings.HasPrefix(value, "url(") {
		return value, nil
	}
	parsed := parseCSSColor(value)
	if parsed == nil {
		if p.spec.Mode == "mono" {
			return p.spec.color(), nil
		}
		return value, nil
	}
	if p.spec.Mode == "mono" {
		return p.spec.color(), parsed.Alpha
	}
	return parsed.Hex, parsed.Alpha
}

// paintShape is _paint_shape: false means the shape is invisible and goes.
func paintShape(node *elementNode, effective map[string]string, painter elementPainter) bool {
	if node.attr("display") == "none" || node.attr("visibility") == "hidden" {
		return false
	}
	fillRaw, fillPresent := effective["fill"]
	var fillPtr *string
	if fillPresent {
		fillPtr = &fillRaw
	}
	fill, fillAlpha := painter.resolve(fillPtr, "#000000")
	strokeRaw, strokePresent := effective["stroke"]
	var strokePtr *string
	if strokePresent {
		strokePtr = &strokeRaw
	}
	stroke, strokeAlpha := painter.resolve(strokePtr, "none")
	if fill == "none" && stroke == "none" {
		return false
	}
	for _, name := range elementInherited {
		node.removeAttr(name)
	}
	node.setAttr("fill", fill)
	if fill != "none" {
		for _, name := range elementFillProps {
			if value, ok := effective[name]; ok {
				node.setAttr(name, value)
			}
		}
		if fillAlpha != nil && !node.hasAttr("fill-opacity") {
			node.setAttr("fill-opacity", fmtNumber(*fillAlpha))
		}
	}
	if stroke != "none" {
		node.setAttr("stroke", stroke)
		for _, name := range elementStrokeProps {
			if value, ok := effective[name]; ok {
				node.setAttr(name, value)
			}
		}
		if strokeAlpha != nil && !node.hasAttr("stroke-opacity") {
			node.setAttr("stroke-opacity", fmtNumber(*strokeAlpha))
		}
	}
	return true
}

// recolorInPlace is _recolor_in_place (defs, masks, documents using <use>).
func recolorInPlace(node *elementNode, painter elementPainter) {
	for _, name := range elementPaintAttrs {
		raw, ok := node.getAttr(name)
		if !ok {
			continue
		}
		if painter.spec.Mode == "mono" && strings.ToLower(strings.TrimSpace(raw)) != "currentcolor" {
			continue
		}
		value, alpha := painter.resolve(&raw, raw)
		node.setAttr(name, value)
		if alpha != nil {
			if alphaName, ok := elementAlphaAttr[name]; ok && !node.hasAttr(alphaName) {
				node.setAttr(alphaName, fmtNumber(*alpha))
			}
		}
	}
}

// walkElement is normalize._walk.
func walkElement(node *elementNode, inherited map[string]string, painter elementPainter, opaque bool) {
	var kept []*elementNode
	for _, child := range node.children {
		name := child.localName()
		if elementDroppedElements[name] {
			continue
		}
		inlineElementStyle(child)
		childOpaque := opaque || elementOpaque[name]
		if childOpaque {
			recolorInPlace(child, painter)
			walkElement(child, map[string]string{}, painter, true)
			kept = append(kept, child)
			continue
		}
		own := map[string]string{}
		for _, key := range elementInherited {
			if value, ok := child.getAttr(key); ok {
				own[key] = value
			}
		}
		effective := map[string]string{}
		for key, value := range inherited {
			effective[key] = value
		}
		for key, value := range own {
			effective[key] = value
		}
		if elementShapes[name] {
			if paintShape(child, effective, painter) {
				kept = append(kept, child)
			}
			continue
		}
		if elementContainers[name] {
			for _, key := range elementInherited {
				child.removeAttr(key)
			}
			walkElement(child, effective, painter, false)
			if len(child.children) > 0 {
				kept = append(kept, child)
			}
			continue
		}
		kept = append(kept, child)
	}
	node.children = kept
}

// -------- elementNode: an ordered XML node (attribute order and text kept) ---------------------------------

// elementNode is one element of the parsed SVG tree.
type elementNode struct {
	name     string // local name
	attrs    []xml.Attr
	text     string
	tail     string
	children []*elementNode
	// textSet tracks whether text was explicitly cleared (Python None vs "").
	textSet bool
}

func (n *elementNode) localName() string { return n.name }

func (n *elementNode) getAttr(key string) (string, bool) {
	for _, attr := range n.attrs {
		if attr.Name.Local == key && attr.Name.Space == "" {
			return attr.Value, true
		}
	}
	return "", false
}

func (n *elementNode) attr(key string) string {
	value, _ := n.getAttr(key)
	return value
}

func (n *elementNode) hasAttr(key string) bool {
	_, ok := n.getAttr(key)
	return ok
}

func (n *elementNode) setAttr(key, value string) {
	for i := range n.attrs {
		if n.attrs[i].Name.Local == key && n.attrs[i].Name.Space == "" {
			n.attrs[i].Value = value
			return
		}
	}
	n.attrs = append(n.attrs, xml.Attr{Name: xml.Name{Local: key}, Value: value})
}

func (n *elementNode) removeAttr(key string) {
	out := n.attrs[:0]
	for _, attr := range n.attrs {
		if attr.Name.Local == key && attr.Name.Space == "" {
			continue
		}
		out = append(out, attr)
	}
	n.attrs = out
}

func (n *elementNode) prepend(child *elementNode) {
	n.children = append([]*elementNode{child}, n.children...)
}

func (n *elementNode) iterateAll(visit func(*elementNode)) {
	visit(n)
	for _, child := range n.children {
		child.iterateAll(visit)
	}
}

// parseElementTree parses the document into ordered nodes (Go's decoder never
// resolves entities or fetches DTDs).
func parseElementTree(data []byte) (*elementNode, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	decoder.Strict = true
	root := &elementNode{name: "#document"}
	stack := []*elementNode{root}
	for {
		token, err := decoder.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("Invalid or unsafe SVG.")
		}
		switch t := token.(type) {
		case xml.StartElement:
			parent := stack[len(stack)-1]
			node := &elementNode{name: t.Name.Local, attrs: append([]xml.Attr{}, t.Attr...)}
			parent.children = append(parent.children, node)
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			current := stack[len(stack)-1]
			if len(current.children) == 0 {
				current.text += string(t)
				current.textSet = true
			} else {
				current.children[len(current.children)-1].tail += string(t)
			}
		}
	}
	if len(root.children) != 1 {
		return nil, fmt.Errorf("Not an SVG document.")
	}
	return root.children[0], nil
}

// inlineElementStyle moves a `style` attribute's declarations into
// presentation attributes (a style wins).
func inlineElementStyle(node *elementNode) {
	style, ok := node.getAttr("style")
	if !ok {
		return
	}
	node.removeAttr("style")
	if style == "" {
		return
	}
	for name, value := range elementDeclarations(style) {
		if svg.AttrAllowed(name) {
			node.setAttr(name, value)
		}
	}
}

// referencedIDs is normalize._referenced_ids.
func referencedIDs(root *elementNode) map[string]bool {
	refs := map[string]bool{}
	root.iterateAll(func(node *elementNode) {
		for _, attr := range node.attrs {
			value := attr.Value
			if (attr.Name.Local == "href") && strings.HasPrefix(value, "#") {
				refs[value[1:]] = true
				continue
			}
			if strings.Contains(value, "url(") {
				for _, match := range urlRefPattern.FindAllStringSubmatch(value, -1) {
					refs[match[1]] = true
				}
			}
		}
		if node.name == "style" && node.text != "" {
			for _, match := range urlRefPattern.FindAllStringSubmatch(node.text, -1) {
				refs[match[1]] = true
			}
		}
	})
	return refs
}

// tidyElement drops unreferenced ids and inter-element whitespace (text
// content is left alone).
func tidyElement(node *elementNode, referenced map[string]bool, inText bool) {
	keepText := inText || spacePreserving[node.name]
	if id, ok := node.getAttr("id"); ok && !referenced[id] {
		node.removeAttr("id")
	}
	if !keepText {
		if strings.TrimSpace(node.text) == "" {
			node.text = ""
			node.textSet = false
		}
	}
	for _, child := range node.children {
		tidyElement(child, referenced, keepText)
		if !keepText && strings.TrimSpace(child.tail) == "" {
			child.tail = ""
		}
	}
}

// elementNumber is normalize._number.
func elementNumber(value string) (float64, bool) {
	match := numberPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, false
	}
	number, err := strconv.ParseFloat(match[1], 64)
	return number, err == nil
}

// elementViewBox is normalize._viewbox.
func elementViewBox(root *elementNode) (float64, float64, float64, float64, error) {
	parts := strings.Fields(strings.ReplaceAll(root.attr("viewBox"), ",", " "))
	if len(parts) == 4 {
		var values [4]float64
		ok := true
		for i, part := range parts {
			number, err := strconv.ParseFloat(part, 64)
			if err != nil {
				ok = false
				break
			}
			values[i] = number
		}
		if ok && values[2] > 0 && values[3] > 0 {
			return values[0], values[1], values[2], values[3], nil
		}
	}
	width, widthOK := elementNumber(root.attr("width"))
	height, heightOK := elementNumber(root.attr("height"))
	if widthOK && heightOK && width > 0 && height > 0 {
		return 0, 0, width, height, nil
	}
	return 0, 0, 0, 0, fmt.Errorf("The SVG has neither a usable viewBox nor a width and height.")
}

// setElementSize is normalize._set_size.
func setElementSize(root *elementNode, spec normalizeSpec) error {
	x, y, w, h, err := elementViewBox(root)
	if err != nil {
		return err
	}
	width, widthOK := elementNumber(root.attr("width"))
	height, heightOK := elementNumber(root.attr("height"))
	if spec.Size != nil {
		scale := *spec.Size / math.Max(w, h)
		width, height = w*scale, h*scale
	} else if !(widthOK && heightOK) {
		width, height = w, h
	}
	root.setAttr("viewBox", fmt.Sprintf("%s %s %s %s", fmtNumber(x), fmtNumber(y), fmtNumber(w), fmtNumber(h)))
	root.setAttr("width", fmtNumber(width))
	root.setAttr("height", fmtNumber(height))
	if spec.Frame {
		frame := &elementNode{name: "rect"}
		frame.setAttr("x", fmtNumber(x))
		frame.setAttr("y", fmtNumber(y))
		frame.setAttr("width", fmtNumber(w))
		frame.setAttr("height", fmtNumber(h))
		frame.setAttr("fill", "none")
		root.prepend(frame)
	}
	return nil
}

// normalizeSVG is normalize.normalize_svg: the recolourable form of `raw`.
func normalizeSVG(raw []byte, spec normalizeSpec) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("Empty SVG.")
	}
	if len(raw) > svg.MaxBytes {
		return nil, fmt.Errorf("SVG is too large.")
	}
	root, err := parseElementTree(raw)
	if err != nil {
		return nil, err
	}
	if root.name != "svg" {
		return nil, fmt.Errorf("Not an SVG document.")
	}
	painter := elementPainter{spec: spec}
	inlineElementStyle(root)
	dropped := append([]string{}, elementRootDropped...)
	hasUse := false
	root.iterateAll(func(node *elementNode) {
		if node.name == "use" {
			hasUse = true
		}
	})
	if hasUse {
		// Referenced shapes still inherit from the root.
		for _, name := range elementInherited {
			dropped = removeString(dropped, name)
		}
		root.iterateAll(func(node *elementNode) {
			inlineElementStyle(node)
			recolorInPlace(node, painter)
		})
	} else {
		dropped = append(dropped, elementInherited...)
		inherited := map[string]string{}
		for _, key := range elementInherited {
			if value, ok := root.getAttr(key); ok {
				inherited[key] = value
			}
		}
		walkElement(root, inherited, painter, false)
	}
	visible := false
	root.iterateAll(func(node *elementNode) {
		if elementShapes[node.name] || node.name == "use" || node.name == "image" {
			visible = true
		}
	})
	if !visible {
		return nil, fmt.Errorf("The SVG contains no visible shapes.")
	}
	if err := setElementSize(root, spec); err != nil {
		return nil, err
	}
	for _, key := range append([]string{}, dropped...) {
		root.removeAttr(key)
	}
	rootAttrs := root.attrs[:0]
	for _, attr := range root.attrs {
		name := attr.Name.Local
		if strings.HasPrefix(name, "aria-") || strings.HasPrefix(name, "data-") {
			continue
		}
		rootAttrs = append(rootAttrs, attr)
	}
	root.attrs = rootAttrs
	tidyElement(root, referencedIDs(root), false)
	return serializeElement(root), nil
}

func removeString(values []string, want string) []string {
	out := values[:0]
	for _, value := range values {
		if value != want {
			out = append(out, value)
		}
	}
	return out
}

// attributeOrder is Python ElementTree's: insertion order for most, but the
// serializer writes attributes in the order they were set (Go keeps it too).
var _ = sort.Strings
