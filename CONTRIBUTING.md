# Contributing to mcp-launcher

Thanks for helping make MCP server lifecycle testing less mysterious.

## Setup

```bash
git clone https://github.com/thebtf/mcp-launcher.git
cd mcp-launcher
go test ./...
go build .
```

The project uses Go 1.22+ and the standard library only.

## Local Checks

Run these before opening a pull request:

```bash
go test ./...
go vet .
go build .
```

For behavior that depends on a real MCP server, also run the relevant launcher
mode against a disposable target binary:

```bash
./mcp-launcher -binary ./my-server -mode hold -hold 30
./mcp-launcher -binary ./my-server -mode tool -tool sessions -args '{"action":"health"}'
```

## Code Style

- Keep the binary dependency-free unless there is a strong reason to add a
  module dependency.
- Prefer small, testable helpers for lifecycle decisions such as reconnect
  timing, cleanup fallback, and payload classification.
- Keep stdout for MCP-visible JSON-RPC when writing fake test servers; diagnostic
  logs belong on stderr.
- Use platform-specific behavior deliberately and cover it with tests where the
  behavior can be simulated.

## Adding or Changing Modes

1. Add the mode to the CLI help and `-mode` flag description.
2. Implement the mode runner in `main.go`.
3. Document the mode in `README.md`.
4. Add a focused test for pure decision logic.
5. Run a manual smoke test against a real MCP server when subprocess lifecycle
   behavior changes.

## Pull Requests

- Branch from the default branch.
- Keep one logical change per PR.
- Use clear commit messages such as `feat: add resource mode` or
  `fix: wait for post-exit install replacement`.
- Include the exact commands you ran and the target platform.
- Avoid committing local `.agent/`, `.serena/`, security scan, or continuity
  artifacts.

## Reporting Bugs

Please include:

- OS and shell.
- `go version`.
- `mcp-launcher` command line.
- Target server name/version when available.
- Expected behavior and actual behavior.
- Full launcher output with secrets redacted.

## Requesting Features

Describe the lifecycle scenario you need to test, the MCP server behavior you
expect, and why the existing modes do not cover it.
