// Network policy for the print browser — Go port of apps/exports/policy.py.
//
// Chromium may load data: URLs and plain GETs from a short origin/prefix
// list (this site's /media/ and /static/); everything else is blocked and
// counted. The image export adds its render page's origin and bundle.
//
// Ports backend/apps/exports/policy.py
package renderpolicy

import (
	"fmt"
	"net/url"
	"strings"
)

var defaultPorts = map[string]int{"http": 80, "https": 443}

// Decision mirrors the Python dataclass.
type Decision struct {
	Allowed bool
	Reason  string
	Fetch   bool // the worker must fetch it itself (no redirects)
}

// Policy mirrors RequestPolicy.
type Policy struct {
	allowedOrigins map[string]bool
	pathPrefixes   []string
	pageOrigin     string
	pagePath       string
	pagePrefixes   []string
}

// NormalizeOrigin ports normalize_origin: "scheme://host:port" with default
// ports spelled out, or false when value is not an http(s) origin.
func NormalizeOrigin(value string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	if parsed.Hostname() == "" {
		return "", false
	}
	port := parsed.Port()
	if port == "" {
		port = fmt.Sprintf("%d", defaultPorts[parsed.Scheme])
	}
	return parsed.Scheme + "://" + strings.ToLower(parsed.Hostname()) + ":" + port, true
}

// Build ports RequestPolicy.build (origins + path prefixes; the image mode
// adds its render page).
func Build(origins []string, prefixes []string, pageURL string) (*Policy, error) {
	p := &Policy{allowedOrigins: map[string]bool{}}
	for _, o := range origins {
		if norm, ok := NormalizeOrigin(o); ok {
			p.allowedOrigins[norm] = true
		}
	}
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		p.pathPrefixes = append(p.pathPrefixes, prefix)
	}
	if pageURL == "" {
		return p, nil
	}
	pageOrigin, ok := NormalizeOrigin(pageURL)
	parsed, err := url.Parse(pageURL)
	if err != nil || !ok || !strings.HasSuffix(parsed.Path, ".html") {
		return nil, fmt.Errorf("not a render page URL: %q", pageURL)
	}
	p.allowedOrigins[pageOrigin] = true
	p.pageOrigin = pageOrigin
	p.pagePath = parsed.Path
	p.pagePrefixes = []string{"/assets/"}
	return p, nil
}

// Evaluate ports RequestPolicy.evaluate.
func (p *Policy) Evaluate(rawURL, method string) Decision {
	if strings.HasPrefix(rawURL, "data:") {
		return Decision{Allowed: true, Reason: "data-url"}
	}
	if rawURL == "about:blank" {
		return Decision{Allowed: true, Reason: "blank"}
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return Decision{Reason: "malformed-url"}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return Decision{Reason: "scheme-" + orNone(parsed.Scheme)}
	}
	if strings.ToUpper(method) != "GET" {
		return Decision{Reason: "method"}
	}
	if parsed.User != nil {
		return Decision{Reason: "credentials-in-url"}
	}
	origin, ok := NormalizeOrigin(rawURL)
	if !ok || !p.allowedOrigins[origin] {
		return Decision{Reason: "origin"}
	}
	path := parsed.Path
	if strings.Contains(path, "\\") || strings.Contains(path, "\x00") {
		return Decision{Reason: "path-traversal"}
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return Decision{Reason: "path-traversal"}
		}
	}
	for _, prefix := range p.pathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return Decision{Allowed: true, Reason: "same-origin", Fetch: true}
		}
	}
	if origin == p.pageOrigin && (path == p.pagePath || hasPrefixIn(path, p.pagePrefixes)) {
		return Decision{Allowed: true, Reason: "render-page", Fetch: true}
	}
	return Decision{Reason: "path"}
}

func hasPrefixIn(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
