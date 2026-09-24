package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

// TestHelperCooldown is the daemon child process for cooldown integration tests.
// Skipped in the parent.
func TestHelperCooldown(t *testing.T) {
	if os.Getenv("DAEMON_MODE") != "1" {
		t.Skip("helper process only")
	}
	cfg := Config{Name: "cdtest", DataDir: os.Getenv("DAEMON_DATA_DIR")}
	if err := Serve(context.Background(), cfg, nil); err != nil {
		fmt.Fprintln(os.Stderr, "cooldown helper serve:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// ---------------------------------------------------------------------------
// Unit tests: cooldown file operations
// ---------------------------------------------------------------------------

func TestWriteCooldown_CreatesFileWithTimestamp(t *testing.T) {
	dir := t.TempDir()

	before := time.Now().UnixMilli()
	if err := writeCooldown(dir, "testapp"); err != nil {
		t.Fatalf("writeCooldown: %v", err)
	}
	after := time.Now().UnixMilli()

	path := cooldownPath(dir, "testapp")
	data, err := os.ReadFile(path) //nolint:gosec // test path from t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	ms, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		t.Fatalf("ParseInt: %v (data=%q)", err, string(data))
	}
	if ms < before || ms > after {
		t.Errorf("timestamp %d not in range [%d, %d]", ms, before, after)
	}
}

func TestIsCooldownActive_WithinTTL(t *testing.T) {
	dir := t.TempDir()

	if err := writeCooldown(dir, "testapp"); err != nil {
		t.Fatalf("writeCooldown: %v", err)
	}

	if !isCooldownActive(dir, "testapp", 5*time.Second) {
		t.Error("cooldown must be active immediately after write")
	}
}

func TestIsCooldownActive_Expired(t *testing.T) {
	dir := t.TempDir()
	path := cooldownPath(dir, "testapp")

	// Write a timestamp 10 seconds in the past.
	oldMs := time.Now().Add(-10 * time.Second).UnixMilli()
	if err := os.WriteFile(path, []byte(strconv.FormatInt(oldMs, 10)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if isCooldownActive(dir, "testapp", 5*time.Second) {
		t.Error("cooldown must be inactive after TTL expires")
	}
}

func TestIsCooldownActive_NoFile(t *testing.T) {
	dir := t.TempDir()

	if isCooldownActive(dir, "testapp", 5*time.Second) {
		t.Error("cooldown must be inactive when file does not exist")
	}
}

func TestIsCooldownActive_CorruptedFile(t *testing.T) {
	dir := t.TempDir()
	path := cooldownPath(dir, "testapp")

	if err := os.WriteFile(path, []byte("not-a-number"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if isCooldownActive(dir, "testapp", 5*time.Second) {
		t.Error("cooldown must be inactive for corrupted file")
	}

	// Corrupted file must be cleaned up.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("corrupted cooldown file must be removed, stat: %v", err)
	}
}

func TestClearCooldown_RemovesFile(t *testing.T) {
	dir := t.TempDir()

	if err := writeCooldown(dir, "testapp"); err != nil {
		t.Fatalf("writeCooldown: %v", err)
	}

	path := cooldownPath(dir, "testapp")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should exist after write: %v", err)
	}

	clearCooldown(dir, "testapp")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file must not exist after clear, stat: %v", err)
	}

	if isCooldownActive(dir, "testapp", 5*time.Second) {
		t.Error("cooldown must be inactive after clear")
	}
}

func TestClearCooldown_NoFile_NoPanic(t *testing.T) {
	dir := t.TempDir()
	// Must not panic or error when the file does not exist.
	clearCooldown(dir, "testapp")
}

func TestCooldownPath_Format(t *testing.T) {
	got := cooldownPath("/data", "myapp")
	want := filepath.Join("/data", "myapp.spawn-cooldown")
	if got != want {
		t.Errorf("cooldownPath = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Integration tests: cooldown behavior in EnsureRunning and Start
// ---------------------------------------------------------------------------

func TestSpawnCooldown_BlocksEnsureRunning(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:          "cdtest",
		DataDir:       dir,
		Binary:        "/no/such/binary/does/not/exist",
		Timeout:       200 * time.Millisecond,
		HealthPath:    "/health",
		SpawnCooldown: 5 * time.Second,
	}

	// First call: fails because binary does not exist.
	_, firstErr := EnsureRunning(context.Background(), cfg)
	if firstErr == nil {
		t.Fatal("expected error from first EnsureRunning with bad binary")
	}

	// Cooldown file must now exist.
	path := cooldownPath(dir, "cdtest")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cooldown file must exist after spawn failure: %v", err)
	}

	// Second call: must return ErrSpawnCooldown without trying to start.
	_, secondErr := EnsureRunning(context.Background(), cfg)
	if secondErr == nil {
		t.Fatal("expected ErrSpawnCooldown from second EnsureRunning")
	}
	if !errors.Is(secondErr, ErrSpawnCooldown) {
		t.Errorf("second call must wrap ErrSpawnCooldown, got: %v", secondErr)
	}
}

func TestSpawnCooldown_ExplicitStartIgnores(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "cdtest.pid")

	// Write an active cooldown marker.
	if err := writeCooldown(dir, "cdtest"); err != nil {
		t.Fatalf("writeCooldown: %v", err)
	}

	pids := &pidStoreMock{pathResult: pidPath}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:          "cdtest",
		DataDir:       dir,
		Binary:        "/nonexistent/binary",
		Timeout:       200 * time.Millisecond,
		HealthPath:    "/health",
		SpawnCooldown: 5 * time.Second,
	}, pids, procs, health)

	// Start() is explicit -- must NOT check cooldown.
	_, err := d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from bad binary")
	}

	if errors.Is(err, ErrSpawnCooldown) {
		t.Error("explicit Start must NOT return ErrSpawnCooldown")
	}
	if !strings.Contains(err.Error(), "start cdtest") {
		t.Errorf("error must indicate start failure, got: %v", err)
	}
}

func TestSpawnCooldown_ClearedOnSuccess(t *testing.T) {
	dir := t.TempDir()

	// Write a cooldown marker (simulating a previous failure).
	if err := writeCooldown(dir, "cdtest"); err != nil {
		t.Fatalf("writeCooldown: %v", err)
	}

	path := cooldownPath(dir, "cdtest")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cooldown file must exist: %v", err)
	}

	// Use helper-process pattern (same as integration_test.go).
	cfg := Config{
		Name:          "cdtest",
		DataDir:       dir,
		Binary:        os.Args[0],
		Args:          []string{"-test.run=^TestHelperCooldown$", "-test.timeout=5m"},
		Timeout:       15 * time.Second,
		SpawnCooldown: 5 * time.Second,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = New(cfg).Stop(ctx)
	})

	d := New(cfg)
	info, err := d.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Port == 0 {
		t.Error("Start must return non-zero port")
	}

	// After successful start, cooldown file must be gone.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("cooldown file must be removed after successful start, stat: %v", statErr)
	}
}

func TestSpawnCooldown_Disabled(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Name:          "cdtest",
		DataDir:       dir,
		Binary:        "/no/such/binary/does/not/exist",
		Timeout:       200 * time.Millisecond,
		HealthPath:    "/health",
		SpawnCooldown: -1, // disabled
	}

	// First call: fails because binary is bad.
	_, firstErr := EnsureRunning(context.Background(), cfg)
	if firstErr == nil {
		t.Fatal("expected error from first EnsureRunning with bad binary")
	}

	// Second call: with cooldown disabled, should NOT get ErrSpawnCooldown.
	_, secondErr := EnsureRunning(context.Background(), cfg)
	if secondErr == nil {
		t.Fatal("expected error from second EnsureRunning")
	}
	if errors.Is(secondErr, ErrSpawnCooldown) {
		t.Error("cooldown must not be checked when SpawnCooldown is disabled (-1)")
	}
}

// ---------------------------------------------------------------------------
// Config defaults tests
// ---------------------------------------------------------------------------

func TestSpawnCooldown_DefaultApplied(t *testing.T) {
	cfg := Config{Name: "test", DataDir: t.TempDir()}
	cfg.applyDefaults()

	if cfg.SpawnCooldown != 5*time.Second {
		t.Errorf("default SpawnCooldown = %v, want 5s", cfg.SpawnCooldown)
	}
}

func TestSpawnCooldown_ExplicitValuePreserved(t *testing.T) {
	cfg := Config{Name: "test", DataDir: t.TempDir(), SpawnCooldown: 10 * time.Second}
	cfg.applyDefaults()

	if cfg.SpawnCooldown != 10*time.Second {
		t.Errorf("explicit SpawnCooldown = %v, want 10s", cfg.SpawnCooldown)
	}
}

func TestSpawnCooldown_DisabledValuePreserved(t *testing.T) {
	cfg := Config{Name: "test", DataDir: t.TempDir(), SpawnCooldown: -1}
	cfg.applyDefaults()

	if cfg.SpawnCooldown != -1 {
		t.Errorf("disabled SpawnCooldown (-1) = %v, must not be overridden", cfg.SpawnCooldown)
	}
}

// ---------------------------------------------------------------------------
// Cooldown vs running daemon: cooldown must NOT block EnsureRunning
// when the daemon is already running.
// ---------------------------------------------------------------------------

func TestSpawnCooldown_NotCheckedWhenDaemonRunning(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "testapp.pid")

	// Acquire lock and write PID data -- simulates a running daemon.
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

	// Write an active cooldown marker (simulating stale marker from previous failure).
	if writeErr := writeCooldown(dir, "testapp"); writeErr != nil {
		t.Fatalf("writeCooldown: %v", writeErr)
	}

	cfg := Config{
		Name:          "testapp",
		DataDir:       dir,
		Binary:        "/usr/bin/app",
		SpawnCooldown: 5 * time.Second,
	}

	// EnsureRunning must return the running daemon's info, ignoring the
	// stale cooldown marker. The cooldown check is AFTER the IsHeld check.
	port, ensureErr := EnsureRunning(context.Background(), cfg)
	if ensureErr != nil {
		t.Fatalf("EnsureRunning must succeed when daemon is running (stale cooldown), got: %v", ensureErr)
	}
	if port != 9876 {
		t.Errorf("port = %d, want 9876", port)
	}
}

// ---------------------------------------------------------------------------
// Cooldown written on startLocked failure (verified via mock deps)
// ---------------------------------------------------------------------------

func TestSpawnCooldown_WrittenOnStartFailure(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "cdtest.pid")

	pids := &pidStoreMock{pathResult: pidPath}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:          "cdtest",
		DataDir:       dir,
		Binary:        "/nonexistent/binary",
		Timeout:       200 * time.Millisecond,
		HealthPath:    "/health",
		SpawnCooldown: 5 * time.Second,
	}, pids, procs, health)

	_, err := d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from bad binary")
	}

	// Cooldown marker must have been written.
	path := cooldownPath(dir, "cdtest")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("cooldown file must exist after startLocked failure: %v", statErr)
	}
	if !isCooldownActive(dir, "cdtest", 5*time.Second) {
		t.Error("cooldown must be active after startLocked failure")
	}
}

// ---------------------------------------------------------------------------
// F2: cooldown NOT written on context cancellation
// ---------------------------------------------------------------------------

func TestSpawnCooldown_NotWrittenOnCtxCancel(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "cdtest.pid")

	// Mock that returns context.Canceled from Start (simulating Ctrl-C during spawn).
	pids := &pidStoreMock{pathResult: pidPath}
	procs := &mockProcessManager{
		startErr: context.Canceled,
	}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:          "cdtest",
		DataDir:       dir,
		Binary:        "/nonexistent/binary",
		Timeout:       200 * time.Millisecond,
		HealthPath:    "/health",
		SpawnCooldown: 5 * time.Second,
	}, pids, procs, health)

	_, err := d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from cancelled spawn")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled in chain, got: %v", err)
	}

	// Cooldown marker must NOT be written on context cancellation.
	path := cooldownPath(dir, "cdtest")
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("cooldown file must NOT be written when start fails due to context cancellation")
	}
}

func TestSpawnCooldown_NotWrittenOnDeadlineExceeded(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "cdtest.pid")

	pids := &pidStoreMock{pathResult: pidPath}
	procs := &mockProcessManager{
		startErr: context.DeadlineExceeded,
	}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:          "cdtest",
		DataDir:       dir,
		Binary:        "/nonexistent/binary",
		Timeout:       200 * time.Millisecond,
		HealthPath:    "/health",
		SpawnCooldown: 5 * time.Second,
	}, pids, procs, health)

	_, err := d.Start(context.Background())
	if err == nil {
		t.Fatal("expected error from deadline exceeded spawn")
	}

	// Cooldown marker must NOT be written on deadline exceeded.
	path := cooldownPath(dir, "cdtest")
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("cooldown file must NOT be written when start fails due to deadline exceeded")
	}
}

// ---------------------------------------------------------------------------
// Full round-trip: start -> health check -> EnsureRunning -> verify
// ---------------------------------------------------------------------------

func TestSpawnCooldown_ClearedOnSuccessfulEnsureRunning(t *testing.T) {
	dir := t.TempDir()

	// Write a cooldown marker (from previous failure).
	if err := writeCooldown(dir, "cdtest"); err != nil {
		t.Fatalf("writeCooldown: %v", err)
	}

	// Start a REAL daemon that will succeed, clearing the cooldown.
	cfg := Config{
		Name:          "cdtest",
		DataDir:       dir,
		Binary:        os.Args[0],
		Args:          []string{"-test.run=^TestHelperCooldown$", "-test.timeout=5m"},
		Timeout:       15 * time.Second,
		SpawnCooldown: 5 * time.Second,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = New(cfg).Stop(ctx)
	})

	// Use Start (explicit) which ignores cooldown. Then verify cooldown is cleared.
	d := New(cfg)
	_, err := d.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	path := cooldownPath(dir, "cdtest")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("cooldown must be cleared after successful start, stat: %v", statErr)
	}

	// Now EnsureRunning should return the daemon info (no cooldown in the way).
	port, ensureErr := EnsureRunning(context.Background(), cfg)
	if ensureErr != nil {
		t.Fatalf("EnsureRunning after successful start: %v", ensureErr)
	}

	// Verify daemon is actually running via health check.
	resp, httpErr := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port)) //nolint:noctx,gosec // test-only
	if httpErr != nil {
		t.Fatalf("health check: %v", httpErr)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
}
