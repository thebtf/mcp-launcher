# v0.1.0

Initial public release of `mcp-launcher`, a zero-dependency Go CLI for testing
stdio MCP server lifecycle behavior without opening a full AI client.

## Highlights

- Start a real MCP stdio owner session with `hold`.
- Probe JSON-RPC methods, MCP tools, and MCP resources from the command line.
- Exercise daemon restart, two-phase handoff, persistence, and hard-kill
  reconnect scenarios.
- Verify local binary upgrade flows with `install`, including deferred
  post-exit replacement waits and health checks.
- Run smoke gates with expected tool count and server version assertions.

## Verification

- `go test ./... -count=1`
- `go vet .`
- `go build .`
- `mcp-launcher.exe -h`
- `gitleaks` history and filesystem scans
