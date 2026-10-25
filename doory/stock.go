// Stock media endpoints under the NO-CONFIGURATION world (no provider
// keys): the shapes an installation ships with before an operator adds
// Unsplash/Pexels/Pixabay/Freesound keys.
//
// Probe lineage: probes/0007-assets steps01-04 (every stock route needs a
// session -> 401) + the derived no-config payloads (stock.py
// _not_configured, audio_stock info/search, StockNotConfigured messages).
// LIVE provider paths (search results, import downloads, previews) need the
// remote-fetch/SSRF slice (family pending) with fake-provider fixtures.
//
// Ports backend/apps/assets/stock.py
// Ports backend/apps/assets/audio_stock.py
// Ports backend/apps/assets/providers.py (configured-only paths)
package doory

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
)

const stockDefaultPerPage = 24 // stock.py DEFAULT_PER_PAGE

// freesound.TYPES/LENGTHS/SORTS vocabularies (audio_stock.py choices) —
// validated from source; empty here means the bare GET path (probed).
func audioSearchTypes() []string {
	return []string{"all", "sound_effect", "music", "voice", "ambience"}
}
func audioSearchLengths() []string { return []string{"any", "short", "medium", "long"} }
func audioSearchSorts() []string {
	return []string{"relevance", "rating_total", "created_desc", "duration_asc", "duration_desc"}
}

var stockProviders = []string{"unsplash", "pexels", "pixabay"}

// stockNotConfigured is stock.py _not_configured: the payload an
// unconfigured server returns instead of an error.
type stockNotConfigured struct {
	Configured bool    `json:"configured"`
	Provider   string  `json:"provider"`
	Total      int64   `json:"total"`
	NextPage   *string `json:"next_page"`
	Results    []any   `json:"results"`
}

type stockPayload struct {
	Configured bool    `json:"configured"`
	Provider   string  `json:"provider"`
	Total      int64   `json:"total"`
	NextPage   *string `json:"next_page"`
	Results    []any   `json:"results"`
}

// stockProvidersView ports StockProvidersView: configured providers in tab
// order + credits + the default tab (no configuration -> empty lists and
// an empty default — derived from the probed source logic).
func (a *App) stockProvidersView(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, stockProvidersEnvelope{
		Providers: []any{},
		All:       []string{},
		Default:   "", // STOCK_DEFAULT_TAB "all" with no configured providers
	})
}

type stockProvidersEnvelope struct {
	Providers []any    `json:"providers"`
	All       []string `json:"all"`
	Default   string   `json:"default"`
}

// stockPhotosSearch ports StockSearchView + search(): serializer bounds
// first, then the configured check (no providers -> _not_configured with
// the requested name, "all" handled first — stock.py order).
func (a *App) stockPhotosSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fields []FieldError

	if v := q.Get("q"); len([]rune(v)) > 100 {
		fields = append(fields, SimpleField("q",
			"Ensure this field has no more than 100 characters.", "max_length"))
	}
	if v, ok := q["page"]; ok {
		n := atoi(v[0])
		if n < 1 {
			fields = append(fields, SimpleField("page",
				"Ensure this value is greater than or equal to 1.", "min_value"))
		} else if n > 100 { // stock.py MAX_PAGE
			fields = append(fields, SimpleField("page",
				"Ensure this value is less than or equal to 100.", "max_value"))
		}
	}
	if v, ok := q["per_page"]; ok {
		n := atoi(v[0])
		if n < 1 {
			fields = append(fields, SimpleField("per_page",
				"Ensure this value is greater than or equal to 1.", "min_value"))
		} else if n > 30 { // providers.MAX_PER_PAGE
			fields = append(fields, SimpleField("per_page",
				"Ensure this value is less than or equal to 30.", "max_value"))
		}
	}
	if v, ok := q["provider"]; ok && v[0] != "" {
		if v[0] != "all" && !choiceValid(stockProviders, v[0]) {
			fields = append(fields, SimpleField("provider",
				fmt.Sprintf("%q is not a valid choice.", v[0]), "invalid_choice"))
		}
	}
	if v, ok := q["lang"]; ok && v[0] != "" && !validLang(v[0]) {
		fields = append(fields, FieldError{Name: "lang",
			Messages: []string{fmt.Sprintf(
				"Enter a value matching the pattern ^[A-Za-z]{2}([-_][A-Za-z0-9]{1,8})*$ (limit 35 characters).")},
			Codes: []string{"invalid"}})
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	requested := q.Get("provider")
	// search(): provider "all" resolves the ALL tab first; nobody is
	// configured, so both branches answer _not_configured.
	if requested == "all" {
		writeJSON(w, http.StatusOK, stockNotConfigured{
			Configured: false, Provider: "all", Results: []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, stockNotConfigured{
		Configured: false, Provider: requested, Results: []any{},
	})
}

// validLang mirrors StockSearchSerializer's lang pattern.
func validLang(v string) bool {
	if len([]rune(v)) > 35 || len(v) < 2 {
		return false
	}
	for i, r := range v {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			// letters anywhere
		case i >= 2 && (r == '-' || r == '_'):
			// separators after the 2-letter prefix
		case i >= 2 && r >= '0' && r <= '9':
			// alphanumerics after the prefix (1-8 allowed — bounded by length)
		default:
			return false
		}
	}
	return true
}

// stockPhotoImport ports StockImportView -> import_photo: with no
// configured provider the DEFAULT StockNotConfigured answers (probe-derived
// message; no network is ever touched).
func (a *App) stockPhotoImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	if !choiceValid(stockProviders, body.Provider) {
		fields = append(fields, SimpleField("provider",
			fmt.Sprintf("%q is not a valid choice.", body.Provider), "invalid_choice"))
	}
	if body.ID == "" {
		fields = append(fields, SimpleField("id", "This field is required.", "required"))
	} else if len([]rune(body.ID)) > 64 {
		fields = append(fields, SimpleField("id",
			"Ensure this field has no more than 64 characters.", "max_length"))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	// import_photo: providers.PROVIDERS[provider].configured == false.
	writeDRF(w, http.StatusBadRequest, "stock_not_configured",
		"No stock photo provider is configured on this server.")
}

// stockAudioSearch ports AudioStockSearchView.search(): no Freesound ->
// the not-configured payload (audio_stock.py line83).
func (a *App) stockAudioSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fields []FieldError
	if v := q.Get("q"); len([]rune(v)) > 100 { // AudioSearchSerializer q max100
		fields = append(fields, SimpleField("q",
			"Ensure this field has no more than 100 characters.", "max_length"))
	}
	if v := q.Get("subcategory"); len([]rune(v)) > 60 {
		fields = append(fields, SimpleField("subcategory",
			"Ensure this field has no more than 60 characters.", "max_length"))
	}
	if v, ok := q["type"]; ok && v[0] != "" && !choiceValid(audioSearchTypes(), v[0]) {
		fields = append(fields, SimpleField("type",
			fmt.Sprintf("%q is not a valid choice.", v[0]), "invalid_choice"))
	}
	if v, ok := q["length"]; ok && v[0] != "" && !choiceValid(audioSearchLengths(), v[0]) {
		fields = append(fields, SimpleField("length",
			fmt.Sprintf("%q is not a valid choice.", v[0]), "invalid_choice"))
	}
	if v, ok := q["sort"]; ok && v[0] != "" && !choiceValid(audioSearchSorts(), v[0]) {
		fields = append(fields, SimpleField("sort",
			fmt.Sprintf("%q is not a valid choice.", v[0]), "invalid_choice"))
	}
	if v, ok := q["page"]; ok {
		if n := atoi(v[0]); n < 1 {
			fields = append(fields, SimpleField("page",
				"Ensure this value is greater than or equal to 1.", "min_value"))
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, audioNotConfigured{Results: []any{}})
}

type audioNotConfigured struct {
	Configured bool    `json:"configured"`
	Total      int64   `json:"total"`
	NextPage   *string `json:"next_page"`
	Results    []any   `json:"results"`
}

// stockAudioInfo ports AudioStockInfoView: info() without Freesound ->
// {"configured": false}.
func (a *App) stockAudioInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"configured": false})
}

// stockAudioImport ports AudioStockImportView: _sound() raises the
// Freesound-specific StockNotConfigured BEFORE any id validation? — the
// serializer validates `id` first (required), then _sound checks config.
func (a *App) stockAudioImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	if body.ID == "" {
		writeDRFValidation(w, []FieldError{SimpleField("id", "This field is required.", "required")})
		return
	}
	writeDRF(w, http.StatusBadRequest, "stock_not_configured",
		"Freesound is not configured on this server.")
}

// stockAudioPreview ports AudioStockPreviewView: unconfigured -> DRF
// NotFound default (NOT the custom message — audio_stock.py raises
// NotFound()).
func (a *App) stockAudioPreview(w http.ResponseWriter, r *http.Request) {
	writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
}

// ---- stock videos (0.2.0, apps/assets/video_stock.py) ------------------------------------------
//
// Like the photo endpoints, only the no-configuration behaviour is ported:
// the provider calls (search, resolve, download) and the ffmpeg conversion
// pipeline are pending with the video family. The availability and limits
// here are real (ffmpeg/ffprobe on PATH, VIDEO_ENABLED), so a server with
// the tools reports itself ready exactly like the oracle.

// videoProviders mirrors video_providers.VIDEO_PROVIDERS registration order.
var videoProviders = []string{"pexels_video", "pixabay_video"}

// videoOrientations mirrors video_providers.ORIENTATIONS (sorted).
var videoOrientations = []string{"landscape", "portrait", "square"}

// videoDurations mirrors video_providers.DURATIONS keys (insertion order).
var videoDurations = []string{"short", "medium", "long"}

// videoAvailable ports video.available(): video is on and both commands exist.
func (a *App) videoAvailable() bool {
	if !a.Settings.VideoEnabled {
		return false
	}
	if _, err := exec.LookPath(a.Settings.FFmpegPath); err != nil {
		return false
	}
	if _, err := exec.LookPath(a.Settings.FFprobePath); err != nil {
		return false
	}
	return true
}

// videoProviderKeys mirrors each provider's _key(): its own video key,
// falling back to the photo key of the same provider.
func (s *Settings) videoProviderKeys() map[string]string {
	return map[string]string{
		"pexels_video":  firstNonEmpty(s.PexelsVideoAPIKey, s.PexelsAccessKey),
		"pixabay_video": firstNonEmpty(s.PixabayVideoAPIKey, s.PixabayAccessKey),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// enabledVideoProviders ports video_stock.enabled(): the clip providers to
// offer, in STOCK_VIDEO_PROVIDERS order; none when this server can't
// convert clips.
func (a *App) enabledVideoProviders() []string {
	if !a.videoAvailable() {
		return nil
	}
	keys := a.Settings.videoProviderKeys()
	enabled := map[string]bool{}
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range a.Settings.StockVideoProviders {
		name := strings.ToLower(strings.TrimSpace(raw))
		if seen[name] || !choiceValid(videoProviders, name) {
			continue
		}
		seen[name] = true
		if keys[name] != "" {
			enabled[name] = true
			out = append(out, name)
		}
	}
	return out
}

// videoStockPayload is stock.py's _not_configured() payload (the same shape
// the photo search answers).
type videoStockPayload struct {
	Configured bool    `json:"configured"`
	Provider   string  `json:"provider"`
	Total      int64   `json:"total"`
	NextPage   *string `json:"next_page"`
	Results    []any   `json:"results"`
}

// stockVideosSearch ports StockVideoSearchView + video_stock.search() for a
// server without configured clip providers: serializer bounds first, then
// the not-configured payload (the requested name, "all" handled first).
func (a *App) stockVideosSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fields []FieldError

	// VideoSearchSerializer (declaration order: q, page, per_page, provider,
	// cursor, lang, orientation, duration).
	if v := q.Get("q"); len([]rune(v)) > 100 {
		fields = append(fields, SimpleField("q",
			"Ensure this field has no more than 100 characters.", "max_length"))
	}
	if v, ok := q["page"]; ok {
		n := atoi(v[0])
		if n < 1 {
			fields = append(fields, SimpleField("page",
				"Ensure this value is greater than or equal to 1.", "min_value"))
		} else if n > 100 { // stock.py MAX_PAGE
			fields = append(fields, SimpleField("page",
				"Ensure this value is less than or equal to 100.", "max_value"))
		}
	}
	if v, ok := q["per_page"]; ok {
		n := atoi(v[0])
		if n < 1 {
			fields = append(fields, SimpleField("per_page",
				"Ensure this value is greater than or equal to 1.", "min_value"))
		} else if n > 30 { // providers.MAX_PER_PAGE
			fields = append(fields, SimpleField("per_page",
				"Ensure this value is less than or equal to 30.", "max_value"))
		}
	}
	if v, ok := q["provider"]; ok && v[0] != "" {
		if v[0] != "all" && !choiceValid(videoProviders, v[0]) {
			fields = append(fields, SimpleField("provider",
				fmt.Sprintf("%q is not a valid choice.", v[0]), "invalid_choice"))
		}
	}
	if v, ok := q["cursor"]; ok && len([]rune(v[0])) > 300 {
		fields = append(fields, SimpleField("cursor",
			"Ensure this field has no more than 300 characters.", "max_length"))
	}
	if v, ok := q["lang"]; ok && v[0] != "" && !validLang(v[0]) {
		fields = append(fields, FieldError{Name: "lang",
			Messages: []string{fmt.Sprintf(
				"Enter a value matching the pattern ^[A-Za-z]{2}([-_][A-Za-z0-9]{1,8})*$ (limit 35 characters).")},
			Codes: []string{"invalid"}})
	}
	if v, ok := q["orientation"]; ok && v[0] != "" && !choiceValid(videoOrientations, v[0]) {
		fields = append(fields, SimpleField("orientation",
			fmt.Sprintf("%q is not a valid choice.", v[0]), "invalid_choice"))
	}
	if v, ok := q["duration"]; ok && v[0] != "" && !choiceValid(videoDurations, v[0]) {
		fields = append(fields, SimpleField("duration",
			fmt.Sprintf("%q is not a valid choice.", v[0]), "invalid_choice"))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	requested := q.Get("provider")
	// video_stock.search(): "all" (or no provider with several enabled)
	// resolves the All tab first; nobody is configured -> _not_configured
	// with "all" or the requested name ("" when none was asked for).
	if requested == "all" {
		writeJSON(w, http.StatusOK, videoStockPayload{
			Configured: false, Provider: "all", Results: []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, videoStockPayload{
		Configured: false, Provider: requested, Results: []any{},
	})
}

// videoProviderCredit is one entry of the providers payload.
type videoProviderCredit struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Attribution string `json:"attribution"`
	URL         string `json:"url"`
}

// videoProviderInfo mirrors the provider class attributes the payload reads.
var videoProviderInfo = map[string]videoProviderCredit{
	"pexels_video": {
		Name: "pexels_video", Label: "Pexels",
		Attribution: "Videos provided by Pexels", URL: "https://www.pexels.com/",
	},
	"pixabay_video": {
		Name: "pixabay_video", Label: "Pixabay",
		Attribution: "Videos from Pixabay", URL: "https://pixabay.com/",
	},
}

// stockVideosProviders ports StockVideoProvidersView: whether this server
// takes clips at all, the clip providers in tab order with their credit, and
// the upload/duration limits.
func (a *App) stockVideosProviders(w http.ResponseWriter, r *http.Request) {
	enabled := a.enabledVideoProviders()
	names := make([]string, 0, len(enabled))
	credits := make([]videoProviderCredit, 0, len(enabled))
	for _, name := range enabled {
		names = append(names, name)
		credits = append(credits, videoProviderInfo[name])
	}
	all := []string{}
	if len(names) > 1 {
		all = names
	}
	defaultTab := ""
	if len(names) > 1 {
		defaultTab = "all"
	} else if len(names) == 1 {
		defaultTab = names[0]
	}
	writeJSON(w, http.StatusOK, struct {
		Enabled        bool                  `json:"enabled"`
		MaxUploadBytes int                   `json:"max_upload_bytes"`
		MaxDurationMS  int                   `json:"max_duration_ms"`
		Providers      []videoProviderCredit `json:"providers"`
		All            []string              `json:"all"`
		Default        string                `json:"default"`
	}{
		Enabled:        a.videoAvailable(),
		MaxUploadBytes: a.Settings.MaxVideoUploadBytes,
		MaxDurationMS:  a.Settings.MaxVideoDurationSeconds * 1000,
		Providers:      credits,
		All:            all,
		Default:        defaultTab,
	})
}

// stockVideoImport ports StockVideoImportView for servers that cannot take
// clips at all: the serializer first (provider choice + id bounds), then
// video_stock.import_video's availability check. Live imports (download +
// ffmpeg conversion) are pending with the video family.
func (a *App) stockVideoImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	if !choiceValid(videoProviders, body.Provider) {
		fields = append(fields, SimpleField("provider",
			fmt.Sprintf("%q is not a valid choice.", body.Provider), "invalid_choice"))
	}
	if body.ID == "" {
		fields = append(fields, SimpleField("id", "This field is required.", "required"))
	} else if len([]rune(body.ID)) > 64 {
		fields = append(fields, SimpleField("id",
			"Ensure this field has no more than 64 characters.", "max_length"))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	if !a.videoAvailable() {
		writeDRF(w, http.StatusBadRequest, "video_unavailable",
			"This server can't take videos (ffmpeg is not installed).")
		return
	}
	// video.available() but the provider search/download/conversion pipeline
	// is not ported yet: say so instead of pretending nothing happened.
	writeDRF(w, http.StatusInternalServerError, "server_error",
		"route registered but not ported: video_stock.import_video (assets/video*.py pending)")
}
