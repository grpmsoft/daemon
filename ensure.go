package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

// EnsureRunning checks if a daemon is already running for this config.
// If running, it returns the existing port. If not, it starts a new daemon
// using the provided binary and args, then waits for health check to pass.
//
// This is the main entry point for consumers like "glide mcp serve" --
// it transparently handles "start if needed" logic.
//
// Concurrency: the PID file itself is the lock (flock on Unix, share-mode on
// Windows). If the daemon holds the PID file, TryLock returns ErrLocked and
// we read the port from the file. If not held, we hold the lock while
// spawning, then pass the locked fd to the child via ExtraFiles.
func EnsureRunning(ctx context.Context, cfg Config, binary string, args []string) (int, error) {
	cfg.applyDefaults()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return 0, fmt.Errorf("ensure running: create data dir: %w", err)
	}

	pidPath := filepath.Join(cfg.DataDir, cfg.Name+".pid")

	for {
		// Try to acquire the PID file lock.
		lock, err := pidlock.TryLock(pidPath)
		if errors.Is(err, pidlock.ErrLocked) {
			// Lock held = daemon running OR another EnsureRunning spawning.
			// Poll until valid PID data appears OR lock becomes free (N12: holder died).
			port, waitErr := waitForPort(ctx, pidPath, cfg.Timeout)
			if waitErr == nil {
				return port, nil
			}
			// If lock freed (holder died), retry the outer loop to become starter.
			if errors.Is(waitErr, errRetryAsStarter) {
				continue
			}
			return 0, waitErr
		}
		if err != nil {
			return 0, fmt.Errorf("ensure running: %w", err)
		}

		// We hold the lock — no daemon is running.
		// Truncate immediately so concurrent ErrLocked readers never see stale data.
		_ = lock.WriteData([]byte{})

		// On Windows, acquire a separate startup lock (LockFileEx) to serialize
		// the spawn window. On Unix this is a no-op (fd inheritance handles it).
		startupLock, slErr := acquireStartupLock(cfg)
		if slErr != nil {
			_ = lock.File().Close()
			return 0, fmt.Errorf("ensure running: startup lock: %w", slErr)
		}
		defer releaseStartupLock(startupLock)

		// Start daemon, pass locked fd to child.
		d := New(cfg)

		port, startErr := d.startWithLock(ctx, binary, args, lock)
		if startErr != nil {
			_ = lock.File().Close()
			return 0, fmt.Errorf("ensure running: start daemon: %w", startErr)
		}

		// Parent closes its fd — child holds the lock via inherited fd.
		_ = lock.File().Close()

		return port, nil
	}
}

// errRetryAsStarter is an internal sentinel: waitForPort detected the lock
// holder died and the caller should retry as the new starter.
var errRetryAsStarter = errors.New("internal: retry as starter")

// waitForPort polls the PID file until it contains a valid port or ctx expires.
// Used when TryLock returns ErrLocked — another instance holds the lock
// (either a running daemon or a concurrent EnsureRunning still spawning).
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

		// Try to read valid port data.
		data, err := pidlock.ReadLocked(pidPath)
		if err == nil && len(data) > 2 {
			info, parseErr := parsePIDData(data)
			if parseErr == nil && info.Port > 0 {
				return info.Port, nil
			}
		}

		// N12: if the holder died (parent crashed after truncate), the lock
		// is now free. Signal caller to retry as the new starter.
		if probe, tryErr := pidlock.TryLock(pidPath); tryErr == nil {
			probe.Release()
			return 0, errRetryAsStarter
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// readPortFromPIDFile reads the PID file (which may be locked by a running daemon)
// and extracts the port.
func readPortFromPIDFile(path string) (int, error) {
	data, err := pidlock.ReadLocked(path)
	if err != nil {
		return 0, fmt.Errorf("ensure running: read pid file: %w", err)
	}

	info, parseErr := parsePIDData(data)
	if parseErr != nil {
		return 0, fmt.Errorf("ensure running: parse pid file: %w", parseErr)
	}

	if info.Port <= 0 {
		return 0, fmt.Errorf("ensure running: daemon running but port is %d", info.Port)
	}

	return info.Port, nil
}

// parsePIDData parses JSON-encoded PID file content into PIDInfo.
func parsePIDData(data []byte) (PIDInfo, error) {
	var info PIDInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return PIDInfo{}, fmt.Errorf("parse pid data: %w", err)
	}
	return info, nil
}
