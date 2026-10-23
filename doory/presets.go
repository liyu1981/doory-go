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
	"encoding/json"
	"math/big"
	"net/http"
	"strconv"
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
	grows, err := a.DB.Query(`SELECT id, COALESCE(parent_id,''), slug, label, icon
		FROM presets_sizepresetgroup WHERE is_active = 1
		ORDER BY sort_order, label, slug`)
	if err != nil {
		logerr(err, "sizePresets.groups")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	groups := []storedGroup{}
	for grows.Next() {
		var g storedGroup
		if err := grows.Scan(&g.id, &g.parent, &g.slug, &g.label, &g.icon); err != nil {
			grows.Close()
			logerr(err, "sizePresets.groups.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		groups = append(groups, g)
	}
	grows.Close()

	prows, err := a.DB.Query(`SELECT slug, label, width, height, unit, icon,
		tags, aliases, meta, is_quick, group_id
		FROM presets_sizepreset WHERE is_active = 1 ORDER BY sort_order, label, slug`)
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
		WHERE p.is_active = 1 AND g.is_active = 1`)
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
