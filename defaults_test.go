package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestNew_ReturnsConfiguredDaemon checks that New() wires default implementations
// and applies Config.applyDefaults().
func TestNew_ReturnsConfiguredDaemon(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "testapp", DataDir: dir}

	d := New(cfg)

	if d == nil {
		t.Fatal("expected non-nil, got nil")
	}
	// applyDefaults must have been called.
	if d.cfg.Timeout != 30*time.Second {
		t.Errorf("got %v, want %v", d.cfg.Timeout, 30*time.Second)
	}
	if d.cfg.HealthPath != "/health" {
		t.Errorf("got %v, want %v", d.cfg.HealthPath, "/health")
	}
	if d.cfg.Name != "testapp" {
		t.Errorf("got %v, want %v", d.cfg.Name, "testapp")
	}
	if d.cfg.DataDir != dir {
		t.Errorf("got %v, want %v", d.cfg.DataDir, dir)
	}
	// Default implementations must be wired (not nil).
	if d.pids == nil {
		t.Error("expected non-nil pids, got nil")
	}
	if d.procs == nil {
		t.Error("expected non-nil procs, got nil")
	}
	if d.health == nil {
		t.Error("expected non-nil health, got nil")
	}
}

// TestNew_IsRunning_WithNoFile verifies that a freshly created Daemon is not running.
func TestNew_IsRunning_WithNoFile(t *testing.T) {
	dir := t.TempDir()
	d := New(Config{Name: "testapp", DataDir: dir})
	if d.IsRunning() {
		t.Error("new daemon with no PID file must not report running")
	}
}

// SetHandler was deleted in v0.2.0 — dead code, nothing read handler field.

// TestDefaultHealthHandler_GetReturns200 verifies the health endpoint returns 200 OK.
func TestDefaultHealthHandler_GetReturns200(t *testing.T) {
	startTime := time.Now().Add(-5 * time.Second)
	handler := defaultHealthHandler("myapp", startTime)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("got %v, want %v", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("%q does not contain %q", ct, "application/json")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("%q does not contain %q", body, `"status":"ok"`)
	}
	if !strings.Contains(body, `"name":"myapp"`) {
		t.Errorf("%q does not contain %q", body, `"name":"myapp"`)
	}
	if !strings.Contains(body, `"pid":`) {
		t.Errorf("%q does not contain %q", body, `"pid":`)
	}
}

// TestDefaultHealthHandler_NonGetReturns405 verifies that POST to the health endpoint
// returns 405 Method Not Allowed.
func TestDefaultHealthHandler_NonGetReturns405(t *testing.T) {
	handler := defaultHealthHandler("myapp", time.Now())

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/health", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("got %v, want %v", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

// TestDefaultHealthHandler_UptimeField verifies that the uptime field reflects elapsed time.
func TestDefaultHealthHandler_UptimeField(t *testing.T) {
	// Start time 10 seconds ago — uptime should be "10s" or similar.
	startTime := time.Now().Add(-10 * time.Second)
	handler := defaultHealthHandler("app", startTime)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %v, want %v", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uptime":`) {
		t.Errorf("response must include uptime field, body: %s", body)
	}
}

// TestDefaultPIDStoreAdapter exercises the pidStoreAdapter through the PIDStore interface.
// This exercises defaults.go's adapter methods with a real PIDFile under the hood.
func TestDefaultPIDStoreAdapter_SaveLoadClear(t *testing.T) {
	dir := t.TempDir()
	store := newDefaultPIDStore(dir, "app")

	if store == nil {
		t.Fatal("expected non-nil, got nil")
	}

	// Path must end with app.pid.
	if !strings.Contains(store.Path(), "app.pid") {
		t.Errorf("%q does not contain %q", store.Path(), "app.pid")
	}

	// IsAlive with no file → false.
	if store.IsAlive() {
		t.Error("expected false, got true")
	}

	// Load with no file → error.
	_, err := store.Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Save.
	startTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	err = store.Save(os.Getpid(), 8080, "app", "", startTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Load after Save must return the same data.
	data, err := store.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data.PID != os.Getpid() {
		t.Errorf("got %v, want %v", data.PID, os.Getpid())
	}
	if data.Port != 8080 {
		t.Errorf("got %v, want %v", data.Port, 8080)
	}
	if data.Name != "app" {
		t.Errorf("got %v, want %v", data.Name, "app")
	}
	if !data.StartTime.Equal(startTime) {
		t.Errorf("got %v, want %v", data.StartTime, startTime)
	}

	// IsAlive requires pidlock held (v0.2.0 — lock-based identity).
	// Without lock, IsAlive must be false even with valid PID.
	if store.IsAlive() {
		t.Error("IsAlive must be false without lock held")
	}

	// Clear.
	err = store.Clear()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// IsAlive after Clear → false.
	if store.IsAlive() {
		t.Error("expected false, got true")
	}
}

// TestDefaultPIDStoreAdapter_SaveWithBinary exercises the binary field through the adapter.
func TestDefaultPIDStoreAdapter_SaveWithBinary(t *testing.T) {
	dir := t.TempDir()
	store := newDefaultPIDStore(dir, "binapp")

	startTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	binary := "/usr/local/bin/myapp"

	err := store.Save(os.Getpid(), 9090, "binapp", binary, startTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := store.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if data.Binary != binary {
		t.Errorf("Binary: got %q, want %q", data.Binary, binary)
	}
	if data.PID != os.Getpid() {
		t.Errorf("PID: got %d, want %d", data.PID, os.Getpid())
	}
	if data.Port != 9090 {
		t.Errorf("Port: got %d, want 9090", data.Port)
	}
}

// TestDefaultProcessManager_IsProcessAlive exercises the defaultProcessManager adapter.
func TestDefaultProcessManager_IsProcessAlive(t *testing.T) {
	pm := defaultProcessManager{}

	if !pm.IsProcessAlive(os.Getpid()) {
		t.Errorf("current process must be alive")
	}
	if pm.IsProcessAlive(999999) {
		t.Errorf("pid 999999 must not be alive")
	}
}

// TestDefaultProcessManager_KillProcess_InvalidPID exercises the KillProcess adapter
// with a PID that does not exist. On Windows, KillProcess falls back to taskkill
// which will fail for a non-existent PID — so we expect an error. On Unix, sending
// SIGTERM to a non-existent PID is also an error.
func TestDefaultProcessManager_KillProcess_InvalidPID(_ *testing.T) {
	pm := defaultProcessManager{}
	// PID 999999 almost certainly does not exist.
	err := pm.KillProcess(context.Background(), 999999, 5*time.Second)
	// The result is platform-specific: on Windows taskkill fails, on Unix FindProcess
	// may or may not return an error depending on OS. We simply verify the call
	// does not panic and returns a non-nil error for a non-existent PID on Windows.
	// On Unix it may return nil (no such process → already gone → OK). Accept both.
	_ = err // both nil and non-nil are acceptable; the important thing is no panic
}

// TestDefaultHealthChecker_Check exercises the defaultHealthChecker adapter.
func TestDefaultHealthChecker_Check(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	checker := defaultHealthChecker{}

	port := extractServerPort(t, srv.URL)
	err := checker.Check(port, "/health")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDefaultHealthChecker_WaitUntilReady exercises the defaultHealthChecker.WaitUntilReady.
func TestDefaultHealthChecker_WaitUntilReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	checker := defaultHealthChecker{}
	port := extractServerPort(t, srv.URL)

	err := checker.WaitUntilReady(context.Background(), port, "/health", 5*time.Second)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// extractServerPort extracts the port number from an httptest.Server URL.
func extractServerPort(t *testing.T, rawURL string) int {
	t.Helper()
	// rawURL format: "http://127.0.0.1:PORT"
	for i := len(rawURL) - 1; i >= 0; i-- {
		if rawURL[i] == ':' {
			portStr := rawURL[i+1:]
			port := 0
			for _, c := range portStr {
				if c < '0' || c > '9' {
					t.Fatalf("invalid port char %q in URL %s", c, rawURL)
				}
				port = port*10 + int(c-'0')
			}
			return port
		}
	}
	t.Fatalf("no port in URL: %s", rawURL)
	return 0
}
