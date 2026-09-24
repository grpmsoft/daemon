# daemon -- AI Assistant Guide

## What is daemon?

Pure-Go on-demand process lifecycle and local IPC layer for shared background
services used by CLI tools and agents. Published under
[grpmsoft](https://github.com/grpmsoft), consumed by GLIDE (headless IDE,
codename GODE) for shared gopls daemon management.

Your CLI starts a background process when needed, shares it between multiple
clients, and shuts it down when idle. The owner of the process lifetime is
demand, not an init system. Zero CGO, zero dependencies, stdlib only.

Not a system service manager. For systemd/launchd/SCM use kardianos/service.
The two are complementary. Do not mix `Restart=always` with idle auto-shutdown.

## Quick Start

```go
import "github.com/grpmsoft/daemon"

cfg := daemon.Config{
    Name:    "myapp",
    DataDir: ".myapp",
    Args:    []string{"serve"},
}

// Package-level convenience: start-if-needed, return port.
port, _ := daemon.EnsureRunning(ctx, cfg)

// Method form: full control.
d := daemon.New(cfg)
info, _ := d.Start(ctx)           // error if already running
info, _ = d.EnsureRunning(ctx)    // idempotent
info, _ = d.Restart(ctx)          // stop + start, one lock
d.Stop(ctx)                       // graceful shutdown
info, _ = d.Status()              // read-only, no lock

// Server mode (called by spawned binary).
daemon.Serve(ctx, cfg, httpHandler)

// Proxy: stdin/stdout ↔ HTTP bridge for MCP/JSON-RPC.
daemon.Proxy(ctx, cfg, daemon.ProxyOptions{MCPPath: "/mcp"})
```

## Architecture

```
daemon.go           -- Daemon struct: Start/Stop/Restart/EnsureRunning/Status,
                       acquireStartupLock, startLocked, stopLocked, buildInfo,
                       Serve (server mode), ConnTracker, waitForIdle,
                       /daemon/attach (lease), authGuard, loopbackGuard
config.go           -- Config, Status (enum), Info, PIDInfo, StartSpec,
                       PIDStore/ProcessManager/HealthChecker interfaces
group.go            -- Group: concurrent actor orchestration (oklog/run, zero deps)
proxy.go            -- Proxy: stdin/stdout ↔ HTTP bridge, proxyAttach (lease),
                       IsHeld pre-check
ensure.go           -- EnsureRunning (package-level wrapper), waitForPort
defaults.go         -- Default implementations: pidStoreAdapter,
                       defaultProcessManager, defaultHealthChecker
errors.go           -- Sentinel errors: ErrNotRunning, ErrAlreadyRunning,
                       ErrStartTimeout, ErrUnhealthy, ErrStalePIDFile, ErrInvalidConfig
internal/
  pidlock/          -- Cross-platform lock primitive: TryLock, Release,
                       WriteData, ReadLocked, IsHeld, InheritFD
  pidfile.go        -- PIDFile: JSON persistence + process verification
  process.go        -- Cross-platform process utilities
  process_unix.go   -- Unix: StartProcess (setsid), KillProcess (SIGTERM→SIGKILL)
  process_windows.go -- Windows: StartProcess (CREATE_NEW_PROCESS_GROUP),
                       KillProcess (TerminateProcess→taskkill)
  health.go         -- HTTP health check polling (fixed 500ms interval)
  lock_unix.go      -- LockCtx: ctx-aware flock with LOCK_NB poll loop
  lock_windows.go   -- LockCtx: ctx-aware LockFileEx
```

### Key Types

- **Daemon** -- lifecycle manager. Depends on three interfaces, not concrete
  implementations. `New()` wires defaults; `NewWithDeps()` accepts mocks.
  Config is immutable after construction -- concurrent calls safe.

- **Config** -- Name, DataDir, Binary (default: os.Executable()), Args,
  Dir (default: DataDir, absolute), Timeout (30s), HealthPath ("/health"),
  IdleTimeout (0=disabled), DisableTokenAuth (false=secure). Validated in all public methods.

- **Info** -- status snapshot: Status, PID, Port, Name, StartTime, Uptime.
  Returned by Start, EnsureRunning, Restart, Status.

- **Group** -- concurrent actor manager. Add(execute, interrupt) pairs; Run()
  starts all; first return triggers interrupt on all others.

- **ConnTracker** -- atomic connection counter with idle notification.

### Key Interfaces

- **PIDStore** -- Save, Load, Clear, IsAlive, Path. Abstracts PID file persistence.
- **ProcessManager** -- Start(ctx, StartSpec), KillProcess(ctx, pid, grace), IsProcessAlive.
- **HealthChecker** -- Check, WaitUntilReady.

### Lock Protocol

All lifecycle mutations serialize on a startup lock file (flock/LockFileEx).
Public methods never call other public methods (prevents OFD deadlock).

```
Start(ctx)         → acquireStartupLock → IsHeld? err : startLocked
EnsureRunning(ctx) → acquireStartupLock → IsHeld? existing : startLocked
Stop(ctx)          → acquireStartupLock → stopLocked
Restart(ctx)       → acquireStartupLock → stopLocked + startLocked
Status()           → NO lock (read-only)
IsRunning()        → NO lock (read-only)
```

### Lease-Based Connection Tracking

TCP connection lifetime = lease lifetime. Client crash → kernel closes socket →
count drops → idle shutdown proceeds. No heartbeats, no timers, no stray counts.

### Proxy Limitations

- One request in flight (sequential dispatch, not pipelined)
- No server-to-client notifications (unidirectional)
- Not Streamable-HTTP/SSE compatible (plain JSON-RPC POST only)
- Use direct HTTP connection for concurrent tool calls

### Security

Bearer token (crypto/rand) stored in PID file (0600). Required for /daemon/*
and app handler by default (DisableTokenAuth=false). /health stays open.
DNS rebinding: loopbackGuard + token.

## Positioning

On-demand background process owned by CLI callers -- NOT a system service
manager. For systemd/launchd/SCM use kardianos/service. Complementary,
not competing. Do not mix `Restart=always` with idle auto-shutdown.

## Ecosystem

| Repo | Relationship |
|------|-------------|
| GLIDE (codename GODE) | Consumer -- shared gopls daemon management |
| [grpmsoft](https://github.com/grpmsoft) | Parent organization |

## Development

```bash
go build ./...
go test ./...
go test -race ./...
go test -coverprofile=c.out ./...
golangci-lint run --timeout=5m
```
