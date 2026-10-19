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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grpmsoft/daemon/internal/pidlock"
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

// TestProxyAttach_NotFoundReturnsNil verifies that proxyAttach returns nil
// when the daemon returns 404 for /daemon/attach. In v0.4.0+ there is no
// fallback to connect/disconnect.
func TestProxyAttach_NotFoundReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	plog := log.New(io.Discard, "", 0)
	lease := proxyAttach(context.Background(), srv.URL, "", plog)

	if lease != nil {
		t.Error("lease should be nil when attach returns 404")
		lease.cancel()
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
	lease := proxyAttach(context.Background(), srv.URL, "", plog)

	if lease == nil {
		t.Fatal("lease should be non-nil in lease mode")
	}

	// Clean up — cancel the attach context.
	lease.cancel()
	<-lease.done // the lease ends once cancelled
}

// ---------------------------------------------------------------------------
// Tests: reconnect decisions
// ---------------------------------------------------------------------------

// A request is retried only when it provably never reached a daemon: the dial
// failed. Anything after the dial (reset, EOF, timeout while waiting) may have
// executed it.
func TestNotDelivered(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connectex: no connection could be made because the target machine actively refused it")}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"dial refused", dialErr, true},
		{"dial wrapped by net/http", fmt.Errorf("http post: %w", &url.Error{Op: "Post", URL: "http://127.0.0.1:1/mcp", Err: dialErr}), true},
		{"read reset after sending", &url.Error{Op: "Post", URL: "u", Err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("wsarecv: an existing connection was forcibly closed by the remote host")}}, false},
		{"EOF after sending", &url.Error{Op: "Post", URL: "u", Err: io.EOF}, false},
		{"unexpected EOF", fmt.Errorf("read response body: %w", io.ErrUnexpectedEOF), false},
		{"context canceled", &url.Error{Op: "Post", URL: "u", Err: context.Canceled}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := notDelivered(tc.err); got != tc.want {
				t.Errorf("notDelivered(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A failed request is answered with a JSON-RPC error carrying its id verbatim
// (number or string); a notification (no id) and unparsable input get none.
func TestErrorReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string // "" = no reply
	}{
		{"numeric id", `{"jsonrpc":"2.0","id":7,"method":"tools/call"}`, `{"jsonrpc":"2.0","id":7,"error":{"code":-32603,"message":"daemon unavailable: boom"}}`},
		{"string id", `{"jsonrpc":"2.0","id":"a-1","method":"x"}`, `{"jsonrpc":"2.0","id":"a-1","error":{"code":-32603,"message":"daemon unavailable: boom"}}`},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, ""},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"x"}`, ""},
		{"not json", `garbage`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := errorReply([]byte(tc.line), "boom")
			if tc.want == "" {
				if ok {
					t.Errorf("errorReply(%s) = %s, want no reply", tc.line, got)
				}
				return
			}
			if !ok || string(got) != tc.want {
				t.Errorf("errorReply(%s) = %s (%v), want %s", tc.line, got, ok, tc.want)
			}
		})
	}
}

// A 401 means the request reached a daemon that is not ours (another daemon
// took the old port; its token differs) and did not run it: the proxy reads the
// PID file again and sends the request once to the daemon named there. Any
// other HTTP error came from our daemon and is not retried.
func TestForward_RetriesOnlyNotDelivered(t *testing.T) {
	for _, tc := range []struct {
		name      string
		oldStatus int
		wantNew   int32 // requests the new daemon gets
		wantErr   bool
	}{
		{"401 from a foreign daemon on the old port", http.StatusUnauthorized, 1, false},
		{"500 from our daemon", http.StatusInternalServerError, 0, true},
		{"404 from our daemon", http.StatusNotFound, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var oldHits, newHits atomic.Int32
			oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/mcp" {
					oldHits.Add(1)
				}
				http.Error(w, "no", tc.oldStatus)
			}))
			defer oldSrv.Close()
			newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/mcp" {
					newHits.Add(1)
					_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
					return
				}
				if r.URL.Path == "/daemon/attach" && r.Header.Get("Authorization") == "Bearer new" {
					// A live daemon takes the lease: that is what proves it alive.
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte{0x00})
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				http.NotFound(w, r)
			}))
			defer newSrv.Close()

			dir := t.TempDir()
			pidPath := filepath.Join(dir, "test.pid")
			lock, err := pidlock.TryLock(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			data, _ := json.Marshal(PIDInfo{PID: 1, Port: portOf(t, newSrv.URL), Name: "test", Token: "new"})
			if err := lock.WriteData(data); err != nil {
				t.Fatal(err)
			}

			p := &proxySession{
				cfg:     Config{Name: "test", DataDir: dir, Timeout: time.Second},
				mcpPath: "/mcp",
				pidPath: pidPath,
				plog:    log.New(io.Discard, "", 0),
				client:  &http.Client{},
			}
			p.target = proxyTarget{port: portOf(t, oldSrv.URL), token: "old", base: oldSrv.URL, url: oldSrv.URL + "/mcp"}

			// The lease is taken under ctx: cancelling it before newSrv.Close
			// (defers run in reverse) ends the held attach request.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resp, _, err := p.forward(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			if (err != nil) != tc.wantErr {
				t.Fatalf("forward error = %v, want error %v (resp %s)", err, tc.wantErr, resp)
			}
			if oldHits.Load() != 1 || newHits.Load() != tc.wantNew {
				t.Errorf("requests: old daemon %d, new daemon %d; want 1 and %d", oldHits.Load(), newHits.Load(), tc.wantNew)
			}
		})
	}
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if _, err := fmt.Sscan(port, &n); err != nil {
		t.Fatal(err)
	}
	return n
}
