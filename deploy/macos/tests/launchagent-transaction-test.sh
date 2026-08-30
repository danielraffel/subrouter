#!/usr/bin/env bash
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/../../.." && pwd)"
MIGRATE="$ROOT/deploy/macos/migrate-launchagent-to-supervisor.sh"
ROLLBACK="$ROOT/deploy/macos/rollback-launchagent-supervisor.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/subrouter-launchagent-test.XXXXXX")"
trap 'if [ -f "$TMP/state" ]; then kill "$(cut -d "|" -f 2 "$TMP/state")" 2>/dev/null || true; fi; if [ "${KEEP_SUBROUTER_TEST_TMP:-0}" = 1 ]; then echo "kept $TMP" >&2; else rm -rf "$TMP"; fi' EXIT INT TERM

mkdir -p "$TMP/bin" "$TMP/home/Library/LaunchAgents" "$TMP/home/.subrouter" "$TMP/home/.subrouter-retiring"

cat >"$TMP/bin/launchctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  print)
    [ -f "$FAKE_LAUNCHD_STATE" ] || exit 113
    IFS='|' read -r program pid <"$FAKE_LAUNCHD_STATE"
    kill -0 "$pid" 2>/dev/null || exit 113
    printf 'program = %s\npid = %s\n' "$program" "$pid"
    ;;
  bootout)
    [ -f "$FAKE_LAUNCHD_STATE" ] || exit 113
    IFS='|' read -r program pid <"$FAKE_LAUNCHD_STATE"
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    if [ "${FAKE_BOOTOUT_REPLACE_PID:-0}" = 1 ]; then
      sleep 300 &
      printf '%s|%s\n' "$program" "$!" >"$FAKE_LAUNCHD_STATE"
    else
      rm -f "$FAKE_LAUNCHD_STATE"
    fi
    ;;
  bootstrap)
    if [ "${FAKE_BOOTSTRAP_FAIL_ONCE:-0}" = 1 ] && [ ! -f "${FAKE_LAUNCHD_STATE}.bootstrap-failed" ]; then
      : >"${FAKE_LAUNCHD_STATE}.bootstrap-failed"
      exit 5
    fi
    program="$(/usr/libexec/PlistBuddy -c 'Print :Program' "$3")"
    sleep 300 &
    printf '%s|%s\n' "$program" "$!" >"$FAKE_LAUNCHD_STATE"
    ;;
  *) exit 2 ;;
esac
SH
cat >"$TMP/bin/curl" <<'SH'
#!/bin/sh
case " $* " in
  *" --unix-socket "*)
    printf '%s\n' '{"accepting":true,"retiring":false,"active":{"id":"candidate"},"backends":[{}]}'
    ;;
esac
exit 0
SH
cat >"$TMP/bin/codesign" <<'SH'
#!/bin/sh
exit 0
SH
cat >"$TMP/bin/lsof" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ -f "$FAKE_LAUNCHD_STATE" ] || exit 1
IFS='|' read -r program pid <"$FAKE_LAUNCHD_STATE"
kill -0 "$pid" 2>/dev/null || exit 1
case " $* " in
  *" -t "*) printf '%s\n' "$pid" ;;
  *) printf 'fake-listener %s\n' "$pid" ;;
esac
SH
cat >"$TMP/bin/ps" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
pid="" field=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p) pid="$2"; shift 2 ;;
    -o) field="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -f "$FAKE_LAUNCHD_STATE" ] || exit 1
IFS='|' read -r program state_pid <"$FAKE_LAUNCHD_STATE"
[ "$pid" = "$state_pid" ] || exit 1
case "$field" in
  command=)
    if [ "$program" = "$SUBROUTER_SUPERVISOR_BIN" ]; then
      printf '%s supervise --control-socket %s --worker-bin %s -- --addr 127.0.0.1:43199\n' \
        "$program" "$SUBROUTER_CONTROL_SOCKET" "$SUBROUTER_BIN"
    else
      printf '%s serve --addr 127.0.0.1:43199\n' "$program"
    fi
    ;;
  lstart=) printf '%s\n' 'Sun Aug 30 12:00:00 2026' ;;
  ppid=) printf '%s\n' 1 ;;
  *) exit 2 ;;
esac
SH
cat >"$TMP/bin/stat" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}|${2:-}" in
  '-f|%HT') printf '%s\n' Socket ;;
  '-f|%Lp')
    if [ "${3:-}" = "$SUBROUTER_CONTROL_SOCKET" ]; then printf '%s\n' 600; else /usr/bin/stat "$@"; fi
    ;;
  '-f|%u') printf '%s\n' "$(id -u)" ;;
  *) exec /usr/bin/stat "$@" ;;
esac
SH
chmod +x "$TMP/bin/launchctl" "$TMP/bin/curl" "$TMP/bin/codesign" "$TMP/bin/lsof" "$TMP/bin/ps" "$TMP/bin/stat"

legacy="$TMP/home/bin/subrouter-legacy"
legacy_dependency="$TMP/home/bin/subrouter-legacy-worker"
worker="$TMP/home/bin/subrouter"
supervisor="$TMP/home/bin/subrouter-supervisor"
mkdir -p "$(dirname "$legacy")"
cat >"$legacy_dependency" <<'SH'
#!/bin/sh
exit 0
SH
cat >"$legacy" <<EOF
#!/bin/sh
exec "$legacy_dependency" \\
  "\$@"
EOF
cat >"$worker" <<'SH'
#!/bin/sh
if [ -n "${FAKE_WORKER_LOG:-}" ]; then
  printf '%s|%s|%s\n' "${SUBROUTER_STATE_DIR:-}" "$*" "${SUBROUTER_RETIRING_STATE_DIR:-}" >>"$FAKE_WORKER_LOG"
fi
case "${1:-}" in
  help) echo ' subrouter supervise --worker-bin PATH ' ;;
  doctor) echo '{"status":"ok"}' ;;
  codex) echo '{"comparison":{"ok":true}}' ;;
esac
exit 0
SH
chmod +x "$legacy" "$legacy_dependency" "$worker"
cat >"$TMP/preflight" <<'SH'
#!/bin/sh
exit 0
SH
cat >"$TMP/canary-fail" <<'SH'
#!/bin/sh
exit 1
SH
cat >"$TMP/canary-ok" <<'SH'
#!/bin/sh
exit 0
SH
chmod +x "$TMP/preflight" "$TMP/canary-fail" "$TMP/canary-ok"

label="test.subrouter.launchagent"
plist="$TMP/home/Library/LaunchAgents/$label.plist"
write_plist() {
  local program="$1" mode="$2"
  cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$label</string>
<key>Program</key><string>$program</string>
<key>ProgramArguments</key><array><string>$program</string><string>$mode</string><string>--addr</string><string>127.0.0.1:43199</string></array>
<key>EnvironmentVariables</key><dict><key>SUBROUTER_STATE_DIR</key><string>$TMP/home/.subrouter-retiring</string></dict>
</dict></plist>
EOF
}

write_wrapper_plist() {
  cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$label</string>
<key>Program</key><string>$legacy</string>
<key>ProgramArguments</key><array><string>$legacy</string></array>
<key>EnvironmentVariables</key><dict>
  <key>SUBROUTER_STATE_DIR</key><string>$TMP/home/.subrouter-retiring</string>
  <key>LEGACY_ONLY</key><string>preserved</string>
</dict>
</dict></plist>
EOF
}

stop_fake_job() {
  if [ -f "$FAKE_LAUNCHD_STATE" ]; then
    kill "$(cut -d '|' -f 2 "$FAKE_LAUNCHD_STATE")" 2>/dev/null || true
    rm -f "$FAKE_LAUNCHD_STATE"
  fi
}

reset_legacy() {
  stop_fake_job
  rm -rf "${plist}.supervisor-transaction"
  rm -f "${FAKE_LAUNCHD_STATE}.bootstrap-failed"
  write_plist "$legacy" serve
  launchctl bootstrap "gui/$(id -u)" "$plist"
  "$MIGRATE" >/dev/null
}

export PATH="$TMP/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export HOME="$TMP/home"
export FAKE_LAUNCHD_STATE="$TMP/state"
export SUBROUTER_LABEL="$label"
export SUBROUTER_PLIST="$plist"
export SUBROUTER_BIN="$worker"
export SUBROUTER_SUPERVISOR_BIN="$supervisor"
export SUBROUTER_STATE_DIR="$TMP/home/.subrouter"
export SUBROUTER_CONTROL_SOCKET="$TMP/home/.subrouter/supervisor.sock"
export SUBROUTER_ABSENCE_ATTEMPTS=10 SUBROUTER_ABSENCE_INTERVAL=0.01
export SUBROUTER_BOOTSTRAP_ATTEMPTS=2 SUBROUTER_BOOTSTRAP_INTERVAL=0.01
export SUBROUTER_HEALTH_ATTEMPTS=2 SUBROUTER_HEALTH_INTERVAL=0.01

write_plist "$legacy" serve
launchctl bootstrap "gui/$(id -u)" "$plist"
"$MIGRATE" >"$TMP/prepare.out" 2>"$TMP/prepare.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:SUBROUTER_STATE_DIR' "${plist}.supervised")" = "$SUBROUTER_STATE_DIR" ]
if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
  SUBROUTER_CANARY_TIMEOUT=0 "$MIGRATE" --activate \
  >"$TMP/invalid-timeout.out" 2>"$TMP/invalid-timeout.err"; then
  echo "invalid canary timeout unexpectedly accepted" >&2
  exit 1
fi
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
grep -q 'functional canary timeout must be a positive integer' "$TMP/invalid-timeout.err"
echo "PASS invalid callback timeout was rejected before activation"

if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
  "$MIGRATE" --activate >"$TMP/migrate.out" 2>"$TMP/migrate.err"; then
  echo "canary failure unexpectedly accepted" >&2
  exit 1
fi
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'functional canary failed; legacy LaunchAgent restored' "$TMP/migrate.err"
identity_manifest="$(find "$(dirname "$plist")" -maxdepth 2 -name 'legacy.plist.identity' -print -quit)"
[ -n "$identity_manifest" ]
grep -Fq "  $legacy_dependency  " "$identity_manifest"
echo "PASS canary failure automatically restored the exact legacy plist"

backup="$TMP/manual-backup.plist"
cp -p "$plist" "$backup"
backup_sha="$(shasum -a 256 "$backup" | awk '{print $1}')"
legacy_sha="$(shasum -a 256 "$legacy" | awk '{print $1}')"
legacy_dependency_sha="$(shasum -a 256 "$legacy_dependency" | awk '{print $1}')"
manual_bundle="$TMP/manual-bundle"
mkdir -m 0700 "$manual_bundle"
legacy_artifact="$manual_bundle/$legacy_sha-legacy"
dependency_artifact="$manual_bundle/$legacy_dependency_sha-worker"
cp -p "$legacy" "$legacy_artifact"
cp -p "$legacy_dependency" "$dependency_artifact"
rollback_identity=(--backup "$backup" --backup-sha256 "$backup_sha" \
  --rollback-artifact "$legacy" "$legacy_artifact" "$legacy_sha" 755 \
  --rollback-artifact "$legacy_dependency" "$dependency_artifact" "$legacy_dependency_sha" 755)
launchctl bootout "gui/$(id -u)/$label"
write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
"$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/rollback.out" 2>"$TMP/rollback.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'rollback LaunchAgent healthy and ready' "$TMP/rollback.out"
echo "PASS standalone rollback enforced identity and restored the exact legacy plist"

launchctl bootout "gui/$(id -u)/$label"
write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
IFS='|' read -r _ mismatch_pid <"$FAKE_LAUNCHD_STATE"
printf '%s|%s\n' "$TMP/home/bin/unexpected" "$mismatch_pid" >"$FAKE_LAUNCHD_STATE"
if "$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/program-mismatch.out" 2>"$TMP/program-mismatch.err"; then
  echo "mismatched loaded program unexpectedly accepted" >&2
  exit 1
fi
grep -q 'runs unexpected program' "$TMP/program-mismatch.err"
stop_fake_job
echo "PASS standalone rollback rejected a mismatched loaded program"

write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
if FAKE_BOOTOUT_REPLACE_PID=1 "$ROLLBACK" "${rollback_identity[@]}" \
  --expected-program "$supervisor" >"$TMP/pid-mismatch.out" 2>"$TMP/pid-mismatch.err"; then
  echo "changed pid during removal unexpectedly accepted" >&2
  exit 1
fi
grep -q 'changed pid during removal' "$TMP/pid-mismatch.err"
stop_fake_job
echo "PASS standalone rollback rejected a changed pid during removal"

write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
tampered_backup="$TMP/tampered-backup.plist"
cp -p "$backup" "$tampered_backup"
printf '\n' >>"$tampered_backup"
if "$ROLLBACK" --backup "$tampered_backup" --backup-sha256 "$backup_sha" \
  --rollback-artifact "$legacy" "$legacy_artifact" "$legacy_sha" 755 \
  --rollback-artifact "$legacy_dependency" "$dependency_artifact" "$legacy_dependency_sha" 755 \
  --expected-program "$supervisor" >"$TMP/tampered-backup.out" 2>"$TMP/tampered-backup.err"; then
  echo "tampered rollback plist unexpectedly accepted" >&2
  exit 1
fi
grep -q 'rollback plist content identity check failed' "$TMP/tampered-backup.err"
launchctl print "gui/$(id -u)/$label" >/dev/null
stop_fake_job
echo "PASS standalone rollback rejected a tampered backup before bootout"

legacy_pristine="$TMP/legacy-pristine"
dependency_pristine="$TMP/dependency-pristine"
cp -p "$legacy" "$legacy_pristine"
cp -p "$legacy_dependency" "$dependency_pristine"
write_plist "$supervisor" supervise
launchctl bootstrap "gui/$(id -u)" "$plist"
printf '\n# changed\n' >>"$legacy"
printf '\n# upgraded dependency\n' >>"$legacy_dependency"
"$ROLLBACK" "${rollback_identity[@]}" --expected-program "$supervisor" \
  >"$TMP/changed-destination.out" 2>"$TMP/changed-destination.err"
[ "$(shasum -a 256 "$legacy" | awk '{print $1}')" = "$legacy_sha" ]
[ "$(shasum -a 256 "$legacy_dependency" | awk '{print $1}')" = "$legacy_dependency_sha" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
cmp -s "$legacy" "$legacy_pristine"
cmp -s "$legacy_dependency" "$dependency_pristine"
echo "PASS standalone rollback exactly restored benignly upgraded destinations"

stop_fake_job
rm -f "$plist"
"$ROLLBACK" "${rollback_identity[@]}" >"$TMP/missing-plist.out" 2>"$TMP/missing-plist.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
grep -q 'rollback LaunchAgent healthy and ready' "$TMP/missing-plist.out"
echo "PASS standalone rollback recovered after installed plist absence"

for fault_phase in candidate_plist_installing candidate_plist_installed legacy_bootout_requested legacy_absent candidate_bootstrap_requested candidate_bootstrapped structural_accepted canary_completed; do
  reset_legacy
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    SUBROUTER_FAULT_INJECT_PHASE="$fault_phase" "$MIGRATE" --activate \
    >"$TMP/fault-$fault_phase.out" 2>"$TMP/fault-$fault_phase.err"; then
    echo "fault injection at $fault_phase unexpectedly succeeded" >&2
    exit 1
  fi
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
  [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
  [ ! -e "${plist}.supervisor-transaction" ]
done
echo "PASS TERM injection at every live transaction boundary restored healthy legacy"

for hard_phase in candidate_plist_installing candidate_bootstrap_requested; do
  reset_legacy
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    SUBROUTER_FAULT_INJECT_HARD_PHASE="$hard_phase" "$MIGRATE" --activate \
    >"$TMP/hard-fault-$hard_phase.out" 2>"$TMP/hard-fault-$hard_phase.err"; then
    echo "hard fault injection at $hard_phase unexpectedly succeeded" >&2
    exit 1
  fi
  [ -d "${plist}.supervisor-transaction" ]
  [ "$(cat "${plist}.supervisor-transaction/phase")" = "$hard_phase" ]
  if [ "$hard_phase" = candidate_plist_installing ]; then
    [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
    [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
  else
    [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
    if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
      echo "candidate unexpectedly bootstrapped before persisted intent boundary" >&2
      exit 1
    fi
  fi
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    "$MIGRATE" --activate >"$TMP/reentry-$hard_phase.out" 2>"$TMP/reentry-$hard_phase.err"; then
    echo "reentry recovery at $hard_phase unexpectedly continued activation" >&2
    exit 1
  fi
  grep -q "recovered interrupted transaction phase $hard_phase to legacy" "$TMP/reentry-$hard_phase.err"
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
  [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
done
echo "PASS pre-mutation SIGKILL journals recovered exact legacy on reentry"

for mutation in candidate_plist_restore candidate_bootstrap; do
  reset_legacy
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    SUBROUTER_FAULT_INJECT_HARD_AFTER_MUTATION="$mutation" "$MIGRATE" --activate \
    >"$TMP/post-mutation-$mutation.out" 2>"$TMP/post-mutation-$mutation.err"; then
    echo "post-mutation hard fault at $mutation unexpectedly succeeded" >&2
    exit 1
  fi
  [ -d "${plist}.supervisor-transaction" ]
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
  if [ "$mutation" = candidate_plist_restore ]; then
    [ "$(cat "${plist}.supervisor-transaction/phase")" = candidate_plist_installing ]
    [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
  else
    [ "$(cat "${plist}.supervisor-transaction/phase")" = candidate_bootstrap_requested ]
    [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
  fi
  if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
    "$MIGRATE" --activate >"$TMP/post-mutation-reentry-$mutation.out" \
    2>"$TMP/post-mutation-reentry-$mutation.err"; then
    echo "post-mutation reentry at $mutation unexpectedly continued activation" >&2
    exit 1
  fi
  grep -q 'recovered interrupted transaction phase .* to legacy' \
    "$TMP/post-mutation-reentry-$mutation.err"
  [ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
  [ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
done
echo "PASS post-mutation SIGKILL windows recovered exact healthy legacy on reentry"

reset_legacy
FAKE_BOOTSTRAP_FAIL_ONCE=1 SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" \
  SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" "$MIGRATE" --activate \
  >"$TMP/bootstrap-retry.out" 2>"$TMP/bootstrap-retry.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$supervisor" ]
echo "PASS bootstrap retry occurred only after classified full absence"

reset_legacy
export FAKE_WORKER_LOG="$TMP/worker.log"
SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" "$MIGRATE" --activate \
  >"$TMP/default-preflight.out" 2>"$TMP/default-preflight.err"
grep -Fq "$SUBROUTER_STATE_DIR|codex isolation-check --json --retiring-state-dir $TMP/home/.subrouter-retiring|" "$FAKE_WORKER_LOG"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$supervisor" ]
unset FAKE_WORKER_LOG
echo "PASS default preflight compared candidate and retiring state roots without shell evaluation"

reset_legacy
if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-ok" \
  "$MIGRATE" --activate --retiring-state-dir "$SUBROUTER_STATE_DIR" \
  >"$TMP/equal-root.out" 2>"$TMP/equal-root.err"; then
  echo "equal candidate and retiring roots unexpectedly accepted" >&2
  exit 1
fi
grep -q 'candidate and retiring state roots must be different' "$TMP/equal-root.err"
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
echo "PASS equal candidate and retiring state roots failed before live mutation"

worker_args_json="$TMP/worker-serve-args.json"
candidate_env_json="$TMP/candidate-env.json"
private_token_file="$TMP/private-token-file"
printf '%s\n' 'test-only-placeholder' >"$private_token_file"
chmod 0600 "$private_token_file"
cat >"$worker_args_json" <<EOF
["--quota-mode", "safe", "--token-file", "$private_token_file"]
EOF
cat >"$candidate_env_json" <<EOF
{"SUBROUTER_TOKEN_FILE": "$private_token_file"}
EOF
ln -s "$worker_args_json" "$TMP/worker-args-link.json"
reset_legacy
write_wrapper_plist
if "$MIGRATE" --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$TMP/worker-args-link.json" \
  >"$TMP/wrapper-symlink.out" 2>"$TMP/wrapper-symlink.err"; then
  echo "symlink JSON input unexpectedly accepted" >&2
  exit 1
fi
grep -q 'must be a regular non-symlink file' "$TMP/wrapper-symlink.err"

cat >"$TMP/worker-args-duplicate.json" <<'EOF'
["serve", "--addr=127.0.0.1:1"]
EOF
if "$MIGRATE" --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$TMP/worker-args-duplicate.json" \
  >"$TMP/wrapper-duplicate.out" 2>"$TMP/wrapper-duplicate.err"; then
  echo "embedded serve/addr arguments unexpectedly accepted" >&2
  exit 1
fi
grep -Eq 'must not embed (the serve subcommand|--addr)' "$TMP/wrapper-duplicate.err"

assert_candidate_env_rejected() {
  local name="$1" input="$2" pattern="$3"
  if "$MIGRATE" --public-addr 127.0.0.1:43199 \
    --worker-serve-args-json "$worker_args_json" --candidate-env-json "$input" \
    >"$TMP/env-$name.out" 2>"$TMP/env-$name.err"; then
    echo "unsafe candidate environment $name unexpectedly accepted" >&2
    exit 1
  fi
  grep -Eq "$pattern" "$TMP/env-$name.err"
}

cat >"$TMP/env-raw-secret.json" <<'EOF'
{"SUBROUTER_ADMIN_TOKEN": "raw-secret-value"}
EOF
assert_candidate_env_rejected raw-secret "$TMP/env-raw-secret.json" 'raw secrets and non-file overrides are forbidden'
cat >"$TMP/env-non-file-key.json" <<'EOF'
{"CANDIDATE_MODE": "isolated"}
EOF
assert_candidate_env_rejected non-file-key "$TMP/env-non-file-key.json" 'keys must match SUBROUTER_.*_FILE'
cat >"$TMP/env-relative.json" <<'EOF'
{"SUBROUTER_TOKEN_FILE": "relative/token"}
EOF
assert_candidate_env_rejected relative "$TMP/env-relative.json" 'must be an absolute path'
cat >"$TMP/env-missing.json" <<EOF
{"SUBROUTER_TOKEN_FILE": "$TMP/missing-token-file"}
EOF
assert_candidate_env_rejected missing "$TMP/env-missing.json" 'is not safely openable'
ln -s "$private_token_file" "$TMP/token-file-link"
cat >"$TMP/env-value-symlink.json" <<EOF
{"SUBROUTER_TOKEN_FILE": "$TMP/token-file-link"}
EOF
assert_candidate_env_rejected symlink "$TMP/env-value-symlink.json" 'is not safely openable'
unsafe_token_file="$TMP/unsafe-token-file"
printf '%s\n' 'test-only-placeholder' >"$unsafe_token_file"
chmod 0644 "$unsafe_token_file"
cat >"$TMP/env-unsafe-mode.json" <<EOF
{"SUBROUTER_TOKEN_FILE": "$unsafe_token_file"}
EOF
assert_candidate_env_rejected unsafe-mode "$TMP/env-unsafe-mode.json" 'has group or other permissions'

"$MIGRATE" --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$worker_args_json" \
  --candidate-env-json "$candidate_env_json" \
  >"$TMP/wrapper-prepare.out" 2>"$TMP/wrapper-prepare.err"
wrapper_prepared="${plist}.supervised"
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$wrapper_prepared")" = "$supervisor" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:1' "$wrapper_prepared")" = supervise ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:3' "$wrapper_prepared")" = 127.0.0.1:43199 ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:9' "$wrapper_prepared")" = --quota-mode ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:SUBROUTER_TOKEN_FILE' "$wrapper_prepared")" = "$private_token_file" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:LEGACY_ONLY' "$wrapper_prepared")" = preserved ]
if /usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:SUBROUTER_TOKEN_FILE' "$plist" >/dev/null 2>&1; then
  echo "candidate environment override leaked into retiring plist" >&2
  exit 1
fi

if SUBROUTER_PREFLIGHT_CALLBACK="$TMP/preflight" SUBROUTER_CANARY_CALLBACK="$TMP/canary-fail" \
  "$MIGRATE" --activate --public-addr 127.0.0.1:43199 \
  --worker-serve-args-json "$worker_args_json" \
  --candidate-env-json "$candidate_env_json" \
  >"$TMP/wrapper-activate.out" 2>"$TMP/wrapper-activate.err"; then
  echo "wrapper-backed failing canary unexpectedly accepted" >&2
  exit 1
fi
grep -q 'functional canary failed; legacy LaunchAgent restored' "$TMP/wrapper-activate.err"
[ "$(/usr/libexec/PlistBuddy -c 'Print :Program' "$plist")" = "$legacy" ]
[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$plist")" = "$legacy" ]
[ "$(launchctl print "gui/$(id -u)/$label" | awk '$1 == "program" { print $3 }')" = "$legacy" ]
echo "PASS wrapper-only plist prepared safely and canary failure restored exact legacy"
