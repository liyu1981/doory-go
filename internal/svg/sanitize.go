// SVG sanitiser + inspector — Go port of apps/core/svg.py.
//
// Hostile input rules: no DTD/entities/XXE (Go's decoder never fetches and
// rejects entity references), element/attr allow-lists, script/animation/
// foreign-content vectors dropped WITH their content, style/url() CSS
// safety with escape unescaping, internal (#id) refs or small raster data:
// images only, <a> links opt-in (http/https/mailbox, no credentials, no
// whitespace), use-expansion bounds.
//
// Ports backend/apps/core/svg.py
package svg

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const (
	SVGNS            = "http://www.w3.org/2000/svg"
	XLINK            = "http://www.w3.org/1999/xlink"
	MaxBytes         = 5 * 1024 * 1024
	MaxNodes         = 50_000
	MaxDepth         = 64
	maxExpandedNodes = 1_000_000
	maxUseDepth      = 200
	maxLinkLength    = 2048
)

// Error is SvgError: the input is not a usable SVG document.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func svgErrorf(format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

var allowedElements = setOf("svg", "g", "defs", "symbol", "use", "title", "desc", "metadata", "path",
	"rect", "circle", "ellipse", "line", "polyline", "polygon", "text", "tspan", "textPath",
	"linearGradient", "radialGradient", "stop", "pattern", "clipPath", "mask", "marker", "image",
	"style", "filter", "feBlend", "feColorMatrix", "feComponentTransfer", "feComposite",
	"feConvolveMatrix", "feDiffuseLighting", "feDisplacementMap", "feDropShadow", "feFlood",
	"feFuncA", "feFuncB", "feFuncG", "feFuncR", "feGaussianBlur", "feMerge", "feMergeNode",
	"feMorphology", "feOffset", "feSpecularLighting", "feTile", "feTurbulence", "feDistantLight",
	"fePointLight", "feSpotLight")

var allowedAttributes = setOf("id", "class", "x", "y", "x1", "x2", "y1", "y2", "cx", "cy", "r", "rx",
	"ry", "width", "height", "d", "points", "transform", "viewBox", "preserveAspectRatio", "version",
	"xmlns", "fill", "fill-opacity", "fill-rule", "stroke", "stroke-width", "stroke-opacity",
	"stroke-linecap", "stroke-linejoin", "stroke-miterlimit", "stroke-dasharray", "stroke-dashoffset",
	"opacity", "clip-path", "clip-rule", "mask", "filter", "marker-start", "marker-mid", "marker-end",
	"offset", "stop-color", "stop-opacity", "gradientUnits", "gradientTransform", "spreadMethod",
	"fx", "fy", "fr", "patternUnits", "patternContentUnits", "patternTransform", "clipPathUnits",
	"maskUnits", "maskContentUnits", "markerWidth", "markerHeight", "markerUnits", "refX", "refY",
	"orient", "font-family", "font-size", "font-weight", "font-style", "text-anchor",
	"dominant-baseline", "letter-spacing", "word-spacing", "text-decoration", "dx", "dy", "rotate",
	"textLength", "lengthAdjust", "startOffset", "method", "spacing", "display", "visibility",
	"overflow", "color", "mix-blend-mode", "isolation", "vector-effect", "shape-rendering",
	"image-rendering", "color-interpolation-filters", "in", "in2", "result", "mode", "type",
	"values", "stdDeviation", "flood-color", "flood-opacity", "operator", "k1", "k2", "k3", "k4",
	"scale", "xChannelSelector", "yChannelSelector", "baseFrequency", "numOctaves", "seed",
	"stitchTiles", "order", "kernelMatrix", "divisor", "bias", "targetX", "targetY", "edgeMode",
	"radius", "slope", "intercept", "amplitude", "exponent", "tableValues", "azimuth", "elevation",
	"z", "pointsAtX", "pointsAtY", "pointsAtZ", "specularExponent", "surfaceScale", "diffuseConstant",
	"specularConstant", "lighting-color", "limitingConeAngle", "filterUnits", "primitiveUnits",
	"xml:space", "xml:lang", "role", "aria-label", "aria-hidden", "focusable")

func setOf(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

var safeDataURL = regexp.MustCompile(`(?i)^data:image/(png|jpeg|jpg|webp|gif);base64,[A-Za-z0-9+/=\s]+$`)

var urlRef = regexp.MustCompile(`(?is)url\(\s*(['"]?)(.*?)['"]?\s*\)`)

var dangerousCSS = regexp.MustCompile(`(?i)@import|expression\s*\(|javascript:|behavior\s*:|-moz-binding|@namespace|<\s*/?\s*script|image-set|cross-fade|\bsrc\s*\(`)

var cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

var cssEscape = regexp.MustCompile(`(?s)\\(?:([0-9a-fA-F]{1,6})[ \t\r\n\f]?|(.))`)

// node is an element with its sanitized children order preserved.
type node struct {
	name     string
	attrs    [][2]string // insertion order preserved (Python dict order)
	text     string
	children []*node
}

// unescapeCSS ports _unescape_css: comments dropped, escapes resolved.
func unescapeCSS(css string) string {
	css = cssComment.ReplaceAllString(css, "")
	return cssEscape.ReplaceAllStringFunc(css, func(m string) string {
		sub := cssEscape.FindStringSubmatch(m)
		if sub[1] == "" {
			return sub[2]
		}
		code, err := strconv.ParseInt(sub[1], 16, 32)
		if err != nil || code <= 0 || code > 0x10FFFF || (code >= 0xD800 && code <= 0xDFFF) {
			return "�"
		}
		return string(rune(code))
	})
}

func cssIsSafe(css string) bool {
	css = unescapeCSS(css)
	if dangerousCSS.MatchString(css) {
		return false
	}
	for _, m := range urlRef.FindAllStringSubmatch(css, -1) {
		target := strings.TrimSpace(m[2])
		if !strings.HasPrefix(target, "#") && !safeDataURL.MatchString(target) {
			return false
		}
	}
	return true
}

// SafeLink ports safe_link.
func SafeLink(value string) (string, bool) {
	link := strings.TrimSpace(value)
	if link == "" || len(link) > maxLinkLength {
		return "", false
	}
	for _, r := range link {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return "", false
		}
	}
	parsed, err := url.Parse(link)
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
		if parsed.Host == "" || parsed.User != nil {
			return "", false
		}
	case "mailto":
		if parsed.Opaque == "" && parsed.Path == "" {
			return "", false
		}
		// mailto: no host allowed
		if parsed.Host != "" && parsed.Opaque == "" {
			return "", false
		}
	default:
		return "", false
	}
	return link, true
}

func cleanAttrValue(value string) bool {
	low := strings.ToLower(strings.TrimSpace(value))
	return !strings.Contains(low, "javascript:") && !strings.Contains(low, "vbscript:") &&
		!strings.Contains(low, "<script")
}

// parseTree parses SVG XML safely and builds the node tree (rejecting DTD
// directives and entity references — Go's decoder never fetches but we are
// strict like defusedxml: any Directive is fatal).
func parseTree(data []byte) (*node, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var stack []*node
	var root *node
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, svgErrorf("Invalid or unsafe SVG.")
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, svgErrorf("Invalid or unsafe SVG.") // DTD/entity declarations
		case xml.StartElement:
			n := &node{name: localName(t.Name)}
			if t.Name.Space != "" && t.Name.Space != SVGNS && t.Name.Space != XLINK {
				return nil, svgErrorf("Invalid or unsafe SVG.")
			}
			for _, a := range t.Attr {
				key := a.Name.Local
				if a.Name.Space == XLINK {
					key = "xlink:" + a.Name.Local
				} else if a.Name.Space == XMLNS {
					key = "xmlns:" + a.Name.Local
					if a.Name.Local == "" {
						key = "xmlns"
					}
				} else if a.Name.Space == "http://www.w3.org/XML/1998/namespace" {
					key = "xml:" + a.Name.Local
				}
				n.attrs = append(n.attrs, [2]string{key, a.Value})
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			} else if root == nil {
				root = n
			} else {
				return nil, svgErrorf("Invalid or unsafe SVG.") // multiple roots
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, svgErrorf("Invalid or unsafe SVG.")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, svgErrorf("Invalid or unsafe SVG.")
	}
	return root, nil
}

const XMLNS = "xmlns"

func localName(n xml.Name) string { return n.Local }

func setAttr(n *node, key, value string) {
	for i, a := range n.attrs {
		if a[0] == key {
			n.attrs[i][1] = value
			return
		}
	}
	n.attrs = append(n.attrs, [2]string{key, value})
}

func removeAttr(n *node, key string) {
	out := n.attrs[:0]
	for _, a := range n.attrs {
		if a[0] != key {
			out = append(out, a)
		}
	}
	n.attrs = out
}

func getAttr(n *node, key string) string {
	for _, a := range n.attrs {
		if a[0] == key {
			return a[1]
		}
	}
	return ""
}

// sanitizeElement ports _sanitize_element: in-place; returns false when the
// element (with its content) must be removed.
func sanitizeElement(n *node, depth int, counter *int, allowLinks bool) bool {
	*counter++
	if *counter > MaxNodes || depth > MaxDepth {
		throwTooComplex()
	}
	if n.name != "svg" && n.name != "" && !allowedElements[n.name] && !(allowLinks && n.name == "a") {
		return false
	}
	if n.name == "style" {
		if !cssIsSafe(n.text) {
			return false
		}
	}
	out := n.attrs[:0]
	for _, a := range n.attrs {
		key, value := a[0], a[1]
		switch {
		case key == "href" || key == "xlink:href":
			v := strings.TrimSpace(value)
			if strings.HasPrefix(v, "#") || (n.name == "image" && safeDataURL.MatchString(v)) {
				out = append(out, a)
				continue
			}
			if n.name == "a" && allowLinks {
				if link, ok := SafeLink(v); ok {
					out = append(out, [2]string{key, link})
					continue
				}
			}
			// dropped
		case key == "style":
			if cssIsSafe(value) {
				out = append(out, a)
			}
		case strings.HasPrefix(strings.ToLower(key), "on") || strings.HasPrefix(key, "{"):
			// event handlers and extension attributes: dropped
		case strings.HasPrefix(key, "xml:"):
			// xml:* presentation attrs kept only when allow-listed
			if allowedAttributes[key] {
				out = append(out, a)
			}
		case !allowedAttributes[key] || !cleanAttrValue(value):
			// dropped
		default:
			if (strings.Contains(value, "(") || strings.Contains(value, "\\")) && !cssIsSafe(value) {
				continue // paint-server/filter url(...) values
			}
			out = append(out, a)
		}
	}
	n.attrs = out
	kept := n.children[:0]
	for _, c := range n.children {
		if sanitizeElement(c, depth+1, counter, allowLinks) {
			kept = append(kept, c)
		}
	}
	n.children = kept
	return true
}

func throwTooComplex() { panic(tooComplexPanic{}) }

type tooComplexPanic struct{}

// serializeNode renders the tree like ElementTree.tostring: default SVG
// namespace on the root, xlink prefixed where used, no xml declaration.
func serializeNode(root *node) []byte {
	var b bytes.Buffer
	b.WriteString(`<svg xmlns="` + SVGNS + `"`)
	usedXlink := hasXlink(root)
	if usedXlink {
		b.WriteString(` xmlns:xlink="` + XLINK + `"`)
	}
	writeAttrs(&b, root)
	b.WriteByte('>')
	writeChildren(&b, root)
	b.WriteString(`</svg>`)
	return b.Bytes()
}

func hasXlink(n *node) bool {
	for _, a := range n.attrs {
		if strings.HasPrefix(a[0], "xlink:") {
			return true
		}
	}
	for _, c := range n.children {
		if hasXlink(c) {
			return true
		}
	}
	return false
}

func xmlEscapeAttr(s string) string {
	var b strings.Builder
	enc := xml.EscapeText(&b, []byte(s))
	_ = enc
	return b.String()
}

func writeAttrs(b *bytes.Buffer, n *node) {
	for _, a := range n.attrs {
		key := a[0]
		if key == "xmlns" {
			continue // emitted on the root
		}
		if strings.HasPrefix(key, "xlink:") {
			key = "xlink:" + key[len("xlink:"):]
		}
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteString(`="`)
		b.WriteString(xmlEscapeAttr(a[1]))
		b.WriteByte('"')
	}
}

func writeChildren(b *bytes.Buffer, n *node) {
	for _, c := range n.children {
		b.WriteByte('<')
		b.WriteString(c.name)
		writeAttrs(b, c)
		b.WriteByte('>')
		b.WriteString(escapeText(c.text))
		writeChildren(b, c)
		b.WriteString("</")
		b.WriteString(c.name)
		b.WriteByte('>')
	}
}

func escapeText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Sanitize ports sanitize_svg: a sanitised copy or an error.
func Sanitize(data []byte, allowLinks bool) ([]byte, error) {
	if len(data) == 0 {
		return nil, svgErrorf("Empty SVG.")
	}
	if len(data) > MaxBytes {
		return nil, svgErrorf("SVG is too large.")
	}
	root, err := parseTree(data)
	if err != nil {
		return nil, err
	}
	if root.name != "svg" {
		return nil, svgErrorf("Not an SVG document.")
	}
	counter := 0
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(tooComplexPanic); ok {
				panic(r) // SvgError equivalent: rethrow as error below via named path
			}
			panic(r)
		}
	}()
	if !sanitizeElement(root, 0, &counter, allowLinks) {
		return nil, svgErrorf("Not an SVG document.")
	}
	if err := checkExpansion(root); err != nil {
		return nil, err
	}
	return serializeNode(root), nil
}

// checkExpansion ports _check_expansion: refuse drawings that are small but
// draw exponentially much through <use> chains (memoized sizes, use-depth
// bound — iterative post-order recursion with a budget).
func checkExpansion(root *node) error {
	memo := map[*node]int{}
	var size func(n *node, active map[*node]bool, useDepth int) (int, error)
	size = func(n *node, active map[*node]bool, useDepth int) (int, error) {
		if v, ok := memo[n]; ok {
			return v, nil
		}
		if active[n] || useDepth > maxUseDepth {
			return 0, svgErrorf("SVG is too complex.")
		}
		active[n] = true
		defer delete(active, n)
		total := 1
		for _, c := range n.children {
			v, err := size(c, active, useDepth)
			if err != nil {
				return 0, err
			}
			total += v
		}
		if n.name == "use" {
			href := getAttr(n, "href")
			if href == "" {
				href = getAttr(n, "xlink:href")
			}
			if strings.HasPrefix(href, "#") {
				if target := findByID(root, href[1:]); target != nil {
					v, err := size(target, active, useDepth+1)
					if err != nil {
						return 0, err
					}
					total += v
				}
			}
		}
		if total > maxExpandedNodes {
			return 0, svgErrorf("SVG is too complex.")
		}
		memo[n] = total
		return total, nil
	}
	_, err := size(root, map[*node]bool{}, 0)
	return err
}

func findByID(n *node, id string) *node {
	if getAttr(n, "id") == id {
		return n
	}
	for _, c := range n.children {
		if found := findByID(c, id); found != nil {
			return found
		}
	}
	return nil
}

// SvgInfo mirrors the Python dataclass (intrinsic size).
type SvgInfo struct {
	Width  float64
	Height float64
}

var lengthRe = regexp.MustCompile(`(?i)^\s*([0-9]*\.?[0-9]+)\s*(px|pt|mm|cm|in|pc|em|%)?\s*$`)

var unitToPx = map[string]float64{
	"px": 1, "pt": 96 / 72, "pc": 16, "mm": 96 / 25.4, "cm": 96 / 2.54, "in": 96, "em": 16, "": 1,
}

// toPx ports _to_px.
func toPx(value string) (float64, bool) {
	if strings.TrimSpace(value) == "" {
		return 0, false
	}
	m := lengthRe.FindStringSubmatch(value)
	if m == nil {
		return 0, false
	}
	unit := strings.ToLower(m[2])
	if unit == "%" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return f * unitToPx[unit], true
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// svgInfoFromRoot ports _svg_info_from_root: width/height or viewBox
// derivation with the 300x150 fallback.
func svgInfoFromRoot(root *node) SvgInfo {
	w, hasW := toPx(getAttr(root, "width"))
	h, hasH := toPx(getAttr(root, "height"))
	vb := strings.Fields(strings.ReplaceAll(getAttr(root, "viewBox"), ",", " "))
	if len(vb) == 4 {
		vbw, err1 := strconv.ParseFloat(vb[2], 64)
		vbh, err2 := strconv.ParseFloat(vb[3], 64)
		if err1 == nil && err2 == nil && vbw > 0 && vbh > 0 {
			switch {
			case hasW && !hasH:
				h = w * vbh / vbw
			case hasH && !hasW:
				w = h * vbw / vbh
			case !hasW && !hasH:
				w, h = vbw, vbh
			}
		}
	}
	width, height := round2(w), round2(h)
	if !hasW {
		width = 300
	}
	if !hasH {
		height = 150
	}
	return SvgInfo{Width: width, Height: height}
}

// Info returns the intrinsic size of a SANITISED svg (svg_info).
func Info(sanitized []byte) (SvgInfo, error) {
	root, err := parseTree(sanitized)
	if err != nil {
		return SvgInfo{}, err
	}
	return svgInfoFromRoot(root), nil
}

// WithIntrinsicSize ports with_intrinsic_size: root width/height set; a
// root without a viewBox gets one for its original size so the drawing
// scales instead of being cut.
func WithIntrinsicSize(sanitized []byte, width, height int) ([]byte, error) {
	root, err := parseTree(sanitized)
	if err != nil {
		return nil, err
	}
	vb := strings.Fields(strings.ReplaceAll(getAttr(root, "viewBox"), ",", " "))
	if len(vb) != 4 {
		info := svgInfoFromRoot(root)
		setAttr(root, "viewBox", fmt.Sprintf("0 0 %g %g", info.Width, info.Height))
	}
	setAttr(root, "width", strconv.Itoa(width))
	setAttr(root, "height", strconv.Itoa(height))
	return serializeNode(root), nil
}

// PrepareForBox sizes a SANITISED svg to the artwork box
// (layout.prepare_page_svg): valid viewBox kept (else derived), x/y
// dropped, width/height/preserveAspectRatio set.
func PrepareForBox(sanitized []byte, artWidth, artHeight string, slice bool) ([]byte, error) {
	root, err := parseTree(sanitized)
	if err != nil {
		return nil, err
	}
	if !validViewBox(getAttr(root, "viewBox")) {
		if getAttr(root, "width") != "" && getAttr(root, "height") != "" {
			info := svgInfoFromRoot(root)
			setAttr(root, "viewBox", fmt.Sprintf("0 0 %s %s", px(info.Width), px(info.Height)))
		} else {
			// fall back to the geometry box via the caller's values
			setAttr(root, "viewBox", "0 0 "+artWidth+" "+artHeight)
		}
	}
	removeAttr(root, "x")
	removeAttr(root, "y")
	setAttr(root, "width", artWidth)
	setAttr(root, "height", artHeight)
	par := "xMidYMid meet"
	if slice {
		par = "xMidYMid slice"
	}
	setAttr(root, "preserveAspectRatio", par)
	return serializeNode(root), nil
}

// px formats like layout.fmt: 4 decimals, trailing zeros stripped.
func px(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// validViewBox ports layout._valid_view_box.
func validViewBox(value string) bool {
	parts := strings.Fields(strings.ReplaceAll(value, ",", " "))
	if len(parts) != 4 {
		return false
	}
	nums := make([]float64, 4)
	for i, part := range parts {
		f, err := strconv.ParseFloat(part, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
		nums[i] = f
	}
	return nums[2] > 0 && nums[3] > 0
}
