# mcp-launcher

MCP stdio server launcher that emulates how Claude Code and Codex spawn MCP servers as subprocesses. Use it to test graceful restart, hot-swap handoff, session lifecycle, and MCP protocol compliance without running a live AI client.

## Install

```bash
go install github.com/thebtf/mcp-launcher@latest
```

Or build from source:

```bash
git clone https://github.com/thebtf/mcp-launcher
cd mcp-launcher
go build .
```

Zero external dependencies — stdlib only.

## Usage

```
mcp-launcher -binary <server> [options]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-binary` | (required) | MCP server executable path |
| `-cwd` | `.` | Working directory for the subprocess |
| `-mode` | `hold` | Mode: `hold`, `call`, `tool`, `resource`, `install`, `test`, `phase2`, `persist`, or `kill-reconnect` |
| `-hold` | `300` | How long to hold the session in seconds (hold mode) |
| `-watch` | `60` | How long to watch the daemon after disconnect in seconds (persist mode) |
| `-ctl` | (required for test/phase2/persist) | Daemon control socket path |
| `-daemon-flag` | `--muxcore-daemon` | Flag to start server in daemon mode |
| `-env-mode` | `full` | `full` (CC-style, inherit all env) or `clean` (Codex-style, platform allow-list) |
| `-timeout` | `120` | MCP request timeout in seconds, including initialize and tools/list |
| `-expect-tools` | `0` | Expected `tools/list` count after session init; `0` disables the check |
| `-expect-version` | (empty) | Expected MCP `serverInfo.version` after session init |
| `-method` | (empty) | JSON-RPC method for `call` mode |
| `-params` | `{}` | JSON params for `call` mode |
| `-tool` | (empty) | MCP tool name for `tool` mode |
| `-args` | `{}` | JSON object arguments for `tool` mode |
| `-uri` | (empty) | MCP resource URI for `resource` mode |
| `-source` | (empty) | Local source binary for `install` mode |
| `-force` | `false` | Force `upgrade(action=apply)` in `install` mode |
| `-upgrade-mode` | `auto` | Upgrade mode for `install`: `auto`, `hot_swap`, or `deferred` |
| `-reconnect-delay` | `2` | Seconds to wait before `install` mode reconnect verification |

### Modes

#### `hold` — keep session alive for external testing

Spawns the server, completes the MCP handshake, and holds the session open. Use an external tool (e.g., `ctltest`) to trigger graceful-restart while a real owner/session exists.

```bash
mcp-launcher -binary ./my-server -mode hold -hold 600
```

#### `call` — call any JSON-RPC method

Spawns the server, completes the MCP handshake, and calls the JSON-RPC method passed via `-method` with JSON params from `-params`.

```bash
mcp-launcher -binary ./my-server -mode call -method tools/list -params '{}'
```

#### `tool` — call any MCP tool

Calls `tools/call` with a tool name and JSON object arguments, then prints both the raw MCP response and the decoded text payload when the tool returns JSON text.

```bash
mcp-launcher -binary ./aimux-dev.exe -mode tool -tool sessions -args '{"action":"health"}'
```

#### `resource` — read any MCP resource

Calls `resources/read` for the URI passed via `-uri`, then prints both the raw MCP response and the decoded text payload when the resource returns JSON text.

```bash
mcp-launcher -binary ./aimux-dev.exe -mode resource -uri aimux://health
```

#### `install` — install a local binary through the MCP upgrade tool

Emulates a project-scoped MCP client that can call `upgrade(action="apply", source=..., force=true)`. The mode starts the installed binary, calls the `upgrade` tool with `-source`, closes stdio so deferred restarts can complete, reconnects, then verifies `sessions(action="health")` and `aimux://health`.

```bash
mcp-launcher \
  -binary ./aimux-dev.exe \
  -cwd /path/to/aimux \
  -mode install \
  -source ./aimux-dev-next.exe \
  -force \
  -expect-tools 27
```

#### `test` — single-phase graceful-restart

Starts a daemon, creates a session with a real owner, triggers graceful-restart via the control socket, and verifies the response is delivered and the new daemon is healthy.

```bash
mcp-launcher -binary ./my-server -mode test -ctl /tmp/my-ctl.sock
```

#### `phase2` — two-phase restart (deadlock reproducer)

The full test: Phase 1 restarts the original daemon (creating a successor), then Phase 2 restarts the successor. This reproduces the specific scenario where a `d.mu` deadlock occurs when owners are detached during handoff.

```bash
mcp-launcher -binary ./my-server -mode phase2 -ctl /tmp/my-ctl.sock
```

#### `persist` — verify daemon survives stdio disconnect

Starts the daemon, opens Session A, closes stdio cleanly to simulate a Ctrl+C disconnect, polls daemon liveness for `-watch` seconds, then reconnects with Session B. `PASS` means the daemon PID stayed alive for the full watch window and Session B reattached to the same PID. `FAIL` means the daemon died or reconnect spawned a new daemon.

```bash
mcp-launcher -binary ./my-server -mode persist -ctl /tmp/my-ctl.sock -watch 60
```

## How it maps to real clients

### Claude Code spawn behavior

CC uses `StdioClientTransport` from `@modelcontextprotocol/sdk`:
- Full `process.env` passed to subprocess
- `stdin`/`stdout` piped, `stderr` piped (buffered)
- Initialize with `clientInfo.name = "claude-code"`, capabilities: `roots`, `elicitation`
- 30s connection timeout

mcp-launcher reproduces this with `-env-mode full` (default).

### Codex spawn behavior

Codex uses `tokio::process::Command` with `env_clear()`:
- Only platform allow-list vars forwarded (PATH, HOME, TEMP, etc.)
- `kill_on_drop(true)`, `process_group(0)` on Unix
- Initialize with `clientInfo.name = "codex-mcp-client"`, capabilities: `elicitation`

mcp-launcher reproduces this with `-env-mode clean`.

## MCP protocol sequence

The launcher performs the standard MCP handshake:

```
launcher → server: initialize (protocolVersion, clientInfo, capabilities)
server → launcher: initialize result (serverInfo, capabilities)
launcher → server: notifications/initialized
launcher → server: tools/list
server → launcher: tools/list result
```

After the handshake, the session is live. The server has a real owner with piped stdio — identical to a CC or Codex session.

## Example: testing aimux graceful restart

```bash
# Kill any existing daemons
taskkill /IM aimux.exe /F 2>nul

# Run the full two-phase test
mcp-launcher \
  -binary ./aimux.exe \
  -cwd /path/to/aimux \
  -mode phase2 \
  -ctl "$TEMP/aimux-muxd.ctl.sock"

# Verify Persistent=true survives a stdio disconnect
mcp-launcher \
  -binary ./aimux.exe \
  -cwd /path/to/aimux \
  -mode persist \
  -ctl "$TEMP/aimux-muxd.ctl.sock" \
  -watch 30
```

Expected output:
```
[phase2] Full test: Phase 1 bootstrap + Phase 2 on successor
  daemon pid=12345
  spawn ./aimux.exe (cwd=..., env=full)
  pid=12346
  initialize: {"jsonrpc":"2.0","id":1,"result":{...}}
  tools: N

--- Phase 1: graceful-restart ---
  Phase 1 OK: snapshot written, shutting down (133ms)

--- Phase 2: graceful-restart on successor ---
  Phase 2 OK: snapshot written, shutting down (132ms)
  final: owner_count=0 handoff=map[attempted:1 ...]
[phase2] Done.
```

## License

MIT
