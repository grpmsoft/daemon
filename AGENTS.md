# daemon -- AI Assistant Guide

## What is daemon?

daemon is a Pure Go cross-platform daemon lifecycle library. It is published
under the [grpmsoft](https://github.com/grpmsoft) organization and consumed by
GLIDE (headless IDE, codename GODE) for shared gopls daemon
management.

It handles the full daemon lifecycle: start, stop, restart, status, health
checks, PID file management, idle auto-shutdown, connection tracking, HTTP
proxy, and concurrent actor orchestration. Zero CGO, zero external dependencies,
stdlib only.

## Quick Start

```go
import "github.com/grpmsoft/daemon"

// Client mode: manage a background process.
cfg := daemon.Config{
    Name:    "myapp",
    DataDir: ".myapp",
    Binary:  "/usr/bin/myapp", // empty = os.Executable()
    Args:    []string{"serve"},
}
d := daemon.New(cfg)
info, err := d.Start(ctx)       // returns *Info with PID, Port, StartTime
info, err = d.EnsureRunning(ctx) // idempotent: returns existing if running
d.Stop(ctx)
info, _ = d.Status()

// Server mode: run as the daemon process (called by the spawned binary).
daemon.Serve(ctx, daemon.Config{
    Name:        "myapp",
    DataDir:     ".myapp",
    IdleTimeout: 30 * time.Minute,
}, httpHandler)

// Ensure pattern (package-level convenience): start-if-needed, return port.
port, _ := daemon.EnsureRunning(ctx, cfg)

// Proxy mode: bridge stdin/stdout to daemon HTTP.
daemon.Proxy(ctx, cfg, daemon.ProxyOptions{MCPPath: "/mcp"})
```

## Architecture

```
daemon.go        -- Daemon struct: Start, Stop, Restart, EnsureRunning, Status, IsRunning, Serve, ConnTracker
                    Internal helpers: acquireStartupLock, startLocked, stopLocked, buildInfo
config.go        -- Config (with Binary, Args), Status (enum), Info, PIDStore/ProcessManager/HealthChecker interfaces, PIDInfo
group.go         -- Group: concurrent actor orchestration (modeled after oklog/run.Group)
proxy.go         -- Proxy: stdin/stdout to HTTP bridge with connection tracking + IsHeld pre-check
ensure.go        -- EnsureRunning (package-level wrapper): start-if-needed with race handling
defaults.go      -- Default implementations: pidStoreAdapter, defaultProcessManager, defaultHealthChecker
internal/
  pidlock/       -- Cross-platform lock primitive (TryLock, Release, WriteData, ReadLocked, IsHeld, InheritFD)
  process.go     -- Cross-platform process utilities (IsProcessAlive, KillProcess)
  process_unix.go    -- Unix: StartDetached via os.StartProcess with Setsid
  process_windows.go -- Windows: StartDetached via CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS
  health.go      -- HTTP health check polling (fixed 500ms interval)
```

### Key Types

- **Daemon** -- lifecycle manager. Depends on three interfaces (PIDStore,
  ProcessManager, HealthChecker), not on concrete implementations. New() wires
  the defaults; NewWithDeps() accepts mocks for testing.

- **Config** -- controls daemon behavior: Name, DataDir, Binary (default
  os.Executable()), Args, Timeout, HealthPath, IdleTimeout, RequireToken.
  `applyDefaults()` fills zero values (30s timeout, "/health" path,
  os.Executable() for Binary). RequireToken (bool, default false) gates
  bearer token on the app handler.

- **Status** -- enum (StatusStopped, StatusStarting, StatusRunning,
  StatusStopping, StatusError) with String() method.

- **Info** -- status snapshot from Daemon.Status(): Status, PID, Port, Name,
  StartTime, Uptime.

- **Group** -- concurrent actor manager. Add(execute, interrupt) pairs; Run()
  starts all; first return triggers interrupt on all others. Zero-dependency
  reimplementation of oklog/run.Group.

- **ConnTracker** -- atomic connection counter with idle notification channels.
  Connect() increments and resets idle timer; Disconnect() decrements and
  signals when count drops to zero. /daemon/attach uses TCP connection as
  lease (kernel close = auto-disconnect).

### Key Interfaces

- **PIDStore** -- Save, Load, Clear, IsAlive, Path. Abstracts PID file
  persistence. Default implementation uses JSON file with atomic write.

- **ProcessManager** -- StartDetached, KillProcess(ctx, pid, grace), IsProcessAlive.
  Abstracts platform-specific process management (Unix setsid vs Windows
  CREATE_NEW_PROCESS_GROUP). KillProcess accepts context and grace period.

- **HealthChecker** -- Check, WaitUntilReady. Abstracts HTTP health polling
  with configurable timeout (polls every 500ms).

### Key Functions

- **New(Config)** -- creates a Daemon with default implementations.
- **NewWithDeps(Config, PIDStore, ProcessManager, HealthChecker)** -- creates a
  Daemon with explicit mocks for testing.
- **Serve(ctx, Config, http.Handler)** -- runs as the daemon: picks free port,
  generates bearer token (rand.Text), registers /health + /daemon/attach +
  /daemon/connect + /daemon/disconnect + /daemon/shutdown, writes PID file
  (with token), blocks until signal/context/idle.
- **EnsureRunning(ctx, Config) (int, error)** -- package-level convenience.
  Creates a Daemon internally, calls d.EnsureRunning(ctx), returns port.
- **Proxy(ctx, cfg, ProxyOptions)** -- bridges stdin/stdout to daemon HTTP endpoint
  with connection tracking. Tries lease-based attach first (GET /daemon/attach),
  falls back to connect/disconnect for v0.3.0 daemons. Reads bearer token from
  PID file. Checks `IsHeld` before dialing (returns `ErrNotRunning` for dead
  daemons instead of connection refused).

### Daemon Methods

- **Start(ctx) (*Info, error)** -- spawn detached background process, wait for
  health check. Returns error if already running. Binary/Args from Config.
- **EnsureRunning(ctx) (*Info, error)** -- idempotent start-if-needed. Returns
  existing *Info if daemon is already running.
- **Stop(ctx) error** -- graceful shutdown serialized on startup lock: HTTP
  /daemon/shutdown -> wait -> fallback to kill.
- **Restart(ctx) (*Info, error)** -- stop + start under ONE lock acquisition.
- **Status() (*Info, error)** -- read-only, no lock. Reads PID file, checks
  process liveness, returns status snapshot.
- **IsRunning() bool** -- read-only, no lock. Quick check via PID file.

All mutating methods (Start, Stop, Restart, EnsureRunning) serialize on the
startup lock. Read-only methods (Status, IsRunning) do not acquire the lock.

## Usage Patterns

### Client Mode (Start/Stop/Status)

Consumer CLI commands call Daemon methods. Binary and args are in Config:

```go
cfg := daemon.Config{Name: "myapp", DataDir: ".myapp", Args: []string{"serve"}}
d := daemon.New(cfg)
info, err := d.Start(ctx)    // spawns detached process, waits for health, returns *Info
info, _ = d.Status()         // reads PID file, checks process (no lock)
d.Stop(ctx)                  // graceful shutdown (serialized on lock)
info, _ = d.Restart(ctx)     // stop + start under one lock
```

### Server Mode (Serve)

The spawned process calls Serve() to become the daemon:

```go
daemon.Serve(ctx, cfg, myHandler) // blocks until shutdown
```

Serve() handles: free port selection, PID file write with binary path
verification, signal trapping (SIGINT/SIGTERM), idle auto-shutdown via
ConnTracker, and graceful HTTP server shutdown.

### Proxy Mode (stdio bridge)

MCP clients (Claude Code) communicate via stdio. The proxy bridges this to
the daemon's HTTP endpoint:

```go
port, _ := daemon.EnsureRunning(ctx, cfg) // Binary and Args are in cfg
daemon.Proxy(ctx, cfg, daemon.ProxyOptions{MCPPath: "/mcp"})
```

### Testing

Use NewWithDeps() to inject mock implementations:

```go
d := daemon.NewWithDeps(cfg, mockPIDs, mockProcs, mockHealth)
```

All three interfaces (PIDStore, ProcessManager, HealthChecker) are small and
easy to mock. See daemon_test.go for examples.

## Ecosystem

| Repo | Relationship |
|------|-------------|
| GLIDE (codename GODE) | Consumer -- shared gopls daemon management |
| [grpmsoft](https://github.com/grpmsoft) | Parent organization |

## Development

```bash
go build ./...                   # Build
go test ./...                    # Test
go test -race ./...              # Race detector
go test -coverprofile=c.out ./... # Coverage
golangci-lint run --timeout=5m   # Lint
```
