package daemon

import (
	"errors"
	"path/filepath"
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
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("zero ShutdownTimeout should default to 10s, got %v", cfg.ShutdownTimeout)
	}
}

// TestConfig_ApplyDefaults_DoesNotOverrideNonZero verifies that applyDefaults
// leaves already-set fields unchanged.
func TestConfig_ApplyDefaults_DoesNotOverrideNonZero(t *testing.T) {
	cfg := Config{
		Name:            "test",
		DataDir:         "/tmp",
		Timeout:         60 * time.Second,
		HealthPath:      "/readyz",
		ShutdownTimeout: 20 * time.Second,
	}
	cfg.applyDefaults()

	if cfg.Timeout != 60*time.Second {
		t.Errorf("non-zero Timeout must not be overridden, got %v", cfg.Timeout)
	}
	if cfg.HealthPath != "/readyz" {
		t.Errorf("non-empty HealthPath must not be overridden, got %v", cfg.HealthPath)
	}
	if cfg.ShutdownTimeout != 20*time.Second {
		t.Errorf("non-zero ShutdownTimeout must not be overridden, got %v", cfg.ShutdownTimeout)
	}
}

// TestConfig_ApplyDefaults_NameUnchanged verifies that applyDefaults
// never touches the Name field.
func TestConfig_ApplyDefaults_NameUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "myapp", DataDir: dir}
	cfg.applyDefaults()

	if cfg.Name != "myapp" {
		t.Errorf("got %v, want %v", cfg.Name, "myapp")
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Errorf("DataDir must be absolute after applyDefaults, got %q", cfg.DataDir)
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"valid", Config{Name: "app", DataDir: "/tmp"}, false},
		{"empty name", Config{Name: "", DataDir: "/tmp"}, true},
		{"slash in name", Config{Name: "a/b", DataDir: "/tmp"}, true},
		{"backslash in name", Config{Name: "a\\b", DataDir: "/tmp"}, true},
		{"dot name", Config{Name: ".", DataDir: "/tmp"}, true},
		{"dotdot name", Config{Name: "..", DataDir: "/tmp"}, true},
		{"empty datadir", Config{Name: "app", DataDir: ""}, true},
		{"healthpath root", Config{Name: "app", DataDir: "/tmp", HealthPath: "/"}, true},
		{"healthpath custom", Config{Name: "app", DataDir: "/tmp", HealthPath: "/ready"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && err != nil && !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("error must wrap ErrInvalidConfig, got %v", err)
			}
		})
	}
}

func TestConfig_ApplyDefaults_DirAbsolute(t *testing.T) {
	tests := []struct {
		name    string
		dataDir string
		dir     string
		wantAbs bool
	}{
		{"relative DataDir gets absolute Dir", ".gode", "", true},
		{"absolute DataDir preserved", t.TempDir(), "", true},
		{"explicit Dir made absolute", ".gode", ".custom", true},
		{"explicit absolute Dir preserved", t.TempDir(), t.TempDir(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Name: "test", DataDir: tt.dataDir, Dir: tt.dir}
			cfg.applyDefaults()

			if tt.wantAbs && !filepath.IsAbs(cfg.DataDir) {
				t.Errorf("DataDir must be absolute after applyDefaults, got %q", cfg.DataDir)
			}
			if tt.wantAbs && !filepath.IsAbs(cfg.Dir) {
				t.Errorf("Dir must be absolute after applyDefaults, got %q", cfg.Dir)
			}
			if tt.dir == "" && cfg.Dir != cfg.DataDir {
				t.Errorf("empty Dir must default to DataDir, got Dir=%q DataDir=%q", cfg.Dir, cfg.DataDir)
			}
		})
	}
}
