package daemon

import (
	"context"
	"fmt"
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
	Name string

	// DataDir is the directory for runtime files (PID, logs), relative to
	// the workspace root. Example: ".gode".
	DataDir string

	// Timeout is how long Start() waits for the health check to pass.
	// Zero means 30 seconds.
	Timeout time.Duration

	// HealthPath is the HTTP path for the health endpoint.
	// Zero value means "/health".
	HealthPath string

	// IdleTimeout is how long the daemon waits with zero active connections
	// before initiating a graceful shutdown. Zero means never auto-shutdown.
	IdleTimeout time.Duration
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
	return nil
}

func (c *Config) applyDefaults() {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.HealthPath == "" {
		c.HealthPath = "/health"
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
}

// ProcessManager abstracts platform-specific process lifecycle operations.
type ProcessManager interface {
	StartDetached(binary string, args []string, logFile string, env []string) (pid int, err error)
	KillProcess(ctx context.Context, pid int, grace time.Duration) error
	IsProcessAlive(pid int) bool
}

// HealthChecker abstracts daemon readiness verification.
type HealthChecker interface {
	Check(port int, healthPath string) error
	WaitUntilReady(port int, healthPath string, timeout time.Duration) error
}
