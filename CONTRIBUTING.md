# Contributing to daemon

Thank you for your interest in contributing to daemon! This document provides
guidelines for contributing to this project.

## Prerequisites

- **Go 1.27+** -- matches grpmsoft ecosystem requirements
- **golangci-lint** -- `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`

## Building

```bash
go build ./...
```

## Testing

```bash
# Unit tests
go test ./...

# With race detector (requires CGO_ENABLED=1)
CGO_ENABLED=1 go test -race ./...

# With coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

## Code Style

- Format with `gofmt` (enforced in CI)
- Pass `golangci-lint run --timeout=5m`
- Pass `go vet ./...`
- Add comments for non-obvious logic -- explain WHY, not WHAT
- All public types and functions must have doc comments
- No `unsafe` -- this library is intentionally pure safe Go
- No external dependencies -- stdlib only

## Pull Requests

1. Fork the repository and create a feature branch
2. Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages:
   - `feat:` new features
   - `fix:` bug fixes
   - `perf:` performance improvements
   - `test:` test additions/changes
   - `docs:` documentation
   - `refactor:` code restructuring
3. Ensure all checks pass: `go build ./... && go test ./... && golangci-lint run --timeout=5m`
4. Open a PR against `main`

AI-assisted contributions are welcome. For guidance on effective AI-assisted
development practices, see the
[Smart Coding](https://github.com/gogpu/wgpu/blob/main/CONTRIBUTING.md#smart-coding)
section in the wgpu CONTRIBUTING.md.

## License

By contributing, you agree that your contributions will be licensed under the
MIT License.
