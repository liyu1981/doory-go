// Buffered responses: Django/Wsgi servers assemble the whole response
// (headers + body) and send it after the middleware chain returns. net/http
// commits headers at first WriteHeader instead, which would lock headers
// before late layers (cors's Vary merge) run. This wrapper defers the send
// until the chain completes, preserving oracle header semantics.
package doory

import "net/http"

type bufferedWriter struct {
	header http.Header
	status int
	body   []byte
	wrote  bool
	out    http.ResponseWriter
}

func newBufferedWriter(out http.ResponseWriter) *bufferedWriter {
	return &bufferedWriter{header: http.Header{}, out: out}
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(status int) {
	if !b.wrote {
		b.status = status
		b.wrote = true
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

// flush sends the assembled response: headers first (order irrelevant to the
// comparator, which normalizes header order and case — recorded rules with
// reasons), then status, then body. net/http adds Date; upstream's server
// (wsgiref/gunicorn) does too — both are comparator-denied as volatile.
func (b *bufferedWriter) flush() {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	dst := b.out.Header()
	for k, vs := range b.header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	b.out.WriteHeader(b.status)
	if len(b.body) > 0 {
		_, _ = b.out.Write(b.body)
	}
}
