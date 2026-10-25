// gapscheck keeps docs/parity/GAPS.md honest about routes: every API route in
// the pinned routes ledger must either be registered by the port (a
// `Path: "..."` in doory/app.go, or a handler route list elsewhere) or be
// listed in GAPS.md. It also checks that GAPS.md cites only DIVERGENCES ids
// that exist.
//
// It deliberately does NOT check behaviour: a registered route with an
// unported branch is found by the `route registered but not ported:` lines,
// which the author must also list in GAPS.md section B (checked by requiring
// each such message's route to appear in GAPS.md as well).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type ledgerRoute struct {
	Route string `json:"route"`
}

type ledger struct {
	HTTP []ledgerRoute `json:"http"`
}

var (
	djangoParam = regexp.MustCompile(`\(\?P<\w+>\[[^\]]*\][^)]*\)|<[^>]+>`)
	pathLiteral = regexp.MustCompile(`Path:\s*"([^"]+)"`)
	divergence  = regexp.MustCompile(`\bD[0-9]{1,2}\b`)
)

func normalize(raw string) string {
	p := strings.TrimSpace(raw)
	// The ledger keeps Django's anchors anywhere in the pattern
	// ("api/v1/^assets/$"); they carry no routing information here.
	p = strings.ReplaceAll(p, "^", "")
	p = strings.ReplaceAll(p, "$", "")
	p = djangoParam.ReplaceAllString(p, "{}")
	p = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(p, "{}")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		p = "/"
	}
	return p
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	fail := 0

	// The newest routes ledger (the pin in internal/dooryversion).
	matches, _ := filepath.Glob(filepath.Join(root, "ledger", "routes-v*.json"))
	if len(matches) == 0 {
		fmt.Fprintln(os.Stderr, "gapscheck: no ledger/routes-v*.json found (run pnpm generate)")
		os.Exit(1)
	}
	sort.Strings(matches)
	raw, err := os.ReadFile(matches[len(matches)-1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "gapscheck: %v\n", err)
		os.Exit(1)
	}
	var doc ledger
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Fprintf(os.Stderr, "gapscheck: %v\n", err)
		os.Exit(1)
	}

	appSource, err := os.ReadFile(filepath.Join(root, "doory", "app.go"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gapscheck: %v\n", err)
		os.Exit(1)
	}
	registered := map[string]bool{}
	for _, m := range pathLiteral.FindAllStringSubmatch(string(appSource), -1) {
		registered[normalize(m[1])] = true
	}

	gapsDoc, err := os.ReadFile(filepath.Join(root, "docs", "parity", "GAPS.md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gapscheck: %v\n", err)
		os.Exit(1)
	}
	gaps := string(gapsDoc)
	gapsNormalized := normalizeGapsText(gaps)

	missing := []string{}
	for _, route := range doc.HTTP {
		path := "/" + route.Route
		if strings.HasPrefix(route.Route, "admin") || !strings.HasPrefix(path, "/api/") {
			continue
		}
		key := normalize(path)
		if registered[key] || gapsNormalized[key] {
			continue
		}
		missing = append(missing, key)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		fmt.Fprintln(os.Stderr, "gapscheck: ledger routes neither registered nor listed in docs/parity/GAPS.md:")
		for _, path := range missing {
			fmt.Fprintf(os.Stderr, "  %s\n", path)
		}
		fail = 1
	}

	// Every "route registered but not ported" message must be tracked too.
	notPorted := regexp.MustCompile(`route registered but not ported: ([^\n"]+)`)
	goFiles, _ := filepath.Glob(filepath.Join(root, "doory", "*.go"))
	tracked := strings.ToLower(gaps)
	untracked := map[string]bool{}
	for _, file := range goFiles {
		body, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, m := range notPorted.FindAllStringSubmatch(string(body), -1) {
			branch := strings.TrimSpace(m[1])
			// GAPS.md rows name the upstream function (e.g. "oidc.begin"), so
			// the first token of the message is the lookup key.
			token := strings.FieldsFunc(branch, func(r rune) bool {
				return r == ' ' || r == '(' || r == ','
			})
			if len(token) == 0 {
				continue
			}
			name := token[0]
			if dot := strings.LastIndex(name, "."); dot >= 0 {
				name = name[dot+1:]
			}
			if !strings.Contains(tracked, strings.ToLower(name)) {
				untracked[token[0]] = true
			}
		}
	}
	if len(untracked) > 0 {
		names := make([]string, 0, len(untracked))
		for name := range untracked {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintln(os.Stderr, "gapscheck: notPorted branches missing from GAPS.md section B:")
		for _, name := range names {
			fmt.Fprintf(os.Stderr, "  %s\n", name)
		}
		fail = 1
	}

	// Divergece ids cited by GAPS.md must exist in the ledger.
	ids := map[string]bool{}
	divFile, _ := os.ReadFile(filepath.Join(root, "docs", "parity", "DIVERGENCE-IDS.txt"))
	for _, line := range strings.Fields(string(divFile)) {
		ids[line] = true
	}
	for _, id := range divergence.FindAllString(gaps, -1) {
		if !ids[id] {
			fmt.Fprintf(os.Stderr, "gapscheck: GAPS.md cites %s, which is not in DIVERGENCE-IDS.txt\n", id)
			fail = 1
		}
	}

	if fail == 0 {
		fmt.Printf("gapscheck: ok (%d ledger api routes, %d registered, GAPS.md in sync)\n",
			countAPIRoutes(doc), len(registered))
	}
	os.Exit(fail)
}

func isMethod(word string) bool {
	switch word {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD":
		return true
	}
	return false
}

func countAPIRoutes(doc ledger) int {
	n := 0
	for _, route := range doc.HTTP {
		if strings.HasPrefix(route.Route, "admin") {
			continue
		}
		if strings.HasPrefix("/"+route.Route, "/api/") {
			n++
		}
	}
	return n
}

// normalizeGapsText turns route mentions in the markdown (backticked, with
// `{id}`/`{slug}`/`{uuid}` placeholders) into the same normalized keys.
func normalizeGapsText(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(text, -1) {
		// A cell may hold several routes, each optionally prefixed with its
		// method ("GET /api/…", "POST /api/…").
		for _, piece := range strings.Split(m[1], ",") {
			piece = strings.TrimSpace(piece)
			fields := strings.Fields(piece)
			switch {
			case len(fields) == 2 && isMethod(fields[0]):
				piece = fields[1]
			case len(fields) == 1:
				piece = fields[0]
			default:
				continue
			}
			if !strings.HasPrefix(piece, "/api/") {
				continue
			}
			out[normalize(piece)] = true
		}
	}
	return out
}
