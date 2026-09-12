package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findFreePort finds an available TCP port for tests.
func findFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return port
}

// ---------------------------------------------------------------------------
// Tests: proxyRequest
// ---------------------------------------------------------------------------

func TestProxyRequest_SuccessReturnsBody(t *testing.T) {
	const wantBody = `{"result":"ok"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("got %v, want %v", r.Method, http.MethodPost)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("got %v, want %v", ct, "application/json")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, wantBody)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	body, _, err := proxyRequest(context.Background(), client, srv.URL+"/mcp", "", []byte(`{"id":1}`))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != wantBody {
		t.Errorf("got %v, want %v", string(body), wantBody)
	}
}

func TestProxyRequest_4xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, _, err := proxyRequest(context.Background(), client, srv.URL+"/mcp", "", []byte(`{}`))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("%q does not contain %q", err.Error(), "400")
	}
}

func TestProxyRequest_5xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, _, err := proxyRequest(context.Background(), client, srv.URL+"/mcp", "", []byte(`{}`))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("%q does not contain %q", err.Error(), "500")
	}
}

func TestProxyRequest_ConnectionRefused_ReturnsError(t *testing.T) {
	port := findFreePort(t) // bind + immediately release → nothing listens

	client := &http.Client{Timeout: time.Second}
	_, _, err := proxyRequest(
		context.Background(),
		client,
		fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		"",
		[]byte(`{}`),
	)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestProxyRequest_ContextCancelled_ReturnsError(t *testing.T) {
	// Block the server so the context cancel races the request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wait until request context is done, then return.
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	client := &http.Client{Timeout: 5 * time.Second}
	_, _, err := proxyRequest(ctx, client, srv.URL+"/mcp", "", []byte(`{}`))

	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Tests: Proxy (stdin↔HTTP bridge)
// ---------------------------------------------------------------------------

// TestProxy_ForwardsLinesAndWritesResponses tests the main Proxy loop by
// replacing os.Stdin/os.Stdout with pipes.
//
// Proxy reads from os.Stdin and writes to os.Stdout, so we cannot inject
// custom readers/writers without modifying the production code. Instead, we
// test the internal proxyRequest helper directly (above) and verify Proxy
// returns nil on EOF.
//
// The Proxy function signature ties it to os.Stdin/os.Stdout. To verify the
// routing behaviour without modifying production code we test the protocol
// end-to-end using real pipes via os.Pipe().

// Old TestProxy_ReturnNilOnEmptyStdin removed — Proxy signature changed in v0.2.0.
// Proxy now reads port from PID file (needs running daemon or locked PID file).
// ProxyRequest tests below cover the HTTP round-trip logic.

// TestProxy_URLConstruction verifies that Proxy constructs the correct target URL.
// We test this indirectly through proxyRequest to avoid os.Stdin dependency.
func TestProxy_URLConstruction(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	port := extractServerPort(t, srv.URL)
	mcpPath := "/api/v1/mcp"
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, mcpPath)

	client := &http.Client{Timeout: 5 * time.Second}
	_, _, err := proxyRequest(context.Background(), client, url, "", []byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != mcpPath {
		t.Errorf("proxyRequest must POST to the correct path, got %v, want %v", gotPath, mcpPath)
	}
}

// TestProxy_LargeBody verifies proxyRequest handles large payloads (simulates
// tool responses with code, matching the 4MB scanner buffer in Proxy).
func TestProxy_LargeBody(t *testing.T) {
	const size = 512 * 1024 // 512 KB — well within the 4 MB scanner buffer
	large := strings.Repeat("x", size)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			http.Error(w, "empty body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"size":%d}`, len(body))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, _, err := proxyRequest(
		context.Background(),
		client,
		srv.URL+"/mcp",
		"",
		[]byte(`{"data":"`+large+`"}`),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Contains(resp, []byte(`"size"`)) {
		t.Errorf("response %q does not contain %q", resp, `"size"`)
	}
}

// ---------------------------------------------------------------------------
// Tests: Proxy — dead daemon and missing PID file
// ---------------------------------------------------------------------------

// TestProxy_DeadDaemon_ReturnsErrNotRunning verifies that Proxy returns
// ErrNotRunning when a valid PID file exists but the lock is NOT held
// (daemon crashed or was killed).
func TestProxy_DeadDaemon_ReturnsErrNotRunning(t *testing.T) {
	dir := t.TempDir()

	// Write a valid PID file without holding the lock.
	info := PIDInfo{
		PID:       99999,
		Port:      12345,
		Name:      "test",
		Binary:    "/usr/bin/test",
		StartTime: time.Now(),
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal PIDInfo: %v", err)
	}

	pidPath := filepath.Join(dir, "test.pid")
	if err := os.WriteFile(pidPath, data, 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	cfg := Config{
		Name:    "test",
		DataDir: dir,
	}
	opts := ProxyOptions{
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
	}

	err = Proxy(context.Background(), cfg, opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNotRunning) {
		t.Errorf("expected ErrNotRunning, got: %v", err)
	}
}

// TestProxy_NoPIDFile_ReturnsError verifies that Proxy returns an error
// when no PID file exists at all.
func TestProxy_NoPIDFile_ReturnsError(t *testing.T) {
	dir := t.TempDir() // empty — no PID file

	cfg := Config{
		Name:    "nonexistent",
		DataDir: dir,
	}
	opts := ProxyOptions{
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
	}

	err := Proxy(context.Background(), cfg, opts)
	if err == nil {
		t.Fatal("expected error for missing PID file, got nil")
	}
	// Should NOT be ErrNotRunning — it's a different failure (can't read PID file).
	if errors.Is(err, ErrNotRunning) {
		t.Errorf("missing PID file should not return ErrNotRunning, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: proxyAttach fallback
// ---------------------------------------------------------------------------

// TestProxyAttach_FallbackOnNotFound verifies that proxyAttach falls back to
// signalConnect when the daemon returns 404 for /daemon/attach (v0.3.0 compat).
func TestProxyAttach_FallbackOnNotFound(t *testing.T) {
	var connectCalled bool

	// Simulate a v0.3.0 daemon: /daemon/attach returns 404, /daemon/connect works.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/daemon/attach":
			http.NotFound(w, r)
		case "/daemon/connect":
			connectCalled = true
			w.WriteHeader(http.StatusNoContent)
		case "/daemon/disconnect":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	plog := log.New(io.Discard, "", 0)
	cancelFn, connected := proxyAttach(context.Background(), srv.URL, "", plog)

	// Must have fallen back to connect/disconnect.
	if cancelFn != nil {
		t.Error("cancelFn should be nil in fallback mode")
		cancelFn()
	}
	if !connected {
		t.Error("should report connected=true after fallback to signalConnect")
	}
	if !connectCalled {
		t.Error("/daemon/connect should have been called in fallback mode")
	}
}

// TestProxyAttach_LeaseMode verifies that proxyAttach returns a cancel function
// when attach succeeds (200 + ack byte).
func TestProxyAttach_LeaseMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/daemon/attach" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0x00})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Block until client disconnects.
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	plog := log.New(io.Discard, "", 0)
	cancelFn, connected := proxyAttach(context.Background(), srv.URL, "", plog)

	if cancelFn == nil {
		t.Fatal("cancelFn should be non-nil in lease mode")
	}
	if connected {
		t.Error("connected should be false in lease mode (only for fallback)")
	}

	// Clean up — cancel the attach context.
	cancelFn()
}
