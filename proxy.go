package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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

// leaseRecoverPoll is how often a proxy whose lease dropped looks for the
// daemon that replaced the old one.
const leaseRecoverPoll = 200 * time.Millisecond

// Proxy bridges an input stream to the daemon's HTTP endpoint.
// Reads newline-delimited JSON-RPC from opts.Stdin, POSTs each to the daemon,
// writes responses to opts.Stdout.
//
// Connection tracking: the proxy holds a lease (GET /daemon/attach, the TCP
// connection is the lease) for as long as it runs, so the daemon's idle timer
// counts it as an agent.
//
// The daemon may be replaced while the proxy runs (restart, crash, idle
// shutdown); the proxy follows it and the client on stdin never notices:
//   - When its lease drops, the proxy looks for a daemon in the PID file for up
//     to cfg.Timeout and re-attaches. It never starts one there: a stop is a
//     decision (Stop, Hold, the idle timer), and an idle client must not undo it.
//   - A request that provably never reached a daemon (the dial failed), or that
//     a daemon rejected with 401 (another daemon on the old port), is sent once
//     more after reconnecting: EnsureRunning when cfg can start the daemon
//     (Binary set), else the PID file is read again.
//   - A request that failed after it was sent may have run (a tools/call may
//     have edited files): it is answered with a JSON-RPC error for its id and
//     never sent again. A notification gets no answer.
//
// Proxy returns only when ctx is done, stdin ends or fails, or stdout fails.
// At start it requires a running daemon (ErrNotRunning otherwise).
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

	plog, plogClose := proxyLogger(cfg)
	defer plogClose()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the lease and its watcher

	p := &proxySession{
		cfg:     cfg,
		mcpPath: opts.MCPPath,
		pidPath: pidPath,
		plog:    plog,
		client:  &http.Client{}, // no hardcoded Timeout — use ctx for deadline control
	}
	p.retarget(ctx, info)
	plog.Printf("proxy started, target=%s", p.current().url)

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

		resp, statusCode, reqErr := p.forward(ctx, line)
		if reqErr != nil {
			if ctx.Err() != nil {
				plog.Printf("context done after %d messages", msgCount)
				return nil
			}
			plog.Printf("msg #%d ERROR: %v", msgCount, reqErr)
			reply, ok := errorReply(line, reqErr.Error())
			if !ok {
				continue // a notification: nothing to answer
			}
			resp, statusCode = reply, http.StatusOK
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

// proxyTarget is the daemon a proxy talks to.
type proxyTarget struct {
	port  int
	token string
	base  string // http://127.0.0.1:port
	url   string // base + MCPPath
}

// proxySession is the state of one Proxy: the current daemon and the lease on
// it. The request loop and the lease watcher both retarget it, under mu.
type proxySession struct {
	cfg     Config
	mcpPath string
	pidPath string
	plog    *log.Logger
	client  *http.Client

	mu     sync.Mutex
	target proxyTarget
	lease  *proxyLease
}

func (p *proxySession) current() proxyTarget {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.target
}

// forward sends one JSON-RPC message, reconnecting and sending it once more
// only when it provably did not reach a daemon (see Proxy).
func (p *proxySession) forward(ctx context.Context, line []byte) ([]byte, int, error) {
	t := p.current()
	resp, code, err := proxyRequest(ctx, p.client, t.url, t.token, line)
	if err == nil || ctx.Err() != nil {
		return resp, code, err
	}
	if !notDelivered(err) && code != http.StatusUnauthorized {
		return resp, code, err
	}
	p.plog.Printf("daemon at %s did not take the request (%v): reconnecting", t.base, err)
	if rerr := p.reconnect(ctx); rerr != nil {
		return nil, 0, fmt.Errorf("%w; reconnect: %w", err, rerr)
	}
	t = p.current()
	return proxyRequest(ctx, p.client, t.url, t.token, line)
}

// reconnect finds the daemon for a request: EnsureRunning when the config can
// start one (it refuses a held daemon with ErrStopIntent), then the PID file.
func (p *proxySession) reconnect(ctx context.Context) error {
	if p.cfg.Binary != "" {
		if _, err := New(p.cfg).EnsureRunning(ctx); err != nil {
			return err
		}
	}
	info, err := readPIDInfo(p.pidPath)
	if err != nil {
		return err
	}
	if !pidlock.IsHeld(p.pidPath) {
		return ErrNotRunning
	}
	p.retarget(ctx, info)
	return nil
}

// retarget points the session at the daemon in info and holds a lease on it.
// It is a no-op when that is already the target and its lease is alive. It
// reports whether a lease is held afterwards.
func (p *proxySession) retarget(ctx context.Context, info PIDInfo) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.target.port == info.Port && p.target.token == info.Token && p.lease.alive() {
		return true
	}
	if p.lease != nil {
		p.lease.cancel()
		p.lease = nil
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", info.Port)
	p.target = proxyTarget{port: info.Port, token: info.Token, base: base, url: base + p.mcpPath}
	l := proxyAttach(ctx, base, info.Token, p.plog)
	if l == nil {
		return false
	}
	p.lease = l
	go p.watch(ctx, l)
	return true
}

// watch waits for lease l to drop and then looks for the daemon that replaced
// the old one, without starting one (see Proxy).
func (p *proxySession) watch(ctx context.Context, l *proxyLease) {
	select {
	case <-ctx.Done():
		return
	case <-l.done:
	}
	p.mu.Lock()
	replaced := p.lease != l
	p.mu.Unlock()
	if replaced || ctx.Err() != nil {
		return // this proxy retargeted already, or is ending
	}
	p.plog.Printf("lease dropped: looking for the daemon for up to %v", p.cfg.Timeout)
	deadline := time.Now().Add(p.cfg.Timeout)
	tick := time.NewTicker(leaseRecoverPoll)
	defer tick.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		p.mu.Lock()
		replaced = p.lease != nil && p.lease != l
		p.mu.Unlock()
		if replaced {
			return // a request reconnected meanwhile; its lease has its own watcher
		}
		info, err := readPIDInfo(p.pidPath)
		if err != nil || !pidlock.IsHeld(p.pidPath) {
			continue
		}
		if p.retarget(ctx, info) {
			p.plog.Printf("re-attached to the daemon on port %d", info.Port)
			return
		}
	}
	p.plog.Printf("no daemon within %v: the next request starts one if the config can", p.cfg.Timeout)
}

// notDelivered reports whether a request failed before it reached any daemon:
// the dial failed. After the dial (reset, EOF, cancellation) it may have run.
func notDelivered(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// errorReply is the JSON-RPC error response to the request in line, carrying
// its id verbatim. A notification (no id, or null) and input that is not a
// JSON object get none.
func errorReply(line []byte, msg string) ([]byte, bool) {
	var req struct {
		ID jsontext.Value `json:"id"`
	}
	if err := json.Unmarshal(line, &req); err != nil || len(req.ID) == 0 || string(req.ID) == "null" {
		return nil, false
	}
	type rpcError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	reply := struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      jsontext.Value `json:"id"`
		Error   rpcError       `json:"error"`
	}{"2.0", req.ID, rpcError{Code: -32603, Message: "daemon unavailable: " + msg}}
	b, err := json.Marshal(reply)
	if err != nil {
		return nil, false
	}
	return b, true
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

// proxyLogger creates a logger that writes to proxy.log in DataDir.
// Returns the logger and a cleanup function that closes the log file.
func proxyLogger(cfg Config) (*log.Logger, func()) {
	logPath := filepath.Join(cfg.DataDir, "proxy.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // logPath from Config.DataDir
	if err != nil {
		return log.New(io.Discard, "", 0), func() {}
	}
	return log.New(f, fmt.Sprintf("[proxy:%s] ", cfg.Name), log.LstdFlags), func() { _ = f.Close() }
}

// proxyLease is a held lease: cancel ends it, done is closed when the
// connection ends for any reason (cancel, daemon exit, network).
type proxyLease struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (l *proxyLease) alive() bool {
	if l == nil {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

// proxyAttach tries lease-based attach (GET /daemon/attach). Returns:
//   - the lease if it succeeded — the caller must cancel it on exit;
//   - nil if attach failed (daemon dead, wrong status, etc.).
func proxyAttach(ctx context.Context, daemonBase, token string, plog *log.Logger) *proxyLease {
	attachCtx, attachCancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(attachCtx, http.MethodGet, daemonBase+"/daemon/attach", nil)
	if err != nil {
		attachCancel()
		return nil
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
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		attachCancel()
		plog.Printf("attach returned unexpected status %d", resp.StatusCode)
		return nil
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
			return nil
		}
	case <-time.After(2 * time.Second):
		_ = resp.Body.Close()
		attachCancel()
		plog.Printf("attach ack timeout (2s)")
		return nil
	}

	plog.Printf("attached via lease (TCP connection)")

	// Keep resp.Body open for the lease's lifetime. When the lease is cancelled,
	// the request context is cancelled, which closes the TCP connection and
	// causes the daemon's handler to observe r.Context().Done(). The daemon's
	// exit ends it from the other side; either way done is closed.
	l := &proxyLease{cancel: attachCancel, done: make(chan struct{})}
	go func() {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		close(l.done)
	}()

	return l
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
