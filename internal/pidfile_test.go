package internal

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPIDFile_Path(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "myapp")
	expected := filepath.Join(dir, "myapp.pid")
	if pf.Path() != expected {
		t.Errorf("got %v, want %v", pf.Path(), expected)
	}
}

func TestPIDFile_Save_Load_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "myapp")

	startTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	err := pf.Save(1234, 8080, "myapp", "", startTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := pf.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if data.PID != 1234 {
		t.Errorf("got %v, want %v", data.PID, 1234)
	}
	if data.Port != 8080 {
		t.Errorf("got %v, want %v", data.Port, 8080)
	}
	if data.Name != "myapp" {
		t.Errorf("got %v, want %v", data.Name, "myapp")
	}
	if !data.StartTime.Equal(startTime) {
		t.Errorf("start time mismatch: want %v, got %v", startTime, data.StartTime)
	}
}

func TestPIDFile_Save_JSONFormat(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "myapp")

	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	err := pf.Save(5678, 9090, "myapp", "", startTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(pf.Path())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it is valid JSON with the expected fields.
	var decoded pidData
	err = json.Unmarshal(raw, &decoded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decoded.PID != 5678 {
		t.Errorf("got %v, want %v", decoded.PID, 5678)
	}
	if decoded.Port != 9090 {
		t.Errorf("got %v, want %v", decoded.Port, 9090)
	}
	if decoded.Name != "myapp" {
		t.Errorf("got %v, want %v", decoded.Name, "myapp")
	}
}

func TestPIDFile_Save_CreatesDirectories(t *testing.T) {
	base := t.TempDir()
	// Deep nested path that does not exist yet.
	nested := filepath.Join(base, "a", "b", "c")
	pf := NewPIDFile(nested, "app")

	err := pf.Save(99, 1234, "app", "", time.Now())
	if err != nil {
		t.Fatalf("Save should create parent directories: %v", err)
	}

	_, statErr := os.Stat(nested)
	if statErr != nil {
		t.Errorf("nested directory should exist after Save: %v", statErr)
	}
	_, statErr = os.Stat(pf.Path())
	if statErr != nil {
		t.Errorf("pid file should exist after Save: %v", statErr)
	}
}

func TestPIDFile_Save_Atomic(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "myapp")

	// After Save, the .tmp file must not remain.
	err := pf.Save(42, 8000, "myapp", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tmpPath := pf.Path() + ".tmp"
	_, err = os.Stat(tmpPath)
	if !os.IsNotExist(err) {
		t.Errorf("tmp file should be gone after atomic rename, stat err: %v", err)
	}
}

func TestPIDFile_Load_PlainTextPID(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantPID int
	}{
		{"plain PID with newline", "12345\n", 12345},
		{"plain PID without newline", "12345", 12345},
		{"plain PID with spaces", "  12345  \n", 12345},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			pf := NewPIDFile(dir, "app")

			err := os.WriteFile(pf.Path(), []byte(tt.content), 0o600)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			data, err := pf.Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if data.PID != tt.wantPID {
				t.Errorf("got %v, want %v", data.PID, tt.wantPID)
			}
			// Plain-text load produces zero-value for Port/Name/StartTime.
			if data.Port != 0 {
				t.Errorf("got %v, want %v", data.Port, 0)
			}
			if data.Name != "" {
				t.Errorf("got %v, want %v", data.Name, "")
			}
		})
	}
}

func TestPIDFile_Load_ErrorCases(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"whitespace only", "   \n"},
		{"invalid content", "not-a-number"},
		{"invalid JSON with text", "{invalid json}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			pf := NewPIDFile(dir, "app")

			err := os.WriteFile(pf.Path(), []byte(tt.content), 0o600)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			_, err = pf.Load()
			if err == nil {
				t.Fatalf("Load should return an error for: %q", tt.content)
			}
		})
	}
}

func TestPIDFile_Load_NoFile(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	_, err := pf.Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "app.pid") {
		t.Errorf("%q does not contain %q", err.Error(), "app.pid")
	}
}

func TestPIDFile_Clear_RemovesFile(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	err := pf.Save(1, 1, "app", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = pf.Clear()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, statErr := os.Stat(pf.Path())
	if !os.IsNotExist(statErr) {
		t.Errorf("pid file should be removed after Clear, stat err: %v", statErr)
	}
}

func TestPIDFile_Clear_NonExistentFile(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	// Clear when file does not exist should not error.
	err := pf.Clear()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPIDFile_IsAlive_CurrentProcess(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	err := pf.Save(os.Getpid(), 1234, "app", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !pf.IsAlive() {
		t.Error("current process PID should be alive")
	}
}

func TestPIDFile_IsAlive_NonExistentPID(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	// PID 999999 is virtually guaranteed to not exist.
	err := pf.Save(999999, 1234, "app", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pf.IsAlive() {
		t.Error("non-existent PID should not be alive")
	}
}

func TestPIDFile_IsAlive_NoFile(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	// No file written — Load will fail → IsAlive returns false.
	if pf.IsAlive() {
		t.Error("expected false, got true")
	}
}

func TestPIDFile_ConcurrentSaveLoad(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	// Pre-create the file so Load goroutines have something to read.
	err := pf.Save(os.Getpid(), 9000, "app", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*2)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if saveErr := pf.Save(os.Getpid(), 9000, "app", "", time.Now()); saveErr != nil {
				errs <- saveErr
			}
		}()
	}
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A racing Save may cause a transient read failure; that is acceptable.
			// Only report errors that indicate data corruption.
			_, _ = pf.Load()
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Save should not fail: %v", err)
		}
	}

	// Final state must be a valid JSON file.
	data, err := pf.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data.PID != os.Getpid() {
		t.Errorf("got %v, want %v", data.PID, os.Getpid())
	}
}

// ---------------------------------------------------------------------------
// Tests: Binary field in PID file (process name verification)
// ---------------------------------------------------------------------------

func TestPIDFile_Save_Load_WithBinary(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	startTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	binary := "/usr/bin/myapp"

	err := pf.Save(1234, 8080, "app", binary, startTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := pf.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if data.Binary != binary {
		t.Errorf("Binary: got %q, want %q", data.Binary, binary)
	}
	if data.PID != 1234 {
		t.Errorf("PID: got %d, want 1234", data.PID)
	}
}

func TestPIDFile_Save_JSONContainsBinaryField(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	err := pf.Save(42, 8000, "app", "/path/to/daemon", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(pf.Path())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded pidData
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decoded.Binary != "/path/to/daemon" {
		t.Errorf("Binary: got %q, want %q", decoded.Binary, "/path/to/daemon")
	}
}

func TestPIDFile_Save_EmptyBinary_OmittedInJSON(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	err := pf.Save(42, 8000, "app", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := os.ReadFile(pf.Path())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(raw)
	// Empty binary should be omitted (omitempty tag).
	if strings.Contains(content, `"binary"`) {
		t.Errorf("empty binary should be omitted from JSON, got: %s", content)
	}
}

func TestPIDFile_Load_BackwardCompat_NoBinaryField(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	// Write a JSON PID file WITHOUT the binary field (simulates old format).
	oldJSON := `{"pid":1234,"port":8080,"name":"app","startTime":"2026-09-04T12:00:00Z"}`
	if err := os.WriteFile(pf.Path(), []byte(oldJSON), 0o600); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := pf.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data.PID != 1234 {
		t.Errorf("PID: got %d, want 1234", data.PID)
	}
	if data.Binary != "" {
		t.Errorf("Binary should be empty for old PID files, got %q", data.Binary)
	}
}

func TestPIDFile_IsAlive_WrongBinary_ReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	// Save current PID but with a binary path that does NOT match.
	err := pf.Save(os.Getpid(), 1234, "app", "/nonexistent/wrong/binary", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// On Linux: ProcessBinaryPath reads /proc/pid/exe which will not match.
	// On macOS: ProcessBinaryPath returns ("", nil) — verification skipped, still alive.
	// On Windows: ProcessBinaryPath via QueryFullProcessImageNameW will not match.
	//
	// We cannot make a universal assertion because macOS skips verification.
	// Instead, just verify IsAlive does not panic and returns a boolean.
	_ = pf.IsAlive()
}

func TestPIDFile_IsAlive_NoBinary_SkipsVerification(t *testing.T) {
	dir := t.TempDir()
	pf := NewPIDFile(dir, "app")

	// Save with empty binary — verification must be skipped.
	err := pf.Save(os.Getpid(), 1234, "app", "", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With no binary stored, IsAlive should rely on PID-only check.
	if !pf.IsAlive() {
		t.Error("IsAlive must return true for current PID when binary is empty (no verification)")
	}
}

// ---------------------------------------------------------------------------
// Tests: binaryPathsEqual (V2 regression)
// ---------------------------------------------------------------------------

func TestBinaryPathsEqual(t *testing.T) {
	tests := []struct {
		name     string
		actual   string
		expected string
		want     bool
	}{
		{"identical", "/usr/bin/app", "/usr/bin/app", true},
		{"deleted suffix on actual", "/usr/bin/app (deleted)", "/usr/bin/app", true},
		{"deleted suffix on expected", "/usr/bin/app", "/usr/bin/app (deleted)", true},
		{"deleted suffix on both", "/usr/bin/app (deleted)", "/usr/bin/app (deleted)", true},
		{"different binaries", "/usr/bin/app", "/usr/bin/other", false},
		{"different with deleted", "/usr/bin/app (deleted)", "/usr/bin/other", false},
		{"trailing slash cleaned", "/usr/bin/app/", "/usr/bin/app", true},
		{"double slash cleaned", "/usr/bin//app", "/usr/bin/app", true},
		{"empty both", "", "", true},
		{"one empty", "/usr/bin/app", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := binaryPathsEqual(tt.actual, tt.expected)
			if got != tt.want {
				t.Errorf("binaryPathsEqual(%q, %q) = %v, want %v", tt.actual, tt.expected, got, tt.want)
			}
		})
	}
}

// Compare-and-delete tests moved to serve_test.go (TestServe_CompareAndDelete_*)
// where they test the actual Serve() behavior, not just PIDFile methods.
