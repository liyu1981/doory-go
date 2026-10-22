// Package chromium drives headless Chromium over the DevTools protocol —
// the Go counterpart of apps/exports/chromium_worker.py (spike S1).
//
// Implementation substitution (recorded): the Python worker uses Playwright
// atop the same protocol; this client speaks CDP directly, launches the
// same binary (EXPORT_CHROMIUM_PATH or a PATH lookup) with the upstream
// LAUNCH_ARGS, and reproduces the worker's observable contract — the report
// JSON (statuses, blocked/allowed counts, render facts) and the output
// file. Launch flags differ where Playwright adds its own; nothing
// user-visible rides on those. Web sockets are not separately intercepted
// (Playwright closes them; here the MAP * ~NOTFOUND resolver gives them no
// network — pages under test open none).
package chromium

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// LAUNCH_ARGS mirrors chromium_worker.LAUNCH_ARGS (network-blackholed,
// locked-down headless launch).
var LAUNCH_ARGS = []string{
	"--disable-dev-shm-usage",
	"--disable-gpu",
	"--disable-extensions",
	"--disable-background-networking",
	"--disable-component-update",
	"--no-first-run",
	"--mute-audio",
	"--force-color-profile=srgb",
	"--host-resolver-rules=MAP * ~NOTFOUND",
	"--force-webrtc-ip-handling-policy",
	"--webrtc-ip-handling-policy=disable_non_proxied_udp",
}

// Browser is one running headless Chromium with one attached page session.
type Browser struct {
	cmd       *exec.Cmd
	conn      *websocket.Conn
	sessionID string
	nextID    int

	eventSubs map[int]chan cdpEvent
	eventNext int

	mu      sync.Mutex
	pending map[int]chan json.RawMessage
}

type cdpEvent struct {
	Method string          `json:"-"`
	Params json.RawMessage `json:"-"`
}

// FindExecutable resolves the browser: explicit path, then PATH names.
func FindExecutable(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no chromium found (set EXPORT_CHROMIUM_PATH)")
}

// Launch starts headless Chromium with TMPDIR isolation and waits for the
// DevTools endpoint.
func Launch(ctx context.Context, executable string, noSandbox bool, workdir string) (*Browser, error) {
	args := []string{"--headless", "--remote-debugging-port=0",
		"--user-data-dir=" + filepath.Join(workdir, "profile")}
	if noSandbox {
		args = append(args, "--no-sandbox")
	}
	args = append(args, LAUNCH_ARGS...)
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = append(os.Environ(), "TMPDIR="+workdir)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b := &Browser{cmd: cmd, eventSubs: map[int]chan cdpEvent{}, pending: map[int]chan json.RawMessage{}}
	wsURL, err := b.waitDevTools(stderr)
	if err != nil {
		b.Close()
		return nil, err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		b.Close()
		return nil, err
	}
	b.conn = conn
	go b.readLoop()
	// The fetch interceptor gets its own subscription for the lifetime of
	// the browser.
	b.subscribe()
	return b, nil
}

func (b *Browser) waitDevTools(stderr io.Reader) (string, error) {
	pipe := bufio.NewReader(stderr)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		line, err := pipe.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("chromium exited before DevTools was ready: %v", err)
		}
		if i := strings.Index(line, "DevTools listening on "); i >= 0 {
			return strings.TrimSpace(line[i+len("DevTools listening on "):]), nil
		}
	}
	return "", fmt.Errorf("DevTools never became ready")
}

func (b *Browser) readLoop() {
	for {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			ID        int             `json:"id"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
			Error     *struct {
				Message string `json:"message"`
			} `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.Method != "" {
			// fan-out: the fetch interceptor and waiters each get a copy
			// (a shared channel would drop one consumer's events)
			b.mu.Lock()
			for _, ch := range b.eventSubs {
				select {
				case ch <- cdpEvent{Method: msg.Method, Params: msg.Params}:
				default:
				}
			}
			b.mu.Unlock()
			continue
		}
		b.mu.Lock()
		ch, ok := b.pending[msg.ID]
		b.mu.Unlock()
		if ok {
			if msg.Error != nil {
				payload, _ := json.Marshal(msg.Error.Message)
				ch <- json.RawMessage(`{"__error":` + string(payload) + `}`)
			} else {
				ch <- msg.Result
			}
		}
	}
}

// Call sends a CDP command on the page session and waits for its result
// (safe for concurrent use by the fetch goroutine and the main flow).
func (b *Browser) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return b.call(ctx, method, params)
}

// Subscribe exposes a fan-out event stream for the Fetch interception loop.
func (b *Browser) Subscribe() <-chan cdpEvent { return b.subscribe() }

func (b *Browser) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.mu.Unlock()
	ch := make(chan json.RawMessage, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()
	msg := map[string]any{"id": id, "method": method, "sessionId": b.sessionID}
	if params != nil {
		msg["params"] = params
	}
	if err := b.conn.WriteJSON(msg); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case raw, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("browser closed")
		}
		var probe struct {
			Error string `json:"__error"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Error != "" {
			return nil, fmt.Errorf("%s: %s", method, probe.Error)
		}
		return raw, nil
	}
}

// subscribe registers a fan-out listener (buffered; slow consumers drop
// events rather than stall the browser).
func (b *Browser) subscribe() chan cdpEvent {
	ch := make(chan cdpEvent, 256)
	b.mu.Lock()
	b.eventNext++
	b.eventSubs[b.eventNext] = ch
	b.mu.Unlock()
	return ch
}

// waitEvent waits for a matching event on a dedicated subscription.
func (b *Browser) waitEvent(ctx context.Context, method string, match func(json.RawMessage) bool) (json.RawMessage, error) {
	sub := b.subscribe()
	defer func() {
		b.mu.Lock()
		for id, ch := range b.eventSubs {
			if ch == sub {
				delete(b.eventSubs, id)
			}
		}
		b.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-sub:
			if !ok {
				return nil, fmt.Errorf("browser closed")
			}
			if ev.Method == method && (match == nil || match(ev.Params)) {
				return ev.Params, nil
			}
		}
	}
}

// AttachPage creates a target and attaches a flat session to it.
func (b *Browser) AttachPage(ctx context.Context) error {
	raw, err := b.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"})
	if err != nil {
		return err
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return err
	}
	raw, err = b.call(ctx, "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true})
	if err != nil {
		return err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &attached); err != nil {
		return err
	}
	b.sessionID = attached.SessionID
	_, _ = b.call(ctx, "Page.enable", nil)
	_, _ = b.call(ctx, "Runtime.enable", nil)
	return nil
}

// SetDocumentContent loads static HTML into the page (pdf mode).
func (b *Browser) SetDocumentContent(ctx context.Context, html string) error {
	raw, err := b.call(ctx, "Page.getFrameTree", nil)
	if err != nil {
		return err
	}
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return err
	}
	_, err = b.call(ctx, "Page.setDocumentContent", map[string]any{
		"frameId": tree.FrameTree.Frame.ID, "html": html,
	})
	return err
}

// Navigate loads a URL and waits for the load event.
func (b *Browser) Navigate(ctx context.Context, url string) error {
	if _, err := b.call(ctx, "Page.navigate", map[string]any{"url": url}); err != nil {
		return err
	}
	_, err := b.waitEvent(ctx, "Page.loadEventFired", nil)
	return err
}

// Eval runs an expression (awaiting promises, returning the value) like
// page.evaluate.
func (b *Browser) Eval(ctx context.Context, expression string) (json.RawMessage, error) {
	raw, err := b.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": expression, "awaitPromise": true, "returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.ExceptionDetails) > 0 && string(out.ExceptionDetails) != "null" {
		return nil, fmt.Errorf("evaluate: %s", string(out.ExceptionDetails))
	}
	return out.Result.Value, nil
}

// PrintToPDF mirrors page.pdf(width/height px, print_background,
// prefer_css_page_size, zero margins): CDP takes INCHES.
func (b *Browser) PrintToPDF(ctx context.Context, widthPx, heightPx float64) ([]byte, error) {
	const ptPerPx = 0.75
	raw, err := b.call(ctx, "Page.printToPDF", map[string]any{
		"landscape":           false,
		"displayHeaderFooter": false,
		"printBackground":     true,
		"scale":               1,
		"paperWidth":          widthPx * ptPerPx / 72.0,
		"paperHeight":         heightPx * ptPerPx / 72.0,
		"marginTop":           0.0,
		"marginBottom":        0.0,
		"marginLeft":          0.0,
		"marginRight":         0.0,
		"preferCSSPageSize":   true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Data)
}

// Close tears the browser down.
func (b *Browser) Close() {
	if b.conn != nil {
		_ = b.conn.Close()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		_, _ = b.cmd.Process.Wait()
	}
}
