# daemon

[![CI](https://github.com/grpmsoft/daemon/actions/workflows/ci.yml/badge.svg)](https://github.com/grpmsoft/daemon/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/grpmsoft/daemon.svg)](https://pkg.go.dev/github.com/grpmsoft/daemon)
[![codecov](https://codecov.io/gh/grpmsoft/daemon/branch/main/graph/badge.svg)](https://codecov.io/gh/grpmsoft/daemon)
[![Go Version](https://img.shields.io/github/go-mod/go-version/grpmsoft/daemon)](https://github.com/grpmsoft/daemon/blob/main/go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Pure-Go on-demand process lifecycle and local IPC layer for shared background services used by CLI tools and agents.**

Zero CGO. Zero dependencies. Cross-platform.

Your CLI starts a background process when needed, shares it between multiple clients, and shuts it down when idle. The owner of the process lifetime is **demand**, not an init system.

```
CLI #1 ──┐
         │    EnsureRunning
CLI #2 ──┼──────────────────► daemon process ──► idle ──► shutdown
         │                        │
CLI #3 ──┘                   lease tracking
                            (zero connections
                              + IdleTimeout)
```

## Why

MCP servers, language servers, and dev tools need a persistent background process shared by multiple agents. Without lifecycle management, each agent spawns its own instance -- 6 agents x ~1-2 GB gopls = 6-12 GB RAM.

`daemon` solves this with one abstraction: **`EnsureRunning`** -- start if needed, return the port if already running. Race-safe: two agents calling simultaneously never produce two daemons. The loser detects the winner via a kernel-level file lock and returns the same port.

## Installation

```bash
go get github.com/grpmsoft/daemon
```

## Quick Start

```go
cfg := daemon.Config{
    Name:    "myapp",
    DataDir: ".myapp",
    Args:    []string{"serve"},
}

// Start-if-needed, get port (package-level convenience):
port, err := daemon.EnsureRunning(ctx, cfg)

// Or use the method form for full control:
d := daemon.New(cfg)
info, err := d.Start(ctx)           // error if already running
info, err = d.EnsureRunning(ctx)    // idempotent
info, err = d.Restart(ctx)          // stop + start, one lock
err = d.Stop(ctx)                   // graceful shutdown
info, err = d.Status()              // read-only, no lock

// Explicit autostart prevention:
err = d.Hold(ctx)                   // stop + write .stop-intent marker
d.Release()                         // clear marker, allow restarts
```

All mutating methods serialize on a startup lock and an in-process mutex. Public methods never call other public methods. Config is immutable after `New()` -- concurrent calls are safe.

## Three Modes

### Client Mode

Your CLI manages a background daemon:

```go
d := daemon.New(daemon.Config{
    Name:    "myapp",
    DataDir: ".myapp",
    Args:    []string{"serve"},
})

info, err := d.Start(ctx)    // spawn, wait for health
info, _ := d.Status()        // PID, port, uptime
d.Stop(ctx)                  // graceful shutdown
```

### Server Mode

The spawned process calls `Serve()` to become the daemon. It picks a free port, writes a PID file with a bearer token, registers `/health`, and blocks until shutdown.

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
    fmt.Fprintln(w, "hello from daemon")
})

daemon.Serve(ctx, daemon.Config{
    Name:        "myapp",
    DataDir:     ".myapp",
    IdleTimeout: 5 * time.Minute,
}, mux)
```

`Serve()` uses a `Group` (actor pattern) internally: HTTP server + signal handler + idle timer + shutdown endpoint run as concurrent actors. When any actor returns, all others are interrupted.

### Proxy Mode

`Proxy()` bridges stdin/stdout to the daemon's HTTP endpoint for MCP/JSON-RPC protocols:

```go
port, _ := daemon.EnsureRunning(ctx, cfg)
daemon.Proxy(ctx, cfg, daemon.ProxyOptions{MCPPath: "/mcp"})
```

`ProxyOptions` fields: `MCPPath` (HTTP path, default `"/mcp"`), `Stdin`/`Stdout` (`io.Reader`/`io.Writer` -- replaceable for testing, default `os.Stdin`/`os.Stdout`), `LogPayloads` (opt-in, default `false` -- when enabled, logs request/response bodies to `proxy.log`; disabled by default because payloads may contain sensitive data).

The proxy automatically establishes a lease-based connection to the daemon (see below) and forwards newline-delimited JSON between stdin and the daemon's HTTP endpoint.

## Lease-Based Connection Tracking

The lifetime of a TCP connection **is** the lease. When a proxy process dies -- SIGKILL, OOM, closed terminal -- the kernel closes the socket, the daemon sees the disconnect, and the connection count drops. No heartbeats. No timers. No stray counts from crashed clients.

```
Proxy process          Daemon
     │                    │
     ├── GET /daemon/attach ──► ct.Connect()
     │         │          │
     │    TCP alive       │     connection count > 0
     │    = lease held    │     idle timer blocked
     │         │          │
     ╳ SIGKILL            │
     │                    │
  kernel closes socket ──► ct.Disconnect()
                          │
                     count drops to 0
                          │
                     IdleTimeout fires
                          │
                     graceful shutdown
```

### Proxy Limitations

- One request in flight (sequential dispatch, not pipelined)
- No server-to-client notifications (unidirectional)
- Not Streamable-HTTP/SSE compatible (plain JSON-RPC POST only)
- Use direct HTTP connection for concurrent tool calls

## Hold / Release

`Stop()` shuts down the daemon but does **not** prevent `EnsureRunning` from starting it again. If you need the daemon to stay down -- for example, during a binary upgrade or manual debugging -- use `Hold()`:

```go
d := daemon.New(cfg)

// Stop and block automatic restarts:
err := d.Hold(ctx)  // writes .stop-intent marker

// Any subsequent EnsureRunning returns ErrStopIntent:
_, err = d.EnsureRunning(ctx) // err == daemon.ErrStopIntent

// Allow restarts again:
d.Release()                   // clears the marker
// -- or --
d.Start(ctx)                  // Start and Restart also clear the marker
```

The stop-intent marker is a file in `DataDir`. It survives process crashes. `Start()` and `Restart()` clear it implicitly -- an explicit start decision overrides a previous hold.

## Spawn Cooldown

When a spawn fails (binary not found, port conflict, health check timeout), `EnsureRunning` writes a cooldown marker. During the cooldown window, subsequent `EnsureRunning` calls return `ErrSpawnCooldown` immediately instead of retrying. This prevents rapid respawn loops when multiple MCP agents call `EnsureRunning` after a failure.

```go
cfg := daemon.Config{
    Name:          "myapp",
    DataDir:       ".myapp",
    SpawnCooldown: 10 * time.Second, // default: 5s, -1 to disable
}

d := daemon.New(cfg)
_, err := d.EnsureRunning(ctx) // spawn fails
_, err = d.EnsureRunning(ctx)  // err == daemon.ErrSpawnCooldown (within 10s)
```

The cooldown marker is time-based: it expires after `SpawnCooldown` elapses. A successful `Start()` or `Restart()` clears it. Context cancellation skips the cooldown check.

## Shutdown Timeout

`Config.ShutdownTimeout` (default `10s`) controls the total time budget for graceful shutdown. The budget is split across shutdown phases:

| Phase | Budget | Description |
|-------|--------|-------------|
| HTTP shutdown request | `ShutdownTimeout / 2` | `Stop()` client sends POST /daemon/shutdown |
| Wait for lock release | `ShutdownTimeout` | Client waits for daemon process to exit |
| Kill grace period | `ShutdownTimeout / 2` | Fallback: SIGTERM/TerminateProcess if still running |

On the server side, `Serve()` uses the same timeout for `http.Server.Shutdown` drain.

```go
cfg := daemon.Config{
    Name:            "myapp",
    DataDir:         ".myapp",
    ShutdownTimeout: 15 * time.Second, // default: 10s
}
```

## Security

`Serve()` generates a bearer token (`crypto/rand`) at startup and stores it in the PID file (`0600` permissions). All `/daemon/*` control-plane endpoints require `Authorization: Bearer <token>`. The `/health` endpoint stays open for external probes. The application handler (everything outside `/health` and `/daemon/*`) also requires the token by default (secure by default since v0.4.0).

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `GET` | `/health` | None | Readiness probe |
| `GET` | `/daemon/attach` | Bearer | Lease connection |
| `POST` | `/daemon/shutdown` | Bearer | Graceful stop |

DNS rebinding protection: `loopbackGuard` middleware rejects requests with non-loopback `Host` or `Origin` headers. Combined with the bearer token, a page with a rebinding domain that passes the Host check still cannot authenticate.

Set `Config.DisableTokenAuth = true` to skip token auth for the application handler (e.g., for local curl debugging).

## Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Name` | `string` | (required) | Application name, used for PID file naming |
| `DataDir` | `string` | (required) | Directory for runtime files (PID, logs). Resolved to absolute path |
| `Binary` | `string` | `os.Executable()` | Executable path for the daemon process |
| `Args` | `[]string` | `nil` | Arguments passed to the child process |
| `Dir` | `string` | `DataDir` | Working directory for the spawned daemon |
| `Timeout` | `time.Duration` | `30s` | How long `Start()` waits for health check |
| `HealthPath` | `string` | `"/health"` | HTTP path for the health endpoint |
| `IdleTimeout` | `time.Duration` | `0` (disabled) | Auto-shutdown after this duration with zero connections |
| `DisableTokenAuth` | `bool` | `false` | Skip bearer token auth for the application handler |
| `SpawnCooldown` | `time.Duration` | `5s` | Cooldown after spawn failure; `-1` to disable |
| `ShutdownTimeout` | `time.Duration` | `10s` | Total time budget for graceful shutdown phases |

Config is normalized at construction time (`New()`). Set `Binary` and `Args` once; all lifecycle methods use them automatically.

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

All interfaces (`PIDStore`, `ProcessManager`, `HealthChecker`) are designed for test injection:

```go
d := daemon.NewWithDeps(cfg, mockPIDStore, mockProcs, mockHealth)
```

Integration tests use the helper-process pattern -- no external binaries needed. CI runs with `-race` on Linux, macOS, and Windows.

## Not a Service Manager

This library is **not** a replacement for systemd, launchd, or Windows SCM. If you need a long-lived system service, use [kardianos/service](https://github.com/kardianos/service) -- it is the industry standard for that job.

The two libraries are complementary: if your tool must be present from boot, run `myapp serve` under kardianos/service and skip `EnsureRunning`. Do not combine `Restart=always` with idle auto-shutdown -- two supervisors will fight.

## Contributing

Contributions welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines and [CHANGELOG.md](CHANGELOG.md) for release history.

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
