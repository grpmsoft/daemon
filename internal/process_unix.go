//go:build !windows

package internal

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// StartDetached spawns a background process in a new session (setsid).
// stdout/stderr are redirected to logFile. The child survives the parent exiting.
func StartDetached(binary string, args []string, logFile string, env []string) (int, error) {
	cmd := exec.Command(binary, args...) //nolint:gosec // binary path comes from verified caller (Daemon.Start)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // logFile path from trusted caller (Daemon.Start)
		if err != nil {
			return 0, fmt.Errorf("open log file %s: %w", logFile, err)
		}
		cmd.Stdout = f
		cmd.Stderr = f
		defer func() { _ = f.Close() }()
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start detached process: %w", err)
	}

	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("release process handle: %w", err)
	}

	return pid, nil
}

// KillProcess sends SIGTERM and waits up to 5 seconds for the process to exit.
// If the process does not exit in time, SIGKILL is sent.
func KillProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	// Graceful: SIGTERM
	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Process may already be gone.
		if !IsProcessAlive(pid) {
			return nil
		}
		return fmt.Errorf("send SIGTERM to %d: %w", pid, err)
	}

	// Wait up to 5 seconds for exit.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Force: SIGKILL
	if err := process.Signal(syscall.SIGKILL); err != nil {
		if !IsProcessAlive(pid) {
			return nil
		}
		return fmt.Errorf("send SIGKILL to %d: %w", pid, err)
	}

	return nil
}

// ProcessBinaryPath returns the full executable path of a running process.
// On Linux, reads /proc/{pid}/exe symlink.
// On macOS/BSD, /proc is not available, so it returns ("", nil) to skip verification.
func ProcessBinaryPath(pid int) (string, error) {
	exePath := fmt.Sprintf("/proc/%d/exe", pid)
	target, err := os.Readlink(exePath)
	if err != nil {
		// macOS/BSD: /proc does not exist. Return empty string to skip verification.
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("readlink %s: %w", exePath, err)
	}
	return target, nil
}

// IsProcessAlive checks whether a process with the given PID exists.
// On Unix, sending signal 0 checks existence without affecting the process.
func IsProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
