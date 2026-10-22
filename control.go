package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// controlBodyLimit bounds a control-plane response body: the endpoints are
// local JSON documents, not streams.
const controlBodyLimit = 8 << 20

// controlErrorSnippet bounds the response body quoted in an error.
const controlErrorSnippet = 300

// controlRequestTimeout bounds a whole control-plane request even when the
// caller passes a context without a deadline. A loopback endpoint that does not
// answer within this window is not going to.
const controlRequestTimeout = 30 * time.Second

// ControlClient talks to the loopback control plane of a running daemon.
//
// It owns the two things every caller would otherwise repeat: the address from
// the PID file and the bearer token written there. The token is the daemon's
// control-plane secret, so it stays inside the client — no accessor hands it
// out, and no caller has to know how a request is authenticated.
//
// The endpoints are the daemon's own (shutdown, health) and those its owner
// registers on the mux it passes to Serve (GODE's /debug/metrics).
type ControlClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// ControlClient returns a client for the running daemon's control plane.
// An error means the PID file cannot be read or names no port — the daemon did
// not reach the point of listening — never that a request failed.
func (d *Daemon) ControlClient() (*ControlClient, error) {
	data, err := d.pids.Load()
	if err != nil {
		return nil, fmt.Errorf("control client: read the PID file: %w", err)
	}
	if data.Port <= 0 {
		return nil, fmt.Errorf("control client: the daemon is not listening (no port in the PID file)")
	}
	return newControlClient(data.Port, data.Token), nil
}

// newControlClient builds a client for an address already read from the PID
// file, so a caller that loaded it does not read the file twice.
func newControlClient(port int, token string) *ControlClient {
	return &ControlClient{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		token:   token,
		http: &http.Client{
			Timeout: controlRequestTimeout,
			// The control plane is a loopback endpoint the daemon owns: a
			// redirect is a misconfiguration, and following one would carry the
			// bearer token to whatever the response names. The 3xx is returned
			// to the caller and reported as a failed status.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Get requests path from the control plane and returns the response body.
func (c *ControlClient) Get(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path)
}

// Post requests path from the control plane and returns the response body.
func (c *ControlClient) Post(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path)
}

// do sends one authenticated request. A non-2xx status is an error carrying the
// status line and the body, so a caller reports why rather than "no output".
func (c *ControlClient) do(ctx context.Context, method, path string) ([]byte, error) {
	endpoint, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("control client: %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("control client: %s: %w", path, err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control client: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, controlBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("control client: %s: read the response: %w", path, err)
	}
	if len(body) > controlBodyLimit {
		// Reading one byte past the limit tells an oversized answer apart from a
		// large one; a silently truncated body would surface as a parse error.
		return nil, fmt.Errorf("control client: %s: response body exceeds %d bytes",
			path, controlBodyLimit)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("control client: %s: %s: %s", path, resp.Status, describeBody(body))
	}
	return body, nil
}

// describeBody quotes the start of a response body for an error.
func describeBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "no body"
	}
	if len(text) > controlErrorSnippet {
		text = text[:controlErrorSnippet] + "…"
	}
	return text
}
