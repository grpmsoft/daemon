package internal

import (
	"fmt"
	"net"
)

// FindFreePort asks the OS for an available TCP port by binding to :0
// and immediately closing the listener.
func FindFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen for free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, fmt.Errorf("close free port listener: %w", err)
	}
	return port, nil
}

// StartDetached, KillProcess, IsProcessAlive are defined in
// platform-specific files (process_windows.go, process_unix.go).
