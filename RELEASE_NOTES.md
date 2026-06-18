# v0.3.0

Install-mode release for aimux/muxcore upgrade acceptance gates.

## Highlights

- New `-install-validation active-pointer` mode validates muxcore successor
  installs where the stable `-binary` path intentionally does not change.
- New `-active-engine-file` flag points the launcher at the active successor
  pointer file, defaulting to `MCPMUX_ACTIVE_ENGINE_FILE`.
- `-env-mode clean` now preserves the aimux smoke isolation/update contract,
  including engine name, session store, warmup, upgrade helper variables, and
  `MCPMUX_ACTIVE_ENGINE_FILE`.
- Install verification keeps waiting for post-exit replacement even when an
  explicit reconnect delay is supplied.
- Cleanup of disposable smoke binaries now runs before replacement/reconnect
  verification when the install handoff needs process cleanup.

## Verification

- `go test ./... -count=1`
- `go test -tags=critical ./tests/critical/... -count=1`
- `go vet .`
- `go build .`
- `mcp-launcher.exe -h`
- `mcp-launcher.exe -binary go -mode compat -compat-report .agent/reports/mcp-launcher-v0.3.0-compat-report.json -- run .\testdata\fake-mcp-server`
- `mcp-launcher.exe -mode hold` (expected `-binary is required` failure)
- `gitleaks detect --source . --no-banner`
- `gitleaks dir . --no-banner`
