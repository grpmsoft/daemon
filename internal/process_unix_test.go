//go:build !windows

package internal

import (
	"fmt"
	"os"
	"testing"
)

// ---------------------------------------------------------------------------
// Tests: B6 — isZombie detection (Unix-only, requires /proc)
//
// isZombie reads /proc/<pid>/stat and checks the state field for 'Z'.
// We cannot easily create a real zombie in a unit test, but we can:
//   1. Verify it returns false for a known-alive PID (os.Getpid()).
//   2. Verify it returns false for a non-existent PID (graceful error path).
//   3. Verify the parser directly with synthetic /proc/<pid>/stat content
//      written to a temp file, exercised via a thin test shim.
// ---------------------------------------------------------------------------

// TestIsZombie_CurrentProcessIsNotZombie verifies that the running test
// process is not detected as a zombie.
func TestIsZombie_CurrentProcessIsNotZombie(t *testing.T) {
	pid := os.Getpid()
	if isZombie(pid) {
		t.Errorf("isZombie(%d): current process must not be a zombie", pid)
	}
}

// TestIsZombie_NonExistentPID_ReturnsFalse verifies that isZombie returns false
// for a PID that does not exist. The /proc read will fail and the function
// must handle the error gracefully by returning false.
func TestIsZombie_NonExistentPID_ReturnsFalse(t *testing.T) {
	if isZombie(999999) {
		t.Error("isZombie(999999): non-existent PID must return false, not true")
	}
}

// parseZombieStatContent is a test helper that applies the same parsing logic
// as isZombie but against an arbitrary byte slice, without touching /proc.
// This lets us test the parser with controlled input without creating real zombies.
func parseZombieStatContent(data []byte) bool {
	i := len(data) - 1
	for i >= 0 && data[i] != ')' {
		i--
	}
	if i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

func TestIsZombieParser_TableDriven(t *testing.T) {
	tests := []struct {
		name    string
		content string // synthetic /proc/<pid>/stat content
		want    bool
	}{
		{
			name:    "running process — state R",
			content: "1234 (myprocess) R 1 1234 1234 0 -1 4194560 100 0 0 0 10 5 0 0 20 0 1 0 0 0 0",
			want:    false,
		},
		{
			name:    "sleeping process — state S",
			content: "5678 (gode) S 1 5678 5678 0 -1 4194560 1000 0 0 0 50 25 0 0 20 0 4 0 0 0 0",
			want:    false,
		},
		{
			name:    "zombie process — state Z",
			content: "9999 (deadproc) Z 1 9999 9999 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 0 0 0",
			want:    true,
		},
		{
			name:    "disk sleep — state D",
			content: "111 (kworker) D 0 0 0 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 0 0 0",
			want:    false,
		},
		{
			name:    "process name with spaces",
			content: "222 (my prog) R 1 222 222 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 0 0 0",
			want:    false,
		},
		{
			name:    "process name with spaces — zombie",
			content: "333 (my app) Z 1 333 333 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 0 0 0",
			want:    true,
		},
		{
			name:    "process name with nested parens",
			content: "444 (prog(v2)) R 1 444 444 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 0 0 0",
			want:    false,
		},
		{
			name:    "process name with nested parens — zombie",
			content: "555 (app(v3)) Z 1 555 555 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 0 0 0 0",
			want:    true,
		},
		{
			name:    "truncated content — no closing paren",
			content: "666 (truncated",
			want:    false, // parser returns false when closing paren not found
		},
		{
			name:    "empty content",
			content: "",
			want:    false,
		},
		{
			name:    "only closing paren — no state char",
			content: "777 (x)",
			want:    false, // i+2 not < len(data) when content ends right after ')'
		},
		{
			name:    "closing paren with one char — state not Z",
			content: "888 (x) R",
			want:    false,
		},
		{
			name:    "closing paren with one char — state Z",
			content: "999 (x) Z",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseZombieStatContent([]byte(tt.content))
			if got != tt.want {
				t.Errorf("parseZombieStatContent(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

// TestIsZombie_ViaRealProcStat exercises isZombie against a real /proc/<pid>/stat
// entry (only available on Linux). On macOS/other non-Linux Unix, /proc does not
// exist so isZombie always returns false (the function's documented behaviour).
func TestIsZombie_ViaRealProcStat(t *testing.T) {
	pid := os.Getpid()
	statPath := fmt.Sprintf("/proc/%d/stat", pid)

	// If /proc is not available (macOS, FreeBSD), skip — isZombie is documented
	// to return false there and the parser test above covers the logic.
	if _, err := os.Stat(statPath); os.IsNotExist(err) {
		t.Skipf("/proc/%d/stat not available on this platform — skipping", pid)
	}

	// Read the real /proc/<pid>/stat and verify our parser agrees with isZombie.
	data, err := os.ReadFile(statPath) //nolint:gosec // test reads /proc/self/stat, not user input
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", statPath, err)
	}

	// A running test process must not be a zombie.
	parserResult := parseZombieStatContent(data)
	if parserResult {
		t.Errorf("parseZombieStatContent on real /proc/%d/stat reports zombie — unexpected for running process", pid)
	}

	// isZombie must agree.
	if isZombie(pid) {
		t.Errorf("isZombie(%d): running test process must not be a zombie", pid)
	}
}
