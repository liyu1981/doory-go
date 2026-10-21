// Replay: send each step's request to both sides with identical bytes on
// the wire (same client, same headers), capture responses, compare, and
// honor per-side cookie semantics (each side's jar holds only its own
// cookies — csrf secrets are per-request randomness, see compare.go).
package runner

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Run is one scenario execution context.
type Run struct {
	Repo    string
	WorkDir string
}

// Execute boots the pair, replays the scenario and reports. Returns the
// list of problems (empty = green).
func Execute(repo string, sc *Scenario) ([]string, error) {
	workDir := filepath.Join(repo, ".oracle", "parity", fmt.Sprintf("%s-%d", sc.Name, time.Now().UnixNano()))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	r := &Run{Repo: repo, WorkDir: workDir}

	oracle, goSide, err := r.bootPair(sc)
	if err != nil {
		return nil, err
	}
	defer r.shutdown(oracle, goSide)

	var problems []string
	for i, step := range sc.Steps {
		// Snapshot each side's csrf secret BEFORE the request: the client
		// jar stores the response cookie during Do, and cookie_stable must
		// compare against the pre-step value.
		preOracle := jarCSRFToken(oracle, step)
		preGo := jarCSRFToken(goSide, step)

		problemsBefore := len(problems)

		o, err := send(oracle, step)
		if err != nil {
			return nil, fmt.Errorf("oracle step %d: %w (log: %s)", i+1, err, oracle.log.Name())
		}
		g, err := send(goSide, step)
		if err != nil {
			return nil, fmt.Errorf("go step %d: %w (log: %s)", i+1, err, goSide.log.Name())
		}

		problems = append(problems, compare(i, step, o, g)...)

		if step.CookieStable {
			for _, chk := range []struct {
				side *Side
				pre  string
				cap  *capture
			}{{oracle, preOracle, o}, {goSide, preGo, g}} {
				if p := cookieStableCheck(chk.pre, chk.cap); p != "" {
					problems = append(problems, fmt.Sprintf("step %02d: %s: %s", i+1, chk.side.Name, p))
				}
			}
		}

		status := "ok"
		if len(problems) > problemsBefore {
			status = "diff"
		}
		fmt.Printf("  step %02d %-6s %-32s oracle=%d go=%d %s\n",
			i+1, step.Method, step.Path, o.Status, g.Status, status)
	}
	return problems, nil
}

// send performs one request against one side and captures the response.
func send(side *Side, step Step) (*capture, error) {
	method := step.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if step.Body != "" {
		body = bytes.NewReader([]byte(step.Body))
	}
	req, err := http.NewRequest(method, side.Base+step.Path, body)
	if err != nil {
		return nil, err
	}
	// Identical request headers on both sides (single client, deterministic
	// set): Accept like curl's default, no Accept-Encoding games.
	req.Header.Set("Accept", "*/*")
	jar := side.jar(step.Jar)
	for name, value := range step.Headers {
		req.Header.Set(name, resolveCookieTemplate(value, jar, req.URL))
	}
	client := &http.Client{
		Jar:     jar,
		Timeout: 15 * time.Second,
		// Never follow redirects: the 301 itself is the compared artifact.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	for name, values := range resp.Header {
		h[name] = append([]string(nil), values...)
	}
	return &capture{Status: resp.StatusCode, Header: h, Body: respBody, Cookies: resp.Cookies()}, nil
}

// resolveCookieTemplate substitutes {{cookie:NAME}} with the named cookie
// value from the side's jar (the SPA echoes the cookie it was given; the
// template keeps scenario steps side-agnostic).
func resolveCookieTemplate(value string, jar http.CookieJar, u *url.URL) string {
	const marker = "{{cookie:"
	for {
		start := strings.Index(value, marker)
		if start < 0 {
			return value
		}
		end := strings.Index(value[start:], "}}")
		if end < 0 {
			return value
		}
		name := value[start+len(marker) : start+end]
		resolved := ""
		for _, c := range jar.Cookies(u) {
			if c.Name == name {
				resolved = c.Value
			}
		}
		value = value[:start] + resolved + value[start+end+2:]
	}
}

// jarCSRFToken reads a side's current csrftoken secret for the step's path.
func jarCSRFToken(side *Side, step Step) string {
	u, err := url.Parse(side.Base + step.Path)
	if err != nil {
		return ""
	}
	for _, c := range side.jar(step.Jar).Cookies(u) {
		if c.Name == "csrftoken" {
			return c.Value
		}
	}
	return ""
}

// cookieStableCheck asserts the side's response renewed (rather than
// changed) its own csrf secret: Django's get_token re-sends the same cookie
// value to renew its expiry (CSRF_COOKIE_NEEDS_UPDATE).
func cookieStableCheck(pre string, cap *capture) string {
	if pre == "" {
		return "" // nothing to keep stable
	}
	for _, c := range cap.Cookies {
		if c.Name == "csrftoken" && c.Value != pre {
			return fmt.Sprintf("csrftoken changed within a stable-cookie step (had %q, response %q)", pre, c.Value)
		}
	}
	return ""
}
