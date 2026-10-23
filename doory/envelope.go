// Error envelope rendering: two exact JSON dialects, both probe-proven.
//
// Ports backend/apps/core/exceptions.py (DRF renderer, compact) and
// backend/apps/core/views.py handler404/handler500 (Django JsonResponse,
// which uses json.dumps' default separators: ", " and ": " after the key).
package doory

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
)

// logerr records the underlying error behind a 500 (parity runs must show
// why; never swallow).
func logerr(err error, where string) {
	if err != nil {
		_, _ = os.Stderr.WriteString("doory: " + where + ": " + err.Error() + "\n")
	}
}

// ErrorEnvelope is the uniform error body:
// {"error": {"code", "message", "fields"?, "field_codes"?, "retry_after"?}}.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code       string              `json:"code"`
	Message    string              `json:"message"`
	Fields     map[string][]string `json:"fields,omitempty"`
	FieldCodes map[string][]string `json:"field_codes,omitempty"`
	RetryAfter *int                `json:"retry_after,omitempty"`
}

// encodeCompactJSON renders like Python json.dumps with compact separators
// and ensure_ascii-style handling of the fields we emit: no HTML escaping
// (Python never escapes <,>,&) and no trailing newline.
func encodeCompactJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	return bytes.TrimRight(b, "\n")
}

// writeDRF renders a DRF-style error response: compact envelope, as
// api_exception_handler produces. Evidence: probe 0001 steps 05 (405) and
// 06 (401).
func writeDRF(w http.ResponseWriter, status int, code, message string) {
	if status >= 500 {
		// Never swallow a server-side failure: parity runs must show why.
		_, _ = os.Stderr.WriteString("doory: server_error: " + message + "\n")
	}
	body := encodeCompactJSON(ErrorEnvelope{Error: ErrorBody{Code: code, Message: message}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeDjangoJSON renders like Django JsonResponse: json.dumps default
// separators (", ", ": "), ensure_ascii semantics for these ASCII messages.
// Evidence: probe 0001 step 04 — {"error": {"code": "not_found", ...}}.
func writeDjangoJSON(w http.ResponseWriter, status int, code, message string) {
	// Fixed structure; message strings are JSON-string-escaped by json.Marshal
	// (identical to Python for ASCII; dynamic non-ASCII messages are not yet
	// reachable — record a probe before extending, see PLAN S4).
	msg, _ := json.Marshal(message)
	body := []byte(`{"error": {"code": "` + code + `", "message": ` + string(msg) + `}}`)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
