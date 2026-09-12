//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/grpmsoft/daemon/internal"
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
	// Release PID file lock — child acquires in Serve foreground path.
	lock.Release()
}

// acquireStartupLock holds a LockFileEx on <DataDir>/<Name>.lock for the
// duration of spawn→ready. Prevents another EnsureRunning from entering
// the spawn path while the PID file lock is released (Windows-specific gap).
func acquireStartupLock(cfg Config) (*os.File, error) {
	lockPath := filepath.Join(cfg.DataDir, cfg.Name+".lock")
	return internal.Lock(lockPath)
}

// releaseStartupLock releases the Windows startup serialization lock.
func releaseStartupLock(f *os.File) {
	internal.Unlock(f)
}
