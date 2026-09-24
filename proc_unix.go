//go:build !windows

package daemon

import (
	"os"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

// populateExtraFiles passes the locked PID file fd to the child via ExtraFiles.
// Unix supports fd inheritance through fork+exec.
func populateExtraFiles(spec *StartSpec, lock *pidlock.Lock) {
	spec.ExtraFiles = []*os.File{lock.File()}
	spec.Env = append(spec.Env, "DAEMON_PIDFD=3")
}
