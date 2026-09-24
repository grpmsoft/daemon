package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Unit tests: stop-intent file operations (no subprocess).
// ---------------------------------------------------------------------------

func TestWriteStopIntent_CreatesFile(t *testing.T) {
	dataDir := t.TempDir()
	name := "testapp"

	if err := writeStopIntent(dataDir, name); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}

	path := stopIntentPath(dataDir, name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("marker file must exist after writeStopIntent: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("marker file must be empty, got %d bytes", info.Size())
	}
}

func TestWriteStopIntent_FilePermissions(t *testing.T) {
	dataDir := t.TempDir()
	name := "testapp"

	if err := writeStopIntent(dataDir, name); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}

	path := stopIntentPath(dataDir, name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// On Unix: 0600 means owner read/write only.
	// On Windows: os.FileMode permissions are approximate (Go maps
	// everything through ACLs — the reported mode is always 0666 or 0444).
	// We only verify the permission intent on Unix.
	if runtime.GOOS != "windows" {
		perm := info.Mode().Perm()
		if perm&0o077 != 0 {
			t.Errorf("marker file has permissions %04o, expected no group/other bits", perm)
		}
	} else if !info.Mode().IsRegular() {
		// On Windows, os.FileMode permissions are approximate (Go maps
		// through ACLs — reported mode is always 0666 or 0444).
		// Verify the file exists and is regular instead.
		t.Errorf("marker file must be a regular file, got mode %v", info.Mode())
	}
}

func TestClearStopIntent_RemovesFile(t *testing.T) {
	dataDir := t.TempDir()
	name := "testapp"

	// Create the marker.
	if err := writeStopIntent(dataDir, name); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}
	if !hasStopIntent(dataDir, name) {
		t.Fatal("marker must exist before clear")
	}

	// Clear the marker.
	clearStopIntent(dataDir, name)

	if hasStopIntent(dataDir, name) {
		t.Error("marker must not exist after clearStopIntent")
	}
}

func TestClearStopIntent_NoErrorIfAbsent(t *testing.T) {
	dataDir := t.TempDir()
	name := "testapp"

	// Must not panic or error when the file does not exist.
	clearStopIntent(dataDir, name)

	if hasStopIntent(dataDir, name) {
		t.Error("hasStopIntent must return false for non-existent file")
	}
}

func TestHasStopIntent_TrueWhenExists(t *testing.T) {
	dataDir := t.TempDir()
	name := "testapp"

	if err := writeStopIntent(dataDir, name); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}

	if !hasStopIntent(dataDir, name) {
		t.Error("hasStopIntent must return true when marker file exists")
	}
}

func TestHasStopIntent_FalseWhenAbsent(t *testing.T) {
	dataDir := t.TempDir()
	name := "testapp"

	if hasStopIntent(dataDir, name) {
		t.Error("hasStopIntent must return false when marker file does not exist")
	}
}

func TestStopIntentPath_Format(t *testing.T) {
	got := stopIntentPath("/data", "myapp")
	want := filepath.Join("/data", "myapp.stop-intent")
	if got != want {
		t.Errorf("stopIntentPath = %q, want %q", got, want)
	}
}

func TestWriteStopIntent_Idempotent(t *testing.T) {
	dataDir := t.TempDir()
	name := "testapp"

	// Write twice — must not error on overwrite.
	if err := writeStopIntent(dataDir, name); err != nil {
		t.Fatalf("first writeStopIntent: %v", err)
	}
	if err := writeStopIntent(dataDir, name); err != nil {
		t.Fatalf("second writeStopIntent: %v", err)
	}

	if !hasStopIntent(dataDir, name) {
		t.Error("marker must still exist after double write")
	}
}

// ---------------------------------------------------------------------------
// Integration tests: stop-intent with real daemon lifecycle.
// Uses mock-based Daemon (NewWithDeps) with real file system for DataDir.
// ---------------------------------------------------------------------------

func TestStopIntent_BlocksEnsureRunning(t *testing.T) {
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	pids := &pidStoreMock{
		loadErr:    errors.New("pid file not found"),
		pathResult: pidPath,
	}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Binary:     "/nonexistent/binary",
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Write the stop-intent marker (simulating a prior Stop).
	if err := writeStopIntent(dataDir, "testapp"); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}

	// EnsureRunning must refuse to start and return ErrStopIntent.
	_, err := d.EnsureRunning(context.Background())
	if err == nil {
		t.Fatal("expected ErrStopIntent, got nil")
	}
	if !errors.Is(err, ErrStopIntent) {
		t.Errorf("expected ErrStopIntent, got: %v", err)
	}

	// Marker must still exist (EnsureRunning does not clear it).
	if !hasStopIntent(dataDir, "testapp") {
		t.Error("stop-intent marker must persist after EnsureRunning rejection")
	}
}

func TestStopIntent_ClearedByStart(t *testing.T) {
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	pids := &pidStoreMock{
		loadErr:    errors.New("pid file not found"),
		pathResult: pidPath,
	}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Binary:     "/nonexistent/binary",
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Write the stop-intent marker.
	if err := writeStopIntent(dataDir, "testapp"); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}
	if !hasStopIntent(dataDir, "testapp") {
		t.Fatal("marker must exist before Start")
	}

	// Start will fail (non-existent binary), but it clears the marker first.
	_, _ = d.Start(context.Background())

	if hasStopIntent(dataDir, "testapp") {
		t.Error("Start must clear the stop-intent marker even if exec fails")
	}
}

func TestStopIntent_ClearedByRestart(t *testing.T) {
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	pids := &pidStoreMock{
		loadErr:     errors.New("pid file not found"),
		aliveResult: false,
		pathResult:  pidPath,
	}
	procs := &mockProcessManager{aliveResult: false}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Binary:     "/nonexistent/binary",
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Write the stop-intent marker.
	if err := writeStopIntent(dataDir, "testapp"); err != nil {
		t.Fatalf("writeStopIntent: %v", err)
	}
	if !hasStopIntent(dataDir, "testapp") {
		t.Fatal("marker must exist before Restart")
	}

	// Restart will fail (non-existent binary), but it clears the marker first.
	_, _ = d.Restart(context.Background())

	if hasStopIntent(dataDir, "testapp") {
		t.Error("Restart must clear the stop-intent marker even if exec fails")
	}
}

func TestStopIntent_StopWritesMarker(t *testing.T) {
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	pids := &pidStoreMock{
		loadErr:     errors.New("pid file not found"),
		aliveResult: false,
		pathResult:  pidPath,
	}
	procs := &mockProcessManager{aliveResult: false}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    100 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// No marker before Stop.
	if hasStopIntent(dataDir, "testapp") {
		t.Fatal("marker must not exist before Stop")
	}

	// Stop on already-stopped daemon is idempotent (returns nil).
	// stopLocked returns nil when no PID file exists.
	err := d.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Marker must be created because stopLocked returned nil.
	if !hasStopIntent(dataDir, "testapp") {
		t.Error("Stop must write the stop-intent marker on success")
	}
}

func TestStopIntent_IdempotentDoubleStop(t *testing.T) {
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	pids := &pidStoreMock{
		loadErr:     errors.New("pid file not found"),
		aliveResult: false,
		pathResult:  pidPath,
	}
	procs := &mockProcessManager{aliveResult: false}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    100 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Stop twice — both must succeed, marker must be present after each.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if !hasStopIntent(dataDir, "testapp") {
		t.Error("marker must exist after first Stop")
	}

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if !hasStopIntent(dataDir, "testapp") {
		t.Error("marker must exist after second Stop")
	}
}

func TestStopIntent_NoMarkerWhenStopFails(t *testing.T) {
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	pids := &pidStoreMock{
		saved:       &PIDInfo{PID: 555, Port: 7070},
		aliveResult: true,
		pathResult:  pidPath,
	}
	procs := &mockProcessManager{
		aliveResult: true,
		killErr:     errors.New("permission denied"),
	}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    100 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Stop will fail because KillProcess returns "permission denied".
	err := d.Stop(context.Background())
	if err == nil {
		t.Fatal("expected error from Stop, got nil")
	}

	// Marker must NOT be written because stopLocked failed.
	if hasStopIntent(dataDir, "testapp") {
		t.Error("stop-intent marker must not be written when Stop fails")
	}
}

func TestStopIntent_FullCycle_StopBlocksEnsure_StartResumes(t *testing.T) {
	// End-to-end cycle: Start → Stop → EnsureRunning (blocked) → Start (clears) → EnsureRunning (ok).
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	pids := &pidStoreMock{
		loadErr:     errors.New("pid file not found"),
		aliveResult: false,
		pathResult:  pidPath,
	}
	procs := &mockProcessManager{aliveResult: false}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Binary:     "/nonexistent/binary",
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Step 1: Stop (idempotent on non-running) — writes marker.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Step 2: EnsureRunning — must fail with ErrStopIntent.
	_, err := d.EnsureRunning(context.Background())
	if !errors.Is(err, ErrStopIntent) {
		t.Fatalf("EnsureRunning after Stop: expected ErrStopIntent, got: %v", err)
	}

	// Step 3: Start — clears marker (exec will fail, that's ok).
	_, _ = d.Start(context.Background())

	// Step 4: EnsureRunning — marker cleared, should attempt to start (will fail at exec).
	_, err = d.EnsureRunning(context.Background())
	if errors.Is(err, ErrStopIntent) {
		t.Fatal("EnsureRunning after Start must not return ErrStopIntent")
	}
	// The error should be from exec (non-existent binary), not from stop-intent.
}
