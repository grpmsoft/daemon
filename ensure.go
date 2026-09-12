package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grpmsoft/daemon/internal"
)

// EnsureRunning checks if a daemon is already running for this config.
// If running, it returns the existing port. If not, it starts a new daemon
// using the provided binary and args, then waits for health check to pass.
//
// This is the main entry point for consumers like "gode mcp serve" --
// it transparently handles "start if needed" logic.
//
// Concurrency: an exclusive file lock serializes the check→spawn→ready
// sequence across processes. If two agents call EnsureRunning simultaneously,
// one acquires the lock and starts the daemon; the other blocks until the
// lock is released and then finds the daemon already running.
func EnsureRunning(ctx context.Context, cfg Config, binary string, args []string) (int, error) {
	cfg.applyDefaults()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return 0, fmt.Errorf("ensure running: create data dir: %w", err)
	}

	// Serialize concurrent startup attempts with an exclusive file lock.
	lockPath := filepath.Join(cfg.DataDir, cfg.Name+".lock")
	lockFile, err := internal.Lock(lockPath)
	if err != nil {
		return 0, fmt.Errorf("ensure running: acquire lock: %w", err)
	}
	defer internal.Unlock(lockFile)

	d := New(cfg)

	// Fast path: daemon already running (checked under lock).
	if d.IsRunning() {
		info, err := d.Status()
		if err != nil {
			return 0, fmt.Errorf("ensure running: status check failed: %w", err)
		}
		if info.Status == StatusRunning && info.Port > 0 {
			return info.Port, nil
		}
	}

	// Slow path: start a new daemon (still under lock).
	if err := d.Start(ctx, binary, args); err != nil {
		if d.IsRunning() {
			info, statusErr := d.Status()
			if statusErr == nil && info.Status == StatusRunning && info.Port > 0 {
				return info.Port, nil
			}
		}
		return 0, fmt.Errorf("ensure running: start daemon: %w", err)
	}

	info, err := d.Status()
	if err != nil {
		return 0, fmt.Errorf("ensure running: status after start: %w", err)
	}
	if info.Port <= 0 {
		return 0, fmt.Errorf("ensure running: daemon started but port is %d", info.Port)
	}

	return info.Port, nil
}
