# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.0] - 2026-09-25

### Breaking Changes

- **`POST /daemon/connect` and `POST /daemon/disconnect` REMOVED** — use `GET /daemon/attach` (lease-based connection tracking). These endpoints were deprecated in v0.3.1 and kept for backward compatibility. They are now fully removed along with the Proxy fallback path that used them
- **`ProcessManager.Start(ctx, StartSpec)` replaces `StartDetached(binary, args, logFile, env)`** — the new `StartSpec` struct consolidates all spawn parameters: `Binary`, `Args`, `Dir`, `Env`, `LogFile`, `ExtraFiles`. Consumers implementing `ProcessManager` must update their method signature
- **`Config.DisableTokenAuth` replaces `Config.RequireToken`** — inverted default: zero value (`false`) means token IS required (secure by default). Migration: `RequireToken: true` → remove the field entirely. `RequireToken: false` → set `DisableTokenAuth: true`
- **`HealthChecker.WaitUntilReady(ctx, port, healthPath, timeout)`** — `ctx context.Context` parameter added as the first argument. Consumers implementing `HealthChecker` must update their method signature
- **`Config.ShutdownTimeout`** (`time.Duration`, default `10s`) replaces hardcoded 5s/10s drain delays — `Serve` uses this for HTTP server drain; `Stop` client waits `ShutdownTimeout` plus margin then falls back to kill. The budget is split across phases: HTTP shutdown request (half), lock release wait (full), kill grace (half)

### Added

- **`Hold(ctx context.Context) error`** — stops the daemon AND writes a `.stop-intent` marker file. While the marker exists, `EnsureRunning` returns `ErrStopIntent` instead of auto-starting. Use `Release()`, `Start()`, or `Restart()` to clear the marker and allow restarts. Default `Stop()` does NOT write the marker — hold is opt-in
- **`Release()`** — clears the `.stop-intent` marker, allowing `EnsureRunning` to restart the daemon. No-op if no marker exists
- **`ErrStopIntent`** — sentinel error returned by `EnsureRunning` when the daemon is held via `Hold()`
- **`Config.SpawnCooldown`** (`time.Duration`, default `5s`, `-1` to disable) — after a spawn failure, a `.spawn-cooldown` marker prevents rapid retry. `EnsureRunning` returns `ErrSpawnCooldown` during the cooldown window. Protects against respawn storms when multiple MCP agents call `EnsureRunning` after a failure. Cooldown is skipped on context cancellation
- **`ErrSpawnCooldown`** — sentinel error returned by `EnsureRunning` during the cooldown period after a spawn failure
- **`sync.Mutex` on `*Daemon`** — in-process serialization complements the file lock. Satisfies the race detector when multiple goroutines call lifecycle methods on the same `*Daemon` instance

### Fixed

- **StateNew connection drain** — `ConnState` callback tracks pre-dialed connections in `StateNew`. Before `Shutdown`, these connections are closed explicitly, eliminating the 5-second stdlib drain delay that occurred under `-race` and connection pooling
- **EnsureRunning check order** — fixed to: (1) IsHeld → return existing daemon, (2) stop-intent → `ErrStopIntent`, (3) cooldown → `ErrSpawnCooldown`, (4) start. A running daemon is returned even if a stale stop-intent or cooldown marker exists

## [0.3.3] - 2026-09-12

### Added

- **`Config.Dir`**: working directory for the spawned daemon process. Default = `DataDir` (absolute). Prevents the daemon from holding the parent's CWD, which may be deleted or unmounted after start
- `DataDir` is now resolved to an absolute path in `applyDefaults()`, ensuring relative paths like `.gode` work correctly with `Dir` and `DAEMON_DATA_DIR`

## [0.3.2] - 2026-09-12

### Changed

- Removed unused docs files from git tracking

## [0.3.1] - 2026-09-12

### Added

- **`GET /daemon/attach`** — lease-based connection tracking. The TCP connection IS the lease: when the client process dies (SIGKILL, OOM, closed terminal), the kernel closes the socket, the connection count drops, and idle auto-shutdown can proceed. No timers, no heartbeats. Proxy tries attach first, falls back to connect/disconnect for v0.3.0 daemons
- **Control-plane bearer token** — `Serve()` generates a token via `rand.Text()` (`crypto/rand`, Go 1.24+) and writes it to the PID file (0600 by `pidlock.TryLock` — owner-only readable). All `/daemon/*` endpoints require `Authorization: Bearer <token>`. Clients (`Proxy`, `Stop`) read the token from the PID file automatically
- **`Config.RequireToken`** (`bool`, default `false`) — when true, the bearer token is also required for the application handler (everything outside `/health` and `/daemon/*`). Recommended for MCP deployments where the app handler serves JSON-RPC
- **`PIDInfo.Token`** (`string`, `omitempty`) — bearer token stored in the PID file for client authentication
- **`authGuard` middleware** — constant-time token comparison via `crypto/subtle.ConstantTimeCompare`. Chain: `loopbackGuard` -> `authGuard` -> `mux`
- Integration tests: lease survives crash (raw `net.Conn` attach), shutdown requires token (401 without, success with), attached clients do not block `Stop`

### Deprecated

- `POST /daemon/connect` — use `GET /daemon/attach` instead. Kept for backward compatibility with v0.3.0 proxies
- `POST /daemon/disconnect` — use `GET /daemon/attach` instead. Kept for backward compatibility with v0.3.0 proxies

### Security

- DNS rebinding hardening completed by bearer token — even if `loopbackGuard` is bypassed (crafted Host header), the attacker cannot issue control-plane commands without the token from the 0600 PID file
- Token never appears in proxy.log, daemon logs, or error messages (`LogPayloads` logs bodies only, never headers)
- Windows note: `pidlock` creates the file with default security attributes inheriting the `DataDir` ACL; `DataDir` under the user profile is the supported configuration

### Notes

**Mixed-version behavior:**

| Proxy version | Daemon version | Behavior |
|---|---|---|
| v0.3.1 | v0.3.1 | Lease attach + bearer token (optimal) |
| v0.3.1 | v0.3.0 | Fallback to connect/disconnect, no token (compatible) |
| v0.3.0 | v0.3.1 | connect/disconnect with 401 (token required) — **upgrade proxy** |
| v0.3.0 | v0.3.0 | connect/disconnect, no token (unchanged) |

A v0.3.0 proxy cannot authenticate with a v0.3.1 daemon. Upgrade both simultaneously or upgrade the daemon first, then the proxy.

## [0.3.0] - 2026-09-12

### Breaking Changes

- `Start(ctx, binary, args) error` → `Start(ctx) (*Info, error)` — binary and args moved to `Config`
- `Restart(ctx, binary, args) error` → `Restart(ctx) (*Info, error)` — binary and args moved to `Config`
- `EnsureRunning(ctx, cfg, binary, args) (int, error)` → method `d.EnsureRunning(ctx) (*Info, error)`; package-level `EnsureRunning(ctx, cfg) (int, error)` kept as convenience wrapper
- `KillProcess(pid int) error` → `KillProcess(ctx context.Context, pid int, grace time.Duration) error` — context-aware with grace period
- `Config` gains `Binary` and `Args` fields — daemon identity is now in config, not per-call
- `Stop(ctx)` now serialized on startup lock — all lifecycle mutations use the same lock

### Added

- Unified lock protocol: all lifecycle mutations (`Start`, `Stop`, `Restart`, `EnsureRunning`) serialize on the startup lock- `startLocked`/`stopLocked` internal methods — public methods never call other public methods (prevents double-lock deadlocks)
- `Config.Binary` (`string`) — executable path for the daemon process, defaults to `os.Executable()` when empty
- `Config.Args` (`[]string`) — arguments passed to the child process in `Start()`/`Restart()`
- `buildInfo()` helper — consistent `*Info` construction from `PIDInfo`
- `acquireStartupLock()` helper — encapsulates lock file acquisition with context
- Proxy: `IsHeld` check before dialing — dead daemon returns `ErrNotRunning` instead of connection refused
- `InheritFD`: improved EWOULDBLOCK handling with tests
- Windows `IsHeld`: uses `OPEN_EXISTING` (no empty file creation)
- CI: actions upgraded to Node 24 (`checkout@v7`, `setup-go@v7`, `codecov@v7`, `golangci-lint@v9`)
- Dependabot for GitHub Actions
- Orphan child killed on start failure (D8)

### Changed

- `EnsureRunning` is now a method on `*Daemon` (testable with `NewWithDeps`); package-level wrapper calls it internally
- All mutating methods return `*Info` for consistency (PID, port, start time in one call)
- `Start()` returns error if daemon is already running (not idempotent — use `EnsureRunning` for idempotent start)

## [0.2.0] - 2026-09-12

### Breaking Changes

- `Stop()` → `Stop(ctx context.Context)` — callers must pass context
- `Proxy(ctx, port, mcpPath)` → `Proxy(ctx, cfg Config, opts ProxyOptions)` — new signature with testable I/O
- `SetHandler()` deleted — was dead code, nothing read the handler field
- `IsAlive()` now based on pidlock (file lock held = alive) — no legacy kill(0) fallback
- PID file is never deleted on shutdown (lock release = identity cleared)

### Added

- **Inherited-lock PID file**: PID file IS the lock via flock (Unix) / share-mode (Windows). One mechanism replaces five separate patches. Process identity correct by construction on all platforms including macOS/BSD
- **`internal/pidlock` package**: cross-platform lock primitive (TryLock, Release, WriteData, ReadLocked, IsHeld, InheritFD, SetCloseOnExec). 9 TDD tests
- **Sentinel errors**: `ErrNotRunning`, `ErrAlreadyRunning`, `ErrStartTimeout`, `ErrUnhealthy`, `ErrStalePIDFile`, `ErrInvalidConfig` — consumers can use `errors.Is()`
- **`Config.Validate()`**: rejects empty Name, slashes, dot/dotdot, empty DataDir, HealthPath "/"
- **`POST /daemon/shutdown`**: cross-platform graceful stop endpoint. Stop() tries HTTP first (5s), waits for lock release (10s), falls back to kill
- **`ProxyOptions`**: `Stdin io.Reader`, `Stdout io.Writer` (testable), `LogPayloads bool` (opt-in, S3 security), `MCPPath string`
- **CAS Disconnect**: CompareAndSwap loop replaces Add+Store (atomic race fix)
- **fd inheritance** (Unix): `EnsureRunning` passes locked PID file fd to child via `ExtraFiles` + `DAEMON_PIDFD=3`. Child inherits lock with zero gap
- **Startup lock protocol**: two locks, two purposes — `.lock` (flock/LockFileEx) serializes starters, `.pid` (flock/share-mode) signals liveness. Correct ordering eliminates race conditions
- **`internal.LockCtx(ctx, path)`**: ctx-aware startup lock with LOCK_NB/LOCKFILE_FAIL_IMMEDIATELY poll loop (50ms). Respects context cancellation
- **N12 recovery**: if starter dies mid-spawn, waitForPort detects freed lock and falls through to spawn path under startup lock
- **Windows path**: platform-specific `setupExtraFiles` — no ExtraFiles (Go #26182), child TryLock with retry. Windows LockCtx checks ERROR_LOCK_VIOLATION specifically
- **5 integration tests**: helper-process pattern (no external binaries). ConcurrentEnsureRunning, StopHonoursDeadline, StopIdempotent, IdleShutdown, HolderDiesMidSpawn
- **EINTR retry**: flock retries on signal interruption (matches `cmd/go/internal/lockedfile`)

### Changed

- `Stop()` is idempotent — returns nil if daemon not running
- `Start()` uses `ErrAlreadyRunning` sentinel
- Proxy: no hardcoded 120s timeout (ctx controls), 5s for control calls, bufio.Reader replaces Scanner (no 4MB limit)
- Proxy logger uses `Config.DataDir` (not hardcoded `.gode`)
- PID data written via Seek+Write+Truncate (preserves inode) instead of temp+rename
- Serve() extracted `buildDaemonMux()` helper

### Removed

- `SetHandler()` method and `handler` field on Daemon struct
- Separate `.lock` file (PID file is the lock)
- `isZombie()` as primary identity (supplementary only, lock is primary)
- Legacy kill(0) + binary path fallback in `IsAlive()`
- `internal/lock_unix.go` / `internal/lock_windows.go` (replaced by `internal/pidlock/`)

## [0.1.1] - 2026-09-12

### Fixed

- **B6**: `Start()` now creates `DataDir` if it does not exist (`os.MkdirAll`), fixing first-run failures
- **B3**: Idle auto-shutdown timer arms immediately on startup when no clients are connected, instead of waiting for a `Disconnect()` that would never come
- **B4**: `ConnTracker.Disconnect()` clamps count to zero — an unmatched disconnect no longer drives the counter negative or triggers premature shutdown
- **B2**: `Stop()` now uses `IsAlive()` (PID + binary path verification) instead of bare `IsProcessAlive()`, preventing accidental termination of unrelated processes with recycled PIDs. Note: on macOS/BSD where `/proc` is unavailable, binary verification is skipped — `Stop()` remains PID-only there (full fix deferred to v0.2.0)
- **B5**: Unix `StartDetached` uses a reaper goroutine (`cmd.Wait()`) instead of `Process.Release()`, preventing zombie processes. Added `isZombie()` detection via `/proc/<pid>/stat`
- **B1**: `EnsureRunning()` serializes concurrent startup attempts with an exclusive file lock (`flock` on Unix, `LockFileEx` on Windows), preventing duplicate daemon spawns
- **V1**: `Unlock()` no longer deletes the lock file — prevents the classic flock+unlink race where two processes acquire the lock on different inodes simultaneously
- **V2**: Binary path comparison now trims Linux ` (deleted)` suffix and resolves symlinks (`filepath.EvalSymlinks`), fixing false mismatch after `go install` binary replacement. Case-insensitive comparison on Windows
- **V2**: `Serve()` cleanup uses compare-and-delete — only clears PID file if it still points to our own PID, preventing orphan daemons from deleting a live daemon's PID file
- **V3b**: `Proxy()` tracks whether `signalConnect` succeeded and skips `signalDisconnect` if it didn't, preventing the library's own code from producing stray disconnects that drive the counter negative

### Security

- **S1**: Added `loopbackGuard` middleware to `Serve()` — rejects HTTP requests with non-loopback `Host` or `Origin` headers, preventing DNS rebinding attacks (MCP spec requirement)
- **V4**: Origin validation uses `net/url.Parse` + hostname extraction instead of `strings.Contains`, blocking bypass vectors like `http://127.0.0.1.evil.com` and `http://localhost.evil.com`

### Changed

- Documentation: corrected "no unsafe" claim in CONTRIBUTING.md and SECURITY.md (process_windows.go uses `unsafe` for Win32 API)
- Documentation: corrected "exponential backoff" claim in AGENTS.md and llms.txt (health check uses fixed 500ms interval)
- `.gitignore`: removed consumer-specific entries (`.goda/`, `.goco/`, `.gode/`)

### Added

- `internal/lock_unix.go` / `internal/lock_windows.go` — cross-platform exclusive file locking
- `internal/process_unix.go:isZombie()` — zombie process detection via `/proc/<pid>/stat`
- Enterprise tests: loopback guard (13 cases), ConnTracker clamp (6 table-driven cases), isZombie parser (13 cases), MkdirAll, Stop identity verification

## [0.1.0] - 2026-09-11

### Added

- Initial release
- Client mode: `Start()`, `Stop()`, `Restart()`, `Status()`, `IsRunning()`
- Server mode: `Serve()` with automatic port selection, PID file, signal handling, health endpoint
- Proxy mode: `Proxy()` bridges stdin/stdout to daemon HTTP for MCP/JSON-RPC protocols
- `EnsureRunning()` — start-if-needed with PID file coordination
- `Group` — actor concurrency pattern (oklog/run reimplemented, zero deps)
- `ConnTracker` — connection counting with idle auto-shutdown
- Cross-platform process management: Windows (`CREATE_NEW_PROCESS_GROUP`) + Unix (`setsid`)
- Interfaces: `PIDStore`, `ProcessManager`, `HealthChecker` for testability
- `NewWithDeps()` for dependency injection in tests
- 106 tests, 83%+ coverage
- CI: GitHub Actions (build/test/lint/fmt on 3 OS, codecov OIDC)
- Docs: README, CONTRIBUTING, SECURITY, CODE_OF_CONDUCT, AGENTS, llms.txt

[0.4.0]: https://github.com/grpmsoft/daemon/compare/v0.3.3...v0.4.0
[0.3.3]: https://github.com/grpmsoft/daemon/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/grpmsoft/daemon/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/grpmsoft/daemon/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/grpmsoft/daemon/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/grpmsoft/daemon/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/grpmsoft/daemon/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/grpmsoft/daemon/releases/tag/v0.1.0
