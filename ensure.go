package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

	// Try to acquire the PID file lock.
	lock, err := pidlock.TryLock(pidPath)
	if errors.Is(err, pidlock.ErrLocked) {
		// Daemon is running — read port from PID file.
		return readPortFromPIDFile(pidPath)
	}
	if err != nil {
		return 0, fmt.Errorf("ensure running: %w", err)
	}

	// We hold the lock — no daemon is running. Start one.
	// Pass the lock to Start which will hand the fd to the child.
	d := New(cfg)

	port, startErr := d.startWithLock(ctx, binary, args, lock)
	if startErr != nil {
		lock.Release()
		return 0, fmt.Errorf("ensure running: start daemon: %w", startErr)
	}

	// Parent closes its fd — child holds the lock via inherited fd.
	// Do NOT call lock.Release() here — just close the file.
	// Release would unlock, but closing the parent's fd is safe because
	// the child's inherited fd keeps the open file description alive.
	_ = lock.File().Close()

	return port, nil
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
