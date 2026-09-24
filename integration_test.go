package daemon_test

// Cross-platform integration tests using the helper-process pattern: the test
// binary re-executes itself as the daemon child. The library marks its children
// with DAEMON_MODE=1 and DAEMON_DATA_DIR, so the parent never enters the helper.
// No external binaries, no build step, runs on Linux/macOS/Windows.

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/grpmsoft/daemon"
	"github.com/grpmsoft/daemon/internal/pidlock"
)

const itName = "it"

// TestHelperProcess is the daemon child. It is skipped in the parent.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("DAEMON_MODE") != "1" {
		t.Skip("helper process only")
	}
	cfg := daemon.Config{
		Name:             itName,
		DataDir:          os.Getenv("DAEMON_DATA_DIR"),
		DisableTokenAuth: true, // allow unauthenticated app requests in tests
	}
	if v := os.Getenv("IT_IDLE"); v != "" {
		cfg.IdleTimeout, _ = time.ParseDuration(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(4 * time.Second) // long in-flight request: graceful shutdown must drain it
		_, _ = fmt.Fprint(w, "ok")
	})
	if err := daemon.Serve(context.Background(), cfg, mux); err != nil {
		fmt.Fprintln(os.Stderr, "helper serve:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func itConfig(t *testing.T) daemon.Config {
	t.Helper()
	cfg := daemon.Config{
		Name:    itName,
		DataDir: t.TempDir(),
		Binary:  os.Args[0],
		Args:    []string{"-test.run=^TestHelperProcess$", "-test.timeout=5m"},
		Timeout: 15 * time.Second,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = daemon.New(cfg).Stop(ctx)
	})
	return cfg
}

func dial(port int) error {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port)) //nolint:noctx,gosec // test-only
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func TestIntegration_ConcurrentEnsureRunning(t *testing.T) {
	cfg := itConfig(t)
	const n = 4
	ports := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { ports[i], errs[i] = daemon.EnsureRunning(context.Background(), cfg) })
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ports[i] != ports[0] {
			t.Fatalf("caller %d got port %d, caller 0 got %d", i, ports[i], ports[0])
		}
	}
	if err := dial(ports[0]); err != nil {
		t.Fatalf("dial: %v", err)
	}
}

func TestIntegration_StopHonoursDeadline(t *testing.T) {
	cfg := itConfig(t)
	port, err := daemon.EnsureRunning(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Read token — required since v0.4.0 (DisableTokenAuth defaults to false).
	_, token := readItPIDInfo(t, cfg.DataDir)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
			fmt.Sprintf("http://127.0.0.1:%d/slow", port), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		_, _ = http.DefaultClient.Do(req) //nolint:bodyclose // test-only, we don't care about the response
	}()
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st := time.Now()
	err = daemon.New(cfg).Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop(ctx) = %v, want DeadlineExceeded", err)
	}
	if el := time.Since(st); el > 1500*time.Millisecond {
		t.Fatalf("Stop returned after %s, deadline was 1s", el)
	}
	// The shutdown already requested continues without us.
	deadline := time.Now().Add(10 * time.Second)
	for daemon.New(cfg).IsRunning() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if daemon.New(cfg).IsRunning() {
		t.Fatal("daemon did not finish its own graceful shutdown")
	}
}

func TestIntegration_StopIdempotentAndStatus(t *testing.T) {
	cfg := itConfig(t)
	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	d := daemon.New(cfg)
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if info, _ := d.Status(); info.Status != daemon.StatusStopped {
		t.Fatalf("Status after Stop = %s, want stopped", info.Status)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestIntegration_IdleShutdownWithoutClients(t *testing.T) {
	cfg := itConfig(t)
	t.Setenv("IT_IDLE", "500ms")
	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for daemon.New(cfg).IsRunning() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if daemon.New(cfg).IsRunning() {
		t.Fatal("daemon with IdleTimeout=500ms and no clients still running after 5s")
	}
}

// N12: a starter took the lock and died before its child wrote anything.
// A waiting EnsureRunning must become the starter once the lock is free.
func TestIntegration_HolderDiesMidSpawn(t *testing.T) {
	cfg := itConfig(t)
	pidPath := filepath.Join(cfg.DataDir, itName+".pid")
	holder, err := pidlock.TryLock(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = holder.WriteData(nil)
	go func() { time.Sleep(1500 * time.Millisecond); holder.Release() }() // "crash"

	port, err := daemon.EnsureRunning(context.Background(), cfg)
	if err != nil {
		t.Fatalf("EnsureRunning after holder death: %v (want: become the starter)", err)
	}
	if err := dial(port); err != nil {
		t.Fatalf("dial: %v", err)
	}
}

// readItPIDInfo reads and parses the PID file for integration tests.
// Returns port and token.
func readItPIDInfo(t *testing.T, dataDir string) (port int, token string) {
	t.Helper()
	pidPath := filepath.Join(dataDir, itName+".pid")
	data, err := pidlock.ReadLocked(pidPath)
	if err != nil {
		t.Fatalf("read PID file: %v", err)
	}
	var info struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("parse PID file: %v", err)
	}
	return info.Port, info.Token
}

// ---------------------------------------------------------------------------
// v0.3.1 integration tests: lease + token
// ---------------------------------------------------------------------------

// Test 5: Lease survives crash — daemon with IdleTimeout:500ms, raw net.Conn
// attach, conn.Close() -> daemon exits within 2s.
func TestIntegration_LeaseSurvivesCrash(t *testing.T) {
	cfg := itConfig(t)
	t.Setenv("IT_IDLE", "500ms")

	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	port, token := readItPIDInfo(t, cfg.DataDir)

	// Open a raw TCP connection and send GET /daemon/attach with auth.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Send HTTP request manually.
	reqLine := fmt.Sprintf("GET /daemon/attach HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nAuthorization: Bearer %s\r\n\r\n", port, token)
	if _, err := conn.Write([]byte(reqLine)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Parse the HTTP response properly (handles chunked encoding).
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Read ack byte from the response body (handles chunked transfer encoding).
	ack := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, ack); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("read ack: %v", err)
	}
	if ack[0] != 0x00 {
		_ = resp.Body.Close()
		t.Fatalf("expected ack 0x00, got 0x%02x", ack[0])
	}

	// Daemon should stay running while the lease is held.
	time.Sleep(2 * time.Second)
	if !daemon.New(cfg).IsRunning() {
		t.Fatal("daemon should still be running while lease is held")
	}

	// Simulate crash: close the TCP connection abruptly.
	_ = conn.Close()

	// Daemon should exit within 2s after lease drop (IdleTimeout=500ms).
	deadline := time.Now().Add(2 * time.Second)
	for daemon.New(cfg).IsRunning() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if daemon.New(cfg).IsRunning() {
		t.Fatal("daemon should have exited after lease drop + idle timeout")
	}
}

// Test 7: Shutdown requires token — POST /daemon/shutdown without token -> 401
// and daemon still running; Stop(ctx) (which reads the token) -> daemon stops.
func TestIntegration_ShutdownRequiresToken(t *testing.T) {
	cfg := itConfig(t)

	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	port, _ := readItPIDInfo(t, cfg.DataDir)

	// POST /daemon/shutdown without token -> 401.
	shutdownURL := fmt.Sprintf("http://127.0.0.1:%d/daemon/shutdown", port)
	resp, err := http.Post(shutdownURL, "", nil) //nolint:noctx,gosec // test-only
	if err != nil {
		t.Fatalf("POST shutdown: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("shutdown without token: got %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	// Daemon must still be running.
	if !daemon.New(cfg).IsRunning() {
		t.Fatal("daemon should still be running after unauthorized shutdown attempt")
	}

	// Stop(ctx) reads the token from PID file and succeeds.
	if err := daemon.New(cfg).Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Daemon must be stopped.
	if daemon.New(cfg).IsRunning() {
		t.Fatal("daemon should be stopped after Stop(ctx)")
	}
}

// Test 8: Attached clients do not block Stop — proxy attached (in-process Proxy
// goroutine with its own ctx); Stop(ctx) completes in < 2s; the Proxy goroutine
// returns after the daemon is gone.
func TestIntegration_AttachedClientsDoNotBlockStop(t *testing.T) {
	cfg := itConfig(t)

	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	// Use a pipe that stays open so Proxy blocks on reading stdin (not EOF).
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	// Start a proxy in a goroutine.
	proxyCtx, proxyCancel := context.WithCancel(context.Background())
	defer proxyCancel()
	proxyErrCh := make(chan error, 1)
	go func() {
		proxyErrCh <- daemon.Proxy(proxyCtx, cfg, daemon.ProxyOptions{
			Stdin:  stdinR,
			Stdout: io.Discard,
		})
	}()

	// Give the proxy a moment to attach.
	time.Sleep(1 * time.Second)

	// Stop must complete in < 2s even with an attached client.
	start := time.Now()
	if err := daemon.New(cfg).Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("Stop took %s, must be < 2s", elapsed)
	}

	// Close stdin pipe so Proxy returns.
	_ = stdinW.Close()
	proxyCancel()

	// Proxy goroutine should return after daemon is gone + stdin closed.
	select {
	case <-proxyErrCh:
		// Expected — proxy returns (either nil or error).
	case <-time.After(5 * time.Second):
		t.Fatal("Proxy goroutine did not return within 5s")
	}

	_ = stdinR.Close()
}

// Test 9: All existing tests pass unchanged — verified by running go test ./...
