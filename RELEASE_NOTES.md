# v0.1.1

Patch release for install-mode smoke reliability when testing aimux under a
clean launcher environment.

## Highlights

- `-env-mode clean` now preserves `AIMUX_STDIN_EOF_POLICY` when the parent
  process sets it.
- Aimux install smokes can keep the eager stdin EOF contract needed for
  post-exit shim cleanup and binary replacement.
- Added a focused regression test for the clean environment preservation
  contract.

## Verification

- `go test -run TestCleanEnvPreservesAimuxStdinEOFPolicy -count=1`
- `go test ./... -count=1`
- `go test -tags=critical ./tests/critical/... -count=1`
- `go vet .`
- `go build .`
- `mcp-launcher.exe -h`
- `mcp-launcher.exe -mode hold` (expected `-binary is required` failure)
- `gitleaks detect --source . --no-banner`
- `gitleaks dir . --no-banner`
