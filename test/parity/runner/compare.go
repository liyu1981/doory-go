// The comparator: exact first, with a small set of normalization rules,
// each carrying a recorded reason (PLAN.md §4 comparator ladder).
//
// Normalization rules (denylist — everything else is compared exactly):
//   - date          volatile timestamp added by both servers (rule, reason:
//     wall-clock differs between two processes)
//   - server        D10 process identity header (divergence record D10)
//   - header names  case-folded: HTTP header names are case-insensitive;
//     Django/corsheaders write some lowercase
//     (e.g. "content-length" on preflights) — reason: no
//     client can observe the case through a compliant parser
//   - vary          compared as a case-insensitive token set, comma-joined
//     (reason: patch_vary_headers order/dupe behavior is
//     join-order, not semantics; set equality is the contract)
//   - allow         compared as a case-insensitive token set: DRF's
//     @api_view builds http_method_names from a Python SET
//     (rest_framework/decorators.py:46-47), so the oracle's own
//     order varies per process (PYTHONHASHSEED): the same request
//     has produced both "GET, OPTIONS" and "OPTIONS, GET" across
//     runs — set equality is the only stable contract
//   - content-length on 204 responses (D11): wsgiref emits
//     Content-Length: 0; Go's net/http strips Content-Length from
//     bodyless statuses — a server-layer invariant not reachable
//     through Go's ResponseWriter
//   - set-cookie    parsed per cookie: name/attrs exact (expires must parse
//     and be within skew), values compared by shape rule
//     (reason: csrf secrets are per-request randomness;
//     stability across steps is asserted by cookie_stable)
//   - body          byte-exact AFTER the shape rules below; when bytes
//     still differ the report also notes structural JSON equality so
//     formatting-only drift is classified, never hidden
//   - body shape rules (each keeps its own regex as the shape assertion —
//     a side whose value violates the shape FAILS because the rule simply
//     will not match its bytes):
//     "id":<uuid>            per-row random uuid; each side mints its own
//     (reason: randomness, format still asserted)
//     "created_at":<iso-micro-z> row creation instant differs per side
//     (reason: two processes, two clocks/instants;
//     RFC3339-with-6-digits-Z shape asserted)
//     "retry_after":<int> and "Expected available in <int> seconds."
//     time-remaining until the lockout window
//     expires, computed at each side's handling
//     instant (reason: wall-clock skew between the
//     paired processes)
package runner

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"sort"
	"strings"
	"time"
)

// capture is one side's recorded response.
type capture struct {
	Status  int
	Header  http.Header
	Body    []byte
	Cookies []*http.Cookie
}

var (
	volatileHeaders = map[string]bool{"date": true, "server": true} // D10 + wall clock
	csrfShape       = regexp.MustCompile(`^[A-Za-z0-9]{32}$`)
	sessionKeyShape = regexp.MustCompile(`^[a-z0-9]{32}$`)

	// Body shape rules (documented above; each regex IS the shape assert).
	bodyShapeRules = []struct {
		re     *regexp.Regexp
		rule   string
		reason string
	}{
		{regexp.MustCompile(`"id":"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"`),
			`"id":"<uuid>"`, "per-row random uuid; each side mints its own (format asserted by the match)"},
		{regexp.MustCompile(`"created_at":"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z"`),
			`"created_at":"<iso-micro-z>"`, "row creation instant differs per side (RFC3339 micro-Z shape asserted)"},
		{regexp.MustCompile(`"updated_at":"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z"`),
			`"updated_at":"<iso-micro-z>"`, "each side stamps its own writes (RFC3339 micro-Z shape asserted)"},
		{regexp.MustCompile(`"trashed_at":"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z"`),
			`"trashed_at":"<iso-micro-z>"`, "trash instant differs per side (shape asserted; null stays literal)"},
		{regexp.MustCompile(`"folder":"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"`),
			`"folder":"<uuid>"`, "each side mints its own folder ids (dashed-uuid format asserted)"},
		{regexp.MustCompile(`"parent":"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"`),
			`"parent":"<uuid>"`, "each side mints its own folder ids (dashed-uuid format asserted)"},
		{regexp.MustCompile(`"url":"/media/assets/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.[a-z0-9]+"`),
			`"url":"<media-asset>"`, "per-side random owner/asset ids in the stored path; format asserted"},
		{regexp.MustCompile(`"thumb_url":"/media/assets/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/thumbs/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.webp"`),
			`"thumb_url":"<media-thumb>"`, "per-side random ids in the thumbnail path; thumbs/<uuid>.webp asserted"},
		{regexp.MustCompile(`"url":"/media/fonts/user/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.[a-z0-9]+"`),
			`"url":"<media-user-font>"`, "per-side random owner/variant ids in the stored path; media/fonts/user/<32hex>/<32hex>.<format> asserted"},
		{regexp.MustCompile(`https?://[A-Za-z0-9.\[\]:-]+`),
			`<origin>`, "oracle and Go serve on different ports; pagination path+query after the origin stay compared"},
		{regexp.MustCompile(`"prefix":"[0-9a-f]{12}"`),
			`"prefix":"<12hex>"`, "random per issued token; format asserted by the match"},
		{regexp.MustCompile(`"token":"doory_[0-9a-f]{12}_[A-Za-z0-9_-]{43}"`),
			`"token":"<issued-secret>"`, "shown-once secret; doory_+12hex+43urlsafe format asserted"},
		{regexp.MustCompile(`"expires_at":"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z"`),
			`"expires_at":"<iso-micro-z>"`, "computed from each side's own issue instant (RFC3339 micro-Z shape asserted)"},
		{regexp.MustCompile(`"retry_after":\d+`),
			`"retry_after":<int>`, "lockout window time-remaining computed at each side's handling instant"},
		{regexp.MustCompile(`Expected available in \d+ seconds\.`),
			`Expected available in <int> seconds.`, "same time-remaining as retry_after (DRF wait sentence)"},
	}
)

// normalizeBody applies the recorded shape rules (reasons above). A value
// that violates a shape stays literal and therefore fails the byte compare.
func normalizeBody(b []byte) []byte {
	out := string(b)
	for _, rule := range bodyShapeRules {
		out = rule.re.ReplaceAllString(out, rule.rule)
	}
	return []byte(out)
}

// compare returns human-readable differences between the two captures.
func compare(stepIdx int, step Step, oracle, goSide *capture) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("step %02d: %s", stepIdx+1, fmt.Sprintf(format, args...)))
	}

	if oracle.Status != goSide.Status {
		add("status: oracle=%d go=%d", oracle.Status, goSide.Status)
	}

	// Headers: canonical names, deny volatile, exact values otherwise.
	oh := canonHeaders(oracle.Header)
	gh := canonHeaders(goSide.Header)
	for name := range oh {
		if volatileHeaders[name] {
			delete(oh, name)
		}
	}
	for name := range gh {
		if volatileHeaders[name] {
			delete(gh, name)
		}
	}

	// D11: bodyless statuses — wsgiref sends Content-Length: 0 on 204;
	// Go's net/http strips Content-Length from bodyless statuses (the
	// comparison below would otherwise flag it as oracle-only).
	if oracle.Status == 204 || goSide.Status == 204 {
		delete(oh, "content-length")
		delete(gh, "content-length")
	}
	names := map[string]bool{}
	for n := range oh {
		names[n] = true
	}
	for n := range gh {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		ov, oOK := oh[name]
		gv, gOK := gh[name]
		if !oOK {
			add("header %q only on go side: %q", name, gv)
			continue
		}
		if !gOK {
			add("header %q only on oracle side: %q", name, ov)
			continue
		}
		if name == "set-cookie" {
			if p := compareCookies(ov, gv); p != nil {
				for _, s := range p {
					add("set-cookie: %s", s)
				}
			}
			continue
		}
		if name == "vary" || name == "allow" {
			if !equalTokens(ov[0], gv[0]) {
				add("%s: oracle=%q go=%q", name, ov[0], gv[0])
			}
			continue
		}
		if strings.Join(ov, "\x00") != strings.Join(gv, "\x00") {
			add("header %q: oracle=%q go=%q", name, ov, gv)
		}
	}

	// Body: byte-exact after the recorded shape rules.
	ob := normalizeBody(oracle.Body)
	gb := normalizeBody(goSide.Body)
	if string(ob) != string(gb) {
		kind := "binary/bytes"
		if oj, gj := jsonOK(oracle.Body), jsonOK(goSide.Body); oj && gj {
			kind = "structurally-equal JSON (formatting-only drift)" + structuralNote(oracle.Body, goSide.Body)
		}
		add("body differs (%s):\n      first-diff: %s\n      oracle: %s\n      go:    %s",
			kind, firstDiff(ob, gb), abbrev(oracle.Body), abbrev(goSide.Body))
	}
	return problems
}

// compareHeadersOnly compares status + headers but not the body (D12:
// non-/api/ error pages are Django templates on one side and Go defaults on
// the other; headers INCLUDING the media CSP keep being compared).
func compareHeadersOnly(stepIdx int, step Step, oracle, goSide *capture) []string {
	full := compare(stepIdx, step, oracle, &capture{Status: goSide.Status, Header: goSide.Header})
	// compare() also flags body diffs: filter them out and mark the skip.
	// Content-Type/Content-Length DESCRIBE the skipped body, so they leave
	// with it; every other header (media CSP, nosniff, security stack)
	// keeps comparing byte-for-byte.
	var kept []string
	for _, p := range full {
		if contains(p, "body differs") {
			// informational only: printed by the runner, never a failure
			continue
		}
		if contains(p, `"content-length"`) || contains(p, `"content-type"`) {
			continue
		}
		kept = append(kept, p)
	}
	// de-dup the skip note
	seen := map[string]bool{}
	var out []string
	for _, p := range kept {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// firstDiff reports the common-prefix length and shows both sides' context
// around the first difference (after normalization).
func firstDiff(a, b []byte) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	ctx := func(x []byte) string {
		lo, hi := i-40, i+40
		if lo < 0 {
			lo = 0
		}
		if hi > len(x) {
			hi = len(x)
		}
		return string(x[lo:hi])
	}
	return fmt.Sprintf("at byte %d (len a=%d b=%d)\n        a: ...%s...\n        b: ...%s...",
		i, len(a), len(b), ctx(a), ctx(b))
}

// canonHeaders lowercases names into a map of value-lists (multi-value
// preserved in order; set-cookie kept as full strings for cookie parsing).
func canonHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	for name, values := range h {
		out[strings.ToLower(name)] = append(out[strings.ToLower(name)], values...)
	}
	return out
}

// compareCookies compares cookie lists by name and attributes; values use
// shape rules (randomness), expires must parse and sit within skew.
func compareCookies(oVals, gVals []string) []string {
	var problems []string
	oMap := map[string]*http.Cookie{}
	for _, c := range parseCookies(oVals) {
		oMap[c.Name] = c
	}
	gMap := map[string]*http.Cookie{}
	for _, c := range parseCookies(gVals) {
		gMap[c.Name] = c
	}
	if len(oMap) != len(gMap) {
		return []string{fmt.Sprintf("cookie count oracle=%d go=%d", len(oMap), len(gMap))}
	}
	for name, oc := range oMap {
		gc, ok := gMap[name]
		if !ok {
			problems = append(problems, "cookie "+name+" only on oracle side")
			continue
		}
		if oc.Path != gc.Path {
			problems = append(problems, fmt.Sprintf("%s path: oracle=%q go=%q", name, oc.Path, gc.Path))
		}
		if oc.MaxAge != gc.MaxAge {
			problems = append(problems, fmt.Sprintf("%s max-age: oracle=%d go=%d", name, oc.MaxAge, gc.MaxAge))
		}
		if oc.SameSite != gc.SameSite {
			problems = append(problems, fmt.Sprintf("%s samesite: oracle=%v go=%v", name, oc.SameSite, gc.SameSite))
		}
		if oc.Secure != gc.Secure || oc.HttpOnly != gc.HttpOnly {
			problems = append(problems, fmt.Sprintf("%s secure/httponly: oracle=%v/%v go=%v/%v",
				name, oc.Secure, oc.HttpOnly, gc.Secure, gc.HttpOnly))
		}
		// Expires: parse + closeness (both compute now+max-age).
		if oc.Expires.IsZero() != gc.Expires.IsZero() {
			problems = append(problems, fmt.Sprintf("%s expires presence: oracle=%v go=%v", name, !oc.Expires.IsZero(), !gc.Expires.IsZero()))
		} else if !oc.Expires.IsZero() {
			if d := oc.Expires.Sub(gc.Expires); d > 10*time.Second || d < -10*time.Second {
				problems = append(problems, fmt.Sprintf("%s expires skew >10s: oracle=%v go=%v", name, oc.Expires, gc.Expires))
			}
		}
		// Value: shape rules (recorded reasons: per-request secrets).
		switch name {
		case "sessionid":
			if oc.Value != "" && !sessionKeyShape.MatchString(oc.Value) {
				problems = append(problems, fmt.Sprintf("sessionid value shape: oracle %q not [a-z0-9]{32}", oc.Value))
			}
			if gc.Value != "" && !sessionKeyShape.MatchString(gc.Value) {
				problems = append(problems, fmt.Sprintf("sessionid value shape: go %q not [a-z0-9]{32}", gc.Value))
			}
		case "csrftoken":
			if !csrfShape.MatchString(oc.Value) {
				problems = append(problems, fmt.Sprintf("csrftoken value shape: oracle %q not 32 alnum", oc.Value))
			}
			if !csrfShape.MatchString(gc.Value) {
				problems = append(problems, fmt.Sprintf("csrftoken value shape: go %q not 32 alnum", gc.Value))
			}
		default:
			if oc.Value != gc.Value {
				problems = append(problems, fmt.Sprintf("%s value: oracle=%q go=%q (no shape rule for this cookie)", name, oc.Value, gc.Value))
			}
		}
	}
	return problems
}

func parseCookies(vals []string) []*http.Cookie {
	var out []*http.Cookie
	for _, v := range vals {
		if c, err := http.ParseSetCookie(v); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func equalTokens(a, b string) bool {
	set := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, t := range strings.Split(s, ",") {
			m[strings.ToLower(strings.TrimSpace(t))] = true
		}
		return m
	}
	ta, tb := set(a), set(b)
	if len(ta) != len(tb) {
		return false
	}
	for t := range ta {
		if !tb[t] {
			return false
		}
	}
	return true
}

func jsonOK(b []byte) bool {
	return json.Valid(b) && len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

// structuralNote pinpoints the first structural difference when both sides
// are valid JSON (used only to classify a byte-level failure, never to pass).
func structuralNote(a, b []byte) string {
	var xa, xb any
	if json.Unmarshal(a, &xa) != nil || json.Unmarshal(b, &xb) != nil {
		return ""
	}
	ja, _ := json.Marshal(xa)
	jb, _ := json.Marshal(xb)
	if string(ja) == string(jb) {
		return ""
	}
	sa, sb := sha256.Sum256(ja), sha256.Sum256(jb)
	return fmt.Sprintf(" (normalized sha oracle=%x go=%x)", sa[:6], sb[:6])
}

func abbrev(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		return s[:300] + fmt.Sprintf("...(%d bytes)", len(b))
	}
	return fmt.Sprintf("%q", s)
}

// newJar builds a per-side cookie jar (each side keeps its own cookies).
func newJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return jar
}
