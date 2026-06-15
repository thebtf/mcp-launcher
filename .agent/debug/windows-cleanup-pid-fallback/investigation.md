# Investigation: windows-cleanup-pid-fallback

**Created:** 2026-06-15
**Reporter:** Engram issue #261
**Status:** FIXED-LOCAL

---

## Symptoms

1. `-cleanup-binary-processes` can warn after a Windows image-name cleanup failure:

```text
WARN cleanup-binary-processes: taskkill aimux-release-task-closeout-20260613193541-26948.exe: exit status 1: ERROR: Access denied
```

2. After that warning, process inventory still showed a unique smoke daemon alive:

```text
PID 21132
```

3. Targeted PID cleanup worked:

```powershell
Stop-Process -Id 21132 -Force
```

## When Did It Start

The broader #261 issue started before the local `os.Exit` cleanup fix. The remaining reopened scope started after the initial #261 fix added `-cleanup-binary-processes` as image-name `taskkill` cleanup without a PID fallback.

## Contradiction

Both cleanup paths target the same leftover smoke daemon:

- `taskkill /F /IM <unique-image>.exe` can fail with `ERROR: Access denied`.
- `Stop-Process -Id <pid> -Force` can remove the same leftover process.

The launcher exposed only the first path, so a process that was removable by PID could survive cleanup.

---

## Hypothesis Table

| Hypothesis | Prediction | Direct test | Status |
| --- | --- | --- | --- |
| H1: Remaining #261 root cause is missing PID fallback after image-name `taskkill` failure. | If `taskkill /IM` returns access denied, current `cleanupBinaryProcesses` returns an error without enumerating/killing PIDs. | `go test . -run TestCleanupBinaryProcessesFallsBackToPIDCleanupOnAccessDenied -count=1 -v` failed before the fix. | CONFIRMED |
| H2: The process is fundamentally unkillable from launcher context. | PID-based cleanup would fail too. | Engram #261 recorded `Stop-Process -Id 21132 -Force` as successful. A local syntax smoke also killed a temporary `powershell Start-Sleep` process by PID. | REFUTED |
| H3: The already-fixed `os.Exit` path is still the active blocker. | Tool error cleanup would still skip `client.close()`. | Existing `TestRunToolErrorClosesClient` passes and verifies the fake MCP server observes stdin close on tool error. | REFUTED |

---

## Root Cause

`cleanupBinaryProcesses` only attempted Windows cleanup through `taskkill /F /IM <image>` and returned a warning on non-`not found` failures, so it never used the known-working PID cleanup path after access-denied image-name cleanup failures.

## Evidence

- VERIFIED Engram #261: exact warning was `ERROR: Access denied`.
- VERIFIED Engram #261: process inventory showed PID `21132` alive after the warning.
- VERIFIED Engram #261: `Stop-Process -Id 21132 -Force` removed the process.
- VERIFIED source before fix: `cleanupBinaryProcesses` called only `taskkill /F /IM <name>`.
- VERIFIED RED:

```text
=== RUN   TestCleanupBinaryProcessesFallsBackToPIDCleanupOnAccessDenied
    main_test.go:60: cleanupBinaryProcesses returned error: taskkill unique-smoke.exe: exit status 1: ERROR: Access denied
--- FAIL: TestCleanupBinaryProcessesFallsBackToPIDCleanupOnAccessDenied (0.00s)
FAIL
```

- VERIFIED GREEN:

```text
=== RUN   TestCleanupBinaryProcessesFallsBackToPIDCleanupOnAccessDenied
--- PASS: TestCleanupBinaryProcessesFallsBackToPIDCleanupOnAccessDenied (0.00s)
PASS
```

- VERIFIED live command syntax smoke:

```text
stopped:29736
```

The smoke started a temporary hidden `powershell Start-Sleep` process and stopped it with the same `Stop-Process -Id <pid> -Force -ErrorAction Stop` syntax used by the fallback.

## Fix Applied

Windows `-cleanup-binary-processes` now:

1. Tries the existing image-name cleanup:

```text
taskkill /F /IM <image-name>
```

2. Preserves the previous `not found` behavior as success.
3. On other `taskkill` errors, enumerates matching image-name PIDs through `Get-CimInstance Win32_Process`.
4. Stops each matching PID through:

```powershell
Stop-Process -Id <pid> -Force -ErrorAction Stop
```

The cleanup remains smoke-test-only and should still be used only with unique disposable binary names.

## Regression Test

Added `TestCleanupBinaryProcessesFallsBackToPIDCleanupOnAccessDenied`.

Verification method: unit test / command output.

The test would have caught the reopened #261 bug because it simulates the exact image-name access-denied failure and asserts that PID enumeration plus `Stop-Process -Id` happens.

## Verification

```text
go test . -run TestCleanupBinaryProcessesFallsBackToPIDCleanupOnAccessDenied -count=1 -v
go test ./... -count=1
go vet .
go build .
git diff --check
```

Results: all passed. `git diff --check` reported CRLF warnings only.

## What To Be Skeptical Of

- The regression test fakes command output; it proves launcher branching and command selection, not a live aimux daemon cleanup.
- The live syntax smoke proves `Stop-Process -Id` command syntax on this machine, not access-denied reproduction against `aimux`.
- This fallback still targets matching image names. It is intentionally bounded to unique smoke binary copies and is not a general production process manager.

## If You Remember 3 Things

1. #261's remaining bug was missing PID fallback after image-name `taskkill` failed.
2. The RED test showed the launcher returned the access-denied error without trying PID cleanup.
3. The fix keeps image-name cleanup first, then uses `Get-CimInstance` + `Stop-Process -Id` for the Windows fallback.
