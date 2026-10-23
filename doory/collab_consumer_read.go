// The consumer's read loop: frame size and rate limits, the pending-login
// auth frames, and text/binary dispatch to the room (consumers.receive /
// _authenticate / _text).

package doory

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"doory-go/internal/yproto"
)

// readLoop ports CollabConsumer.receive.
func (cc *collabConn) readLoop() {
	defer cc.cleanup()
	maxFrame := envInt("REALTIME_MAX_FRAME_BYTES", 1024*1024)
	cc.conn.SetReadLimit(int64(maxFrame) + 1024)
	for {
		messageType, data, err := cc.conn.ReadMessage()
		if err != nil {
			return // the connection is gone
		}
		size := len(data)
		if size > maxFrame {
			cc.closeMember(4413, "frame_too_large")
			return
		}
		if !cc.bucket.Allow(size) {
			cc.registry.addCounter("rate_limited")
			cc.closeMember(4429, "rate_limited")
			return
		}
		if cc.state == "authenticating" {
			if messageType != websocket.TextMessage {
				cc.refuse(4400, "auth_required")
				return
			}
			if size > collabMaxAuthFrameBytes {
				cc.refuse(4413, "frame_too_large")
				return
			}
			cc.authenticate(data)
			if cc.state != "active" {
				return
			}
			continue
		}
		if cc.state != "active" {
			return
		}
		member, room := cc.member, cc.room
		if member == nil || room == nil || member.closing {
			return
		}
		member.lastSeen = time.Now()
		if messageType == websocket.TextMessage {
			if !cc.handleText(room, member, data) {
				return
			}
			continue
		}
		frame, ferr := yproto.ParseFrame(data)
		if ferr != nil {
			cc.closeMember(4400, "bad_frame")
			return
		}
		room.enqueue(roomItem{kind: "frame", member: member, frame: &frame})
	}
}

// authenticate ports CollabConsumer._authenticate.
func (cc *collabConn) authenticate(data []byte) {
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		cc.refuse(4400, "bad_auth")
		return
	}
	kind, _ := message["type"].(string)
	if kind != "auth" {
		cc.refuse(4400, "auth_required")
		return
	}
	token, tokenOK := message["token"].(string)
	passwordRaw, passwordAny := message["password"], message["password"]
	password, passwordOK := "", passwordAny == nil
	if passwordAny != nil {
		password, passwordOK = passwordRaw.(string)
	}
	if !tokenOK || !passwordOK {
		cc.refuse(4400, "bad_auth")
		return
	}
	grant, err := cc.app.linkGrant(cc.designID, token, password, clientIP(cc.req))
	switch {
	case errAsBool(err, errCollabForbidden):
		cc.registry.addCounter("auth_failed")
		cc.refuse(4403, "forbidden")
		return
	case err == errCollabPasswordNeed:
		cc.attempts++
		cc.registry.addCounter("auth_failed")
		if cc.attempts >= collabMaxPasswordAttempts {
			cc.refuse(4403, "forbidden")
			return
		}
		cc.sendDirectText(`{"type":"error","code":"password_required","message":"This link needs its password."}`)
		return
	default:
		var locked *CollabLocked
		if err != nil && asCollabLocked(err, &locked) {
			cc.registry.addCounter("auth_failed")
			retry := int(math.Ceil(locked.RetryAfter))
			cc.sendDirectText(`{"type":"error","code":"throttled","message":"Too many wrong passwords.","retry_after":` +
				itoaInt(retry) + `}`)
			cc.refuse(4429, "password_locked")
			return
		}
		if err != nil {
			cc.registry.addCounter("auth_failed")
			cc.refuse(4403, "forbidden")
			return
		}
	}
	cc.enter(&grant)
}

func errAsBool(err, target error) bool { return err == target }

func asCollabLocked(err error, target **CollabLocked) bool {
	l, ok := err.(*CollabLocked)
	if ok {
		*target = l
	}
	return ok
}

// sendDirectText writes a text frame outside the room (pre-member paths; the
// pump is not running yet, so this is the only writer).
func (cc *collabConn) sendDirectText(text string) {
	_ = cc.transport.SendText(text)
}

// handleText ports CollabConsumer._text: ping/flush and the frame contract.
// Returns false when the connection must stop.
func (cc *collabConn) handleText(room *collabRoom, member *CollabMember, data []byte) bool {
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		cc.closeMember(4400, "bad_frame")
		return false
	}
	kind, _ := message["type"].(string)
	switch kind {
	case "ping":
		room.sendText(member, `{"type":"pong"}`)
	case "flush":
		if member.CanWrite {
			room.enqueue(roomItem{kind: "flush", member: member})
		}
	case "auth":
		// a late auth frame is ignored (already active)
	default:
		cc.closeMember(4400, "bad_frame")
		return false
	}
	return true
}

// closeMember closes an active connection through the room so its state is
// cleaned up first (consumer._close_member).
func (cc *collabConn) closeMember(code int, reason string) {
	if cc.member != nil && cc.room != nil {
		cc.state = "closing"
		cc.room.kick(cc.member, code, reason)
		return
	}
	cc.refuse(code, reason)
}

// cleanup runs on read-loop exit (consumer.disconnect).
func (cc *collabConn) cleanup() {
	if cc.authTimer != nil {
		cc.authTimer.Stop()
	}
	cc.settlePending()
	member, room := cc.member, cc.room
	cc.member = nil
	if member != nil && room != nil {
		room.leave(member)
	}
	if member != nil {
		cc.app.CollabRelease(member.ConnectionID)
	}
	cc.state = "closed"
	_ = cc.conn.Close()
}

var _ = http.StatusNotFound
