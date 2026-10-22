package transport_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	yjs "github.com/CivNode/yjs-go"
	"github.com/CivNode/yjs-go/transport"
)

// TestConnectBadURL verifies that Connect returns an error when the URL is
// unreachable.
func TestConnectBadURL(t *testing.T) {
	doc := yjs.NewDoc()
	ctx := context.Background()

	_, err := transport.Connect(ctx, doc, "ws://127.0.0.1:1", "room", "")
	if err == nil {
		t.Fatal("expected error for bad URL, got nil")
	}
}

// TestConnectCancelledContext verifies that Connect fails fast when the context
// is already cancelled before dialing.
func TestConnectCancelledContext(t *testing.T) {
	doc := yjs.NewDoc()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before dialing

	_, err := transport.Connect(ctx, doc, "ws://127.0.0.1:1", "room", "")
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

// TestSyncHandshakePartialStep1 verifies that a server that sends an
// unexpected message type during handshake causes Connect to return an error
// without hanging.
func TestSyncHandshakePartialStep1(t *testing.T) {
	// Serve a plain HTTP 200 — not a WebSocket upgrade — to trigger an
	// immediate dial error. This exercises the dial-failure path in Connect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	doc := yjs.NewDoc()
	ctx := context.Background()
	_, err := transport.Connect(ctx, doc, wsURL, "room", "")
	if err == nil {
		t.Fatal("expected error when server returns non-WebSocket response, got nil")
	}
}

// TestNoHandlerLeakOnFailedHandshake verifies that a failed Connect does not
// leave an OnUpdate handler registered on the doc. If it did, a subsequent
// Transact would invoke a handler pointing at a closed connection.
func TestNoHandlerLeakOnFailedHandshake(t *testing.T) {
	// Count how many times the doc's OnUpdate fires so we can detect a leak.
	doc := yjs.NewDoc()
	var leaked atomic.Int32
	// Register a sentinel handler so we can distinguish "our" leak from
	// legitimate callers. We track calls separately below.
	_ = doc.OnUpdate(func(_ []byte, _ interface{}) { leaked.Add(1) })

	// Baseline: one handler is registered (sentinel). Record the call count
	// after a transact with NO failed connect.
	text := doc.GetText("x")
	doc.Transact(func() { text.Insert(0, "baseline") }, nil)
	baseline := leaked.Load() // should be 1

	// Attempt a connect that will fail — plain HTTP endpoint, not WebSocket.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	_, err := transport.Connect(context.Background(), doc, wsURL, "room", "")
	if err == nil {
		t.Fatal("expected Connect to fail")
	}

	// A transact after the failed connect must not trigger any additional
	// handler calls beyond the sentinel we already registered.
	doc.Transact(func() { text.Insert(8, "!") }, nil)
	after := leaked.Load()
	if after != baseline+1 {
		t.Errorf("handler leak detected: baseline=%d, after failed connect+transact=%d (want %d)", baseline, after, baseline+1)
	}
}
