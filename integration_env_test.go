package daemon_test

// The daemon's environment, end to end. Config.Env nil inherits the caller's
// environment; set, it is all the daemon gets besides the DAEMON_* variables
// Start adds. Same helper-process pattern as integration_test.go, with a
// helper that reports its whole environment: it must come up with nothing but
// DAEMON_* set, so it reads no other variable.

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grpmsoft/daemon"
)

const envName = "envtest"

// TestEnvHelperProcess is the daemon child. It is skipped in the parent.
func TestEnvHelperProcess(t *testing.T) {
	if os.Getenv("DAEMON_MODE") != "1" {
		t.Skip("helper process only")
	}
	cfg := daemon.Config{
		Name:             envName,
		DataDir:          os.Getenv("DAEMON_DATA_DIR"),
		DisableTokenAuth: true, // the parent has no token to present
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /environ", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.MarshalWrite(w, slices.Sorted(slices.Values(os.Environ())))
	})
	if err := daemon.Serve(context.Background(), cfg, mux); err != nil {
		fmt.Fprintln(os.Stderr, "env helper serve:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestIntegration_Env(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{"nil inherits the caller's environment", nil},
		{"empty leaves only the DAEMON_* variables", []string{}},
		{"set is all the daemon gets", []string{"ENV_TEST_PASSED=declared"}},
		{"a DAEMON_* entry cannot be overridden", []string{"DAEMON_MODE=0", "ENV_TEST_PASSED=declared"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := daemon.Config{
				Name:    envName,
				DataDir: t.TempDir(),
				Binary:  os.Args[0],
				Args:    []string{"-test.run=^TestEnvHelperProcess$", "-test.timeout=5m"},
				Timeout: 15 * time.Second,
				Env:     tt.env,
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = daemon.New(cfg).Stop(ctx)
			})
			t.Setenv("ENV_TEST_CALLER_ONLY", "must-not-leak")

			port, err := daemon.EnsureRunning(context.Background(), cfg)
			if err != nil {
				t.Fatalf("EnsureRunning: %v", err)
			}
			got := daemonEnvironment(t, port)

			// What the daemon was given: everything of the caller's when Env
			// is nil, otherwise only the entries it names...
			given := tt.env
			if given == nil {
				given = os.Environ()
			}
			want := envMap(given)
			// ...plus what Start adds to hand it its data directory and PID
			// lock. Those come last, so they win a duplicate key.
			for key, value := range envMap(daemonVariables(cfg.DataDir)) {
				want[key] = value
			}
			if runtime.GOOS == "windows" {
				// os/exec adds SYSTEMROOT to a Windows child that lacks it.
				if _, ok := want["SYSTEMROOT"]; !ok {
					want["SYSTEMROOT"] = os.Getenv("SYSTEMROOT")
				}
			}
			assertEnvironment(t, got, want)
		})
	}
}

// daemonVariables returns the entries Start adds to the child's environment to
// hand it its data directory and PID lock.
func daemonVariables(dataDir string) []string {
	env := []string{"DAEMON_MODE=1", "DAEMON_DATA_DIR=" + dataDir}
	if runtime.GOOS != "windows" {
		env = append(env, "DAEMON_PIDFD=3")
	}
	return env
}

// daemonEnvironment returns the environment of the daemon listening on port,
// as its handler reports it.
func daemonEnvironment(t *testing.T, port int) map[string]string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/environ", port)) //nolint:noctx,gosec // loopback test daemon
	if err != nil {
		t.Fatalf("GET /environ: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /environ: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /environ: %s: %s", resp.Status, body)
	}
	var entries []string
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("parse /environ %q: %v", body, err)
	}
	return envMap(entries)
}

// envMap indexes KEY=value entries by key. Names are case-insensitive on
// Windows, so they are compared upper-cased there.
func envMap(entries []string) map[string]string {
	m := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		m[key] = value
	}
	return m
}

// assertEnvironment fails the test unless got has exactly the entries of want.
// Differences are reported by key only: the nil case compares the caller's
// whole environment, which is not for the test log.
func assertEnvironment(t *testing.T, got, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if gotValue, ok := got[key]; !ok {
			t.Errorf("daemon has no %s", key)
		} else if gotValue != value {
			t.Errorf("daemon %s differs from what it was given", key)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("daemon has %s, which it was not given", key)
		}
	}
}
