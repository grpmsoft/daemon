package daemon

// What Start hands ProcessManager.Start as the child's environment: Config.Env
// when it is set, the caller's otherwise, with the DAEMON_* variables appended.
// The daemon's own view of that environment is checked end to end in
// integration_env_test.go.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// daemonVariables returns the entries Start adds to the child's environment to
// hand it its data directory and PID lock.
func daemonVariables(dataDir string) []string {
	env := []string{"DAEMON_MODE=1", "DAEMON_DATA_DIR=" + dataDir}
	if runtime.GOOS != "windows" {
		env = append(env, "DAEMON_PIDFD=3")
	}
	return env
}

// recordingProcessManager keeps every StartSpec Start hands it.
type recordingProcessManager struct {
	mockProcessManager
	specs []StartSpec
}

func (m *recordingProcessManager) Start(ctx context.Context, spec StartSpec) (int, error) {
	m.specs = append(m.specs, spec)
	return m.mockProcessManager.Start(ctx, spec)
}

// startRecorded runs Start with a ProcessManager that records its StartSpec,
// and returns that spec and the config it was built from.
func startRecorded(t *testing.T, cfg Config) (StartSpec, Config) {
	t.Helper()
	cfg.DataDir = t.TempDir()
	pids := &pidStoreMock{
		pathResult: filepath.Join(cfg.DataDir, cfg.Name+".pid"),
		saved:      &PIDInfo{PID: 42, Port: 8080, Name: cfg.Name}, // what the started daemon wrote
	}
	procs := &recordingProcessManager{mockProcessManager: mockProcessManager{startPID: 42}}
	d := NewWithDeps(cfg, pids, procs, &mockHealthChecker{})
	if _, err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return procs.specs[0], d.cfg
}

func TestDaemon_Start_EnvIsTheCompleteChildEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{"nil inherits the caller's environment", nil},
		{"empty leaves only the DAEMON_* variables", []string{}},
		{"set, the entries are all the daemon gets", []string{"ENV_TEST_PASSED=declared"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, cfg := startRecorded(t, Config{Name: "testapp", Env: tt.env})

			want := tt.env
			if want == nil {
				want = os.Environ()
			}
			want = append(slices.Clone(want), daemonVariables(cfg.DataDir)...)
			if slices.Equal(spec.Env, want) {
				return
			}
			if tt.env == nil {
				// The caller's whole environment is not for the test log.
				t.Errorf("StartSpec.Env has %d entries, want the caller's %d plus %d DAEMON_* variables",
					len(spec.Env), len(os.Environ()), len(daemonVariables(cfg.DataDir)))
				return
			}
			t.Errorf("StartSpec.Env = %q, want %q", spec.Env, want)
		})
	}
}

func TestDaemon_Start_EnvLeavesTheCallersSliceAlone(t *testing.T) {
	env := make([]string, 1, 8) // spare capacity: an append in place would show behind len
	env[0] = "ENV_TEST_PASSED=declared"

	startRecorded(t, Config{Name: "testapp", Env: env})

	if len(env) != 1 || slices.Contains(env[:cap(env)], "DAEMON_MODE=1") {
		t.Errorf("Config.Env = %q (capacity %d): Start appended to the caller's slice", env, cap(env))
	}
}
