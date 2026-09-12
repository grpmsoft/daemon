package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// Tests: S1 — loopbackGuard middleware
//
// loopbackGuard must allow requests with loopback Host headers and reject
// requests from non-loopback hosts. Additionally, a valid Host with a
// non-loopback Origin header must be rejected (DNS-rebinding defence).
// ---------------------------------------------------------------------------

// sentinel handler used as the wrapped next handler.
// Records whether it was called so tests can assert pass-through behaviour.
type sentinelHandler struct {
	called bool
}

func (s *sentinelHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.called = true
	w.WriteHeader(http.StatusOK)
}

func TestLoopbackGuard(t *testing.T) {
	const port = 8080

	tests := []struct {
		name            string
		host            string
		origin          string // empty means no Origin header
		wantStatus      int
		wantPassThrough bool // true when the sentinel next handler must be reached
	}{
		{
			name:            "loopback IP host — allowed",
			host:            "127.0.0.1:8080",
			wantStatus:      http.StatusOK,
			wantPassThrough: true,
		},
		{
			name:            "loopback localhost host — allowed",
			host:            "localhost:8080",
			wantStatus:      http.StatusOK,
			wantPassThrough: true,
		},
		{
			name:            "loopback IP without port — allowed",
			host:            "127.0.0.1",
			wantStatus:      http.StatusOK,
			wantPassThrough: true,
		},
		{
			name:            "localhost without port — allowed",
			host:            "localhost",
			wantStatus:      http.StatusOK,
			wantPassThrough: true,
		},
		{
			name:            "external host — rejected",
			host:            "evil.com:8080",
			wantStatus:      http.StatusForbidden,
			wantPassThrough: false,
		},
		{
			name:            "IP external host — rejected",
			host:            "192.168.1.1:8080",
			wantStatus:      http.StatusForbidden,
			wantPassThrough: false,
		},
		{
			name:            "no host header — rejected",
			host:            "",
			wantStatus:      http.StatusForbidden,
			wantPassThrough: false,
		},
		{
			name:            "loopback IP host with non-loopback origin — rejected",
			host:            "127.0.0.1:8080",
			origin:          "https://evil.com",
			wantStatus:      http.StatusForbidden,
			wantPassThrough: false,
		},
		{
			name:            "loopback IP host with loopback IP origin — allowed",
			host:            "127.0.0.1:8080",
			origin:          "http://127.0.0.1:8080",
			wantStatus:      http.StatusOK,
			wantPassThrough: true,
		},
		{
			name:            "loopback IP host with localhost origin — allowed",
			host:            "127.0.0.1:8080",
			origin:          "http://localhost:8080",
			wantStatus:      http.StatusOK,
			wantPassThrough: true,
		},
		{
			name:            "loopback IP host with no origin header — allowed",
			host:            "127.0.0.1:8080",
			origin:          "",
			wantStatus:      http.StatusOK,
			wantPassThrough: true,
		},
		{
			name:            "localhost host with non-loopback origin — rejected",
			host:            "localhost:8080",
			origin:          "https://attacker.example.com",
			wantStatus:      http.StatusForbidden,
			wantPassThrough: false,
		},
		{
			name:            "localhost host with localhost origin — allowed",
			host:            "localhost:8080",
			origin:          "http://localhost:4200",
			wantStatus:      http.StatusOK,
			wantPassThrough: true,
		},
		// V4 bypass attack vectors — crafted origins that previously passed strings.Contains.
		{
			name:            "origin bypass: 127.0.0.1.evil.com — rejected",
			host:            "127.0.0.1:8080",
			origin:          "http://127.0.0.1.evil.com",
			wantStatus:      http.StatusForbidden,
			wantPassThrough: false,
		},
		{
			name:            "origin bypass: localhost.evil.com — rejected",
			host:            "127.0.0.1:8080",
			origin:          "http://localhost.evil.com",
			wantStatus:      http.StatusForbidden,
			wantPassThrough: false,
		},
		{
			name:            "origin bypass: query param with ://localhost — rejected",
			host:            "127.0.0.1:8080",
			origin:          "http://evil.com/?x=://localhost",
			wantStatus:      http.StatusForbidden,
			wantPassThrough: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &sentinelHandler{}
			guard := loopbackGuard(next, port)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			rec := httptest.NewRecorder()
			guard.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status: got %d, want %d (host=%q origin=%q)",
					rec.Code, tt.wantStatus, tt.host, tt.origin)
			}
			if next.called != tt.wantPassThrough {
				t.Errorf("next handler called=%v, want %v (host=%q origin=%q)",
					next.called, tt.wantPassThrough, tt.host, tt.origin)
			}
		})
	}
}

// TestLoopbackGuard_RejectBodyContainsForbiddenText verifies that rejected
// responses include a human-readable explanation (useful for debugging).
func TestLoopbackGuard_RejectBodyContainsForbiddenText(t *testing.T) {
	guard := loopbackGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 9999)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.com:9999"

	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	body := rec.Body.String()
	if body == "" {
		t.Error("rejected response must contain a non-empty body explaining the denial")
	}
}

// TestLoopbackGuard_PortIsolation verifies that the guard built for port X
// does not accept requests with Host pointing to port Y.
func TestLoopbackGuard_PortIsolation(t *testing.T) {
	// Guard built for port 7777.
	guard := loopbackGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 7777)

	tests := []struct {
		name   string
		host   string
		wantOK bool
	}{
		{"correct port", "127.0.0.1:7777", true},
		{"wrong port", "127.0.0.1:8888", false},
		{"no port — bare IP", "127.0.0.1", true},   // bare IP without port is in allowed set
		{"no port — localhost", "localhost", true}, // bare localhost is in allowed set
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			guard.ServeHTTP(rec, req)

			if tt.wantOK && rec.Code != http.StatusOK {
				t.Errorf("want 200, got %d (host=%q)", rec.Code, tt.host)
			}
			if !tt.wantOK && rec.Code != http.StatusForbidden {
				t.Errorf("want 403, got %d (host=%q)", rec.Code, tt.host)
			}
		})
	}
}
