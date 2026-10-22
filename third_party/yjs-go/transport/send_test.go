package transport_test

import (
	"context"
	"testing"
	"time"

	yjs "github.com/CivNode/yjs-go"
	"github.com/CivNode/yjs-go/transport"
)

// TestClientConnSend exercises ClientConn.Send directly.
func TestClientConnSend(t *testing.T) {
	url := startRelay(t)
	ctx := context.Background()

	docA := yjs.NewDoc()
	connA, err := transport.Connect(ctx, docA, url, "send-room", "")
	if err != nil {
		t.Fatalf("Connect A: %v", err)
	}
	defer func() { _ = connA.Close() }()

	docB := yjs.NewDoc()
	connB, err := transport.Connect(ctx, docB, url, "send-room", "")
	if err != nil {
		t.Fatalf("Connect B: %v", err)
	}
	defer func() { _ = connB.Close() }()

	received := make(chan struct{}, 4)
	connB.OnUpdate(func(_ []byte) { received <- struct{}{} })

	// Build an update from a separate doc and send it via Send.
	srcDoc := yjs.NewDocWithClientID(9999)
	var rawUpdate []byte
	srcDoc.OnUpdate(func(u []byte, _ interface{}) { rawUpdate = u })
	srcDoc.Transact(func() { srcDoc.GetText("code").Insert(0, "sent") }, nil)

	if len(rawUpdate) == 0 {
		t.Fatal("no update produced by srcDoc")
	}

	if err := connA.Send(rawUpdate); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: B did not receive the manually-sent update")
	}
}
