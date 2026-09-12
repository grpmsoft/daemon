//go:build !windows

package internal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Lock acquires an exclusive advisory lock on the given file path.
// Blocks until acquired. For ctx-aware version, use LockCtx.
func Lock(path string) (*os.File, error) {
	return LockCtx(context.Background(), path)
}

// LockCtx acquires an exclusive advisory lock with context support.
// Uses LOCK_NB in a poll loop so ctx cancellation is respected.
func LockCtx(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lock file path from trusted caller
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("acquire lock %s: %w", path, err)
		}

		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Unlock releases the lock. The lock file is intentionally NOT deleted —
// removing the file while another process is blocked on flock() causes
// the classic inode-reuse race where two holders acquire the lock on
// different inodes simultaneously. An empty <name>.lock file is harmless.
func Unlock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
