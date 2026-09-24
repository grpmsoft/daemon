package daemon

import (
	"os"
	"path/filepath"
)

// stopIntentPath returns the path to the stop-intent marker file.
// The marker prevents EnsureRunning from auto-starting a daemon that
// was explicitly stopped by the user.
func stopIntentPath(dataDir, name string) string {
	return filepath.Join(dataDir, name+".stop-intent")
}

// writeStopIntent creates the stop-intent marker file. The marker is
// a zero-byte file with 0600 permissions (owner-only).
func writeStopIntent(dataDir, name string) error {
	return os.WriteFile(stopIntentPath(dataDir, name), []byte{}, 0o600)
}

// clearStopIntent removes the stop-intent marker file. It is a no-op
// if the file does not exist.
func clearStopIntent(dataDir, name string) {
	_ = os.Remove(stopIntentPath(dataDir, name))
}

// hasStopIntent returns true if the stop-intent marker file exists.
func hasStopIntent(dataDir, name string) bool {
	_, err := os.Stat(stopIntentPath(dataDir, name))
	return err == nil
}
