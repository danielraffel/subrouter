#!/usr/bin/env bash
# Shared fail-closed launchd transition helpers. This file is sourced by the
# LaunchAgent migration and rollback commands; it is not an entry point.

launchagent_die() {
  echo "${SUBROUTER_TRANSITION_NAME:-subrouter-launchagent-transition}: $*" >&2
  exit 1
}

positive_integer() {
  case "${1:-}" in
    ''|*[!0-9]*|0) return 1 ;;
    *) return 0 ;;
  esac
}

plist_value() {
  local plist="$1" key="$2"
  /usr/libexec/PlistBuddy -c "Print :${key}" "$plist" 2>/dev/null
}

plist_program() {
  local plist="$1" program
  program="$(plist_value "$plist" Program || true)"
  if [ -z "$program" ]; then
    program="$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$plist" 2>/dev/null)"
  fi
  printf '%s\n' "$program"
}

sha256_file() {
  shasum -a 256 "$1" | awk '{print $1}'
}

verify_file_sha256() {
  local path="$1" expected="$2" actual
  [ -f "$path" ] && [ ! -L "$path" ] \
    || { echo "identity file $path is missing, non-regular, or a symlink" >&2; return 1; }
  actual="$(sha256_file "$path")"
  [ "$actual" = "$expected" ] || {
    echo "identity mismatch for $path: expected $expected, got $actual" >&2
    return 1
  }
}

copy_file_nofollow() {
  python3 - "$1" "$2" "$3" <<'PY'
import os, shutil, stat, sys
source, destination, mode = sys.argv[1:]
flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
source_fd = os.open(source, flags)
try:
    info = os.fstat(source_fd)
    if not stat.S_ISREG(info.st_mode):
        raise SystemExit("source is not a regular file")
    destination_fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, int(mode, 8))
    try:
        with os.fdopen(source_fd, "rb", closefd=False) as src, os.fdopen(destination_fd, "wb", closefd=False) as dst:
            shutil.copyfileobj(src, dst)
            dst.flush(); os.fsync(dst.fileno())
    finally:
        os.close(destination_fd)
finally:
    os.close(source_fd)
PY
}

atomic_restore_nofollow() {
  local artifact="$1" destination="$2" expected="$3" mode="$4" next
  verify_file_sha256 "$artifact" "$expected" || return 1
  next="${destination}.rollback-next.$$"
  rm -f "$next"
  copy_file_nofollow "$artifact" "$next" "$mode" || return 1
  verify_file_sha256 "$artifact" "$expected" || { rm -f "$next"; return 1; }
  verify_file_sha256 "$next" "$expected" || { rm -f "$next"; return 1; }
  mv -f "$next" "$destination"
}

plist_executable_dependencies() {
  python3 - "$1" <<'PY'
import os
import plistlib
import shlex
import sys

with open(sys.argv[1], "rb") as stream:
    plist = plistlib.load(stream)

paths = []
program = plist.get("Program")
arguments = list(plist.get("ProgramArguments") or [])
for candidate in [program, *arguments]:
    if isinstance(candidate, str) and os.path.isabs(candidate):
        paths.append(candidate)

# A legacy LaunchAgent commonly starts a small shell wrapper. Discover literal
# absolute executable paths in that wrapper (worker and helper binaries) while
# ignoring comments, shebangs, variables, and non-executable secret files.
wrapper = program or (arguments[0] if arguments else None)
if isinstance(wrapper, str) and os.path.isfile(wrapper):
    try:
        with open(wrapper, "r", encoding="utf-8") as stream:
            content = stream.read().replace("\\\n", " ")
            for line in content.splitlines():
                if line.startswith("#!") or line.lstrip().startswith("#"):
                    continue
                try:
                    words = shlex.split(line, comments=True, posix=True)
                except ValueError:
                    continue
                paths.extend(word for word in words if os.path.isabs(word))
    except (OSError, UnicodeError):
        pass

for path in dict.fromkeys(paths):
    if os.path.isfile(path) and os.access(path, os.X_OK):
        print(path)
PY
}

launchagent_job_field() {
  local service="$1" field="$2"
  launchctl print "$service" 2>/dev/null \
    | awk -v field="$field" '$1 == field && $2 == "=" {
        sub(/^[^=]*=[[:space:]]*/, "", $0); print; exit
      }'
}

launchagent_job_loaded() {
  launchctl print "$1" >/dev/null 2>&1
}

capture_launchctl_snapshot() {
  local service="$1" destination="$2"
  launchctl print "$service" >"$destination" 2>/dev/null
}

launchctl_snapshot_field() {
  local snapshot="$1" field="$2"
  awk -v field="$field" '$1 == field && $2 == "=" {
    sub(/^[^=]*=[[:space:]]*/, "", $0); print; exit
  }' "$snapshot"
}

require_plist_identity() {
  local plist="$1" expected_label="$2" expected_program="$3"
  [ -f "$plist" ] || launchagent_die "$plist not found"
  [ "$(plist_value "$plist" Label)" = "$expected_label" ] \
    || launchagent_die "$plist does not declare label $expected_label"
  [ "$(plist_program "$plist")" = "$expected_program" ] \
    || launchagent_die "$plist does not declare expected program $expected_program"
}

capture_loaded_identity() {
  local service="$1" expected_program="$2"
  local program pid
  launchagent_job_loaded "$service" || return 1
  program="$(launchagent_job_field "$service" program)"
  pid="$(launchagent_job_field "$service" pid)"
  [ "$program" = "$expected_program" ] \
    || launchagent_die "$service runs unexpected program ${program:-unknown}"
  case "$pid" in
    ''|*[!0-9]*) launchagent_die "$service has no stable numeric pid" ;;
  esac
  printf '%s\n' "$pid"
}

process_fingerprint() {
  local pid="$1" program="$2" program_sha command start
  program_sha="$(sha256_file "$program")"
  command="$(ps -p "$pid" -o command= | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]][[:space:]]*/ /g')"
  start="$(ps -p "$pid" -o lstart= | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]][[:space:]]*/ /g')"
  [ -n "$command" ] && [ -n "$start" ] || return 1
  printf '%s|%s|%s|%s\n' "$pid" "$program_sha" "$start" "$command"
}

require_process_fingerprint() {
  local expected="$1" program="$2" pid
  pid="${expected%%|*}"
  [ "$(process_fingerprint "$pid" "$program")" = "$expected" ]
}

plist_public_addr() {
  python3 - "$1" <<'PY'
import plistlib
import sys

with open(sys.argv[1], "rb") as stream:
    arguments = list(plistlib.load(stream).get("ProgramArguments") or [])

for index, argument in enumerate(arguments):
    if argument == "--addr" and index + 1 < len(arguments):
        print(arguments[index + 1])
        break
    if argument.startswith("--addr="):
        print(argument.split("=", 1)[1])
        break
else:
    raise SystemExit("plist has no --addr")
PY
}

listener_present() {
  local public_addr="$1" port
  port="${public_addr##*:}"
  case "$port" in
    ''|*[!0-9]*) launchagent_die "cannot determine listener port from $public_addr" ;;
  esac
  lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1
}

wait_for_full_absence() {
  local service="$1" captured_pid="$2" public_addr="$3"
  local attempts="${SUBROUTER_ABSENCE_ATTEMPTS:-120}"
  local interval="${SUBROUTER_ABSENCE_INTERVAL:-1}"
  local stable=0 current_pid i

  i=0
  while [ "$i" -lt "$attempts" ]; do
    i=$((i + 1))
    if launchagent_job_loaded "$service"; then
      current_pid="$(launchagent_job_field "$service" pid)"
      if [ -n "$captured_pid" ] && [ "$current_pid" != "$captured_pid" ]; then
        echo "$service changed pid during removal ($captured_pid -> ${current_pid:-unknown})" >&2
        return 1
      fi
      stable=0
    elif { [ -z "$captured_pid" ] || ! kill -0 "$captured_pid" 2>/dev/null; } \
      && ! listener_present "$public_addr"; then
      stable=$((stable + 1))
      [ "$stable" -ge 2 ] && return 0
    else
      stable=0
    fi
    sleep "$interval"
  done
  echo "$service did not reach full label, pid, and listener absence" >&2
  return 1
}

bootstrap_with_retry() {
  local domain="$1" plist="$2" service="$3" public_addr="$4"
  local attempts="${SUBROUTER_BOOTSTRAP_ATTEMPTS:-10}"
  local interval="${SUBROUTER_BOOTSTRAP_INTERVAL:-2}"
  local i=0
  while [ "$i" -lt "$attempts" ]; do
    if launchctl bootstrap "$domain" "$plist"; then
      return 0
    fi
    i=$((i + 1))
    wait_for_full_absence "$service" "" "$public_addr" || return 1
    sleep "$interval"
  done
  return 1
}

pid_is_candidate_descendant() {
  local pid="$1" candidate="$2" parent steps=0
  while [ "$pid" -gt 1 ] 2>/dev/null && [ "$steps" -lt 64 ]; do
    [ "$pid" = "$candidate" ] && return 0
    parent="$(ps -p "$pid" -o ppid= | tr -d '[:space:]')"
    case "$parent" in ''|*[!0-9]*) return 1 ;; esac
    pid="$parent"; steps=$((steps + 1))
  done
  return 1
}

require_sole_listener_owner() {
  local public_addr="$1" candidate_pid="$2" port pid found=0
  port="${public_addr##*:}"
  while IFS= read -r pid; do
    case "$pid" in ''|*[!0-9]*) continue ;; esac
    found=1
    pid_is_candidate_descendant "$pid" "$candidate_pid" || {
      echo "listener on port $port is owned by unexpected pid $pid" >&2
      return 1
    }
  done < <(lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | sort -u)
  [ "$found" -eq 1 ] || { echo "no listener owner found on port $port" >&2; return 1; }
}

require_control_socket_status() {
  local socket="$1" expected_uid="$2" status
  [ "$(stat -f '%HT' "$socket" 2>/dev/null)" = "Socket" ] || return 1
  [ "$(stat -f '%Lp' "$socket" 2>/dev/null)" = "600" ] || return 1
  [ "$(stat -f '%u' "$socket" 2>/dev/null)" = "$expected_uid" ] || return 1
  status="$(curl -fsS --unix-socket "$socket" http://localhost/_subrouter/supervisor-status)" || return 1
  printf '%s' "$status" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("accepting") is True and d.get("retiring") is False and d.get("active",{}).get("id") and len(d.get("backends",[])) == 1'
}

wait_for_http_acceptance() {
  local health_url="$1" ready_url="$2"
  local attempts="${SUBROUTER_HEALTH_ATTEMPTS:-60}"
  local interval="${SUBROUTER_HEALTH_INTERVAL:-1}"
  local i=0
  while [ "$i" -lt "$attempts" ]; do
    if curl -fsS --max-time 2 "$health_url" >/dev/null 2>&1 \
      && curl -fsS --max-time 2 "$ready_url" >/dev/null 2>&1; then
      return 0
    fi
    i=$((i + 1))
    sleep "$interval"
  done
  return 1
}

run_bounded_argv() {
  local name="$1" timeout="$2"
  shift 2
  positive_integer "$timeout" \
    || launchagent_die "$name timeout must be a positive integer"
  [ "$#" -gt 0 ] || launchagent_die "$name command is required"

  python3 - "$name" "$timeout" "$@" <<'PY'
import os
import signal
import subprocess
import sys

name, timeout, *command = sys.argv[1:]
process = subprocess.Popen(command, start_new_session=True)
try:
    raise SystemExit(process.wait(timeout=int(timeout)))
except subprocess.TimeoutExpired:
    print(f"{name} timed out after {timeout}s", file=sys.stderr)
    os.killpg(process.pid, signal.SIGTERM)
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait()
    raise SystemExit(124)
PY
}
