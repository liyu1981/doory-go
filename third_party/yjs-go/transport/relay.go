// Package transport — in-process test relay, reusable in Tier 2 integration tests.
//
// The relay is a minimal y-websocket-compatible server:
//   - Each room maintains a shared Yjs Doc.
//   - On connect, server sends its step1 immediately, then processes messages.
//   - step1 from client → server replies with step2.
//   - update message from a client → broadcast to all other clients in the room.
package transport

import (
	"context"
	"net/http"
	"sync"

	"nhooyr.io/websocket"

	yjs "github.com/CivNode/yjs-go"
	"github.com/CivNode/yjs-go/protocol"
)

// Room is a single collaborative room backed by a Yjs doc.
type Room struct {
	mu      sync.Mutex
	doc     *yjs.Doc
	clients []*relayClient
}

func newRoom() *Room { return &Room{doc: yjs.NewDoc()} }

// relayClient represents one WebSocket connection in a room.
type relayClient struct {
	room   *Room
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
}

func (rc *relayClient) send(data []byte) {
	_ = rc.conn.Write(rc.ctx, websocket.MessageBinary, data)
}

// Relay is an http.Handler serving multiple rooms.
//
// Pass a cancellable context to NewRelayWithContext; cancelling it closes all
// open WebSocket connections promptly. This is required in tests because
// nhooyr.io/websocket hijacks the HTTP connection, making httptest.Server's
// Close/CloseClientConnections unable to reach WS connections.
type Relay struct {
	mu    sync.Mutex
	rooms map[string]*Room

	// shutdownCtx, when cancelled, causes all active per-connection contexts
	// to be cancelled, unblocking their read loops and driving conn.Done().
	shutdownCtx context.Context
	shutdown    context.CancelFunc
}

// NewRelay creates a new in-process relay with context.Background() as its
// shutdown context. Use NewRelayWithContext when you need to close all open
// WebSocket connections on demand (e.g. simulating a relay server restart in tests).
func NewRelay() *Relay {
	ctx, cancel := context.WithCancel(context.Background())
	return &Relay{
		rooms:       make(map[string]*Room),
		shutdownCtx: ctx,
		shutdown:    cancel,
	}
}

// NewRelayWithContext creates a new in-process relay whose active connections
// are torn down when ctx is cancelled. This is the preferred constructor for
// tests that need to simulate a relay server restart.
func NewRelayWithContext(ctx context.Context) *Relay {
	shutdownCtx, cancel := context.WithCancel(ctx)
	return &Relay{
		rooms:       make(map[string]*Room),
		shutdownCtx: shutdownCtx,
		shutdown:    cancel,
	}
}

// Shutdown cancels all open WebSocket connections managed by this relay.
// After Shutdown returns, in-flight reads on connected clients will fail and
// their conn.Done() channels will be closed.
func (relay *Relay) Shutdown() {
	relay.shutdown()
}

func (relay *Relay) getOrCreateRoom(roomName string) *Room {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	r, ok := relay.rooms[roomName]
	if !ok {
		r = newRoom()
		relay.rooms[roomName] = r
	}
	return r
}

// ServeHTTP handles a WebSocket upgrade for the path /<roomName>.
func (relay *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	roomName := r.URL.Path
	if len(roomName) > 0 && roomName[0] == '/' {
		roomName = roomName[1:]
	}
	room := relay.getOrCreateRoom(roomName)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	// Derive per-connection context from the relay's shutdown context.
	// Cancelling relay.shutdownCtx (via Shutdown()) tears down all connections.
	ctx, cancel := context.WithCancel(relay.shutdownCtx)
	rc := &relayClient{room: room, conn: conn, ctx: ctx, cancel: cancel}
	defer cancel()

	// Send our step1 immediately so the client can reply with step2.
	room.mu.Lock()
	sv, _ := yjs.EncodeStateVector(room.doc)
	room.mu.Unlock()
	rc.send(protocol.EncodeSyncStep1(sv))

	// Register client.
	room.mu.Lock()
	room.clients = append(room.clients, rc)
	room.mu.Unlock()

	defer func() {
		room.mu.Lock()
		for i, c := range room.clients {
			if c == rc {
				room.clients = append(room.clients[:i], room.clients[i+1:]...)
				break
			}
		}
		room.mu.Unlock()
	}()

	// Read loop — handles step1, step2, and update messages.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		br := newByteReader(data)
		msgType, err := protocol.ReadVarUint(br)
		if err != nil {
			continue
		}
		payload, err := protocol.ReadVarBytes(br)
		if err != nil {
			continue
		}

		switch msgType {
		case protocol.MessageYjsSyncStep1:
			// Client's step1 → reply with step2 (what client is missing).
			room.mu.Lock()
			update, err2 := yjs.EncodeStateAsUpdate(room.doc, payload)
			room.mu.Unlock()
			if err2 == nil {
				rc.send(protocol.EncodeSyncStep2(update))
			}

		case protocol.MessageYjsSyncStep2:
			// Client's step2 → apply update to room doc.
			if len(payload) > 0 {
				room.mu.Lock()
				_ = yjs.ApplyUpdate(room.doc, payload, "relay")
				room.mu.Unlock()
			}

		case protocol.MessageYjsUpdate:
			// Broadcast update to all other clients, apply to room doc.
			room.mu.Lock()
			_ = yjs.ApplyUpdate(room.doc, payload, "relay")
			broadcast := protocol.EncodeUpdate(payload)
			others := make([]*relayClient, 0, len(room.clients))
			for _, c := range room.clients {
				if c != rc {
					others = append(others, c)
				}
			}
			room.mu.Unlock()
			for _, c := range others {
				c.send(broadcast)
			}
		}
	}
}
