# ADR-002: Lifecycle API v2 — Unified Lock Protocol

> **Status**: Accepted  
> **Date**: 2026-09-12  
> **Supersedes**: Partial design in ADR-001 (v0.2.0 Start/EnsureRunning split)

## Context

v0.2.0 introduced inherited-lock PID file architecture with `EnsureRunning()` using
the full protocol (startup lock → IsHeld → TryLock → startWithLock → inherited fd).
However, `Start()` retained the legacy path: direct `StartDetached()` without lock
serialization, child acquires lock itself in `Serve()`.

This creates two code paths for the same operation — the #1 source of "works in dev,
breaks in prod" bugs. Specific risks:

1. **Race**: Two concurrent `Start()` calls spawn two daemons (no startup lock).
2. **Lock gap**: Between parent spawn and child `TryLock` in `Serve()`, PID file is
   unlocked. `Stop()` in this window sees "not held" → returns nil → daemon starts.
3. **Restart deadlock**: If `Restart()` takes startup lock then calls public `Start()`
   which also takes startup lock → same-process OFD deadlock.
4. **API asymmetry**: `Start()` returns `error`, `EnsureRunning()` returns `(int, error)`.
   After `Start()`, caller needs `Status()` for the port — unnecessary round-trip.
5. **Binary/Args repeated**: `Start(ctx, binary, args)`, `EnsureRunning(ctx, cfg, binary, args)`,
   `Restart(ctx, binary, args)` — binary identity is per-daemon, not per-call.
6. **EnsureRunning untestable**: Package-level function calls `New(cfg)` internally,
   bypassing `NewWithDeps()` → cannot inject mocks.

## Decision

### Single internal path, two public semantics

All lifecycle mutations serialize on the startup lock. Public methods are thin
wrappers: acquire lock → check state → delegate to internal → release lock.

```
Public API:
  Start(ctx)          → (*Info, error)   // error if already running
  EnsureRunning(ctx)  → (*Info, error)   // idempotent, return existing if running
  Stop(ctx)           → error            // graceful HTTP → force kill fallback
  Restart(ctx)        → (*Info, error)   // stop + start under ONE lock acquisition
  Status()            → (*Info, error)   // read-only, NO lock
  IsRunning()         → bool             // read-only, NO lock

Internal (assume lock held by caller):
  startLocked(ctx)    → (*Info, error)   // TryLock PID → startWithLock → Info
  stopLocked(ctx)     → error            // HTTP shutdown → KillProcess

Package convenience:
  EnsureRunning(ctx, cfg) → (int, error) // New(cfg).EnsureRunning(ctx)
```

### Binary/Args move to Config

```go
type Config struct {
    Name        string
    DataDir     string
    Binary      string        // default: os.Executable()
    Args        []string      // args for Serve subcommand
    Timeout     time.Duration
    HealthPath  string
    IdleTimeout time.Duration
}
```

Typical call site simplifies:
```go
// Before (v0.2.0):
daemon.EnsureRunning(ctx, cfg, binary, []string{"serve"})

// After (v0.3.0):
d := daemon.New(daemon.Config{Name: "gode", DataDir: ".gode", Args: []string{"serve"}})
info, err := d.EnsureRunning(ctx)
```

### Symmetric return values

All mutating methods that result in a running daemon return `*Info`:
```go
type Info struct {
    Status    Status        `json:"status"`
    PID       int           `json:"pid"`
    Port      int           `json:"port"`
    Name      string        `json:"name"`
    StartTime time.Time     `json:"startTime"`
    Uptime    time.Duration `json:"uptime"`
}
```

### Lock protocol invariant

```
RULE: All lifecycle mutations serialize on startup lock (.lock file).
      Status() and IsRunning() are read-only — no lock.
      Composition only through *Locked() internals.
      Public methods NEVER call other public methods.
```

This prevents the OFD deadlock: `Restart()` calls `stopLocked()` + `startLocked()`,
not `Stop()` + `Start()`.

## Consequences

### Breaking changes (v0 — acceptable)

- `Start(ctx, binary, args) error` → `Start(ctx) (*Info, error)`
- `Stop(ctx) error` → signature unchanged but now serialized
- `Restart(ctx, binary, args) error` → `Restart(ctx) (*Info, error)`
- `EnsureRunning(ctx, cfg, binary, args) (int, error)` → method `d.EnsureRunning(ctx) (*Info, error)`
- `Config` gains `Binary`, `Args` fields
- Package-level `EnsureRunning()` becomes convenience wrapper

### Consumers to update

- **GODE** (`cmd/gode/main.go`): update `daemon.EnsureRunning()` call, `d.Stop()`, `d.Restart()`
- **Tests**: all lifecycle tests rewritten for new signatures

### What does NOT change

- `Serve()` — unchanged (server mode, called by child process)
- `Proxy()` — unchanged (transport bridge)
- `Group` — unchanged (actor pattern)
- `ConnTracker` — unchanged
- PID file format, lock protocol internals, platform-specific code

## Validation criteria

1. Two parallel `Start()` calls → one succeeds, one gets `ErrAlreadyRunning` (never two daemons)
2. `Restart()` under parallel `EnsureRunning()` → no lost start, no double daemon
3. `Stop()` during spawn window → waits for lock, then stops correctly
4. All existing integration tests pass with new API
5. Mock injection works for all methods via `NewWithDeps()`
