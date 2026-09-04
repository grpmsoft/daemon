package daemon

import (
	"context"
	"fmt"
)

// EnsureRunning checks if a daemon is already running for this config.
// If running, it returns the existing port. If not, it starts a new daemon
// using the provided binary and args, then waits for health check to pass.
//
// This is the main entry point for consumers like "gode mcp serve" --
// it transparently handles "start if needed" logic.
//
// Race handling: if two agents call EnsureRunning simultaneously and
// Start fails with "already running", it falls back to reading the
// existing daemon's status. The PID file acts as a natural lock.
func EnsureRunning(ctx context.Context, cfg Config, binary string, args []string) (int, error) {
	d := New(cfg)

	// Fast path: daemon already running.
	if d.IsRunning() {
		info, err := d.Status()
		if err != nil {
			return 0, fmt.Errorf("ensure running: status check failed: %w", err)
		}
		if info.Status == StatusRunning && info.Port > 0 {
			return info.Port, nil
		}
	}

	// Slow path: start a new daemon.
	if err := d.Start(ctx, binary, args); err != nil {
		// Race condition: another agent started the daemon between our
		// IsRunning check and Start call. If the daemon is now running,
		// return its port instead of failing.
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
