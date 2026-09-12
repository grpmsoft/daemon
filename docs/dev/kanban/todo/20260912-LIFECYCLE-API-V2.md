# LIFECYCLE-API-V2: Unified Lock Protocol

> **Priority**: P0  
> **ADR**: ADR-002-LIFECYCLE-API-V2  
> **Estimated**: 1 session  
> **Branch**: `refactor/lifecycle-v2`

## Summary

Rewrite daemon lifecycle API so all mutations go through one lock-protected path.
Two public semantics (Start = error if running, EnsureRunning = idempotent) over
one internal implementation. Binary/Args move to Config. All methods return *Info.

## Tasks

### 1. Config: add Binary, Args (config.go)
- [ ] Add `Binary string` and `Args []string` to Config
- [ ] `applyDefaults()`: Binary="" → os.Executable()
- [ ] Update `Validate()` if needed
- [ ] Remove binary/args from all method signatures

### 2. Internal methods: startLocked, stopLocked (daemon.go)
- [ ] `startLocked(ctx) (*Info, error)` — TryLock PID → startWithLock → close parent fd → build Info
- [ ] `stopLocked(ctx) error` — existing Stop() logic minus lock acquisition
- [ ] Both assume startup lock is held by caller

### 3. Public methods: lock + delegate (daemon.go)
- [ ] `Start(ctx) (*Info, error)` — lock → IsHeld? ErrAlreadyRunning : startLocked
- [ ] `EnsureRunning(ctx) (*Info, error)` — lock → IsHeld? existingInfo : startLocked
- [ ] `Stop(ctx) error` — lock → stopLocked
- [ ] `Restart(ctx) (*Info, error)` — lock → stopLocked → startLocked (ONE lock!)
- [ ] `Status() (*Info, error)` — NO lock, read-only
- [ ] `IsRunning() bool` — NO lock, read-only
- [ ] Public methods NEVER call other public methods

### 4. EnsureRunning: method + package wrapper (ensure.go)
- [ ] Move EnsureRunning logic to *Daemon method
- [ ] Package-level `EnsureRunning(ctx, cfg) (int, error)` as one-liner wrapper
- [ ] Remove binary/args params

### 5. Tests (daemon_test.go, ensure_test.go)
- [ ] Update all Start/Stop/Restart test signatures
- [ ] Test: two parallel Start() → one ErrAlreadyRunning
- [ ] Test: Restart under parallel EnsureRunning
- [ ] Test: Stop during spawn window
- [ ] All mocks updated for new signatures
- [ ] Integration tests with real lock files (t.TempDir)

### 6. Cleanup
- [ ] Remove dead code (old Start body, old EnsureRunning body)
- [ ] Update CHANGELOG.md
- [ ] Update AGENTS.md
- [ ] Update README.md examples

## Acceptance criteria

- `go build ./...` clean
- `go test ./... -count=1` all green
- `go vet ./...` clean
- Two parallel Start() → exactly one daemon (test)
- Restart + EnsureRunning concurrent → no deadlock, no double daemon (test)
- Fable review: no findings above MEDIUM
