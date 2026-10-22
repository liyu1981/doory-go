package transport_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	yjs "github.com/CivNode/yjs-go"
	"github.com/CivNode/yjs-go/transport"
)

// TestClientConn_Done_OrganicDrop verifies that Done() fires when the relay
// cancels the connection via Shutdown, exercising Done + NewRelayWithContext
// + Shutdown together.
func TestClientConn_Done_OrganicDrop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relay := transport.NewRelayWithContext(ctx)
	srv := httptest.NewServer(relay)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	doc := yjs.NewDoc()

	conn, err := transport.Connect(ctx, doc, wsURL, "room1", "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Done() must not be closed yet.
	select {
	case <-conn.Done():
		t.Fatal("Done() fired before Shutdown was called")
	default:
	}

	// Shutdown the relay while the client is connected.
	relay.Shutdown()

	// conn.Done() must close within 2s.
	select {
	case <-conn.Done():
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("conn.Done() did not close after relay.Shutdown()")
	}
}

// TestNewRelayWithContext_CancelClosesConnections verifies that cancelling
// the relay's context (not calling Shutdown directly) also drives existing
// client Done() channels closed. This separates the context-cancellation
// path from the explicit Shutdown path.
func TestNewRelayWithContext_CancelClosesConnections(t *testing.T) {
	relayCtx, relayCancel := context.WithCancel(context.Background())

	relay := transport.NewRelayWithContext(relayCtx)
	srv := httptest.NewServer(relay)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	doc := yjs.NewDoc()

	conn, err := transport.Connect(context.Background(), doc, wsURL, "room2", "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Done() must not be closed yet.
	select {
	case <-conn.Done():
		t.Fatal("Done() fired before relay context was cancelled")
	default:
	}

	// Cancel the relay context directly (not via Shutdown).
	relayCancel()

	// conn.Done() must close within 2s.
	select {
	case <-conn.Done():
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("conn.Done() did not close after relay context cancellation")
	}
}
