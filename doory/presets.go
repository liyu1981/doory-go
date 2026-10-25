// Public size-preset catalogue (apps/presets/views.py): the DB rows the
// 0002 seed migration inserted (copied verbatim by pnpm seedgen), nested in
// groups. JSON columns (tags/aliases/meta) pass through compacted-but-
// order-preserving — the oracle round-trips them through Python dicts with
// the same key order (probe0004 step15).
//
// width_px/height_px are model properties (validators.to_px: whole CSS px at
// 96 dpi, ROUND_HALF_UP — implemented exactly with big.Rat so halves never
// flip against Python's Decimal); they are NOT columns.
//
// Ports backend/apps/presets/views.py
// Ports backend/apps/presets/validators.py (to_px, UNITS)
package doory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// formatVersion mirrors presets/importer.FORMAT_VERSION (probed: 1).
const formatVersion = 1

// unitsPerInch mirrors validators.UNITS_PER_INCH (PX_PER_INCH = 96).
var unitsPerInch = map[string]*big.Rat{
	"px": big.NewRat(96, 1), // PX_PER_INCH: value * 96 / 96 (identity)
	"in": big.NewRat(1, 1),
	"cm": big.NewRat(254, 100),
	"mm": big.NewRat(254, 10),
}

// toPx ports validators.to_px: value * 96 / unit, whole pixels, rounded
// half away from zero (exact rational arithmetic == Python Decimal at the
// precision these catalogue values need).
func toPx(value any, unit string) int64 {
	r := anyToRat(value)
	if r == nil {
		return 0
	}
	den, ok := unitsPerInch[unit]
	if !ok || den == nil {
		return 0
	}
	px := new(big.Rat).Quo(new(big.Rat).Mul(r, big.NewRat(96, 1)), den)
	// ROUND_HALF_UP (away from zero): floor(px + 1/2) for positives.
	half := big.NewRat(1, 2)
	rounded := new(big.Rat).Add(px, half)
	return new(big.Int).Quo(rounded.Num(), rounded.Denom()).Int64()
}

// anyToRat converts an int64/float64/[]byte driver value to an exact rational.
func anyToRat(v any) *big.Rat {
	switch x := v.(type) {
	case int64:
		return big.NewRat(x, 1)
	case float64:
		r, ok := new(big.Rat).SetString(strconv.FormatFloat(x, 'g', -1, 64))
		if !ok {
			return nil
		}
		return r
	case []byte:
		r, ok := new(big.Rat).SetString(string(x))
		if !ok {
			return nil
		}
		return r
	case string:
		r, ok := new(big.Rat).SetString(x)
		if !ok {
			return nil
		}
		return r
	}
	return nil
}

// numJSON renders a stored number like the oracle's _number: integral values
// as integers (no .0), fractions as their shortest exact decimal form.
func numJSON(v any) any {
	switch x := v.(type) {
	case int64:
		return jsonNumber(itoa(int(x)))
	case float64:
		if x == float64(int64(x)) && x < 1e15 {
			return jsonNumber(itoa(int(x)))
		}
		return jsonNumber(strconv.FormatFloat(x, 'f', -1, 64))
	case []byte:
		return numJSON(string(x))
	case string:
		if s := x; s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return numJSON(f)
			}
		}
		return jsonNumber("0")
	}
	return jsonNumber("0")
}

func jsonNumber(s string) json.Number { return json.Number(s) }

// presetsResponse keeps key order (version, groups) — maps would randomize.
type presetsResponse struct {
	Version int          `json:"version"`
	Groups  []presetNode `json:"groups"`
}

// presetNode is one group: children + presets (insertion order preserved).
type presetNode struct {
	Slug     string       `json:"slug"`
	Label    string       `json:"label"`
	Icon     string       `json:"icon"`
	Children []presetNode `json:"children"`
	Presets  []any        `json:"presets"`
}

// orderedPreset mirrors the view's _preset dict (order: slug, label, width,
// height, unit, width_px, height_px, icon, tags, is_quick, aliases, meta);
// JSON columns are rawCompacted passthrough.
type orderedPreset struct {
	Slug     string       `json:"slug"`
	Label    string       `json:"label"`
	Width    any          `json:"width"`
	Height   any          `json:"height"`
	Unit     string       `json:"unit"`
	WidthPx  int64        `json:"width_px"`
	HeightPx int64        `json:"height_px"`
	Icon     string       `json:"icon"`
	Tags     rawCompacted `json:"tags"`
	IsQuick  bool         `json:"is_quick"`
	Aliases  rawCompacted `json:"aliases"`
	Meta     rawCompacted `json:"meta"`
}

type storedGroup struct {
	id, parent, slug, label, icon string
}

type storedPreset struct {
	slug, label, unit, icon string
	width, height           any
	tags, aliases, meta     string
	isQuick                 bool
	groupID                 string
}

// sizePresets ports the size_presets api_view (GET only, public,
// cache_control public max-age=60, auth classes [] => no Vary: Cookie).
func (a *App) sizePresets(w http.ResponseWriter, r *http.Request) {
	// @cache_control(public=True, max_age=60) — set BEFORE the middleware
	// stack so PrivateApiCache's setdefault leaves it (probe0004 step30).
	w.Header().Set("Cache-Control", "public, max-age=60")
	grows, err := a.DB.Query(`SELECT id, parent_id, slug, label, icon
		FROM presets_sizepresetgroup WHERE is_active = TRUE
		ORDER BY sort_order, label, slug`)
	if err != nil {
		logerr(err, "sizePresets.groups")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	groups := []storedGroup{}
	for grows.Next() {
		var g storedGroup
		var parent sql.NullString
		if err := grows.Scan(&g.id, &parent, &g.slug, &g.label, &g.icon); err != nil {
			grows.Close()
			logerr(err, "sizePresets.groups.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		g.parent = parent.String
		groups = append(groups, g)
	}
	grows.Close()

	prows, err := a.DB.Query(`SELECT slug, label, width, height, unit, icon,
		tags, aliases, meta, is_quick, group_id
		FROM presets_sizepreset WHERE is_active = TRUE ORDER BY sort_order, label, slug`)
	if err != nil {
		logerr(err, "sizePresets.presets")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	presets := []storedPreset{}
	for prows.Next() {
		var p storedPreset
		if err := prows.Scan(&p.slug, &p.label, &p.width, &p.height, &p.unit, &p.icon,
			&p.tags, &p.aliases, &p.meta, &p.isQuick, &p.groupID); err != nil {
			prows.Close()
			logerr(err, "sizePresets.presets.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		presets = append(presets, p)
	}
	prows.Close()

	byGroup := map[string][]storedPreset{}
	for _, p := range presets {
		byGroup[p.groupID] = append(byGroup[p.groupID], p)
	}
	children := map[string][]storedGroup{}
	for _, grp := range groups {
		children[grp.parent] = append(children[grp.parent], grp)
	}

	var build func(list []storedGroup) []presetNode
	build = func(list []storedGroup) []presetNode {
		nodes := []presetNode{}
		for _, grp := range list {
			node := presetNode{
				Slug: grp.slug, Label: grp.label, Icon: grp.icon,
				Children: build(children[grp.id]),
				Presets:  []any{},
			}
			for _, p := range byGroup[grp.id] {
				node.Presets = append(node.Presets, orderedPreset{
					Slug: p.slug, Label: p.label,
					Width: numJSON(p.width), Height: numJSON(p.height),
					Unit:    p.unit,
					WidthPx: toPx(p.width, p.unit), HeightPx: toPx(p.height, p.unit),
					Icon:    p.icon,
					Tags:    rawCompacted(compactStoredJSON([]byte(p.tags))),
					IsQuick: p.isQuick,
					Aliases: rawCompacted(compactStoredJSON([]byte(p.aliases))),
					Meta:    rawCompacted(compactStoredJSON([]byte(p.meta))),
				})
			}
			if len(node.Children) > 0 || len(node.Presets) > 0 {
				nodes = append(nodes, node)
			}
		}
		return nodes
	}

	writeJSON(w, http.StatusOK, presetsResponse{Version: formatVersion, Groups: build(children[""])})
}

// presetLookup mirrors the parts preset_parts needs.
type presetLookup struct {
	slug      string
	widthPx   int
	heightPx  int
	groupSlug string
	platforms []string
}

// presetRowFor finds a preset by slug or alias among ACTIVE presets of
// ACTIVE groups (filenames.preset_parts source query).
func (a *App) presetRowFor(slug string) *presetLookup {
	rows, err := a.DB.Query(`
		SELECT p.slug, p.width, p.height, p.meta, g.slug
		FROM presets_sizepreset p JOIN presets_sizepresetgroup g ON g.id = p.group_id
		WHERE p.is_active = TRUE AND g.is_active = TRUE`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var p presetLookup
		var width, height, meta, groupSlug string
		if err := rows.Scan(&p.slug, &width, &height, &meta, &groupSlug); err != nil {
			return nil
		}
		p.groupSlug = groupSlug
		matches := p.slug == slug
		var aliasList []string
		var metaObj map[string]any
		_ = json.Unmarshal([]byte(meta), &metaObj)
		if raw, ok := metaObj["platforms"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					aliasList = append(aliasList, s)
				}
			}
		}
		if !matches {
			// aliases column holds the alternative names
			var aliases []string
			_ = json.Unmarshal(readAliases(a, p.slug), &aliases)
			for _, alias := range aliases {
				if alias == slug {
					matches = true
				}
			}
		}
		if matches {
			w := numFromString(width)
			h := numFromString(height)
			p.widthPx = w
			p.heightPx = h
			p.platforms = aliasList
			return &p
		}
	}
	return nil
}

func readAliases(a *App, presetSlug string) []byte {
	var raw string
	_ = a.DB.QueryRow(`SELECT aliases FROM presets_sizepreset WHERE slug = ?`, presetSlug).Scan(&raw)
	return []byte(raw)
}

func numFromString(s string) int {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(f + 0.5)
}

// ---- the signed-in person's size choices (0.2.0) ------------------------------------------------

// MAX_PINNED/MAX_COLLAPSED/MAX_RECENT and RECENT_DESIGNS mirror
// apps/presets/views.py.
const (
	maxPinnedSizes       = 24
	maxCollapsedSections = 20
	maxRecentSizes       = 8
	recentDesigns        = 50
	maxPresetSlugLen     = 64 // designs.validators.MAX_PRESET_LENGTH
)

// mySizes ports views.my_sizes (GET/PATCH /size-presets/mine/): the sizes
// the signed-in person pinned, used recently, and the picker sections they
// folded. `pinned` is null until they pin or unpin something.
func (a *App) mySizes(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)

	var pinnedRaw sql.NullString
	collapsedRaw := ""
	hasRow := true
	err := a.DB.QueryRow(`SELECT pinned, collapsed FROM presets_sizechoices WHERE user_id = ?`,
		u.ID).Scan(&pinnedRaw, &collapsedRaw)
	if err == sql.ErrNoRows {
		hasRow = false
		collapsedRaw = ""
	} else if err != nil {
		logerr(err, "mySizes.select")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}

	if r.Method == http.MethodPatch {
		// request.data: a JSON object, or {} for anything else (a list body
		// is not a dict and changes nothing).
		body, perr := readJSONObjectBody(r)
		if perr != nil {
			writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
			return
		}
		var (
			pinnedSet    bool
			pinnedNull   bool
			pinned       []string
			collapsedSet bool
			collapsed    []string
		)
		if raw, ok := body["pinned"]; ok {
			pinnedSet = true
			if string(raw) == "null" {
				pinnedNull = true
			} else {
				values, ok := jsonStringList(raw)
				if !ok || !allMatch(values, presetSlugPattern) {
					writeDRFValidation(w, []FieldError{SimpleField("pinned",
						"`pinned` must be a list of slugs.", "invalid")})
					return
				}
				pinned = uniqueStrings(values)
				if len(pinned) > maxPinnedSizes {
					writeDRFValidation(w, []FieldError{SimpleField("pinned",
						"`pinned` can hold at most 24 items.", "invalid")})
					return
				}
			}
		}
		if raw, ok := body["collapsed"]; ok {
			collapsedSet = true
			values, ok := jsonStringList(raw)
			if !ok || !allMatch(values, groupSlugPattern) {
				writeDRFValidation(w, []FieldError{SimpleField("collapsed",
					"`collapsed` must be a list of slugs.", "invalid")})
				return
			}
			collapsed = uniqueStrings(values)
			if len(collapsed) > maxCollapsedSections {
				writeDRFValidation(w, []FieldError{SimpleField("collapsed",
					"`collapsed` can hold at most 20 items.", "invalid")})
				return
			}
		}
		now := formatDjangoDateTime(timeNowUTC())
		switch {
		case !hasRow:
			// update_or_create: a row exists after any PATCH, with the
			// model defaults for the keys it doesn't set.
			var pinnedJSON any
			if pinnedSet && !pinnedNull {
				pinnedJSON = mustMarshalString(pinned)
			}
			if !collapsedSet {
				collapsed = []string{}
			}
			if _, err := a.DB.Exec(`INSERT INTO presets_sizechoices
				(id, user_id, pinned, collapsed, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
				newUUID4Hex(), u.ID, pinnedJSON, mustMarshalString(collapsed), now, now); err != nil {
				logerr(err, "mySizes.insert")
				writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
				return
			}
			hasRow = true
			if pinnedSet && !pinnedNull {
				pinnedRaw = sql.NullString{String: mustMarshalString(pinned), Valid: true}
			}
			collapsedRaw = mustMarshalString(collapsed)
		case pinnedSet || collapsedSet:
			if pinnedSet {
				if pinnedNull {
					pinnedRaw = sql.NullString{}
				} else {
					pinnedRaw = sql.NullString{String: mustMarshalString(pinned), Valid: true}
				}
			}
			if collapsedSet {
				collapsedRaw = mustMarshalString(collapsed)
			}
			var pinnedValue any
			if pinnedRaw.Valid {
				pinnedValue = pinnedRaw.String
			}
			if _, err := a.DB.Exec(`UPDATE presets_sizechoices SET pinned = ?, collapsed = ?, updated_at = ?
				WHERE user_id = ?`, pinnedValue, collapsedRaw, now, u.ID); err != nil {
				logerr(err, "mySizes.update")
				writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
				return
			}
		}
	}

	// Response: {pinned, recent, collapsed} in that order.
	var pinnedOut any
	if pinnedRaw.Valid {
		pinnedOut = json.RawMessage(compactStoredJSON([]byte(pinnedRaw.String)))
	}
	collapsedOut := json.RawMessage("[]")
	if strings.TrimSpace(collapsedRaw) != "" {
		collapsedOut = json.RawMessage(compactStoredJSON([]byte(collapsedRaw)))
	}
	recent, rerr := a.recentSizes(u.ID)
	if rerr != nil {
		logerr(rerr, "mySizes.recent")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	payload := struct {
		Pinned    any      `json:"pinned"`
		Recent    []string `json:"recent"`
		Collapsed any      `json:"collapsed"`
	}{Pinned: pinnedOut, Recent: recent, Collapsed: collapsedOut}
	writeJSON(w, http.StatusOK, payload)
}

// recentSizes ports views._recent: the sizes of the person's latest designs,
// newest first, each once (at most 8, read from the latest 50).
func (a *App) recentSizes(ownerID string) ([]string, error) {
	rows, err := a.DB.Query(`SELECT preset FROM designs_design
		WHERE owner_id = ? AND trashed_at IS NULL AND preset <> ''
		ORDER BY created_at DESC LIMIT ?`, ownerID, recentDesigns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	seen := map[string]bool{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
		if len(out) == maxRecentSizes {
			break
		}
	}
	return out, rows.Err()
}

// Preset/group slug rules live in presets_import.go (PRESET_SLUG/GROUP_SLUG).

func allMatch(values []string, pattern *regexp.Regexp) bool {
	for _, value := range values {
		if !pattern.MatchString(value) {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// readJSONObjectBody decodes a JSON body into its raw object members; a
// non-object body (list, scalar) yields an empty map, like DRF's
// `isinstance(request.data, dict)` guard in my_sizes.
func readJSONObjectBody(r *http.Request) (map[string]json.RawMessage, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxJSONBody {
		return nil, errors.New("too large")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	if trimmed[0] != '{' {
		var anyValue any
		if err := json.Unmarshal(trimmed, &anyValue); err != nil {
			return nil, err
		}
		return map[string]json.RawMessage{}, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &members); err != nil {
		return nil, err
	}
	return members, nil
}

// jsonStringList reports the members of a JSON array of strings.
func jsonStringList(raw json.RawMessage) ([]string, bool) {
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}
