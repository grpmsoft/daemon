package daemon

import "errors"

var (
	// ErrNotRunning indicates the daemon is not currently running.
	ErrNotRunning = errors.New("daemon: not running")

	// ErrAlreadyRunning indicates a daemon instance is already running.
	ErrAlreadyRunning = errors.New("daemon: already running")

	// ErrStartTimeout indicates the daemon did not become ready within the configured timeout.
	ErrStartTimeout = errors.New("daemon: start timeout")

	// ErrUnhealthy indicates the health check failed after the daemon started.
	ErrUnhealthy = errors.New("daemon: health check failed")

	// ErrStalePIDFile indicates the PID file exists but the daemon is not running.
	ErrStalePIDFile = errors.New("daemon: stale pid file")

	// ErrInvalidConfig indicates the configuration is invalid.
	ErrInvalidConfig = errors.New("daemon: invalid config")

	// ErrStopIntent indicates the daemon was explicitly stopped and
	// EnsureRunning refuses to auto-start it. Use Start() to override.
	ErrStopIntent = errors.New("daemon: explicitly stopped; use Start to restart")

	// ErrSpawnCooldown indicates a recent spawn failure triggered a cooldown
	// period. EnsureRunning returns this error instead of retrying immediately.
	// The cooldown prevents rapid respawn loops when multiple agents call
	// EnsureRunning after a failure.
	ErrSpawnCooldown = errors.New("daemon: spawn cooldown active after recent failure")
)
