package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Proxy bridges stdin/stdout to an HTTP endpoint on the daemon.
// Used by "gode mcp serve" to proxy MCP JSON-RPC through stdio to the shared daemon.
//
// Protocol: reads complete JSON-RPC messages from stdin (newline-delimited JSON),
// POSTs each to http://127.0.0.1:{port}{mcpPath}, writes responses to stdout.
//
// Connection tracking: on start, POSTs /daemon/connect to increment the daemon's
// active connection count. On exit (EOF or error), POSTs /daemon/disconnect to
// decrement it. This enables the daemon's idle auto-shutdown feature.
//
// Exits cleanly on EOF (stdin closed = agent disconnected, daemon stays).
// Returns error on connection refused (daemon died).
func Proxy(ctx context.Context, port int, mcpPath string) error {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, mcpPath)
	daemonBase := fmt.Sprintf("http://127.0.0.1:%d", port)

	// Log to file for debugging (stderr may be closed by Claude Code).
	plog := proxyLogger(port)

	client := &http.Client{
		Timeout: 120 * time.Second,
	}

	plog.Printf("proxy started, target=%s", baseURL)

	connected := signalConnect(ctx, client, daemonBase)
	defer func() {
		if connected {
			plog.Printf("proxy stopping, sending disconnect")
			signalDisconnect(client, daemonBase)
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	msgCount := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			plog.Printf("context done after %d messages", msgCount)
			return nil
		default:
		}

		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		msgCount++
		plog.Printf("msg #%d REQ len=%d body=%s", msgCount, len(line), truncate(line, 500))

		resp, statusCode, err := proxyRequest(ctx, client, baseURL, line)
		if err != nil {
			plog.Printf("msg #%d ERROR: %v", msgCount, err)
			return fmt.Errorf("proxy to %s: %w", baseURL, err)
		}

		plog.Printf("msg #%d RESP status=%d len=%d body=%s", msgCount, statusCode, len(resp), truncate(resp, 500))

		if statusCode == 204 || len(resp) == 0 {
			continue
		}

		if _, writeErr := os.Stdout.Write(resp); writeErr != nil {
			plog.Printf("msg #%d stdout write error: %v", msgCount, writeErr)
			return fmt.Errorf("write response to stdout: %w", writeErr)
		}
		if _, writeErr := os.Stdout.Write([]byte("\n")); writeErr != nil {
			return fmt.Errorf("write newline to stdout: %w", writeErr)
		}
	}

	if err := scanner.Err(); err != nil {
		plog.Printf("scanner error: %v", err)
		return fmt.Errorf("read stdin: %w", err)
	}

	plog.Printf("EOF after %d messages", msgCount)
	return nil
}

func truncate(b []byte, limit int) string {
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "..."
}

func proxyLogger(port int) *log.Logger {
	dataDir := os.Getenv("DAEMON_DATA_DIR")
	if dataDir == "" {
		dataDir = ".gode"
	}
	logPath := filepath.Join(dataDir, "proxy.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // logPath is constructed from DAEMON_DATA_DIR env var + fixed filename
	if err != nil {
		return log.New(io.Discard, "", 0)
	}
	return log.New(f, fmt.Sprintf("[proxy:%d] ", port), log.LstdFlags)
}

// signalConnect notifies the daemon that a new proxy connection is active.
// Best-effort: errors are silently ignored (daemon may be an older version).
func signalConnect(ctx context.Context, client *http.Client, daemonBase string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonBase+"/daemon/connect", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 300
}

// signalDisconnect notifies the daemon that a proxy connection has ended.
// Best-effort: errors are silently ignored. Uses a background context because
// the original ctx may already be cancelled at defer time.
// Must only be called if signalConnect returned true.
func signalDisconnect(client *http.Client, daemonBase string) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, daemonBase+"/daemon/disconnect", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// proxyRequest POSTs a single JSON-RPC message to the daemon and returns the response body.
func proxyRequest(ctx context.Context, client *http.Client, url string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("daemon returned HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}

	return respBody, resp.StatusCode, nil
}
