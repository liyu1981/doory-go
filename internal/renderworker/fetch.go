// Fetch interception: every request the page makes goes through the policy
// (chromium_worker.py's context.route handler): allowed plain GETs
// continue, same-origin assets are fetched by THIS process without
// following redirects, everything else is blocked and counted.
package renderworker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"doory-go/internal/chromium"
	"doory-go/internal/renderpolicy"
)

type fetchInterceptor struct {
	mu sync.Mutex
}

func (f *fetchInterceptor) install(ctx context.Context, browser *chromium.Browser,
	policy *renderpolicy.Policy, maxAsset int, report *Report,
) {
	_, _ = browser.Call(ctx, "Fetch.enable", map[string]any{
		"patterns": []map[string]any{{"urlPattern": "*", "requestStage": "Request"}},
	})
	sub := browser.Subscribe()
	go func() {
		for ev := range sub {
			if ev.Method != "Fetch.requestPaused" {
				continue
			}
			var pause struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL    string `json:"url"`
					Method string `json:"method"`
				} `json:"request"`
			}
			if json.Unmarshal(ev.Params, &pause) != nil {
				continue
			}
			decision := policy.Evaluate(pause.Request.URL, pause.Request.Method)
			if !decision.Allowed {
				f.block(browser, pause.RequestID, pause.Request.URL, decision.Reason, report)
				continue
			}
			report.AllowedTotal++
			if !decision.Fetch {
				_, _ = browser.Call(ctx, "Fetch.continueRequest", map[string]any{"requestId": pause.RequestID})
				continue
			}
			// the worker fetches it itself, without following redirects
			body, status, headers, err := fetchOnce(pause.Request.URL, maxAsset)
			switch {
			case err != nil:
				f.block(browser, pause.RequestID, pause.Request.URL, "fetch-failed", report)
			case status >= 300 && status < 400:
				f.block(browser, pause.RequestID, pause.Request.URL, "redirect", report)
			case body == nil:
				f.block(browser, pause.RequestID, pause.Request.URL, "too-large", report)
			default:
				out := make([]map[string]string, 0, len(headers))
				for name, values := range headers {
					for _, v := range values {
						out = append(out, map[string]string{"name": name, "value": v})
					}
				}
				_, _ = browser.Call(ctx, "Fetch.fulfillRequest", map[string]any{
					"requestId":       pause.RequestID,
					"responseCode":    status,
					"responseHeaders": out,
					"body":            base64.StdEncoding.EncodeToString(body),
				})
			}
		}
	}()
}

func (f *fetchInterceptor) block(browser *chromium.Browser, id, url, reason string, report *Report) {
	report.BlockedTotal++
	if len(report.Blocked) < maxRecordedBlocked {
		report.Blocked = append(report.Blocked, Blocked{URL: url[:min(len(url), 300)], Reason: reason})
	}
	_, _ = browser.Call(context.Background(), "Fetch.failRequest", map[string]any{
		"requestId": id, "errorReason": "BlockedByClient",
	})
}

// fetchOnce performs the allowed GET with no redirects and a size cap.
func fetchOnce(rawURL string, maxBytes int) ([]byte, int, http.Header, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, 0, nil, err
	}
	if len(body) > maxBytes {
		return nil, 0, nil, nil // too large
	}
	return body, resp.StatusCode, resp.Header, nil
}
