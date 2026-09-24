# Roadmap

## Current State: v0.4.0

Cross-platform daemon lifecycle library with unified lock protocol,
lease-based connection tracking, control-plane bearer token, stop-intent,
and spawn cooldown.

### What works

- Client mode: Start/Stop/Restart/Status — all serialized on startup lock + in-process mutex
- Server mode: Serve with auto port, inherited-lock PID file, health endpoint, configurable ShutdownTimeout
- Proxy mode: stdin/stdout ↔ HTTP bridge with IsHeld pre-check
- EnsureRunning: method on *Daemon + package-level wrapper, fully testable
- Binary/Args in Config — daemon identity per-config, not per-call
- All mutating methods return *Info (pid, port, start time)
- Graceful stop: HTTP /daemon/shutdown → clean exit on all platforms
- Lease-based connection tracking: GET /daemon/attach — TCP lease, crash-safe
- Bearer token on /daemon/* endpoints — PID file 0600 as auth boundary
- Secure by default: token required for app handler (DisableTokenAuth to opt out)
- Hold/Release: explicit stop-intent marker prevents EnsureRunning auto-start
- SpawnCooldown: prevents rapid respawn loops after spawn failure (default 5s)
- StateNew connection drain: pre-dialed connections closed before Shutdown
- Orphan child killed on start failure
- Cross-platform: Windows (share-mode), Linux/macOS (flock), BSD (flock)
- CI: Node 24 actions, Dependabot

## v0.5.0 — Hardening

- [ ] `slog.Logger` in Config (replace `fmt.Fprintf(os.Stderr)`)
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
