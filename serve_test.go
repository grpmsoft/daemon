package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
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

// V2c regression: Serve() must not remove a PID file a newer instance has overwritten.
func TestServe_CompareAndDelete_KeepsForeignPIDFile(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, Config{Name: "cad", DataDir: dir}, nil) }()
	store := newDefaultPIDStore(dir, "cad")
	waitOwnPIDFile(t, store)

	// A newer instance overwrites the PID file; we are now the orphan.
	if err := store.Save(os.Getpid()+1, 65000, "cad", "/other/binary", time.Now()); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	data, err := store.Load()
	if err != nil {
		t.Fatalf("V2c regression: orphan deleted the live daemon's PID file: %v", err)
	}
	if data.PID != os.Getpid()+1 {
		t.Fatalf("PID file changed: got %d, want %d", data.PID, os.Getpid()+1)
	}
}

// Positive case: Serve() does remove its own PID file.
func TestServe_CompareAndDelete_RemovesOwnPIDFile(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, Config{Name: "cad2", DataDir: dir}, nil) }()
	store := newDefaultPIDStore(dir, "cad2")
	waitOwnPIDFile(t, store)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("own PID file must be removed on shutdown, stat err=%v", err)
	}
}
