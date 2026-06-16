# Production Testing Playbook

## Scope

This playbook covers the public CLI surface of `mcp-launcher`: source checkout,
local build, help output, compatibility audit smoke, and the documented
missing-argument failure path.

## Prerequisites

- Go 1.22 or newer.
- A clean checkout of `github.com/thebtf/mcp-launcher`.
- PowerShell on Windows or a POSIX shell on macOS/Linux.

No external MCP server is required for the release smoke in this playbook.

## Scenarios

### 1. Build from source

**Run:**

```bash
go test ./...
go build .
```

**Expected:**

- Tests exit 0.
- Build exits 0.
- The local `mcp-launcher` binary is created.

### 2. Inspect CLI help

**Run:**

```bash
./mcp-launcher -h
```

On Windows:

```powershell
.\mcp-launcher.exe -h
```

**Expected:**

- Command exits 0.
- Help lists the documented modes:
  `hold`, `call`, `tool`, `resource`, `install`, `test`, `phase2`, `persist`,
  `kill-reconnect`, and `compat`.
- Help lists `-compat-level`, `-compat-profiles`, and `-compat-report`.
- `-ctl` help says it is required for `test`, `phase2`, `persist`, and
  `kill-reconnect`.

### 3. Run compatibility audit smoke

**Run:**

```bash
go run . -binary go -mode compat -compat-report compat-report.json -- run ./testdata/fake-mcp-server
```

On Windows:

```powershell
go run . -binary go -mode compat -compat-report compat-report.json -- run .\testdata\fake-mcp-server
```

**Expected:**

- Command exits 0.
- Console output includes `overall=PASS`.
- `compat-report.json` exists.
- JSON contains `schema_version`, `overall_status`, `profiles`, and `checks`.
- Default profiles include `generic`, `claude-code`, and `codex`.

### 4. Missing binary fails clearly

**Run:**

```bash
./mcp-launcher -mode hold
```

On Windows:

```powershell
.\mcp-launcher.exe -mode hold
```

**Expected:**

- Command exits non-zero.
- Output contains `error: -binary is required`.
- Usage text is printed.

## Verdict Template

| # | Scenario | Ran | Expected | Observed | Verdict |
| --- | --- | --- | --- | --- | --- |
| 1 | Build from source |  |  |  |  |
| 2 | Inspect CLI help |  |  |  |  |
| 3 | Run compatibility audit smoke |  |  |  |  |
| 4 | Missing binary fails clearly |  |  |  |  |

Overall verdict: `PRODUCT_WORKS`, `PARTIALLY_WORKS`, or `BROKEN`.
