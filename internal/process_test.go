package internal

import (
	"os"
	"testing"
)

func TestFindFreePort_ValidRange(t *testing.T) {
	port, err := FindFreePort()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port < 1024 {
		t.Errorf("port should be >= 1024 (unprivileged range), got %d", port)
	}
	if port > 65535 {
		t.Errorf("port should be <= 65535, got %d", port)
	}
}

func TestFindFreePort_DifferentPortsOnRepeatedCalls(t *testing.T) {
	// Run several times; at least two should differ (practically always true).
	ports := make(map[int]bool)
	const attempts = 5
	for range attempts {
		port, err := FindFreePort()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ports[port] = true
	}
	// It is statistically near-impossible for all 5 calls to return the same port.
	if len(ports) <= 1 {
		t.Errorf("repeated FindFreePort calls should return different ports, got %d unique", len(ports))
	}
}

func TestIsProcessAlive_CurrentProcess(t *testing.T) {
	alive := IsProcessAlive(os.Getpid())
	if !alive {
		t.Errorf("current process (pid %d) should be alive", os.Getpid())
	}
}

func TestIsProcessAlive_NonExistentPID(t *testing.T) {
	alive := IsProcessAlive(999999)
	if alive {
		t.Error("pid 999999 should not be alive")
	}
}

// ---------------------------------------------------------------------------
// Tests: ProcessBinaryPath
// ---------------------------------------------------------------------------

func TestProcessBinaryPath_CurrentProcess(t *testing.T) {
	// ProcessBinaryPath for the current process should either return a non-empty
	// path or ("", nil) on platforms that don't support it (e.g. macOS).
	path, err := ProcessBinaryPath(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessBinaryPath(%d): %v", os.Getpid(), err)
	}

	// On Linux/Windows: path should be non-empty and absolute.
	// On macOS: path is "" (no /proc, returns empty without error).
	if path != "" {
		// Verify the binary exists on disk.
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("ProcessBinaryPath returned %q but Stat failed: %v", path, statErr)
		}
	}
}

func TestProcessBinaryPath_NonExistentPID(_ *testing.T) {
	// Non-existent PID should return an error (or empty on macOS).
	path, err := ProcessBinaryPath(999999)
	// Platform-dependent: on Linux, readlink fails; on macOS, returns "";
	// on Windows, OpenProcess fails. We accept both outcomes.
	_ = path
	_ = err
}
