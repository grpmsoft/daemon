# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in daemon, please report it privately
through [GitHub Security Advisories](https://github.com/grpmsoft/daemon/security/advisories/new).

**Do not open a public issue for security vulnerabilities.**

## Response

We commit to:

- Acknowledging your report within **48 hours**
- Providing an initial assessment within **5 business days**
- Releasing a fix as soon as practical, coordinated with you

## Scope

daemon is a pure-Go cross-platform daemon lifecycle library with no external
dependencies and no CGO. The only use of `unsafe` is in `internal/process_windows.go`
for the Win32 `QueryFullProcessImageNameW` API call. Security concerns include:

- **PID file race conditions** -- concurrent daemon startup could lead to stale
  PID files or duplicate instances
- **Process spawning** -- detached process creation with environment variables
  and log file paths
- **HTTP proxy request handling** -- the stdio-to-HTTP proxy forwards JSON-RPC
  messages; crafted input could cause unexpected behavior
- **Signal handling** -- SIGINT/SIGTERM handling for graceful shutdown
- **Connection tracking** -- active connection counting for idle auto-shutdown

## Supported Versions

Only the latest release receives security updates.
