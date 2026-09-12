# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.1] - 2026-09-12

### Fixed

- **B6**: `Start()` now creates `DataDir` if it does not exist (`os.MkdirAll`), fixing first-run failures
- **B3**: Idle auto-shutdown timer arms immediately on startup when no clients are connected, instead of waiting for a `Disconnect()` that would never come
- **B4**: `ConnTracker.Disconnect()` clamps count to zero — an unmatched disconnect no longer drives the counter negative or triggers premature shutdown
- **B2**: `Stop()` now uses `IsAlive()` (PID + binary path verification) instead of bare `IsProcessAlive()`, preventing accidental termination of unrelated processes with recycled PIDs
- **B5**: Unix `StartDetached` uses a reaper goroutine (`cmd.Wait()`) instead of `Process.Release()`, preventing zombie processes. Added `isZombie()` detection via `/proc/<pid>/stat`
- **B1**: `EnsureRunning()` serializes concurrent startup attempts with an exclusive file lock (`flock` on Unix, `LockFileEx` on Windows), preventing duplicate daemon spawns

### Security

- **S1**: Added `loopbackGuard` middleware to `Serve()` — rejects HTTP requests with non-loopback `Host` or `Origin` headers, preventing DNS rebinding attacks (MCP spec requirement)

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

[0.1.1]: https://github.com/grpmsoft/daemon/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/grpmsoft/daemon/releases/tag/v0.1.0
