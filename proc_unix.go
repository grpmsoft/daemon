//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// setupExtraFiles passes the locked PID file fd to the child via ExtraFiles.
// Unix supports fd inheritance through fork+exec.
func setupExtraFiles(cmd *exec.Cmd, lock *pidlock.Lock) {
	cmd.ExtraFiles = []*os.File{lock.File()}
	cmd.Env = append(cmd.Env, "DAEMON_PIDFD=3")
}

// acquireStartupLock is a no-op on Unix — fd inheritance via ExtraFiles
// ensures the lock is never released between parent and child.
func acquireStartupLock(_ Config) (*os.File, error) {
	return nil, nil
}

// releaseStartupLock is a no-op on Unix.
func releaseStartupLock(_ *os.File) {}
