//go:build windows

package internal

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

// StartDetached spawns a background process that survives the parent exiting.
// stdout/stderr are redirected to logFile. The child process is fully detached.
func StartDetached(binary string, args []string, logFile string, env []string) (int, error) {
	cmd := exec.Command(binary, args...) //nolint:gosec // binary is the daemon's own executable path

	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
		HideWindow:    true,
	}

	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // logFile is a controlled daemon log path
		if err != nil {
			return 0, fmt.Errorf("open log file %s: %w", logFile, err)
		}
		cmd.Stdout = f
		cmd.Stderr = f
		// Close after Start — the child inherits the handle.
		defer func() { _ = f.Close() }()
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start detached process: %w", err)
	}

	pid := cmd.Process.Pid
	// Release the process handle so the parent does not wait.
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("release process handle: %w", err)
	}

	return pid, nil
}

// KillProcess terminates a process by PID on Windows.
// It first attempts TerminateProcess via syscall; on failure falls back to taskkill.
func KillProcess(pid int) error {
	if pid <= 0 || pid > 0x7FFFFFFF {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	handle, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid)) //nolint:gosec // pid bounds checked above
	if err == nil {
		termErr := syscall.TerminateProcess(handle, 1)
		_ = syscall.CloseHandle(handle)
		if termErr == nil {
			return nil
		}
		// Fall through to taskkill.
	}

	// Fallback: taskkill /F /PID <pid>
	cmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)) //nolint:gosec // pid is a process ID, not user input
	if killErr := cmd.Run(); killErr != nil {
		return fmt.Errorf("kill process %d: syscall failed, taskkill failed: %w", pid, killErr)
	}
	return nil
}

// ProcessBinaryPath returns the full executable path of a running process on Windows.
// Uses OpenProcess + QueryFullProcessImageNameW via syscall.
func ProcessBinaryPath(pid int) (string, error) {
	const processQueryLimitedInformation = 0x1000

	if pid <= 0 || pid > 0x7FFFFFFF {
		return "", fmt.Errorf("invalid pid: %d", pid)
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid)) //nolint:gosec // pid bounds checked above
	if err != nil {
		return "", fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	// QueryFullProcessImageNameW requires unsafe.Pointer to pass buffer addresses
	// to the Win32 API — this is the only way to call this API from pure Go.
	var buf [syscall.MAX_PATH]uint16
	n := uint32(len(buf))

	queryFullProcessImageName := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")
	ret, _, callErr := queryFullProcessImageName.Call(
		uintptr(handle),
		0,
		uintptr(unsafe.Pointer(&buf[0])), //nolint:gosec // required for Win32 API call
		uintptr(unsafe.Pointer(&n)),      //nolint:gosec // required for Win32 API call
	)
	if ret == 0 {
		return "", fmt.Errorf("query process image name for pid %d: %w", pid, callErr)
	}

	return syscall.UTF16ToString(buf[:n]), nil
}

// IsProcessAlive checks whether a process with the given PID is still running on Windows.
// Uses OpenProcess + GetExitCodeProcess. STILL_ACTIVE (259) means running.
func IsProcessAlive(pid int) bool {
	const (
		processQueryInformation = 0x0400
		synchronize             = 0x00100000
		stillActive             = 259
	)

	if pid <= 0 || pid > 0x7FFFFFFF {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryInformation|synchronize, false, uint32(pid)) //nolint:gosec // pid bounds checked above
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}
