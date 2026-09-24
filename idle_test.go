package daemon

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestServe_ConnectDisconnectEndpointsRemoved verifies that the removed
// /daemon/connect and /daemon/disconnect endpoints no longer return 204.
func TestServe_ConnectDisconnectEndpointsRemoved(t *testing.T) {
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

	// Read the token from PID file.
	data, err := store.Load()
	if err != nil {
		t.Fatalf("load PID info: %v", err)
	}
	token := data.Token

	// POST /daemon/connect must NOT return 204 (endpoint removed in v0.4.0).
	for _, path := range []string{"/daemon/connect", "/daemon/disconnect"} {
		url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, reqErr := client.Do(req)
		if reqErr != nil {
			t.Fatalf("POST %s: %v", path, reqErr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			t.Errorf("POST %s: got 204, endpoint must be removed", path)
		}
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

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Read the token from PID file.
	data, err := store.Load()
	if err != nil {
		t.Fatalf("load PID info: %v", err)
	}
	token := data.Token

	// Attach via lease, then disconnect to trigger idle countdown.
	attachCtx, attachCancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(attachCtx, http.MethodGet, base+"/daemon/attach", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /daemon/attach: %v", err)
	}
	// Read ack byte.
	ack := make([]byte, 1)
	if _, err := resp.Body.Read(ack); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("read ack: %v", err)
	}
	// Disconnect by cancelling the attach context.
	attachCancel()
	_ = resp.Body.Close()
	// Give the server a moment to process the disconnect.
	time.Sleep(100 * time.Millisecond)

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
