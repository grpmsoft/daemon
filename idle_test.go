package daemon

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestServe_ConnectDisconnectEndpoints verifies that the /daemon/connect and
// /daemon/disconnect HTTP endpoints respond with 204 No Content.
func TestServe_ConnectDisconnectEndpoints(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:    "idle-endpoint-test",
		DataDir: dir,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- Serve(ctx, cfg, nil)
	}()

	// Wait for the PID file and extract the port.
	store := newDefaultPIDStore(dir, "idle-endpoint-test")
	var port int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := store.Load()
		if err == nil && data.Port > 0 {
			port = data.Port
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if port == 0 {
		t.Fatal("PID file must be written within 5 seconds")
	}

	client := &http.Client{Timeout: 5 * time.Second}

	// POST /daemon/connect must return 204.
	connectURL := fmt.Sprintf("http://127.0.0.1:%d/daemon/connect", port)
	resp, err := client.Post(connectURL, "", nil) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /daemon/connect: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST /daemon/connect: got %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	// POST /daemon/disconnect must return 204.
	disconnectURL := fmt.Sprintf("http://127.0.0.1:%d/daemon/disconnect", port)
	resp, err = client.Post(disconnectURL, "", nil) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /daemon/disconnect: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST /daemon/disconnect: got %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	cancel()
	select {
	case serveErr := <-serveErrCh:
		if serveErr != nil {
			t.Errorf("Serve must return nil on clean shutdown, got: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5 seconds")
	}
}

// TestServe_IdleAutoShutdown verifies that Serve exits gracefully when
// IdleTimeout is configured and connections drop to zero.
func TestServe_IdleAutoShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:        "idle-shutdown-test",
		DataDir:     dir,
		IdleTimeout: 200 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- Serve(ctx, cfg, nil)
	}()

	store := newDefaultPIDStore(dir, "idle-shutdown-test")
	var port int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := store.Load()
		if err == nil && data.Port > 0 {
			port = data.Port
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if port == 0 {
		t.Fatal("PID file must be written within 5 seconds")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Connect, then disconnect to trigger idle countdown.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/daemon/connect", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /daemon/connect: %v", err)
	}
	_ = resp.Body.Close()

	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, base+"/daemon/disconnect", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /daemon/disconnect: %v", err)
	}
	_ = resp.Body.Close()

	// Serve should auto-shutdown within IdleTimeout (200ms) + some margin.
	select {
	case serveErr := <-serveErrCh:
		if serveErr != nil {
			t.Errorf("Serve must return nil on idle shutdown, got: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not auto-shutdown after idle timeout")
	}

	// PID file must be cleaned up.
	if store.IsAlive() {
		t.Error("PID file must be cleared after idle shutdown")
	}
}

// TestServe_NoIdleTimeout_DoesNotAutoShutdown verifies that Serve does NOT
// auto-shutdown when IdleTimeout is zero (the default).
func TestServe_NoIdleTimeout_DoesNotAutoShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:    "no-idle-test",
		DataDir: dir,
		// IdleTimeout is zero — no auto-shutdown.
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- Serve(ctx, cfg, nil)
	}()

	store := newDefaultPIDStore(dir, "no-idle-test")
	var port int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := store.Load()
		if err == nil && data.Port > 0 {
			port = data.Port
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if port == 0 {
		t.Fatal("PID file must be written within 5 seconds")
	}
	_ = port

	// Wait 300ms — Serve must NOT have exited.
	select {
	case serveErr := <-serveErrCh:
		t.Fatalf("Serve should NOT exit when IdleTimeout=0, but got: %v", serveErr)
	case <-time.After(300 * time.Millisecond):
		// expected: Serve is still running
	}

	cancel()
	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Errorf("Serve must return nil on cancel, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
