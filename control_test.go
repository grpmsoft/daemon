package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// controlTestDaemon returns a Daemon whose PID file names the given port and
// token, as a running daemon's would.
func controlTestDaemon(port int, token string) *Daemon {
	pids := &pidStoreMock{
		saved:       &PIDInfo{PID: 4242, Port: port, Token: token},
		aliveResult: true,
	}
	return newMockDaemon(pids, &mockProcessManager{aliveResult: true}, &mockHealthChecker{})
}

func controlTestPort(t *testing.T, serverURL string) int {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func controlTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestControlClient_GetSendsTheBearerToken(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"metrics":"ok"}`))
	}))
	defer srv.Close()

	client, err := controlTestDaemon(controlTestPort(t, srv.URL), "s3cret").ControlClient()
	if err != nil {
		t.Fatalf("ControlClient: %v", err)
	}
	body, err := client.Get(controlTestContext(t), "/debug/metrics")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization %q, want %q", gotAuth, "Bearer s3cret")
	}
	if gotPath != "/debug/metrics" {
		t.Errorf("path %q, want %q", gotPath, "/debug/metrics")
	}
	if string(body) != `{"metrics":"ok"}` {
		t.Errorf("body %q", body)
	}
}

func TestControlClient_GetWithoutTokenSendsNoHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	client, err := controlTestDaemon(controlTestPort(t, srv.URL), "").ControlClient()
	if err != nil {
		t.Fatalf("ControlClient: %v", err)
	}
	if _, err := client.Get(controlTestContext(t), "/health"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization %q, want none", gotAuth)
	}
}

func TestControlClient_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client, err := controlTestDaemon(controlTestPort(t, srv.URL), "wrong").ControlClient()
	if err != nil {
		t.Fatalf("ControlClient: %v", err)
	}
	_, err = client.Get(controlTestContext(t), "/debug/metrics")
	if err == nil {
		t.Fatal("a 401 must be an error, not an empty body")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("error %q should carry the status and the body", err)
	}
}

func TestControlClient_NotRunningIsAnError(t *testing.T) {
	if _, err := controlTestDaemon(0, "").ControlClient(); err == nil {
		t.Error("a daemon without a port must not produce a client")
	}
}

func TestControlClient_NoPIDFileIsAnError(t *testing.T) {
	pids := &pidStoreMock{loadErr: errors.New("no such file")}
	d := newMockDaemon(pids, &mockProcessManager{}, &mockHealthChecker{})

	if _, err := d.ControlClient(); err == nil {
		t.Error("a missing PID file must be an error")
	}
}
