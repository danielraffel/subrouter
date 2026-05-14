# Subrouter upgrades

Use this runbook for local macOS daemon upgrades when Codex is already pointed at `127.0.0.1:31415`.

## Current safe handoff

This path has a short listener restart. Existing in-flight requests can fail once, but future Codex requests reconnect to the same URL. Do not change client base URLs for a local binary upgrade.

Run from the Subrouter checkout:

```bash
set -euo pipefail

bin="${SUBROUTER_BIN:-$HOME/bin/subrouter}"
label="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
service="gui/$(id -u)/$label"
health_url="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
backup="$bin.backup-$(date +%Y%m%d-%H%M%S)"
next="$(mktemp "$bin.next.XXXXXX")"

go build -ldflags=-linkmode=external -o "$next" ./cmd/subrouter
chmod 0755 "$next"
"$next" help >/dev/null
curl -fsS "$health_url" >/dev/null

before_pid="$(launchctl print "$service" | awk '/pid =/ {print $3; exit}')"
before_sha="$(shasum -a 256 "$bin" | awk '{print $1}')"

cp -p "$bin" "$backup"
mv "$next" "$bin"

launchctl kickstart -k "$service"

ok=0
for _ in $(seq 1 80); do
  if curl -fsS "$health_url" >/tmp/subrouter-health.json; then
    ok=1
    break
  fi
  sleep 0.25
done

after_pid="$(launchctl print "$service" | awk '/pid =/ {print $3; exit}')"
after_sha="$(shasum -a 256 "$bin" | awk '{print $1}')"

printf 'before_pid=%s\nafter_pid=%s\nbefore_sha=%s\nafter_sha=%s\nbackup=%s\nhealth_ok=%s\n' \
  "${before_pid:-missing}" "${after_pid:-missing}" "$before_sha" "$after_sha" "$backup" "$ok"
cat /tmp/subrouter-health.json

if [ "$ok" != 1 ]; then
  exit 1
fi
if [ "${before_pid:-}" = "${after_pid:-}" ]; then
  echo "subrouter pid did not change" >&2
  exit 1
fi
```

Rollback uses the printed backup path:

```bash
set -euo pipefail

backup="<printed-backup-path>"
bin="${SUBROUTER_BIN:-$HOME/bin/subrouter}"
service="gui/$(id -u)/${SUBROUTER_LABEL:-ai.manaflow.subrouter}"

cp -p "$backup" "$bin"
launchctl kickstart -k "$service"
curl -fsS http://127.0.0.1:31415/_subrouter/health
```

## Rules

- Keep the public URL stable. Codex desktop and long-running CLI processes do not reliably adopt a new base URL mid-session.
- Replace the binary atomically with `mv`, then restart the supervisor. Do not edit the live binary in place.
- Use `launchctl kickstart -k`, not `kill -9`. Launchd owns restart policy and environment.
- Do not run `install-daemon` for ordinary binary upgrades. It rewrites the LaunchAgent and does a heavier bootout/bootstrap flow.
- Keep a backup binary until the new daemon has served real traffic.
- Do not work around upstream write failures by globally disabling connection pooling. Keep the outbound transport pooled, keep ChatGPT traffic on HTTP/1.1 to avoid HTTP/2 stream fanout, limit concurrent replayable `/responses` uploads, and handle transient `broken pipe`, `closed network connection`, `connection reset`, `unexpected EOF`, and TLS record errors with several replay attempts for buffered `/responses` and `/responses/compact` POSTs.

## Verifying compact failures

The daemon logs show request starts and proxy transport errors. A client-side compact error can be stale if the retry later completes, so check the transcript for the same session before changing code:

```bash
session_id="<codex-session-id>"
tail -n 50000 "$HOME/.subrouter/transcripts/by-agent/codex/by-session/$session_id.jsonl" \
  | jq -r 'select(.type=="subrouter_meta" or .type=="http_body") | [.timestamp,.type,(.payload.direction // "" ),(.payload.method // "" ),(.payload.path // "" ),(.payload.status // "" ),(.payload.bytes // "" ),(.payload.headers."Content-Length"[0] // "" ),(.payload.headers."Content-Encoding"[0] // "" )] | @tsv' \
  | tail -120
```

For a successful compact, expect a `subrouter_meta` row for `POST /responses/compact`, a full `client_to_upstream` body byte count matching `Content-Length`, then an `upstream_to_client` row with status `200`.

## Zero-disruption target

True no-drop upgrades need a stable front listener that is separate from the Subrouter worker.

The front listener should own `127.0.0.1:31415`. Each Subrouter build starts as a versioned worker on a Unix socket or private loopback port. The listener health-checks the new worker, routes only new requests to it, and keeps existing HTTP streams and WebSockets pinned to the old worker until they finish or hit a drain timeout. Rollback is just changing the active worker pointer back.

Until that supervisor exists, the guarded launchd handoff above is the safest local upgrade path.
