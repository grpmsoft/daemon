// Package internal provides platform-specific implementations for the daemon library:
// PID file management, process lifecycle operations, and health checking.
package internal

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// CheckHealth performs a single HTTP GET to the health endpoint and returns
// nil if the response status is 2xx.
func CheckHealth(port int, healthPath string) error {
	client := &http.Client{Timeout: time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, healthPath)

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("health check %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("health check %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// WaitUntilReady polls the health endpoint every 500ms until it responds OK,
// the timeout expires, or ctx is cancelled.
func WaitUntilReady(ctx context.Context, port int, healthPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := CheckHealth(port, healthPath); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("daemon not ready after %s on port %d", timeout, port)
}
