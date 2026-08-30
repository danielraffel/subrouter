#!/usr/bin/env bash
# Restore an exact pre-supervisor LaunchAgent plist. The command refuses an
# unexpected loaded program or PID change and waits for label, PID, and listener
# absence before bootstrapping the preserved plist.
set -euo pipefail

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/launchagent-transition-lib.sh"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/mutation-lease-lib.sh"

export SUBROUTER_TRANSITION_NAME="rollback-launchagent-supervisor"
LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter}"
PLIST="${SUBROUTER_PLIST:-$HOME/Library/LaunchAgents/${LABEL}.plist}"
MUTATION_LOCK_FILE="${SUBROUTER_MUTATION_LOCK_FILE:-${PLIST}.supervisor-mutation.lock}"
DOMAIN="${SUBROUTER_LAUNCHD_DOMAIN:-gui/$(id -u)}"
BACKUP="${SUBROUTER_ROLLBACK_PLIST:-}"
EXPECTED_PROGRAM="${SUBROUTER_EXPECTED_PROGRAM:-}"
EXPECTED_RUNNING_PROGRAM="${SUBROUTER_EXPECTED_RUNNING_PROGRAM:-}"
BACKUP_SHA256="${SUBROUTER_ROLLBACK_PLIST_SHA256:-}"
EXPECTED_FILES=()
EXPECTED_FILE_SHAS=()
ROLLBACK_DESTINATIONS=()
ROLLBACK_ARTIFACTS=()
ROLLBACK_ARTIFACT_SHAS=()
ROLLBACK_ARTIFACT_MODES=()

usage() {
  echo "usage: $0 --backup PLIST --backup-sha256 SHA --rollback-artifact DEST ARTIFACT SHA MODE [--expected-program PATH] [--expected-running-program PATH]" >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --backup) [ "$#" -ge 2 ] || usage; BACKUP="$2"; shift 2 ;;
    --backup-sha256) [ "$#" -ge 2 ] || usage; BACKUP_SHA256="$2"; shift 2 ;;
    --expected-file-sha256)
      [ "$#" -ge 3 ] || usage
      EXPECTED_FILES+=("$2"); EXPECTED_FILE_SHAS+=("$3"); shift 3 ;;
    --rollback-artifact)
      [ "$#" -ge 5 ] || usage
      ROLLBACK_DESTINATIONS+=("$2"); ROLLBACK_ARTIFACTS+=("$3")
      ROLLBACK_ARTIFACT_SHAS+=("$4"); ROLLBACK_ARTIFACT_MODES+=("$5"); shift 5 ;;
    --expected-program) [ "$#" -ge 2 ] || usage; EXPECTED_PROGRAM="$2"; shift 2 ;;
    --expected-running-program)
      [ "$#" -ge 2 ] || usage; EXPECTED_RUNNING_PROGRAM="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done

[ -n "$BACKUP" ] || usage
[ -n "$BACKUP_SHA256" ] || usage
[ "${#ROLLBACK_ARTIFACTS[@]}" -gt 0 ] || usage

# Adopt the migration's kernel lease (or acquire it for a standalone rollback)
# before hashing or parsing any rollback input. If the migration is killed
# while this child validates a large bundle, the helper must already know this
# exact child generation so no updater can interpose before live restoration.
owns_mutation_lease=0
if ! subrouter_mutation_lease_is_held_by_parent "$MUTATION_LOCK_FILE"; then
  acquire_subrouter_mutation_lease "$MUTATION_LOCK_FILE" \
    || { status=$?; echo "rollback-launchagent-supervisor: another deployment or worker update holds $MUTATION_LOCK_FILE" >&2; exit "$status"; }
  owns_mutation_lease=1
fi
restore_next=""
cleanup() {
  [ -z "$restore_next" ] || rm -f "$restore_next"
  [ "$owns_mutation_lease" -eq 0 ] || release_subrouter_mutation_lease
}
trap cleanup EXIT

[ -f "$BACKUP" ] || launchagent_die "rollback plist $BACKUP not found"
[ ! -L "$BACKUP" ] || launchagent_die "rollback plist must not be a symlink"
verify_file_sha256 "$BACKUP" "$BACKUP_SHA256" \
  || launchagent_die "rollback plist content identity check failed"
[ "$(plist_value "$BACKUP" Label)" = "$LABEL" ] \
  || launchagent_die "rollback plist does not declare label $LABEL"
rollback_program="$(plist_program "$BACKUP")"
[ -x "$rollback_program" ] || launchagent_die "rollback program $rollback_program is not executable"
program_identity_present=0
for index in "${!ROLLBACK_ARTIFACTS[@]}"; do
  verify_file_sha256 "${ROLLBACK_ARTIFACTS[$index]}" "${ROLLBACK_ARTIFACT_SHAS[$index]}" \
    || launchagent_die "rollback bundle artifact identity check failed"
  [ "${ROLLBACK_DESTINATIONS[$index]}" = "$rollback_program" ] && program_identity_present=1
done
if [ "${#EXPECTED_FILES[@]}" -gt 0 ]; then
  for index in "${!EXPECTED_FILES[@]}"; do
    path="${EXPECTED_FILES[$index]}"
    expected_sha="${EXPECTED_FILE_SHAS[$index]}"
    verify_file_sha256 "$path" "$expected_sha" \
      || launchagent_die "rollback executable content identity check failed"
    [ "$path" = "$rollback_program" ] && program_identity_present=1
  done
fi
[ "$program_identity_present" -eq 1 ] \
  || launchagent_die "rollback program has no expected content identity"
while IFS= read -r required_path; do
  [ -n "$required_path" ] || continue
  required_identity_present=0
  if [ "${#EXPECTED_FILES[@]}" -gt 0 ]; then
    for path in "${EXPECTED_FILES[@]}"; do
      [ "$path" = "$required_path" ] && required_identity_present=1
    done
  fi
  for path in "${ROLLBACK_DESTINATIONS[@]}"; do
    [ "$path" = "$required_path" ] && required_identity_present=1
  done
  [ "$required_identity_present" -eq 1 ] \
    || launchagent_die "rollback dependency $required_path has no expected content identity"
done < <(plist_executable_dependencies "$BACKUP")

service="$DOMAIN/$LABEL"
captured_pid=""
if [ -e "$PLIST" ]; then
  [ ! -L "$PLIST" ] || launchagent_die "installed plist must not be a symlink"
  if [ -z "$EXPECTED_PROGRAM" ]; then
    EXPECTED_PROGRAM="$(plist_program "$PLIST")"
  fi
  require_plist_identity "$PLIST" "$LABEL" "$EXPECTED_PROGRAM"
  [ -n "$EXPECTED_RUNNING_PROGRAM" ] || EXPECTED_RUNNING_PROGRAM="$EXPECTED_PROGRAM"
  public_addr="$(plist_public_addr "$PLIST")"
  if launchagent_job_loaded "$service"; then
    captured_pid="$(capture_loaded_identity "$service" "$EXPECTED_RUNNING_PROGRAM")"
    launchctl bootout "$service"
  fi
else
  launchagent_job_loaded "$service" \
    && launchagent_die "installed plist is absent but $service is still loaded"
  public_addr="$(plist_public_addr "$BACKUP")"
fi
wait_for_full_absence "$service" "$captured_pid" "$public_addr"

restore_next="${PLIST}.rollback-next.$$"
trap 'exit 130' INT
trap 'exit 143' TERM
for index in "${!ROLLBACK_ARTIFACTS[@]}"; do
  atomic_restore_nofollow \
    "${ROLLBACK_ARTIFACTS[$index]}" "${ROLLBACK_DESTINATIONS[$index]}" \
    "${ROLLBACK_ARTIFACT_SHAS[$index]}" "${ROLLBACK_ARTIFACT_MODES[$index]}" \
    || launchagent_die "rollback executable restore failed"
done
verify_file_sha256 "$BACKUP" "$BACKUP_SHA256" \
  || launchagent_die "rollback plist changed immediately before restore"
atomic_restore_nofollow "$BACKUP" "$PLIST" "$BACKUP_SHA256" 0644 \
  || launchagent_die "rollback plist restore failed"
plutil -lint "$PLIST" >/dev/null

bootstrap_with_retry "$DOMAIN" "$PLIST" "$service" "$public_addr" \
  || launchagent_die "rollback LaunchAgent failed to bootstrap"
restored_pid="$(capture_loaded_identity "$service" "$rollback_program")" \
  || launchagent_die "rollback LaunchAgent is not loaded"

health_host="$public_addr"
case "$health_host" in
  0.0.0.0:*) health_host="127.0.0.1:${public_addr#0.0.0.0:}" ;;
  \[::\]:*) health_host="127.0.0.1:${public_addr#\[::\]:}" ;;
esac
health_url="${SUBROUTER_HEALTH_URL:-http://${health_host}/_subrouter/health}"
ready_url="${SUBROUTER_READY_URL:-http://${health_host}/_subrouter/ready}"
wait_for_http_acceptance "$health_url" "$ready_url" \
  || launchagent_die "rollback LaunchAgent failed health/readiness acceptance"

echo "restored $BACKUP as $PLIST"
echo "rollback LaunchAgent healthy and ready (pid $restored_pid)"
