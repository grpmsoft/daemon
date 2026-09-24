// Package daemon provides a reusable, cross-platform daemon lifecycle library.
//
// Two modes of operation:
//
//   - Client mode: Start/Stop/Restart/EnsureRunning/Status -- manage a background daemon process.
//   - Server mode: Serve() -- run as the foreground daemon process with HTTP, PID management, signal handling.
//
// All lifecycle mutations (Start, Stop, Restart, EnsureRunning) serialize on a
// single startup lock file. Public methods are thin wrappers: acquire lock,
// check state, delegate to internal *Locked methods, release lock.
//
// Consumer wires CLI commands; this library provides the operations.
//
//	import "github.com/grpmsoft/daemon"
//
//	d := daemon.New(daemon.Config{Name: "myapp", DataDir: ".myapp", Args: []string{"serve"}})
//	info, err := d.Start(ctx)         // spawn detached background process
//	info, err = d.EnsureRunning(ctx)  // idempotent start-or-attach
//	d.Stop(ctx)                       // graceful shutdown
//	d.Status()                        // check if running, get info
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/grpmsoft/daemon/internal"
	"github.com/grpmsoft/daemon/internal/pidlock"
)

// Daemon manages a background daemon process.
// It depends on PIDStore, ProcessManager, and HealthChecker interfaces —
// not on concrete implementations. New() wires the defaults.
type Daemon struct {
	mu     sync.Mutex // in-process serialization (complements the file lock)
	cfg    Config
	pids   PIDStore
	procs  ProcessManager
	health HealthChecker
}

// New creates a Daemon with the given configuration and default implementations.
// Config is normalized (defaults applied) at construction time and is immutable
// after New returns. Concurrent method calls on the same *Daemon are safe.
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

// Start spawns a detached background process using Config.Binary and Config.Args.
// Returns ErrAlreadyRunning if a daemon is already running.
// The spawned process must eventually call Serve().
//
// Lock protocol:
//   - Acquire startup lock (serializes concurrent Start/EnsureRunning/Stop/Restart)
//   - Under startup lock, check if daemon already running (PID file held)
//   - Delegate to startLocked (acquire PID lock, spawn, health check)
func (d *Daemon) Start(ctx context.Context) (*Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.cfg.Validate(); err != nil {
		return nil, err
	}
	startupFile, err := d.acquireStartupLock(ctx)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", d.cfg.Name, err)
	}
	defer internal.Unlock(startupFile)

	// Explicit Start overrides a previous stop decision.
	clearStopIntent(d.cfg.DataDir, d.cfg.Name)

	pidPath := d.pids.Path()
	if pidlock.IsHeld(pidPath) {
		data, _ := d.pids.Load()
		return nil, fmt.Errorf("%w (pid %d, port %d)", ErrAlreadyRunning, data.PID, data.Port)
	}

	return d.startLocked(ctx)
}

// EnsureRunning is idempotent: if a daemon is already running, returns its
// info. If not, starts a new one. Safe for concurrent callers — all serialize
// on the startup lock.
func (d *Daemon) EnsureRunning(ctx context.Context) (*Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.cfg.Validate(); err != nil {
		return nil, err
	}
	startupFile, err := d.acquireStartupLock(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensure running %s: %w", d.cfg.Name, err)
	}
	defer internal.Unlock(startupFile)

	pidPath := d.pids.Path()
	if pidlock.IsHeld(pidPath) {
		// Daemon already running — wait for port and return existing info.
		port, waitErr := waitForPort(ctx, pidPath, d.cfg.Timeout)
		if waitErr != nil && pidlock.IsHeld(pidPath) {
			return nil, waitErr
		}
		if waitErr == nil {
			_ = port // port is available via pids.Load below
			data, loadErr := d.pids.Load()
			if loadErr != nil {
				return nil, fmt.Errorf("ensure running %s: pid file unreadable: %w", d.cfg.Name, loadErr)
			}
			return d.buildInfo(data), nil
		}
		// Daemon died while waiting — fall through to start.
	}

	// Stop-intent: refuse to auto-start if the user called Hold().
	// Checked AFTER IsHeld so a running daemon (e.g. foreground `serve`) is
	// returned even if a stale stop-intent marker exists.
	if hasStopIntent(d.cfg.DataDir, d.cfg.Name) {
		return nil, ErrStopIntent
	}

	// Spawn cooldown: refuse rapid retry after a recent spawn failure.
	// Checked AFTER IsHeld so a running daemon is returned even if a stale
	// cooldown marker exists from a previous failed attempt.
	if d.cfg.SpawnCooldown > 0 && isCooldownActive(d.cfg.DataDir, d.cfg.Name, d.cfg.SpawnCooldown) {
		return nil, fmt.Errorf("ensure running %s: %w", d.cfg.Name, ErrSpawnCooldown)
	}

	return d.startLocked(ctx)
}

// acquireStartupLock creates DataDir if needed and acquires the startup lock file.
// All lifecycle mutations must hold this lock.
func (d *Daemon) acquireStartupLock(ctx context.Context) (*os.File, error) {
	if err := os.MkdirAll(d.cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", d.cfg.DataDir, err)
	}
	lockPath := filepath.Join(d.cfg.DataDir, d.cfg.Name+".lock")
	return internal.LockCtx(ctx, lockPath)
}

// startLocked starts a new daemon process. Caller must hold the startup lock.
func (d *Daemon) startLocked(ctx context.Context) (*Info, error) {
	pidPath := d.pids.Path()

	lock, tryErr := pidlock.TryLock(pidPath)
	if tryErr != nil {
		if errors.Is(tryErr, pidlock.ErrLocked) {
			return nil, fmt.Errorf("%w: pid file locked", ErrAlreadyRunning)
		}
		return nil, fmt.Errorf("start %s: pid lock: %w", d.cfg.Name, tryErr)
	}
	// Truncate stale data so concurrent readers never see old port.
	_ = lock.WriteData([]byte{})

	_, startErr := d.startWithLock(ctx, lock)
	if startErr != nil {
		_ = lock.File().Close()
		// Skip cooldown on context cancellation — Ctrl-C by one agent must not
		// penalize other agents with ErrSpawnCooldown.
		if !errors.Is(startErr, context.Canceled) && !errors.Is(startErr, context.DeadlineExceeded) {
			_ = writeCooldown(d.cfg.DataDir, d.cfg.Name)
		}
		return nil, fmt.Errorf("start %s: %w", d.cfg.Name, startErr)
	}

	// Successful start -- clear any stale cooldown from a previous failure.
	clearCooldown(d.cfg.DataDir, d.cfg.Name)

	// Parent closes fd — child holds lock via inherited fd (Unix)
	// or via own TryLock (Windows, after setupExtraFiles released parent's).
	_ = lock.File().Close()

	data, loadErr := d.pids.Load()
	if loadErr != nil {
		return nil, fmt.Errorf("start %s: pid file unreadable after start: %w", d.cfg.Name, loadErr)
	}

	return d.buildInfo(data), nil
}

// stopLocked stops the daemon. Caller must hold the startup lock.
func (d *Daemon) stopLocked(ctx context.Context) error {
	data, err := d.pids.Load()
	if err != nil {
		return nil
	}

	if !d.pids.IsAlive() {
		return nil
	}

	// Try graceful shutdown via HTTP first.
	// Budget: 2s for HTTP signal + ShutdownTimeout+2s for daemon drain + 3s kill.
	// Worst case: ShutdownTimeout + 7s. Correct: daemon actually drains.
	const httpSignalTimeout = 2 * time.Second
	if data.Port > 0 {
		shutdownURL := fmt.Sprintf("http://127.0.0.1:%d/daemon/shutdown", data.Port)
		reqCtx, cancel := context.WithTimeout(ctx, httpSignalTimeout)
		req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, shutdownURL, nil)
		if reqErr == nil {
			// Send bearer token if available (v0.3.1+ daemon requires it).
			if data.Token != "" {
				req.Header.Set("Authorization", "Bearer "+data.Token)
			}
			client := &http.Client{}
			resp, doErr := client.Do(req)
			cancel()
			if doErr == nil {
				_ = resp.Body.Close()
				// Wait for daemon to drain: ShutdownTimeout (daemon's own drain budget) + 2s margin.
				waitTimeout := d.cfg.ShutdownTimeout + 2*time.Second
				if waitErr := waitForLockRelease(ctx, d.pids, waitTimeout); waitErr == nil {
					return nil
				} else if ctx.Err() != nil {
					return ctx.Err()
				}
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
		} else {
			cancel()
		}
	}

	// Fallback: force kill with a fixed short grace period.
	const killGrace = 3 * time.Second
	if err := d.procs.KillProcess(ctx, data.PID, killGrace); err != nil {
		return fmt.Errorf("kill daemon %s (pid %d): %w", d.cfg.Name, data.PID, err)
	}

	return nil
}

// buildInfo creates an Info snapshot from PIDInfo.
func (d *Daemon) buildInfo(data PIDInfo) *Info {
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
	}
}

// startWithLock starts a daemon child process with the locked PID file fd
// passed via ExtraFiles. The child inherits the lock — no window where the
// lock is released. Returns the port the daemon is listening on.
// Uses d.cfg.Binary and d.cfg.Args for the command.
func (d *Daemon) startWithLock(ctx context.Context, lock *pidlock.Lock) (int, error) {
	if err := os.MkdirAll(d.cfg.DataDir, 0o750); err != nil {
		return 0, fmt.Errorf("create data dir %s: %w", d.cfg.DataDir, err)
	}

	logFile := filepath.Join(d.cfg.DataDir, d.cfg.Name+".log")

	spec := StartSpec{
		Binary:  d.cfg.Binary,
		Args:    d.cfg.Args,
		Dir:     d.cfg.Dir,
		LogFile: logFile,
		Env: []string{
			"DAEMON_MODE=1",
			fmt.Sprintf("DAEMON_DATA_DIR=%s", d.cfg.DataDir),
		},
	}
	populateExtraFiles(&spec, lock)

	pid, startErr := d.procs.Start(ctx, spec)
	if startErr != nil {
		return 0, fmt.Errorf("start daemon %s: %w", d.cfg.Name, startErr)
	}

	killGrace := d.cfg.ShutdownTimeout / 2
	if err := d.waitForPIDFile(ctx, pid); err != nil {
		_ = d.procs.KillProcess(ctx, pid, killGrace)
		return 0, fmt.Errorf("daemon %s (pid %d) did not become ready, killed: %w", d.cfg.Name, pid, err)
	}

	data, loadErr := d.pids.Load()
	if loadErr != nil {
		_ = d.procs.KillProcess(ctx, pid, killGrace)
		return 0, fmt.Errorf("daemon %s (pid %d) started but pid file unreadable, killed: %w", d.cfg.Name, pid, loadErr)
	}

	if err := d.health.WaitUntilReady(ctx, data.Port, d.cfg.HealthPath, d.cfg.Timeout); err != nil {
		_ = d.procs.KillProcess(ctx, pid, killGrace)
		return 0, fmt.Errorf("daemon %s (pid %d) health check failed, killed: %w", d.cfg.Name, pid, err)
	}

	return data.Port, nil
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

// readLogTail returns the last n lines from the log file for diagnostic messages.
// Returns "" if the file does not exist, is empty, or cannot be read.
func readLogTail(logFile string, n int) string {
	if logFile == "" || n <= 0 {
		return ""
	}
	data, err := os.ReadFile(logFile) //nolint:gosec // trusted path from DataDir
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Stop gracefully shuts down the daemon. Serialized on the startup lock.
// Idempotent: returns nil if the daemon is already stopped.
//
// Stop does NOT prevent EnsureRunning from restarting the daemon. Use
// Hold() to stop the daemon AND block automatic restarts.
func (d *Daemon) Stop(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.cfg.Validate(); err != nil {
		return err
	}
	startupFile, err := d.acquireStartupLock(ctx)
	if err != nil {
		return fmt.Errorf("stop %s: %w", d.cfg.Name, err)
	}
	defer internal.Unlock(startupFile)

	return d.stopLocked(ctx)
}

// Hold stops the daemon and writes a stop-intent marker that prevents
// EnsureRunning from restarting it. Use Release() or Start() to clear
// the marker and allow restarts again.
func (d *Daemon) Hold(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.cfg.Validate(); err != nil {
		return err
	}
	startupFile, err := d.acquireStartupLock(ctx)
	if err != nil {
		return fmt.Errorf("hold %s: %w", d.cfg.Name, err)
	}
	defer internal.Unlock(startupFile)

	_ = d.stopLocked(ctx)
	return writeStopIntent(d.cfg.DataDir, d.cfg.Name)
}

// Release clears the stop-intent marker, allowing EnsureRunning to
// restart the daemon. It is a no-op if no marker exists.
func (d *Daemon) Release() {
	clearStopIntent(d.cfg.DataDir, d.cfg.Name)
}

// Restart stops a running daemon (if any) and starts a new one, under a
// single startup lock acquisition. This prevents the deadlock that would
// occur if Restart called the public Stop() then Start() (each acquires
// the startup lock independently).
func (d *Daemon) Restart(ctx context.Context) (*Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.cfg.Validate(); err != nil {
		return nil, err
	}
	startupFile, err := d.acquireStartupLock(ctx)
	if err != nil {
		return nil, fmt.Errorf("restart %s: %w", d.cfg.Name, err)
	}
	defer internal.Unlock(startupFile)

	// Explicit Restart overrides a previous stop decision.
	clearStopIntent(d.cfg.DataDir, d.cfg.Name)

	_ = d.stopLocked(ctx)
	return d.startLocked(ctx)
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

	if !d.pids.IsAlive() {
		return &Info{
			Status: StatusStopped,
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
	for {
		old := ct.count.Load()
		next := old - 1
		if next < 0 {
			next = 0
		}
		if ct.count.CompareAndSwap(old, next) {
			if next == 0 {
				select {
				case ct.idleCh <- struct{}{}:
				default:
				}
			}
			return
		}
	}
}

// Active returns the current number of active connections.
func (ct *ConnTracker) Active() int64 {
	return ct.count.Load()
}

// Serve runs the daemon in the foreground. Called by the spawned process.
// It picks a free port, registers /health + connection tracking endpoints,
// writes PID data to the locked PID file, and blocks until ctx is
// cancelled, SIGINT/SIGTERM is received, or idle timeout fires.
//
// Lock inheritance: if DAEMON_PIDFD=3 is set (spawned by Start/EnsureRunning),
// Serve inherits the locked fd from the parent via ExtraFiles. If not set
// (manual foreground start), Serve acquires the lock itself.
//
// Idle auto-shutdown: if Config.IdleTimeout > 0 and all connections disconnect,
// Serve waits IdleTimeout before initiating graceful shutdown. A new connection
// arriving during the wait resets the timer.
func Serve(ctx context.Context, cfg Config, handler http.Handler) error {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}

	startTime := time.Now()
	pidPath := filepath.Join(cfg.DataDir, cfg.Name+".pid")

	// Acquire or inherit the PID file lock.
	lock, lockErr := acquireServeLock(pidPath)
	if lockErr != nil {
		return lockErr
	}
	defer lock.Release()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// Generate a control-plane bearer token. Written to the PID file (0600 by
	// pidlock.TryLock), so only the file owner can read it. Clients (Proxy,
	// stopLocked) read the token from the PID file and send it in Authorization
	// headers. rand.Text uses crypto/rand (Go 1.24+).
	token := rand.Text()

	// Write PID data through the lock (Seek+Truncate, preserves inode).
	binaryPath, _ := os.Executable()
	pidData := PIDInfo{
		PID:       os.Getpid(),
		Port:      port,
		Name:      cfg.Name,
		Binary:    binaryPath,
		StartTime: startTime,
		Token:     token,
	}
	pidJSON, _ := json.Marshal(pidData)
	if err := lock.WriteData(pidJSON); err != nil {
		_ = ln.Close()
		return fmt.Errorf("write pid data: %w", err)
	}

	ct := NewConnTracker()
	shutdownCh := make(chan struct{}, 1)

	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()

	mux := buildDaemonMux(cfg, ct, shutdownCh, handler, startTime)

	// Track StateNew connections so they can be closed before Shutdown.
	// net/http.Server.Shutdown waits up to 5s for StateNew connections
	// (Go stdlib behavior). Pre-dialed pooled connections sit in StateNew
	// and cause unnecessary delays under -race and connection pooling.
	var (
		connMu   sync.Mutex
		newConns = make(map[net.Conn]struct{})
	)
	closeNewConns := func() {
		connMu.Lock()
		defer connMu.Unlock()
		for conn := range newConns {
			_ = conn.Close()
		}
		clear(newConns)
	}

	server := &http.Server{
		Handler:           loopbackGuard(authGuard(mux, token, !cfg.DisableTokenAuth, cfg.HealthPath), port),
		ReadHeaderTimeout: 10 * time.Second,
		// BaseContext derives each request's context from serveCtx.
		// serveCancel is called in the HTTP server's interrupt before Shutdown,
		// which cancels all in-flight request contexts (unblocking /daemon/attach).
		BaseContext: func(_ net.Listener) context.Context { return serveCtx },
		ConnState: func(conn net.Conn, state http.ConnState) {
			connMu.Lock()
			defer connMu.Unlock()
			if state == http.StateNew {
				newConns[conn] = struct{}{}
			} else {
				delete(newConns, conn)
			}
		},
	}

	return runServeGroup(server, ln, shutdownCh, ct, cfg.IdleTimeout, cfg.ShutdownTimeout, closeNewConns, serveCancel)
}

// runServeGroup sets up the actor group (HTTP server, signal handler, shutdown
// endpoint, idle timer) and runs them. Extracted from Serve to keep both
// functions within the funlen limit.
//
// cancelBase is called before server.Shutdown to cancel the BaseContext,
// unblocking long-lived handlers like /daemon/attach.
func runServeGroup(server *http.Server, ln net.Listener, shutdownCh chan struct{}, ct *ConnTracker, idleTimeout, shutdownTimeout time.Duration, closeNewConns func(), cancelBase context.CancelFunc) error {
	// Use the server's BaseContext (serveCtx) for signal notifications.
	sigCtx, sigStop := signal.NotifyContext(server.BaseContext(ln), syscall.SIGINT, syscall.SIGTERM)

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
			// Cancel the base context first — this cancels all in-flight request
			// contexts, unblocking long-lived handlers like /daemon/attach.
			cancelBase()
			// Close pre-dialed StateNew connections that would otherwise cause
			// Shutdown to wait up to 5s (Go net/http stdlib behavior).
			closeNewConns()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
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

	// Actor 3: HTTP shutdown endpoint (cross-platform graceful stop).
	shutdownActorCtx, shutdownActorCancel := context.WithCancel(context.Background())
	g.Add(
		func() error {
			select {
			case <-shutdownActorCtx.Done():
				return shutdownActorCtx.Err()
			case <-shutdownCh:
				return nil
			}
		},
		func(error) {
			shutdownActorCancel()
		},
	)

	// Actor 4: Idle auto-shutdown timer (only if configured).
	if idleTimeout > 0 {
		idleCtx, idleCancel := context.WithCancel(context.Background())
		g.Add(
			func() error {
				return waitForIdle(idleCtx, ct, idleTimeout)
			},
			func(error) {
				idleCancel()
			},
		)
	}

	runErr := g.Run()

	// context.Canceled from signal handler is a clean shutdown.
	if errors.Is(runErr, context.Canceled) {
		return nil
	}

	return runErr
}

// waitForLockRelease polls IsAlive under ctx until the lock is released or deadline.
func waitForLockRelease(ctx context.Context, pids PIDStore, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if !pids.IsAlive() {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for daemon to exit")
			}
		}
	}
}

// buildDaemonMux creates the HTTP mux with health, attach, and shutdown endpoints.
func buildDaemonMux(cfg Config, ct *ConnTracker, shutdownCh chan struct{}, handler http.Handler, startTime time.Time) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(cfg.HealthPath, defaultHealthHandler(cfg.Name, startTime))

	// Lease-based connection tracking. The TCP connection IS the lease: when the
	// client process dies, the kernel closes the socket, r.Context() is cancelled,
	// and the connection count drops. No timers, no heartbeats.
	mux.HandleFunc("GET /daemon/attach", func(w http.ResponseWriter, r *http.Request) {
		ct.Connect()
		defer ct.Disconnect()
		fmt.Fprintf(os.Stderr, "[daemon] agent attached (active: %d, remote: %s)\n", ct.Active(), r.RemoteAddr)

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// Write one ack byte so the client knows the lease is established.
		_, _ = w.Write([]byte{0x00})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Block until the client disconnects. r.Context() is cancelled by:
		// - TCP close (client crash/exit) — kernel closes socket
		// - server.Shutdown (via cancelBase which cancels BaseContext)
		// No need to listen on shutdownCh — cancelBase() already propagates.
		<-r.Context().Done()
		fmt.Fprintf(os.Stderr, "[daemon] agent detached (active: %d, remote: %s)\n", ct.Active()-1, r.RemoteAddr)
	})

	mux.HandleFunc("POST /daemon/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(os.Stderr, "[daemon] shutdown requested\n")
		w.WriteHeader(http.StatusAccepted)
		select {
		case shutdownCh <- struct{}{}:
		default:
		}
	})
	if handler != nil {
		mux.Handle("/", handler)
	}
	return mux
}

// acquireServeLock obtains the PID file lock for Serve.
// If DAEMON_PIDFD env is set, inherits the fd from the parent process.
// Otherwise (foreground start), acquires the lock directly.
func acquireServeLock(pidPath string) (*pidlock.Lock, error) {
	if fdStr := os.Getenv("DAEMON_PIDFD"); fdStr != "" {
		fd, err := strconv.Atoi(fdStr)
		if err != nil {
			return nil, fmt.Errorf("invalid DAEMON_PIDFD=%q: %w", fdStr, err)
		}
		return pidlock.InheritFD(fd, pidPath)
	}

	// Foreground mode — acquire lock directly.
	// On Windows, the parent may have just released the PID file lock
	// (setupExtraFiles). Retry briefly in case of transient contention
	// from probes (IsHeld, Status) that also use exclusive CreateFile.
	var lock *pidlock.Lock
	var err error
	for range 20 {
		lock, err = pidlock.TryLock(pidPath)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, pidlock.ErrLocked) {
			return nil, fmt.Errorf("acquire pid lock: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon already running (pid file locked): %w", err)
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

// authGuard is middleware that requires a bearer token for control-plane
// endpoints (/daemon/*). /health stays unauthenticated (used by WaitUntilReady
// and external probes). When requireApp is true, the token is also required for
// the application handler (everything outside /health and /daemon/*).
//
// Chain order: loopbackGuard -> authGuard -> mux.
func authGuard(next http.Handler, token string, requireApp bool, healthPath string) http.Handler {
	tokenBytes := []byte(token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Health endpoint is always unauthenticated (used by WaitUntilReady
		// and external probes).
		needsAuth := false
		if strings.HasPrefix(path, "/daemon/") {
			needsAuth = true
		} else if requireApp && path != healthPath {
			needsAuth = true
		}

		if needsAuth {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			got := []byte(strings.TrimPrefix(auth, "Bearer "))
			if subtle.ConstantTimeCompare(got, tokenBytes) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
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
			if !isLoopbackOrigin(origin) {
				http.Error(w, "forbidden: non-loopback origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackOrigin parses an Origin header value and checks if the hostname
// is a loopback address. Uses net/url.Parse for safe parsing instead of
// string matching, which would pass crafted values like "http://127.0.0.1.evil.com".
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
