package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

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

func (m *mockProcessManager) KillProcess(_ int) error {
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

func (h *hookProcessManager) KillProcess(pid int) error {
	return h.inner.KillProcess(pid)
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
	pids := &pidStoreMock{
		aliveResult: true,
		saved:       &PIDInfo{PID: 42, Port: 8080, Name: "testapp"},
	}
	d := newMockDaemon(pids, &mockProcessManager{}, &mockHealthChecker{})

	err := d.Start(context.Background(), "/usr/bin/app", nil)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q does not contain %q", err, "already running")
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("error %q does not contain %q", err, "42")
	}
	if !strings.Contains(err.Error(), "8080") {
		t.Errorf("error %q does not contain %q", err, "8080")
	}
}

func TestDaemon_Start_Success_CallsStartDetachedAndHealth(t *testing.T) {
	const wantPID = 1001
	inner := &mockProcessManager{startPID: wantPID}
	health := &mockHealthChecker{}
	pids := &pidStoreMock{}

	// hookProcessManager fires onStart after StartDetached so we can simulate
	// the spawned process writing the PID file before waitForPIDFile polls.
	hook := &hookProcessManager{
		inner: inner,
		onStart: func() {
			pids.saved = &PIDInfo{PID: wantPID, Port: 8080, Name: "testapp"}
		},
	}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    "/tmp/testapp",
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, hook, health)

	err := d.Start(context.Background(), "/usr/bin/app", []string{"--port=8080"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hook.startCallCount != 1 {
		t.Errorf("StartDetached must be called once, got %d", hook.startCallCount)
	}
	if health.waitCallCount != 1 {
		t.Errorf("WaitUntilReady must be called once, got %d", health.waitCallCount)
	}
}

func TestDaemon_Start_StartDetachedError_ReturnsError(t *testing.T) {
	pids := &pidStoreMock{}
	procs := &mockProcessManager{startErr: errors.New("exec: binary not found")}
	health := &mockHealthChecker{}

	d := newMockDaemon(pids, procs, health)

	err := d.Start(context.Background(), "/no/such/binary", nil)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "binary not found") {
		t.Errorf("error %q does not contain %q", err, "binary not found")
	}
}

func TestDaemon_Start_HealthCheckFails_ReturnsError(t *testing.T) {
	const wantPID = 1002
	inner := &mockProcessManager{startPID: wantPID}
	pids := &pidStoreMock{}
	health := &mockHealthChecker{waitErr: errors.New("connection refused")}

	// Inject PID file entry after StartDetached fires, so waitForPIDFile succeeds
	// and the test reaches the health check step.
	hook := &hookProcessManager{
		inner: inner,
		onStart: func() {
			pids.saved = &PIDInfo{PID: wantPID, Port: 9090, Name: "testapp"}
		},
	}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    "/tmp/testapp",
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, hook, health)

	err := d.Start(context.Background(), "/usr/bin/app", nil)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Errorf("error %q does not contain %q", err, "health check failed")
	}
}

func TestDaemon_Start_ContextCancelled_ReturnsError(t *testing.T) {
	// pids.saved is nil → Load always fails → waitForPIDFile will poll until
	// context cancellation or timeout.
	pids := &pidStoreMock{}
	procs := &mockProcessManager{startPID: 9999}
	health := &mockHealthChecker{}

	d := newMockDaemon(pids, procs, health)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	err := d.Start(ctx, "/usr/bin/app", nil)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "context cancelled") {
		t.Errorf("error %q does not contain %q", err, "context cancelled")
	}
}

// ---------------------------------------------------------------------------
// Tests: Stop
// ---------------------------------------------------------------------------

func TestDaemon_Stop_NoPIDFile_ReturnsError(t *testing.T) {
	pids := &pidStoreMock{loadErr: errors.New("pid file not found")}
	d := newMockDaemon(pids, &mockProcessManager{}, &mockHealthChecker{})

	err := d.Stop()

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pid file not found") {
		t.Errorf("error %q does not contain %q", err, "pid file not found")
	}
}

func TestDaemon_Stop_DeadProcess_ClearsPIDWithoutKill(t *testing.T) {
	pids := &pidStoreMock{saved: &PIDInfo{PID: 9999, Port: 8080}}
	procs := &mockProcessManager{aliveResult: false}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	err := d.Stop()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if procs.killCallCount != 0 {
		t.Errorf("KillProcess must NOT be called for a dead process, got %d calls", procs.killCallCount)
	}
	if pids.clearCallCount != 1 {
		t.Errorf("Clear must be called once, got %d", pids.clearCallCount)
	}
	if pids.saved != nil {
		t.Errorf("PID store must be cleared, got %+v", pids.saved)
	}
}

func TestDaemon_Stop_AliveProcess_KillsAndClears(t *testing.T) {
	const pid = 12345
	pids := &pidStoreMock{saved: &PIDInfo{PID: pid, Port: 8080}}
	procs := &mockProcessManager{aliveResult: true}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	err := d.Stop()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if procs.killCallCount != 1 {
		t.Errorf("KillProcess must be called once, got %d", procs.killCallCount)
	}
	if pids.clearCallCount != 1 {
		t.Errorf("Clear must be called once, got %d", pids.clearCallCount)
	}
}

func TestDaemon_Stop_KillFails_ReturnsError(t *testing.T) {
	pids := &pidStoreMock{saved: &PIDInfo{PID: 555, Port: 7070}}
	procs := &mockProcessManager{
		aliveResult: true,
		killErr:     errors.New("permission denied"),
	}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	err := d.Stop()

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
	pids := &pidStoreMock{saved: &PIDInfo{PID: 777, Port: 9000, Name: "testapp", StartTime: startTime}}
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

func TestDaemon_Status_DeadPID_ReturnsStatusError(t *testing.T) {
	pids := &pidStoreMock{saved: &PIDInfo{PID: 888, Port: 6060}}
	procs := &mockProcessManager{aliveResult: false}
	d := newMockDaemon(pids, procs, &mockHealthChecker{})

	info, err := d.Status()

	if err != nil {
		t.Fatalf("Status must not return an error for a dead PID: %v", err)
	}
	if info.Status != StatusError {
		t.Errorf("got %v, want %v", info.Status, StatusError)
	}
	if info.PID != 888 {
		t.Errorf("PID must be preserved in StatusError, got %v", info.PID)
	}
	if info.Port != 6060 {
		t.Errorf("Port must be preserved in StatusError, got %v", info.Port)
	}
}

func TestDaemon_Status_ZeroStartTime_UptimeIsZero(t *testing.T) {
	pids := &pidStoreMock{saved: &PIDInfo{PID: 100, Port: 8080}} // StartTime is zero
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
	const newPID = 2000

	procs := &mockProcessManager{
		startPID:    newPID,
		aliveResult: false, // dead → Stop clears without kill
	}
	health := &mockHealthChecker{}

	pids := &pidStoreMock{
		saved: &PIDInfo{PID: 999, Port: 1111}, // old stale entry
	}

	// Use a hookProcessManager so we can inject the new PID file entry
	// as soon as StartDetached fires (simulating the spawned process calling Serve).
	hook := &hookProcessManager{
		inner: procs,
		onStart: func() {
			pids.saved = &PIDInfo{PID: newPID, Port: 8080, Name: "testapp"}
		},
	}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    "/tmp/testapp",
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, hook, health)

	err := d.Restart(context.Background(), "/usr/bin/app", nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hook.startCallCount != 1 {
		t.Errorf("Start must be called once during Restart, got %d", hook.startCallCount)
	}
}

func TestDaemon_Restart_IgnoresStopError(t *testing.T) {
	const newPID = 3000

	pids := &pidStoreMock{
		// Load will fail → Stop returns error (no PID file) → Restart ignores it.
		loadErr: errors.New("no pid file"),
	}
	procs := &mockProcessManager{startPID: newPID}
	health := &mockHealthChecker{}

	hook := &hookProcessManager{
		inner: procs,
		onStart: func() {
			// Clear loadErr so Start's waitForPIDFile can succeed.
			pids.loadErr = nil
			pids.saved = &PIDInfo{PID: newPID, Port: 8080, Name: "testapp"}
		},
	}

	d := NewWithDeps(Config{
		Name:       "testapp",
		DataDir:    "/tmp/testapp",
		Timeout:    500 * time.Millisecond,
		HealthPath: "/health",
	}, pids, hook, health)

	// Restart ignores Stop error and proceeds to Start.
	err := d.Restart(context.Background(), "/usr/bin/app", nil)

	if err != nil {
		t.Fatalf("Restart must ignore Stop errors and attempt Start: %v", err)
	}
	if hook.startCallCount != 1 {
		t.Errorf("got %v, want %v", hook.startCallCount, 1)
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
