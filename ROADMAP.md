# Roadmap

## Current State: v0.2.0

Cross-platform daemon lifecycle library with lock-based process identity.
PID file IS the lock — correct by construction on all platforms.

### What works

- Client mode: Start/Stop/Restart/Status with lock-based identity
- Server mode: Serve with auto port, inherited-lock PID file, health endpoint
- Proxy mode: stdin/stdout ↔ HTTP bridge with testable io.Reader/Writer
- EnsureRunning: start-if-needed with flock serialization (no duplicate daemons)
- Graceful stop: HTTP /daemon/shutdown → clean exit on all platforms
- Connection tracking with idle auto-shutdown
- Cross-platform: Windows (share-mode), Linux/macOS (flock), BSD (flock)
- Sentinel errors for programmatic error handling

## v0.3.0 — API Maturity

- [ ] Token auth for `/daemon/*` endpoints (nonce in PID file, `Authorization: Bearer`)
- [ ] Lease-based connection tracking (crash-safe, replaces counting)
- [ ] `slog.Logger` in Config (replace `fmt.Fprintf(os.Stderr)`)
- [ ] `EnsureRunning` as `(*Daemon)` method (uses injected deps, fully testable)
- [ ] `Status()` returns `Info` by value (never errors)
- [ ] Pipe handshake parent→child (JSON, replaces env vars)
- [ ] Upgrade detection: binary version in PID file, auto-restart on change

## v1.0.0 — Stability

- [ ] Stable public API (no breaking changes after 1.0)
- [ ] macOS process identity via `x/sys/unix.SysctlKinfoProc` (belt-and-braces)
- [ ] Windows `CREATE_BREAKAWAY_FROM_JOB` for CI/Job Object environments
- [ ] `example_test.go` (pkg.go.dev runnable examples)
- [ ] Release workflow (tag → GitHub Release with changelog)
- [ ] OpenSSF Scorecard badge
- [ ] systemd/launchd user service integration (daemon survives logout)

## Non-Goals

- **Process supervision**: use systemd/launchd/Windows SCM for production. This library manages dev-tool daemons (language servers, MCP servers)
- **Multi-process orchestration**: one daemon per workspace. Multiple workspaces = multiple daemons
- **Remote daemon**: localhost only (127.0.0.1). Remote development is GLIDE's responsibility
