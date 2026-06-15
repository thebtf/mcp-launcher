# Release Protocol

## Applies When

This protocol applies to every public release of `github.com/thebtf/mcp-launcher`.

## Additional Release Surfaces

| Surface | Version source | Publish command | Verification |
| --- | --- | --- | --- |
| Go module / CLI install path | Git tag `vX.Y.Z` | `git push origin main --follow-tags` | `git ls-remote --tags origin refs/tags/vX.Y.Z` and `go install github.com/thebtf/mcp-launcher@vX.Y.Z` in a disposable module cache |
| GitHub Release | Git tag `vX.Y.Z` and `RELEASE_NOTES.md` | `gh release create vX.Y.Z --title vX.Y.Z --notes-file RELEASE_NOTES.md` | `gh release view vX.Y.Z --json tagName,url,isLatest` |

## Required Gates

| Gate | Command / evidence | Blocks release when |
| --- | --- | --- |
| Unit tests | `go test ./... -count=1` | Exit code non-zero |
| Critical CLI smoke | `go test -tags=critical ./tests/critical/...` | Exit code non-zero |
| Static checks | `go vet .` | Exit code non-zero |
| Build | `go build .` | Exit code non-zero |
| CLI walkthrough | `./mcp-launcher -h` or `./mcp-launcher.exe -h` | Help does not list documented release flags |
| Production playbook | Walk `docs/PRODUCTION-TESTING-PLAYBOOK.md` and record `PRODUCT_WORKS` | Any scenario fails or produces a release-relevant surprise |
| Secret scan | `gitleaks detect --source .` and filesystem scan | Any leak is reported |

## Version Alignment

- There is no checked-in package version file. The release version is the
  annotated Git tag.
- `CHANGELOG.md`, `RELEASE_NOTES.md`, and the GitHub Release title must use the
  same `vX.Y.Z` version.

## Release Notes

- `CHANGELOG.md` records the technical delta.
- `RELEASE_NOTES.md` records the user-facing release summary used by
  `gh release create`.

## Publish / Smoke / Handoff

1. Push `main` and the annotated tag to `origin`.
2. Create the GitHub Release from `RELEASE_NOTES.md`.
3. Verify `go install github.com/thebtf/mcp-launcher@vX.Y.Z` in a disposable
   `GOMODCACHE`.
4. Verify the installed binary can print help.

## Terminal Verdict

- `PROJECT_RELEASE_PROTOCOL_PASS`: all required gates pass, the remote tag is
  visible, the GitHub Release exists, and disposable `go install` smoke passes.
- `PROJECT_RELEASE_PROTOCOL_BLOCKED`: any required gate, remote tag, GitHub
  Release, or disposable install smoke cannot be verified.
