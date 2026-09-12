package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

// EnsureRunning is a convenience package-level wrapper that creates a Daemon
// from cfg and calls its EnsureRunning method. Returns the port the daemon is
// listening on.
func EnsureRunning(ctx context.Context, cfg Config) (int, error) {
	d := New(cfg)
	info, err := d.EnsureRunning(ctx)
	if err != nil {
		return 0, err
	}
	return info.Port, nil
}

// waitForPort polls the PID file until it contains a valid port or ctx expires.
func waitForPort(ctx context.Context, pidPath string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("ensure running: %w", ctx.Err())
		default:
		}

		if time.Now().After(deadline) {
			return 0, fmt.Errorf("ensure running: %w: waiting for daemon to write port", ErrStartTimeout)
		}

		// If daemon died (lock released), exit early — caller will handle.
		if !pidlock.IsHeld(pidPath) {
			return 0, fmt.Errorf("ensure running: daemon exited while waiting for port")
		}

		data, err := pidlock.ReadLocked(pidPath)
		if err == nil && len(data) > 2 {
			info, parseErr := parsePIDData(data)
			if parseErr == nil && info.Port > 0 {
				return info.Port, nil
			}
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// parsePIDData parses JSON-encoded PID file content into PIDInfo.
func parsePIDData(data []byte) (PIDInfo, error) {
	var info PIDInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return PIDInfo{}, fmt.Errorf("parse pid data: %w", err)
	}
	return info, nil
}
