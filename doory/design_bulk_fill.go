// Bulk template filling (apps/designs/fill.py bulk_fill): one new design per
// row, each row reporting its design or its error. A bad row never stops the
// others.
package doory

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type bulkFillRow struct {
	Row         int       `json:"row"`
	OK          bool      `json:"ok"`
	DesignID    string    `json:"design_id,omitempty"`
	Title       string    `json:"title,omitempty"`
	Filled      *[]string `json:"filled,omitempty"`
	Skipped     *[]string `json:"skipped,omitempty"`
	Overflow    *[]string `json:"overflow,omitempty"`
	Error       string    `json:"error,omitempty"`
	ExportError string    `json:"export_error,omitempty"`
	Image       any       `json:"image,omitempty"`
}

type bulkFillResponse struct {
	Created int           `json:"created"`
	Rows    []bulkFillRow `json:"rows"`
}

type bulkImageInline struct {
	MIME       string `json:"mime"`
	DataBase64 string `json:"data_base64"`
	Bytes      int    `json:"bytes"`
}

type bulkImageURL struct {
	MIME             string `json:"mime"`
	URL              string `json:"url"`
	Bytes            int    `json:"bytes"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

// Simple fixed-window per-user rate counters for the bulk-row and export
// charges (the wider DRF throttle table is still G22).
type bulkRateWindow struct {
	start time.Time
	count int
}

var (
	bulkRateMu sync.Mutex
	bulkRates  = map[string]*bulkRateWindow{}
)

func allowBulkRate(userID, kind string, limit int) (bool, float64) {
	bulkRateMu.Lock()
	defer bulkRateMu.Unlock()
	key := kind + ":" + userID
	now := time.Now()
	window := bulkRates[key]
	if window == nil || now.Sub(window.start) >= time.Minute {
		bulkRates[key] = &bulkRateWindow{start: now, count: 1}
		return true, 0
	}
	if window.count >= limit {
		return false, time.Until(window.start.Add(time.Minute)).Seconds()
	}
	window.count++
	return true, 0
}

func (a *App) designBulkFill(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	template, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	if template.TrashedAt.Valid {
		writeDRF(w, http.StatusConflict, "design_trashed",
			"This design is in the trash. Restore it before changing it.")
		return
	}
	var body struct {
		Rows        []map[string]any `json:"rows"`
		RowNumbers  []int            `json:"row_numbers"`
		NamePattern *string          `json:"name_pattern"`
		Folder      json.RawMessage  `json:"folder"`
		Export      *string          `json:"export"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	if len(body.Rows) == 0 {
		fields = append(fields, SimpleField("rows", "This field is required.", "required"))
	} else if len(body.Rows) > a.Settings.BulkFillMaxRows {
		fields = append(fields, SimpleField("rows",
			fmt.Sprintf("At most %d rows at a time.", a.Settings.BulkFillMaxRows), "invalid"))
	}
	if body.RowNumbers != nil && len(body.RowNumbers) != len(body.Rows) {
		fields = append(fields, SimpleField("row_numbers",
			"Must have one entry per row when provided.", "invalid"))
	}
	for _, number := range body.RowNumbers {
		if number < 1 {
			fields = append(fields, SimpleField("row_numbers",
				"Ensure this value is greater than or equal to 1.", "min_value"))
			break
		}
	}
	namePattern := ""
	if body.NamePattern != nil {
		if len([]rune(*body.NamePattern)) > 200 {
			fields = append(fields, SimpleField("name_pattern",
				"Ensure this field has no more than 200 characters.", "max_length"))
		} else {
			namePattern = *body.NamePattern
		}
	}
	var folder *string
	if len(body.Folder) > 0 && string(body.Folder) != "null" {
		var folderID string
		if err := json.Unmarshal(body.Folder, &folderID); err != nil {
			fields = append(fields, SimpleField("folder", "Not a valid UUID.", "invalid"))
		} else if !uuidShape.MatchString(strings.ToLower(folderID)) {
			fields = append(fields, SimpleField("folder", uuidInvalidMsg(folderID), "invalid"))
		} else if existing, _ := a.getFolderByOwner(u.ID, hexUUID(folderID)); existing == nil {
			fields = append(fields, SimpleField("folder",
				fmt.Sprintf("Invalid pk %q - object does not exist.", folderID), "does_not_exist"))
		} else {
			folder = &folderID
		}
	}
	export := ""
	if body.Export != nil {
		value := strings.ToLower(strings.TrimSpace(*body.Export))
		if value != "png" && value != "jpg" {
			fields = append(fields, SimpleField("export",
				fmt.Sprintf("%q is not a valid choice.", *body.Export), "invalid_choice"))
		} else {
			export = value
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	if !acquireFillLock(u.ID) {
		writeDRF(w, http.StatusTooManyRequests, "throttled",
			"A fill is already running for your account. Try again when it has finished.")
		return
	}
	defer releaseFillLock(u.ID)

	rows := make([]bulkFillRow, 0, len(body.Rows))
	created := 0
	for index, values := range body.Rows {
		number := index + 1
		if len(body.RowNumbers) == len(body.Rows) {
			number = body.RowNumbers[index]
		}
		if ok, wait := allowBulkRate(u.ID, "bulk_fill", 120); !ok {
			rows = append(rows, bulkFillRow{Row: number, OK: false,
				Error: throttledMessageDetail("Request was throttled.", wait)})
			continue
		}
		title := rowName(namePattern, template.Title, number, values)
		result, fillErr, docErr := a.fillDesign(template, u, values, true, title, folder)
		if fillErr != nil {
			rows = append(rows, bulkFillRow{Row: number, OK: false, Error: fillErr.Error()})
			continue
		}
		if docErr != nil {
			rows = append(rows, bulkFillRow{Row: number, OK: false, Error: docErr.Message})
			continue
		}
		filled, skipped, overflow := result.Filled, result.Skipped, result.Overflow
		row := bulkFillRow{
			Row: number, OK: true, DesignID: dashedUUID(result.Design.ID), Title: result.Design.Title,
			Filled: &filled, Skipped: &skipped, Overflow: &overflow,
		}
		created++
		if export != "" {
			if ok, wait := allowBulkRate(u.ID, "export", 30); !ok {
				row.ExportError = throttledMessageDetail("Request was throttled.", wait)
			} else if image, err := a.bulkExportImage(result.Design, export, u); err != nil {
				row.ExportError = err.Error()
			} else {
				row.Image = image
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, bulkFillResponse{Created: created, Rows: rows})
}

func rowName(pattern, title string, number int, values map[string]any) string {
	if pattern == "" {
		pattern = fillDefaultName
	}
	known := map[string]string{"title": title, "n": fmt.Sprint(number)}
	matches := fillPlaceholderPattern.FindAllStringSubmatchIndex(pattern, -1)
	if len(matches) == 0 {
		name := strings.TrimSpace(pattern)
		if name == "" {
			return truncateRunes(fmt.Sprintf("%s - %d", title, number), 200)
		}
		return truncateRunes(name, 200)
	}
	var out strings.Builder
	last := 0
	for _, match := range matches {
		out.WriteString(pattern[last:match[0]])
		key := pattern[match[2]:match[3]]
		if value, ok := known[key]; ok {
			out.WriteString(value)
		} else if raw, ok := values[key]; ok {
			if value, err := fillTextValue(raw); err == nil {
				out.WriteString(value)
			}
		}
		last = match[1]
	}
	out.WriteString(pattern[last:])
	name := strings.TrimSpace(out.String())
	if name == "" {
		name = fmt.Sprintf("%s - %d", title, number)
	}
	return truncateRunes(name, 200)
}

func (a *App) bulkExportImage(design *designRow, format string, owner *UserRow) (any, error) {
	if !a.Settings.ServerImageExportEnabled {
		return nil, fmt.Errorf("Server-side image export is not enabled on this server.")
	}
	docAny, err := parseJSONValue([]byte(design.Document))
	if err != nil {
		return nil, fmt.Errorf("The design has no pages.")
	}
	docMap, _ := docAny.(map[string]any)
	page, index, perr := pickPage(docMap, nil)
	if perr != nil {
		return nil, fmt.Errorf("%s", perr.Message)
	}
	width, height, serr := pageSizeOf(docMap, page)
	if serr != nil {
		return nil, fmt.Errorf("%s", serr.Message)
	}
	if int64(width)*int64(height) > int64(a.Settings.ExportImageMaxPixels) {
		return nil, fmt.Errorf("The page is too large to export.")
	}
	payload := map[string]any{
		"document": docMap, "page": index, "multiplier": 1.0,
		"transparent": false, "fonts": a.fontManifest(docMap, owner.ID),
	}
	result, derr := a.renderImagePayload(payload)
	if derr != nil {
		return nil, fmt.Errorf("%s", derr.Message)
	}
	data, cerr := convertImage(result.Data, format, 90, false, false)
	if cerr != nil {
		return nil, fmt.Errorf("The image could not be rendered.")
	}
	mime := imageMIME(format)
	if len(data) > a.Settings.McpExportInlineMaxBytes {
		url, err := a.storeImage(data, mime, time.Now())
		if err != nil {
			return nil, fmt.Errorf("The image could not be stored.")
		}
		return bulkImageURL{MIME: mime, URL: url, Bytes: len(data),
			ExpiresInSeconds: a.Settings.McpExportURLTTLSeconds}, nil
	}
	return bulkImageInline{MIME: mime, DataBase64: base64.StdEncoding.EncodeToString(data),
		Bytes: len(data)}, nil
}
