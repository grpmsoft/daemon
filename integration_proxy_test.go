package daemon_test

// Proxy across daemon restarts and failures (helper-process daemon, real
// sockets). A proxy is one MCP client's stdio bridge; a daemon restart (a
// deploy) or a crash must not end it, and a request must never run twice.

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grpmsoft/daemon"
)

type proxyClient struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan string
	done  chan error
}

// startProxy runs daemon.Proxy over pipes, as an MCP client would over stdio.
func startProxy(t *testing.T, cfg daemon.Config) *proxyClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &proxyClient{t: t, in: inW, lines: make(chan string, 16), done: make(chan error, 1)}
	go func() {
		err := daemon.Proxy(context.Background(), cfg, daemon.ProxyOptions{MCPPath: "/mcp", Stdin: inR, Stdout: outW})
		_ = outW.Close()
		c.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case err := <-c.done:
			if err != nil {
				t.Errorf("proxy returned %v after stdin EOF, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("proxy did not return after stdin EOF")
		}
	})
	return c
}

func (c *proxyClient) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatalf("write to proxy: %v", err)
	}
}

type rpcReply struct {
	ID     int `json:"id"`
	Result *struct {
		PID int `json:"pid"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *proxyClient) reply(within time.Duration) rpcReply {
	c.t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			c.t.Fatalf("proxy output closed (proxy returned: %v)", <-c.done)
		}
		var r rpcReply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			c.t.Fatalf("reply %q: %v", line, err)
		}
		return r
	case <-time.After(within):
		c.t.Fatalf("no reply within %v", within)
	}
	return rpcReply{}
}

// call sends a ping and requires a result (not an error) with that id.
func (c *proxyClient) call(id int) rpcReply {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"ping"}`)
	r := c.reply(30 * time.Second)
	if r.ID != id || r.Result == nil {
		c.t.Fatalf("call %d: got id %d result %v error %+v", id, r.ID, r.Result, r.Error)
	}
	return r
}

func (c *proxyClient) requireRunning() {
	c.t.Helper()
	select {
	case err := <-c.done:
		c.t.Fatalf("proxy returned: %v", err)
	default:
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func execCount(t *testing.T, dataDir, method string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dataDir, "exec.log")) //nolint:gosec // test-only file in t.TempDir
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if l == method {
			n++
		}
	}
	return n
}

func status(t *testing.T, cfg daemon.Config) daemon.Info {
	t.Helper()
	info, err := daemon.New(cfg).Status()
	if err != nil {
		t.Fatal(err)
	}
	return *info
}

// A restart (deploy) between two requests: the second request reaches the new
// daemon; the proxy does not exit; the MCP client sees nothing. Which path gets
// there first (the lease watcher re-attaching, or the request's retry) is not
// fixed here; the retry alone is pinned by the stopped-daemon and held-daemon
// tests, where no watcher can help.
func TestIntegration_ProxyRequestAfterRestart(t *testing.T) {
	cfg := itConfig(t)
	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	c := startProxy(t, cfg)
	first := c.call(1)

	info, err := daemon.New(cfg).Restart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.PID == first.Result.PID {
		t.Fatalf("restart kept pid %d", info.PID)
	}

	second := c.call(2)
	if second.Result.PID != info.PID {
		t.Errorf("second request served by pid %d, want the new daemon %d", second.Result.PID, info.PID)
	}
	c.requireRunning()
}

// After a restart the proxy re-attaches its lease without waiting for a request:
// the new daemon, idle timeout 2 s, must not see zero agents while the client
// is idle (a session that is open but quiet keeps its daemon).
func TestIntegration_ProxyReattachesLeaseAfterRestart(t *testing.T) {
	t.Setenv("IT_IDLE", "2s")
	cfg := itConfig(t)
	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	c := startProxy(t, cfg)
	c.call(1)

	info, err := daemon.New(cfg).Restart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second) // > 2 s idle: an unattached daemon would be gone
	if st := status(t, cfg); st.Status != daemon.StatusRunning || st.PID != info.PID {
		t.Fatalf("new daemon after an idle client: %+v, want running pid %d (lease not re-attached)", st, info.PID)
	}
	if r := c.call(2); r.Result.PID != info.PID {
		t.Errorf("served by %d, want %d", r.Result.PID, info.PID)
	}
}

// A stopped daemon is not resurrected by an idle proxy (a stop is the user's or
// the idle timer's decision); the next request starts it on demand.
func TestIntegration_ProxyStartsStoppedDaemonOnDemandOnly(t *testing.T) {
	cfg := itConfig(t)
	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	c := startProxy(t, cfg)
	first := c.call(1)

	if err := daemon.New(cfg).Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if st := status(t, cfg); st.Status != daemon.StatusStopped {
		t.Fatalf("idle proxy restarted a stopped daemon: %+v", st)
	}

	second := c.call(2)
	if second.Result.PID == first.Result.PID {
		t.Errorf("second request served by the stopped daemon's pid %d", first.Result.PID)
	}
	c.requireRunning()
}

// A daemon that dies while a request is in flight: the request may have run
// (a tools/call may have edited files), so it is answered with a JSON-RPC
// error for its id and NOT sent again; the proxy goes on.
func TestIntegration_ProxyMidFlightFailureIsAnsweredNotRetried(t *testing.T) {
	cfg := itConfig(t)
	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	c := startProxy(t, cfg)
	first := c.call(1)

	c.send(`{"jsonrpc":"2.0","id":7,"method":"slow"}`)
	deadline := time.Now().Add(10 * time.Second)
	for execCount(t, cfg.DataDir, "slow") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slow request never reached the daemon")
		}
		time.Sleep(50 * time.Millisecond)
	}
	p, err := os.FindProcess(first.Result.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}

	r := c.reply(15 * time.Second)
	if r.ID != 7 || r.Error == nil || r.Result != nil {
		t.Fatalf("reply to the interrupted request: %+v, want a JSON-RPC error with id 7", r)
	}
	time.Sleep(time.Second)
	if n := execCount(t, cfg.DataDir, "slow"); n != 1 {
		t.Fatalf("slow request executed %d times, want 1 (an interrupted request is never retried)", n)
	}
	if r := c.call(8); r.Result.PID == first.Result.PID {
		t.Errorf("served by the killed pid %d", first.Result.PID)
	}
	c.requireRunning()
}

// A held daemon (stop intent) is not started by the proxy: requests get a
// JSON-RPC error, notifications get nothing, and the proxy stays up until
// Release, after which requests work again.
func TestIntegration_ProxyHeldDaemonAnswersErrors(t *testing.T) {
	cfg := itConfig(t)
	if _, err := daemon.EnsureRunning(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	c := startProxy(t, cfg)
	c.call(1)

	d := daemon.New(cfg)
	if err := d.Hold(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`) // no id: no reply
	c.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	r := c.reply(30 * time.Second)
	if r.ID != 2 || r.Error == nil {
		t.Fatalf("request to a held daemon: %+v, want a JSON-RPC error with id 2 (and nothing for the notification)", r)
	}
	if st := status(t, cfg); st.Status != daemon.StatusStopped {
		t.Fatalf("proxy started a held daemon: %+v", st)
	}
	c.requireRunning()

	d.Release()
	c.call(3)
}
