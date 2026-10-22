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

// startRelay creates an in-process relay server and returns its WebSocket URL.
func startRelay(t *testing.T) string {
	t.Helper()
	relay := transport.NewRelay()
	srv := httptest.NewServer(relay)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestConnectAndDisconnect(t *testing.T) {
	url := startRelay(t)
	doc := yjs.NewDoc()
	ctx := context.Background()

	conn, err := transport.Connect(ctx, doc, url, "test-room", "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := conn.Close(); err != nil {
		// Normal closure from server side may produce an error — ignore.
		t.Logf("Close: %v", err)
	}
}

func TestSyncOnConnect(t *testing.T) {
	url := startRelay(t)

	// Client A connects and sets a text value.
	docA := yjs.NewDoc()
	ctxA := context.Background()
	connA, err := transport.Connect(ctxA, docA, url, "sync-room", "")
	if err != nil {
		t.Fatalf("Connect A: %v", err)
	}
	defer func() { _ = connA.Close() }()

	textA := docA.GetText("code")
	docA.Transact(func() {
		textA.Insert(0, "hello")
	}, nil)
	// Give the relay time to receive the update.
	time.Sleep(50 * time.Millisecond)

	// Client B connects and should receive the existing state.
	docB := yjs.NewDoc()
	connB, err := transport.Connect(ctxA, docB, url, "sync-room", "")
	if err != nil {
		t.Fatalf("Connect B: %v", err)
	}
	defer func() { _ = connB.Close() }()

	// Allow handshake propagation.
	time.Sleep(50 * time.Millisecond)

	textB := docB.GetText("code")
	if got := textB.String(); got != "hello" {
		t.Errorf("docB should have 'hello' after sync, got %q", got)
	}
}

func TestRealtimePropagation(t *testing.T) {
	url := startRelay(t)
	ctx := context.Background()

	docA := yjs.NewDoc()
	connA, err := transport.Connect(ctx, docA, url, "rt-room", "")
	if err != nil {
		t.Fatalf("Connect A: %v", err)
	}
	defer func() { _ = connA.Close() }()

	docB := yjs.NewDoc()
	connB, err := transport.Connect(ctx, docB, url, "rt-room", "")
	if err != nil {
		t.Fatalf("Connect B: %v", err)
	}
	defer func() { _ = connB.Close() }()

	// Subscribe to updates on B.
	received := make(chan []byte, 4)
	connB.OnUpdate(func(update []byte) {
		received <- update
	})

	// A inserts text.
	textA := docA.GetText("code")
	docA.Transact(func() {
		textA.Insert(0, "world")
	}, nil)

	select {
	case <-received:
		// Update received by B.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: docB did not receive update from docA")
	}

	time.Sleep(20 * time.Millisecond) // let ApplyUpdate finish
	textB := docB.GetText("code")
	if got := textB.String(); got != "world" {
		t.Errorf("docB text want 'world' got %q", got)
	}
}

func TestMultipleRooms(t *testing.T) {
	url := startRelay(t)
	ctx := context.Background()

	docRoom1 := yjs.NewDoc()
	connRoom1, err := transport.Connect(ctx, docRoom1, url, "room-1", "")
	if err != nil {
		t.Fatalf("Connect room1: %v", err)
	}
	defer func() { _ = connRoom1.Close() }()

	docRoom2 := yjs.NewDoc()
	connRoom2, err := transport.Connect(ctx, docRoom2, url, "room-2", "")
	if err != nil {
		t.Fatalf("Connect room2: %v", err)
	}
	defer func() { _ = connRoom2.Close() }()

	// Insert in room1.
	text1 := docRoom1.GetText("code")
	docRoom1.Transact(func() {
		text1.Insert(0, "room1-data")
	}, nil)

	time.Sleep(50 * time.Millisecond)

	// room2 doc should be empty (different room).
	text2 := docRoom2.GetText("code")
	if got := text2.String(); got != "" {
		t.Errorf("room2 should be isolated, got %q", got)
	}
}

func TestConvergenceThroughRelay(t *testing.T) {
	url := startRelay(t)
	ctx := context.Background()

	docA := yjs.NewDoc()
	connA, err := transport.Connect(ctx, docA, url, "conv-room", "")
	if err != nil {
		t.Fatalf("Connect A: %v", err)
	}
	defer func() { _ = connA.Close() }()

	docB := yjs.NewDoc()
	connB, err := transport.Connect(ctx, docB, url, "conv-room", "")
	if err != nil {
		t.Fatalf("Connect B: %v", err)
	}
	defer func() { _ = connB.Close() }()

	// Both clients insert concurrently; let relay propagate.
	textA := docA.GetText("code")
	textB := docB.GetText("code")

	docA.Transact(func() { textA.Insert(0, "A") }, nil)
	docB.Transact(func() { textB.Insert(0, "B") }, nil)

	time.Sleep(200 * time.Millisecond)

	// Both should have the same content.
	strA := textA.String()
	strB := textB.String()
	if strA != strB {
		t.Errorf("docs did not converge: A=%q B=%q", strA, strB)
	}
	if len(strA) != 2 {
		t.Errorf("expected 2 chars, got %q", strA)
	}
}
