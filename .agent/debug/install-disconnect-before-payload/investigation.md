# Investigation: install-disconnect-before-payload

**Created:** 2026-06-15
**Reporter:** Engram issue #286
**Status:** FIXED-LOCAL

---

## Symptoms

1. Engram #286 reports that the #278 hash-wait fix only runs when `upgrade(action=apply)` returns a payload that says `updated_deferred` or `post-exit install scheduled`.
2. If the upgrade connection closes before the payload is returned, `runInstall` treats the disconnect as expected but continues with `payload == nil`.

## When Did It Start

After commit `86db73c fix: wait for post-exit install replacement`, which fixed the payload-returning post-exit install path.

## Contradiction

Both paths are install-mode post-exit upgrade paths:

- Payload path: `upgrade(action=apply)` returns post-exit/deferred metadata, so mcp-launcher waits for the installed binary SHA256 to change before reconnecting.
- Disconnect-before-payload path: `upgrade(action=apply)` closes the connection during apply, so mcp-launcher knows an upgrade disconnect happened, but previously discarded that signal when deciding whether to wait for binary replacement.

The same safety condition was required, but only the payload path received it.

---

## Hypothesis Table

| Hypothesis | Prediction | Direct test | Status |
| --- | --- | --- | --- |
| H1: The disconnect-before-payload path loses the post-exit wait signal. | A decision test with `payload == nil`, expected upgrade disconnect, and no explicit reconnect delay returns `false` before the fix. | `go test . -run TestShouldWaitForInstallReplacement -count=1 -v` failed on `expected_disconnect_before_payload`. | CONFIRMED |
| H2: Nil payload alone should mean post-exit install. | Existing payload classifier would need to treat `nil` as scheduled. | Existing `TestIsPostExitInstallScheduled` intentionally expects nil payload to be `false`. | REFUTED |
| H3: The bug is only reconnect delay length, not missing wait. | Increasing delay would be the core behavior. | Source evidence shows the stronger gate is `waitForInstallBinaryReplacement`, not `effectiveInstallReconnectDelaySec`; the fixed path waits for SHA256 change and then reconnects immediately. | REFUTED |

---

## Root Cause

`runInstall` recorded an expected upgrade disconnect only for logging/control flow, then computed replacement waiting solely from `isPostExitInstallScheduled(payload)`, so a disconnect-before-payload path bypassed `waitForInstallBinaryReplacement`.

## Evidence

- VERIFIED source: `runInstall` previously computed `waitForReplacement` from the payload-only predicate.
- VERIFIED source: `isPostExitInstallScheduled(nil)` returns false by design.
- VERIFIED RED test:

```text
=== RUN   TestShouldWaitForInstallReplacement/expected_disconnect_before_payload
    install_reconnect_test.go:164: shouldWaitForInstallReplacement = false, want true
--- FAIL: TestShouldWaitForInstallReplacement (0.00s)
FAIL
```

- VERIFIED GREEN test:

```text
=== RUN   TestShouldWaitForInstallReplacement/expected_disconnect_before_payload
--- PASS: TestShouldWaitForInstallReplacement (0.00s)
PASS
```

## Fix Applied

Added `upgradeDisconnected` as an explicit signal in `runInstall` and routed the replacement-wait decision through `shouldWaitForInstallReplacement`.

`shouldWaitForInstallReplacement` now returns true when:

- reconnect delay was not explicitly set; and
- either the payload reports post-exit/deferred install, or the upgrade call ended with an expected upgrade disconnect.

Explicit `-reconnect-delay` remains an operator override.

## Regression Test

Added `TestShouldWaitForInstallReplacement`.

Verification method: unit test / command output.

The test would have caught #286 before the fix because the `expected disconnect before payload` case failed against the pre-fix decision rule.

## Verification

```text
go test . -run TestShouldWaitForInstallReplacement -count=1 -v
go test ./... -count=1
go vet .
go build .
git diff --check
```

Results: all passed. `git diff --check` reported CRLF warnings only.

## What To Be Skeptical Of

- This is unit-level proof of the launcher decision rule, not a fresh real aimux post-exit smoke for the disconnect-before-payload timing.
- #261 remains separate: Windows cleanup can still fail when image-name `taskkill` returns access denied.
- The fix is conservative. If an upgrade disconnect happens without replacement, install mode will wait for the binary hash to change and fail on timeout instead of reconnecting to the old binary.

## If You Remember 3 Things

1. #286 was real: expected disconnect was not used as a replacement-wait signal.
2. The RED test proved the exact decision gap before the fix.
3. The fix makes disconnect-before-payload follow the same safe wait-for-hash-change behavior as the #278 payload path.
