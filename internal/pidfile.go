package internal

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/grpmsoft/daemon/internal/pidlock"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pidData is the JSON schema for the PID file.
type pidData struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Name      string    `json:"name"`
	Binary    string    `json:"binary,omitempty"`
	StartTime time.Time `json:"startTime"`
	Token     string    `json:"token,omitempty"`
}

// PIDInfo mirrors daemon.PIDInfo for the PIDStore interface contract.
type PIDInfo struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Name      string    `json:"name"`
	Binary    string    `json:"binary"`
	StartTime time.Time `json:"startTime"`
	Token     string    `json:"token,omitempty"`
}

// PIDFile manages reading and writing a JSON PID file on disk.
// A per-instance mutex serialises concurrent Save/Load/Clear calls so that
// the atomic write (write-tmp → rename) is safe on Windows, where rename over
// an open file is not permitted.
type PIDFile struct {
	mu   sync.Mutex
	path string
}

// NewPIDFile returns a PIDFile that stores data at <dataDir>/<name>.pid.
func NewPIDFile(dataDir, name string) *PIDFile {
	return &PIDFile{
		path: filepath.Join(dataDir, name+".pid"),
	}
}

// Save atomically writes PID data as JSON. Parent directories are created if needed.
// binary is the path to the daemon executable (used for process verification).
func (p *PIDFile) Save(pid, port int, name, binary string, startTime time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create pid dir %s: %w", dir, err)
	}

	data := pidData{
		PID:       pid,
		Port:      port,
		Name:      name,
		Binary:    binary,
		StartTime: startTime,
	}

	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal pid data: %w", err)
	}

	// Use os.CreateTemp for a unique tmp path so concurrent Save calls on
	// Windows do not fight over the same ".tmp" file (file-locking issue).
	tmpf, err := os.CreateTemp(filepath.Dir(p.path), "*.pid.tmp")
	if err != nil {
		return fmt.Errorf("write pid temp file: %w", err)
	}
	tmp := tmpf.Name()
	if _, err := tmpf.Write(b); err != nil {
		_ = tmpf.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write pid temp file: %w", err)
	}
	if err := tmpf.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write pid temp file: %w", err)
	}

	if err := os.Rename(tmp, p.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename pid file: %w", err)
	}

	return nil
}

// Load reads and parses the PID file. For backward compatibility, it also handles
// plain-text files containing just a PID number.
func (p *PIDFile) Load() (PIDInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	raw, err := pidlock.ReadLocked(p.path)
	if err != nil {
		return PIDInfo{}, fmt.Errorf("read pid file %s: %w", p.path, err)
	}

	content := strings.TrimSpace(string(raw))
	if len(content) == 0 {
		return PIDInfo{}, fmt.Errorf("pid file %s is empty", p.path)
	}

	var data pidData
	if err := json.Unmarshal(raw, &data); err == nil && data.PID > 0 {
		return PIDInfo(data), nil
	}

	pid, err := strconv.Atoi(content)
	if err != nil {
		return PIDInfo{}, fmt.Errorf("parse pid file %s: not JSON and not a plain PID: %w", p.path, err)
	}

	return PIDInfo{PID: pid}, nil
}

// Clear removes the PID file. No error if the file does not exist.
func (p *PIDFile) Clear() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove pid file %s: %w", p.path, err)
	}
	return nil
}

// IsAlive checks whether the daemon that owns the PID file is still running.
// Uses pidlock.IsHeld — if the PID file is flock'd (Unix) or share-mode held
// (Windows), the owner is alive by construction. Works on all platforms
// including macOS/BSD where /proc is unavailable.
func (p *PIDFile) IsAlive() bool {
	return pidlock.IsHeld(p.path)
}

// binaryPathsEqual compares two binary paths with platform-aware normalization.
// On Linux, /proc/<pid>/exe appends " (deleted)" after binary replacement (go install).
// On Windows, paths may differ in case or 8.3 form.
func binaryPathsEqual(actual, expected string) bool {
	actual = strings.TrimSuffix(actual, " (deleted)")
	expected = strings.TrimSuffix(expected, " (deleted)")

	// Resolve symlinks for both paths.
	if resolved, err := filepath.EvalSymlinks(actual); err == nil {
		actual = resolved
	}
	if resolved, err := filepath.EvalSymlinks(expected); err == nil {
		expected = resolved
	}

	// Case-insensitive on Windows, case-sensitive elsewhere.
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(actual), filepath.Clean(expected))
	}
	return filepath.Clean(actual) == filepath.Clean(expected)
}

// Path returns the filesystem path of the PID file.
func (p *PIDFile) Path() string {
	return p.path
}
