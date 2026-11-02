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
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
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
	enabled := a.configuredPhotoProviders()
	providers := make([]any, 0, len(enabled))
	names := make([]string, 0, len(enabled))
	for _, name := range enabled {
		info := photoProviderInfos[name]
		providers = append(providers, map[string]string{"name": info.Name, "label": info.Label,
			"attribution": info.Attribution, "url": info.URL})
		names = append(names, name)
	}
	all := []string{}
	if len(names) > 1 {
		all = names
	}
	def := ""
	if len(all) > 0 {
		def = "all"
	} else if len(names) == 1 {
		def = names[0]
	}
	writeJSON(w, http.StatusOK, stockProvidersEnvelope{Providers: providers, All: all, Default: def})
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
	page := atoi(q.Get("page"))
	if page == 0 {
		page = 1
	}
	perPage := atoi(q.Get("per_page"))
	if perPage == 0 {
		perPage = stockDefaultPerPage
	}
	payload, derr := a.searchStockPhotos(requested, q.Get("q"), page, perPage, q.Get("cursor"))
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	if configured, _ := payload["configured"].(bool); !configured {
		writeJSON(w, http.StatusOK, stockNotConfigured{Configured: false,
			Provider: stringFromAny(payload["provider"]), Results: []any{}})
		return
	}
	writeJSON(w, http.StatusOK, payload)
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
	asset, created, derr := a.importStockPhoto(currentUser(r), body.Provider, body.ID)
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, asset.envelope())
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
	payload, derr := a.searchFreesound(q.Get("q"), q.Get("type"), q.Get("subcategory"),
		q.Get("length"), q.Get("sort"), atoi(q.Get("page")), atoi(q.Get("per_page")))
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	if configured, _ := payload["configured"].(bool); !configured {
		writeJSON(w, http.StatusOK, audioNotConfigured{Results: []any{}})
		return
	}
	writeJSON(w, http.StatusOK, payload)
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
	if !a.freesoundConfigured() {
		writeJSON(w, http.StatusOK, map[string]bool{"configured": false})
		return
	}
	types := map[string][]string{}
	for name, info := range freesoundTypes {
		types[name] = info.Subcategories
	}
	lengths := make([]string, 0, len(freesoundLengths))
	for name := range freesoundLengths {
		lengths = append(lengths, name)
	}
	sorts := make([]string, 0, len(freesoundSorts))
	for name := range freesoundSorts {
		sorts = append(sorts, name)
	}
	slicesSort(lengths)
	slicesSort(sorts)
	writeJSON(w, http.StatusOK, struct {
		Configured bool                `json:"configured"`
		Provider   string              `json:"provider"`
		Label      string              `json:"label"`
		URL        string              `json:"url"`
		Licenses   []string            `json:"licenses"`
		Types      map[string][]string `json:"types"`
		Lengths    []string            `json:"lengths"`
		Sorts      []string            `json:"sorts"`
	}{true, "freesound", "Freesound", "https://freesound.org/", a.freesoundAllowedLicenses(), types,
		lengths, sorts})
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
	if !a.freesoundConfigured() {
		writeDRF(w, http.StatusBadRequest, "stock_not_configured", "Freesound is not configured on this server.")
		return
	}
	if !freesoundID.MatchString(body.ID) {
		writeDRF(w, http.StatusBadRequest, "invalid", "Not a valid Freesound id.")
		return
	}
	sound, err := a.freesoundResolve(body.ID)
	if err != nil {
		if strings.Contains(err.Error(), "licence") {
			writeDRF(w, http.StatusBadRequest, "invalid", err.Error())
		} else {
			derr := stockErrorDocument(err, "Freesound could not be reached. Try again shortly.")
			writeDRF(w, derr.Status, derr.Code, derr.Message)
		}
		return
	}
	if existing := a.findAudioBySame(currentUser(r), sound.ID); existing != nil {
		writeJSON(w, http.StatusOK, existing.envelope())
		return
	}
	data, derr2 := freesoundDownload(a, sound.DownloadURL, map[string]bool{"cdn.freesound.org": true}, maxUploadBytes)
	if derr2 != nil {
		if _, ok := derr2.(*remoteImageTooLarge); ok {
			writeDRF(w, http.StatusRequestEntityTooLarge, "file_too_large", "The file is too large.")
		} else {
			writeDRF(w, http.StatusServiceUnavailable, "stock_unavailable", "Freesound could not be reached. Try again shortly.")
		}
		return
	}
	prepared, perr := prepareFile(data)
	if perr != nil {
		writeDRF(w, perr.Status, perr.Code, perr.Message)
		return
	}
	meta := map[string]any{"provider": "freesound", "sound_id": sound.ID, "credit": sound.credit(),
		"attribution": map[string]any{"provider": "freesound", "author": sound.Author,
			"author_url": sound.AuthorURL, "source_url": sound.SourceURL}}
	asset, derr3 := a.storePreparedAsset(currentUser(r), prepared, cleanAssetName(sound.Title, "Sound"), nil, meta)
	if derr3 != nil {
		writeDRF(w, derr3.Status, derr3.Code, derr3.Message)
		return
	}
	writeJSON(w, http.StatusCreated, audioImportEnvelope(asset, sound.credit()))
}

// stockAudioPreview ports AudioStockPreviewView: unconfigured -> DRF
// NotFound default (NOT the custom message — audio_stock.py raises
// NotFound()).
func audioImportEnvelope(asset *assetRow, credit map[string]any) map[string]any {
	data, _ := json.Marshal(asset.envelope())
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	out["credit"] = credit
	return out
}

func (a *App) stockAudioPreview(w http.ResponseWriter, r *http.Request) {
	if !a.freesoundConfigured() {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	data, derr := a.freesoundPreview(routeParam(r, "sound_id"))
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---- stock videos (0.2.0, apps/assets/video_stock.py) ------------------------------------------
//
// The availability and limits are real (ffmpeg/ffprobe on PATH, VIDEO_ENABLED);
// configured providers search, resolve and import through video_stock.go.

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

func slicesSort(values []string) { sort.Strings(values) }

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
	page := atoi(q.Get("page"))
	if page == 0 {
		page = 1
	}
	perPage := atoi(q.Get("per_page"))
	if perPage == 0 {
		perPage = stockDefaultPerPage
	}
	payload, derr := a.searchVideos(requested, q.Get("q"), page, perPage, q.Get("cursor"),
		q.Get("orientation"), q.Get("duration"))
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	if configured, _ := payload["configured"].(bool); !configured {
		writeJSON(w, http.StatusOK, videoStockPayload{
			Configured: false, Provider: stringFromAny(payload["provider"]), Results: []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, payload)
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

// stockVideoImport ports StockVideoImportView: serializer bounds, then the
// provider resolve/download/probe/conversion pipeline.
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
	asset, created, derr := a.importStockVideo(currentUser(r), body.Provider, body.ID)
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if asset.Status == "processing" {
			status = http.StatusAccepted
		}
	}
	writeJSON(w, status, asset.envelope())
}
