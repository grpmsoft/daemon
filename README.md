# daemon

[![CI](https://github.com/grpmsoft/daemon/actions/workflows/ci.yml/badge.svg)](https://github.com/grpmsoft/daemon/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/grpmsoft/daemon.svg)](https://pkg.go.dev/github.com/grpmsoft/daemon)
[![codecov](https://codecov.io/gh/grpmsoft/daemon/branch/main/graph/badge.svg)](https://codecov.io/gh/grpmsoft/daemon)
[![Go Version](https://img.shields.io/github/go-mod/go-version/grpmsoft/daemon)](https://github.com/grpmsoft/daemon/blob/main/go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Cross-platform daemon lifecycle library for Go applications.** Pure Go, zero CGO, zero dependencies.

Manages background daemon processes with HTTP health checks, PID file locking, signal handling, connection tracking, and idle auto-shutdown. Built for tools that need a persistent background server -- MCP servers, language servers, dev tools.

## Features

- **Client mode** -- Start/Stop/Restart/Status to manage a background daemon from any CLI
- **Server mode** -- `Serve()` runs as a foreground daemon with automatic port selection, PID file, signal handling, and health endpoint
- **Proxy mode** -- `Proxy()` bridges stdin/stdout to the daemon's HTTP endpoint for MCP/JSON-RPC protocols
- **EnsureRunning** -- "start if needed" with race-safe PID locking (two agents calling simultaneously is safe)
- **run.Group** -- actor concurrency pattern (oklog/run reimplemented, zero deps) for composing HTTP server + signal handler + idle timer
- **Connection tracking** -- atomic connect/disconnect counting with idle auto-shutdown when all clients leave
- **Rich Model** -- `Daemon` struct depends on interfaces (`PIDStore`, `ProcessManager`, `HealthChecker`), not concrete implementations. Testable by design
- **Cross-platform** -- Windows (`CREATE_NEW_PROCESS_GROUP`), Linux/macOS (`setsid`) with platform-specific process management in `internal/`
- **Pure Go** -- no CGO, no assembly, stdlib only

## Installation

```bash
go get github.com/grpmsoft/daemon
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/grpmsoft/daemon"
)

func main() {
    cfg := daemon.Config{
        Name:    "myapp",
        DataDir: ".myapp",
        Binary:  "", // empty = os.Executable()
        Args:    []string{"serve"},
    }

    d := daemon.New(cfg)

    // Start a background daemon.
    info, err := d.Start(context.Background())
    if err != nil {
        fmt.Fprintf(os.Stderr, "start: %v\n", err)
        os.Exit(1)
    }
    fmt.Printf("Started: PID %d, Port %d\n", info.PID, info.Port)

    // Check status.
    info, _ = d.Status()
    fmt.Printf("Status: %s, PID: %d, Port: %d\n", info.Status, info.PID, info.Port)

    // Stop the daemon.
    if err := d.Stop(context.Background()); err != nil {
        fmt.Fprintf(os.Stderr, "stop: %v\n", err)
    }
}
```

## Server Mode

The spawned process calls `Serve()` to run as the daemon. It picks a free port, writes a PID file, registers a `/health` endpoint, and blocks until shutdown.

```go
package main

import (
    "context"
    "fmt"
    "net/http"
    "os"
    "time"

    "github.com/grpmsoft/daemon"
)

func main() {
    cfg := daemon.Config{
        Name:        "myapp",
        DataDir:     ".myapp",
        IdleTimeout: 5 * time.Minute, // auto-shutdown after 5 min idle
    }

    mux := http.NewServeMux()
    mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintln(w, "hello from daemon")
    })

    // Blocks until SIGINT/SIGTERM, context cancel, or idle timeout.
    if err := daemon.Serve(context.Background(), cfg, mux); err != nil {
        fmt.Fprintf(os.Stderr, "serve: %v\n", err)
        os.Exit(1)
    }
}
```

`Serve()` uses a `Group` (actor pattern) internally: HTTP server + signal handler + idle timer run as concurrent actors. When any actor returns, all others are interrupted.

## Proxy Mode

`Proxy()` bridges stdin/stdout to the daemon's HTTP endpoint. Reads newline-delimited JSON from stdin, POSTs each message to the daemon, writes responses to stdout. Tracks connections for idle auto-shutdown.

```go
// In your CLI's "mcp serve" command:
port, err := daemon.EnsureRunning(ctx, cfg)
if err != nil {
    return err
}
return daemon.Proxy(ctx, cfg, daemon.ProxyOptions{MCPPath: "/mcp"})
```

`ProxyOptions` supports custom `Stdin`/`Stdout` (for testing) and `LogPayloads` (opt-in, disabled by default for security).

## EnsureRunning

`EnsureRunning` is the main entry point for consumers. It checks if a daemon is already running and returns its port. If not, it starts a new one and waits for the health check to pass.

Available as both a method on `*Daemon` (returns `*Info`) and a package-level convenience function (returns port as `int`):

```go
// Package-level convenience (creates a Daemon internally):
port, err := daemon.EnsureRunning(ctx, daemon.Config{
    Name:    "myapp",
    DataDir: ".myapp",
    Args:    []string{"serve"},
})
if err != nil {
    return err
}
fmt.Printf("Daemon ready on port %d\n", port)

// Method form (when you already have a Daemon instance):
d := daemon.New(cfg)
info, err := d.EnsureRunning(ctx)
```

Race-safe: if two agents call `EnsureRunning` simultaneously and one wins the start, the other detects the running daemon via the PID file and returns its port.

## Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Name` | `string` | (required) | Application name. Used for PID file naming and health response |
| `DataDir` | `string` | (required) | Directory for runtime files (PID, logs) |
| `Binary` | `string` | `os.Executable()` | Executable path for the daemon process. Empty uses current binary |
| `Args` | `[]string` | `nil` | Arguments passed to the child process in `Start()`/`Restart()` |
| `Timeout` | `time.Duration` | `30s` | How long `Start()` waits for health check to pass |
| `HealthPath` | `string` | `"/health"` | HTTP path for the health endpoint |
| `IdleTimeout` | `time.Duration` | `0` (disabled) | Auto-shutdown after this duration with zero connections |
| `Dir` | `string` | `DataDir` | Working directory for spawned daemon. Prevents holding parent's CWD |
| `RequireToken` | `bool` | `false` | When true, bearer token required for app handler too |

**Note:** `DataDir` will contain a persistent `<Name>.lock` file used to serialize concurrent `EnsureRunning` calls. After a binary upgrade (`go install`), `EnsureRunning` returns the existing daemon's port -- call `Restart()` to pick up the new binary. Set `Binary` and `Args` in Config once; all lifecycle methods (`Start`, `Stop`, `Restart`, `EnsureRunning`) use them automatically.

## Control-Plane Endpoints

`Serve()` registers internal endpoints for lifecycle management. All `/daemon/*` endpoints require a bearer token (generated at startup, stored in the PID file).

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `GET` | `/health` | None | Health check (used by `WaitUntilReady`) |
| `GET` | `/daemon/attach` | Bearer | Lease-based connection tracking (v0.3.1+) |
| `POST` | `/daemon/connect` | Bearer | Increment connection count (deprecated, use attach) |
| `POST` | `/daemon/disconnect` | Bearer | Decrement connection count (deprecated, use attach) |
| `POST` | `/daemon/shutdown` | Bearer | Graceful shutdown |

### Lease-Based Connection Tracking

`Proxy()` opens `GET /daemon/attach` and keeps the TCP connection alive for its lifetime. The connection IS the lease -- when the proxy process dies (SIGKILL, OOM, closed terminal), the kernel closes the socket, the connection count drops, and idle auto-shutdown can proceed. No heartbeats, no timers, no stray counts from crashed clients.

If the daemon doesn't support `/daemon/attach` (v0.3.0), the proxy falls back to `POST /daemon/connect` and `POST /daemon/disconnect`.

## run.Group

`Group` is a zero-dependency reimplementation of the [oklog/run](https://github.com/oklog/run) actor pattern. Start multiple concurrent actors; when any one returns, all others are interrupted.

```go
var g daemon.Group

g.Add(
    func() error { return httpServer.Serve(ln) },
    func(error) { httpServer.Shutdown(ctx) },
)

g.Add(
    func() error { <-sigCtx.Done(); return sigCtx.Err() },
    func(error) { sigStop() },
)

err := g.Run() // blocks until all actors stop
```

## Testing

All interfaces (`PIDStore`, `ProcessManager`, `HealthChecker`) are designed for test injection via `NewWithDeps()`:

```go
d := daemon.NewWithDeps(cfg, mockPIDStore, mockProcs, mockHealth)
```

```bash
go test ./...
```

## Not a Service Manager

`grpmsoft/daemon` is not a system service manager. If you need a long-lived daemon under systemd, launchd, or Windows SCM, use [kardianos/service](https://github.com/kardianos/service) -- it is the industry standard for that job.

`grpmsoft/daemon` manages a background process **on demand**: your CLI starts it when needed, shares it between clients, and shuts it down when idle. The owner of the process lifetime is **demand**, not an init system. The two libraries complement each other: if you need your tool to be present from boot, run `myapp serve` under kardianos/service and skip `EnsureRunning`.

## Contributing

Contributions are welcome. Please open an issue or pull request on [GitHub](https://github.com/grpmsoft/daemon).

See [CONTRIBUTING.md](CONTRIBUTING.md) for development guidelines and [CHANGELOG.md](CHANGELOG.md) for release history.

## Star History

<a href="https://starhistory.io">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.starhistory.io/png?repos=grpmsoft/daemon&style=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.starhistory.io/png?repos=grpmsoft/daemon&style=professional" />
   <img alt="Star History Chart" src="https://api.starhistory.io/png?repos=grpmsoft/daemon" width="800" />
 </picture>
</a>

## License

MIT License. See [LICENSE](LICENSE).
