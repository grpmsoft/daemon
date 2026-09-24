//go:build windows

package daemon

import (
	"github.com/grpmsoft/daemon/internal/pidlock"
)

// populateExtraFiles on Windows releases the PID file lock so the child
// can acquire it in Serve's foreground path. ExtraFiles is not supported
// on Windows (Go issue #26182).
func populateExtraFiles(_ *StartSpec, lock *pidlock.Lock) {
	lock.Release()
}
