# v0.2.0

Feature release adding a local compatibility audit for MCP stdio servers.

## Highlights

- New `-mode compat` runs a profile-aware MCP stdio audit without launching a
  real AI client.
- Default profiles cover generic MCP behavior plus Claude Code-style and
  Codex-style launch envelopes.
- `-compat-level` supports `smoke`, `standard`, `lifecycle`, and `maximum`.
- `-compat-report` writes schema-versioned JSON for CI and release gates.
- Reserved profiles such as `fixture`, `openclaw-registry`, and `hermes` return
  explicit evidence-needed results instead of guessed compatibility claims.

## Verification

- `go test ./... -count=1`
- `go test -tags=critical ./tests/critical/... -count=1`
- `go vet .`
- `go build .`
- `mcp-launcher.exe -h`
- `mcp-launcher.exe -binary go -mode compat -compat-report .agent/reports/mcp-launcher-v0.2.0-compat-report.json -- run .\testdata\fake-mcp-server`
- `mcp-launcher.exe -mode hold` (expected `-binary is required` failure)
- `gitleaks detect --source . --no-banner`
- `gitleaks dir . --no-banner`
