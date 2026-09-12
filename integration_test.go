package daemon_test

// Cross-platform integration tests using the helper-process pattern: the test
// binary re-executes itself as the daemon child. The library marks its children
// with DAEMON_MODE=1 and DAEMON_DATA_DIR, so the parent never enters the helper.
// No external binaries, no build step, runs on Linux/macOS/Windows.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/grpmsoft/daemon"
	"github.com/grpmsoft/daemon/internal/pidlock"
)

const itName = "it"

// TestHelperProcess is the daemon child. It is skipped in the parent.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("DAEMON_MODE") != "1" {
		t.Skip("helper process only")
	}
	cfg := daemon.Config{Name: itName, DataDir: os.Getenv("DAEMON_DATA_DIR")}
	if v := os.Getenv("IT_IDLE"); v != "" {
		cfg.IdleTimeout, _ = time.ParseDuration(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(4 * time.Second) // long in-flight request: graceful shutdown must drain it
		_, _ = fmt.Fprint(w, "ok")
	})
	if err := daemon.Serve(context.Background(), cfg, mux); err != nil {
		fmt.Fprintln(os.Stderr, "helper serve:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func helperBinary() (string, []string) {
	return os.Args[0], []string{"-test.run=^TestHelperProcess$", "-test.timeout=5m"}
}

func itConfig(t *testing.T) daemon.Config {
	t.Helper()
	cfg := daemon.Config{Name: itName, DataDir: t.TempDir(), Timeout: 15 * time.Second}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = daemon.New(cfg).Stop(ctx)
	})
	return cfg
}

func dial(port int) error {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func TestIntegration_ConcurrentEnsureRunning(t *testing.T) {
	cfg := itConfig(t)
	bin, args := helperBinary()
	const n = 4
	ports := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { ports[i], errs[i] = daemon.EnsureRunning(context.Background(), cfg, bin, args) })
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ports[i] != ports[0] {
			t.Fatalf("caller %d got port %d, caller 0 got %d", i, ports[i], ports[0])
		}
	}
	if err := dial(ports[0]); err != nil {
		t.Fatalf("dial: %v", err)
	}
}

func TestIntegration_StopHonoursDeadline(t *testing.T) {
	cfg := itConfig(t)
	bin, args := helperBinary()
	port, err := daemon.EnsureRunning(context.Background(), cfg, bin, args)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = http.Get(fmt.Sprintf("http://127.0.0.1:%d/slow", port)) }()
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st := time.Now()
	err = daemon.New(cfg).Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop(ctx) = %v, want DeadlineExceeded", err)
	}
	if el := time.Since(st); el > 1500*time.Millisecond {
		t.Fatalf("Stop returned after %s, deadline was 1s", el)
	}
	// The shutdown already requested continues without us.
	deadline := time.Now().Add(10 * time.Second)
	for daemon.New(cfg).IsRunning() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if daemon.New(cfg).IsRunning() {
		t.Fatal("daemon did not finish its own graceful shutdown")
	}
}

func TestIntegration_StopIdempotentAndStatus(t *testing.T) {
	cfg := itConfig(t)
	bin, args := helperBinary()
	if _, err := daemon.EnsureRunning(context.Background(), cfg, bin, args); err != nil {
		t.Fatal(err)
	}
	d := daemon.New(cfg)
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if info, _ := d.Status(); info.Status != daemon.StatusStopped {
		t.Fatalf("Status after Stop = %s, want stopped", info.Status)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestIntegration_IdleShutdownWithoutClients(t *testing.T) {
	cfg := itConfig(t)
	t.Setenv("IT_IDLE", "500ms")
	bin, args := helperBinary()
	if _, err := daemon.EnsureRunning(context.Background(), cfg, bin, args); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for daemon.New(cfg).IsRunning() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if daemon.New(cfg).IsRunning() {
		t.Fatal("daemon with IdleTimeout=500ms and no clients still running after 5s")
	}
}

// N12: a starter took the lock and died before its child wrote anything.
// A waiting EnsureRunning must become the starter once the lock is free.
func TestIntegration_HolderDiesMidSpawn(t *testing.T) {
	cfg := itConfig(t)
	bin, args := helperBinary()
	pidPath := filepath.Join(cfg.DataDir, itName+".pid")
	holder, err := pidlock.TryLock(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = holder.WriteData(nil)
	go func() { time.Sleep(1500 * time.Millisecond); holder.Release() }() // "crash"

	port, err := daemon.EnsureRunning(context.Background(), cfg, bin, args)
	if err != nil {
		t.Fatalf("EnsureRunning after holder death: %v (want: become the starter)", err)
	}
	if err := dial(port); err != nil {
		t.Fatalf("dial: %v", err)
	}
}
