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
	"regexp"

	"doory-go/internal/pdfscan"
	"strings"
	"time"
)

// comparePDF compares a PDF export: status + headers (minus
// content-length — chromium embeds per-render ids/times) + the pdfscan
// canonical structure instead of bytes (page count, boxes, catalog keys).
func comparePDF(stepIdx int, step Step, oracle, goSide *capture) []string {
	var problems []string
	if oracle.Status != goSide.Status {
		problems = append(problems, fmt.Sprintf("step %02d: status: oracle=%d go=%d", stepIdx+1, oracle.Status, goSide.Status))
	}
	oh := canonHeaders(oracle.Header)
	gh := canonHeaders(goSide.Header)
	delete(oh, "content-length")
	delete(gh, "content-length")
	for k := range oh {
		if volatileHeaders[k] {
			delete(oh, k)
		}
	}
	for k := range gh {
		if volatileHeaders[k] {
			delete(gh, k)
		}
	}
	for name, ov := range oh {
		if volatileHeaders[name] {
			continue
		}
		gv, ok := gh[name]
		if !ok || strings.Join(ov, "\x00") != strings.Join(gv, "\x00") {
			problems = append(problems, fmt.Sprintf("step %02d: header %q: oracle=%v go=%v", stepIdx+1, name, ov, gv))
		}
	}
	for name := range gh {
		if _, ok := oh[name]; !ok && !volatileHeaders[name] {
			problems = append(problems, fmt.Sprintf("step %02d: header %q only on go side: %v", stepIdx+1, name, gh[name]))
		}
	}
	// body: canonical structure
	if oracle.Status == 200 && goSide.Status == 200 {
		oStruct, oErr := pdfscan.Scan(oracle.Body)
		gStruct, gErr := pdfscan.Scan(goSide.Body)
		if oErr != nil || gErr != nil {
			problems = append(problems, fmt.Sprintf("step %02d: pdf parse: oracle=%v go=%v", stepIdx+1, oErr, gErr))
		} else if oStruct.Canonical() != gStruct.Canonical() {
			problems = append(problems, fmt.Sprintf("step %02d: pdf structure differs:\n      oracle: %s\n      go:    %s",
				stepIdx+1, oStruct.Canonical(), gStruct.Canonical()))
		}
	}
	return problems
}

// RepoRoot is the repo base for relative file paths (set by Execute).
var RepoRoot string

// Run is one scenario execution context.
type Run struct {
	Repo    string
	WorkDir string
}

// Execute boots the pair, replays the scenario and reports. Returns the
// list of problems (empty = green).
func Execute(repo string, sc *Scenario, renderURL string) ([]string, error) {
	workDir := filepath.Join(repo, ".oracle", "parity", fmt.Sprintf("%s-%d", sc.Name, time.Now().UnixNano()))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	r := &Run{Repo: repo, WorkDir: workDir}
	RepoRoot = repo

	oracle, goSide, err := r.bootPair(sc, renderURL)
	if err != nil {
		return nil, err
	}
	defer r.shutdown(oracle, goSide)

	var problems []string
	captures := map[string]map[string]string{"oracle": {}, "go": {}}
	vars := map[string]string{}
	for i, step := range sc.Steps {
		// Snapshot each side's csrf secret BEFORE the request: the client
		// jar stores the response cookie during Do, and cookie_stable must
		// compare against the pre-step value.
		preOracle := jarCSRFToken(oracle, step)
		preGo := jarCSRFToken(goSide, step)

		problemsBefore := len(problems)

		var stepProblems []string
		status := "ok"
		wsStep := step.Ws
		if wsStep == nil && i == len(sc.Steps)-1 {
			// a scenario-level [ws] table is the trailing WS interaction
			wsStep = sc.Ws
		}
		if wsStep != nil {
			stepProblems = runWsStep(i, oracle, goSide, *wsStep, vars, captures)
			if len(stepProblems) > 0 {
				status = "diff"
			}
			fmt.Printf("  step %02d WS      %-32s %s\n", i+1, wsStep.DesignID, status)
		} else {
			o, err := send(oracle, step, captures["oracle"])
			if err != nil {
				return nil, fmt.Errorf("oracle step %d: %w (log: %s)", i+1, err, oracle.log.Name())
			}
			g, err := send(goSide, step, captures["go"])
			if err != nil {
				return nil, fmt.Errorf("go step %d: %w (log: %s)", i+1, err, goSide.log.Name())
			}
			applyCaptures(step, captures["oracle"], o.Body)
			applyCaptures(step, captures["go"], g.Body)
			for _, m := range []map[string]string{captures["oracle"], captures["go"]} {
				for k, v := range m {
					if _, ok := vars[k]; !ok {
						vars[k] = v
					}
				}
			}
			if len(step.Capture) > 0 {
				fmt.Printf("      capture oracle=%v go=%v\n", captures["oracle"], captures["go"])
			}

			if step.Comparator == "pdf" {
				problems = append(problems, comparePDF(i, step, o, g)...)
			} else if step.HeadersOnly {
				if len(o.Body) != len(g.Body) || string(o.Body) != string(g.Body) {
					fmt.Printf("      step %02d: body skipped (D12: non-API error pages differ by design)\n", i+1)
				}
				problems = append(problems, compareHeadersOnly(i, step, o, g)...)
			} else {
				problems = append(problems, compare(i, step, o, g)...)
			}

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

			if len(problems) > problemsBefore {
				status = "diff"
			}
			fmt.Printf("  step %02d %-6s %-32s oracle=%d go=%d %s\n",
				i+1, step.Method, step.Path, o.Status, g.Status, status)
		}
		if len(stepProblems) > 0 {
			problems = append(problems, stepProblems...)
		}
	}
	return problems, nil
}

// applyCaptures extracts named groups from this side's response body.
func applyCaptures(step Step, vars map[string]string, body []byte) {
	for name, pattern := range step.Capture {
		re := regexp.MustCompile(pattern)
		if m := re.FindSubmatch(body); len(m) > 1 {
			vars[name] = string(m[1])
		}
	}
}

// resolveCaptures substitutes {{capture:name}} from this side's vars.
func resolveCaptures(s string, vars map[string]string) string {
	const marker = "{{capture:"
	for {
		start := strings.Index(s, marker)
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "}}")
		if end < 0 {
			return s
		}
		name := s[start+len(marker) : start+end]
		s = s[:start] + vars[name] + s[start+end+2:]
	}
}

// send performs one request against one side and captures the response.
func send(side *Side, step Step, vars map[string]string) (*capture, error) {
	method := step.Method
	if method == "" {
		method = http.MethodGet
	}
	reqPath := resolveCaptures(step.Path, vars)
	var body io.Reader
	if step.Body != "" {
		body = bytes.NewReader([]byte(resolveCaptures(step.Body, vars)))
	}
	var req *http.Request
	var err error
	if len(step.Multipart) > 0 {
		raw, contentType, merr := buildMultipart(step.Multipart, RepoRoot)
		if merr != nil {
			return nil, merr
		}
		req, err = http.NewRequest(method, side.Base+reqPath, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
	} else {
		req, err = http.NewRequest(method, side.Base+reqPath, body)
		if err != nil {
			return nil, err
		}
	}
	// Identical request headers on both sides (single client, deterministic
	// set): Accept like curl's default, no Accept-Encoding games.
	req.Header.Set("Accept", "*/*")
	jar := side.jar(step.Jar)
	for name, value := range step.Headers {
		req.Header.Set(name, resolveCookieTemplate(resolveCaptures(value, vars), jar, req.URL))
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

// runWsStep replays one WebSocket interaction (dial/send/expect/close) on
// both sides of the pair and reports expectation problems.
func runWsStep(i int, oracle, goSide *Side, step WsStep, vars map[string]string, captures map[string]map[string]string) []string {
	var problems []string
	designID := resolveCaptures(step.DesignID, vars)
	if designID == "" {
		return []string{fmt.Sprintf("step %02d: ws: missing design_id", i+1)}
	}
	for _, pair := range []struct {
		name string
		side *Side
	}{{"oracle", oracle}, {"go", goSide}} {
		s, err := dialWs(pair.side, designID, step.Jar)
		if err != nil {
			problems = append(problems, fmt.Sprintf("step %02d: ws(%s): dial: %v", i+1, pair.name, err))
			continue
		}
		switch step.Action {
		case "connect":
			if _, err := s.waitForType(step.Expect, 10*time.Second); err != nil {
				problems = append(problems, fmt.Sprintf("step %02d: ws(%s) connect: %v", i+1, pair.name, err))
			}
			if step.Send != "" {
				if err := s.send(step.Send); err != nil {
					problems = append(problems, fmt.Sprintf("step %02d: ws(%s) send: %v", i+1, pair.name, err))
				}
				if step.ExpectAfter != "" {
					if _, err := s.waitForType(step.ExpectAfter, 10*time.Second); err != nil {
						problems = append(problems, fmt.Sprintf("step %02d: ws(%s) expect: %v", i+1, pair.name, err))
					}
				}
			}
			if step.CloseCode > 0 || step.CloseReason != "" {
				if err := s.close(step.CloseCode, step.CloseReason); err != nil {
					problems = append(problems, fmt.Sprintf("step %02d: ws(%s) close: %v", i+1, pair.name, err))
				}
			}
		case "send":
			if err := s.send(step.Send); err != nil {
				problems = append(problems, fmt.Sprintf("step %02d: ws(%s) send: %v", i+1, pair.name, err))
			} else if step.ExpectAfter != "" {
				if _, err := s.waitForType(step.ExpectAfter, 10*time.Second); err != nil {
					problems = append(problems, fmt.Sprintf("step %02d: ws(%s) expect: %v", i+1, pair.name, err))
				}
			}
		case "close":
			if err := s.close(step.CloseCode, step.CloseReason); err != nil {
				problems = append(problems, fmt.Sprintf("step %02d: ws(%s) close: %v", i+1, pair.name, err))
			}
		default:
			problems = append(problems, fmt.Sprintf("step %02d: ws(%s): unknown action %q", i+1, pair.name, step.Action))
		}
		s.closeIfNeeded()
	}
	return problems
}
