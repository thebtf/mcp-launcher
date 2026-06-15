# Investigation: adaptive-install-reconnect

**Created:** 2026-06-15
**Reporter:** Engram issue #278 / session load
**Status:** PHASE-0-COMPLETE

---

## 1. Symptom (verbatim)

> `mcp-launcher -mode install` can produce a false FAIL on Windows post-exit upgrades by reconnecting too early after `updated_deferred` / `handoff_error: "post-exit install scheduled"`.

Observed output (exact):
```text
upgrade payload: status=updated_deferred, handoff_error="post-exit install scheduled"
[install] Reconnecting and verifying installed daemon
spawn D:\tmp\aimux-postexit-current.exe
FAIL server version: got "5.16.1-smoke-current", want "5.16.1-smoke-next"
```

Passing operator workaround from the same report used `-reconnect-delay 15` and returned:
```text
verified server version: 5.16.1-smoke-next
aimux://health.version = 5.16.1-smoke-next
[install] PASS
```

---

## 2. Reproduction

Steps attempted:
1. Read Engram issue #278 during session load; it contains the failing command, failing output, and passing delayed command.
2. Read the active dirty checkout on branch `fix/install-mode-verification`.
3. Ran semantic/source checks for install reconnect functions.

Reproduction status: CONFIRMED from issue evidence; local end-to-end Windows post-exit smoke is still required after the code review pass because it depends on unique aimux current/next binaries and process cleanup.

---

## 3. Hypotheses

### H1 - Fixed default reconnect delay is too short for post-exit replacement (Confidence: 9/10)
**Claim:** The root cause is that install mode reconnects after a fixed default delay even when the upgrade payload says replacement is scheduled after the old session exits.
**Prediction if true:** Source before the fix has no branch that interprets post-exit upgrade semantics; a delayed reconnect should pass while the default path can spawn the old binary.
**Disproving test:** Inspect install reconnect logic and compare it with #278 evidence.
**Disproving Test Result:** `RAN: SocratiCode and Serena found current fix adding effectiveInstallReconnectDelaySec and isPostExitInstallScheduled; #278 showed manual 15s delay passes while default 2s fails.`
**Status:** CONFIRMED
**Refuted by (if REFUTED):** -

### H2 - The aimux replacement helper itself is failing (Confidence: 2/10)
**Claim:** The binary replacement helper cannot replace current.exe even when mcp-launcher waits long enough.
**Prediction if true:** The same installed smoke would fail even with `-reconnect-delay 15`.
**Disproving test:** Compare failing and passing smoke evidence in #278.
**Disproving Test Result:** `RAN: #278 includes a PASS with -reconnect-delay 15, expected tools, expected version, and aimux://health version.`
**Status:** REFUTED
**Refuted by (if REFUTED):** Delayed reconnect smoke passed with version `5.16.1-smoke-next`.

### H3 - This is only documentation/operator guidance (Confidence: 1/10)
**Claim:** The launcher already adapts correctly and the only missing piece is telling operators to pass a longer delay.
**Prediction if true:** Current or prior launcher logic would infer post-exit scheduling automatically.
**Disproving test:** Inspect `runInstall` and delay handling.
**Disproving Test Result:** `RAN: the active fix adds new adaptive delay helpers; the previous behavior described in #278 required a manual -reconnect-delay 15 workaround.`
**Status:** REFUTED
**Refuted by (if REFUTED):** Manual operator delay was required by the observed passing smoke; a pure docs fix would preserve the false-fail default.

### H4 - Explicit user reconnect-delay gets ignored by the fix (Confidence: 3/10)
**Claim:** The adaptive fix might override an explicitly supplied `-reconnect-delay`, breaking operator control.
**Prediction if true:** `flag.Visit` would not detect explicit reconnect-delay, or effective delay would always force 15s for post-exit payloads.
**Disproving test:** Inspect flag parsing and run the explicit-value regression test.
**Disproving Test Result:** `RAN: source has reconnectDelayExplicit via flag.Visit and TestEffectiveInstallReconnectDelayHonorsExplicitValue covers the explicit branch.`
**Status:** REFUTED
**Refuted by (if REFUTED):** Source and regression test show explicit values are preserved.

---

## 4. Evidence Classification

| Fact | Tag | Source tool-call | Source |
|------|-----|------------------|--------|
| Issue #278 reports default install smoke failing after `updated_deferred` / `post-exit install scheduled` with old version observed. | OBSERVED | `tool:mcp__engram.issues@get-278` | Engram issue #278 |
| Issue #278 reports the same smoke passing with `-reconnect-delay 15`. | OBSERVED | `tool:mcp__engram.issues@get-278` | Engram issue #278 |
| Active checkout branch is `fix/install-mode-verification` with dirty `main.go`, `README.md`, and untracked tests. | OBSERVED | `tool:shell@git-status` | Session load git status |
| Recent commits include MCP tool/resource install modes and timeout bootstrap work. | OBSERVED | `tool:shell@git-log-20` | `git log --oneline -20` |
| Engram `recall(action="by_file")` is unavailable in v5, so file-scoped recall fell back to search. | OBSERVED | `tool:mcp__engram.recall@by-file-error` | Engram tool error |
| Engram search for `install reconnect post-exit bug mcp-launcher` returned no project memories. | OBSERVED | `tool:mcp__engram.recall@search-empty` | Engram recall |
| SocratiCode located `runInstall`, `waitForInstallReconnect`, `effectiveInstallReconnectDelaySec`, and `isPostExitInstallScheduled` as the semantic center of the bug. | OBSERVED | `tool:mcp__socraticode.codebase_search@install-reconnect` | SocratiCode search |
| Serena showed `effectiveInstallReconnectDelaySec` preserves explicit reconnect-delay and raises only non-explicit post-exit defaults below 15s. | OBSERVED | `tool:mcp__serena.find_symbol@effective-delay` | `main.go` |
| Serena showed `waitForInstallReconnect` waits before the first reconnect attempt and retries until timeout when retry delay is positive. | OBSERVED | `tool:mcp__serena.find_symbol@wait-reconnect` | `main.go` |
| Adaptive reconnect should be triggered by upgrade result semantics rather than binary name or aimux version. | INFERRED | - | Derived from #278 evidence and source inspection |

---

## 5. Hypothesis Status Log

| Hypothesis | Status Change | Disproving Evidence (if REFUTED) |
|------------|---------------|----------------------------------|
| H1 | PENDING -> CONFIRMED | #278 default-vs-delayed evidence plus source showing adaptive handling is the required fix surface. |
| H2 | PENDING -> REFUTED | #278 delayed smoke passed with the next version, so replacement can succeed when reconnect does not race it. |
| H3 | PENDING -> REFUTED | #278 required manual delay; current branch adds code, not just README guidance. |
| H4 | PENDING -> REFUTED | `flag.Visit` records explicit `-reconnect-delay`; regression test covers explicit preservation. |

---

## 6. Root Cause (populated at Phase 0 exit)

**Root cause:** `mcp-launcher` treated install-mode reconnect delay as a fixed CLI sleep and did not interpret post-exit upgrade responses, so Windows verification could spawn the old locked executable before the helper replaced it.

**Evidence chain:**
- #278 default install smoke reconnected immediately after a post-exit scheduled response and observed the old version - OBSERVED (`tool:mcp__engram.issues@get-278`).
- #278 delayed smoke with `-reconnect-delay 15` observed the expected next version and PASS - OBSERVED (`tool:mcp__engram.issues@get-278`).
- Therefore the replacement can succeed if verification does not race the post-exit helper - INFERRED from the two #278 outcomes.
- Source inspection shows the active fix belongs at install reconnect delay selection and first reconnect timing - OBSERVED (`tool:mcp__socraticode.codebase_search@install-reconnect`, `tool:mcp__serena.find_symbol@runInstall`).

**Evidence chain tag check:**
- Contains 0 ASSUMED links: YES

**Hypotheses ruled out:**
- H2: REFUTED by delayed smoke passing with the replacement version.
- H3: REFUTED by the false-fail default requiring manual operator delay.
- H4: REFUTED by explicit reconnect-delay preservation in source and regression coverage.

**Phase 0 exit gate:**
- >=3 hypotheses enumerated with all required fields: YES
- >=1 hypothesis REFUTED with disproving evidence: YES
- Symptom reproduction documented: YES
- Root cause named with evidence chain: YES
- Evidence chain contains 0 ASSUMED links: YES
- Evidence table classifies every used fact: YES

GATE RESULT: PASS

---

## 7. Phase 1 Fix And Verification

Fix attempt 1:
- Change: adaptive default raised non-explicit post-exit reconnect delay from 2s to 15s.
- Narrow tests: `go test . -run "TestEffectiveInstallReconnectDelay|TestIsPostExitInstallScheduled" -count=1 -v` passed.
- Full checks: `go test . -count=1 -v`, `go vet .`, `go test ./... -count=1`, `go build .`, and `git diff --check` passed, with only CRLF warnings from Git.
- User-observable smoke: FAILED on stale `D:\tmp\aimux-postexit-current.exe` / `D:\tmp\aimux-postexit-next.exe` copies. The adaptive 15s branch fired, but reconnect still saw `5.16.1-review-current`; post-run evidence showed a staged `.aimux-update-*.exe` with next hash and unchanged current hash.
- Result: disproved "longer fixed delay alone is sufficient" for the stale fixture.

Fix attempt 2:
- Change: for non-explicit deferred/post-exit install payloads, `runInstall` fingerprints the installed binary before upgrade, closes the install session, waits until the installed binary SHA256 changes, then reconnects. Explicit `-reconnect-delay` still bypasses this adaptive gate.
- Regression tests added: delay policy table, post-exit payload detection table, and file fingerprint hash-change detection.
- User-observable smoke: PASSED on the documented aimux post-exit fixture copied to unique paths:
  - current source: `D:\Dev\aimux\bin\postexit-smoke\aimux-postexit-current.exe.old`
  - next source: `D:\Dev\aimux\bin\postexit-smoke\aimux-postexit-next.exe`
  - smoke current: `D:\tmp\mcp-launcher-278-goodfixture-20260615191238\aimux-current-20260615191238.exe`
  - smoke next: `D:\tmp\mcp-launcher-278-goodfixture-20260615191238\aimux-next-20260615191238.exe`
  - command shape: `go run . -mode install ... -expect-version 5.16.1-review-next -timeout 120 -cleanup-binary-processes`
  - observed: `installed binary changed: cae642f5160b -> a4afc8b2de1c`, `verified server version: 5.16.1-review-next`, `aimux://health.version = 5.16.1-review-next`, `[install] PASS`.
- Cleanup verification: no processes remained for `aimux-current-20260615191238*` or `aimux-next-20260615191238*`; current and next hashes matched after the smoke.

Final verification:
- `go test ./... -count=1`: PASS.
- `go vet .`: PASS.
- `go build .`: PASS.
- `git diff --check`: PASS with CRLF warnings only.

Residual scope note:
- Engram #261 remains unfixed. The current `-cleanup-binary-processes` flag is best-effort image-name cleanup; PID/tree cleanup and Windows access-denied fallback are still separate follow-up work.
