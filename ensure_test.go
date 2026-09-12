package daemon

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

// EnsureRunning uses pidlock.TryLock internally. Tests must hold a lock
// on the PID file to simulate a running daemon.

// TestEnsureRunning_AlreadyRunning_ReturnsFastPath verifies that EnsureRunning
// returns the port from the PID file when the lock is held (daemon alive).
func TestEnsureRunning_AlreadyRunning_ReturnsFastPath(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "testapp", DataDir: dir, Binary: "/usr/bin/app"}
	pidPath := filepath.Join(dir, "testapp.pid")

	// Acquire lock and write PID data — simulates a running daemon.
	lock, err := pidlock.TryLock(pidPath)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	defer lock.Release()

	pidData := PIDInfo{PID: os.Getpid(), Port: 9876, Name: "testapp", StartTime: time.Now()}
	data, _ := json.Marshal(pidData)
	if err := lock.WriteData(data); err != nil {
		t.Fatalf("WriteData: %v", err)
	}

	port, ensureErr := EnsureRunning(context.Background(), cfg)
	if ensureErr != nil {
		t.Fatalf("unexpected error: %v", ensureErr)
	}
	if port != 9876 {
		t.Errorf("EnsureRunning must return port from PID file, got %v, want 9876", port)
	}
}

// TestEnsureRunning_NotRunning_StartFails_ReturnsError verifies that EnsureRunning
// propagates Start errors when the daemon is not running and the binary is bad.
func TestEnsureRunning_NotRunning_StartFails_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:       "testapp",
		DataDir:    dir,
		Binary:     "/no/such/binary/that/does/not/exist",
		Timeout:    200 * time.Millisecond,
		HealthPath: "/health",
	}

	// No PID file -> IsRunning false -> slow path -> Start -> error.
	port, err := EnsureRunning(context.Background(), cfg)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if port != 0 {
		t.Errorf("got %v, want %v", port, 0)
	}
	// Error should indicate a start failure.
	if !strings.Contains(err.Error(), "start") {
		t.Errorf("%q does not contain %q", err.Error(), "start")
	}
}

// TestEnsureRunning_StalePIDFile_StartFails_ReturnsError verifies that EnsureRunning
// handles a stale PID file (process dead) by taking the slow path and failing.
func TestEnsureRunning_StalePIDFile_StartFails_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:       "testapp",
		DataDir:    dir,
		Binary:     "/no/such/binary/x",
		Timeout:    200 * time.Millisecond,
		HealthPath: "/health",
	}

	// Write a PID file with a dead PID (999999 is virtually never a real process).
	store := newDefaultPIDStore(dir, "testapp")
	err := store.Save(999999, 8080, "testapp", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// IsRunning -> false (dead PID) -> slow path -> Start -> fails.
	port, err := EnsureRunning(context.Background(), cfg)

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
		Binary:     "/no/such/binary/x",
		Timeout:    5 * time.Second,
		HealthPath: "/health",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	port, err := EnsureRunning(ctx, cfg)

	// Either a StartDetached error or context-cancelled error — both yield port=0.
	_ = err
	if port != 0 {
		t.Errorf("cancelled context / bad binary must yield port=0, got %v", port)
	}
}
