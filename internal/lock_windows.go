//go:build windows

package internal

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var (
	modkernel32    = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = modkernel32.NewProc("LockFileEx")
)

const (
	lockfileExclusiveLock   = 0x02
	lockfileFailImmediately = 0x01
)

// Lock acquires an exclusive lock on the given file path.
// Blocks until acquired. For ctx-aware version, use LockCtx.
func Lock(path string) (*os.File, error) {
	return LockCtx(context.Background(), path)
}

// LockCtx acquires an exclusive lock with context support.
// Uses LOCKFILE_FAIL_IMMEDIATELY in a poll loop so ctx cancellation is respected.
func LockCtx(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lock file path from trusted caller
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	h := syscall.Handle(f.Fd())

	for {
		ol := new(syscall.Overlapped)
		r1, _, _ := procLockFileEx.Call(
			uintptr(h),
			uintptr(lockfileExclusiveLock|lockfileFailImmediately),
			0,
			1, 0,
			uintptr(unsafe.Pointer(ol)), //nolint:gosec // required for Win32 API call
		)
		if r1 != 0 {
			return f, nil
		}

		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Unlock releases the lock. The lock file is intentionally NOT deleted.
func Unlock(f *os.File) {
	if f == nil {
		return
	}
	_ = f.Close()
}
