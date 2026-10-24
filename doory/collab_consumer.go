// The WebSocket consumer (apps/collab/consumers.py): handshake, login, rate
// limits, then frames to and from the room — /api/v1/ws/designs/<uuid>/.

// Ports backend/apps/collab/consumers.py
// Ports backend/apps/collab/routing.py
package doory

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// consumer.py constants.
const (
	collabMaxPasswordAttempts = 5
	collabMaxAuthFrameBytes   = 1024
)

// collabRegistry lazily builds this process's room registry.
func (a *App) collabRegistry() *CollabRoomRegistry {
	a.collabOnce.Do(func() {
		a.collab = newCollabRoomRegistry(a)
	})
	return a.collab
}

// ---------------------------------------------------------------- transport --

// wsTransport is the CollabTransport over a gorilla connection. All writes
// pass one mutex (single-writer rule) through the member's outbox pump.
type wsTransport struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (t *wsTransport) SendBytes(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return t.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (t *wsTransport) SendText(text string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return t.conn.WriteMessage(websocket.TextMessage, []byte(text))
}

func (t *wsTransport) Close(code int, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := t.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(5*time.Second))
	// Django's close ends the transport; drop the socket so the reader wakes.
	_ = t.conn.Close()
	return err
}

// --------------------------------------------------------------- connection --

// collabConn is one accepted WebSocket connection.
type collabConn struct {
	app       *App
	room      *collabRoom
	member    *CollabMember
	conn      *websocket.Conn
	req       *http.Request
	transport *wsTransport

	designID  string
	user      *UserRow
	bucket    *TokenBucket
	state     string // "authenticating" | "active" | "closed"
	authTimer *time.Timer
	attempts  int
	pending   bool
	registry  *CollabRoomRegistry
	closeOnce sync.Once
}

var wsUpgrader = websocket.Upgrader{
	// Origin: the page that opens the socket must be this site (ports
	// RealtimeOriginValidator — a denied origin answers HTTP403 before the
	// upgrade; probe0012 phase B).
	HandshakeTimeout: 10 * time.Second,
}

// HandleCollabWS ports the ASGI route ws/designs/<uuid>/.
func (a *App) HandleCollabWS(w http.ResponseWriter, r *http.Request) {
	registry := a.collabRegistry()
	designID := strings.ToLower(routeParam(r, "uuid"))
	if !uuidShape.MatchString(designID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Storage form (32 hex, Django's uuid.hex) for the registry/DB keys; the
	// route validated the dashed canonical form above.
	designID = hexUUID(designID)

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		CheckOrigin: func(req *http.Request) bool {
			return a.Settings.OriginAllowed(req.Header.Get("Origin"), req.Host)
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the handshake was refused (403 for a foreign origin)
	}
	cc := &collabConn{
		app: a, conn: conn, req: r, transport: &wsTransport{conn: conn},
		designID: designID, registry: registry,
		bucket: NewTokenBucket(), state: "authenticating",
	}
	cc.serve()
}

// refuse closes with a code+reason (consumer.refuse).
func (cc *collabConn) refuse(code int, reason string) {
	cc.settlePending()
	cc.state = "closed"
	cc.registry.addCounter("refused_" + itoaInt(code))
	_ = cc.transport.Close(code, reason)
}

func itoaInt(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func (cc *collabConn) settlePending() {
	if cc.pending {
		cc.pending = false
		cc.registry.mu.Lock()
		cc.registry.pending[cc.memberKey()]--
		cc.registry.mu.Unlock()
	}
}

func (cc *collabConn) memberKey() string {
	if cc.user != nil {
		return cc.user.ID
	}
	return ""
}

// serve is the connection lifecycle (connect -> login -> frames).
func (cc *collabConn) serve() {
	// connect: session first (probe0012: accept then close4401).
	cc.user = currentUserFromRequest(cc.req)
	if cc.user == nil {
		cc.refuse(4401, "not_authenticated")
		cc.waitClosed()
		return
	}
	userID := cc.user.ID
	if !AllowConnectionAttempt(userID) {
		cc.refuse(4429, "rate_limited")
		cc.waitClosed()
		return
	}
	grant, err := cc.app.ownerGrant(cc.user, cc.designID)
	if err != nil { // DesignGone
		cc.refuse(4404, "design_trashed")
		cc.waitClosed()
		return
	}
	if grant != nil {
		if !cc.enter(grant) {
			cc.waitClosed()
			return
		}
		cc.readLoop()
		return
	}
	// pending login (the auth timer races the auth frame)
	cc.registry.mu.Lock()
	cc.registry.pending[userID]++
	cc.pending = true
	pendingCount := cc.registry.pending[userID]
	cc.registry.mu.Unlock()
	if pendingCount > envInt("REALTIME_MAX_CONNECTIONS_PER_USER", 10) {
		cc.refuse(4429, "too_many_connections")
		cc.waitClosed()
		return
	}
	timeout := time.Duration(envFloat("REALTIME_AUTH_TIMEOUT_SECONDS", 10) * float64(time.Second))
	cc.authTimer = time.AfterFunc(timeout, func() {
		if cc.state == "authenticating" {
			cc.refuse(4408, "auth_timeout")
		}
	})
	cc.readLoop()
}

// currentUserFromRequest resolves the session on the upgrade request.
func currentUserFromRequest(r *http.Request) *UserRow {
	if r == nil {
		return nil
	}
	return currentUser(r)
}

// enter admits the connection (consumer.enter).
func (cc *collabConn) enter(grant *CollabGrant) bool {
	cc.settlePending()
	admission := cc.app.CollabAdmit(cc.designID, cc.user.ID, grant.Role, grant.LinkID, registryInstanceID(cc.registry))
	if !admission.OK {
		cc.refuse(4429, admission.Reason)
		return false
	}
	if cc.state == "closed" {
		cc.app.CollabRelease(admission.ConnectionID)
		return false
	}
	avatar := ""
	if cc.user.Avatar != "" {
		avatar = cc.user.Avatar
	}
	member := &CollabMember{
		ID: newUUID4Hex(), UserID: cc.user.ID, Name: publicName(cc.user),
		AvatarURL: avatar, Role: grant.Role, LinkID: grant.LinkID,
		SessionKey: cc.sessionKey(),
		ConnectionID: admission.ConnectionID,
		Transport:  cc.transport,
		CanWrite:   collabWriterRoles[grant.Role],
		controlled: map[uint64]bool{}, introduced: map[uint64]bool{},
		outbox: make(chan outbound, collabOutboxLimit),
		done:   make(chan struct{}),
	}
	// The ready frame first, then the room's welcome (probe0012 order).
	ready := map[string]any{
		"type": "ready", "role": grant.Role, "canWrite": member.CanWrite,
		"user": map[string]any{"id": member.UserID, "name": member.Name, "avatar_url": avatar},
		"title":  grant.Title, // the room shows the same title at join time
		"revision": grant.Revision,
		"limits": map[string]any{
			"maxFrameBytes":   envInt("REALTIME_MAX_FRAME_BYTES", 1024*1024),
			"maxParticipants": envInt("REALTIME_MAX_CONNECTIONS_PER_DESIGN", 25),
		},
	}
	readyText, _ := json.Marshal(ready)
	member.outbox <- outbound{kind: 't', text: string(readyText)}

	var room *collabRoom
	for i := 0; i < 3; i++ {
		r, err := cc.registry.acquire(cc.designID)
		if err != nil {
			var unavailable *CollabRoomUnavailable
			if errAs(err, &unavailable) {
				cc.app.CollabRelease(admission.ConnectionID)
				code := 1013
				if unavailable.Reason == "internal_error" {
					code = 1011
				}
				cc.refuse(code, unavailable.Reason)
				return false
			}
			cc.app.CollabRelease(admission.ConnectionID)
			cc.refuse(1011, "internal_error")
			return false
		}
		if r.join(member) {
			room = r
			break
		}
	}
	if room == nil {
		cc.app.CollabRelease(admission.ConnectionID)
		cc.refuse(1013, "try_again")
		return false
	}
	if cc.state == "closed" { // the client left while being admitted
		room.leave(member)
		cc.app.CollabRelease(admission.ConnectionID)
		return false
	}
	cc.member, cc.room, cc.state = member, room, "active"
	if cc.authTimer != nil {
		cc.authTimer.Stop()
	}
	cc.registry.addCounter("connects")
	return true
}

func (cc *collabConn) sessionKey() string {
	if cc.req == nil {
		return ""
	}
	if c, err := cc.req.Cookie(sessionCookieName); err == nil {
		return c.Value
	}
	return ""
}

func errAs(err error, target any) bool {
	e, ok := target.(**CollabRoomUnavailable)
	if ok {
		u, isa := err.(*CollabRoomUnavailable)
		if isa {
			*e = u
			return true
		}
	}
	return false
}

// waitClosed blocks until the peer closes (read deadline-free).
func (cc *collabConn) waitClosed() {
	_ = cc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := cc.conn.ReadMessage(); err != nil {
			return
		}
	}
}
