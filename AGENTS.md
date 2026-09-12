# daemon -- AI Assistant Guide

## What is daemon?

daemon is a Pure Go cross-platform daemon lifecycle library. It is published
under the [grpmsoft](https://github.com/grpmsoft) organization and consumed by
[GODE](https://github.com/goco-ai/gode) (headless IDE) for shared gopls daemon
management.

It handles the full daemon lifecycle: start, stop, restart, status, health
checks, PID file management, idle auto-shutdown, connection tracking, HTTP
proxy, and concurrent actor orchestration. Zero CGO, zero external dependencies,
stdlib only.

## Quick Start

```go
import "github.com/grpmsoft/daemon"

// Client mode: manage a background process.
d := daemon.New(daemon.Config{Name: "myapp", DataDir: ".myapp"})
d.Start(ctx, "/usr/bin/myapp", []string{"serve"})
d.Stop()
info, _ := d.Status()

// Server mode: run as the daemon process (called by the spawned binary).
daemon.Serve(ctx, daemon.Config{
    Name:        "myapp",
    DataDir:     ".myapp",
    IdleTimeout: 30 * time.Minute,
}, httpHandler)

// Ensure pattern: start-if-needed, return port.
port, _ := daemon.EnsureRunning(ctx, cfg, binary, args)

// Proxy mode: bridge stdin/stdout to daemon HTTP.
daemon.Proxy(ctx, port, "/mcp")
```

## Architecture

```
daemon.go        -- Daemon struct: Start, Stop, Restart, Status, IsRunning, Serve, ConnTracker
config.go        -- Config, Status (enum), Info, PIDStore/ProcessManager/HealthChecker interfaces, PIDInfo
group.go         -- Group: concurrent actor orchestration (modeled after oklog/run.Group)
proxy.go         -- Proxy: stdin/stdout to HTTP bridge with connection tracking
ensure.go        -- EnsureRunning: start-if-needed with race handling
defaults.go      -- Default implementations: pidStoreAdapter, defaultProcessManager, defaultHealthChecker
internal/
  pidfile.go     -- PIDFile: JSON-based PID file with atomic write + process name verification
  process.go     -- Cross-platform process utilities (IsProcessAlive, KillProcess)
  process_unix.go    -- Unix: StartDetached via os.StartProcess with Setsid
  process_windows.go -- Windows: StartDetached via CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS
  health.go      -- HTTP health check polling (fixed 500ms interval)
```

### Key Types

- **Daemon** -- lifecycle manager. Depends on three interfaces (PIDStore,
  ProcessManager, HealthChecker), not on concrete implementations. New() wires
  the defaults; NewWithDeps() accepts mocks for testing.

- **Config** -- controls daemon behavior: Name, DataDir, Timeout, HealthPath,
  IdleTimeout. `applyDefaults()` fills zero values (30s timeout, "/health" path).

- **Status** -- enum (StatusStopped, StatusStarting, StatusRunning,
  StatusStopping, StatusError) with String() method.

- **Info** -- status snapshot from Daemon.Status(): Status, PID, Port, Name,
  StartTime, Uptime.

- **Group** -- concurrent actor manager. Add(execute, interrupt) pairs; Run()
  starts all; first return triggers interrupt on all others. Zero-dependency
  reimplementation of oklog/run.Group.

- **ConnTracker** -- atomic connection counter with idle notification channels.
  Connect() increments and resets idle timer; Disconnect() decrements and
  signals when count drops to zero.

### Key Interfaces

- **PIDStore** -- Save, Load, Clear, IsAlive, Path. Abstracts PID file
  persistence. Default implementation uses JSON file with atomic write.

- **ProcessManager** -- StartDetached, KillProcess, IsProcessAlive. Abstracts
  platform-specific process management (Unix setsid vs Windows
  CREATE_NEW_PROCESS_GROUP).

- **HealthChecker** -- Check, WaitUntilReady. Abstracts HTTP health polling
  with configurable timeout (polls every 500ms).

### Key Functions

- **New(Config)** -- creates a Daemon with default implementations.
- **NewWithDeps(Config, PIDStore, ProcessManager, HealthChecker)** -- creates a
  Daemon with explicit mocks for testing.
- **Serve(ctx, Config, http.Handler)** -- runs as the daemon: picks free port,
  registers /health + /daemon/connect + /daemon/disconnect, writes PID file,
  blocks until signal/context/idle.
- **EnsureRunning(ctx, Config, binary, args)** -- start-if-needed pattern with
  race condition handling. Returns the port.
- **Proxy(ctx, port, mcpPath)** -- bridges stdin/stdout to daemon HTTP endpoint
  with connection tracking (connect on start, disconnect on exit).

## Usage Patterns

### Client Mode (Start/Stop/Status)

Consumer CLI commands call Daemon methods:

```go
d := daemon.New(cfg)
d.Start(ctx, binary, args)  // spawns detached process, waits for health
d.Status()                   // reads PID file, checks process
d.Stop()                     // kills process, clears PID file
d.Restart(ctx, binary, args) // stop + start
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
port, _ := daemon.EnsureRunning(ctx, cfg, binary, args)
daemon.Proxy(ctx, port, "/mcp")
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
| [gode](https://github.com/goco-ai/gode) | Consumer -- shared gopls daemon management |
| [grpmsoft](https://github.com/grpmsoft) | Parent organization |

## Development

```bash
go build ./...                   # Build
go test ./...                    # Test
go test -race ./...              # Race detector
go test -coverprofile=c.out ./... # Coverage
golangci-lint run --timeout=5m   # Lint
```
