# Subrouter upgrades

Use this runbook for local macOS daemon upgrades when Codex is already pointed at `127.0.0.1:31415`.

## Replacing the binary in place on macOS

A LaunchAgent that runs `subrouter serve` directly, rather than behind
`subrouter supervise`, is upgraded by replacing its executable. **Do not
overwrite the live executable with `cp`.** Writing through the existing inode
invalidates the binary's code-signing state, and macOS then kills every respawn
with `OS_REASON_CODESIGNING` (SIGKILL, exit 137). The daemon appears to
flap: launchd restarts it, the kernel kills it, and the log shows nothing
useful.

Restoring the previous binary to the same path does **not** recover it. The
pathname stays poisoned even when `codesign --verify --strict` passes and the
restored file is byte-identical to the original. Recovery requires deleting the
file first, so the copy lands on a fresh inode:

```bash
set -euo pipefail
cp ~/bin/subrouter.backup ~/bin/subrouter.restore
chmod 755 ~/bin/subrouter.restore
codesign --verify --strict --verbose=4 ~/bin/subrouter.restore
~/bin/subrouter.restore --help >/dev/null
launchctl bootout gui/$(id -u)/<label>
mv -f ~/bin/subrouter.restore ~/bin/subrouter
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/<label>.plist
```

The upgrade procedure that avoids this staged the new binary under its own name,
signs and proves it runs *before* the old one is taken out of service, and puts
it in place with an atomic rename:

```bash
set -euo pipefail
cp subrouter.new ~/bin/subrouter.new
# Preserve release signatures. A failed release verification is fatal; only an
# explicitly selected local build may replace its absent signature ad hoc.
if ! codesign --verify --strict ~/bin/subrouter.new 2>/dev/null; then
  [ "${SUBROUTER_LOCAL_BUILD:-0}" = 1 ] || {
    echo "release artifact signature verification failed" >&2
    exit 1
  }
  codesign --force --sign - ~/bin/subrouter.new
fi
codesign --verify --strict --verbose=4 ~/bin/subrouter.new
~/bin/subrouter.new --help >/dev/null            # prove it executes first
cp -p ~/bin/subrouter ~/bin/subrouter.rollback
launchctl bootout gui/$(id -u)/<label>
mv ~/bin/subrouter.new ~/bin/subrouter           # rename, never cp
if ! launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/<label>.plist; then
  mv -f ~/bin/subrouter.rollback ~/bin/subrouter
  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/<label>.plist
  exit 1
fi
rm -f ~/bin/subrouter.rollback
```

On macOS versions whose launchd policy accepts ad-hoc signatures, the explicit
local-build fallback makes an unsigned build runnable without replacing a valid
release signature. A deployment using `SpawnConstraint` with a
`team-identifier` must instead use a certificate-backed signature for that team;
an ad-hoc signature has no Team ID and cannot satisfy the constraint.

### What a restart costs

`serve` drains on SIGTERM: it stops accepting, finishes in-flight requests, and
exits, bounded by `--shutdown-timeout` (default 10 minutes). launchd is the
shorter fuse. Without an explicit `ExitTimeOut` in the direct `install-daemon`
plist, the escalation timeout is system-defined and may be shorter than
`--shutdown-timeout`; a stream still running when launchd escalates is cut, and
`ThrottleInterval` delays the restart by its value. Sticky session assignments
survive, because the session store is read back from its file at startup; the
scheduler's exhaustion marks do not, since they are in-memory only.

During a supervised worker upgrade, the supervisor keeps owning the listener
and hands existing connections to their original worker, avoiding listener
interruption and connection drops. Restarting the supervisor itself still
closes the listener and uses the normal drain/restart behavior.

## Credential-origin rollback boundary

The rollback binary must understand the stored Codex
`oauthCredentialOrigin` field once `sr codex migrate-isolation` has run. A
binary from before that field existed ignores it while reading an account and
can erase it the next time it refreshes and rewrites the credential. The next
upgraded daemon then rejects that account as unisolated.

Do not keep a pre-isolation binary as the post-migration rollback artifact;
retain the last build that preserves credential origin instead. If an emergency
rollback did run an older binary, stop it and rerun
`sr codex migrate-isolation` before starting an isolation-enforcing build. That
re-enrollment may require browser approval for every account the older binary
rewrote.

## Supervised handoff

On macOS, run Subrouter behind `subrouter supervise`. The supervisor owns the public listener, starts each worker on an inherited private socket, and pins accepted TCP connections to that worker generation. An upgrade starts and health-checks the replacement before switching new connections. Old WebSockets, SSE streams, HTTP requests, and keep-alive connections remain on the old worker. The old worker exits only after its connection count reaches zero.

The supervisor is deliberately separate from the replaceable worker binary. Routine releases update `/usr/local/bin/subrouter`; they do not replace or restart `/usr/local/libexec/subrouter-supervisor`.

### One-time LaunchDaemon migration

Preparation does not touch the running service:

```bash
sudo ./deploy/macos/migrate-launchdaemon-to-supervisor.sh
```

Inspect the generated `.plist.supervised`, then perform the one-time listener transition:

```bash
sudo ./deploy/macos/migrate-launchdaemon-to-supervisor.sh --activate
```

The one-time transition cannot preserve connections accepted by an older, unsupervised process because that process owns their file descriptors. Perform it in a maintenance window. All later worker upgrades preserve connections.

### Transactional per-user LaunchAgent migration

The per-user migration does not accept health alone as cutover proof. Preparation
is non-disruptive, while activation requires a bounded preflight and an explicit
functional canary command:

```bash
deploy/macos/migrate-launchagent-to-supervisor.sh
deploy/macos/migrate-launchagent-to-supervisor.sh --activate \
  --canary-callback /path/to/real-routed-canary
```

The default preflight directly executes the candidate as `subrouter codex
isolation-check --json --retiring-state-dir PATH`; it is read-only and compares
the complete candidate and retiring account inventories. It fails unless the
candidate uses its exact isolated store, every served Codex OAuth credential
has trusted provenance, and no refresh-token chain is shared within the
candidate store or with the retiring store. The retiring root is read from the
preserved plist's explicit `SUBROUTER_STATE_DIR`; use `--retiring-state-dir
PATH` only when migrating a plist that predates that declaration. Missing,
ambiguous, or equal roots fail before live mutation. `--preflight-callback
PATH` can replace that check for a deployment-specific executable.

`SUBROUTER_STATE_DIR` selects the candidate state root and is written into the
prepared LaunchAgent explicitly; launchd does not inherit the activating
shell's environment. During migration from a binary that predates credential
provenance, point the candidate at a separately migrated state root. The
retained rollback plist must keep using the untouched legacy state root so the
old binary cannot erase provenance from candidate credentials.

The canary callback must be an executable file that exercises ordinary routed
traffic and returns nonzero unless the expected response is observed. Callback
paths are executed directly with no shell evaluation or command-string parsing.
Do not put credentials in filenames or arguments; have the callback read any
required file-backed credential through its normal consumer.
`SUBROUTER_PREFLIGHT_TIMEOUT` and `SUBROUTER_CANARY_TIMEOUT` bound the callbacks
(120 and 300 seconds by default); timeout terminates and waits for the callback
process group.

The migration itself checks local health and readiness. The functional canary
owns every deployment-specific acceptance leg: remote health/readiness probes,
sticky or existing-session continuity, and a real routed provider response.
Keep those checks in the callback so the generic migration contains no machine
names, tailnet addresses, account identifiers, or credentials.

Activation records a single launchd identity snapshot plus a PID, executable
hash, start-time, and command fingerprint. A mode-`0700` transaction journal is
armed before the first live mutation. It waits for two observations of complete
label/PID/listener absence, atomically installs the candidate plist, and proves
that the candidate or its descendants are the sole listener owners. The
supervisor control socket must be a mode-`0600` Unix socket owned by the
expected uid and report one accepting, non-retiring backend. Candidate identity,
socket status, health, and readiness must remain stable through the functional
canary. Bootstrap, structural acceptance, timeout, signal, or canary failure
invokes the standalone rollback command automatically; a hard interruption is
recovered from the phase journal before a later activation may proceed.

The successful activation output prints the exact retained backup. To roll back
later, use that path and the installed supervisor path:

```bash
deploy/macos/rollback-launchagent-supervisor.sh \
  --backup '<printed-backup-path>' \
  --backup-sha256 '<printed-plist-sha256>' \
  --rollback-artifact '<rollback-program-destination>' '<bundle-artifact>' \
    '<printed-program-sha256>' '<mode>' \
  --expected-program "$HOME/bin/subrouter-supervisor"
```

Use the complete copy-pasteable command printed by successful activation; it
contains one `--rollback-artifact DEST ARTIFACT SHA MODE` entry for the rollback
program and each literal executable dependency discoverable from its plist or
shell wrapper. The mode-`0700` bundle contains immutable copies named by their
SHA-256; activation also writes the same identities to the printed mode-`0600`
manifest beside the retained plist. Preserve the complete bundle, not only the
plist path.

Standalone rollback refuses a mismatched installed plist, loaded program, or
changed PID. Before requesting bootout, it verifies the retained plist and all
bundle artifacts against the activation hashes. The installed rollback-program
destinations may have received ordinary upgrades since activation; after exact
loaded-service identity and full launchd-label, captured-PID, and listener
absence are proven, rollback atomically restores the byte-checked artifact
copies over those destinations. It then requires the restored program identity,
health, and readiness. The script is host-neutral; customize
non-default installations with `SUBROUTER_LABEL`, `SUBROUTER_PLIST`,
`SUBROUTER_LAUNCHD_DOMAIN`, `SUBROUTER_HEALTH_URL`, and `SUBROUTER_READY_URL`.
`--expected-running-program` is reserved for transaction recovery when launchd
is still removing the captured legacy process; normal standalone rollback does
not need it.
If the installed plist is already absent, rollback proceeds only after proving
the launchd label and listener are absent, then restores the retained backup.

### Worker upgrade

Replace the worker binary atomically, then ask the stable supervisor to create a generation:

```bash
install -m 0755 ./subrouter /usr/local/bin/subrouter.new
mv -f /usr/local/bin/subrouter.new /usr/local/bin/subrouter
curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock -X POST http://localhost/_subrouter/upgrade
curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status | jq
```

The control socket lives in the service's state directory (`/var/lib/subrouter/supervisor.sock` for a `_subrouter` service user) because a non-root service cannot bind inside root-owned `/var/run`. The migration script writes the chosen path into the LaunchDaemon `--control-socket` argument, and `subrouter-autoupdate.sh` reads it back from the plist.

`deploy/macos/subrouter-autoupdate.sh` performs the same sequence with release checksum verification and automatic worker-binary rollback when readiness fails.

## Linux and GCP

The GCP VM runs the same supervisor. Migrate once, then every worker upgrade
preserves connections:

```bash
sudo deploy/gcp/migrate-systemd-to-supervisor.sh            # prepare and review
sudo deploy/gcp/migrate-systemd-to-supervisor.sh --activate # one-time listener transition
```

The prepared unit reuses the existing worker arguments, the existing `User=` and
`Group=`, and `HOME` from the running service. It deliberately omits
`StateDirectory=`, because systemd chowns that directory tree to the unit's user
and a supervised unit running as root would take `/var/lib/subrouter` away from
the unsupervised service it may need to roll back to.

Activation stops `subrouter.socket`, since socket activation and the supervisor
cannot both own the port. Rollback re-enables it, which matters because the
unsupervised unit declares `Requires=subrouter.socket` and will not start
without it.

`deploy/gcp/subrouter-autoupdate.sh` then upgrades through the control socket
and falls back to `systemctl restart` only when no supervisor is present.

### What socket activation does and does not preserve

Measured on the GCP VM:

| upgrade path | new connections | established connection | in-flight stream |
| --- | --- | --- | --- |
| `systemctl restart` (socket-activated) | accepted | closed by peer | cut |
| supervised worker upgrade | accepted | preserved | continued to completion |

Socket activation keeps the listener open, so clients are never refused. It does
not keep established connections alive: those descriptors belong to the worker
being replaced. For agent traffic, where one turn is one long streaming
response, that difference is a cancelled turn.

## Legacy unsupervised handoff

The following guarded path is only for installations that have not migrated yet. It still has a listener restart. New builds enter drain mode and wait for in-flight proxy requests on SIGTERM/SIGINT, but clients can see a connection gap because the worker owns the public listener.

On macOS, use the new binary's `install-daemon` path so launchd re-registers the LaunchAgent. Modern launchd can attach launch constraints to the binary it bootstrapped; a plain `mv` plus `launchctl kickstart -k` can fail with `OS_REASON_CODESIGNING | Launch Constraint Violation`.

Run from the Subrouter checkout:

```bash
set -euo pipefail

bin="${SUBROUTER_BIN:-$HOME/bin/subrouter}"
label="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
service="gui/$(id -u)/$label"
health_url="${SUBROUTER_HEALTH_URL:-http://127.0.0.1:31415/_subrouter/health}"
backup="$bin.backup-$(date +%Y%m%d-%H%M%S)"
next="$(mktemp "$bin.next.XXXXXX")"
smoke_log="${TMPDIR:-/tmp}/subrouter-upgrade-smoke.log"

rollback() {
  if [ -x "$backup" ]; then
    "$backup" install-daemon \
      --addr 127.0.0.1:31415 \
      --cx-switch-interval 10m \
      --working-directory "$PWD"
  fi
}

go build -ldflags=-linkmode=external -o "$next" ./cmd/subrouter
chmod 0755 "$next"
if command -v codesign >/dev/null 2>&1; then
  codesign --force --sign - "$next" >/dev/null
  codesign --verify --verbose=4 "$next"
fi
"$next" help >/dev/null
curl -fsS "$health_url" >/dev/null

rm -f "$smoke_log"
"$next" serve --addr 127.0.0.1:31416 --fetch-usage=false --sr-switch-interval 0 >"$smoke_log" 2>&1 &
smoke_pid="$!"
smoke_ok=0
for _ in $(seq 1 40); do
  if curl -fsS http://127.0.0.1:31416/_subrouter/health >/dev/null; then
    smoke_ok=1
    break
  fi
  sleep 0.25
done
kill "$smoke_pid" >/dev/null 2>&1 || true
wait "$smoke_pid" >/dev/null 2>&1 || true
if [ "$smoke_ok" != 1 ]; then
  cat "$smoke_log" >&2
  exit 1
fi

before_pid="$(launchctl print "$service" | awk '/pid =/ {print $3; exit}')"
before_sha="$(shasum -a 256 "$bin" | awk '{print $1}')"

cp -p "$bin" "$backup"
"$next" install-daemon \
  --addr 127.0.0.1:31415 \
  --sr-switch-interval 10m \
  --working-directory "$PWD"

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
  rollback
  exit 1
fi
if [ "${before_pid:-}" = "${after_pid:-}" ]; then
  echo "subrouter pid did not change" >&2
  rollback
  exit 1
fi
```

Rollback uses the printed backup path:

```bash
set -euo pipefail

backup="<printed-backup-path>"

"$backup" install-daemon \
  --addr 127.0.0.1:31415 \
  --cx-switch-interval 10m \
  --working-directory "$(pwd)"
curl -fsS http://127.0.0.1:31415/_subrouter/health
```

## Rules

- Keep the public URL stable. Codex desktop and long-running CLI processes do not reliably adopt a new base URL mid-session.
- Check `/_subrouter/ready` before sending traffic to a process. A draining process returns 503.
- Use `POST /_subrouter/drain` from loopback before controlled shutdowns when you can.
- Use `install-daemon` for local macOS binary upgrades so launchd refreshes its launch constraint for the new binary.
- On Linux, install with `install-systemd` and keep `subrouter.socket` enabled. systemd owns the public TCP listener and passes it to the Subrouter worker, so new client connections queue across worker restarts instead of hitting a closed port.
- Do not edit the live binary in place. Build a separate binary, smoke-test it, then let `install-daemon` copy it into place.
- Do not use `kill -9`. Launchd owns restart policy and environment.
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

## Supervisor status

The permissioned Unix control socket reports the active generation and the connection count pinned to every draining generation. Browser pages cannot reach this socket or trigger upgrades:

```bash
curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status | jq
```

There is intentionally no drain timeout. A routine upgrade never terminates a worker that still owns a client connection.
