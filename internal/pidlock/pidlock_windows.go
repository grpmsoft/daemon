//go:build windows

// Package pidlock provides an exclusive advisory lock backed by the PID file itself.
//
// On Windows, locking is achieved via file sharing mode: the file is opened
// with FILE_SHARE_READ only (no FILE_SHARE_WRITE or FILE_SHARE_DELETE).
// While the handle is open, any attempt to open for writing returns
// ERROR_SHARING_VIOLATION (32). Reading is always possible.
//
// The kernel closes the handle (and releases the lock) on any process death,
// including TerminateProcess.
package pidlock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ErrLocked is returned when the PID file is already held by another process.
var ErrLocked = errors.New("pidlock: locked by another instance")

// errSharingViolation is ERROR_SHARING_VIOLATION (32).
var errSharingViolation = syscall.Errno(32)

// Lock represents an exclusively held PID file on Windows.
type Lock struct {
	f *os.File
}

// TryLock opens (or creates) the file at path with exclusive write access.
// If another process already holds the file, returns ErrLocked.
func TryLock(path string) (*Lock, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("pidlock: utf16 %s: %w", path, err)
	}

	h, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ, // allow reads, block writes and deletes
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("pidlock: open %s: %w", path, err)
	}

	f := os.NewFile(uintptr(h), path)
	return &Lock{f: f}, nil
}

// File returns the underlying *os.File.
func (l *Lock) File() *os.File {
	if l == nil {
		return nil
	}
	return l.f
}

// WriteData writes data to the locked file using Seek+Write+Truncate+Sync.
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

// Release closes the file handle, releasing the sharing-mode lock.
// The file is NOT deleted.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Close()
	l.f = nil
}

// SetCloseOnExec is a no-op on Windows — handles are not inherited by default
// unless explicitly configured via PROC_THREAD_ATTRIBUTE_HANDLE_LIST.
func (l *Lock) SetCloseOnExec() {}

// InheritFD is not supported on Windows — ExtraFiles fd inheritance is not
// available. Windows uses a different startup mechanism (see ADR-001).
// This returns an error; callers should use TryLock on Windows.
func InheritFD(_ int, _ string) (*Lock, error) {
	return nil, fmt.Errorf("pidlock: fd inheritance not supported on Windows")
}

// ReadLocked reads the content of a PID file that may be held by another process.
// Opens with FILE_SHARE_READ|FILE_SHARE_WRITE to allow reading alongside the
// exclusive writer lock.
func ReadLocked(path string) ([]byte, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("pidlock: utf16 %s: %w", path, err)
	}
	h, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("pidlock: read %s: %w", path, err)
	}
	f := os.NewFile(uintptr(h), path)
	defer func() { _ = f.Close() }()

	return io.ReadAll(f)
}

// IsHeld checks whether the PID file is currently held by a running daemon.
// Opens with OPEN_EXISTING (not OPEN_ALWAYS) to avoid creating an empty file
// when no daemon has ever run. If the daemon holds the file without
// FILE_SHARE_WRITE, CreateFile returns ERROR_SHARING_VIOLATION.
func IsHeld(path string) bool {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}

	h, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.OPEN_EXISTING, // do NOT create file if absent
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return errors.Is(err, errSharingViolation)
	}
	_ = syscall.CloseHandle(h)
	return false
}
