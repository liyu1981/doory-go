// Dev-mode proxy parity with frontend/vite.config.ts: the SPA is served by the
// Vite dev server (with HMR), and the API, media, admin, static files and the
// websocket upgrade are proxied there by Vite. The Go binary's `dev` mode does
// the mirror image — it owns the API and proxies every other path to Vite —
// so one origin (the Go listener) behaves like `vite dev` does upstream.
package doory

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// proxyToDevServer forwards a request to the Vite dev server (websocket
// upgrades included).
func proxyToDevServer(w http.ResponseWriter, r *http.Request, target string) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		http.Error(w, "invalid dev proxy target", http.StatusBadGateway)
		return
	}
	proxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			request.URL.Scheme = parsed.Scheme
			request.URL.Host = parsed.Host
			request.Host = parsed.Host
			if request.Header.Get("X-Forwarded-Proto") == "" {
				request.Header.Set("X-Forwarded-Proto", "http")
			}
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			http.Error(writer, "the Vite dev server is not reachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
