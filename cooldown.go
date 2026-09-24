package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// cooldownPath returns the path to the spawn cooldown marker file.
func cooldownPath(dataDir, name string) string {
	return filepath.Join(dataDir, name+".spawn-cooldown")
}

// writeCooldown creates the cooldown marker with current unix timestamp (milliseconds).
func writeCooldown(dataDir, name string) error {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	return os.WriteFile(cooldownPath(dataDir, name), []byte(ts), 0o600)
}

// clearCooldown removes the cooldown marker. Errors are silently ignored
// because a missing file is the desired end state.
func clearCooldown(dataDir, name string) {
	_ = os.Remove(cooldownPath(dataDir, name))
}

// isCooldownActive returns true if the spawn cooldown marker exists and
// the timestamp within it has not expired according to ttl.
func isCooldownActive(dataDir, name string, ttl time.Duration) bool {
	data, err := os.ReadFile(cooldownPath(dataDir, name)) //nolint:gosec // trusted path from DataDir
	if err != nil {
		return false
	}
	ms, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		// Corrupted file -- remove and allow retry.
		_ = os.Remove(cooldownPath(dataDir, name))
		return false
	}
	created := time.UnixMilli(ms)
	return time.Since(created) < ttl
}
