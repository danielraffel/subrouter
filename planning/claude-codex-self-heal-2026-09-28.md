# Claude and Codex self-healing work ledger

**Updated:** 2026-09-28

## Completed or verified

- **Claude session self-healing:** PR #361 branch `feat/claude-session-self-heal`
  is pushed at `25a64528`. It contains durable quota alarms, exact cmux
  session/surface binding, early reset detection, and bounded resume dispatch.
  The full Go suite passes locally. Merge and live deployment are still
  separate gates and are not claimed here.
- **Codex stale-account refresh failover:** current `origin/main` already
  contains the OAuth refresh change that retries an untried Codex account even
  when its usage score is stale or unavailable. The regression test
  `TestRefreshSelectedCodexAccountFailsOverWhenAlternateScoreIsStale` passes.
  The older `feat/codex-session-self-heal` branch has no net diff against
  current `origin/main`, so no duplicate Codex PR is needed for this behavior.
- **Auto-resume terminology:** `sr wake` remains the stable command because it
  owns durable wake alarms; user-facing output and documentation now call the
  behavior auto-resume. Quota alarms dispatch `continue`; Codex provider
  capacity alarms use bounded `/goal resume` attempts, with `continue` fallback
  explicitly opt-in.

## Open tracked items

- **Claude malformed/expired credentials:** repair affected profiles with the
  appropriate browser OAuth flow and verify plan/usage metadata; never log or
  persist tokens. This requires interactive user authentication where needed.
- **Claude plan discovery:** confirm PR #443's browser-OAuth/default behavior
  and metadata handling are merged and deployed before treating `unknown` plan
  output as fixed.
- **Codex session recovery:** validate the cmux route-aware restore behavior
  for plain Codex versus `sr codex` without interrupting live sessions; retain
  fail-closed behavior for ambiguous historical sessions.
- **Provider comparison:** compare the Codex and Claude recovery paths after
  the Claude proxy change is deployed, including account refresh, quota
  classification, and resume dispatch evidence.
- **Deployment/merge:** obtain completed CI and maintainer merge/deploy
  evidence for PR #361; local tests and a pushed branch are not sufficient.
