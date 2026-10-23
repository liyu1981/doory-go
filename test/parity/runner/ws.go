// The WebSocket scenario driver: dials the collab realtime socket
// (ws/designs/<uuid>/) on a booted side and replays dial/send/expect/close
// interactions. The frame contract mirrors probe0012-collab (owner
// connect -> ready -> welcome sync/step1, ping -> pong, client sync ->
// server sync/step2, close codes).

package runner

import (
	"encoding/hex"
	"errors"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// WsStep is one WebSocket interaction inside a scenario step:
//   - action: "connect" | "send" | "close"
//   - design_id: design uuid template (captures resolved through the step
//     variables, e.g. "{{capture:did}}")
//   - expect: frame type to wait for after dialing (e.g. "ready", "pong")
//   - send: frame payload — JSON text ({"type":"ping"}) or binary as hex
//   - expect_after: frame type to wait for after sending ("" = none)
//   - expect_binary: hex of the binary frame to compare byte-exactly
//     (the welcome sync frame); takes precedence over text matching
//   - close_code / close_reason: close the socket with these (action=close)
type WsStep struct {
	Action        string `toml:"action"`
	DesignID      string `toml:"design_id"`
	Jar           string `toml:"jar"`
	Expect        string `toml:"expect"`
	Send          string `toml:"send"`
	ExpectAfter  string `toml:"expect_after"`
	ExpectBinary string `toml:"expect_binary"`
	CloseCode     int    `toml:"close_code"`
	CloseReason   string `toml:"close_reason"`
}

// wsFrame is one received WebSocket frame.
type wsFrame struct {
	Kind int    // websocket.TextMessage / BinaryMessage
	Text string // text frames only
	Data []byte // binary frames (and text bytes)
}

// wsSession is one dialed collab socket with a buffered inbound queue.
type wsSession struct {
	conn   *websocket.Conn
	frames chan wsFrame
	cancel func()
}

func dialWs(side *Side, designID, jarName string) (*wsSession, error) {
	base := strings.TrimSuffix(side.Base, "/")
	host := strings.TrimPrefix(base, "http://")
	wsURL := "ws://" + host + "/api/v1/ws/designs/" + designID + "/"
	header := http.Header{}
	header.Set("Origin", base)
	if jar := side.jar(jarName); jar != nil {
		if u, err := url.Parse(wsURL); err == nil {
			for _, c := range jar.Cookies(u) {
				header.Add("Cookie", c.Name+"="+c.Value)
			}
		}
	}
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		status := 0
		if errors.Is(err, websocket.ErrBadHandshake) {
			status = resp.StatusCode
		}
		return nil, fmt.Errorf("ws dial %s: status=%d err=%w", wsURL, status, err)
	}
	cancel := func() { _ = c.Close() }
	s := &wsSession{conn: c, frames: make(chan wsFrame, 8), cancel: cancel}
	go func() {
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				cancel()
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

// waitForType waits for the next text frame whose JSON "type" equals want
// ("" = any text frame). Returns the frame; error on timeout/mismatch.
func (s *wsSession) waitForType(want string, timeout time.Duration) (*wsFrame, error) {
	deadline := time.Now().Add(timeout)
	for {
		var f *wsFrame
		select {
		case fr, ok := <-s.frames:
			if !ok {
				return nil, fmt.Errorf("ws inbound closed")
			}
			f = &fr
		case <-time.After(time.Until(deadline)):
			return nil, fmt.Errorf("ws expect timeout after %v", timeout)
		}
		if f.Kind == int(websocket.TextMessage) {
			var m map[string]any
			if err := json.Unmarshal([]byte(f.Text), &m); err == nil {
				if ts, isStr := m["type"].(string); isStr && (want == "" || ts == want) {
					return f, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ws expect timeout after %v (got %q)", timeout, f.Text)
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
