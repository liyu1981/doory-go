// Stock video providers (Pexels and Pixabay videos): search, resolve and
// download. Photo providers remain G12; this file only needs the video API
// surface the clip endpoints use.
package doory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	videoPexelsBase  = "https://api.pexels.com"
	videoPixabayBase = "https://pixabay.com/api/videos/"
)

type stockVideo struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	ThumbURL   string `json:"thumb_url"`
	PreviewURL string `json:"preview_url"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	DurationMS int    `json:"duration_ms"`
	Author     string `json:"author"`
	AuthorURL  string `json:"author_url"`
	SourceURL  string `json:"source_url"`
}

type videoSearchPage struct {
	Total    int          `json:"total"`
	NextPage *string      `json:"next_page"`
	Results  []stockVideo `json:"results"`
	Provider string       `json:"provider"`
}

type resolvedVideo struct {
	Video       stockVideo
	DownloadURL string
}

type videoProviderError struct{ message string }

func (e *videoProviderError) Error() string { return e.message }

type videoCacheEntry struct {
	payload videoSearchPage
	at      time.Time
}

var (
	videoCacheMu sync.Mutex
	videoCache   = map[string]videoCacheEntry{}
)

func (a *App) videoProviderKey(name string) string {
	if a.Settings == nil {
		return ""
	}
	switch name {
	case "pexels_video":
		return firstNonEmpty(a.Settings.PexelsVideoAPIKey, a.Settings.PexelsAccessKey)
	case "pixabay_video":
		return firstNonEmpty(a.Settings.PixabayVideoAPIKey, a.Settings.PixabayAccessKey)
	}
	return ""
}

func (a *App) videoProviderConfigured(name string) bool {
	return a.videoAvailable() && a.videoProviderKey(name) != ""
}

func (a *App) configuredVideoProviders() []string {
	out := []string{}
	for _, name := range a.Settings.StockVideoProviders {
		name = strings.ToLower(strings.TrimSpace(name))
		if choiceValid(videoProviders, name) && a.videoProviderConfigured(name) {
			out = append(out, name)
		}
	}
	return out
}

func videoOrientation(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	ratio := float64(width) / float64(height)
	if ratio >= 0.9 && ratio <= 1.1 {
		return "square"
	}
	if ratio > 1 {
		return "landscape"
	}
	return "portrait"
}

func videoMatches(v stockVideo, orientation, duration string) bool {
	if orientation != "" && videoOrientation(v.Width, v.Height) != orientation {
		return false
	}
	seconds := float64(v.DurationMS) / 1000
	switch duration {
	case "short":
		return seconds < 15
	case "medium":
		return seconds >= 15 && seconds <= 60
	case "long":
		return seconds > 60
	}
	return true
}

func videoHTTPGet(rawURL string, headers map[string]string, timeout time.Duration) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func videoAPIJSON(rawURL string, headers map[string]string) (map[string]any, error) {
	body, status, err := videoHTTPGet(rawURL, headers, 15*time.Second)
	if err != nil {
		return nil, &videoProviderError{"unavailable"}
	}
	if status == 429 {
		return nil, &videoProviderError{"busy"}
	}
	if status < 200 || status >= 300 {
		return nil, &videoProviderError{"unavailable"}
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, &videoProviderError{"unavailable"}
	}
	return data, nil
}

func videoQString(values ...string) string {
	pairs := make([]string, 0, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		pairs = append(pairs, values[i]+"="+url.QueryEscape(values[i+1]))
	}
	return strings.Join(pairs, "&")
}

func (a *App) pexelsVideoSearch(query string, page, perPage int, orientation, duration string) (videoSearchPage, error) {
	low, high := 0, 0
	switch duration {
	case "short":
		low, high = 0, 15
	case "medium":
		low, high = 15, 60
	case "long":
		low = 60
	}
	filteredHere := (query != "" && duration != "") || (query == "" && orientation != "")
	asked := perPage
	if filteredHere {
		asked = minIntInt(perPage*4, 80)
	}
	params := videoQString("page", fmt.Sprint(page), "per_page", fmt.Sprint(asked))
	if query != "" {
		params += "&query=" + url.QueryEscape(query)
		if orientation != "" {
			params += "&orientation=" + url.QueryEscape(orientation)
		}
		if low > 0 {
			params += "&min_duration=" + fmt.Sprint(low)
		}
		if high > 0 {
			params += "&max_duration=" + fmt.Sprint(high)
		}
	} else {
		if low > 0 {
			params += "&min_duration=" + fmt.Sprint(low)
		}
		if high > 0 {
			params += "&max_duration=" + fmt.Sprint(high)
		}
	}
	endpoint := videoPexelsBase + "/videos/search?" + params
	if query == "" {
		endpoint = videoPexelsBase + "/videos/popular?" + params
	}
	headers := map[string]string{"Authorization": a.videoProviderKey("pexels_video")}
	data, err := videoAPIJSON(endpoint, headers)
	if err != nil {
		return videoSearchPage{}, err
	}
	rawVideos, _ := data["videos"].([]any)
	var results []stockVideo
	for _, raw := range rawVideos {
		item, _ := raw.(map[string]any)
		video := pexelsVideoItem(item, "pexels_video")
		if video.ID != "" && videoMatches(video, orientation, duration) {
			results = append(results, video)
		}
	}
	total := intFromAny(data["total_results"])
	var next *string
	if total > page*asked {
		value := fmt.Sprint(page + 1)
		next = &value
	}
	return videoSearchPage{Total: total, NextPage: next, Results: results, Provider: "pexels_video"}, nil
}

func pexelsVideoItem(item map[string]any, provider string) stockVideo {
	id := intFromAny(item["id"])
	if id == 0 {
		return stockVideo{}
	}
	thumb := stringFromAny(item["image"])
	if !strings.HasPrefix(thumb, "https://") {
		return stockVideo{}
	}
	files, _ := item["video_files"].([]any)
	var links []string
	for _, raw := range files {
		entry, _ := raw.(map[string]any)
		link := stringFromAny(entry["link"])
		if strings.HasPrefix(link, "https://videos.pexels.com/") &&
			stringFromAny(entry["file_type"]) == "video/mp4" {
			links = append(links, link)
		}
	}
	if len(links) == 0 {
		return stockVideo{}
	}
	user, _ := item["user"].(map[string]any)
	return stockVideo{ID: fmt.Sprint(id), Provider: provider, ThumbURL: thumb, PreviewURL: links[0],
		Width: intFromAny(item["width"]), Height: intFromAny(item["height"]),
		DurationMS: intFromAny(item["duration"]) * 1000, Author: stringFromAny(user["name"]),
		AuthorURL: httpsLink(stringFromAny(user["url"])), SourceURL: httpsLink(stringFromAny(item["url"]))}
}

func (a *App) pixabayVideoSearch(query string, page, perPage int, orientation, duration string) (videoSearchPage, error) {
	asked := perPage
	if orientation != "" || duration != "" {
		asked = minIntInt(maxInt(perPage*4, 3), 200)
	}
	params := videoQString("key", a.videoProviderKey("pixabay_video"), "page", fmt.Sprint(page),
		"per_page", fmt.Sprint(asked), "video_type", "film", "safesearch", "true", "lang", "en")
	if query != "" {
		params += "&q=" + url.QueryEscape(query)
	}
	data, err := videoAPIJSON(videoPixabayBase+"?"+params, nil)
	if err != nil {
		return videoSearchPage{}, err
	}
	hits, _ := data["hits"].([]any)
	var results []stockVideo
	for _, raw := range hits {
		item, _ := raw.(map[string]any)
		video := pixabayVideoItem(item, "pixabay_video")
		if video.ID != "" && videoMatches(video, orientation, duration) {
			results = append(results, video)
		}
	}
	total := intFromAny(data["totalHits"])
	if total > 500 {
		total = 500
	}
	next := (*string)(nil)
	if total > page*asked {
		value := fmt.Sprint(page + 1)
		next = &value
	}
	return videoSearchPage{Total: total, NextPage: next, Results: results, Provider: "pixabay_video"}, nil
}

func pixabayVideoItem(item map[string]any, provider string) stockVideo {
	id := intFromAny(item["id"])
	if id == 0 {
		return stockVideo{}
	}
	videos, _ := item["videos"].(map[string]any)
	var files []string
	var width, height int
	for _, name := range []string{"tiny", "small", "medium", "large"} {
		entry, _ := videos[name].(map[string]any)
		link := stringFromAny(entry["url"])
		if strings.HasPrefix(link, "https://cdn.pixabay.com/") {
			files = append(files, link)
		}
		if name == "large" {
			width, height = intFromAny(entry["width"]), intFromAny(entry["height"])
		}
	}
	if len(files) == 0 {
		return stockVideo{}
	}
	thumb := ""
	for _, name := range []string{"small", "tiny"} {
		entry, _ := videos[name].(map[string]any)
		if link := stringFromAny(entry["thumbnail"]); strings.HasPrefix(link, "https://cdn.pixabay.com/") {
			thumb = link
			break
		}
	}
	if thumb == "" {
		return stockVideo{}
	}
	user := stringFromAny(item["user"])
	return stockVideo{ID: fmt.Sprint(id), Provider: provider, ThumbURL: thumb, PreviewURL: files[0],
		Width: width, Height: height, DurationMS: intFromAny(item["duration"]) * 1000, Author: user,
		AuthorURL: "", SourceURL: httpsLink(stringFromAny(item["pageURL"]))}
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n := 0
		_, _ = fmt.Sscan(x, &n)
		return n
	}
	return 0
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func httpsLink(raw string) string {
	if strings.HasPrefix(raw, "https://") {
		return raw
	}
	return ""
}

func minIntInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (a *App) videoSearchProvider(name, query string, page, perPage int, orientation, duration string) (videoSearchPage, error) {
	key := strings.Join([]string{name, query, fmt.Sprint(page, perPage), orientation, duration}, "|")
	videoCacheMu.Lock()
	entry, ok := videoCache[key]
	videoCacheMu.Unlock()
	if ok && time.Since(entry.at) < 5*time.Minute {
		return entry.payload, nil
	}
	var found videoSearchPage
	var err error
	switch name {
	case "pexels_video":
		found, err = a.pexelsVideoSearch(query, page, perPage, orientation, duration)
	case "pixabay_video":
		found, err = a.pixabayVideoSearch(query, page, perPage, orientation, duration)
	}
	if err != nil {
		return videoSearchPage{}, err
	}
	videoCacheMu.Lock()
	videoCache[key] = videoCacheEntry{payload: found, at: time.Now()}
	videoCacheMu.Unlock()
	return found, nil
}

func (a *App) searchVideos(requested, query string, page, perPage int, cursor, orientation, duration string) (map[string]any, *DocumentError) {
	enabled := a.configuredVideoProviders()
	if requested == "all" || (requested == "" && len(enabled) > 1) {
		if len(enabled) == 0 {
			return map[string]any{"configured": false, "provider": "all", "total": 0, "next_page": nil, "results": []any{}}, nil
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
			found, err := a.videoSearchProvider(name, query, p, perPage, orientation, duration)
			if err != nil {
				continue
			}
			total += found.Total
			for _, video := range found.Results {
				results = append(results, video)
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
		} else {
			name = ""
		}
	}
	if name == "" || !a.videoProviderConfigured(name) {
		return map[string]any{"configured": false, "provider": requested, "total": 0, "next_page": nil, "results": []any{}}, nil
	}
	found, err := a.videoSearchProvider(name, query, page, perPage, orientation, duration)
	if err != nil {
		return nil, &DocumentError{Code: "stock_unavailable", Status: 503,
			Message: "The stock video provider could not be reached. Try again shortly."}
	}
	results := make([]any, len(found.Results))
	for i, video := range found.Results {
		results[i] = video
	}
	return map[string]any{"configured": true, "provider": name, "total": found.Total,
		"next_page": found.NextPage, "results": results, "stale": false}, nil
}

func (a *App) resolveVideo(name, id string) (resolvedVideo, error) {
	headers := map[string]string{}
	var endpoint string
	switch name {
	case "pexels_video":
		if !validPexelsID(id) {
			return resolvedVideo{}, &videoProviderError{"invalid"}
		}
		endpoint = videoPexelsBase + "/videos/videos/" + url.PathEscape(id)
		headers["Authorization"] = a.videoProviderKey(name)
	case "pixabay_video":
		if !validPixabayID(id) {
			return resolvedVideo{}, &videoProviderError{"invalid"}
		}
		endpoint = videoPixabayBase + "?" + videoQString("key", a.videoProviderKey(name), "id", id)
	default:
		return resolvedVideo{}, &videoProviderError{"invalid"}
	}
	data, err := videoAPIJSON(endpoint, headers)
	if err != nil {
		return resolvedVideo{}, err
	}
	var item map[string]any
	if name == "pixabay_video" {
		hits, _ := data["hits"].([]any)
		if len(hits) > 0 {
			item, _ = hits[0].(map[string]any)
		}
	} else {
		item = data
	}
	video := stockVideo{}
	if name == "pexels_video" {
		video = pexelsVideoItem(item, name)
	} else {
		video = pixabayVideoItem(item, name)
	}
	if video.ID != id {
		return resolvedVideo{}, &videoProviderError{"unexpected"}
	}
	download := video.PreviewURL
	if name == "pexels_video" {
		files, _ := item["video_files"].([]any)
		type candidate struct {
			short int
			link  string
		}
		var candidates []candidate
		for _, raw := range files {
			entry, _ := raw.(map[string]any)
			link := stringFromAny(entry["link"])
			w, h := intFromAny(entry["width"]), intFromAny(entry["height"])
			if strings.HasPrefix(link, "https://videos.pexels.com/") && w > 0 && h > 0 {
				candidates = append(candidates, candidate{minIntInt(w, h), link})
			}
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].short < candidates[j].short })
		for _, c := range candidates {
			if c.short <= 1080 {
				download = c.link
			}
		}
	} else {
		files := []string{}
		videos, _ := item["videos"].(map[string]any)
		for _, rend := range []string{"tiny", "small", "medium", "large"} {
			entry, _ := videos[rend].(map[string]any)
			if link := stringFromAny(entry["url"]); strings.HasPrefix(link, "https://cdn.pixabay.com/") {
				files = append(files, link)
			}
		}
		if len(files) > 0 {
			download = files[len(files)-1]
		}
	}
	if download == "" {
		return resolvedVideo{}, &videoProviderError{"unexpected"}
	}
	return resolvedVideo{Video: video, DownloadURL: download}, nil
}

func validPexelsID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validPixabayID(id string) bool { return validPexelsID(id) }

func (a *App) downloadVideo(rawURL string, hosts map[string]bool, maxBytes int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Settings.VideoDownloadDeadlineSeconds)*time.Second)
	defer cancel()
	current := rawURL
	for hop := 0; hop <= 3; hop++ {
		target, err := remoteCheckURL(ctx, current)
		if err != nil {
			return nil, err
		}
		if !hosts[target.host] {
			return nil, remoteError("That address is not on the public internet.")
		}
		response, err := remoteDoRealWithMax(ctx, target, maxBytes)
		if err != nil {
			return nil, err
		}
		switch response.status {
		case 301, 302, 303, 307, 308:
			location := response.header.Get("Location")
			if location == "" {
				return nil, remoteError("The server redirected without a target.")
			}
			base, _ := url.Parse(current)
			next, perr := url.Parse(location)
			if perr != nil {
				return nil, remoteError("The server redirected without a target.")
			}
			current = base.ResolveReference(next).String()
			continue
		case 200:
			if len(response.body) > maxBytes {
				return nil, &remoteImageTooLarge{remoteImageError{"The file is too large."}}
			}
			return response.body, nil
		default:
			return nil, remoteError("The download failed.")
		}
	}
	return nil, remoteError("Too many redirects.")
}

func (a *App) importStockVideo(owner *UserRow, name, id string) (*assetRow, bool, *DocumentError) {
	if !a.videoAvailable() {
		return nil, false, &DocumentError{Code: "video_unavailable", Status: 400,
			Message: "This server can't take videos (ffmpeg is not installed)."}
	}
	if !a.videoProviderConfigured(name) {
		return nil, false, &DocumentError{Code: "stock_not_configured", Status: 400,
			Message: "This stock video provider is not configured on this server."}
	}
	if (name == "pexels_video" && !validPexelsID(id)) || (name == "pixabay_video" && !validPixabayID(id)) {
		return nil, false, &DocumentError{Code: "invalid", Status: 400, Message: "Not a valid video id for this provider."}
	}
	a.failStaleVideoJobs(owner.ID)
	existing, derr := a.findVideoBySame(owner, map[string]string{"provider": name, "video_id": id})
	if derr != nil {
		return nil, false, derr
	}
	if existing != nil {
		return existing, false, nil
	}
	resolved, err := a.resolveVideo(name, id)
	if err != nil {
		if err.Error() == "invalid" {
			return nil, false, &DocumentError{Code: "invalid", Status: 400,
				Message: "Not a valid video id for this provider."}
		}
		return nil, false, &DocumentError{Code: "stock_unavailable", Status: 503,
			Message: "The stock video provider could not be reached. Try again shortly."}
	}
	hosts := map[string]bool{"videos.pexels.com": true}
	if name == "pixabay_video" {
		hosts = map[string]bool{"cdn.pixabay.com": true}
	}
	data, derr2 := a.downloadVideo(resolved.DownloadURL, hosts, a.Settings.MaxVideoUploadBytes)
	if derr2 != nil {
		if _, ok := derr2.(*remoteImageTooLarge); ok {
			return nil, false, &DocumentError{Code: "file_too_large", Status: 413, Message: "The video is too large."}
		}
		return nil, false, &DocumentError{Code: "stock_unavailable", Status: 503,
			Message: "The stock video provider could not be reached. Try again shortly."}
	}
	workdir, err := a.videoWorkDir(videoWorkPrefix)
	if err != nil {
		return nil, false, &DocumentError{Code: "server_error", Status: 500, Message: "Something went wrong on our side."}
	}
	source := filepath.Join(workdir, "source")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		_ = os.RemoveAll(workdir)
		return nil, false, &DocumentError{Code: "server_error", Status: 500, Message: "Something went wrong on our side."}
	}
	info, perr := a.probeVideo(source)
	if perr != nil {
		_ = os.RemoveAll(workdir)
		return nil, false, &DocumentError{Code: "invalid_file", Status: 400, Message: perr.Error()}
	}
	label := "Video"
	if resolved.Video.Author != "" {
		label = "Video by " + resolved.Video.Author
	}
	meta := map[string]any{"provider": name, "video_id": id, "attribution": map[string]any{
		"provider": name, "author": resolved.Video.Author, "author_url": resolved.Video.AuthorURL,
		"source_url": resolved.Video.SourceURL}}
	asset, created, derr := a.createVideoAsset(owner, source, info, cleanAssetName(label, "Video"), nil,
		meta, map[string]string{"provider": name, "video_id": id})
	if derr != nil {
		_ = os.RemoveAll(workdir)
	}
	return asset, created, derr
}
