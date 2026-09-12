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

	port, err := readPort(pidPath)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	if !pidlock.IsHeld(pidPath) {
		return fmt.Errorf("proxy: %w", ErrNotRunning)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, opts.MCPPath)
	daemonBase := fmt.Sprintf("http://127.0.0.1:%d", port)

	plog := proxyLogger(cfg)

	// No hardcoded Timeout — use ctx for deadline control.
	client := &http.Client{}

	// Short-timeout client for control calls (connect/disconnect).
	controlClient := &http.Client{Timeout: 5 * time.Second}

	plog.Printf("proxy started, target=%s", baseURL)

	connected := signalConnect(ctx, controlClient, daemonBase)
	defer func() {
		if connected {
			plog.Printf("proxy stopping, sending disconnect")
			signalDisconnect(controlClient, daemonBase)
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

		resp, statusCode, reqErr := proxyRequest(ctx, client, baseURL, line)
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

func readPort(pidPath string) (int, error) {
	data, err := pidlock.ReadLocked(pidPath)
	if err != nil {
		return 0, fmt.Errorf("read pid file: %w", err)
	}
	var info PIDInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return 0, fmt.Errorf("parse pid file: %w", err)
	}
	if info.Port <= 0 {
		return 0, fmt.Errorf("daemon running but port is %d", info.Port)
	}
	return info.Port, nil
}

func proxyLogger(cfg Config) *log.Logger {
	logPath := filepath.Join(cfg.DataDir, "proxy.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // logPath from Config.DataDir
	if err != nil {
		return log.New(io.Discard, "", 0)
	}
	return log.New(f, fmt.Sprintf("[proxy:%s] ", cfg.Name), log.LstdFlags)
}

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

func signalDisconnect(client *http.Client, daemonBase string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonBase+"/daemon/disconnect", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

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
