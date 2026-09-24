package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

	// Write the stop-intent marker (simulating a prior Hold).
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

func TestStopIntent_StopDoesNotWriteMarker(t *testing.T) {
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
	err := d.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Stop must NOT write marker (K1 fix: stop-intent is opt-in via Hold).
	if hasStopIntent(dataDir, "testapp") {
		t.Error("Stop must NOT write the stop-intent marker; use Hold instead")
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

	// Stop twice — both must succeed, no marker written.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if hasStopIntent(dataDir, "testapp") {
		t.Error("Stop must not write marker")
	}

	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if hasStopIntent(dataDir, "testapp") {
		t.Error("Stop must not write marker on second call either")
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

func TestStopIntent_FullCycle_HoldBlocksEnsure_StartResumes(t *testing.T) {
	// End-to-end cycle: Hold → EnsureRunning (blocked) → Start (clears) → EnsureRunning (ok).
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

	// Step 1: Hold (stops + writes marker).
	if err := d.Hold(context.Background()); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if !hasStopIntent(dataDir, "testapp") {
		t.Fatal("Hold must write stop-intent marker")
	}

	// Step 2: EnsureRunning — must fail with ErrStopIntent.
	_, err := d.EnsureRunning(context.Background())
	if !errors.Is(err, ErrStopIntent) {
		t.Fatalf("EnsureRunning after Hold: expected ErrStopIntent, got: %v", err)
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

// TestHold_WritesMarker verifies that Hold() stops the daemon AND writes the
// stop-intent marker.
func TestHold_WritesMarker(t *testing.T) {
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

	if hasStopIntent(dataDir, "testapp") {
		t.Fatal("marker must not exist before Hold")
	}

	err := d.Hold(context.Background())
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	if !hasStopIntent(dataDir, "testapp") {
		t.Error("Hold must write the stop-intent marker")
	}
}

// TestHold_BlocksEnsureRunning verifies that after Hold(), EnsureRunning
// returns ErrStopIntent.
func TestHold_BlocksEnsureRunning(t *testing.T) {
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

	if err := d.Hold(context.Background()); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	_, err := d.EnsureRunning(context.Background())
	if !errors.Is(err, ErrStopIntent) {
		t.Fatalf("EnsureRunning after Hold: expected ErrStopIntent, got: %v", err)
	}
}

// TestRelease_ClearsMarker verifies that Release() clears the stop-intent
// marker so EnsureRunning can proceed.
func TestRelease_ClearsMarker(t *testing.T) {
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

	// Hold writes marker.
	if err := d.Hold(context.Background()); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if !hasStopIntent(dataDir, "testapp") {
		t.Fatal("marker must exist after Hold")
	}

	// Release clears marker.
	d.Release()
	if hasStopIntent(dataDir, "testapp") {
		t.Error("Release must clear the stop-intent marker")
	}

	// EnsureRunning should not return ErrStopIntent anymore.
	_, err := d.EnsureRunning(context.Background())
	if errors.Is(err, ErrStopIntent) {
		t.Fatal("EnsureRunning after Release must not return ErrStopIntent")
	}
}

// ---------------------------------------------------------------------------
// H1: Integration test — EnsureRunning → Stop → EnsureRunning (no ErrStopIntent)
// ---------------------------------------------------------------------------

// TestHelperStopIntent is the daemon child process for stop-intent integration tests.
func TestHelperStopIntent(t *testing.T) {
	if os.Getenv("DAEMON_MODE") != "1" {
		t.Skip("helper process only")
	}
	cfg := Config{Name: "sitest", DataDir: os.Getenv("DAEMON_DATA_DIR")}
	if err := Serve(context.Background(), cfg, nil); err != nil {
		fmt.Fprintln(os.Stderr, "stop-intent helper serve:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestH1_EnsureRunning_Stop_EnsureRunning is the most important scenario:
//  1. EnsureRunning → daemon starts, returns info
//  2. Stop → daemon stops
//  3. EnsureRunning → daemon restarts (NO ErrStopIntent!)
//
// This MUST work without Hold(). Stop alone must not prevent restarts.
func TestH1_EnsureRunning_Stop_EnsureRunning(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:    "sitest",
		DataDir: dir,
		Binary:  os.Args[0],
		Args:    []string{"-test.run=^TestHelperStopIntent$", "-test.timeout=5m"},
		Timeout: 15 * time.Second,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = New(cfg).Stop(ctx)
	})

	d := New(cfg)

	// Step 1: EnsureRunning → starts daemon.
	info, err := d.EnsureRunning(context.Background())
	if err != nil {
		t.Fatalf("first EnsureRunning: %v", err)
	}
	if info.Port == 0 {
		t.Fatal("first EnsureRunning must return non-zero port")
	}
	firstPort := info.Port

	// Verify daemon is alive.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", firstPort)) //nolint:noctx,gosec // test-only
	if err != nil {
		t.Fatalf("health check on first daemon: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}

	// Step 2: Stop → daemon stops. No marker written.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify no stop-intent marker.
	if hasStopIntent(dir, "sitest") {
		t.Fatal("Stop must NOT write stop-intent marker")
	}

	// Wait for daemon to actually stop.
	deadline := time.Now().Add(10 * time.Second)
	for d.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if d.IsRunning() {
		t.Fatal("daemon did not stop within 10s")
	}

	// Step 3: EnsureRunning → daemon RESTARTS (no ErrStopIntent).
	info2, err := d.EnsureRunning(context.Background())
	if err != nil {
		if errors.Is(err, ErrStopIntent) {
			t.Fatal("EnsureRunning after Stop returned ErrStopIntent — this is the K1 bug")
		}
		t.Fatalf("second EnsureRunning: %v", err)
	}
	if info2.Port == 0 {
		t.Fatal("second EnsureRunning must return non-zero port")
	}

	// Verify second daemon is alive.
	resp2, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", info2.Port)) //nolint:noctx,gosec // test-only
	if err != nil {
		t.Fatalf("health check on second daemon: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second daemon health status = %d, want 200", resp2.StatusCode)
	}
}
