# ADR-003: Lease-Based Connection Tracking and Control-Plane Token

**Status:** Accepted
**Date:** 2026-09-12
**Release:** v0.3.1

## Context

Two defects remain in the connection layer after v0.3.0:

**D1 — Counting cannot survive a client crash.** `ConnTracker` counts
`POST /daemon/connect` and `POST /daemon/disconnect`. A proxy killed by
SIGKILL, a closed terminal, or an OOM never sends `disconnect`; the count
stays > 0 and a daemon with `IdleTimeout` never exits. Clamping (v0.1.1)
and CAS (v0.2.0) fixed negative-count ordering; the positive-count leak
is inherent to counting.

**S2 — Unauthenticated control plane.** Any local process (or a page via
DNS rebinding that passes the `Host` check) can `POST /daemon/shutdown`.
The PID file is created `0600` by `pidlock.TryLock`, so a secret inside
it is readable only by the owner -- that is the auth boundary.

## Decision

### Lease-based attach (D1)

New endpoint `GET /daemon/attach`. The handler calls `ct.Connect()`,
writes headers + one ack byte, flushes, then blocks until
`r.Context().Done()` (client TCP close) or daemon shutdown. Defer calls
`ct.Disconnect()`.

The TCP connection IS the lease: when the client process dies, the kernel
closes the socket, `r.Context()` is cancelled, the count drops. No
timers, no heartbeats.

`http.Server.BaseContext` is wired to a cancellable context. The server's
interrupt function calls `cancelBase()` before `server.Shutdown()`, which
cancels all in-flight request contexts and unblocks attach handlers
immediately.

`Proxy` tries `GET /daemon/attach` first; if the daemon returns 404
(v0.3.0 daemon), falls back to `POST /daemon/connect` + `/disconnect`.

### Bearer token (S2)

`Serve()` generates `token := rand.Text()` (crypto/rand, Go 1.24+) and
writes it to the PID file. `authGuard` middleware requires
`Authorization: Bearer <token>` for all `/daemon/*` endpoints.
Constant-time comparison via `crypto/subtle.ConstantTimeCompare`.
`/health` stays unauthenticated.

`Config.RequireToken bool` (default false): when true, the token is also
required for the application handler.

Clients (`Proxy`, `stopLocked`) read the token from the PID file and
send it in the Authorization header. If the PID file has no token (v0.3.0
daemon), no header is sent.

## Mixed-Version Matrix

| Proxy | Daemon | Behavior |
|-------|--------|----------|
| v0.3.1 | v0.3.1 | Lease attach + bearer token (optimal) |
| v0.3.1 | v0.3.0 | Fallback to connect/disconnect, no token (compatible) |
| v0.3.0 | v0.3.1 | connect/disconnect returns 401 -- **upgrade proxy** |
| v0.3.0 | v0.3.0 | connect/disconnect, no token (unchanged) |

## Alternatives Rejected

**Heartbeats.** TCP keepalive or application-level pings add complexity,
timers, and tuning knobs. The OS already detects dead connections --
lease leverages that for zero-config crash survival.

**SSE (Server-Sent Events).** More complex protocol for a problem solved
by a raw TCP connection. SSE adds Content-Type negotiation, retry logic,
and event parsing for no benefit.

**mTLS.** Massive complexity (certificate generation, rotation, CA trust)
for a localhost-only daemon. Bearer token in a 0600 file provides the
same security boundary with zero infrastructure.

**Token rotation.** Not needed for a single-user localhost daemon. The
token lives as long as the process and is regenerated on restart.

## Consequences

- Crashed proxies no longer leak connection count
- Idle auto-shutdown works reliably with any client failure mode
- Control-plane endpoints are protected from unauthorized access
- v0.3.0 proxy cannot authenticate with v0.3.1 daemon (upgrade required)
- Zero new dependencies (stdlib only)
- Token never appears in logs (LogPayloads logs bodies, never headers)
