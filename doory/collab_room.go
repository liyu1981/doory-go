// Rooms: the in-process replica of one design's Y document (apps/collab/
// room.py). A Room is an actor: connections put work in its inbox and one
// goroutine processes it in order, so no two operations on the same document
// ever overlap. Updates are durable (appended to the database) before they
// are relayed.
//
// The upstream channel layer (cross-process notify) is replaced by direct
// in-process calls into the registry — single-process substitution, recorded
// in PLAN.md; self-echoes are filtered by the saving_from_room flag.

// Ports backend/apps/collab/room.py
// Ports backend/apps/collab/notify.py
package doory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"doory-go/internal/crdt"
	"doory-go/internal/yproto"
	"doory-go/internal/yschema"
)

// room.py constants.
const (
	collabMaxControlledClients = 4
	collabMaxAwarenessBytes    = 2048
	collabOutboxLimit          = 512
	collabInboxLimit           = 200
	collabHeartbeatSeconds     = 10 * time.Second
	collabAwarenessExpiry      = 60 * time.Second
	collabBatchLimit           = 200
)

var collabWriterRoles = map[string]bool{"owner": true, "edit": true}

var collabColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

var collabPalette = []string{
	"#e11d48", "#2563eb", "#059669", "#d97706", "#7c3aed", "#0891b2",
	"#db2777", "#4d7c0f", "#c2410c", "#4338ca", "#0f766e", "#a21caf",
}

// collabColorFor ports color_for: sha256(user id) mod palette.
func collabColorFor(userID string) string {
	sum := sha256Hex(userID)
	n, err := strconvParseUintHex(sum)
	if err != nil {
		n = 0
	}
	return collabPalette[int(n%uint64(len(collabPalette)))]
}

// sha256Hex is the hex digest room.color_for hashes into the palette
// (Python: sha256(user_id.bytes).hexdigest() - the UUID's 16 bytes).
func sha256Hex(s string) string {
	id, err := parseUUID(s)
	if err != nil {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(id)
	return hex.EncodeToString(sum[:])
}

func parseUUID(s string) ([]byte, error) {
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return nil, fmt.Errorf("bad uuid")
	}
	return hex.DecodeString(s)
}

// ---------------------------------------------------------------- transport --

// CollabTransport is what a room needs from a connection (room.Transport).
type CollabTransport interface {
	SendBytes(data []byte) error
	SendText(text string) error
	Close(code int, reason string) error
}

// CollabMember is room.Member: one live connection inside a room.
type CollabMember struct {
	ID           string
	UserID       string
	Name         string
	AvatarURL    string
	Role         string
	LinkID       string
	SessionKey   string
	ConnectionID string
	Transport    CollabTransport

	CanWrite         bool
	lastSeen         time.Time
	controlled       map[uint64]bool
	introduced       map[uint64]bool
	outbox           chan outbound
	done             chan struct{}
	closing          bool
	notifiedReadOnly bool
}

type outbound struct {
	kind  byte // 'b' bytes, 't' text
	bytes []byte
	text  string
}

// CollabAwarenessSlot is room.AwarenessSlot.
type CollabAwarenessSlot struct {
	Clock uint64
	State string
	Owner string // member id; "" for a client connected to another process
	Seen  time.Time
}

// roomItem is room.Item (tagged by kind).
type roomItem struct {
	kind    string
	member  *CollabMember
	frame   *yproto.Frame
	payload []byte
	message map[string]any
}

// versionTracker ports room.VersionTracker.
type versionTracker struct {
	contiguous int64
	latest     int64
	ahead      map[int64]bool
}

func newVersionTracker(loaded int64) versionTracker {
	return versionTracker{contiguous: loaded, latest: loaded, ahead: map[int64]bool{}}
}

func (v *versionTracker) add(version int64) {
	if version > v.latest {
		v.latest = version
	}
	if version > v.contiguous {
		v.ahead[version] = true
	}
	for {
		next := v.contiguous + 1
		if !v.ahead[next] {
			break
		}
		delete(v.ahead, next)
		v.contiguous = next
	}
}

func (v *versionTracker) reset(version int64) {
	v.contiguous, v.latest = version, version
	v.ahead = map[int64]bool{}
}

// ------------------------------------------------------------------- room ---

// collabRoom is room.Room.
type collabRoom struct {
	registry *CollabRoomRegistry
	app      *App
	designID string

	inbox   chan roomItem
	members map[string]*CollabMember

	doc  *crdt.Doc
	sv   map[uint64]uint64
	size int

	title    string
	revision int64

	versions         versionTracker
	rowsSinceCompact int
	dirty            bool
	lastEditorID     string
	budget           *UpdateBudget

	awareness map[uint64]*CollabAwarenessSlot

	closing bool

	lastFlush   time.Time
	lastRecheck time.Time
	behind      int
	warned      map[string]bool

	// flush lifecycle: a background goroutine converts + persists, then
	// posts the outcome back into the inbox (room never blocks on flush).
	flushMu   sync.Mutex
	flushBusy bool

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// outbound queue sizing + the JSON text helper (room._text).
func roomText(payload map[string]any) string {
	b, _ := json.Marshal(payload) // compact; Go escapes minimal — frames carry ASCII fields
	return string(b)
}

// -------------------------------------------------------------- registry ----

// CollabRoomUnavailable is room.RoomUnavailable.
type CollabRoomUnavailable struct{ Reason string }

func (e *CollabRoomUnavailable) Error() string { return "room unavailable: " + e.Reason }

// CollabRoomRegistry is room.RoomRegistry: all rooms of this process plus the
// process-wide counters and heartbeat.
type CollabRoomRegistry struct {
	app      *App
	instance string

	mu       sync.Mutex
	rooms    map[string]*collabRoom
	opening  map[string]bool
	counters map[string]int64
	pending  map[string]int

	heartbeatOnce sync.Once
	stopBeat      chan struct{}
}

func newCollabRoomRegistry(a *App) *CollabRoomRegistry {
	host, _ := os.Hostname()
	return &CollabRoomRegistry{
		app:      a,
		instance: fmt.Sprintf("%s:%d:%s", host, os.Getpid(), uuid.New().String()[:8]),
		rooms:    map[string]*collabRoom{},
		opening:  map[string]bool{},
		counters: map[string]int64{},
		pending:  map[string]int{},
		stopBeat: make(chan struct{}),
	}
}

// SharedLayer is always false: single-process (no cross-process channel layer).
func (r *CollabRoomRegistry) SharedLayer() bool { return false }

// processKey is instance without the trailing uuid segment (host:pid).
func (r *CollabRoomRegistry) processKey() string {
	host, _, _ := strings.Cut(r.instance, ":")
	return host
}

func (r *CollabRoomRegistry) addCounter(key string) {
	r.mu.Lock()
	r.counters[key]++
	r.mu.Unlock()
}

func (r *CollabRoomRegistry) snapshotCounters() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int64{}
	for k, v := range r.counters {
		out[k] = v
	}
	return out
}

// Counters returns the process counters (stats endpoint).
func (r *CollabRoomRegistry) Counters() map[string]int64 { return r.snapshotCounters() }

// acquire ports RoomRegistry.acquire: the running room of a design, opening
// it (from the database) when this process has none.
func (r *CollabRoomRegistry) acquire(designID string) (*collabRoom, error) {
	for {
		r.mu.Lock()
		room := r.rooms[designID]
		if room != nil && !room.closing {
			r.mu.Unlock()
			return room, nil
		}
		if room != nil {
			r.mu.Unlock()
			<-room.doneCh
			continue
		}
		if r.opening[designID] {
			r.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			continue
		}
		r.opening[designID] = true
		r.mu.Unlock()
		room, err := r.open(designID)
		r.mu.Lock()
		delete(r.opening, designID)
		if err == nil {
			r.rooms[designID] = room
		}
		r.mu.Unlock()
		return room, err
	}
}

func (r *CollabRoomRegistry) open(designID string) (*collabRoom, error) {
	others := r.app.CollabOtherInstanceServes(designID, r.instance)
	for _, name := range others {
		if cutFirst2(name) != r.processKey() {
			return nil, &CollabRoomUnavailable{Reason: "shared_layer_required"}
		}
	}
	loaded, ok, err := r.app.CollabLoadOrInit(designID, r.instance,
		func(document map[string]any) ([]byte, error) { return buildSnapshot(document) })
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &CollabRoomUnavailable{Reason: "design_missing"}
	}
	opened, oerr := openCollabDoc(loaded)
	if oerr != nil {
		// Unreadable stored state: start over from the saved design.
		r.addCounter("state_rebuilt")
		if rerr := r.app.CollabResetState(designID); rerr != nil {
			return nil, rerr
		}
		loaded, ok, err = r.app.CollabLoadOrInit(designID, r.instance,
			func(document map[string]any) ([]byte, error) { return buildSnapshot(document) })
		if err != nil || !ok {
			return nil, err
		}
		opened, oerr = openCollabDoc(loaded)
		if oerr != nil {
			return nil, oerr
		}
	}
	room := newCollabRoom(r, designID, opened, loaded)
	room.start()
	r.ensureHeartbeat()
	return room, nil
}

func cutFirst2(s string) string {
	host, _, _ := strings.Cut(s, ":")
	return host
}

func (r *CollabRoomRegistry) forget(room *collabRoom) {
	r.mu.Lock()
	if r.rooms[room.designID] == room {
		delete(r.rooms, room.designID)
	}
	r.mu.Unlock()
}

func (r *CollabRoomRegistry) ensureHeartbeat() {
	r.heartbeatOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(collabHeartbeatSeconds)
			defer ticker.Stop()
			for {
				select {
				case <-r.stopBeat:
					return
				case <-ticker.C:
					r.heartbeat()
				}
			}
		}()
	})
}

// heartbeat ports RoomRegistry.heartbeat: refresh rows of live connections.
func (r *CollabRoomRegistry) heartbeat() {
	var ids []string
	r.mu.Lock()
	for _, room := range r.rooms {
		for _, m := range room.members {
			ids = append(ids, m.ConnectionID)
		}
	}
	counters := r.snapshotCountersUnsafe()
	r.mu.Unlock()
	r.app.CollabHeartbeat(r.instance, false, jsonCounters(counters), ids)
}

func (r *CollabRoomRegistry) snapshotCountersUnsafe() map[string]int64 {
	out := map[string]int64{}
	for k, v := range r.counters {
		out[k] = v
	}
	return out
}

func jsonCounters(in map[string]int64) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// notify ports notify_design + signals: tell a room about a change made
// through the HTTP API. Suppressed while the room itself is saving.
func (r *CollabRoomRegistry) notify(designID string, messageType string, payload map[string]any) {
	if savingFromRoom.Load() {
		return
	}
	r.mu.Lock()
	room := r.rooms[designID]
	r.mu.Unlock()
	if room == nil {
		return
	}
	msg := map[string]any{"type": messageType, "origin": "http"}
	for k, v := range payload {
		msg[k] = v
	}
	room.enqueue(roomItem{kind: "remote", message: msg})
}

// heartbeatID is the CollabInstance id helper (stats join).
func registryInstanceID(r *CollabRoomRegistry) string { return r.instance }

// ----------------------------------------------------------- room lifecycle --

func newCollabRoom(reg *CollabRoomRegistry, designID string, opened openedCollabDoc,
	loaded CollabLoadedState,
) *collabRoom {
	return &collabRoom{
		registry: reg, app: reg.app, designID: designID,
		inbox:   make(chan roomItem, collabInboxLimit),
		members: map[string]*CollabMember{},
		doc:     opened.doc, sv: opened.sv, size: opened.size,
		title: loaded.Title, revision: loaded.Revision,
		versions:         newVersionTracker(loaded.Version),
		rowsSinceCompact: len(loaded.Updates),
		dirty:            loaded.Version > loaded.FlushedVersion,
		budget:           NewUpdateBudget(),
		awareness:        map[uint64]*CollabAwarenessSlot{},
		warned:           map[string]bool{},
		lastFlush:        time.Now(),
		lastRecheck:      time.Now(),
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
}

func (room *collabRoom) start() {
	go room.run()
	go room.ticker()
}

func (room *collabRoom) enqueue(item roomItem) {
	select {
	case room.inbox <- item:
	default:
		room.registry.addCounter("inbox_full")
		go func() { room.inbox <- item }() // waits instead of dropping
	}
}

// run is room._run: batch-process inbox items until shutdown.
func (room *collabRoom) run() {
	defer close(room.doneCh)
	for {
		first, ok := <-room.inbox
		if !ok {
			return
		}
		items := []roomItem{first}
		batching := true
		for batching && len(items) < collabBatchLimit {
			select {
			case more := <-room.inbox:
				items = append(items, more)
			default:
				batching = false
			}
		}
		if room.process(items) {
			return
		}
	}
}

func (room *collabRoom) ticker() {
	interval := minFloat(1.0, envFloat("REALTIME_RECHECK_SECONDS", 5)/2,
		envFloat("REALTIME_FLUSH_INTERVAL_SECONDS", 10)/2)
	if interval < 0.01 {
		interval = 0.01
	}
	t := time.NewTicker(time.Duration(interval * float64(time.Second)))
	defer t.Stop()
	for {
		select {
		case <-room.stopCh:
			return
		case <-t.C:
			room.enqueue(roomItem{kind: "tick"})
		}
	}
}

func minFloat(values ...float64) float64 {
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

// join adds a connection; false when the room is just closing.
func (room *collabRoom) join(member *CollabMember) bool {
	if room.closing {
		return false
	}
	room.members[member.ID] = member
	member.lastSeen = time.Now()
	go room.pump(member)
	room.enqueue(roomItem{kind: "welcome", member: member})
	return true
}

// leave removes a connection (room goroutine only).
func (room *collabRoom) leave(member *CollabMember) {
	if _, ok := room.members[member.ID]; !ok {
		return
	}
	member.closing = true
	close(member.done)
	delete(room.members, member.ID)
	type goneEntry struct {
		client, clock uint64
	}
	var gone []goneEntry
	for client := range member.controlled {
		if slot, ok := room.awareness[client]; ok && slot.Owner == member.ID {
			delete(room.awareness, client)
			gone = append(gone, goneEntry{client, slot.Clock})
		}
	}
	for _, g := range gone {
		room.enqueue(roomItem{kind: "awareness_gone", payload: packGone(g.client, g.clock)})
	}
	if len(room.members) == 0 {
		room.closing = true
		room.enqueue(roomItem{kind: "shutdown"})
	}
}

func packGone(client, clock uint64) []byte {
	return append(append(uint64BytesBE(client), uint64BytesBE(clock)...), byte(clock>>56))
}

func uint64BytesBE(v uint64) []byte {
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	return out
}

func strconvParseUintHex(s string) (uint64, error) {
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint64(c-'A') + 10
		default:
			return 0, fmt.Errorf("bad hex")
		}
		v = v<<4 | d
	}
	return v, nil
}

// buildSnapshot ports engine.build_snapshot: the Y state of a stored
// document.
func buildSnapshot(document map[string]any) ([]byte, error) {
	return yschema.DocumentToState(document)
}

// openedCollabDoc is engine.OpenedDoc.
type openedCollabDoc struct {
	doc  *crdt.Doc
	sv   map[uint64]uint64
	size int
}

// openCollabDoc ports engine.open_doc: snapshot + appended rows.
func openCollabDoc(loaded CollabLoadedState) (openedCollabDoc, error) {
	doc := crdt.NewDoc()
	if err := crdt.ApplyUpdate(doc, loaded.Snapshot); err != nil {
		return openedCollabDoc{}, err
	}
	for _, u := range loaded.Updates {
		if err := crdt.ApplyUpdate(doc, u); err != nil {
			return openedCollabDoc{}, err
		}
	}
	return newOpenedCollabDoc(doc)
}

func newOpenedCollabDoc(doc *crdt.Doc) (openedCollabDoc, error) {
	state, err := crdt.StateVector(doc)
	if err != nil {
		return openedCollabDoc{}, err
	}
	sv, err := yproto.DecodeStateVector(state, 0)
	if err != nil {
		return openedCollabDoc{}, err
	}
	size, err := documentSize(doc)
	if err != nil {
		return openedCollabDoc{}, err
	}
	return openedCollabDoc{doc: doc, sv: sv, size: size}, nil
}

// kick closes a member with a code (room goroutine only).
func (room *collabRoom) kick(member *CollabMember, code int, reason string) {
	if member.closing {
		return
	}
	room.registry.addCounter(fmt.Sprintf("closed_%d", code))
	room.leave(member)
	go func() {
		_ = member.Transport.Close(code, reason)
	}()
}

// send queues one outbound frame; slow consumers are kicked.
func (room *collabRoom) send(member *CollabMember, kind byte, payload []byte, text string) {
	if member.closing {
		return
	}
	select {
	case member.outbox <- outbound{kind: kind, bytes: payload, text: text}:
	default:
		room.registry.addCounter("slow_consumers")
		room.kick(member, 1013, "slow_consumer")
	}
}

func (room *collabRoom) sendBytes(member *CollabMember, data []byte) {
	room.send(member, 'b', data, "")
}

func (room *collabRoom) sendText(member *CollabMember, text string) {
	room.send(member, 't', nil, text)
}

// pump drains a member's outbox to its transport.
func (room *collabRoom) pump(member *CollabMember) {
	for {
		select {
		case <-member.done:
			return
		case out := <-member.outbox:
			var err error
			if out.kind == 'b' {
				err = member.Transport.SendBytes(out.bytes)
			} else {
				err = member.Transport.SendText(out.text)
			}
			if err != nil {
				// The socket is gone: drop the member (leave is idempotent).
				select {
				case <-member.done:
				default:
					room.enqueue(roomItem{kind: "member_gone", member: member})
				}
				return
			}
		}
	}
}

// sendBytesOthers relays a frame to everyone but the sender.
func (room *collabRoom) sendBytesOthers(sender *CollabMember, frame []byte) {
	for _, m := range room.members {
		if m != sender {
			room.sendBytes(m, frame)
		}
	}
}

// sendTextAll broadcasts a JSON text frame (optionally writers only).
func (room *collabRoom) sendTextAll(payload map[string]any, writersOnly bool) {
	text := roomText(payload)
	for _, m := range room.members {
		if writersOnly && !m.CanWrite {
			continue
		}
		room.sendText(m, text)
	}
}

// closeAll kicks every connection (fail paths).
func (room *collabRoom) closeAll(code int, reason string) {
	for _, m := range snapshotMembers(room) {
		room.kick(m, code, reason)
	}
}

func snapshotMembers(room *collabRoom) []*CollabMember {
	out := make([]*CollabMember, 0, len(room.members))
	for _, m := range room.members {
		out = append(out, m)
	}
	return out
}

var _ = net.JoinHostPort
