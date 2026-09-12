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

	// Reap the child in a background goroutine to prevent zombie processes.
	// Without Wait(), an exited child stays in the process table as a zombie
	// while the parent (proxy) is alive. No stdout/stderr pipes exist (output
	// goes to *os.File directly), so Wait only calls waitpid.
	go func() { _ = cmd.Wait() }()

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

// IsProcessAlive checks whether a process with the given PID exists and is not a zombie.
// On Unix, sending signal 0 checks existence without affecting the process.
// On Linux, /proc/<pid>/stat is checked for zombie state (Z) since kill(0)
// succeeds on zombies.
func IsProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return !isZombie(pid)
}

// isZombie checks if a process is in zombie state via /proc/<pid>/stat.
// Returns false on non-Linux (macOS/BSD have no /proc) — kill(0) is
// sufficient there since zombies are reaped by the kernel differently.
func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// /proc/<pid>/stat format: "pid (comm) state ..."
	// Find the closing paren (comm can contain spaces/parens), then the state char.
	i := len(data) - 1
	for i >= 0 && data[i] != ')' {
		i--
	}
	if i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}
