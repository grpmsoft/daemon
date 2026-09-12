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

// setupExtraFiles on Windows releases the PID file lock so the child
// can acquire it in Serve's foreground path. ExtraFiles is not supported
// on Windows (Go issue #26182).
func setupExtraFiles(_ *exec.Cmd, lock *pidlock.Lock) {
	lock.Release()
}
