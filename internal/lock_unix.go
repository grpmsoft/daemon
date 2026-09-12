//go:build !windows

package internal

import (
	"fmt"
	"os"
	"syscall"
)

// Lock acquires an exclusive advisory lock on the given file path.
// The lock is released when the returned *os.File is closed or the process exits.
// This serializes concurrent daemon startup across processes.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lock file path from trusted caller
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}

	return f, nil
}

// Unlock releases the lock and removes the lock file.
func Unlock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
}
