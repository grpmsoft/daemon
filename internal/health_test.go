package internal

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// extractPort pulls the port number from an httptest.Server URL ("http://127.0.0.1:PORT").
func extractPort(t *testing.T, rawURL string) int {
	t.Helper()
	// rawURL is "http://127.0.0.1:PORT"
	idx := strings.LastIndex(rawURL, ":")
	if idx == -1 {
		t.Fatalf("no colon in server URL: %s", rawURL)
	}
	portStr := rawURL[idx+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port from %q: %v", portStr, err)
	}
	return port
}

// TestCheckHealth verifies behaviour of the single-shot health checker.
func TestCheckHealth(t *testing.T) {
	t.Run("returns nil for 200 OK", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		err := CheckHealth(extractPort(t, srv.URL), "/health")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("returns nil for 204 No Content", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		err := CheckHealth(extractPort(t, srv.URL), "/health")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("returns error for 500 Internal Server Error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		err := CheckHealth(extractPort(t, srv.URL), "/health")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("%q does not contain %q", err.Error(), "500")
		}
	})

	t.Run("returns error for 404 Not Found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		err := CheckHealth(extractPort(t, srv.URL), "/health")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("%q does not contain %q", err.Error(), "404")
		}
	})

	t.Run("returns error when no server is listening", func(t *testing.T) {
		// Port is bound then released — nothing listens on it.
		freePort, err := FindFreePort()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		err = CheckHealth(freePort, "/health")
		if err == nil {
			t.Fatal("should fail when no server listens on the port")
		}
	})

	t.Run("uses custom health path", func(t *testing.T) {
		const customPath = "/readyz"
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		err := CheckHealth(extractPort(t, srv.URL), customPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotPath != customPath {
			t.Errorf("CheckHealth should GET the specified path, got %v, want %v", gotPath, customPath)
		}
	})
}

// TestWaitUntilReady verifies the polling health-check loop.
func TestWaitUntilReady(t *testing.T) {
	t.Run("succeeds immediately when server is already up", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		start := time.Now()
		err := WaitUntilReady(extractPort(t, srv.URL), "/health", 5*time.Second)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should return in well under 1 second (not wait the full timeout).
		if elapsed >= time.Second {
			t.Errorf("should return quickly when server is already healthy, took %v", elapsed)
		}
	})

	t.Run("returns error when timeout expires before server responds", func(t *testing.T) {
		// Use a port on which nothing listens.
		freePort, err := FindFreePort()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		start := time.Now()
		err = WaitUntilReady(freePort, "/health", 600*time.Millisecond)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("should time out when no server ever becomes ready")
		}
		if elapsed < 500*time.Millisecond {
			t.Errorf("should wait at least close to the timeout, waited %v", elapsed)
		}
		if !strings.Contains(err.Error(), "not ready") {
			t.Errorf("%q does not contain %q", err.Error(), "not ready")
		}
	})

	t.Run("succeeds when server starts with a delay", func(t *testing.T) {
		// Channel controls when the handler starts returning 200.
		ready := make(chan struct{})

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			select {
			case <-ready:
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}))
		defer srv.Close()

		// Close the ready channel after 600ms so the handler returns 200.
		go func() {
			time.Sleep(600 * time.Millisecond)
			close(ready)
		}()

		err := WaitUntilReady(extractPort(t, srv.URL), "/health", 5*time.Second)
		if err != nil {
			t.Fatalf("should succeed once the server becomes ready within the timeout: %v", err)
		}
	})
}
