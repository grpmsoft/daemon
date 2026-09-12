package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

// TestServe_StartsAndRespondsToHealthCheck verifies that Serve:
//  1. Picks a free port and binds to it.
//  2. Writes a PID file.
//  3. Responds 200 OK at the configured health path.
//  4. Shuts down cleanly when the context is cancelled.
//  5. Removes the PID file on shutdown.
func TestServe_StartsAndRespondsToHealthCheck(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:       "serve-test",
		DataDir:    dir,
		HealthPath: "/health",
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErrCh := make(chan error, 1)

	go func() {
		serveErrCh <- Serve(ctx, cfg, nil)
	}()

	// Wait for the PID file to appear (Serve writes it before it starts serving).
	store := newDefaultPIDStore(dir, "serve-test")
	var port int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := store.Load()
		if err == nil && data.Port > 0 && data.PID > 0 {
			port = data.Port
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if port == 0 {
		t.Fatal("PID file must be written within 5 seconds")
	}

	// Verify PID file contains the current process PID.
	data, err := store.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data.PID != os.Getpid() {
		t.Errorf("got %v, want %v", data.PID, os.Getpid())
	}
	if data.Port != port {
		t.Errorf("got %v, want %v", data.Port, port)
	}
	if data.Name != "serve-test" {
		t.Errorf("got %v, want %v", data.Name, "serve-test")
	}

	// Hit the health endpoint.
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	resp, err := http.Get(url) //nolint:noctx,gosec // test-only: URL is localhost with controlled port
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %v, want %v", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("%q does not contain %q", ct, "application/json")
	}

	// Decode the JSON body and assert fields.
	var body struct {
		Status string `json:"status"`
		Name   string `json:"name"`
		PID    int    `json:"pid"`
		Uptime string `json:"uptime"`
	}
	err = json.UnmarshalRead(resp.Body, &body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("got %v, want %v", body.Status, "ok")
	}
	if body.Name != "serve-test" {
		t.Errorf("got %v, want %v", body.Name, "serve-test")
	}
	if body.PID != os.Getpid() {
		t.Errorf("got %v, want %v", body.PID, os.Getpid())
	}

	// Shut down by cancelling the context.
	cancel()

	select {
	case serveErr := <-serveErrCh:
		if serveErr != nil {
			t.Errorf("Serve must return nil on clean shutdown, got: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5 seconds after context cancel")
	}

	// PID file must be removed after shutdown.
	if store.IsAlive() {
		t.Error("PID file must be cleared after Serve returns")
	}
}

// TestServe_CustomHandler verifies that a custom HTTP handler is accessible
// alongside the built-in health endpoint.
func TestServe_CustomHandler(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:       "serve-custom",
		DataDir:    dir,
		HealthPath: "/health",
	}

	customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "pong")
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- Serve(ctx, cfg, customHandler)
	}()

	store := newDefaultPIDStore(dir, "serve-custom")
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

	// Custom endpoint must be reachable.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", port)) //nolint:noctx,gosec // test-only: localhost with controlled port
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %v, want %v", resp.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// TestServe_AppliesDefaultsToConfig verifies that Serve calls applyDefaults
// even if the caller passes a zero-value Config (apart from Name and DataDir).
func TestServe_AppliesDefaultsToConfig(t *testing.T) {
	dir := t.TempDir()
	// Pass Config with zero Timeout and empty HealthPath.
	cfg := Config{Name: "defaults-test", DataDir: dir}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- Serve(ctx, cfg, nil)
	}()

	store := newDefaultPIDStore(dir, "defaults-test")
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

	// The default health path must be "/health" (applied by applyDefaults).
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port)) //nolint:noctx,gosec // test-only: localhost with controlled port
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %v, want %v", resp.StatusCode, http.StatusOK)
	}

	cancel()
	<-serveErrCh
}

// ---------------------------------------------------------------------------
// Tests: Compare-and-delete (V2c regression)
// ---------------------------------------------------------------------------

// waitOwnPIDFile polls until Serve writes its PID file with our PID.
func waitOwnPIDFile(t *testing.T, store PIDStore) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := store.Load(); err == nil && data.PID == os.Getpid() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Serve did not write its PID file")
}

// ---------------------------------------------------------------------------
// Tests: /daemon/attach (lease-based connection tracking)
// ---------------------------------------------------------------------------

// TestServe_AttachIncrementsAndDecrements verifies that GET /daemon/attach
// increments the connection count on connect and decrements it when the
// client disconnects (closes the response body).
func TestServe_AttachIncrementsAndDecrements(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "attach-test", DataDir: dir}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, cfg, nil) }()

	store := newDefaultPIDStore(dir, "attach-test")
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

	// Open an attach connection.
	attachURL := fmt.Sprintf("http://127.0.0.1:%d/daemon/attach", port)
	resp, err := http.Get(attachURL) //nolint:noctx,gosec // test-only
	if err != nil {
		t.Fatalf("GET /daemon/attach: %v", err)
	}

	// Read the ack byte.
	ack := make([]byte, 1)
	n, err := resp.Body.Read(ack)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if n != 1 || ack[0] != 0x00 {
		t.Fatalf("expected ack byte 0x00, got %d bytes: %v", n, ack[:n])
	}

	// Verify status code and headers.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/octet-stream")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store")
	}

	// Close the client-side body — this disconnects the TCP, decrementing the count.
	_ = resp.Body.Close()

	// Give the server a moment to process the disconnect.
	time.Sleep(200 * time.Millisecond)

	// Shut down and verify clean exit.
	cancel()
	select {
	case serveErr := <-errCh:
		if serveErr != nil {
			t.Errorf("Serve returned error: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s")
	}
}

// TestServe_AttachReturnsPromptlyOnShutdown verifies that when the daemon
// shuts down, the /daemon/attach handler unblocks within 500ms.
func TestServe_AttachReturnsPromptlyOnShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "attach-shutdown", DataDir: dir}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, cfg, nil) }()

	store := newDefaultPIDStore(dir, "attach-shutdown")
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

	// Open attach connection.
	attachCtx, attachCancel := context.WithCancel(context.Background())
	defer attachCancel()
	req, _ := http.NewRequestWithContext(attachCtx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/daemon/attach", port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /daemon/attach: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read ack byte.
	ack := make([]byte, 1)
	if _, err := resp.Body.Read(ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}

	// Trigger shutdown and measure how long the attach handler takes to return.
	start := time.Now()
	cancel()

	select {
	case serveErr := <-errCh:
		elapsed := time.Since(start)
		if elapsed > 500*time.Millisecond {
			t.Fatalf("shutdown took %s, must be < 500ms", elapsed)
		}
		if serveErr != nil {
			t.Errorf("Serve returned error: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s after cancel")
	}
}

// With inherited-lock PID file, the file is never deleted (flock+unlink race prevention).
// Instead, the lock is released on exit — IsHeld becomes false.

// TestServe_LockReleasedOnShutdown verifies that Serve releases the PID file lock
// when shutting down, making the file available for a new instance.
func TestServe_LockReleasedOnShutdown(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, Config{Name: "cad2", DataDir: dir}, nil) }()
	store := newDefaultPIDStore(dir, "cad2")
	waitOwnPIDFile(t, store)

	// Lock is held while Serve is running.
	pidPath := filepath.Join(dir, "cad2.pid")
	if !pidlock.IsHeld(pidPath) {
		t.Fatal("lock must be held while Serve is running")
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Serve: %v", err)
	}

	// After shutdown, lock is released — file stays but IsHeld is false.
	if pidlock.IsHeld(pidPath) {
		t.Fatal("lock must be released after Serve shuts down")
	}
}
