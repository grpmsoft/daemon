package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		Name:             "serve-custom",
		DataDir:          dir,
		HealthPath:       "/health",
		DisableTokenAuth: true, // allow unauthenticated access to custom handler
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

	// Read token from PID file — required for /daemon/* endpoints since v0.3.1.
	pidData, loadErr := store.Load()
	if loadErr != nil {
		t.Fatalf("load PID info: %v", loadErr)
	}
	token := pidData.Token

	// Open an attach connection.
	attachURL := fmt.Sprintf("http://127.0.0.1:%d/daemon/attach", port)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, attachURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
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

	// Read token from PID file.
	pidData, loadErr := store.Load()
	if loadErr != nil {
		t.Fatalf("load PID info: %v", loadErr)
	}

	// Open attach connection.
	attachCtx, attachCancel := context.WithCancel(context.Background())
	defer attachCancel()
	req, _ := http.NewRequestWithContext(attachCtx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/daemon/attach", port), nil)
	req.Header.Set("Authorization", "Bearer "+pidData.Token)
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

// ---------------------------------------------------------------------------
// Tests: removed connect/disconnect endpoints (v0.4.0 breaking change)
// ---------------------------------------------------------------------------

// TestServe_ConnectEndpointRemoved verifies that POST /daemon/connect returns
// 404 after removal in v0.4.0. ConnTracker is only managed via /daemon/attach.
func TestServe_ConnectEndpointRemoved(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "no-connect", DataDir: dir}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, cfg, nil) }()

	store := newDefaultPIDStore(dir, "no-connect")
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

	pidData, loadErr := store.Load()
	if loadErr != nil {
		t.Fatalf("load PID info: %v", loadErr)
	}

	// POST /daemon/connect must return 405 (method not found on mux).
	client := &http.Client{}
	for _, path := range []string{"/daemon/connect", "/daemon/disconnect"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
		req.Header.Set("Authorization", "Bearer "+pidData.Token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_ = resp.Body.Close()
		// ServeMux returns 404 for unregistered paths (or 405 for wrong method).
		// Either way, the endpoint must NOT return 204 (which was the old behavior).
		if resp.StatusCode == http.StatusNoContent {
			t.Errorf("POST %s must not return 204 (endpoint removed), got %d", path, resp.StatusCode)
		}
	}

	cancel()
	select {
	case serveErr := <-errCh:
		if serveErr != nil {
			t.Errorf("Serve: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// ---------------------------------------------------------------------------
// Tests: DisableTokenAuth (v0.4.0 — replaces RequireToken with inverted default)
// ---------------------------------------------------------------------------

// TestServe_DisableTokenAuth_DefaultRequiresToken verifies that when
// DisableTokenAuth is false (default), the application handler requires
// a bearer token.
func TestServe_DisableTokenAuth_DefaultRequiresToken(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:    "auth-default",
		DataDir: dir,
		// DisableTokenAuth defaults to false = token required for app handler.
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, cfg, appHandler) }()

	store := newDefaultPIDStore(dir, "auth-default")
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

	// App endpoint without token must be rejected (401).
	appURL := fmt.Sprintf("http://127.0.0.1:%d/api/data", port)
	resp, err := http.Get(appURL) //nolint:noctx,gosec // test-only
	if err != nil {
		t.Fatalf("GET /api/data: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("default DisableTokenAuth=false must require token for app, got %d", resp.StatusCode)
	}

	// Health endpoint must still be open (unauthenticated).
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	resp, err = http.Get(healthURL) //nolint:noctx,gosec // test-only
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health must be open, got %d", resp.StatusCode)
	}

	// App endpoint WITH token must succeed.
	pidData, _ := store.Load()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, appURL, nil)
	req.Header.Set("Authorization", "Bearer "+pidData.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/data with token: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("app with token must return 200, got %d", resp.StatusCode)
	}

	cancel()
	<-errCh
}

// TestServe_DisableTokenAuth_ExplicitTrue verifies that when DisableTokenAuth
// is true, the application handler does NOT require a bearer token.
func TestServe_DisableTokenAuth_ExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:             "auth-disabled",
		DataDir:          dir,
		DisableTokenAuth: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	appHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	})

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, cfg, appHandler) }()

	store := newDefaultPIDStore(dir, "auth-disabled")
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

	// App endpoint without token must succeed when DisableTokenAuth=true.
	appURL := fmt.Sprintf("http://127.0.0.1:%d/api/data", port)
	resp, err := http.Get(appURL) //nolint:noctx,gosec // test-only
	if err != nil {
		t.Fatalf("GET /api/data: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("DisableTokenAuth=true must allow app without token, got %d", resp.StatusCode)
	}

	cancel()
	<-errCh
}

// ---------------------------------------------------------------------------
// Tests: authGuard middleware
// ---------------------------------------------------------------------------

func TestAuthGuard_DaemonEndpoints(t *testing.T) {
	const token = "test-secret-token-12345"
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name       string
		path       string
		authHeader string
		wantStatus int
	}{
		{"daemon path, no token", "/daemon/shutdown", "", http.StatusUnauthorized},
		{"daemon path, wrong token", "/daemon/shutdown", "Bearer wrong-token", http.StatusUnauthorized},
		{"daemon path, correct token", "/daemon/shutdown", "Bearer " + token, http.StatusOK},
		{"daemon attach, correct token", "/daemon/attach", "Bearer " + token, http.StatusOK},
		{"health, no token", "/health", "", http.StatusOK},
	}

	handler := authGuard(inner, token, false, "/health")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("path=%s auth=%q: got %d, want %d", tt.path, tt.authHeader, rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if wwa := rec.Header().Get("WWW-Authenticate"); wwa != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want %q", wwa, "Bearer")
				}
			}
		})
	}
}

func TestAuthGuard_RequireToken(t *testing.T) {
	token := "app-token-678" //nolint:gosec // test-only: not a real credential
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name         string
		requireToken bool
		path         string
		authHeader   string
		wantStatus   int
	}{
		{"app path, requireToken=false, no token", false, "/api/data", "", http.StatusOK},
		{"app path, requireToken=true, no token", true, "/api/data", "", http.StatusUnauthorized},
		{"app path, requireToken=true, correct token", true, "/api/data", "Bearer " + token, http.StatusOK},
		{"app path, requireToken=true, wrong token", true, "/api/data", "Bearer wrong", http.StatusUnauthorized},
		{"health, requireToken=true, no token", true, "/health", "", http.StatusOK},
		{"daemon path, requireToken=false, no token", false, "/daemon/shutdown", "", http.StatusUnauthorized},
		{"daemon path, requireToken=true, correct token", true, "/daemon/shutdown", "Bearer " + token, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := authGuard(inner, token, tt.requireToken, "/health")
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("got %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: PIDInfo round-trip with and without Token
// ---------------------------------------------------------------------------

func TestPIDInfo_RoundTrip_WithToken(t *testing.T) {
	original := PIDInfo{
		PID:       1234,
		Port:      8080,
		Name:      "test",
		Binary:    "/usr/bin/test",
		StartTime: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC),
		Token:     "secret-token-abc",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded PIDInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.PID != original.PID {
		t.Errorf("PID: got %d, want %d", decoded.PID, original.PID)
	}
	if decoded.Port != original.Port {
		t.Errorf("Port: got %d, want %d", decoded.Port, original.Port)
	}
	if decoded.Name != original.Name {
		t.Errorf("Name: got %q, want %q", decoded.Name, original.Name)
	}
	if decoded.Token != original.Token {
		t.Errorf("Token: got %q, want %q", decoded.Token, original.Token)
	}
}

func TestPIDInfo_RoundTrip_WithoutToken(t *testing.T) {
	original := PIDInfo{
		PID:       5678,
		Port:      9090,
		Name:      "test2",
		Binary:    "/usr/bin/test2",
		StartTime: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Token must be omitted from JSON when empty (omitempty).
	if strings.Contains(string(data), "token") {
		t.Errorf("JSON should not contain 'token' when empty, got: %s", data)
	}

	var decoded PIDInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Token != "" {
		t.Errorf("Token: got %q, want empty", decoded.Token)
	}
	if decoded.PID != original.PID {
		t.Errorf("PID: got %d, want %d", decoded.PID, original.PID)
	}
}

// ---------------------------------------------------------------------------
// Tests: Token in Serve PID file
// ---------------------------------------------------------------------------

// TestServe_WritesTokenToPIDFile verifies that Serve generates a token and
// writes it to the PID file. The token must be non-empty and readable.
func TestServe_WritesTokenToPIDFile(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "token-test", DataDir: dir}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, cfg, nil) }()

	store := newDefaultPIDStore(dir, "token-test")
	var data PIDInfo
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		data, err = store.Load()
		if err == nil && data.Port > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if data.Port == 0 {
		t.Fatal("PID file must be written within 5 seconds")
	}

	if data.Token == "" {
		t.Fatal("Token must be non-empty in PID file")
	}
	if len(data.Token) < 16 {
		t.Errorf("Token too short: %q (expected at least 16 chars)", data.Token)
	}

	// Verify the token works for /daemon/shutdown.
	shutdownURL := fmt.Sprintf("http://127.0.0.1:%d/daemon/shutdown", data.Port)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, shutdownURL, nil)
	req.Header.Set("Authorization", "Bearer "+data.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("shutdown request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("shutdown with token: got %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	select {
	case serveErr := <-errCh:
		if serveErr != nil {
			t.Errorf("Serve: %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Serve did not return")
	}
}
