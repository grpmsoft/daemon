// Package daemon provides a reusable, cross-platform daemon lifecycle library.
//
// Two modes of operation:
//
//   - Client mode: Start/Stop/Restart/Status -- manage a background daemon process.
//   - Server mode: Serve() -- run as the foreground daemon process with HTTP, PID management, signal handling.
//
// Consumer wires CLI commands; this library provides the operations.
//
//	import "github.com/grpmsoft/daemon"
//
//	d := daemon.New(daemon.Config{Name: "myapp", DataDir: ".myapp"})
//	d.Start(ctx, binary, args) // spawn detached background process
//	d.Stop()                   // graceful shutdown
//	d.Status()                 // check if running, get info
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Daemon manages a background daemon process.
// It depends on PIDStore, ProcessManager, and HealthChecker interfaces —
// not on concrete implementations. New() wires the defaults.
type Daemon struct {
	cfg     Config
	handler http.Handler
	pids    PIDStore
	procs   ProcessManager
	health  HealthChecker
}

// New creates a Daemon with the given configuration and default implementations.
func New(cfg Config) *Daemon {
	cfg.applyDefaults()
	return &Daemon{
		cfg:    cfg,
		pids:   newDefaultPIDStore(cfg.DataDir, cfg.Name),
		procs:  defaultProcessManager{},
		health: defaultHealthChecker{},
	}
}

// NewWithDeps creates a Daemon with explicit interface implementations.
// Intended for testing — callers supply mock PIDStore, ProcessManager, and HealthChecker.
func NewWithDeps(cfg Config, pids PIDStore, procs ProcessManager, health HealthChecker) *Daemon {
	cfg.applyDefaults()
	return &Daemon{
		cfg:    cfg,
		pids:   pids,
		procs:  procs,
		health: health,
	}
}

// SetHandler sets the HTTP handler that Serve() will expose.
func (d *Daemon) SetHandler(h http.Handler) {
	d.handler = h
}

// Start spawns a detached background process running the given binary with args.
// It waits for the health check to pass within Config.Timeout.
// The spawned process must eventually call Serve().
func (d *Daemon) Start(ctx context.Context, binary string, args []string) error {
	if d.IsRunning() {
		info, _ := d.Status()
		return fmt.Errorf("daemon %s already running (pid %d, port %d)", d.cfg.Name, info.PID, info.Port)
	}

	if err := os.MkdirAll(d.cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir %s: %w", d.cfg.DataDir, err)
	}

	if err := d.pids.Clear(); err != nil {
		return fmt.Errorf("clear stale pid: %w", err)
	}

	logFile := filepath.Join(d.cfg.DataDir, d.cfg.Name+".log")
	env := []string{
		"DAEMON_MODE=1",
		fmt.Sprintf("DAEMON_DATA_DIR=%s", d.cfg.DataDir),
	}

	pid, err := d.procs.StartDetached(binary, args, logFile, env)
	if err != nil {
		return fmt.Errorf("start daemon %s: %w", d.cfg.Name, err)
	}

	if err := d.waitForPIDFile(ctx, pid); err != nil {
		return fmt.Errorf("daemon %s started (pid %d) but did not become ready: %w", d.cfg.Name, pid, err)
	}

	data, loadErr := d.pids.Load()
	if loadErr != nil {
		return fmt.Errorf("daemon %s started but pid file unreadable: %w", d.cfg.Name, loadErr)
	}

	if err := d.health.WaitUntilReady(data.Port, d.cfg.HealthPath, d.cfg.Timeout); err != nil {
		return fmt.Errorf("daemon %s health check failed: %w", d.cfg.Name, err)
	}

	return nil
}

func (d *Daemon) waitForPIDFile(ctx context.Context, expectedPID int) error {
	deadline := time.Now().Add(d.cfg.Timeout)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled: %w", ctx.Err())
		default:
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pid file after %s", d.cfg.Timeout)
		}

		data, err := d.pids.Load()
		if err == nil && data.PID == expectedPID {
			return nil
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// Stop reads the PID file, verifies the process identity (PID + binary path),
// kills the daemon process, and clears the PID file. If the PID has been
// recycled to a different process, the stale PID file is cleared without killing.
func (d *Daemon) Stop() error {
	data, err := d.pids.Load()
	if err != nil {
		return fmt.Errorf("stop daemon %s: %w", d.cfg.Name, err)
	}

	// Use IsAlive (PID + binary verification) instead of bare IsProcessAlive
	// to avoid killing an unrelated process with a recycled PID.
	if !d.pids.IsAlive() {
		return d.pids.Clear()
	}

	if err := d.procs.KillProcess(data.PID); err != nil {
		return fmt.Errorf("kill daemon %s (pid %d): %w", d.cfg.Name, data.PID, err)
	}

	if err := d.pids.Clear(); err != nil {
		return fmt.Errorf("clear pid file after stop: %w", err)
	}

	return nil
}

// Restart stops a running daemon (if any) and starts a new one.
func (d *Daemon) Restart(ctx context.Context, binary string, args []string) error {
	_ = d.Stop()
	return d.Start(ctx, binary, args)
}

// Status reads the PID file and checks the process state.
func (d *Daemon) Status() (*Info, error) {
	data, err := d.pids.Load()
	if err != nil {
		return &Info{
			Status: StatusStopped,
			Name:   d.cfg.Name,
		}, nil
	}

	if !d.procs.IsProcessAlive(data.PID) {
		return &Info{
			Status: StatusError,
			PID:    data.PID,
			Port:   data.Port,
			Name:   d.cfg.Name,
		}, nil
	}

	var uptime time.Duration
	if !data.StartTime.IsZero() {
		uptime = time.Since(data.StartTime).Truncate(time.Second)
	}

	return &Info{
		Status:    StatusRunning,
		PID:       data.PID,
		Port:      data.Port,
		Name:      d.cfg.Name,
		StartTime: data.StartTime,
		Uptime:    uptime,
	}, nil
}

// IsRunning returns true if the daemon process is alive according to the PID file.
func (d *Daemon) IsRunning() bool {
	return d.pids.IsAlive()
}

// ConnTracker tracks active client connections to the daemon.
// When IdleTimeout is configured, dropping to zero connections starts an idle
// timer that triggers graceful shutdown.
type ConnTracker struct {
	count   atomic.Int64
	idleCh  chan struct{} // closed when connections drop to 0
	resetCh chan struct{} // signaled when a new connection arrives
}

// NewConnTracker creates a connection tracker.
func NewConnTracker() *ConnTracker {
	return &ConnTracker{
		idleCh:  make(chan struct{}, 1),
		resetCh: make(chan struct{}, 1),
	}
}

// Connect increments the connection count and signals that idle timer
// should be reset.
func (ct *ConnTracker) Connect() {
	ct.count.Add(1)
	// Non-blocking signal to reset idle timer.
	select {
	case ct.resetCh <- struct{}{}:
	default:
	}
}

// Disconnect decrements the connection count. If it drops to zero,
// signals on idleCh so the idle timer can start.
// The count is clamped to zero — an unmatched Disconnect (stray request,
// crashed client) cannot drive it negative and cause premature shutdown.
func (ct *ConnTracker) Disconnect() {
	n := ct.count.Add(-1)
	if n < 0 {
		ct.count.Store(0)
		n = 0
	}
	if n == 0 {
		select {
		case ct.idleCh <- struct{}{}:
		default:
		}
	}
}

// Active returns the current number of active connections.
func (ct *ConnTracker) Active() int64 {
	return ct.count.Load()
}

// Serve runs the daemon in the foreground. Called by the spawned process.
// It picks a free port, registers /health + connection tracking endpoints,
// writes PID file (with binary verification), and blocks until ctx is
// cancelled, SIGINT/SIGTERM is received, or idle timeout fires.
//
// Idle auto-shutdown: if Config.IdleTimeout > 0 and all connections disconnect,
// Serve waits IdleTimeout before initiating graceful shutdown. A new connection
// arriving during the wait resets the timer.
func Serve(ctx context.Context, cfg Config, handler http.Handler) error {
	cfg.applyDefaults()

	startTime := time.Now()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	ct := NewConnTracker()

	mux := http.NewServeMux()
	mux.Handle(cfg.HealthPath, defaultHealthHandler(cfg.Name, startTime))
	mux.HandleFunc("POST /daemon/connect", func(w http.ResponseWriter, r *http.Request) {
		ct.Connect()
		fmt.Fprintf(os.Stderr, "[daemon] agent connected (active: %d, remote: %s)\n", ct.Active(), r.RemoteAddr)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /daemon/disconnect", func(w http.ResponseWriter, r *http.Request) {
		ct.Disconnect()
		fmt.Fprintf(os.Stderr, "[daemon] agent disconnected (active: %d, remote: %s)\n", ct.Active(), r.RemoteAddr)
		w.WriteHeader(http.StatusNoContent)
	})
	if handler != nil {
		mux.Handle("/", handler)
	}

	server := &http.Server{
		Handler:           loopbackGuard(mux, port),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Resolve the daemon binary path for process verification.
	binaryPath, _ := os.Executable()

	pidStore := newDefaultPIDStore(cfg.DataDir, cfg.Name)
	if err := pidStore.Save(os.Getpid(), port, cfg.Name, binaryPath, startTime); err != nil {
		if closeErr := ln.Close(); closeErr != nil {
			// Both errors reported; wrap the primary, log the secondary.
			return fmt.Errorf("write pid file (listener close: %s): %w", closeErr.Error(), err)
		}
		return fmt.Errorf("write pid file: %w", err)
	}

	sigCtx, sigStop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)

	var g Group

	// Actor 1: HTTP server.
	g.Add(
		func() error {
			if serveErr := server.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
				return fmt.Errorf("http serve: %w", serveErr)
			}
			return nil
		},
		func(error) {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutdownCancel()
			if shutErr := server.Shutdown(shutdownCtx); shutErr != nil {
				fmt.Fprintf(os.Stderr, "[daemon] server shutdown: %v\n", shutErr)
			}
		},
	)

	// Actor 2: Signal/context handler.
	g.Add(
		func() error {
			<-sigCtx.Done()
			return sigCtx.Err()
		},
		func(error) {
			sigStop()
		},
	)

	// Actor 3: Idle auto-shutdown timer (only if configured).
	if cfg.IdleTimeout > 0 {
		idleCtx, idleCancel := context.WithCancel(context.Background())
		g.Add(
			func() error {
				return waitForIdle(idleCtx, ct, cfg.IdleTimeout)
			},
			func(error) {
				idleCancel()
			},
		)
	}

	runErr := g.Run()

	clearErr := pidStore.Clear()

	// context.Canceled from signal handler is a clean shutdown.
	if errors.Is(runErr, context.Canceled) {
		runErr = nil
	}

	if runErr != nil {
		return runErr
	}
	if clearErr != nil {
		return fmt.Errorf("clear pid file on shutdown: %w", clearErr)
	}

	return nil
}

// waitForIdle blocks until connections drop to zero AND stay zero for the
// idle timeout duration. Returns nil (triggering Group shutdown) when the
// idle timeout expires without new connections. Returns ctx.Err() if the
// context is cancelled (another actor shut down first).
func waitForIdle(ctx context.Context, ct *ConnTracker, timeout time.Duration) error {
	// If already idle at startup (no connections yet), arm the timer immediately
	// instead of waiting for a Disconnect that will never come.
	if ct.Active() <= 0 {
		select {
		case ct.idleCh <- struct{}{}:
		default:
		}
	}

	for {
		// Wait for connections to drop to zero.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ct.idleCh:
		}

		// Drain any stale reset signals that arrived before connections
		// reached zero. Without this, a buffered resetCh from a prior
		// Connect() would immediately cancel the idle timer.
		select {
		case <-ct.resetCh:
		default:
		}

		// Start idle countdown.
		timer := time.NewTimer(timeout)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-ct.resetCh:
			// New connection arrived — reset.
			timer.Stop()
			continue
		case <-timer.C:
			// Idle timeout expired with zero connections.
			if ct.Active() <= 0 {
				return nil
			}
			// Spurious: connections appeared between check and timer.
			continue
		}
	}
}

// loopbackGuard rejects HTTP requests whose Host header does not resolve to
// a loopback address. This prevents DNS rebinding attacks where a browser
// page tricks the user's machine into sending requests to the local daemon.
// Required by MCP Streamable HTTP transport specification for localhost servers.
func loopbackGuard(next http.Handler, port int) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
		"127.0.0.1":                       true,
		"localhost":                       true,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden: non-loopback host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if !strings.Contains(origin, "://127.0.0.1") &&
				!strings.Contains(origin, "://localhost") {
				http.Error(w, "forbidden: non-loopback origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
