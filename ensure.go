package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/grpmsoft/daemon/internal"
	"github.com/grpmsoft/daemon/internal/pidlock"
)

// EnsureRunning checks if a daemon is already running for this config.
// If running, it returns the existing port. If not, it starts a new daemon
// using the provided binary and args, then waits for health check to pass.
//
// Protocol: two locks, two purposes, one ordering.
//   - Startup lock (<name>.lock): serializes starters. Held during spawn→ready.
//     Kernel releases on holder death → next waiter becomes starter automatically (N12).
//   - PID file lock (<name>.pid): liveness signal. Held by running daemon for lifetime.
//     Under startup lock, "PID held" means exactly "daemon alive" (no race with spawning).
func EnsureRunning(ctx context.Context, cfg Config, binary string, args []string) (int, error) {
	cfg.applyDefaults()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return 0, fmt.Errorf("ensure running: create data dir: %w", err)
	}

	// Step 1: Acquire startup lock (flock/LockFileEx on .lock file).
	// Serializes all starters. If previous holder died, kernel released the lock
	// and we become the starter (N12 solved automatically).
	lockPath := filepath.Join(cfg.DataDir, cfg.Name+".lock")
	startupFile, err := internal.LockCtx(ctx, lockPath)
	if err != nil {
		return 0, fmt.Errorf("ensure running: startup lock: %w", err)
	}
	defer internal.Unlock(startupFile)

	pidPath := filepath.Join(cfg.DataDir, cfg.Name+".pid")

	// Step 2: Under startup lock, check if daemon is alive (PID file held).
	// "Held" under startup lock = daemon running (no race with another spawner).
	if pidlock.IsHeld(pidPath) {
		port, waitErr := waitForPort(ctx, pidPath, cfg.Timeout)
		if waitErr != nil && pidlock.IsHeld(pidPath) {
			return 0, waitErr
		}
		if waitErr == nil {
			return port, nil
		}
		// Daemon died while we waited (lock freed). Fall through to Step 3.
	}

	// Step 3: PID file not held — no daemon running. Start one.
	lock, tryErr := pidlock.TryLock(pidPath)
	if tryErr != nil {
		// Shouldn't happen under startup lock, but handle gracefully.
		if errors.Is(tryErr, pidlock.ErrLocked) {
			return waitForPort(ctx, pidPath, cfg.Timeout)
		}
		return 0, fmt.Errorf("ensure running: %w", tryErr)
	}

	// Truncate stale data so concurrent readers never see old port.
	_ = lock.WriteData([]byte{})

	d := New(cfg)

	port, startErr := d.startWithLock(ctx, binary, args, lock)
	if startErr != nil {
		_ = lock.File().Close()
		return 0, fmt.Errorf("ensure running: start daemon: %w", startErr)
	}

	// Parent closes its fd — child holds lock via inherited fd (Unix)
	// or via own TryLock (Windows, after setupExtraFiles released parent's).
	_ = lock.File().Close()

	return port, nil
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
