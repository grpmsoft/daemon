//go:build windows

package daemon

import (
	"os/exec"
	"syscall"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
	}
}

// setupExtraFiles is a no-op on Windows — ExtraFiles is not supported
// (Go issue #26182). The child process acquires the PID file lock itself
// via the foreground Serve path (no DAEMON_PIDFD env set).
// Parent releases its lock before spawn so child can TryLock.
func setupExtraFiles(_ *exec.Cmd, lock *pidlock.Lock) {
	// Release parent's lock — child will acquire in Serve foreground path.
	// This creates a brief window, but startup serialization via EnsureRunning's
	// TryLock prevents duplicate spawns.
	lock.Release()
}
