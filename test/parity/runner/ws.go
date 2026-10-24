// The WebSocket scenario driver: dials the collab realtime socket
// (ws/designs/<uuid>/) on a booted side and replays dial/send/expect/close
// interactions. The frame contract mirrors probe0012-collab (owner
// connect -> ready -> welcome sync/step1, ping -> pong, client sync ->
// server sync/step2, close codes) and probe0014-collab-share (link logins,
// pending auth timeout, trashed design, origin refusal, frame/rate limits).

package runner

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"doory-go/internal/crdt"
	"doory-go/internal/yproto"
)

// WsStep is one WebSocket interaction inside a scenario step:
//   - action: "connect" | "send" | "close" | "expect"
//   - design_id: design uuid template (captures resolved through the step
//     variables, e.g. "{{capture:did}}")
//   - jar: cookie jar name (default "main") — an unauthenticated connection
//     uses a jar that never logged in
//   - origin: overrides the Origin header (foreign-origin handshake checks)
//   - expect_dial_status: the handshake must be REFUSED with this HTTP status
//     (403 for a cross-site Origin; the dial fails, which is the assertion)
//   - expect: text frame type to wait for after dialing ("ready", "pong",
//     "error"); expect_code asserts the JSON "code" field on that frame
//   - expect_role / expect_can_write: assertions on the ready frame
//   - send: frame payload — JSON text ({"type":"ping"}) or binary as hex
//   - expect_after / expect_after_code / expect_after_role /
//     expect_after_can_write: the text frame (usually ready) after sending
//   - expect_binary: hex of the binary frame to compare byte-exactly
//   - expect_binary_kind: "sync/step1" | "sync/step2" | "awareness" — asserts
//     the binary frame's y-protocol message/sync kind
//   - expect_after_binary / expect_after_document: the same for a binary
//     frame received AFTER `send` (e.g. the sync/step2 answer to a client
//     sync/step1)
//   - expect_document: "sync/step2" — decode that update into a fresh engine
//     document and compare the canonical document JSON ACROSS SIDES (uuid
//     values normalized: page/object ids are per-design randomness)
//   - close_code / close_reason: close the socket with these (client side)
//   - expect_close_code: the SERVER closes with this code (4400/4401/4403/
//     4404/4408/4413/4429); expect_close_reason asserts the reason text
type WsStep struct {
	Action            string   `toml:"action"`
	Session           string   `toml:"session"` // named socket kept across steps (A/B peers)
	DesignID          string   `toml:"design_id"`
	Jar               string   `toml:"jar"`
	Origin            string   `toml:"origin"`
	ExpectDialStatus  int      `toml:"expect_dial_status"`
	Expect            string   `toml:"expect"`
	ExpectCode        string   `toml:"expect_code"`
	ExpectRole        string   `toml:"expect_role"`
	ExpectCanWrite    *bool    `toml:"expect_can_write"`
	Send              string   `toml:"send"`
	Sends             []string `toml:"sends"` // several frames (rate-limit probes)
	ExpectAfter       string   `toml:"expect_after"`
	ExpectAfterCode   string   `toml:"expect_after_code"`
	ExpectAfterRole   string   `toml:"expect_after_role"`
	ExpectAfterWrite  *bool    `toml:"expect_after_can_write"`
	ExpectBinary      string   `toml:"expect_binary"`
	ExpectBinaryKind  string   `toml:"expect_binary_kind"` // binary frame BEFORE send
	ExpectDocument    string   `toml:"expect_document"`
	ExpectAfterBinary string   `toml:"expect_after_binary"`   // binary frame AFTER send
	ExpectAfterDoc    string   `toml:"expect_after_document"` // decoded document of that frame
	CloseCode         int      `toml:"close_code"`
	CloseReason       string   `toml:"close_reason"`
	ExpectCloseCode   int      `toml:"expect_close_code"`
	ExpectCloseReason string   `toml:"expect_close_reason"`
}

// wsSessionStore keeps named sockets alive ACROSS steps of one scenario, so a
// second connection (or an HTTP write) can be observed by a socket that is
// already joined: session name -> side name -> session.
type wsSessionStore struct {
	m map[string]map[string]*wsSession
}

func newWsSessionStore() *wsSessionStore {
	return &wsSessionStore{m: map[string]map[string]*wsSession{}}
}

func (st *wsSessionStore) get(name, side string) *wsSession {
	if st == nil || name == "" {
		return nil
	}
	return st.m[name][side]
}

func (st *wsSessionStore) set(name, side string, s *wsSession) {
	if st == nil || name == "" {
		return
	}
	if st.m[name] == nil {
		st.m[name] = map[string]*wsSession{}
	}
	st.m[name][side] = s
}

func (st *wsSessionStore) drop(name, side string) {
	if st == nil || name == "" {
		return
	}
	if byside := st.m[name]; byside != nil {
		if s := byside[side]; s != nil {
			s.closeIfNeeded()
			delete(byside, side)
		}
	}
}

func (st *wsSessionStore) closeAll() {
	if st == nil {
		return
	}
	for _, byside := range st.m {
		for _, s := range byside {
			s.closeIfNeeded()
		}
	}
	st.m = map[string]map[string]*wsSession{}
}

// wsFrame is one received WebSocket frame.
type wsFrame struct {
	Kind int    // websocket.TextMessage / BinaryMessage
	Text string // text frames only
	Data []byte // binary frames (and text bytes)
}

// wsSession is one dialed collab socket with a buffered inbound queue.
type wsSession struct {
	conn      *websocket.Conn
	frames    chan wsFrame
	backlog   []wsFrame // frames a previous expectation looked past (order kept)
	cancel    func()
	closeCode int    // code of the SERVER-initiated close (0 = none/clean)
	closeText string // reason text of that close
	closeErr  error
}

// next returns the oldest unread frame (backlog first, then the socket queue).
func (s *wsSession) next(timeout time.Duration) (*wsFrame, error) {
	if len(s.backlog) > 0 {
		f := s.backlog[0]
		s.backlog = s.backlog[1:]
		return &f, nil
	}
	select {
	case fr, ok := <-s.frames:
		if !ok {
			if s.closeCode != 0 {
				return nil, fmt.Errorf("ws closed by server code=%d reason=%q", s.closeCode, s.closeText)
			}
			if s.closeErr != nil {
				return nil, fmt.Errorf("ws transport error: %v", s.closeErr)
			}
			return nil, fmt.Errorf("ws inbound closed")
		}
		return &fr, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("ws expect timeout after %v", timeout)
	}
}

// keep puts a frame we looked past back at the front of the backlog.
func (s *wsSession) keep(f *wsFrame) {
	s.backlog = append([]wsFrame{*f}, s.backlog...)
}

func dialWs(side *Side, designID, jarName, origin string) (*wsSession, error) {
	base := strings.TrimSuffix(side.Base, "/")
	host := strings.TrimPrefix(base, "http://")
	wsURL := "ws://" + host + "/ws/designs/" + designID + "/"
	header := http.Header{}
	if origin == "" {
		origin = base
	}
	header.Set("Origin", origin)
	if jar := side.jar(jarName); jar != nil {
		// Cookie-jar lookups go through the http:// form of the socket URL:
		// Go's cookiejar matches cookies by host/path and returns nothing for
		// a ws:// URL. One Cookie header value: the handshake reads it as a
		// single string, so join rather than Add (Add appends → multi-value).
		var cookies []string
		if u, err := url.Parse("http://" + host + "/ws/designs/" + designID + "/"); err == nil {
			for _, c := range jar.Cookies(u) {
				cookies = append(cookies, c.Name+"="+c.Value)
			}
		}
		if len(cookies) > 0 {
			header.Set("Cookie", strings.Join(cookies, "; "))
		}
	}
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		status := 0
		if errors.Is(err, websocket.ErrBadHandshake) && resp != nil {
			status = resp.StatusCode
		}
		return nil, &wsDialError{URL: wsURL, Status: status, err: err}
	}
	cancel := func() { _ = c.Close() }
	s := &wsSession{conn: c, frames: make(chan wsFrame, 16), cancel: cancel}
	go func() {
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					s.closeCode, s.closeText = ce.Code, ce.Text
				} else {
					s.closeErr = err
				}
				close(s.frames)
				return
			}
			f := wsFrame{Kind: int(mt), Data: data}
			if mt == websocket.TextMessage {
				f.Text = string(data)
			}
			select {
			case s.frames <- f:
			default:
			}
		}
	}()
	return s, nil
}

// wsDialError carries the handshake status so a scenario can ASSERT a refusal
// (expect_dial_status) instead of treating it as a runner error.
type wsDialError struct {
	URL    string
	Status int
	err    error
}

func (e *wsDialError) Error() string {
	return fmt.Sprintf("ws dial %s: status=%d err=%v", e.URL, e.Status, e.err)
}

// waitForType waits for the next text frame whose JSON "type" equals want
// ("" = any text frame). Frames that do not match are kept for later
// expectations: the server sends ready and the welcome sync back to back, so
// an expectation must not swallow the other frame.
func (s *wsSession) waitForType(want string, timeout time.Duration) (*wsFrame, error) {
	deadline := time.Now().Add(timeout)
	var kept []wsFrame
	defer func() { s.backlog = append(kept, s.backlog...) }()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("ws expect %q timeout after %v", want, timeout)
		}
		f, err := s.next(remaining)
		if err != nil {
			return nil, err
		}
		if f.Kind == int(websocket.TextMessage) {
			var m map[string]any
			if err := json.Unmarshal([]byte(f.Text), &m); err == nil {
				if ts, isStr := m["type"].(string); isStr && (want == "" || ts == want) {
					return f, nil
				}
			}
		}
		kept = append(kept, *f)
	}
}

// waitForBinaryKind waits for the next binary frame of the given y-protocol
// kind ("sync/step1", "sync/step2", "sync/update", "awareness") and returns it.
func (s *wsSession) waitForBinaryKind(want string, timeout time.Duration) (*wsFrame, error) {
	deadline := time.Now().Add(timeout)
	var kept []wsFrame
	defer func() { s.backlog = append(kept, s.backlog...) }()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("ws expect binary %q timeout after %v", want, timeout)
		}
		f, err := s.next(remaining)
		if err != nil {
			return nil, err
		}
		if f.Kind == int(websocket.BinaryMessage) && binaryFrameKind(f.Data) == want {
			return f, nil
		}
		kept = append(kept, *f)
	}
}

// binaryFrameKind names a y-protocol frame (protocol.parse_frame categories).
func binaryFrameKind(data []byte) string {
	frame, err := yproto.ParseFrame(data)
	if err != nil {
		return "invalid"
	}
	if frame.Query {
		return "query-awareness"
	}
	switch frame.Kind {
	case yproto.SyncStep1:
		return "sync/step1"
	case yproto.SyncStep2:
		return "sync/step2"
	case yproto.SyncUpdate:
		return "sync/update"
	case -1:
		return "awareness"
	}
	return "unknown"
}

// waitForClose waits for the server to close the socket and returns its code.
func (s *wsSession) waitForClose(timeout time.Duration) (int, string, error) {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case _, ok := <-s.frames:
			if !ok {
				if s.closeCode == 0 {
					if s.closeErr != nil {
						return 0, "", fmt.Errorf("ws transport error: %v", s.closeErr)
					}
					return 0, "", fmt.Errorf("ws closed without a close frame")
				}
				return s.closeCode, s.closeText, nil
			}
		case <-time.After(time.Until(deadline)):
			return 0, "", fmt.Errorf("ws close timeout after %v", timeout)
		}
	}
}

// send queues a frame: JSON text when it parses as one, else hex -> binary.
func (s *wsSession) send(frame string) error {
	if frame == "" {
		return fmt.Errorf("empty ws frame")
	}
	if frame[0] == '{' || frame[0] == '[' {
		return s.conn.WriteMessage(websocket.TextMessage, []byte(frame))
	}
	raw, err := hex.DecodeString(frame)
	if err != nil {
		return fmt.Errorf("ws send binary hex: %w", err)
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, raw)
}

// close sends a close frame and tears the session down.
func (s *wsSession) close(code int, reason string) error {
	return s.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(5*time.Second))
}

// closeIfNeeded releases the session.
func (s *wsSession) closeIfNeeded() {
	s.cancel()
	_ = s.conn.Close()
}

// decodeSyncDocument applies a sync/step2 (or update) frame to a fresh engine
// document and returns its canonical JSON (the collab document the frame
// carries). Used for the CRDT roundtrip parity check: both sides must answer
// a client sync/step1 with the SAME document, ids normalized.
func decodeSyncDocument(data []byte) (map[string]any, error) {
	frame, err := yproto.ParseFrame(data)
	if err != nil {
		return nil, err
	}
	doc := crdt.NewDoc()
	if err := crdt.ApplyUpdate(doc, frame.Payload); err != nil {
		return nil, err
	}
	return crdt.ToDocument(doc)
}

// normalizeDocument replaces per-design random uuids (page/object ids) so the
// two sides' documents compare structurally. The default document differs
// only in the page uuid the design was created with.
func normalizeDocument(value any) any {
	switch v := value.(type) {
	case string:
		if isUUIDString(v) {
			return "<uuid>"
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = normalizeDocument(v[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if isUUIDString(k) {
				out["<uuid>"] = normalizeDocument(item)
				continue
			}
			out[k] = normalizeDocument(item)
		}
		return out
	default:
		return v
	}
}

func isUUIDString(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// frameCode reads the JSON "code" field of a text frame.
func frameCode(f *wsFrame) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(f.Text), &m); err != nil {
		return ""
	}
	code, _ := m["code"].(string)
	return code
}

// frameBool reads a JSON boolean field.
func frameBool(f *wsFrame, name string) (bool, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(f.Text), &m); err != nil {
		return false, false
	}
	v, ok := m[name].(bool)
	return v, ok
}

// frameString reads a JSON string field.
func frameString(f *wsFrame, name string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(f.Text), &m); err != nil {
		return ""
	}
	v, _ := m[name].(string)
	return v
}
