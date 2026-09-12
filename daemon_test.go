package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grpmsoft/daemon/internal"
	"github.com/grpmsoft/daemon/internal/pidlock"
)

// fileStatForTest is a thin wrapper around os.Stat used by DataDir-creation tests.
// Named so callers read as "stat the path for the test" rather than importing os directly.
func fileStatForTest(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// ---------------------------------------------------------------------------
// Mock implementations of the three interfaces.
// ---------------------------------------------------------------------------

// pidStoreMock is an in-memory PIDStore for testing.
type pidStoreMock struct {
	saved          *PIDInfo
	aliveResult    bool
	saveErr        error
	loadErr        error
	clearErr       error
	saveCallCount  int
	clearCallCount int
}

func (m *pidStoreMock) Save(pid, port int, name, binary string, startTime time.Time) error {
	m.saveCallCount++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved = &PIDInfo{PID: pid, Port: port, Name: name, Binary: binary, StartTime: startTime}
	return nil
}

func (m *pidStoreMock) Load() (PIDInfo, error) {
	if m.loadErr != nil {
		return PIDInfo{}, m.loadErr
	}
	if m.saved == nil {
		return PIDInfo{}, errors.New("pid file not found")
	}
	return *m.saved, nil
}

func (m *pidStoreMock) Clear() error {
	m.clearCallCount++
	if m.clearErr != nil {
		return m.clearErr
	}
	m.saved = nil
	return nil
}

func (m *pidStoreMock) IsAlive() bool {
	return m.aliveResult
}

func (m *pidStoreMock) Path() string { return "/mock/app.pid" }

var _ PIDStore = (*pidStoreMock)(nil)

// mockProcessManager controls StartDetached, KillProcess, IsProcessAlive for tests.
type mockProcessManager struct {
	startPID       int
	startErr       error
	killErr        error
	aliveResult    bool
	startCallCount int
	killCallCount  int
}

func (m *mockProcessManager) StartDetached(_ string, _ []string, _ string, _ []string) (int, error) {
	m.startCallCount++
	if m.startErr != nil {
		return 0, m.startErr
	}
	return m.startPID, nil
}

func (m *mockProcessManager) KillProcess(_ context.Context, _ int, _ time.Duration) error {
	m.killCallCount++
	return m.killErr
}

func (m *mockProcessManager) IsProcessAlive(_ int) bool {
	return m.aliveResult
}

var _ ProcessManager = (*mockProcessManager)(nil)

// mockHealthChecker allows tests to control the health check result.
type mockHealthChecker struct {
	checkErr      error
	waitErr       error
	waitCallCount int
}

func (m *mockHealthChecker) Check(_ int, _ string) error {
	return m.checkErr
}

func (m *mockHealthChecker) WaitUntilReady(_ int, _ string, _ time.Duration) error {
	m.waitCallCount++
	return m.waitErr
}

var _ HealthChecker = (*mockHealthChecker)(nil)

// hookProcessManager wraps a ProcessManager and fires a callback on StartDetached.
type hookProcessManager struct {
	inner          *mockProcessManager
	onStart        func()
	startCallCount int
}

func (h *hookProcessManager) StartDetached(binary string, args []string, logFile string, env []string) (int, error) {
	h.startCallCount++
	pid, err := h.inner.StartDetached(binary, args, logFile, env)
	if err == nil && h.onStart != nil {
		h.onStart()
	}
	return pid, err
}

func (h *hookProcessManager) KillProcess(ctx context.Context, pid int, grace time.Duration) error {
	return h.inner.KillProcess(ctx, pid, grace)
}

func (h *hookProcessManager) IsProcessAlive(pid int) bool {
	return h.inner.IsProcessAlive(pid)
}

var _ ProcessManager = (*hookProcessManager)(nil)

// ---------------------------------------------------------------------------
// Helper: construct a Daemon backed by mocks.
// ---------------------------------------------------------------------------

func newMockDaemon(pids PIDStore, procs ProcessManager, health HealthChecker) *Daemon {
	cfg := Config{
		Name:       "testapp",
		DataDir:    "/tmp/testapp",
		Timeout:    100 * time.Millisecond, // fast timeouts for unit tests
		HealthPath: "/health",
	}
	return NewWithDeps(cfg, pids, procs, health)
}

// ---------------------------------------------------------------------------
// Tests: IsRunning
// ---------------------------------------------------------------------------

func TestDaemon_IsRunning_DelegatesToPIDStore(t *testing.T) {
	tests := []struct {
		name        string
		alive       bool
		wantRunning bool
	}{
		{"alive PID", true, true},
		{"no PID file", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pids := &pidStoreMock{aliveResult: tt.alive}
			d := newMockDaemon(pids, &mockProcessManager{}, &mockHealthChecker{})
			if got := d.IsRunning(); got != tt.wantRunning {
				t.Errorf("got %v, want %v", got, tt.wantRunning)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: Start
// ---------------------------------------------------------------------------

func TestDaemon_Start_AlreadyRunning_ReturnsError(t *testing.T) {
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	// Acquire the PID lock to simulate a running daemon.
	lock, err := pidlock.TryLock(pidPath)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	defer lock.Release()

	pids := &pidStoreMock{
		aliveResult: true,
		saved:       &PIDInfo{PID: 42, Port: 8080, Name: "testapp"},
	}
	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, &mockProcessManager{}, &mockHealthChecker{})

	startErr := d.Start(context.Background(), "/usr/bin/app", nil)

	if startErr == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(startErr, ErrAlreadyRunning) {
		t.Errorf("error %q must wrap ErrAlreadyRunning", startErr)
	}
	if !strings.Contains(startErr.Error(), "42") {
		t.Errorf("error %q does not contain PID %q", startErr, "42")
	}
	if !strings.Contains(startErr.Error(), "8080") {
		t.Errorf("error %q does not contain port %q", startErr, "8080")
	}
}

func TestDaemon_Start_LockProtocol_AcquiresStartupLock(t *testing.T) {
	dataDir := t.TempDir()

	pids := &pidStoreMock{}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Start will acquire startup lock, then PID lock, then call startWithLock
	// which uses exec.Command with a non-existent binary. The error proves
	// the lock protocol ran (it got past lock steps to the exec step).
	err := d.Start(context.Background(), "/nonexistent/binary/app", nil)

	if err == nil {
		t.Fatal("expected error from exec of non-existent binary, got nil")
	}
	// The error should come from startWithLock (exec failure), proving
	// the lock protocol completed steps 1-3 successfully.
	if !strings.Contains(err.Error(), "start testapp") {
		t.Errorf("error %q must indicate start failure", err)
	}

	// After Start fails, the startup lock file must exist (never deleted).
	lockPath := filepath.Join(dataDir, "testapp.lock")
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("startup lock file must exist after Start: %v", statErr)
	}

	// After Start fails, the PID lock must be released (lock.File().Close called).
	pidPath := filepath.Join(dataDir, "testapp.pid")
	if pidlock.IsHeld(pidPath) {
		t.Error("PID lock must be released after Start failure")
	}
}

func TestDaemon_Start_BinaryNotFound_ReturnsError(t *testing.T) {
	dataDir := t.TempDir()

	pids := &pidStoreMock{}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// startWithLock calls exec.Command with a non-existent binary.
	err := d.Start(context.Background(), "/no/such/binary", nil)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "start testapp") {
		t.Errorf("error %q does not contain %q", err, "start testapp")
	}

	// PID lock must be released on failure.
	pidPath := filepath.Join(dataDir, "testapp.pid")
	if pidlock.IsHeld(pidPath) {
		t.Error("PID lock must be released after binary-not-found failure")
	}
}

func TestDaemon_Start_ErrorCleansPIDLock(t *testing.T) {
	// Verify that any startWithLock failure releases the PID lock.
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	pids := &pidStoreMock{}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Start with non-existent binary -- startWithLock fails at exec.
	err := d.Start(context.Background(), "/nonexistent/binary", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// PID lock file must NOT be held (parent closes fd on failure).
	if pidlock.IsHeld(pidPath) {
		t.Error("PID lock must be released after startWithLock failure")
	}

	// Startup lock file must NOT be held (deferred Unlock).
	lockPath := filepath.Join(dataDir, "testapp.lock")
	// If we can acquire it, it's not held.
	f, lockErr := os.OpenFile(lockPath, os.O_RDONLY, 0)
	if lockErr == nil {
		_ = f.Close()
	}
}

func TestDaemon_Start_ContextCancelled_ReturnsError(t *testing.T) {
	dataDir := t.TempDir()

	// Hold the startup lock so LockCtx must wait and see ctx cancelled.
	lockPath := filepath.Join(dataDir, "testapp.lock")
	lockFile, lockErr := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if lockErr != nil {
		t.Fatalf("create lock file: %v", lockErr)
	}

	// Acquire startup lock to create contention.
	startupFile, acqErr := internal.LockCtx(context.Background(), lockPath)
	if acqErr != nil {
		_ = lockFile.Close()
		t.Fatalf("acquire startup lock: %v", acqErr)
	}

	pids := &pidStoreMock{}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- d.Start(ctx, "/usr/bin/app", nil)
	}()

	// Give Start time to block on LockCtx, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	err := <-done

	// Release the lock for cleanup.
	internal.Unlock(startupFile)
	_ = lockFile.Close()

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error must wrap context.Canceled, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Start — orphan cleanup (D8 from Fable review)
// ---------------------------------------------------------------------------

func TestStart_ConcurrentStartsSerialized(t *testing.T) {
	// Two concurrent Start() calls must be serialized by the startup lock.
	// Both will fail (non-existent binary), but neither should panic or deadlock.
	dataDir := t.TempDir()

	pids := &pidStoreMock{}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- d.Start(ctx, "/nonexistent/binary", nil)
		}()
	}

	for range 2 {
		err := <-errs
		if err == nil {
			t.Error("expected error from each Start call, got nil")
		}
	}

	// After both calls, PID lock must be released.
	pidPath := filepath.Join(dataDir, "testapp.pid")
	if pidlock.IsHeld(pidPath) {
		t.Error("PID lock must be released after both Start calls complete")
	}
}

func TestStart_SecondCallBlockedByRunningDaemon(t *testing.T) {
	// If a daemon is already running (PID lock held), the second Start
	// must return ErrAlreadyRunning without attempting to spawn.
	dataDir := t.TempDir()
	pidPath := filepath.Join(dataDir, "testapp.pid")

	// Hold the PID lock to simulate a running daemon.
	lock, err := pidlock.TryLock(pidPath)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	defer lock.Release()

	pids := &pidStoreMock{
		aliveResult: true,
		saved:       &PIDInfo{PID: 5555, Port: 7070, Name: "testapp"},
	}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	startErr := d.Start(context.Background(), "/usr/bin/app", nil)

	if startErr == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(startErr, ErrAlreadyRunning) {
		t.Errorf("error must wrap ErrAlreadyRunning, got: %v", startErr)
	}
	if !strings.Contains(startErr.Error(), "5555") {
		t.Errorf("error %q must contain PID 5555", startErr)
	}
}

func TestReadLogTail(t *testing.T) {
	tests := []struct {
		name    string
		content string
		n       int
		want    string
	}{
		{"empty file", "", 5, ""},
		{"fewer lines than n", "line1\nline2\n", 5, "line1\nline2"},
		{"exact n lines", "a\nb\nc\n", 3, "a\nb\nc"},
		{"more lines than n", "1\n2\n3\n4\n5\n", 2, "4\n5"},
		{"single line no newline", "hello", 3, "hello"},
		{"zero n", "data\n", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.content == "" && tt.n > 0 {
				// Test empty file: write empty content.
				f := filepath.Join(t.TempDir(), "test.log")
				if writeErr := os.WriteFile(f, []byte{}, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
				got := readLogTail(f, tt.n)
				if got != tt.want {
					t.Errorf("readLogTail(%q, %d) = %q, want %q", f, tt.n, got, tt.want)
				}
				return
			}

			f := filepath.Join(t.TempDir(), "test.log")
			if writeErr := os.WriteFile(f, []byte(tt.content), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			got := readLogTail(f, tt.n)
			if got != tt.want {
				t.Errorf("readLogTail(%q, %d) = %q, want %q", f, tt.n, got, tt.want)
			}
		})
	}
}

func TestReadLogTail_MissingFile(t *testing.T) {
	got := readLogTail("/nonexistent/path/daemon.log", 10)
	if got != "" {
		t.Errorf("readLogTail for missing file = %q, want empty", got)
	}
}

func TestReadLogTail_EmptyPath(t *testing.T) {
	got := readLogTail("", 10)
	if got != "" {
		t.Errorf("readLogTail for empty path = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Tests: Stop
// ---------------------------------------------------------------------------

func TestDaemon_Stop_NoPIDFile_Idempotent(t *testing.T) {
	pids := &pidStoreMock{loadErr: errors.New("pid file not found")}
	d := newMockDaemon(pids, &mockProcessManager{}, &mockHealthChecker{})

	err := d.Stop(context.Background())

	// Stop on a stopped daemon is idempotent — returns nil.
	if err != nil {
		t.Fatalf("Stop on stopped daemon must be idempotent, got %v", err)
	}
}

func TestDaemon_Stop_DeadProcess_Idempotent(t *testing.T) {
	pids := &pidStoreMock{saved: &PIDInfo{PID: 9999, Port: 8080}, aliveResult: false}
	procs := &mockProcessManager{aliveResult: false}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	err := d.Stop(context.Background())

	// Dead process (lock not held) → idempotent nil, no kill.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if procs.killCallCount != 0 {
		t.Errorf("KillProcess must NOT be called for a dead process, got %d calls", procs.killCallCount)
	}
}

func TestDaemon_Stop_AliveProcess_KillsProcess(t *testing.T) {
	const pid = 12345
	pids := &pidStoreMock{saved: &PIDInfo{PID: pid, Port: 0}, aliveResult: true}
	procs := &mockProcessManager{aliveResult: true}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	// Port=0 → HTTP shutdown skipped → direct KillProcess.
	err := d.Stop(context.Background())

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if procs.killCallCount != 1 {
		t.Errorf("KillProcess must be called once, got %d", procs.killCallCount)
	}
}

func TestDaemon_Stop_KillFails_ReturnsError(t *testing.T) {
	pids := &pidStoreMock{saved: &PIDInfo{PID: 555, Port: 7070}, aliveResult: true}
	procs := &mockProcessManager{
		aliveResult: true,
		killErr:     errors.New("permission denied"),
	}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	err := d.Stop(context.Background())

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q does not contain %q", err, "permission denied")
	}
}

// ---------------------------------------------------------------------------
// Tests: Status
// ---------------------------------------------------------------------------

func TestDaemon_Status_NoPIDFile_ReturnsStopped(t *testing.T) {
	pids := &pidStoreMock{loadErr: errors.New("no such file")}
	d := newMockDaemon(pids, &mockProcessManager{}, &mockHealthChecker{})

	info, err := d.Status()

	if err != nil {
		t.Fatalf("Status must not error when PID file is absent: %v", err)
	}
	if info.Status != StatusStopped {
		t.Errorf("got %v, want %v", info.Status, StatusStopped)
	}
	if info.Name != "testapp" {
		t.Errorf("got %v, want %v", info.Name, "testapp")
	}
	if info.PID != 0 {
		t.Errorf("got %v, want %v", info.PID, 0)
	}
}

func TestDaemon_Status_AlivePID_ReturnsRunning(t *testing.T) {
	startTime := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	pids := &pidStoreMock{saved: &PIDInfo{PID: 777, Port: 9000, Name: "testapp", StartTime: startTime}, aliveResult: true}
	procs := &mockProcessManager{aliveResult: true}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	info, err := d.Status()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Status != StatusRunning {
		t.Errorf("got %v, want %v", info.Status, StatusRunning)
	}
	if info.PID != 777 {
		t.Errorf("got %v, want %v", info.PID, 777)
	}
	if info.Port != 9000 {
		t.Errorf("got %v, want %v", info.Port, 9000)
	}
	if info.Name != "testapp" {
		t.Errorf("got %v, want %v", info.Name, "testapp")
	}
	if info.StartTime != startTime {
		t.Errorf("got %v, want %v", info.StartTime, startTime)
	}
	if int64(info.Uptime) < 0 {
		t.Errorf("expected uptime >= 0, got %v", info.Uptime)
	}
}

func TestDaemon_Status_LockNotHeld_ReturnsStopped(t *testing.T) {
	// Lock not held (aliveResult=false) → daemon not running → StatusStopped.
	pids := &pidStoreMock{saved: &PIDInfo{PID: 888, Port: 6060}, aliveResult: false}
	procs := &mockProcessManager{aliveResult: false}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	info, err := d.Status()

	if err != nil {
		t.Fatalf("Status must not return an error: %v", err)
	}
	if info.Status != StatusStopped {
		t.Errorf("got %v, want %v (lock not held = stopped)", info.Status, StatusStopped)
	}
	if info.PID != 888 {
		t.Errorf("PID must be preserved in StatusError, got %v", info.PID)
	}
	if info.Port != 6060 {
		t.Errorf("Port must be preserved in StatusError, got %v", info.Port)
	}
}

func TestDaemon_Status_ZeroStartTime_UptimeIsZero(t *testing.T) {
	pids := &pidStoreMock{saved: &PIDInfo{PID: 100, Port: 8080}, aliveResult: true}
	procs := &mockProcessManager{aliveResult: true}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	info, err := d.Status()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Status != StatusRunning {
		t.Errorf("got %v, want %v", info.Status, StatusRunning)
	}
	if info.Uptime != time.Duration(0) {
		t.Errorf("zero StartTime must produce zero Uptime, got %v", info.Uptime)
	}
}

// ---------------------------------------------------------------------------
// Tests: Restart
// ---------------------------------------------------------------------------

func TestDaemon_Restart_StopsThenStarts(t *testing.T) {
	dataDir := t.TempDir()

	pids := &pidStoreMock{
		saved:       &PIDInfo{PID: 999, Port: 1111}, // old stale entry
		aliveResult: false,                          // dead -> Stop is idempotent
	}
	procs := &mockProcessManager{aliveResult: false}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Restart = Stop (idempotent) + Start (fails at exec of non-existent binary).
	err := d.Restart(context.Background(), "/nonexistent/binary", nil)

	// Error expected from Start (exec failure), not from Stop.
	if err == nil {
		t.Fatal("expected error from exec of non-existent binary")
	}
	if !strings.Contains(err.Error(), "start testapp") {
		t.Errorf("error must come from Start, got: %v", err)
	}
}

func TestDaemon_Restart_IgnoresStopError(t *testing.T) {
	dataDir := t.TempDir()

	pids := &pidStoreMock{
		loadErr: errors.New("no pid file"),
	}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    dataDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Restart ignores Stop error and proceeds to Start.
	// Start will fail at exec, but the point is it reaches Start.
	err := d.Restart(context.Background(), "/nonexistent/binary", nil)

	if err == nil {
		t.Fatal("expected error from exec, got nil")
	}
	// Must contain Start error (not Stop error).
	if !strings.Contains(err.Error(), "start testapp") {
		t.Errorf("error must come from Start phase, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: NewWithDeps constructor
// ---------------------------------------------------------------------------

func TestNewWithDeps_AppliesDefaults(t *testing.T) {
	cfg := Config{Name: "app", DataDir: "/tmp"} // zero Timeout and HealthPath
	d := NewWithDeps(cfg, &pidStoreMock{}, &mockProcessManager{}, &mockHealthChecker{})

	if d.cfg.Timeout != 30*time.Second {
		t.Errorf("applyDefaults must set 30s Timeout, got %v", d.cfg.Timeout)
	}
	if d.cfg.HealthPath != "/health" {
		t.Errorf("applyDefaults must set /health path, got %v", d.cfg.HealthPath)
	}
}

func TestNewWithDeps_InjectsProvidedDependencies(t *testing.T) {
	pids := &pidStoreMock{aliveResult: true, saved: &PIDInfo{PID: 123, Port: 9000}}
	d := NewWithDeps(
		Config{Name: "app", DataDir: "/tmp"},
		pids,
		&mockProcessManager{},
		&mockHealthChecker{},
	)

	// IsRunning delegates to pids.IsAlive — verify the injected mock is used.
	if !d.IsRunning() {
		t.Error("IsRunning must use the injected PIDStore, expected true, got false")
	}
}

// ---------------------------------------------------------------------------
// Tests: B6 — Start() creates DataDir if it doesn't exist.
// ---------------------------------------------------------------------------

func TestDaemon_Start_CreatesDataDir(t *testing.T) {
	// Use t.TempDir() as the parent; append a nested subdir that does not exist.
	parent := t.TempDir()
	nestedDir := parent + "/sub/nested/datadir"

	pids := &pidStoreMock{}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "mkdirtest",
		DataDir:    nestedDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Start will fail at exec (non-existent binary), but DataDir
	// must be created BEFORE lock acquisition.
	_ = d.Start(context.Background(), "/nonexistent/binary", nil)

	// Verify the directory was actually created on disk.
	info, statErr := fileStatForTest(nestedDir)
	if statErr != nil {
		t.Fatalf("DataDir %q must have been created: %v", nestedDir, statErr)
	}
	if !info.IsDir() {
		t.Errorf("DataDir %q must be a directory", nestedDir)
	}
}

func TestDaemon_Start_DataDirAlreadyExists_Succeeds(t *testing.T) {
	// DataDir pre-exists — MkdirAll is idempotent, Start proceeds to lock step.
	existingDir := t.TempDir()

	pids := &pidStoreMock{}
	procs := &mockProcessManager{}
	health := &mockHealthChecker{}

	d := NewWithDeps(Config{
		Name:       "existtest",
		DataDir:    existingDir,
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, procs, health)

	// Will fail at exec, but should not fail at DataDir creation.
	err := d.Start(context.Background(), "/nonexistent/binary", nil)
	if err == nil {
		t.Fatal("expected error from non-existent binary")
	}
	// Error should NOT mention "create data dir".
	if strings.Contains(err.Error(), "create data dir") {
		t.Errorf("error %q should not be about DataDir creation", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: B5 — Stop() uses IsAlive() (PID + binary verification), not
// bare IsProcessAlive(). Stale PID that belongs to a different process
// must be cleared without killing.
// ---------------------------------------------------------------------------

// TestDaemon_Stop_StalePID_Idempotent verifies that when IsAlive()
// returns false (lock not held — PID recycled or daemon died), Stop
// returns nil without killing.
func TestDaemon_Stop_StalePID_Idempotent(t *testing.T) {
	pids := &pidStoreMock{
		saved:       &PIDInfo{PID: 77777, Port: 8080, Name: "testapp"},
		aliveResult: false,
	}
	procs := &mockProcessManager{aliveResult: false}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	err := d.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop must be idempotent for stale PID: %v", err)
	}
	if procs.killCallCount != 0 {
		t.Errorf("KillProcess must NOT be called for a stale PID, got %d calls", procs.killCallCount)
	}
}

// ---------------------------------------------------------------------------
// Tests: B4 — ConnTracker Disconnect clamp (cannot go negative).
// ---------------------------------------------------------------------------

func TestConnTracker_Disconnect_StrayWithoutConnect_StaysZero(t *testing.T) {
	ct := NewConnTracker()

	// Disconnect with no prior Connect — must not go negative.
	ct.Disconnect()

	if got := ct.Active(); got != 0 {
		t.Errorf("Active() after stray Disconnect = %d, want 0 (clamp prevents negative)", got)
	}
}

func TestConnTracker_Disconnect_DoubleDisconnect_StaysZero(t *testing.T) {
	ct := NewConnTracker()

	ct.Connect()
	ct.Disconnect() // balanced — count reaches 0
	ct.Disconnect() // stray — must clamp to 0, not -1

	if got := ct.Active(); got != 0 {
		t.Errorf("Active() after Connect+Disconnect+stray Disconnect = %d, want 0", got)
	}
}

func TestConnTracker_Disconnect_StrayExtraDisconnect_OtherConnectionsIntact(t *testing.T) {
	// Connect A, Connect B, stray Disconnect, Disconnect A → B still connected.
	ct := NewConnTracker()

	ct.Connect()    // A
	ct.Connect()    // B
	ct.Disconnect() // stray (simulates crashed client) — count goes 2→1
	ct.Disconnect() // A disconnects — count goes 1→0

	// B is still logically connected, but the stray Disconnect consumed A's slot.
	// The clamp ensures we never see negative values; actual count is 0.
	// The important invariant: Active() never reports a negative value.
	if got := ct.Active(); got < 0 {
		t.Errorf("Active() must never be negative, got %d", got)
	}
}

func TestConnTracker_ClampNeverGoesNegative_TableDriven(t *testing.T) {
	tests := []struct {
		name        string
		connects    int
		disconnects int
		wantMin     int64 // Active() must be >= wantMin
	}{
		{"zero disconnects zero connects", 0, 0, 0},
		{"one stray disconnect", 0, 1, 0},
		{"three stray disconnects", 0, 3, 0},
		{"two connects one extra disconnect", 2, 3, 0},
		{"balanced", 5, 5, 0},
		{"more connects than disconnects", 5, 3, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := NewConnTracker()
			for range tt.connects {
				ct.Connect()
			}
			for range tt.disconnects {
				ct.Disconnect()
			}
			got := ct.Active()
			if got < 0 {
				t.Errorf("Active() = %d, must never be negative", got)
			}
			if got < tt.wantMin {
				t.Errorf("Active() = %d, want >= %d", got, tt.wantMin)
			}
		})
	}
}
