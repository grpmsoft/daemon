//go:build windows

package internal

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	modkernel32    = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = modkernel32.NewProc("LockFileEx")
)

const (
	lockfileExclusiveLock = 0x02
)

// Lock acquires an exclusive lock on the given file path using Win32 LockFileEx.
// The lock is released when the returned *os.File is closed or the process exits.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lock file path from trusted caller
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	h := syscall.Handle(f.Fd())
	ol := new(syscall.Overlapped)

	r1, _, e1 := procLockFileEx.Call(
		uintptr(h),
		uintptr(lockfileExclusiveLock),
		0,
		1, 0,
		uintptr(unsafe.Pointer(ol)), //nolint:gosec // required for Win32 API call
	)
	if r1 == 0 {
		_ = f.Close()
		return nil, fmt.Errorf("acquire lock %s: %w", path, e1)
	}

	return f, nil
}

// Unlock releases the lock and removes the lock file.
func Unlock(f *os.File) {
	if f == nil {
		return
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
}
