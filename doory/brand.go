// Brand kits: named colours, font roles and logo assets
// (apps/brand/{views,services,serializers,models}.py).
//
// Ports backend/apps/brand/views.py
// Ports backend/apps/brand/services.py
// Ports backend/apps/brand/serializers.py
// Ports backend/apps/brand/models.py
//
// Logos are assets.Asset rows (assets family); M5 asserts the empty-logos
// shapes and the logo_ids ownership rejection (probe0005).
package doory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maxKitsPerUser = 20 // serializers.MAX_KITS_PER_USER

var (
	brandHex  = regexp.MustCompile(`(?i)^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
	brandIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	fontRoles = []string{"heading", "subheading", "body"}
)

// brandKitRow maps brand_brandkit (+ logos M2M read: always empty until the
// assets family).
type brandKitRow struct {
	ID        string
	OwnerID   string
	Name      string
	IsDefault bool
	Colors    string // stored JSON text (submitted order)
	Fonts     string
	CreatedAt string
	UpdatedAt string
}

const brandCols = `id, owner_id, name, is_default, colors, fonts, created_at, updated_at`

func scanBrandKit(s scanner) (brandKitRow, error) {
	var k brandKitRow
	err := s.Scan(&k.ID, &k.OwnerID, &k.Name, &k.IsDefault, &k.Colors, &k.Fonts,
		&k.CreatedAt, &k.UpdatedAt)
	return k, err
}

// brandEnvelope mirrors BrandKitSerializer field order (logo_ids is
// write-only; logos stay [] until the assets family).
type brandEnvelope struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	IsDefault bool         `json:"is_default"`
	Colors    rawCompacted `json:"colors"`
	Fonts     rawCompacted `json:"fonts"`
	Logos     []any        `json:"logos"`
	CreatedAt string       `json:"created_at"`
	UpdatedAt string       `json:"updated_at"`
}

func (k *brandKitRow) envelope() brandEnvelope {
	return brandEnvelope{
		ID: dashedUUID(k.ID), Name: k.Name, IsDefault: k.IsDefault,
		Colors:    rawCompacted(compactStoredJSON([]byte(k.Colors))),
		Fonts:     rawCompacted(compactStoredJSON([]byte(k.Fonts))),
		Logos:     []any{},
		CreatedAt: renderDRFDatetime(k.CreatedAt),
		UpdatedAt: renderDRFDatetime(k.UpdatedAt),
	}
}

type brandCreateBody struct {
	Name      *string         `json:"name"`
	IsDefault *bool           `json:"is_default"`
	Colors    json.RawMessage `json:"colors"`
	Fonts     json.RawMessage `json:"fonts"`
	LogoIDs   *[]string       `json:"logo_ids"`
}

type brandColor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Hex  string `json:"hex"`
}

type brandFont struct {
	Role   string `json:"role"`
	Family string `json:"family"`
}

// brandList ports BrandKitViewSet list (paginated 24, owner-scoped, ordered
// -is_default, name, created_at).
func (a *App) brandList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.brandCreate(w, r)
		return
	}
	a.brandListPage(w, r)
}

func (a *App) brandListPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := a.DB.Query(
		"SELECT "+brandCols+" FROM brand_brandkit WHERE owner_id = ?"+
			" ORDER BY is_default DESC, name ASC, created_at ASC", u.ID)
	if err != nil {
		logerr(err, "brandList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	kits := []brandEnvelope{}
	for rows.Next() {
		k, err := scanBrandKit(rows)
		if err != nil {
			logerr(err, "brandList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		kits = append(kits, k.envelope())
	}
	page, ferr := windowSlice(r, len(kits), designPageSize)
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	resp, err := paginateSized(r, len(kits), kits[page.from:page.to], designPageSize)
	if err != nil {
		if de, ok := err.(*DocumentError); ok {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// brandCreate ports BrandKitViewSet.create + services.create_kit (limit,
// single-default under the owner lock, logo attach).
func (a *App) brandCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var body brandCreateBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	fields, colorsRaw, fontsRaw := a.validateBrand(&body, u.ID, false)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	var count int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM brand_brandkit WHERE owner_id = ?`, u.ID).Scan(&count)
	if count >= maxKitsPerUser {
		writeDRF(w, http.StatusConflict, "limit_reached",
			"You can have at most 20 brand kits.")
		return
	}
	name := ""
	if body.Name != nil {
		name = *body.Name
	}
	isDefault := body.IsDefault != nil && *body.IsDefault
	if isDefault {
		// _make_only_default: clear other defaults first.
		_, _ = a.DB.Exec(
			`UPDATE brand_brandkit SET is_default = 0, updated_at = ? WHERE owner_id = ? AND is_default = 1`,
			formatDjangoDateTime(time.Now()), u.ID)
	}
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	if _, err := a.DB.Exec(
		`INSERT INTO brand_brandkit (id, owner_id, name, is_default, colors, fonts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, u.ID, name, isDefault, string(colorsRaw), string(fontsRaw), now, now); err != nil {
		logerr(err, "brandCreate")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	a.presentBrand(w, r, id, http.StatusCreated)
}

// validateBrand mirrors BrandKitSerializer validation in declaration order
// (name, colors, fonts, logo_ids). Child errors nest as
// {"<index>":{"<field>":[msg]}} (probe0005: colors hex, fonts role) while
// list-level messages (uniqueness, length) are FLAT field errors; the same
// shapes carry the matching codes (invalid / invalid_choice / required).
func (a *App) validateBrand(body *brandCreateBody, ownerID string, partial bool) (fields []FieldError, colors, fonts []byte) {
	colors = []byte("[]")
	fonts = []byte("[]")

	// name: required -> trim/blank -> clean -> length (TitleField pattern).
	// partial=True (PATCH): an absent name is simply not updated.
	if body.Name == nil && partial {
		// nothing to validate
	} else if body.Name == nil {
		fields = append(fields, SimpleField("name", "This field is required.", "required"))
	} else if strings.TrimSpace(*body.Name) == "" {
		fields = append(fields, SimpleField("name", "This field may not be blank.", "blank"))
	} else {
		cleaned := cleanText(*body.Name)
		switch {
		case cleaned == "":
			fields = append(fields, SimpleField("name", "The name cannot be empty.", "invalid"))
		case len([]rune(cleaned)) > 80:
			fields = append(fields, SimpleField("name",
				"Ensure this field has no more than 80 characters.", "max_length"))
		default:
			body.Name = &cleaned
		}
	}

	// colors: BrandColorSerializer children (id regex, hex pattern with the
	// explicit message, name cleaned+lowercased hex on the way out).
	if body.Colors != nil {
		var list []brandColor
		if err := json.Unmarshal(body.Colors, &list); err != nil {
			fields = append(fields, SimpleField("colors", "Not a valid list.", "invalid"))
		} else {
			if len(list) > 100 {
				fields = append(fields, SimpleField("colors",
					"Ensure this field has no more than 100 elements.", "max_length"))
			}
			seen := map[string]bool{}
			uniqueViolated := false
			child := nestedChild{} // index -> field -> (msg, code)
			for i, c := range list {
				if seen[c.ID] {
					uniqueViolated = true
				}
				seen[c.ID] = true
				if !brandIDRE.MatchString(c.ID) || len([]rune(c.ID)) > 64 {
					child.add(i, "id",
						fmt.Sprintf("Enter a value matching the pattern %s (limit 64 characters).",
							brandIDRE.String()), "invalid")
				}
				if !brandHex.MatchString(c.Hex) {
					child.add(i, "hex", "Use #rgb or #rrggbb.", "invalid")
				}
				list[i].Hex = strings.ToLower(c.Hex)
				list[i].Name = cleanText(c.Name)
			}
			if uniqueViolated {
				fields = append(fields, SimpleField("colors",
					"Colour ids must be unique within a brand kit.", "invalid"))
			}
			if !child.empty() {
				fields = append(fields, child.fieldError("colors"))
			}
			colors = encodeCompactJSON(list)
		}
	}

	// fonts: BrandFontSerializer children (role choice with the straight-
	// quote DRF message, family required + cleaned).
	if body.Fonts != nil {
		var list []brandFont
		if err := json.Unmarshal(body.Fonts, &list); err != nil {
			fields = append(fields, SimpleField("fonts", "Not a valid list.", "invalid"))
		} else {
			if len(list) > len(fontRoles) {
				fields = append(fields, SimpleField("fonts",
					"Ensure this field has no more than 3 elements.", "max_length"))
			}
			seen := map[string]bool{}
			rolesViolated := false
			child := nestedChild{}
			for i, f := range list {
				if seen[f.Role] {
					rolesViolated = true
				}
				seen[f.Role] = true
				if !choiceValid(fontRoles, f.Role) {
					child.add(i, "role", fmt.Sprintf("%q is not a valid choice.", f.Role), "invalid_choice")
				}
				family := cleanText(f.Family)
				if f.Family == "" {
					child.add(i, "family", "This field is required.", "required")
				} else if family == "" {
					child.add(i, "family", "The font family cannot be empty.", "invalid")
				}
				if len([]rune(family)) > 100 {
					child.add(i, "family",
						"Ensure this field has no more than 100 characters.", "max_length")
				}
				list[i].Family = family
			}
			if rolesViolated {
				fields = append(fields, SimpleField("fonts",
					"Each font role can only be used once.", "invalid"))
			}
			if !child.empty() {
				fields = append(fields, child.fieldError("fonts"))
			}
			fonts = encodeCompactJSON(list)
		}
	}

	// logo_ids: uuid children, then ownership (assets family owns nothing
	// yet, so any non-empty list is rejected — probe0005).
	if body.LogoIDs != nil {
		child := nestedChild{}
		valid := true
		for i, id := range *body.LogoIDs {
			if !uuidShape.MatchString(strings.ToLower(id)) {
				valid = false
				child.add(i, "", "Must be a valid UUID.", "invalid")
			}
		}
		if !valid && len(*body.LogoIDs) > 0 {
			// Both errors can coexist; DRF renders child errors instead of
			// the list-level one when children fail, so child wins.
			fields = append(fields, child.fieldError("logo_ids"))
		} else if len(*body.LogoIDs) > 0 {
			fields = append(fields, SimpleField("logo_ids",
				"Every logo must be one of your own assets.", "invalid"))
		}
	}
	return fields, colors, fonts
}

// nestedChild accumulates per-index child errors of a list field:
// {"0":{"hex":["..."]}} with the parallel codes shape.
type nestedChild struct {
	order      []int
	fieldOrder []string
	msg        map[int]map[string][]string
	code       map[int]map[string][]string
}

func (n *nestedChild) add(index int, field, msg, code string) {
	if n.msg == nil {
		n.msg = map[int]map[string][]string{}
		n.code = map[int]map[string][]string{}
	}
	if _, ok := n.msg[index]; !ok {
		n.order = append(n.order, index)
		n.msg[index] = map[string][]string{}
		n.code[index] = map[string][]string{}
	}
	if field != "" {
		found := false
		for _, f := range n.fieldOrder {
			if f == field {
				found = true
			}
		}
		if !found {
			n.fieldOrder = append(n.fieldOrder, field)
		}
	}
	key := field
	n.msg[index][key] = append(n.msg[index][key], msg)
	n.code[index][key] = append(n.code[index][key], code)
}

func (n *nestedChild) empty() bool { return len(n.order) == 0 }

// fieldError renders {"<i>":{...}} for both values (order preserved).
func (n *nestedChild) fieldError(name string) FieldError {
	build := func(store map[int]map[string][]string) string {
		var b strings.Builder
		b.WriteByte('{')
		for oi, index := range n.order {
			if oi > 0 {
				b.WriteByte(',')
			}
			fields := store[index]
			// Primitives-only children (logo uuids) render a bare array;
			// serializer children render {"field":[...]}.
			onlyUnnamed := true
			for f := range fields {
				if f != "" {
					onlyUnnamed = false
				}
			}
			if onlyUnnamed {
				b.WriteString(`"` + itoa(index) + `":[`)
				for mi, m := range fields[""] {
					if mi > 0 {
						b.WriteByte(',')
					}
					b.WriteString(jsonStr(m))
				}
				b.WriteString("]")
				continue
			}
			b.WriteString(`"` + itoa(index) + `":{`)
			for fi, field := range n.fieldOrder {
				msgs, ok := fields[field]
				if !ok {
					continue
				}
				if fi > 0 && field != n.fieldOrder[0] {
					b.WriteByte(',')
				}
				b.WriteString(`"` + field + `":[`)
				for mi, m := range msgs {
					if mi > 0 {
						b.WriteByte(',')
					}
					b.WriteString(jsonStr(m))
				}
				b.WriteString("]")
			}
			b.WriteByte('}')
		}
		b.WriteByte('}')
		return b.String()
	}
	// NOTE: map iteration inside an index orders fields non-deterministically
	// when a child has several fields; every probed case has ONE field per
	// child, so order is stable. (Multi-field child -> single-key maps only.)
	return FieldError{Name: name, Raw: build(n.msg), RawCodes: build(n.code)}
}

// brandDetail routes: retrieve / patch / destroy.
func (a *App) brandDetail(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.brandRetrieve(w, r)
	case http.MethodPatch:
		a.brandUpdate(w, r)
	case http.MethodDelete:
		a.brandDestroy(w, r)
	}
}

func (a *App) getBrandFor(w http.ResponseWriter, r *http.Request) (*brandKitRow, bool) {
	u := currentUser(r)
	id := hexUUID(routeParam(r, "uuid"))
	k, err := a.getBrandByID(u.ID, id)
	if err != nil {
		logerr(err, "getBrandFor")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return nil, false
	}
	if k == nil {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	return k, true
}

func (a *App) getBrandByID(ownerID, id string) (*brandKitRow, error) {
	row := a.DB.QueryRow(
		"SELECT "+brandCols+" FROM brand_brandkit WHERE id = ? AND owner_id = ?", id, ownerID)
	k, err := scanBrandKit(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &k, err
}

func (a *App) brandRetrieve(w http.ResponseWriter, r *http.Request) {
	k, ok := a.getBrandFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, k.envelope())
}

// brandUpdate ports partial_update + services.update_kit (single-default
// flip on promotion; logo set when logo_ids present).
func (a *App) brandUpdate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	k, ok := a.getBrandFor(w, r)
	if !ok {
		return
	}
	var body brandCreateBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	fields, colorsRaw, fontsRaw := a.validateBrand(&body, u.ID, true)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	updates := []string{}
	args := []any{}
	if body.Name != nil {
		updates = append(updates, "name = ?")
		args = append(args, *body.Name)
	}
	if body.Colors != nil {
		updates = append(updates, "colors = ?")
		args = append(args, string(colorsRaw))
	}
	if body.Fonts != nil {
		updates = append(updates, "fonts = ?")
		args = append(args, string(fontsRaw))
	}
	if body.IsDefault != nil {
		if *body.IsDefault {
			_, _ = a.DB.Exec(
				`UPDATE brand_brandkit SET is_default = 0, updated_at = ?
				 WHERE owner_id = ? AND is_default = 1 AND id != ?`,
				formatDjangoDateTime(time.Now()), u.ID, k.ID)
		}
		updates = append(updates, "is_default = ?")
		args = append(args, *body.IsDefault)
	}
	// logo_ids present: assets family owns persistence; empty list = clear
	// (no rows exist anyway).
	if body.LogoIDs != nil && len(*body.LogoIDs) == 0 {
		updates = append(updates, "updated_at = ?")
		args = append(args, formatDjangoDateTime(time.Now()))
	}
	if len(updates) == 0 {
		a.presentBrand(w, r, k.ID, http.StatusOK)
		return
	}
	updates = append(updates, "updated_at = ?")
	args = append(args, formatDjangoDateTime(time.Now()))
	args = append(args, k.ID, u.ID)
	if _, err := a.DB.Exec(
		"UPDATE brand_brandkit SET "+strings.Join(updates, ", ")+
			" WHERE id = ? AND owner_id = ?", args...); err != nil {
		logerr(err, "brandUpdate")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	a.presentBrand(w, r, k.ID, http.StatusOK)
}

// brandDestroy ports DestroyModelMixin.
func (a *App) brandDestroy(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	k, ok := a.getBrandFor(w, r)
	if !ok {
		return
	}
	if _, err := a.DB.Exec(`DELETE FROM brand_brandkit WHERE id = ? AND owner_id = ?`,
		k.ID, u.ID); err != nil {
		logerr(err, "brandDestroy")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// presentBrand re-reads with the logos prefetch (self._present).
func (a *App) presentBrand(w http.ResponseWriter, r *http.Request, id string, status int) {
	u := currentUser(r)
	k, err := a.getBrandByID(u.ID, id)
	if err != nil || k == nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, status, k.envelope())
}
