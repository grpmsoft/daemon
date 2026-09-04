package daemon

import (
	"testing"
	"time"
)

// TestStatus_String verifies human-readable names for all Status values.
func TestStatus_String(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusStopped, "stopped"},
		{StatusStarting, "starting"},
		{StatusRunning, "running"},
		{StatusStopping, "stopping"},
		{StatusError, "error"},
		{Status(99), "unknown(99)"},
		{Status(-1), "unknown(-1)"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.status.String()
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestConfig_ApplyDefaults_SetsZeroValues verifies that applyDefaults fills in
// zero-value fields with their documented defaults.
func TestConfig_ApplyDefaults_SetsZeroValues(t *testing.T) {
	cfg := Config{Name: "test", DataDir: "/tmp"}
	cfg.applyDefaults()

	if cfg.Timeout != 30*time.Second {
		t.Errorf("zero Timeout should default to 30s, got %v", cfg.Timeout)
	}
	if cfg.HealthPath != "/health" {
		t.Errorf("empty HealthPath should default to /health, got %v", cfg.HealthPath)
	}
}

// TestConfig_ApplyDefaults_DoesNotOverrideNonZero verifies that applyDefaults
// leaves already-set fields unchanged.
func TestConfig_ApplyDefaults_DoesNotOverrideNonZero(t *testing.T) {
	cfg := Config{
		Name:       "test",
		DataDir:    "/tmp",
		Timeout:    60 * time.Second,
		HealthPath: "/readyz",
	}
	cfg.applyDefaults()

	if cfg.Timeout != 60*time.Second {
		t.Errorf("non-zero Timeout must not be overridden, got %v", cfg.Timeout)
	}
	if cfg.HealthPath != "/readyz" {
		t.Errorf("non-empty HealthPath must not be overridden, got %v", cfg.HealthPath)
	}
}

// TestConfig_ApplyDefaults_NameAndDataDirUnchanged verifies that applyDefaults
// never touches fields it does not own.
func TestConfig_ApplyDefaults_NameAndDataDirUnchanged(t *testing.T) {
	cfg := Config{Name: "myapp", DataDir: "/var/run/myapp"}
	cfg.applyDefaults()

	if cfg.Name != "myapp" {
		t.Errorf("got %v, want %v", cfg.Name, "myapp")
	}
	if cfg.DataDir != "/var/run/myapp" {
		t.Errorf("got %v, want %v", cfg.DataDir, "/var/run/myapp")
	}
}
