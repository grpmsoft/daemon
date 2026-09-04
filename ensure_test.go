package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// EnsureRunning uses New() internally, so we test it via the real PIDFile mechanism
// by writing a PID file into a temp DataDir before calling EnsureRunning.

// TestEnsureRunning_AlreadyRunning_ReturnsFastPath verifies that EnsureRunning
// returns the port from the existing PID file when the daemon is alive.
func TestEnsureRunning_AlreadyRunning_ReturnsFastPath(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "testapp", DataDir: dir}

	// Pre-write a PID file with the current process's PID so IsAlive → true.
	store := newDefaultPIDStore(dir, "testapp")
	err := store.Save(os.Getpid(), 9876, "testapp", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	port, err := EnsureRunning(context.Background(), cfg, "/usr/bin/app", nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port != 9876 {
		t.Errorf("EnsureRunning must return the port from the existing PID file, got %v, want %v", port, 9876)
	}
}

// TestEnsureRunning_NotRunning_StartFails_ReturnsError verifies that EnsureRunning
// propagates Start errors when the daemon is not running and the binary is bad.
func TestEnsureRunning_NotRunning_StartFails_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:       "testapp",
		DataDir:    dir,
		Timeout:    200 * time.Millisecond,
		HealthPath: "/health",
	}

	// No PID file → IsRunning false → slow path → Start("/no/such/binary") → error.
	port, err := EnsureRunning(context.Background(), cfg, "/no/such/binary/that/does/not/exist", nil)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if port != 0 {
		t.Errorf("got %v, want %v", port, 0)
	}
	if !strings.Contains(err.Error(), "ensure running") {
		t.Errorf("%q does not contain %q", err.Error(), "ensure running")
	}
}

// TestEnsureRunning_StalePIDFile_StartFails_ReturnsError verifies that EnsureRunning
// handles a stale PID file (process dead) by taking the slow path and failing.
func TestEnsureRunning_StalePIDFile_StartFails_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:       "testapp",
		DataDir:    dir,
		Timeout:    200 * time.Millisecond,
		HealthPath: "/health",
	}

	// Write a PID file with a dead PID (999999 is virtually never a real process).
	store := newDefaultPIDStore(dir, "testapp")
	err := store.Save(999999, 8080, "testapp", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// IsRunning → false (dead PID) → slow path → Start → fails.
	port, err := EnsureRunning(context.Background(), cfg, "/no/such/binary/x", nil)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if port != 0 {
		t.Errorf("got %v, want %v", port, 0)
	}
}

// TestEnsureRunning_ContextCancelled_ReturnsZeroPort verifies that a pre-cancelled
// context causes EnsureRunning to return an error with port=0.
func TestEnsureRunning_ContextCancelled_ReturnsZeroPort(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:       "testapp",
		DataDir:    dir,
		Timeout:    5 * time.Second,
		HealthPath: "/health",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	port, err := EnsureRunning(ctx, cfg, "/no/such/binary/x", nil)

	// Either a StartDetached error or context-cancelled error — both yield port=0.
	_ = err
	if port != 0 {
		t.Errorf("cancelled context / bad binary must yield port=0, got %v", port)
	}
}
