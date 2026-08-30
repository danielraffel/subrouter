#!/usr/bin/env bash
# Put a per-user Subrouter LaunchAgent behind the supervisor, so upgrading the
# binary stops cutting connections that coding agents are actively streaming on.
#
# The existing migrate-launchdaemon-to-supervisor.sh handles the system-wide
# LaunchDaemon (/Library/LaunchDaemons, `system` domain, root). A developer
# machine runs a per-user LaunchAgent instead (~/Library/LaunchAgents,
# `gui/<uid>` domain, no sudo), which that script cannot touch.
#
# Without the supervisor, replacing ~/bin/subrouter and restarting the agent
# closes every established connection: an agent mid-turn loses its response.
# With it, the supervisor owns the listener, health-checks the replacement, and
# lets the old worker finish the connections it already accepted.
#
# The one-time transition below still drops in-flight connections, because the
# unsupervised process owns those file descriptors. Run it when no agent is
# mid-turn. Every upgrade after it is non-disruptive.
set -euo pipefail

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/launchagent-transition-lib.sh"

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
PLIST="${SUBROUTER_PLIST:-$HOME/Library/LaunchAgents/${LABEL}.plist}"
WORKER_BIN="${SUBROUTER_BIN:-$HOME/bin/subrouter}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-$HOME/bin/subrouter-supervisor}"
STATE_DIR="${SUBROUTER_STATE_DIR:-$HOME/.subrouter}"
CONTROL_SOCKET="${SUBROUTER_CONTROL_SOCKET:-${STATE_DIR}/supervisor.sock}"
DOMAIN="gui/$(id -u)"

PREFLIGHT_CALLBACK="${SUBROUTER_PREFLIGHT_CALLBACK:-}"
CANARY_CALLBACK="${SUBROUTER_CANARY_CALLBACK:-}"
RETIRING_STATE_DIR="${SUBROUTER_RETIRING_STATE_DIR:-}"
PUBLIC_ADDR_OVERRIDE="${SUBROUTER_PUBLIC_ADDR:-}"
WORKER_SERVE_ARGS_JSON="${SUBROUTER_WORKER_SERVE_ARGS_JSON:-}"
CANDIDATE_ENV_JSON="${SUBROUTER_CANDIDATE_ENV_JSON:-}"
PREFLIGHT_TIMEOUT="${SUBROUTER_PREFLIGHT_TIMEOUT:-120}"
CANARY_TIMEOUT="${SUBROUTER_CANARY_TIMEOUT:-300}"

die() { echo "migrate-launchagent-to-supervisor: $*" >&2; exit 1; }
run_verified_recovery() {
  local recovery recovery_sha_file recovery_sha
  for recovery in "$@"; do
    [ -x "$recovery" ] || continue
    recovery_sha_file="${recovery}.sha256"
    [ -f "$recovery_sha_file" ] && [ ! -L "$recovery_sha_file" ] || continue
    recovery_sha="$(cat "$recovery_sha_file")"
    if verify_file_sha256 "$recovery" "$recovery_sha" && "$recovery"; then
      return 0
    fi
  done
  return 1
}
activate=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --activate) activate=1; shift ;;
    --preflight-callback)
      [ "$#" -ge 2 ] || die "--preflight-callback requires an executable path"
      PREFLIGHT_CALLBACK="$2"; shift 2 ;;
    --canary-callback)
      [ "$#" -ge 2 ] || die "--canary-callback requires an executable path"
      CANARY_CALLBACK="$2"; shift 2 ;;
    --retiring-state-dir)
      [ "$#" -ge 2 ] || die "--retiring-state-dir requires a path"
      RETIRING_STATE_DIR="$2"; shift 2 ;;
    --public-addr)
      [ "$#" -ge 2 ] || die "--public-addr requires HOST:PORT"
      PUBLIC_ADDR_OVERRIDE="$2"; shift 2 ;;
    --worker-serve-args-json)
      [ "$#" -ge 2 ] || die "--worker-serve-args-json requires a file path"
      WORKER_SERVE_ARGS_JSON="$2"; shift 2 ;;
    --candidate-env-json)
      [ "$#" -ge 2 ] || die "--candidate-env-json requires a file path"
      CANDIDATE_ENV_JSON="$2"; shift 2 ;;
    -h|--help)
      echo "usage: $0 [--activate] [--preflight-callback PATH] [--canary-callback PATH] [--retiring-state-dir PATH] [--public-addr HOST:PORT --worker-serve-args-json FILE] [--candidate-env-json FILE]"
      exit 0 ;;
    *) die "unknown argument $1" ;;
  esac
done

if [ -n "$PUBLIC_ADDR_OVERRIDE" ] || [ -n "$WORKER_SERVE_ARGS_JSON" ]; then
  [ -n "$PUBLIC_ADDR_OVERRIDE" ] && [ -n "$WORKER_SERVE_ARGS_JSON" ] \
    || die "--public-addr and --worker-serve-args-json must be provided together"
fi
for json_input in "$WORKER_SERVE_ARGS_JSON" "$CANDIDATE_ENV_JSON"; do
  [ -z "$json_input" ] || { [ -f "$json_input" ] && [ ! -L "$json_input" ]; } \
    || die "JSON input $json_input must be a regular non-symlink file"
done

export SUBROUTER_TRANSITION_NAME="migrate-launchagent-to-supervisor"
export SUBROUTER_STATE_DIR="$STATE_DIR"

if [ "$activate" -eq 1 ]; then
  positive_integer "$PREFLIGHT_TIMEOUT" \
    || die "preflight timeout must be a positive integer"
  positive_integer "$CANARY_TIMEOUT" \
    || die "functional canary timeout must be a positive integer"
fi

TRANSACTION_DIR="${PLIST}.supervisor-transaction"
if [ "$activate" -eq 1 ]; then
  if ! mkdir -m 0700 "$TRANSACTION_DIR" 2>/dev/null; then
    phase="$(cat "$TRANSACTION_DIR/phase" 2>/dev/null || true)"
    case "$phase" in
      ''|prelive)
        rm -rf "$TRANSACTION_DIR"
        die "cleared an incomplete pre-live transaction; rerun activation"
        ;;
      candidate_plist_installing)
        recovery_candidates=(
          "$TRANSACTION_DIR/recover-legacy-unchanged"
          "$TRANSACTION_DIR/recover-legacy-running"
        )
        ;;
      candidate_bootstrap_requested)
        recovery_candidates=(
          "$TRANSACTION_DIR/recover-legacy-running"
          "$TRANSACTION_DIR/recover-candidate-running"
        )
        ;;
      candidate_plist_installed|legacy_bootout_requested|bootout_requested|legacy_absent)
        recovery_candidates=("$TRANSACTION_DIR/recover-legacy-running")
        ;;
      *) recovery_candidates=("$TRANSACTION_DIR/recover-candidate-running") ;;
    esac
    run_verified_recovery "${recovery_candidates[@]}" \
      || die "reentry recovery failed for transaction phase $phase"
    rm -rf "$TRANSACTION_DIR"
    die "recovered interrupted transaction phase $phase to legacy; rerun activation"
  fi
  printf 'prelive\n' >"$TRANSACTION_DIR/phase"
  transaction_active=1
  prelive_transaction_exit() {
    local status=$?
    trap - EXIT INT TERM
    [ -d "$TRANSACTION_DIR" ] && rm -rf "$TRANSACTION_DIR"
    [ "$status" -ne 0 ] || status=1
    exit "$status"
  }
  trap prelive_transaction_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
fi

[ -f "$PLIST" ] || die "$PLIST not found"
[ -x "$WORKER_BIN" ] || die "$WORKER_BIN is not executable"
"$WORKER_BIN" help 2>/dev/null | grep -q ' supervise ' \
  || die "$WORKER_BIN does not support supervise; upgrade it first"

# A separate copy, because routine upgrades replace the worker and must never
# replace the supervisor that is holding the listener.
mkdir -p "$(dirname "$SUPERVISOR_BIN")"
cp -f "$WORKER_BIN" "${SUPERVISOR_BIN}.new"
chmod 0755 "${SUPERVISOR_BIN}.new"
mv -f "${SUPERVISOR_BIN}.new" "$SUPERVISOR_BIN"
# An ad-hoc signature keeps macOS from killing the copy with OS_REASON_CODESIGNING.
codesign -s - -f "$SUPERVISOR_BIN" >/dev/null 2>&1 || true

prepared="${PLIST}.supervised"
python3 - "$PLIST" "$prepared" "$SUPERVISOR_BIN" "$WORKER_BIN" "$CONTROL_SOCKET" "$STATE_DIR" \
  "$PUBLIC_ADDR_OVERRIDE" "$WORKER_SERVE_ARGS_JSON" "$CANDIDATE_ENV_JSON" <<'PY'
import json
import os
import plistlib
import re
import stat
import sys

(
    source,
    destination,
    supervisor_bin,
    worker_bin,
    control_socket,
    state_dir,
    public_addr_override,
    worker_args_path,
    candidate_env_path,
) = sys.argv[1:10]


def load_json_file(path):
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(path, flags)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise SystemExit(f"JSON input {path} is not a regular file")
        with os.fdopen(fd, "r", encoding="utf-8", closefd=False) as stream:
            return json.load(stream)
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise SystemExit(f"invalid JSON input {path}: {error}") from error
    finally:
        os.close(fd)


def validate_public_addr(value):
    host, separator, port_text = value.rpartition(":")
    if not separator or not host or not port_text.isdigit():
        raise SystemExit("public address must be HOST:PORT or [IPv6]:PORT")
    port = int(port_text)
    if not 1 <= port <= 65535:
        raise SystemExit("public address port must be between 1 and 65535")
    if (host.startswith("[") or host.endswith("]")) and not (
        host.startswith("[") and host.endswith("]")
    ):
        raise SystemExit("IPv6 public address must use [IPv6]:PORT form")


with open(source, "rb") as handle:
    plist = plistlib.load(handle)

arguments = list(plist.get("ProgramArguments") or [])
if not arguments:
    raise SystemExit("existing plist has no ProgramArguments")

# The supervisor owns the public address and supplies `serve` plus the worker's
# private socket itself, so strip both from the inherited arguments.
if worker_args_path:
    filtered = load_json_file(worker_args_path)
    if not isinstance(filtered, list) or not all(isinstance(item, str) for item in filtered):
        raise SystemExit("worker serve args JSON must be an array of strings")
    if any("\x00" in item for item in filtered):
        raise SystemExit("worker serve args must not contain NUL bytes")
    for argument in filtered:
        if argument == "serve":
            raise SystemExit("worker serve args must not embed the serve subcommand")
        if argument == "--addr" or argument.startswith("--addr="):
            raise SystemExit("worker serve args must not embed --addr")
    public_addr = public_addr_override
else:
    public_addr = None
    filtered = []
    filtered_source = arguments[1:]
    if filtered_source and filtered_source[0] == "serve":
        filtered_source = filtered_source[1:]
    i = 0
    while i < len(filtered_source):
        argument = filtered_source[i]
        if argument == "--addr":
            if i + 1 >= len(filtered_source):
                raise SystemExit("existing --addr has no value")
            public_addr = filtered_source[i + 1]
            i += 2
            continue
        if argument.startswith("--addr="):
            public_addr = argument.split("=", 1)[1]
            i += 1
            continue
        filtered.append(argument)
        i += 1
    if not public_addr:
        raise SystemExit(
            "could not find --addr in the existing plist; wrapper-backed services require "
            "--public-addr and --worker-serve-args-json"
        )

validate_public_addr(public_addr)

plist["Program"] = supervisor_bin
plist["ProgramArguments"] = [
    supervisor_bin,
    "supervise",
    "--addr", public_addr,
    "--control-socket", control_socket,
    "--worker-bin", worker_bin,
    "--",
    *filtered,
]
# Pin the candidate to the state root that passed the isolation preflight.
# launchd does not inherit the activating shell's environment, and a legacy
# rollback may intentionally continue using a separate untouched store.
environment = dict(plist.get("EnvironmentVariables") or {})
if candidate_env_path:
    overrides = load_json_file(candidate_env_path)
    if not isinstance(overrides, dict) or not all(
        isinstance(key, str) and isinstance(value, str)
        for key, value in overrides.items()
    ):
        raise SystemExit("candidate environment JSON must be an object of string values")
    for key, value in overrides.items():
        if not re.fullmatch(r"SUBROUTER_[A-Z0-9_]+_FILE", key):
            raise SystemExit(
                "candidate environment keys must match SUBROUTER_*_FILE; "
                "raw secrets and non-file overrides are forbidden"
            )
        if "\x00" in value or not os.path.isabs(value):
            raise SystemExit(f"candidate environment file for {key} must be an absolute path")
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        try:
            file_fd = os.open(value, flags)
        except OSError as error:
            raise SystemExit(f"candidate environment file for {key} is not safely openable: {error}") from error
        try:
            info = os.fstat(file_fd)
            if not stat.S_ISREG(info.st_mode):
                raise SystemExit(f"candidate environment file for {key} is not regular")
            if info.st_uid != os.getuid():
                raise SystemExit(f"candidate environment file for {key} is not owned by the current uid")
            if stat.S_IMODE(info.st_mode) & 0o077:
                raise SystemExit(f"candidate environment file for {key} has group or other permissions")
        finally:
            os.close(file_fd)
    environment.update(overrides)
environment["SUBROUTER_STATE_DIR"] = state_dir
plist["EnvironmentVariables"] = environment
# The supervisor must outlive its draining workers.
plist["ExitTimeOut"] = 600
plist["ThrottleInterval"] = 10

with open(destination, "wb") as handle:
    plistlib.dump(plist, handle, fmt=plistlib.FMT_XML, sort_keys=False)
print(public_addr)
PY

public_addr="$(python3 - "$prepared" <<'PY'
import plistlib, sys
with open(sys.argv[1], "rb") as handle:
    plist = plistlib.load(handle)
arguments = plist["ProgramArguments"]
print(arguments[arguments.index("--addr") + 1])
PY
)"
health_url="http://${public_addr}/_subrouter/health"
case "$public_addr" in
  0.0.0.0:*) health_url="http://127.0.0.1:${public_addr##*:}/_subrouter/health" ;;
  \[::\]:*) health_url="http://127.0.0.1:${public_addr#\[::\]:}/_subrouter/health" ;;
esac

echo "prepared $prepared"
if [ "$activate" -eq 0 ]; then
  cat <<EOF

Review the plist above, then activate with:
  $0 --activate --canary-callback /path/to/real-routed-canary

Activation restarts the agent once, which drops connections currently in
flight. Do it when no coding agent is mid-turn. Every upgrade after it
preserves connections.
EOF
  exit 0
fi

[ -n "$CANARY_CALLBACK" ] \
  || die "activation requires --canary-callback (or SUBROUTER_CANARY_CALLBACK)"
[ -x "$CANARY_CALLBACK" ] || die "canary callback $CANARY_CALLBACK is not executable"
if [ -n "$PREFLIGHT_CALLBACK" ]; then
  [ -x "$PREFLIGHT_CALLBACK" ] \
    || die "preflight callback $PREFLIGHT_CALLBACK is not executable"
fi

if [ -z "$RETIRING_STATE_DIR" ]; then
  RETIRING_STATE_DIR="$(plist_value "$PLIST" EnvironmentVariables:SUBROUTER_STATE_DIR || true)"
fi
[ -n "$RETIRING_STATE_DIR" ] \
  || die "retiring plist has no explicit SUBROUTER_STATE_DIR; pass --retiring-state-dir"
[ "$RETIRING_STATE_DIR" != "$STATE_DIR" ] \
  || die "candidate and retiring state roots must be different"

echo "running bounded preflight"
run_bounded_argv "credential isolation preflight" "$PREFLIGHT_TIMEOUT" \
  "$WORKER_BIN" codex isolation-check --json \
    --retiring-state-dir "$RETIRING_STATE_DIR" \
  || die "Codex isolation preflight failed"
if [ -n "$PREFLIGHT_CALLBACK" ]; then
  run_bounded_argv "deployment preflight" "$PREFLIGHT_TIMEOUT" "$PREFLIGHT_CALLBACK" \
    || die "preflight callback failed"
fi

bundle="${PLIST}.rollback-bundle-$(date +%Y%m%d-%H%M%S)-$$"
mkdir -m 0700 "$bundle"
backup="$bundle/legacy.plist"
copy_file_nofollow "$PLIST" "$backup" 0600
old_program="$(plist_program "$backup")"
require_plist_identity "$PLIST" "$LABEL" "$old_program"
backup_sha256="$(sha256_file "$backup")"
rollback_identity_args=()
identity_manifest="${backup}.identity"
: >"$identity_manifest"
chmod 0600 "$identity_manifest"
printf '%s  %s\n' "$backup_sha256" "$backup" >>"$identity_manifest"
while IFS= read -r dependency; do
  [ -n "$dependency" ] || continue
  dependency_sha="$(sha256_file "$dependency")"
  dependency_mode="$(stat -f '%Lp' "$dependency")"
  dependency_artifact="$bundle/${dependency_sha}-$(basename "$dependency")"
  copy_file_nofollow "$dependency" "$dependency_artifact" "$dependency_mode"
  rollback_identity_args+=(--rollback-artifact "$dependency" "$dependency_artifact" "$dependency_sha" "$dependency_mode")
  printf '%s  %s  %s\n' "$dependency_sha" "$dependency" "$dependency_artifact" >>"$identity_manifest"
done < <(plist_executable_dependencies "$backup")
[ "${#rollback_identity_args[@]}" -gt 0 ] \
  || die "could not discover rollback program identity"
service="$DOMAIN/$LABEL"
legacy_snapshot="$TRANSACTION_DIR/legacy.launchctl"
capture_launchctl_snapshot "$service" "$legacy_snapshot" || die "$service is not loaded"
captured_program="$(launchctl_snapshot_field "$legacy_snapshot" program)"
captured_pid="$(launchctl_snapshot_field "$legacy_snapshot" pid)"
[ "$captured_program" = "$old_program" ] || die "$service legacy program identity mismatch"
case "$captured_pid" in ''|*[!0-9]*) die "$service has no stable legacy pid" ;; esac
legacy_fingerprint="$(process_fingerprint "$captured_pid" "$old_program")" \
  || die "could not capture stable legacy process identity"
candidate_plist_sha="$(sha256_file "$prepared")"
candidate_supervisor_sha="$(sha256_file "$SUPERVISOR_BIN")"
candidate_worker_sha="$(sha256_file "$WORKER_BIN")"
printf '%s  %s\n%s  %s\n%s  %s\n' \
  "$candidate_plist_sha" "$prepared" \
  "$candidate_supervisor_sha" "$SUPERVISOR_BIN" \
  "$candidate_worker_sha" "$WORKER_BIN" >>"$identity_manifest"
printf 'legacy-process  %s\n' "$legacy_fingerprint" >>"$identity_manifest"

rollback() {
  local expected_running="${1:-$SUPERVISOR_BIN}"
  echo "rolling back through the standalone identity-checked command" >&2
  if SUBROUTER_LABEL="$LABEL" \
  SUBROUTER_PLIST="$PLIST" \
  SUBROUTER_LAUNCHD_DOMAIN="$DOMAIN" \
  SUBROUTER_CONTROL_SOCKET="$CONTROL_SOCKET" \
    "$SCRIPT_DIR/rollback-launchagent-supervisor.sh" \
      --backup "$backup" \
      --backup-sha256 "$backup_sha256" \
      "${rollback_identity_args[@]}" \
      --expected-program "$SUPERVISOR_BIN" \
      --expected-running-program "$expected_running"; then
    transaction_active=0
    trap - EXIT INT TERM
    rm -rf "$TRANSACTION_DIR"
    return 0
  fi
  return 1
}

write_recovery_script() {
  local destination="$1" expected_running="$2" expected_installed="${3:-$SUPERVISOR_BIN}"
  {
    printf '#!/usr/bin/env bash\nset -euo pipefail\n'
    printf 'export SUBROUTER_LABEL=%q SUBROUTER_PLIST=%q SUBROUTER_LAUNCHD_DOMAIN=%q SUBROUTER_CONTROL_SOCKET=%q\n' \
      "$LABEL" "$PLIST" "$DOMAIN" "$CONTROL_SOCKET"
    printf 'exec %q' "$SCRIPT_DIR/rollback-launchagent-supervisor.sh"
    printf ' %q' --backup "$backup" --backup-sha256 "$backup_sha256" \
      "${rollback_identity_args[@]}" --expected-program "$expected_installed" \
      --expected-running-program "$expected_running"
    printf '\n'
  } >"$destination"
  chmod 0700 "$destination"
}
write_recovery_script "$TRANSACTION_DIR/recover-legacy-running" "$old_program"
write_recovery_script "$TRANSACTION_DIR/recover-candidate-running" "$SUPERVISOR_BIN"
write_recovery_script "$TRANSACTION_DIR/recover-legacy-unchanged" "$old_program" "$old_program"
for recovery in "$TRANSACTION_DIR/recover-legacy-running" "$TRANSACTION_DIR/recover-candidate-running" "$TRANSACTION_DIR/recover-legacy-unchanged"; do
  sha256_file "$recovery" >"${recovery}.sha256"
  chmod 0600 "${recovery}.sha256"
done

recover_transaction_on_exit() {
  local status=$? phase recovered=0
  trap - EXIT INT TERM
  if [ "${transaction_active:-0}" -eq 1 ]; then
    phase="$(cat "$TRANSACTION_DIR/phase" 2>/dev/null || true)"
    case "$phase" in
      prelive)
        rm -rf "$TRANSACTION_DIR"
        recovered=1
        ;;
      candidate_plist_installing)
        if run_verified_recovery \
          "$TRANSACTION_DIR/recover-legacy-unchanged" \
          "$TRANSACTION_DIR/recover-legacy-running"; then
          recovered=1
        else
          echo "CRITICAL: automatic legacy recovery failed at phase $phase" >&2
        fi
        ;;
      candidate_bootstrap_requested)
        if run_verified_recovery \
          "$TRANSACTION_DIR/recover-legacy-running" \
          "$TRANSACTION_DIR/recover-candidate-running"; then
          recovered=1
        else
          echo "CRITICAL: automatic legacy recovery failed at phase $phase" >&2
        fi
        ;;
      candidate_plist_installed|legacy_bootout_requested|bootout_requested|legacy_absent)
        if run_verified_recovery "$TRANSACTION_DIR/recover-legacy-running"; then
          recovered=1
        else
          echo "CRITICAL: automatic legacy recovery failed at phase $phase" >&2
        fi
        ;;
      *)
        if run_verified_recovery "$TRANSACTION_DIR/recover-candidate-running"; then
          recovered=1
        else
          echo "CRITICAL: automatic legacy recovery failed at phase $phase" >&2
        fi
        ;;
    esac
    [ "$recovered" -eq 0 ] || rm -rf "$TRANSACTION_DIR"
  fi
  [ "$status" -ne 0 ] || status=1
  exit "$status"
}
trap recover_transaction_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

set_phase() {
  python3 - "$TRANSACTION_DIR" "$1" <<'PY'
import os
import sys

directory, phase = sys.argv[1:]
next_path = os.path.join(directory, "phase.next")
phase_path = os.path.join(directory, "phase")
fd = os.open(next_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
try:
    os.write(fd, (phase + "\n").encode())
    os.fsync(fd)
finally:
    os.close(fd)
os.replace(next_path, phase_path)
directory_fd = os.open(directory, os.O_RDONLY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY
  if [ "${SUBROUTER_FAULT_INJECT_PHASE:-}" = "$1" ]; then
    kill -TERM $$
  fi
  if [ "${SUBROUTER_FAULT_INJECT_HARD_PHASE:-}" = "$1" ]; then
    kill -KILL $$
  fi
}

inject_hard_fault_after_mutation() {
  if [ "${SUBROUTER_FAULT_INJECT_HARD_AFTER_MUTATION:-}" = "$1" ]; then
    kill -KILL $$
  fi
}

verify_file_sha256 "$prepared" "$candidate_plist_sha" || die "candidate plist changed before installation"
verify_file_sha256 "$SUPERVISOR_BIN" "$candidate_supervisor_sha" || die "candidate supervisor changed before installation"
verify_file_sha256 "$WORKER_BIN" "$candidate_worker_sha" || die "candidate worker changed before installation"
set_phase candidate_plist_installing
atomic_restore_nofollow "$prepared" "$PLIST" "$candidate_plist_sha" 0644 \
  || die "candidate plist installation failed"
inject_hard_fault_after_mutation candidate_plist_restore
set_phase candidate_plist_installed
set_phase legacy_bootout_requested
if ! launchctl bootout "$service"; then
  rollback "$old_program"
  die "could not request removal of $service; legacy LaunchAgent restored"
fi
set_phase bootout_requested
if ! wait_for_full_absence "$service" "$captured_pid" "$public_addr"; then
  rollback "$old_program"
  die "$service did not fully disappear; legacy LaunchAgent restored"
fi
set_phase legacy_absent
set_phase candidate_bootstrap_requested
if ! bootstrap_with_retry "$DOMAIN" "$PLIST" "$service" "$public_addr"; then
  rollback
  die "supervised agent failed to bootstrap"
fi
inject_hard_fault_after_mutation candidate_bootstrap
set_phase candidate_bootstrapped

ready_url="${health_url%/_subrouter/health}/_subrouter/ready"
candidate_snapshot="$TRANSACTION_DIR/candidate.launchctl"
candidate_pid=""
if capture_launchctl_snapshot "$service" "$candidate_snapshot"; then
  candidate_program="$(launchctl_snapshot_field "$candidate_snapshot" program)"
  candidate_pid="$(launchctl_snapshot_field "$candidate_snapshot" pid)"
  [ "$candidate_program" = "$SUPERVISOR_BIN" ] || candidate_pid=""
fi
candidate_fingerprint=""
[ -n "$candidate_pid" ] && candidate_fingerprint="$(process_fingerprint "$candidate_pid" "$SUPERVISOR_BIN" || true)"
printf 'candidate-process  %s\n' "$candidate_fingerprint" >>"$identity_manifest"
if [ -z "$candidate_fingerprint" ] \
  || ! verify_file_sha256 "$PLIST" "$candidate_plist_sha" \
  || ! verify_file_sha256 "$SUPERVISOR_BIN" "$candidate_supervisor_sha" \
  || ! verify_file_sha256 "$WORKER_BIN" "$candidate_worker_sha" \
  || ! require_process_fingerprint "$candidate_fingerprint" "$SUPERVISOR_BIN" \
  || ! require_sole_listener_owner "$public_addr" "$candidate_pid" \
  || ! require_control_socket_status "$CONTROL_SOCKET" "$(id -u)" \
  || ! wait_for_http_acceptance "$health_url" "$ready_url"; then
  rollback
  die "supervised agent failed structural acceptance"
fi
set_phase structural_accepted

echo "running bounded functional canary"
if ! run_bounded_argv "functional canary" "$CANARY_TIMEOUT" "$CANARY_CALLBACK"; then
  rollback
  die "functional canary failed; legacy LaunchAgent restored"
fi
set_phase canary_completed
if ! require_process_fingerprint "$candidate_fingerprint" "$SUPERVISOR_BIN" \
  || ! verify_file_sha256 "$PLIST" "$candidate_plist_sha" \
  || ! verify_file_sha256 "$SUPERVISOR_BIN" "$candidate_supervisor_sha" \
  || ! verify_file_sha256 "$WORKER_BIN" "$candidate_worker_sha" \
  || ! require_sole_listener_owner "$public_addr" "$candidate_pid" \
  || ! require_control_socket_status "$CONTROL_SOCKET" "$(id -u)" \
  || ! wait_for_http_acceptance "$health_url" "$ready_url"; then
  rollback
  die "candidate acceptance changed during canary; legacy LaunchAgent restored"
fi
set_phase accepted
transaction_active=0
trap - EXIT INT TERM
rm -rf "$TRANSACTION_DIR"

echo "supervised Subrouter passed health, readiness, and functional canary acceptance"
echo "control socket: $CONTROL_SOCKET"
echo "backup: $backup"
echo "rollback identity manifest: $identity_manifest"
echo "standalone rollback:"
printf '  %q' "$SCRIPT_DIR/rollback-launchagent-supervisor.sh" \
  --backup "$backup" --backup-sha256 "$backup_sha256" \
  "${rollback_identity_args[@]}" --expected-program "$SUPERVISOR_BIN"
printf '\n'
echo
echo "Upgrades are now non-disruptive:"
echo "  curl -fsS --unix-socket $CONTROL_SOCKET -X POST http://localhost/_subrouter/upgrade"
