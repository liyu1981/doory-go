// Package transport provides a WebSocket client for the y-websocket relay protocol.
//
// Wire framing: each WebSocket binary message is one y-protocols sync message:
//
//	varuint(msgType), varBytes(payload)
//
// The client performs a sync handshake on connect:
//  1. Receives step1 from server (server's state vector)
//  2. Sends step2 (update server needs) + sends own step1
//  3. Receives step2 from server (update we need) and applies it
//  4. Subsequent update messages (type 2) are forwarded to OnUpdate handlers.
package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"nhooyr.io/websocket"

	yjs "github.com/CivNode/yjs-go"
	"github.com/CivNode/yjs-go/protocol"
)

// UpdateHandler is called when a remote update arrives.
type UpdateHandler func(update []byte)

// ClientConn is a WebSocket connection to a y-websocket relay.
//
// The ctx field is intentionally absent: Go style requires contexts to be
// passed explicitly to each operation rather than stored in structs.
// dialCtx/dialCancel own the connection lifetime; writeMsg/readMsg each
// receive a ctx parameter.
type ClientConn struct {
	doc        *yjs.Doc
	conn       *websocket.Conn
	handlers   []UpdateHandler
	mu         sync.Mutex
	dialCtx    context.Context
	dialCancel context.CancelFunc
	done       chan struct{}
	// unsubscribeDoc removes the doc-level OnUpdate handler registered by
	// Connect. It is called by Close to prevent handler accumulation across
	// reconnects.
	unsubscribeDoc func()
}

// Connect dials url, joins roomName, and performs the sync handshake.
// token is sent as a Bearer Authorization header if non-empty.
func Connect(ctx context.Context, doc *yjs.Doc, url, roomName, token string) (*ClientConn, error) {
	dialCtx, dialCancel := context.WithCancel(ctx)

	opts := &websocket.DialOptions{
		HTTPHeader: make(http.Header),
	}
	if token != "" {
		opts.HTTPHeader.Set("Authorization", "Bearer "+token)
	}

	conn, _, err := websocket.Dial(dialCtx, url+"/"+roomName, opts)
	if err != nil {
		dialCancel()
		return nil, fmt.Errorf("ws dial %s/%s: %w", url, roomName, err)
	}

	c := &ClientConn{
		doc:        doc,
		conn:       conn,
		dialCtx:    dialCtx,
		dialCancel: dialCancel,
		done:       make(chan struct{}),
	}

	// Perform initial sync handshake before registering the local-update
	// forwarder. If the handshake fails, no handler is ever registered, so
	// the doc is not left with a dead handler pointing at a closed conn.
	if err := c.syncHandshake(dialCtx); err != nil {
		dialCancel()
		_ = conn.Close(websocket.StatusInternalError, "handshake failed")
		return nil, fmt.Errorf("sync handshake: %w", err)
	}

	// Register to send local updates to the relay now that the connection is
	// confirmed live. Store the unsubscribe function so Close can deregister
	// the handler and prevent accumulation across reconnect cycles.
	c.unsubscribeDoc = doc.OnUpdate(func(update []byte, origin interface{}) {
		if origin == "remote" {
			return
		}
		_ = c.sendUpdate(update)
	})

	// Start receive loop.
	go c.receiveLoop()

	return c, nil
}

// syncHandshake:
//  1. Send our step1 (state vector).
//  2. Wait for server's step1, reply with step2 (what server is missing).
//  3. Wait for server's step2, apply it.
func (c *ClientConn) syncHandshake(ctx context.Context) error {
	// Send our step1.
	sv, err := yjs.EncodeStateVector(c.doc)
	if err != nil {
		return fmt.Errorf("encode state vector: %w", err)
	}
	if err := c.writeMsg(ctx, protocol.EncodeSyncStep1(sv)); err != nil {
		return fmt.Errorf("send step1: %w", err)
	}

	// Read server's step1.
	msgType, payload, err := c.readMsg(ctx)
	if err != nil {
		return fmt.Errorf("read server step1: %w", err)
	}
	if msgType != protocol.MessageYjsSyncStep1 {
		return fmt.Errorf("expected step1 (0), got %d", msgType)
	}

	// Compute what server is missing and send step2.
	update, err := yjs.EncodeStateAsUpdate(c.doc, payload)
	if err != nil {
		return fmt.Errorf("encode state-as-update: %w", err)
	}
	if err := c.writeMsg(ctx, protocol.EncodeSyncStep2(update)); err != nil {
		return fmt.Errorf("send step2: %w", err)
	}

	// Read server's step2.
	msgType, payload, err = c.readMsg(ctx)
	if err != nil {
		return fmt.Errorf("read server step2: %w", err)
	}
	if msgType != protocol.MessageYjsSyncStep2 {
		return fmt.Errorf("expected step2 (1), got %d", msgType)
	}
	if len(payload) > 0 {
		if err := yjs.ApplyUpdate(c.doc, payload, "remote"); err != nil {
			return fmt.Errorf("apply server step2: %w", err)
		}
	}

	return nil
}

// receiveLoop processes incoming messages until the connection closes.
func (c *ClientConn) receiveLoop() {
	defer close(c.done)
	for {
		msgType, payload, err := c.readMsg(c.dialCtx)
		if err != nil {
			return
		}
		switch msgType {
		case protocol.MessageYjsUpdate:
			if err := yjs.ApplyUpdate(c.doc, payload, "remote"); err != nil {
				continue
			}
			c.mu.Lock()
			hs := make([]UpdateHandler, len(c.handlers))
			copy(hs, c.handlers)
			c.mu.Unlock()
			for _, h := range hs {
				h(payload)
			}
		case protocol.MessageYjsSyncStep1:
			// Late step1 from server (e.g. server restarted) — re-sync.
			update, err := yjs.EncodeStateAsUpdate(c.doc, payload)
			if err != nil {
				continue
			}
			_ = c.writeMsg(c.dialCtx, protocol.EncodeSyncStep2(update))
		}
	}
}

// Send broadcasts a raw Yjs v1 update to the relay.
func (c *ClientConn) Send(update []byte) error {
	return c.sendUpdate(update)
}

func (c *ClientConn) sendUpdate(update []byte) error {
	return c.writeMsg(c.dialCtx, protocol.EncodeUpdate(update))
}

// OnUpdate registers a handler called for every remote update received.
func (c *ClientConn) OnUpdate(h UpdateHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = append(c.handlers, h)
}

// Done returns a channel that is closed when the connection's receive loop
// exits. This happens when the relay closes the WebSocket (organic drop) or
// when Close is called. Callers can select on Done to detect organic relay
// disconnections without polling via Send.
func (c *ClientConn) Done() <-chan struct{} {
	return c.done
}

// Close gracefully closes the connection and deregisters the doc-level handler.
func (c *ClientConn) Close() error {
	if c.unsubscribeDoc != nil {
		c.unsubscribeDoc()
	}
	c.dialCancel()
	err := c.conn.Close(websocket.StatusNormalClosure, "client close")
	<-c.done
	return err
}

// writeMsg sends a binary WebSocket message.
func (c *ClientConn) writeMsg(ctx context.Context, data []byte) error {
	return c.conn.Write(ctx, websocket.MessageBinary, data)
}

// readMsg reads one binary WebSocket message and parses the y-protocols header.
func (c *ClientConn) readMsg(ctx context.Context) (uint64, []byte, error) {
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return 0, nil, err
	}
	if len(data) == 0 {
		return 0, nil, fmt.Errorf("empty message")
	}
	br := newByteReader(data)
	msgType, err := protocol.ReadVarUint(br)
	if err != nil {
		return 0, nil, fmt.Errorf("read msg type: %w", err)
	}
	payload, err := protocol.ReadVarBytes(br)
	if err != nil {
		return 0, nil, fmt.Errorf("read msg payload: %w", err)
	}
	return msgType, payload, nil
}

// byteReader wraps a []byte as an io.Reader.
type byteReader struct {
	data []byte
	pos  int
}

func newByteReader(data []byte) *byteReader { return &byteReader{data: data} }

func (b *byteReader) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}
