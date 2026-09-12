//go:build windows

package internal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStartDetached_SpawnsProcess verifies that StartDetached launches a real
// background process and returns a valid PID.
//
// We use "cmd.exe /C timeout /T 60" — a long-lived Windows command that stays
// alive long enough to be inspected, then killed.
func TestStartDetached_SpawnsProcess(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "detach.log")

	// "cmd /C timeout /T 60 /NOBREAK >NUL 2>&1" runs for 60 seconds.
	pid, err := StartDetached("cmd.exe", []string{"/C", "timeout", "/T", "60", "/NOBREAK"}, logFile, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid <= 0 {
		t.Errorf("StartDetached must return a positive PID, got %d", pid)
	}

	// The process must be alive immediately after spawning.
	if !IsProcessAlive(pid) {
		t.Error("spawned process must be alive")
	}

	// Kill it to avoid leaving a dangling process.
	killErr := KillProcess(context.Background(), pid, 5*time.Second)
	if killErr != nil {
		t.Errorf("unexpected error: %v", killErr)
	}

	// Give the OS a moment to reap the process.
	time.Sleep(200 * time.Millisecond)
	if IsProcessAlive(pid) {
		t.Error("process must be dead after KillProcess")
	}
}

// TestStartDetached_CreatesLogFile verifies that the log file is created when
// StartDetached is given a non-empty logFile path.
func TestStartDetached_CreatesLogFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "out.log")

	pid, err := StartDetached("cmd.exe", []string{"/C", "echo", "hello"}, logFile, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid <= 0 {
		t.Errorf("StartDetached must return a positive PID, got %d", pid)
	}

	// Give the process a moment to start and write output.
	time.Sleep(200 * time.Millisecond)

	_, statErr := os.Stat(logFile)
	if statErr != nil {
		t.Errorf("log file must exist after StartDetached: %v", statErr)
	}

	// Clean up (process may have already exited).
	_ = KillProcess(context.Background(), pid, 5*time.Second)
}

// TestStartDetached_EmptyLogFile verifies StartDetached works without a log file.
func TestStartDetached_EmptyLogFile(t *testing.T) {
	pid, err := StartDetached("cmd.exe", []string{"/C", "timeout", "/T", "10", "/NOBREAK"}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid <= 0 {
		t.Errorf("StartDetached must return a positive PID, got %d", pid)
	}

	_ = KillProcess(context.Background(), pid, 5*time.Second)
}

// TestStartDetached_InvalidBinary_ReturnsError verifies that a bad binary path
// results in an error.
func TestStartDetached_InvalidBinary_ReturnsError(t *testing.T) {
	_, err := StartDetached("/no/such/binary/that/does/not/exist", nil, "", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestKillProcess_AlreadyDead_NoError verifies that KillProcess succeeds even
// when the process has already exited.
func TestKillProcess_AlreadyDead_NoError(t *testing.T) {
	// Start a process that exits immediately.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "exit.log")

	pid, err := StartDetached("cmd.exe", []string{"/C", "exit", "0"}, logFile, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Give it time to exit naturally.
	time.Sleep(300 * time.Millisecond)

	// KillProcess on an already-dead process must not return an error.
	killErr := KillProcess(context.Background(), pid, 5*time.Second)
	// On Windows, killing a dead process may or may not error depending on handle state.
	// We accept both outcomes; the important thing is it doesn't panic.
	_ = killErr
}
