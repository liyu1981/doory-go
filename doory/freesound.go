// Freesound: search sounds, information, previews and imports. The licence
// is decided here, never by the client; only CC0 (and optionally BY) sounds
// are accepted.
package doory

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var freesoundAPIBase = "https://freesound.org/apiv2"

type freesoundLicense struct{ Code, Name, URL string }

type freesoundSound struct {
	ID, Title, Author, AuthorURL, SourceURL string
	DurationMS                              int
	Type, Subcategory                       string
	License                                 freesoundLicense
	Tags                                    []string
	PreviewURL, DownloadURL                 string
}

type freesoundPage struct {
	Total    int              `json:"total"`
	NextPage *string          `json:"next_page"`
	Results  []freesoundSound `json:"results"`
}

var (
	cc0License    = regexp.MustCompile(`^https?://creativecommons\.org/publicdomain/zero/1\.0/?$`)
	byLicense     = regexp.MustCompile(`^https?://creativecommons\.org/licenses/by/(\d\.\d)/?$`)
	freesoundID   = regexp.MustCompile(`^[0-9]{1,12}$`)
	freesoundUser = regexp.MustCompile(`^[\w.@+-]{1,100}$`)
)

var freesoundTypes = map[string]struct {
	Category      string
	Subcategories []string
}{
	"music": {"Music", []string{"Solo instrument", "Solo percussion", "Multiple instruments", "Other"}},
	"effects": {"Sound effects", []string{"Objects / House appliances", "Human sounds and actions",
		"Natural elements and explosions", "Animals", "Vehicles", "Other mechanisms, engines, machines",
		"Electronic / Design", "Experimental", "Other"}},
	"ambience": {"Soundscapes", []string{"Nature", "Urban", "Indoors", "Synthetic / Artificial", "Other"}},
}
var freesoundLengths = map[string][2]float64{"any": {0, 900}, "short": {0, 5},
	"medium": {5, 30}, "long": {30, 120}, "extended": {120, 900}}
var freesoundSorts = map[string]string{"relevance": "score", "rating": "rating_desc",
	"downloads": "downloads_desc", "newest": "created_desc", "shortest": "duration_asc"}

type freesoundCacheEntry struct {
	page freesoundPage
	at   time.Time
}

var (
	freesoundCacheMu sync.Mutex
	freesoundCache   = map[string]freesoundCacheEntry{}
)

func (a *App) freesoundConfigured() bool {
	return a.Settings != nil && strings.TrimSpace(a.Settings.FreesoundAccessKey) != ""
}

func (a *App) freesoundAllowedLicenses() []string {
	if a.Settings == nil {
		return []string{"cc0"}
	}
	var out []string
	for _, raw := range a.Settings.FreesoundLicenses {
		name := strings.ToLower(strings.TrimSpace(raw))
		if (name == "cc0" || name == "by") && !stringInList(out, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		out = []string{"cc0"}
	}
	return out
}

func freesoundLicenseOf(raw string) (freesoundLicense, bool) {
	if cc0License.MatchString(raw) {
		return freesoundLicense{"cc0", "CC0 1.0", "https://creativecommons.org/publicdomain/zero/1.0/"}, true
	}
	if m := byLicense.FindStringSubmatch(raw); m != nil {
		return freesoundLicense{"by", "CC BY " + m[1], "https://creativecommons.org/licenses/by/" + m[1] + "/"}, true
	}
	return freesoundLicense{}, false
}

func (a *App) freesoundFilter(kind, subcategory, length string) string {
	var licenses []string
	for _, name := range a.freesoundAllowedLicenses() {
		if name == "cc0" {
			licenses = append(licenses, `"Creative Commons 0"`)
		}
		if name == "by" {
			licenses = append(licenses, `"Attribution"`)
		}
	}
	part := "license:" + licenses[0]
	if len(licenses) > 1 {
		part = "license:(" + strings.Join(licenses, " OR ") + ")"
	}
	parts := []string{part}
	if info, ok := freesoundTypes[kind]; ok {
		parts = append(parts, "category:"+freesoundQuote(info.Category))
		for _, sub := range info.Subcategories {
			if sub == subcategory {
				parts = append(parts, "subcategory:"+freesoundQuote(sub))
				break
			}
		}
	}
	band := freesoundLengths[length]
	if band == [2]float64{} {
		band = freesoundLengths["any"]
	}
	parts = append(parts, fmt.Sprintf("duration:[%g TO %g]", band[0], band[1]))
	return strings.Join(parts, " ")
}

func freesoundQuote(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, ""), `"`, "") + `"`
}

func freesoundSoundFrom(item map[string]any) freesoundSound {
	id := intFromAny(item["id"])
	license, ok := freesoundLicenseOf(stringFromAny(item["license"]))
	if id == 0 || !ok {
		return freesoundSound{}
	}
	previews, _ := item["previews"].(map[string]any)
	hosts := map[string]bool{"cdn.freesound.org": true}
	preview := checkPhotoHost(stringFromAny(previews["preview-lq-mp3"]), hosts)
	if preview == "" {
		preview = checkPhotoHost(stringFromAny(previews["preview-hq-mp3"]), hosts)
	}
	download := checkPhotoHost(stringFromAny(previews["preview-hq-ogg"]), hosts)
	if download == "" {
		download = checkPhotoHost(stringFromAny(previews["preview-hq-mp3"]), hosts)
	}
	duration, _ := item["duration"].(float64)
	if preview == "" || download == "" || duration <= 0 || duration > 900 {
		return freesoundSound{}
	}
	author := stringFromAny(item["username"])
	authorURL := ""
	if freesoundUser.MatchString(author) {
		authorURL = "https://freesound.org/people/" + author + "/"
	}
	tags := []string{}
	if rawTags, ok := item["tags"].([]any); ok {
		for _, raw := range rawTags {
			if tag, ok := raw.(string); ok {
				if len([]rune(tag)) > 40 {
					tag = string([]rune(tag)[:40])
				}
				tags = append(tags, tag)
			}
			if len(tags) >= 12 {
				break
			}
		}
	}
	typ := "other"
	for name, info := range freesoundTypes {
		if info.Category == stringFromAny(item["category"]) {
			typ = name
		}
	}
	title := strings.TrimSpace(stringFromAny(item["name"]))
	if title == "" {
		title = fmt.Sprintf("Freesound %d", id)
	}
	return freesoundSound{ID: fmt.Sprint(id), Title: title, Author: author, AuthorURL: authorURL,
		SourceURL: httpsPhotoLink(item["url"]), DurationMS: int(duration * 1000), Type: typ,
		Subcategory: stringFromAny(item["subcategory"]), License: license, Tags: tags,
		PreviewURL: preview, DownloadURL: download}
}

func (a *App) freesoundAPI(path string, params map[string]string) (map[string]any, error) {
	values := url.Values{}
	for key, value := range params {
		values.Set(key, value)
	}
	return a.stockJSON("freesound", freesoundAPIBase+path+"?"+values.Encode(), map[string]string{
		"Authorization": "Token " + a.Settings.FreesoundAccessKey})
}

func (a *App) freesoundSearch(query, kind, subcategory, length, sort string, page, perPage int) (freesoundPage, error) {
	if perPage > 30 {
		perPage = 30
	}
	params := map[string]string{"query": truncateRunes(query, 100), "filter": a.freesoundFilter(kind, subcategory, length),
		"sort": freesoundSorts[sort], "page": fmt.Sprint(page), "page_size": fmt.Sprint(perPage),
		"fields": "id,name,username,license,duration,previews,category,subcategory,url,tags"}
	data, err := a.freesoundAPI("/search/text/", params)
	if err != nil {
		return freesoundPage{}, err
	}
	rawResults, _ := data["results"].([]any)
	sounds := []freesoundSound{}
	for _, raw := range rawResults {
		item, _ := raw.(map[string]any)
		sound := freesoundSoundFrom(item)
		if sound.ID != "" && stringInList(a.freesoundAllowedLicenses(), sound.License.Code) {
			sounds = append(sounds, sound)
		}
	}
	total := intFromAny(data["count"])
	var next *string
	if data["next"] != nil && total > page*perPage {
		value := fmt.Sprint(page + 1)
		next = &value
	}
	return freesoundPage{Total: total, NextPage: next, Results: sounds}, nil
}

func (a *App) searchFreesound(query, kind, subcategory, length, sort string, page, perPage int) (map[string]any, *DocumentError) {
	if !a.freesoundConfigured() {
		return map[string]any{"configured": false, "total": 0, "next_page": nil, "results": []any{}}, nil
	}
	key := strings.Join([]string{query, kind, subcategory, length, sort, fmt.Sprint(page, perPage)}, "|")
	freesoundCacheMu.Lock()
	cached, ok := freesoundCache[key]
	freesoundCacheMu.Unlock()
	if ok && time.Since(cached.at) < time.Hour {
		return freesoundPagePayload(cached.page), nil
	}
	found, err := a.freesoundSearch(query, kind, subcategory, length, sort, page, perPage)
	if err != nil {
		return nil, stockErrorDocument(err, "Freesound could not be reached. Try again shortly.")
	}
	freesoundCacheMu.Lock()
	freesoundCache[key] = freesoundCacheEntry{found, time.Now()}
	freesoundCacheMu.Unlock()
	return freesoundPagePayload(found), nil
}

func (s freesoundSound) asDict() map[string]any {
	return map[string]any{"id": s.ID, "provider": "freesound", "title": s.Title,
		"author": s.Author, "author_url": s.AuthorURL, "source_url": s.SourceURL,
		"duration_ms": s.DurationMS, "type": s.Type, "subcategory": s.Subcategory,
		"license": s.License.Name, "license_url": s.License.URL,
		"credit_required": s.License.Code != "cc0", "tags": s.Tags}
}

func (s freesoundSound) credit() map[string]any {
	return map[string]any{"source": "freesound", "id": s.ID, "title": s.Title,
		"author": s.Author, "author_url": s.AuthorURL, "url": s.SourceURL,
		"license": s.License.Name, "license_url": s.License.URL,
		"required": s.License.Code != "cc0"}
}

func freesoundPagePayload(found freesoundPage) map[string]any {
	results := make([]any, len(found.Results))
	for i, sound := range found.Results {
		results[i] = sound.asDict()
	}
	return map[string]any{"configured": true, "provider": "freesound", "total": found.Total,
		"next_page": found.NextPage, "results": results}
}

func (a *App) freesoundResolve(id string) (freesoundSound, error) {
	data, err := a.freesoundAPI("/sounds/"+url.PathEscape(id)+"/", map[string]string{
		"fields": "id,name,username,license,duration,previews,category,subcategory,url,tags"})
	if err != nil {
		return freesoundSound{}, err
	}
	sound := freesoundSoundFrom(data)
	if sound.ID != id {
		return freesoundSound{}, fmt.Errorf("unexpected provider response")
	}
	if !stringInList(a.freesoundAllowedLicenses(), sound.License.Code) {
		return freesoundSound{}, fmt.Errorf("This sound's licence is not allowed here.")
	}
	return sound, nil
}

var freesoundDownload = func(a *App, rawURL string, hosts map[string]bool, maxBytes int) ([]byte, error) {
	return a.downloadStockImage(rawURL, hosts, maxBytes)
}

func (a *App) findAudioBySame(owner *UserRow, id string) *assetRow {
	rows, err := a.DB.Query(`SELECT `+assetCols+` FROM assets_asset WHERE owner_id = ? AND kind = 'audio' ORDER BY created_at DESC, id ASC`, owner.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			continue
		}
		var meta map[string]any
		if jsonUnmarshalMap(asset.Meta, &meta) && stringFromAny(meta["sound_id"]) == id {
			return &asset
		}
	}
	return nil
}

func (a *App) freesoundPreview(id string) ([]byte, *DocumentError) {
	if !a.freesoundConfigured() {
		return nil, &DocumentError{Code: "not_found", Status: 404, Message: "Not found."}
	}
	if !freesoundID.MatchString(id) {
		return nil, &DocumentError{Code: "invalid", Status: 400, Message: "Not a valid Freesound id."}
	}
	sound, err := a.freesoundResolve(id)
	if err != nil {
		return nil, stockErrorDocument(err, "Freesound could not be reached. Try again shortly.")
	}
	data, derr := freesoundDownload(a, sound.PreviewURL, map[string]bool{"cdn.freesound.org": true}, 12*1024*1024)
	if derr != nil {
		return nil, &DocumentError{Code: "stock_unavailable", Status: 503,
			Message: "Freesound could not be reached. Try again shortly."}
	}
	return data, nil
}
