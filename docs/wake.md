# Quota wake alarms

`sr wake` is the local control surface for quota-triggered agent resumes. The
alarm store and policy commands are available now; automatic proxy scheduling
and the launchd worker are the remaining integration work.
It is disabled by default and must be enabled independently for Codex and
Claude. The existing cmux/session watcher remains the only component that
reads a terminal surface or sends a resume action; Subrouter owns quota
classification, reset selection, and durable alarm state.

## Behavior

When an enabled agent receives an authoritative account or model-pool quota
failure, Subrouter fails over across untried accounts first. If no eligible
account remains, it records the earliest reset for the requested pool and
creates one persistent wake alarm. The alarm is limited to a recently active
session that produced the quota signal. Its fire time is reset time plus a
configurable grace delay and bounded per-session jitter. Dispatch is rate
limited so many sessions do not reconnect at once.

On the first watcher scan, recently active means last activity within eight
hours. Later monitor passes require a newly observed, confirmed signal. They do
not rediscover and resume an old tab from a broad screen scrape. Existing old
alarms remain visible for audit but are never reactivated by that first scan.

The watcher validates the original machine, cmux surface, agent type, session
ID, and active-writer state before sending the configured action. A mismatched
or missing surface becomes `stale`; no replacement tab is selected.

The queue keeps three recovery kinds separate: `codex-provider` is a temporary
model-provider capacity event and requires a cooldown plus a lightweight health
check; `codex-quota` is an account reset and uses `/goal resume` after reset and
grace; `claude-quota` is a Claude reset and uses `continue`. A signal for one
kind cannot create or dispatch another kind of alarm.

Codex capacity recovery must not immediately replay a large `/goal resume`
request after a provider failure. The first failure records the provider error
and enters a short cooldown. The watcher or a lightweight provider probe must
show that the route is usable before one expensive resume is attempted. Further
failures use bounded backoff and a finite attempt budget; they do not loop
large-context resumes. Token usage and whether generation began are recorded
for each attempt so this policy can be tuned from evidence.

## Configuration and controls

The configuration is per agent and disabled by default:

```toml
[watcher.auto_resume.claude]
enabled = false
action = "continue"
delay = "2m"

[watcher.auto_resume.codex]
enabled = false
action = "goal-resume"
delay = "2m"
```

Planned controls are:

```text
sr wake list
sr wake show <id>
sr wake enable <claude|codex>
sr wake disable <claude|codex>
sr wake update <id> --delay 5m --expires-in 2d4h15m
sr wake now <claude|codex|all>
sr wake cancel <id>
sr wake cancel --agent <claude|codex>
sr wake cancel --all
```

Durations accept days, hours, and minutes (`2d4h15m`). They are converted to
absolute UTC timestamps when stored so alarms survive reboot. Each record has
`wake_at`, `expires_at`, the exact cmux surface and session identity, the
provider/model pool, action, launchd label, and status (`scheduled`, `fired`,
`completed`, `stale`, `cancelled`, `expired`, or `failed`).

`wake now` revalidates and dispatches existing alarms immediately. It uses the
same writer checks and throttling as automatic dispatch, which makes it safe
after a manual quota reset.

## Provider signals

Codex quota signals include `usage_limit_reached`, `insufficient_quota`,
`usage_not_included`, `quota_exceeded`, `rate_limit_exceeded`, workspace credit
or spend-cap failures, and reset fields such as `resets_in_seconds` or
`resets_at`, including their `response.failed` SSE and websocket forms.

Claude quota signals include HTTP 429/401/403, rejected
`anthropic-ratelimit-unified-status`, rejected 5-hour or weekly windows,
model-scoped `7d_oi` rejection, `Retry-After`, and unified reset timestamps.
Headerless transient 429s and `allowed_warning` responses remain request-level
failover only and do not create long-lived alarms.

## Acceptance

- No wake alarm is created while the agent setting is disabled.
- A session fails over immediately before any alarm is created.
- The alarm selects the earliest eligible account reset for the requested pool.
- Reboot and sleep/wake preserve the alarm and its absolute timestamps.
- `list`, `show`, `update`, `now`, and `cancel` are deterministic and durable.
- Duplicate workers cannot read or write the same surface concurrently.
- Codex and Claude actions are not interchangeable.
- The full `go test ./...` suite passes before handoff.
