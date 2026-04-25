# mcp-launcher

## Project

MCP stdio server launcher for testing graceful restart and session lifecycle.
Emulates Claude Code / Codex spawn behavior. Single Go binary, zero external deps.

## Stack

- **Language:** Go 1.22+
- **Dependencies:** stdlib only (no external modules)
- **Build:** `go build .`

## Architecture

Single-file binary (`main.go`). Three components:

- **mcpClient** — spawns subprocess with piped stdio, manages JSON-RPC 2.0 request/response routing with concurrent notification handling
- **controlSend** — raw Unix domain socket client for daemon control commands (status, graceful-restart)
- **Mode runners** — `hold`, `test`, `phase2` orchestrate the test scenarios

## Key Commands

```bash
go build .                    # build
go vet .                      # lint
./mcp-launcher -binary <srv>  # run (hold mode)
```

## Testing

Manual only — run against a real MCP server:

```bash
./mcp-launcher -binary ./my-server -mode test -ctl /tmp/ctl.sock
./mcp-launcher -binary ./my-server -mode phase2 -ctl /tmp/ctl.sock
```

No unit tests — the tool IS the test.
