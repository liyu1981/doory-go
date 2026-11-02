// Stock-photo providers (Unsplash, Pexels, Pixabay): search, resolve and
// download. The provider API is the only source of a download URL; every hop
// must stay on its allow-listed image CDN over https.
package doory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	unsplashAPIBase = "https://api.unsplash.com"
	pexelsAPIBase   = "https://api.pexels.com/v1"
	pixabayAPIBase  = "https://pixabay.com/api/"
)

type stockPhoto struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	ThumbURL   string `json:"thumb_url"`
	PreviewURL string `json:"preview_url"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Author     string `json:"author"`
	AuthorURL  string `json:"author_url"`
	SourceURL  string `json:"source_url"`
}

type stockPhotoPage struct {
	Total    int          `json:"total"`
	NextPage *string      `json:"next_page"`
	Results  []stockPhoto `json:"results"`
	Provider string       `json:"provider"`
}

type resolvedPhoto struct {
	Photo       stockPhoto
	DownloadURL string
	TrackURL    string
}

type photoCacheEntry struct {
	payload stockPhotoPage
	at      time.Time
}

var (
	photoCacheMu sync.Mutex
	photoCache   = map[string]photoCacheEntry{}
)

type photoProviderInfo struct {
	Name, Label, Attribution, URL string
	CacheSeconds                  int
}

var photoProviderInfos = map[string]photoProviderInfo{
	"unsplash": {Name: "unsplash", Label: "Unsplash", Attribution: "Photos from Unsplash",
		URL: "https://unsplash.com/", CacheSeconds: 3600},
	"pexels": {Name: "pexels", Label: "Pexels", Attribution: "Photos provided by Pexels",
		URL: "https://www.pexels.com/", CacheSeconds: 3600},
	"pixabay": {Name: "pixabay", Label: "Pixabay", Attribution: "Images from Pixabay",
		URL: "https://pixabay.com/", CacheSeconds: 86400},
}

var photoProviderOrder = []string{"unsplash", "pexels", "pixabay"}

func (a *App) photoProviderKey(name string) string {
	if a.Settings == nil {
		return ""
	}
	switch name {
	case "unsplash":
		return a.Settings.UnsplashAccessKey
	case "pexels":
		return a.Settings.PexelsAccessKey
	case "pixabay":
		return a.Settings.PixabayAccessKey
	}
	return ""
}

func (a *App) photoProviderConfigured(name string) bool {
	for _, candidate := range a.Settings.StockProviders {
		if strings.EqualFold(strings.TrimSpace(candidate), name) {
			return a.photoProviderKey(name) != ""
		}
	}
	return false
}

func (a *App) configuredPhotoProviders() []string {
	var out []string
	for _, name := range a.Settings.StockProviders {
		name = strings.ToLower(strings.TrimSpace(name))
		if _, ok := photoProviderInfos[name]; ok && a.photoProviderKey(name) != "" && !stringInList(out, name) {
			out = append(out, name)
		}
	}
	return out
}

type stockHTTPError struct {
	status     int
	retryAfter float64
}

func (e *stockHTTPError) Error() string { return fmt.Sprintf("provider answered %d", e.status) }

type stockBusyError struct{ wait float64 }

func (e *stockBusyError) Error() string { return "The provider is busy." }

func (a *App) stockJSON(name, rawURL string, headers map[string]string) (map[string]any, error) {
	if wait := stockAcquire(name); wait > 0 {
		return nil, &stockBusyError{wait}
	}
	data, err := stockHTTPJSON(rawURL, headers)
	if err == nil {
		return data, nil
	}
	if he, ok := err.(*stockHTTPError); ok && he.status == 429 {
		stockPause(name, he.retryAfter)
		return nil, &stockBusyError{he.retryAfter}
	}
	return nil, err
}

func stockHTTPJSON(rawURL string, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Doory-StockProxy/1.0")
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		retry := 60.0
		if value, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && value > 0 {
			retry = value
		}
		return nil, &stockHTTPError{resp.StatusCode, retry}
	}
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

func stockURL(base string, params map[string]string) string {
	values := url.Values{}
	for key, value := range params {
		values.Set(key, value)
	}
	return base + "?" + values.Encode()
}

func checkPhotoHost(raw string, hosts map[string]bool) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" ||
		(parsed.Port() != "" && parsed.Port() != "443") || !hosts[parsed.Hostname()] {
		return ""
	}
	return raw
}

func httpsPhotoLink(raw any) string {
	text, _ := raw.(string)
	if strings.HasPrefix(text, "https://") {
		return text
	}
	return ""
}

func withUTM(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := parsed.Query()
	q.Set("utm_source", "doory")
	q.Set("utm_medium", "referral")
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func (a *App) unsplashSearch(query string, page, perPage int) (stockPhotoPage, error) {
	headers := map[string]string{"Authorization": "Client-ID " + a.photoProviderKey("unsplash"), "Accept-Version": "v1"}
	params := map[string]string{"page": fmt.Sprint(page), "per_page": fmt.Sprint(perPage)}
	endpoint := stockURL(unsplashAPIBase+"/photos", params)
	filtered := false
	if query != "" {
		params["query"] = query
		params["content_filter"] = "high"
		endpoint = stockURL(unsplashAPIBase+"/search/photos", params)
		filtered = true
	}
	data, err := a.stockJSON("unsplash", endpoint, headers)
	if err != nil {
		return stockPhotoPage{}, &videoProviderError{"unavailable"}
	}
	var items []any
	if filtered {
		items, _ = data["results"].([]any)
	} else {
		items, _ = data["photos"].([]any)
	}
	var results []stockPhoto
	for _, raw := range items {
		if photo := unsplashPhoto(raw, "unsplash"); photo.ID != "" {
			results = append(results, photo)
		}
	}
	total := intFromAny(data["total"])
	if !filtered {
		total = (page-1)*perPage + len(items)
	}
	var next *string
	if total > page*perPage {
		value := fmt.Sprint(page + 1)
		next = &value
	}
	return stockPhotoPage{Total: total, NextPage: next, Results: results, Provider: "unsplash"}, nil
}

func unsplashPhoto(raw any, provider string) stockPhoto {
	item, _ := raw.(map[string]any)
	id := stringFromAny(item["id"])
	urls, _ := item["urls"].(map[string]any)
	user, _ := item["user"].(map[string]any)
	links, _ := item["links"].(map[string]any)
	userLinks, _ := user["links"].(map[string]any)
	hosts := map[string]bool{"images.unsplash.com": true}
	thumb := checkPhotoHost(stringFromAny(urls["small"]), hosts)
	preview := checkPhotoHost(stringFromAny(urls["regular"]), hosts)
	if id == "" || thumb == "" || preview == "" {
		return stockPhoto{}
	}
	authorURL := httpsPhotoLink(userLinks["html"])
	sourceURL := httpsPhotoLink(links["html"])
	if authorURL != "" {
		authorURL = withUTM(authorURL)
	}
	if sourceURL != "" {
		sourceURL = withUTM(sourceURL)
	}
	return stockPhoto{ID: id, Provider: provider, ThumbURL: thumb, PreviewURL: preview,
		Width: intFromAny(item["width"]), Height: intFromAny(item["height"]),
		Author: stringFromAny(user["name"]), AuthorURL: authorURL, SourceURL: sourceURL}
}

func (a *App) pexelsSearch(query string, page, perPage int) (stockPhotoPage, error) {
	params := map[string]string{"page": fmt.Sprint(page), "per_page": fmt.Sprint(perPage)}
	endpoint := stockURL(pexelsAPIBase+"/curated", params)
	if query != "" {
		params["query"] = query
		endpoint = stockURL(pexelsAPIBase+"/search", params)
	}
	data, err := a.stockJSON("pexels", endpoint, map[string]string{"Authorization": a.photoProviderKey("pexels")})
	if err != nil {
		return stockPhotoPage{}, &videoProviderError{"unavailable"}
	}
	items, _ := data["photos"].([]any)
	var results []stockPhoto
	for _, raw := range items {
		if photo := pexelsPhoto(raw, "pexels"); photo.ID != "" {
			results = append(results, photo)
		}
	}
	total := intFromAny(data["total_results"])
	if total == 0 {
		total = (page-1)*perPage + len(items)
	}
	var next *string
	if total > page*perPage {
		value := fmt.Sprint(page + 1)
		next = &value
	}
	return stockPhotoPage{Total: total, NextPage: next, Results: results, Provider: "pexels"}, nil
}

func pexelsPhoto(raw any, provider string) stockPhoto {
	item, _ := raw.(map[string]any)
	id := intFromAny(item["id"])
	src, _ := item["src"].(map[string]any)
	hosts := map[string]bool{"images.pexels.com": true}
	thumb := checkPhotoHost(stringFromAny(src["medium"]), hosts)
	preview := checkPhotoHost(stringFromAny(src["large"]), hosts)
	if id == 0 || thumb == "" || preview == "" {
		return stockPhoto{}
	}
	return stockPhoto{ID: fmt.Sprint(id), Provider: provider, ThumbURL: thumb, PreviewURL: preview,
		Width: intFromAny(item["width"]), Height: intFromAny(item["height"]),
		Author: stringFromAny(item["photographer"]), AuthorURL: httpsPhotoLink(item["photographer_url"]),
		SourceURL: httpsPhotoLink(item["url"])}
}

func (a *App) pixabaySearch(query string, page, perPage int) (stockPhotoPage, error) {
	asked := perPage
	if asked < 3 {
		asked = 3
	}
	params := map[string]string{"key": a.photoProviderKey("pixabay"), "page": fmt.Sprint(page),
		"per_page": fmt.Sprint(asked), "image_type": "photo", "safesearch": "true", "lang": "en"}
	if query != "" {
		params["q"] = query
	}
	data, err := a.stockJSON("pixabay", stockURL(pixabayAPIBase, params), nil)
	if err != nil {
		return stockPhotoPage{}, &videoProviderError{"unavailable"}
	}
	hits, _ := data["hits"].([]any)
	var results []stockPhoto
	for _, raw := range hits {
		if photo := pixabayPhoto(raw, "pixabay"); photo.ID != "" {
			results = append(results, photo)
		}
		if len(results) >= perPage {
			break
		}
	}
	total := intFromAny(data["totalHits"])
	if total > 500 {
		total = 500
	}
	var next *string
	if asked == perPage && total > page*perPage {
		value := fmt.Sprint(page + 1)
		next = &value
	}
	return stockPhotoPage{Total: total, NextPage: next, Results: results, Provider: "pixabay"}, nil
}

func pixabayPhoto(raw any, provider string) stockPhoto {
	item, _ := raw.(map[string]any)
	id := intFromAny(item["id"])
	web := stringFromAny(item["webformatURL"])
	hosts := map[string]bool{"cdn.pixabay.com": true, "pixabay.com": true}
	preview := checkPhotoHost(web, hosts)
	thumb := preview
	if strings.Contains(web, "_640.") {
		thumb = checkPhotoHost(strings.Replace(web, "_640.", "_340.", 1), hosts)
	}
	if thumb == "" {
		thumb = checkPhotoHost(stringFromAny(item["previewURL"]), hosts)
	}
	if id == 0 || thumb == "" || preview == "" {
		return stockPhoto{}
	}
	user := stringFromAny(item["user"])
	authorURL := ""
	if user != "" && intFromAny(item["user_id"]) > 0 {
		authorURL = "https://pixabay.com/users/" + url.PathEscape(user) + "-" + fmt.Sprint(intFromAny(item["user_id"])) + "/"
	}
	return stockPhoto{ID: fmt.Sprint(id), Provider: provider, ThumbURL: thumb, PreviewURL: preview,
		Width: intFromAny(item["imageWidth"]), Height: intFromAny(item["imageHeight"]),
		Author: user, AuthorURL: authorURL, SourceURL: httpsPhotoLink(item["pageURL"])}
}

func (a *App) searchPhotoProvider(name, query string, page, perPage int) (stockPhotoPage, error) {
	key := strings.Join([]string{name, query, fmt.Sprint(page, perPage)}, "|")
	info := photoProviderInfos[name]
	ttl := time.Duration(info.CacheSeconds) * time.Second
	photoCacheMu.Lock()
	cached, ok := photoCache[key]
	photoCacheMu.Unlock()
	if ok && time.Since(cached.at) < ttl {
		return cached.payload, nil
	}
	var found stockPhotoPage
	var err error
	switch name {
	case "unsplash":
		found, err = a.unsplashSearch(query, page, perPage)
	case "pexels":
		found, err = a.pexelsSearch(query, page, perPage)
	case "pixabay":
		found, err = a.pixabaySearch(query, page, perPage)
	}
	if err != nil {
		return stockPhotoPage{}, err
	}
	photoCacheMu.Lock()
	photoCache[key] = photoCacheEntry{payload: found, at: time.Now()}
	photoCacheMu.Unlock()
	return found, nil
}

func (a *App) searchStockPhotos(requested, query string, page, perPage int, cursor string) (map[string]any, *DocumentError) {
	enabled := a.configuredPhotoProviders()
	if requested == "all" {
		if len(enabled) == 0 {
			return notConfiguredPhoto("all"), nil
		}
		pages := map[string]int{}
		if cursor != "" {
			raw, err := base64.RawURLEncoding.DecodeString(cursor)
			if err != nil || json.Unmarshal(raw, &pages) != nil {
				return nil, &DocumentError{Code: "invalid", Status: 400, Message: "Not a valid cursor."}
			}
		}
		var results []any
		total := 0
		nextPages := map[string]int{}
		for _, name := range enabled {
			p := pages[name]
			if p == 0 {
				p = 1
			}
			found, err := a.searchPhotoProvider(name, query, p, perPage)
			if err != nil {
				continue
			}
			total += found.Total
			for _, photo := range found.Results {
				results = append(results, photo)
			}
			if found.NextPage != nil {
				nextPages[name] = p + 1
			}
		}
		var next *string
		if len(nextPages) > 0 {
			raw, _ := json.Marshal(nextPages)
			value := base64.RawURLEncoding.EncodeToString(raw)
			next = &value
		}
		return map[string]any{"configured": true, "provider": "all", "total": total, "next_page": next, "results": results}, nil
	}
	name := requested
	if name == "" {
		if len(enabled) == 1 {
			name = enabled[0]
		}
	}
	if name == "" || !a.photoProviderConfigured(name) {
		return notConfiguredPhoto(requested), nil
	}
	found, err := a.searchPhotoProvider(name, query, page, perPage)
	if err != nil {
		return nil, &DocumentError{Code: "stock_unavailable", Status: 503,
			Message: "The stock photo provider could not be reached. Try again shortly."}
	}
	results := make([]any, len(found.Results))
	for i, photo := range found.Results {
		results[i] = photo
	}
	return map[string]any{"configured": true, "provider": name, "total": found.Total,
		"next_page": found.NextPage, "results": results}, nil
}

func stockErrorDocument(err error, message string) *DocumentError {
	if busy, ok := err.(*stockBusyError); ok {
		return &DocumentError{Code: "stock_busy", Status: 429,
			Message: fmt.Sprintf("The stock provider is busy. Try again in %d seconds.", int(busy.wait+0.999))}
	}
	return &DocumentError{Code: "stock_unavailable", Status: 503, Message: message}
}

func notConfiguredPhoto(provider string) map[string]any {
	return map[string]any{"configured": false, "provider": provider, "total": 0, "next_page": nil, "results": []any{}}
}

func (a *App) resolvePhoto(name, id string) (resolvedPhoto, error) {
	switch name {
	case "unsplash":
		data, err := a.stockJSON("unsplash", unsplashAPIBase+"/photos/"+url.PathEscape(id),
			map[string]string{"Authorization": "Client-ID " + a.photoProviderKey(name), "Accept-Version": "v1"})
		if err != nil {
			return resolvedPhoto{}, err
		}
		photo := unsplashPhoto(data, name)
		urls, _ := data["urls"].(map[string]any)
		hosts := map[string]bool{"images.unsplash.com": true}
		raw := checkPhotoHost(stringFromAny(urls["raw"]), hosts)
		download := ""
		if raw != "" {
			u, _ := url.Parse(raw)
			q := u.Query()
			q.Set("w", "2400")
			q.Set("q", "85")
			q.Set("fm", "jpg")
			q.Set("fit", "max")
			u.RawQuery = q.Encode()
			download = u.String()
		}
		if download == "" {
			download = checkPhotoHost(stringFromAny(urls["regular"]), hosts)
		}
		if photo.ID == "" || download == "" {
			return resolvedPhoto{}, &videoProviderError{"unexpected"}
		}
		return resolvedPhoto{Photo: photo, DownloadURL: download}, nil
	case "pexels":
		data, err := a.stockJSON("pexels", pexelsAPIBase+"/photos/"+url.PathEscape(id), map[string]string{"Authorization": a.photoProviderKey(name)})
		if err != nil {
			return resolvedPhoto{}, err
		}
		photo := pexelsPhoto(data, name)
		src, _ := data["src"].(map[string]any)
		hosts := map[string]bool{"images.pexels.com": true}
		download := checkPhotoHost(stringFromAny(src["large2x"]), hosts)
		if download == "" {
			download = checkPhotoHost(stringFromAny(src["original"]), hosts)
		}
		if photo.ID == "" || download == "" {
			return resolvedPhoto{}, &videoProviderError{"unexpected"}
		}
		return resolvedPhoto{Photo: photo, DownloadURL: download}, nil
	case "pixabay":
		data, err := a.stockJSON("pixabay", stockURL(pixabayAPIBase, map[string]string{"key": a.photoProviderKey(name), "id": id}), nil)
		if err != nil {
			return resolvedPhoto{}, err
		}
		hits, _ := data["hits"].([]any)
		if len(hits) == 0 {
			return resolvedPhoto{}, &videoProviderError{"unexpected"}
		}
		photo := pixabayPhoto(hits[0], name)
		item, _ := hits[0].(map[string]any)
		hosts := map[string]bool{"cdn.pixabay.com": true, "pixabay.com": true}
		download := checkPhotoHost(stringFromAny(item["fullHDURL"]), hosts)
		if download == "" {
			download = checkPhotoHost(stringFromAny(item["largeImageURL"]), hosts)
		}
		if photo.ID != id || download == "" {
			return resolvedPhoto{}, &videoProviderError{"unexpected"}
		}
		return resolvedPhoto{Photo: photo, DownloadURL: download}, nil
	}
	return resolvedPhoto{}, &videoProviderError{"invalid"}
}

func validPhotoID(name, id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch name {
		case "unsplash":
			if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return false
			}
		default:
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	if name == "unsplash" && (len(id) < 5 || len(id) > 40) {
		return false
	}
	if name != "unsplash" && len(id) > 12 {
		return false
	}
	return true
}

func (a *App) downloadStockImage(rawURL string, hosts map[string]bool, maxBytes int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	current := rawURL
	for hop := 0; hop <= 3; hop++ {
		target, err := remoteCheckURL(ctx, current)
		if err != nil {
			return nil, err
		}
		if !hosts[target.host] {
			return nil, remoteError("URL is not on an allowed host.")
		}
		response, err := remoteDoRealWithMax(ctx, target, maxBytes)
		if err != nil {
			return nil, err
		}
		if response.status >= 300 && response.status < 400 {
			location := response.header.Get("Location")
			if location == "" {
				return nil, remoteError("The provider redirected without a target.")
			}
			base, _ := url.Parse(current)
			next, perr := url.Parse(location)
			if perr != nil {
				return nil, remoteError("Malformed redirect target.")
			}
			current = base.ResolveReference(next).String()
			continue
		}
		if response.status != 200 {
			return nil, remoteError(fmt.Sprintf("The image host answered %d.", response.status))
		}
		if len(response.body) > maxBytes {
			return nil, &remoteImageTooLarge{remoteImageError{"The image is too large."}}
		}
		return response.body, nil
	}
	return nil, remoteError("Too many redirects.")
}

func (a *App) findPhotoBySame(owner *UserRow, provider, id string) *assetRow {
	rows, err := a.DB.Query(`SELECT `+assetCols+` FROM assets_asset WHERE owner_id = ? ORDER BY created_at DESC, id ASC`, owner.ID)
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
		if json.Unmarshal([]byte(asset.Meta), &meta) != nil {
			continue
		}
		if stringFromAny(meta["provider"]) == provider && stringFromAny(meta["photo_id"]) == id {
			return &asset
		}
	}
	return nil
}

var stockPhotoDownload = func(a *App, rawURL string, hosts map[string]bool, maxBytes int) ([]byte, error) {
	return a.downloadStockImage(rawURL, hosts, maxBytes)
}

func (a *App) importStockPhoto(owner *UserRow, provider, id string) (*assetRow, bool, *DocumentError) {
	if !a.photoProviderConfigured(provider) {
		return nil, false, &DocumentError{Code: "stock_not_configured", Status: 400,
			Message: "No stock photo provider is configured on this server."}
	}
	if !validPhotoID(provider, id) {
		return nil, false, &DocumentError{Code: "invalid", Status: 400, Message: "Not a valid photo id for this provider."}
	}
	if existing := a.findPhotoBySame(owner, provider, id); existing != nil {
		return existing, false, nil
	}
	resolved, err := a.resolvePhoto(provider, id)
	if err != nil {
		return nil, false, stockErrorDocument(err, "The stock photo provider could not be reached. Try again shortly.")
	}
	hosts := map[string]bool{"images.unsplash.com": true, "images.pexels.com": true,
		"cdn.pixabay.com": true, "pixabay.com": true}
	data, err := stockPhotoDownload(a, resolved.DownloadURL, hosts, maxUploadBytes)
	if err != nil {
		if _, ok := err.(*remoteImageTooLarge); ok {
			return nil, false, &DocumentError{Code: "file_too_large", Status: 413, Message: "The file is too large."}
		}
		return nil, false, &DocumentError{Code: "stock_unavailable", Status: 503,
			Message: "The stock photo provider could not be reached. Try again shortly."}
	}
	prepared, perr := prepareFile(data)
	if perr != nil {
		return nil, false, perr
	}
	name := resolved.Photo.Author
	if name == "" {
		name = resolved.Photo.Provider + " photo"
	}
	asset, derr := a.storePreparedAsset(owner, prepared, cleanAssetName(name, "Photo"), nil,
		map[string]any{"provider": provider, "photo_id": id, "attribution": map[string]any{
			"provider": provider, "author": resolved.Photo.Author, "author_url": resolved.Photo.AuthorURL,
			"source_url": resolved.Photo.SourceURL}})
	if derr != nil {
		return nil, false, derr
	}
	return asset, true, nil
}
