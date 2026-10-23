// Download filenames (apps/exports/filenames.py): the same rules as the
// editor's engine/export/filenames.ts — simple (title-based) and
// structured (`<title>_<platform>_<preset>_<W>x<H>[_pNN][_<unix>].<ext>`).
//
// Ports backend/apps/exports/filenames.py
package doory

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	defaultStem   = "design"
	maxStemLength = 80
	maxSlugLength = 80
)

var (
	disallowedInStem = regexp.MustCompile(`[^\w\- .()]+`)
	whitespaceRe     = regexp.MustCompile(`\s+`)
	spacesRe         = regexp.MustCompile(`[\s._]{2,}`)
	notSlug          = regexp.MustCompile(`[^a-z0-9]+`)
)

// exportFilename ports export_filename: NFC, collapsed whitespace,
// control-free, unsafe punctuation stripped, trimmed, clipped.
func exportFilename(title, extension string) string {
	text := norm.NFC.String(title)
	text = whitespaceRe.ReplaceAllString(text, " ")
	text = removeControl(text)
	text = disallowedInStem.ReplaceAllString(text, "")
	text = spacesRe.ReplaceAllString(text, " ")
	text = strings.Trim(text, " .-_")
	runes := []rune(text)
	if len(runes) > maxStemLength {
		runes = runes[:maxStemLength]
	}
	text = strings.TrimSpace(string(runes))
	text = strings.Trim(text, " .-_")
	if text == "" {
		text = defaultStem
	}
	return text + "." + extension
}

func removeControl(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.C, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// slugPort ports slug: NFKD-folded lowercase alphanumerics joined by dashes.
func slugPort(text string, limit int) string {
	folded := strings.Map(func(r rune) rune {
		if r > 127 {
			return -1 // accents dropped (NFKD+ascii-ignore approximation)
		}
		return r
	}, text)
	folded = notSlug.ReplaceAllString(strings.ToLower(folded), "-")
	folded = strings.Trim(folded, "-")
	runes := []rune(folded)
	if len(runes) > limit {
		folded = string(runes[:limit])
	}
	return strings.Trim(folded, "-")
}

func pxRound(v float64) int { return int(v + 0.5) } // half up, Math.round

// structuredFilename ports structured_filename.
func structuredFilename(title, extension string, platform, preset string,
	width, height float64, page, pages int, unix *int, suffix string,
) string {
	parts := []string{slugPort(title, maxSlugLength)}
	if parts[0] == "" {
		parts[0] = defaultStem
	}
	if preset != "" {
		if platform != "" {
			parts = append(parts, slugPort(platform, maxSlugLength))
		}
		parts = append(parts, slugPort(preset, maxSlugLength))
	}
	if width > 0 && height > 0 {
		parts = append(parts, strconv.Itoa(pxRound(width))+"x"+strconv.Itoa(pxRound(height)))
	}
	if page > 0 && pages > 1 {
		widthDigits := len(strconv.Itoa(pages))
		if widthDigits < 2 {
			widthDigits = 2
		}
		parts = append(parts, "p"+leftPad(strconv.Itoa(page), widthDigits))
	}
	if suffix != "" {
		parts = append(parts, slugPort(suffix, maxSlugLength))
	}
	if unix != nil {
		parts = append(parts, strconv.Itoa(*unix))
	}
	return strings.Join(parts, "_") + "." + extension
}

func leftPad(s string, width int) string {
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// pageFilename ports page_filename (image exports): simple or structured.
func (a *App) pageFilename(title string, result *imageExportResult, style string, unix *int) string {
	if style != "structured" {
		if result.PageCount > 1 {
			title = title + " - page " + strconv.Itoa(result.PageIndex+1)
		}
		return exportFilename(title, result.Format)
	}
	platform, preset := presetParts(a, result.Preset, result.PageWidth, result.PageHeight)
	return structuredFilename(title, result.Format, platform, preset,
		result.PageWidth, result.PageHeight, result.PageIndex+1, result.PageCount, unix, "")
}

// presetParts ports preset_parts: the platform+preset for a page still at
// its preset's size, else empty (impl: App.presetRowFor in presets.go).
func presetParts(a *App, presetSlug string, width, height float64) (string, string) {
	if presetSlug == "" {
		return "", ""
	}
	found := a.presetRowFor(presetSlug)
	if found == nil {
		return "", ""
	}
	if found.widthPx != pxRound(width) || found.heightPx != pxRound(height) {
		return "", ""
	}
	platform := ""
	if len(found.platforms) > 0 {
		platform = slugPort(found.platforms[0], maxSlugLength)
	}
	if platform == "" {
		platform = slugPort(found.groupSlug, maxSlugLength)
	}
	return platform, found.slug
}

// contentDisposition ports content_disposition_header(attachment=True):
// RFC 5987 for non-ASCII names.
func contentDisposition(filename string, attachment bool) string {
	disposition := "inline"
	if attachment {
		disposition = "attachment"
	}
	asciiName := strings.Map(func(r rune) rune {
		if r > 127 {
			return '?'
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, filename)
	if strings.EqualFold(asciiName, filename) {
		return disposition + `; filename="` + escapeQuotes(asciiName) + `"`
	}
	return disposition + `; filename="` + escapeQuotes(asciiName) + `"` +
		`; filename*=UTF-8''` + percentEncode(filename)
}

func escapeQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, "\\x22")
}

func percentEncode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("-_.~", r) {
			b.WriteRune(r)
			continue
		}
		for _, enc := range []byte(string(r)) {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(strconv.FormatInt(int64(enc), 16)))
		}
	}
	return b.String()
}

// pdfFilename ports pdf_filename (PDF downloads).
func pdfFilename(title string) string { return exportFilename(title, "pdf") }
