// Off-site image optimizers (apps/exports/optimizers/{http,tinypng,kraken,custom}.py):
// the image is sent through the operator transport policy to TinyPNG, Kraken.io
// or a custom contract server and the returned file is kept only when smaller.
package doory

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	tinyPngAPI        = "https://api.tinify.com"
	krakenAPI         = "https://api.kraken.io/v1/upload"
	optimizerJSONCap  = 64 * 1024
	optimizerMetaNone = "none"
)

type offsiteImageJob struct {
	data      []byte
	format    string
	mime      string
	width     int
	height    int
	quality   *int
	stripMeta bool
	srgb      bool
}

type optimizerHTTPResponse struct {
	status int
	header http.Header
	body   []byte
}

func (a *App) optimizeOffsite(job offsiteImageJob, optimizer string) ([]byte, error) {
	deadline := time.Now().Add(time.Duration(a.Settings.ImageOptimizerTimeout) * time.Second)
	if a.Settings.ImageOptimizerTimeout <= 0 {
		deadline = time.Now().Add(30 * time.Second)
	}
	switch optimizer {
	case "tinypng":
		return a.tinyPngShrink(job, deadline)
	case "kraken":
		return a.krakenShrink(job, deadline)
	case "custom":
		return a.customShrink(job, deadline)
	}
	return nil, fmt.Errorf("unknown optimizer")
}

func optimizerMIME(format string) string {
	return map[string]string{"png": "image/png", "jpg": "image/jpeg", "webp": "image/webp", "avif": "image/avif"}[format]
}

func (a *App) optimizerSend(method, rawURL string, headers map[string]string,
	body []byte, maxBytes int, deadline time.Time) (optimizerHTTPResponse, error) {
	policy := a.Settings.aiOperatorPolicy()
	endpoint, terr := a.Settings.aiCheckURL(rawURL, policy, true)
	if terr != nil {
		return optimizerHTTPResponse{}, fmt.Errorf("%s", terr.message)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return optimizerHTTPResponse{}, fmt.Errorf("It took too long.")
	}
	dialer := &net.Dialer{Timeout: minDuration(5*time.Second, remaining)}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var last error
			for _, address := range endpoint.addresses {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address, strconv.Itoa(endpoint.port)))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			if last == nil {
				last = fmt.Errorf("no address")
			}
			return nil, last
		},
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   remaining,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		return optimizerHTTPResponse{}, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		if time.Now().After(deadline) {
			return optimizerHTTPResponse{}, fmt.Errorf("It took too long.")
		}
		return optimizerHTTPResponse{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return optimizerHTTPResponse{}, err
	}
	if len(payload) > maxBytes {
		return optimizerHTTPResponse{}, fmt.Errorf("the service answered with a file that is too large")
	}
	return optimizerHTTPResponse{status: response.StatusCode, header: response.Header, body: payload}, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func optimizerJSON(body []byte) map[string]any {
	var data map[string]any
	_ = json.Unmarshal(body, &data)
	return data
}

func optimizerFailure(response optimizerHTTPResponse, fields ...string) error {
	data := optimizerJSON(response.body)
	text := ""
	for _, field := range fields {
		if value, ok := data[field].(string); ok && value != "" {
			text = value
			break
		}
	}
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return ' '
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	if len(text) > 160 {
		text = text[:160]
	}
	if text == "" {
		return fmt.Errorf("the service answered %d", response.status)
	}
	return fmt.Errorf("the service answered %d: %s", response.status, text)
}

func (a *App) tinyPngShrink(job offsiteImageJob, deadline time.Time) ([]byte, error) {
	key := strings.TrimSpace(a.Settings.TinyPngAPIKey)
	if key == "" {
		return nil, fmt.Errorf("TinyPNG is not configured")
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("api:"+key))
	shrunk, err := a.optimizerSend("POST", tinyPngAPI+"/shrink",
		map[string]string{"Authorization": auth, "Content-Type": job.mime}, job.data, optimizerJSONCap, deadline)
	if err != nil {
		return nil, err
	}
	if shrunk.status != http.StatusCreated {
		return nil, optimizerFailure(shrunk, "message", "error")
	}
	location := shrunk.header.Get("Location")
	if strings.HasPrefix(location, "/") {
		location = tinyPngAPI + location
	}
	if !strings.HasPrefix(location, tinyPngAPI+"/output/") {
		return nil, fmt.Errorf("the service sent no download address")
	}
	var output optimizerHTTPResponse
	if job.stripMeta {
		output, err = a.optimizerSend("GET", location, map[string]string{"Authorization": auth},
			nil, a.Settings.ImageOptimizerMaxBytes, deadline)
	} else {
		body, _ := json.Marshal(map[string]any{"preserve": []string{"copyright", "creation", "location"}})
		output, err = a.optimizerSend("POST", location,
			map[string]string{"Authorization": auth, "Content-Type": "application/json"},
			body, a.Settings.ImageOptimizerMaxBytes, deadline)
	}
	if err != nil {
		return nil, err
	}
	if output.status != http.StatusOK {
		return nil, optimizerFailure(output, "message", "error")
	}
	return output.body, nil
}

func (a *App) krakenShrink(job offsiteImageJob, deadline time.Time) ([]byte, error) {
	apiKey, secret := strings.TrimSpace(a.Settings.KrakenAPIKey), strings.TrimSpace(a.Settings.KrakenAPISecret)
	if apiKey == "" || secret == "" {
		return nil, fmt.Errorf("Kraken.io is not configured")
	}
	options := map[string]any{
		"auth": map[string]string{"api_key": apiKey, "api_secret": secret},
		"wait": true,
	}
	if job.quality != nil {
		options["lossy"] = true
		options["quality"] = *job.quality
	}
	if !job.stripMeta {
		options["preserve_meta"] = []string{"profile", "date", "copyright", "geotag", "orientation"}
	} else if !job.srgb {
		options["preserve_meta"] = []string{"profile"}
	}
	optionsJSON, _ := json.Marshal(options)
	body, contentType := optimizerMultipart(
		map[string]string{"data": string(optionsJSON)}, "upload",
		"image."+job.format, job.mime, job.data)
	answer, err := a.optimizerSend("POST", krakenAPI,
		map[string]string{"Content-Type": contentType}, body, optimizerJSONCap, deadline)
	if err != nil {
		return nil, err
	}
	data := optimizerJSON(answer.body)
	if answer.status != http.StatusOK || data["success"] != true {
		return nil, optimizerFailure(answer, "message")
	}
	rawURL, _ := data["kraked_url"].(string)
	if !optimizerKrakenDownload(rawURL) {
		return nil, fmt.Errorf("the service sent no download address")
	}
	output, err := a.optimizerSend("GET", rawURL, nil, nil, a.Settings.ImageOptimizerMaxBytes, deadline)
	if err != nil {
		return nil, err
	}
	if output.status != http.StatusOK {
		return nil, optimizerFailure(output)
	}
	return output.body, nil
}

func optimizerKrakenDownload(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	port := parsed.Port()
	if port != "" && port != "443" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	return parsed.Scheme == "https" && parsed.User == nil &&
		(host == "kraken.io" || strings.HasSuffix(host, ".kraken.io"))
}

func optimizerMultipart(fields map[string]string, fileField, filename, mimeType string, data []byte) ([]byte, string) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		_ = writer.WriteField(name, value)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fileField, filename))
	header.Set("Content-Type", mimeType)
	part, _ := writer.CreatePart(header)
	_, _ = part.Write(data)
	_ = writer.Close()
	return body.Bytes(), writer.FormDataContentType()
}

func (a *App) customShrink(job offsiteImageJob, deadline time.Time) ([]byte, error) {
	rawURL := strings.TrimSpace(a.Settings.ImageOptimizerCustomURL)
	if rawURL == "" {
		return nil, fmt.Errorf("the custom optimizer is not configured")
	}
	query := url.Values{}
	query.Set("format", "keep")
	if !job.stripMeta {
		query.Set("metadata", "all")
	} else if !job.srgb {
		query.Set("metadata", "icc")
	} else {
		query.Set("metadata", optimizerMetaNone)
	}
	if job.quality != nil {
		query.Set("quality", strconv.Itoa(*job.quality))
	}
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	headers := map[string]string{"Content-Type": job.mime}
	if key := strings.TrimSpace(a.Settings.ImageOptimizerCustomKey); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	answer, err := a.optimizerSend("POST", rawURL+separator+query.Encode(), headers,
		job.data, a.Settings.ImageOptimizerMaxBytes, deadline)
	if err != nil {
		return nil, err
	}
	if answer.status != http.StatusOK {
		return nil, optimizerFailure(answer, "message", "error")
	}
	expected := map[string]string{
		"X-Format":      map[string]string{"png": "png", "jpg": "jpeg", "webp": "webp", "avif": "avif"}[job.format],
		"X-Width":       strconv.Itoa(job.width),
		"X-Height":      strconv.Itoa(job.height),
		"X-Output-Size": strconv.Itoa(len(answer.body)),
	}
	for name, want := range expected {
		sent := strings.ToLower(strings.TrimSpace(answer.header.Get(name)))
		if sent != "" && sent != strings.ToLower(want) && !(want == "jpeg" && sent == "jpg") {
			return nil, fmt.Errorf("its %s header doesn't match the image", name)
		}
	}
	contentType := answer.header.Get("Content-Type")
	if contentType != "" && !strings.HasPrefix(contentType, job.mime) {
		return nil, fmt.Errorf("it answered with another content type")
	}
	return answer.body, nil
}
