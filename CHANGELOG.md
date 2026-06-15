# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog, and this project uses semantic version
tags for public releases.

## [Unreleased]

### Added

- Initial MCP stdio launcher with `hold`, `test`, and `phase2` modes for live
  owner-session and graceful-restart testing.
- `call`, `tool`, and `resource` modes for direct JSON-RPC, MCP tool, and MCP
  resource probes after session initialization.
- `persist` mode to verify daemon survival across stdio disconnect and
  reconnect.
- `kill-reconnect` mode to measure recovery after a hard daemon kill.
- `install` mode to call `upgrade(action=apply)`, close stdio, reconnect, and
  verify installed server health.
- `-env-mode` profiles for full inherited environments and clean allow-list
  environments.
- `-expect-tools` and `-expect-version` assertions for smoke-test gates.

### Fixed

- Session bootstrap now applies the configured request timeout to initialize and
  `tools/list`.
- Install verification now waits for post-exit binary replacement before
  reconnecting when the upgrade payload or disconnect indicates deferred
  replacement.
- Payloadless upgrade disconnects are treated as expected install handoff
  signals when reconnect verification can prove the new server is healthy.
- Windows cleanup now falls back to PID-based `Stop-Process` when image-name
  `taskkill` is blocked.

### Changed

- Prepared the public README, contributor guide, changelog, issue templates,
  and CI metadata for GitHub publication.

Comparison links will be added after the first public release tag.
