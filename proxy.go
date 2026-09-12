package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/grpmsoft/daemon/internal/pidlock"
)

// ProxyOptions configures the Proxy behavior.
type ProxyOptions struct {
	// MCPPath is the HTTP path on the daemon to POST JSON-RPC messages to.
	// Default: "/mcp".
	MCPPath string

	// Stdin is the input reader for JSON-RPC messages. Default: os.Stdin.
	Stdin io.Reader

	// Stdout is the output writer for JSON-RPC responses. Default: os.Stdout.
	Stdout io.Writer

	// LogPayloads enables logging of request/response bodies (first 500 bytes).
	// Default: false (only method/id/size/latency logged for security — S3).
	LogPayloads bool
}

func (o *ProxyOptions) applyDefaults() {
	if o.MCPPath == "" {
		o.MCPPath = "/mcp"
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
}

// Proxy bridges an input stream to the daemon's HTTP endpoint.
// Reads newline-delimited JSON-RPC from opts.Stdin, POSTs each to the daemon,
// writes responses to opts.Stdout.
//
// Connection tracking: signals /daemon/connect on start and /daemon/disconnect
// on exit (only if connect succeeded — prevents stray decrements).
//
// Respects ctx for cancellation. Uses context-based timeouts instead of
// hardcoded http.Client.Timeout.
func Proxy(ctx context.Context, cfg Config, opts ProxyOptions) error {
	cfg.applyDefaults()
	opts.applyDefaults()

	pidPath := filepath.Join(cfg.DataDir, cfg.Name+".pid")

	info, err := readPIDInfo(pidPath)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	if !pidlock.IsHeld(pidPath) {
		return fmt.Errorf("proxy: %w", ErrNotRunning)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d%s", info.Port, opts.MCPPath)
	daemonBase := fmt.Sprintf("http://127.0.0.1:%d", info.Port)
	token := info.Token

	plog := proxyLogger(cfg)

	// No hardcoded Timeout — use ctx for deadline control.
	client := &http.Client{}

	plog.Printf("proxy started, target=%s", baseURL)

	// Try lease-based attach first (v0.3.1+). If the daemon returns 404
	// (v0.3.0 daemon without /daemon/attach), fall back to the deprecated
	// POST connect/disconnect pair.
	attachCancel, fallbackConnected := proxyAttach(ctx, daemonBase, token, plog)
	defer func() {
		if attachCancel != nil {
			// Lease mode: cancel the attach context to close the TCP connection.
			attachCancel()
		} else if fallbackConnected {
			// Fallback mode: send explicit disconnect.
			plog.Printf("proxy stopping, sending disconnect")
			controlClient := &http.Client{Timeout: 5 * time.Second}
			signalDisconnect(controlClient, daemonBase, token)
		}
	}()

	reader := bufio.NewReaderSize(opts.Stdin, 64*1024)

	msgCount := 0
	for {
		select {
		case <-ctx.Done():
			plog.Printf("context done after %d messages", msgCount)
			return nil
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				plog.Printf("EOF after %d messages", msgCount)
				return nil
			}
			plog.Printf("read error: %v", err)
			return fmt.Errorf("read stdin: %w", err)
		}

		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		msgCount++
		if opts.LogPayloads {
			plog.Printf("msg #%d REQ len=%d body=%s", msgCount, len(line), truncate(line, 500))
		}

		resp, statusCode, reqErr := proxyRequest(ctx, client, baseURL, token, line)
		if reqErr != nil {
			plog.Printf("msg #%d ERROR: %v", msgCount, reqErr)
			return fmt.Errorf("proxy to %s: %w", baseURL, reqErr)
		}

		if opts.LogPayloads {
			plog.Printf("msg #%d RESP status=%d len=%d body=%s", msgCount, statusCode, len(resp), truncate(resp, 500))
		}

		if statusCode == 204 || len(resp) == 0 {
			continue
		}

		// Write response + newline in one call for NDJSON framing.
		resp = append(bytes.TrimRight(resp, " \t\r\n"), '\n')
		if _, writeErr := opts.Stdout.Write(resp); writeErr != nil {
			plog.Printf("msg #%d stdout write error: %v", msgCount, writeErr)
			return fmt.Errorf("write response to stdout: %w", writeErr)
		}
	}
}

func truncate(b []byte, limit int) string {
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "..."
}

func readPIDInfo(pidPath string) (PIDInfo, error) {
	data, err := pidlock.ReadLocked(pidPath)
	if err != nil {
		return PIDInfo{}, fmt.Errorf("read pid file: %w", err)
	}
	var info PIDInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return PIDInfo{}, fmt.Errorf("parse pid file: %w", err)
	}
	if info.Port <= 0 {
		return PIDInfo{}, fmt.Errorf("daemon running but port is %d", info.Port)
	}
	return info, nil
}

func proxyLogger(cfg Config) *log.Logger {
	logPath := filepath.Join(cfg.DataDir, "proxy.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // logPath from Config.DataDir
	if err != nil {
		return log.New(io.Discard, "", 0)
	}
	return log.New(f, fmt.Sprintf("[proxy:%s] ", cfg.Name), log.LstdFlags)
}

// proxyAttach tries lease-based attach (GET /daemon/attach). Returns:
//   - (cancelFunc, false) if lease succeeded — caller must cancel on exit.
//   - (nil, true) if 404 fallback to connect/disconnect succeeded.
//   - (nil, false) if nothing connected.
func proxyAttach(ctx context.Context, daemonBase, token string, plog *log.Logger) (context.CancelFunc, bool) {
	attachCtx, attachCancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(attachCtx, http.MethodGet, daemonBase+"/daemon/attach", nil)
	if err != nil {
		attachCancel()
		return nil, false
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// Use a transport with no idle timeout to keep the connection alive.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		attachCancel()
		// Connection error — daemon may be dead.
		return nil, false
	}

	// 404 means v0.3.0 daemon without /daemon/attach — fall back.
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		attachCancel()
		plog.Printf("attach endpoint not available (404), falling back to connect/disconnect")
		controlClient := &http.Client{Timeout: 5 * time.Second}
		connected := signalConnect(ctx, controlClient, daemonBase, token)
		return nil, connected
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		attachCancel()
		plog.Printf("attach returned unexpected status %d", resp.StatusCode)
		return nil, false
	}

	// Wait for ack byte with 2-second timeout.
	ackDone := make(chan error, 1)
	go func() {
		ack := make([]byte, 1)
		_, readErr := resp.Body.Read(ack)
		ackDone <- readErr
	}()
	select {
	case ackErr := <-ackDone:
		if ackErr != nil {
			_ = resp.Body.Close()
			attachCancel()
			plog.Printf("attach ack read error: %v", ackErr)
			return nil, false
		}
	case <-time.After(2 * time.Second):
		_ = resp.Body.Close()
		attachCancel()
		plog.Printf("attach ack timeout (2s)")
		return nil, false
	}

	plog.Printf("attached via lease (TCP connection)")

	// Keep resp.Body open for proxy lifetime. When attachCancel() is called,
	// the request context is cancelled, which closes the TCP connection and
	// causes the daemon's handler to observe r.Context().Done().
	// We don't close resp.Body here — attachCancel() handles cleanup.
	go func() {
		// Block until the connection is closed (context cancelled or daemon shutdown).
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}()

	return attachCancel, false
}

func signalConnect(ctx context.Context, client *http.Client, daemonBase, token string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonBase+"/daemon/connect", nil)
	if err != nil {
		return false
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 300
}

func signalDisconnect(client *http.Client, daemonBase, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonBase+"/daemon/disconnect", nil)
	if err != nil {
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func proxyRequest(ctx context.Context, client *http.Client, url, token string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

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
