//go:build !windows

// Package pidlock provides an exclusive advisory lock backed by the PID file itself.
//
// The lock is acquired via flock(2) with LOCK_NB (non-blocking). The locked file
// descriptor can be passed to a child process via os/exec.Cmd.ExtraFiles, giving
// the child ownership of the lock without any window where the lock is released.
//
// The lock file is intentionally never deleted — removing a flock'd file while
// another process waits on flock() causes the classic inode-reuse race.
//
// On process death (including SIGKILL and zombie state), the kernel releases the
// flock during exit_files(), before the process enters EXIT_ZOMBIE state.
package pidlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked is returned by TryLock when the file is already locked
// by another process (flock returned EWOULDBLOCK).
var ErrLocked = errors.New("pidlock: locked by another instance")

// Lock represents an exclusively locked PID file.
type Lock struct {
	f *os.File
}

// TryLock opens (or creates) the file at path and attempts a non-blocking
// exclusive flock. Returns ErrLocked if another process holds the lock.
// EINTR from signal delivery is retried automatically.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path from trusted caller
	if err != nil {
		return nil, fmt.Errorf("pidlock: open %s: %w", path, err)
	}

	if err := flockNB(f); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("pidlock: flock %s: %w", path, err)
	}

	return &Lock{f: f}, nil
}

// File returns the underlying *os.File. Use this to pass the fd to a child
// process via cmd.ExtraFiles. The child inherits the lock.
func (l *Lock) File() *os.File {
	if l == nil {
		return nil
	}
	return l.f
}

// WriteData writes data to the locked file using Seek(0) + Write + Truncate + Sync.
// This preserves the inode (and thus the lock) — unlike temp+rename which would
// create a new inode and break the flock.
func (l *Lock) WriteData(data []byte) error {
	if _, err := l.f.Seek(0, 0); err != nil {
		return fmt.Errorf("pidlock: seek: %w", err)
	}
	if _, err := l.f.Write(data); err != nil {
		return fmt.Errorf("pidlock: write: %w", err)
	}
	if err := l.f.Truncate(int64(len(data))); err != nil {
		return fmt.Errorf("pidlock: truncate: %w", err)
	}
	return l.f.Sync()
}

// Release unlocks and closes the file. The file is NOT deleted.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// SetCloseOnExec sets FD_CLOEXEC on the locked fd so that child processes
// spawned by the lock holder (e.g., gopls) do not inherit the lock.
// Call this in the child process after inheriting the fd via ExtraFiles.
func (l *Lock) SetCloseOnExec() {
	if l == nil || l.f == nil {
		return
	}
	syscall.CloseOnExec(int(l.f.Fd()))
}

// ReadLocked reads the content of a PID file that may be locked by another process.
// flock is advisory — reading is always possible regardless of lock state.
func ReadLocked(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // path from trusted caller
}

// IsHeld attempts a non-blocking flock to determine if the file is currently
// locked by another process. Returns true if locked, false if not (or if the
// file doesn't exist).
func IsHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // path from trusted caller
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	err = flockNB(f)
	if err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	// We got the lock — means nobody held it. Release immediately.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// flockNB attempts a non-blocking exclusive flock with EINTR retry.
func flockNB(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return err
	}
}
