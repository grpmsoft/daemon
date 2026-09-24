package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Status represents the lifecycle state of a daemon process.
type Status int

// Status constants representing the lifecycle state of a daemon process.
const (
	StatusStopped  Status = iota // not running, no PID file
	StatusStarting               // process spawned, health check pending
	StatusRunning                // healthy and responding
	StatusStopping               // shutdown in progress
	StatusError                  // PID file exists but process is dead or unhealthy
)

// String returns a human-readable representation of the status.
func (s Status) String() string {
	switch s {
	case StatusStopped:
		return "stopped"
	case StatusStarting:
		return "starting"
	case StatusRunning:
		return "running"
	case StatusStopping:
		return "stopping"
	case StatusError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// Config controls daemon lifecycle behavior. Consumers provide this when
// creating a Daemon instance.
type Config struct {
	// Name identifies the application (e.g. "gode", "goda").
	// Used for PID file naming and health response.
	Name string `json:"name"`

	// DataDir is the directory for runtime files (PID, logs), relative to
	// the workspace root. Example: ".gode".
	DataDir string `json:"dataDir"`

	// Binary is the executable path for the daemon process.
	// Empty means os.Executable() (current binary).
	Binary string `json:"binary"`

	// Args are the arguments passed to the child process in Start/Restart.
	Args []string `json:"args"`

	// Timeout is how long Start() waits for the health check to pass.
	// Zero means 30 seconds.
	Timeout time.Duration `json:"timeout"`

	// HealthPath is the HTTP path for the health endpoint.
	// Zero value means "/health".
	HealthPath string `json:"healthPath"`

	// IdleTimeout is how long the daemon waits with zero active connections
	// before initiating a graceful shutdown. Zero means never auto-shutdown.
	IdleTimeout time.Duration `json:"idleTimeout"`

	// Dir is the working directory for the spawned daemon process.
	// Empty means DataDir. Prevents the daemon from holding the parent's
	// CWD (which may be deleted or unmounted after start).
	Dir string `json:"dir"`

	// DisableTokenAuth, when true, skips bearer token authentication for
	// the application handler (everything outside /health and /daemon/*).
	// Default false = token required (secure by default). Set to true only
	// for local curl debugging.
	//
	// Breaking change v0.4.0: replaces RequireToken (inverted semantics).
	// Migration: RequireToken: true -> remove field (default secure).
	//            RequireToken: false -> DisableTokenAuth: true.
	DisableTokenAuth bool `json:"disableTokenAuth"`

	// SpawnCooldown is the duration to wait after a spawn failure before
	// retrying. During cooldown, EnsureRunning returns ErrSpawnCooldown
	// instead of attempting another spawn. Prevents rapid respawn loops
	// when multiple MCP agents call EnsureRunning after a spawn failure.
	// Default: 5s. Set to -1 to disable.
	SpawnCooldown time.Duration `json:"spawnCooldown"`

	// ShutdownTimeout is the total time budget for graceful shutdown.
	// stopLocked divides this budget across its phases:
	//   - HTTP shutdown request: ShutdownTimeout / 2
	//   - Wait for lock release: ShutdownTimeout
	//   - Kill grace period:     ShutdownTimeout / 2
	// Also used for kill grace in startWithLock failure cleanup.
	// Default: 10s.
	ShutdownTimeout time.Duration `json:"shutdownTimeout,omitempty"`
}

// Validate checks the configuration for invalid values.
// Name must be a single path element (no slashes). DataDir must be non-empty.
// HealthPath must not conflict with reserved daemon paths.
func (c *Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("%w: Name is required", ErrInvalidConfig)
	}
	if strings.ContainsAny(c.Name, "/\\") || c.Name == "." || c.Name == ".." {
		return fmt.Errorf("%w: Name %q must be a single path element", ErrInvalidConfig, c.Name)
	}
	if c.DataDir == "" {
		return fmt.Errorf("%w: DataDir is required", ErrInvalidConfig)
	}
	if c.HealthPath == "/" {
		return fmt.Errorf("%w: HealthPath %q conflicts with root handler", ErrInvalidConfig, c.HealthPath)
	}
	if strings.HasPrefix(c.HealthPath, "/daemon/") {
		return fmt.Errorf("%w: HealthPath %q conflicts with daemon control endpoints", ErrInvalidConfig, c.HealthPath)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.DataDir != "" {
		if abs, err := filepath.Abs(c.DataDir); err == nil {
			c.DataDir = abs
		}
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.HealthPath == "" {
		c.HealthPath = "/health"
	}
	if c.Binary == "" {
		bin, _ := os.Executable()
		c.Binary = bin
	}
	if c.Dir == "" {
		c.Dir = c.DataDir
	}
	if c.Dir != "" {
		if abs, err := filepath.Abs(c.Dir); err == nil {
			c.Dir = abs
		}
	}
	if c.SpawnCooldown == 0 {
		c.SpawnCooldown = 5 * time.Second
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 10 * time.Second
	}
}

// Info is the status snapshot returned by Daemon.Status().
type Info struct {
	Status    Status        `json:"status"`
	PID       int           `json:"pid"`
	Port      int           `json:"port"`
	Name      string        `json:"name"`
	StartTime time.Time     `json:"startTime"`
	Uptime    time.Duration `json:"uptime"`
}

// PIDStore abstracts PID file persistence. The Daemon model depends on this
// interface, not on a concrete file-based implementation.
// PIDInfo is returned by Load() with the stored daemon state.
type PIDStore interface {
	Save(pid, port int, name, binary string, startTime time.Time) error
	Load() (PIDInfo, error)
	Clear() error
	IsAlive() bool
	Path() string
}

// PIDInfo holds the persisted state of a running daemon, as returned by PIDStore.Load().
type PIDInfo struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Name      string    `json:"name"`
	Binary    string    `json:"binary"`
	StartTime time.Time `json:"startTime"`
	Token     string    `json:"token,omitempty"`
}

// StartSpec describes how to spawn a daemon child process.
type StartSpec struct {
	Binary     string     // executable path
	Args       []string   // command-line arguments
	Dir        string     // working directory
	Env        []string   // environment variables
	LogFile    string     // stderr/stdout log file path
	ExtraFiles []*os.File // additional file descriptors (e.g., PID lock fd)
}

// ProcessManager abstracts platform-specific process lifecycle operations.
type ProcessManager interface {
	Start(ctx context.Context, spec StartSpec) (pid int, err error)
	KillProcess(ctx context.Context, pid int, grace time.Duration) error
	IsProcessAlive(pid int) bool
}

// HealthChecker abstracts daemon readiness verification.
type HealthChecker interface {
	Check(port int, healthPath string) error
	WaitUntilReady(ctx context.Context, port int, healthPath string, timeout time.Duration) error
}
