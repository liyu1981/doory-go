package transport_test

// Interop tests run only when YJS_GO_INTEROP=1.
// They require Node.js and the interop server dependencies installed via:
//
//	testdata/interop/setup.sh
//
// Then run with:
//
//	YJS_GO_INTEROP=1 go test ./transport/... -run TestInterop -v

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	yjs "github.com/CivNode/yjs-go"
	"github.com/CivNode/yjs-go/transport"
)

func interopEnabled() bool {
	return os.Getenv("YJS_GO_INTEROP") == "1"
}

func interopServerDir() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "testdata", "interop")
}

// startJSServer starts the Node.js interop server on a random port and
// returns the port and a cleanup function.
func startJSServer(t *testing.T) int {
	t.Helper()

	// Find a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	serverJS := filepath.Join(interopServerDir(), "server.js")
	cmd := exec.Command("node", serverJS)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port))
	cmd.Dir = interopServerDir()
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// Wait for "READY\n".
	sc := bufio.NewScanner(stdout)
	ready := make(chan struct{})
	go func() {
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "READY" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("JS server did not become ready within 10s")
	}
	return port
}

// TestInteropGoToJS: Go client writes text, JS server stores it;
// a second Go client connects and syncs the text.
func TestInteropGoToJS(t *testing.T) {
	if !interopEnabled() {
		t.Skip("set YJS_GO_INTEROP=1 to run interop tests")
	}

	port := startJSServer(t)
	url := fmt.Sprintf("ws://127.0.0.1:%d", port)
	ctx := context.Background()

	// Go client A writes "hello".
	docA := yjs.NewDoc()
	connA, err := transport.Connect(ctx, docA, url, "interop-room", "")
	if err != nil {
		t.Fatalf("Connect A: %v", err)
	}
	defer func() { _ = connA.Close() }()

	textA := docA.GetText("code")
	docA.Transact(func() { textA.Insert(0, "hello") }, nil)
	time.Sleep(100 * time.Millisecond)

	// Go client B syncs from JS server.
	docB := yjs.NewDoc()
	connB, err := transport.Connect(ctx, docB, url, "interop-room", "")
	if err != nil {
		t.Fatalf("Connect B: %v", err)
	}
	defer func() { _ = connB.Close() }()

	time.Sleep(100 * time.Millisecond)

	textB := docB.GetText("code")
	if got := textB.String(); got != "hello" {
		t.Errorf("interop sync: want 'hello' got %q", got)
	}
}

// TestInteropJSToGo: use the JS server's persisted room doc.
// A Go client connects to a room with existing JS-side state.
func TestInteropConvergence(t *testing.T) {
	if !interopEnabled() {
		t.Skip("set YJS_GO_INTEROP=1 to run interop tests")
	}

	port := startJSServer(t)
	url := fmt.Sprintf("ws://127.0.0.1:%d", port)
	ctx := context.Background()

	// Client A and B both connect and write concurrently.
	docA := yjs.NewDoc()
	connA, err := transport.Connect(ctx, docA, url, "conv-interop", "")
	if err != nil {
		t.Fatalf("Connect A: %v", err)
	}
	defer func() { _ = connA.Close() }()

	docB := yjs.NewDoc()
	connB, err := transport.Connect(ctx, docB, url, "conv-interop", "")
	if err != nil {
		t.Fatalf("Connect B: %v", err)
	}
	defer func() { _ = connB.Close() }()

	textA := docA.GetText("code")
	textB := docB.GetText("code")

	docA.Transact(func() { textA.Insert(0, "A") }, nil)
	docB.Transact(func() { textB.Insert(0, "B") }, nil)

	time.Sleep(300 * time.Millisecond)

	strA := textA.String()
	strB := textB.String()
	if strA != strB {
		t.Errorf("interop convergence: A=%q B=%q (should be equal)", strA, strB)
	}
	if len(strA) != 2 {
		t.Errorf("expected 2 chars, got %q", strA)
	}
}
