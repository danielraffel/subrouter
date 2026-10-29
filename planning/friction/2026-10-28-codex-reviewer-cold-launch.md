# Fresh unattended Codex reviewer launch stalled

**Status:** IN PROGRESS
**Filed:** 2026-10-28 by Codex@m3
**Owner:** Subrouter maintainers
**Class:** SYSTEMIC
**Recurrence:** first confirmed incident; affects every cold `sr codex exec` launch when the selected provider, account, or transport is unavailable

## What happened

A Shipyard exact-head reviewer needed a genuine `pulp-review` verdict. Existing interactive/resumed Subrouter sessions worked, but fresh unattended `sr codex exec` launches from SSH stalled or failed. The attempts mixed several independent failures: MCP/OAuth 401s, refresh-token reuse errors, provider model-capacity responses, transient GitHub DNS/502s, and a reviewer sandbox that could not write its default temporary directory. No verdict was posted, so Shipyard correctly kept the PR out of the merge queue.

The waste came from treating each retry as a new reviewer session and from carrying native Codex launcher flags into the `sr` wrapper. The durable distinction is: retry a still-live logical request/session when capacity is the only failure; start a new session only when the process or transport is gone.

## Evidence

- Interactive/resumed `sr codex` sessions were usable.
- Fresh cold launches over SSH were the failing path.
- `--ignore-user-config` removed MCP initialization but did not fix the timeout, so MCP 401 was not the complete root cause.
- Subrouter's current Codex docs define pooled account selection, capacity retry, account failover, WebSocket handling, and the `X-Subrouter-Retry-Policy: autonomous` path.
- Subrouter's current test suite covers capacity retry, WebSocket retry, account switching, and bounded exhaustion.

## Root cause

The reviewer workflow had no durable launch contract. It conflated three boundaries:

1. the `sr codex` wrapper and native Codex CLI arguments;
2. provider capacity versus account quota/authentication;
3. a live session waiting for capacity versus a dead launcher that needs a new session.

The existing Subrouter retry machinery was therefore bypassed or repeatedly restarted by the operator instead of being allowed to finish its bounded logical request.

## The fix now (unblock)

From the authenticated m3 shell, use the wrapper's command contract and a writable project-local temporary directory:

```bash
sr doctor
sr status
curl -fsS http://127.0.0.1:31415/_subrouter/health
TMPDIR="$PWD/.shipyard-review-tmp" mkdir -p "$PWD/.shipyard-review-tmp"
TMPDIR="$PWD/.shipyard-review-tmp" sr codex exec \
  --sandbox read-only --skip-git-repo-check \
  "Review the current exact-head changes and return a signed Shipyard verdict."
```

Do not pass native-only approval flags before `exec`. If the process is still alive and reports capacity, let Subrouter's autonomous retry run; do not create another session. If it exits with a classified `retry after Ns`/capacity error, resume the same session when a session id is available:

```bash
sr codex resume --last
```

Use a fresh session only after verifying the prior process has exited and the failure is classified as launcher, auth, or transport initialization. Preserve the exact reviewer identity and exact PR head.

## The fix forever (prevent)

1. Keep Subrouter's autonomous capacity policy as the default. It retries the current account, then eligible pooled accounts, and returns a bounded classified error rather than hanging.
2. Keep the wrapper/native CLI argument boundary explicit; regression tests must cover `sr codex exec` and reject unsupported native approval flags.
3. Add a reviewer preflight that records daemon health, selected server, account selection, transport, and first-response timing without logging secrets. Health alone is not acceptance; a real inference response is required.
4. Have Shipyard classify launcher/auth/transport failures separately from reviewer verdicts and leave the PR unarmed on any non-verdict result.
5. Keep the reviewer sandbox on a writable project-local temp path; `/tmp` permission failures are environmental evidence, not a review verdict.

## Routing

- [x] Fix now — the operational contract and recovery commands are recorded here and in `docs/codex.md`.
- [ ] Hand off — add the Shipyard preflight/receipt integration after the current drain is restored.

## Resolution

**Outcome:** Existing Subrouter capacity retry and pooled-account behavior were verified in source/docs/tests. The exact wrapper invocation, session-continuation rule, diagnostics, and writable-temp workaround are now durable.
**Landed in:** this commit; Shipyard preflight integration remains follow-up work.
**Did it work?:** Interactive/resumed sessions continued to work, and the documented command avoids the native approval-flag mismatch. The exact-head reviewer verdict still needs a fresh live proof.
**Still open:** cold unattended launch acceptance requires three fresh successful runs and one counted Shipyard verdict on m3.
