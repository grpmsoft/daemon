# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
- **fd inheritance**: `EnsureRunning` passes locked PID file fd to child via `ExtraFiles` + `DAEMON_PIDFD=3`. Child inherits lock with zero gap
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

[0.2.0]: https://github.com/grpmsoft/daemon/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/grpmsoft/daemon/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/grpmsoft/daemon/releases/tag/v0.1.0
