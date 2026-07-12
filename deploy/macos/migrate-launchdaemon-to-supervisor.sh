#!/usr/bin/env bash
# Convert an existing macOS Subrouter LaunchDaemon to the stable supervisor
# layout. Preparation is non-disruptive. --activate performs the one-time
# service transition; all later worker updates are zero-disruption.
set -euo pipefail

LABEL="${SUBROUTER_LABEL:-ai.manaflow.subrouter-team}"
PLIST="${SUBROUTER_PLIST:-/Library/LaunchDaemons/${LABEL}.plist}"
WORKER_BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
SUPERVISOR_BIN="${SUBROUTER_SUPERVISOR_BIN:-/usr/local/libexec/subrouter-supervisor}"
CONTROL_ADDR="${SUBROUTER_CONTROL_ADDR:-127.0.0.1:31414}"
ACTIVATE=0
[ "${1:-}" = "--activate" ] && ACTIVATE=1

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
[ -f "$PLIST" ] || { echo "missing $PLIST" >&2; exit 1; }
[ -x "$WORKER_BIN" ] || { echo "missing $WORKER_BIN" >&2; exit 1; }

mkdir -p "$(dirname "$SUPERVISOR_BIN")"
if [ ! -x "$SUPERVISOR_BIN" ]; then
  install -m 0755 "$WORKER_BIN" "$SUPERVISOR_BIN"
fi
"$SUPERVISOR_BIN" help 2>/dev/null | grep -q ' supervise ' || {
  echo "$SUPERVISOR_BIN does not support supervise" >&2
  exit 1
}

prepared="${PLIST}.supervised"
PLIST="$PLIST" PREPARED="$prepared" WORKER_BIN="$WORKER_BIN" \
SUPERVISOR_BIN="$SUPERVISOR_BIN" CONTROL_ADDR="$CONTROL_ADDR" python3 <<'PY'
import os
import plistlib

source = os.environ["PLIST"]
destination = os.environ["PREPARED"]
worker_bin = os.environ["WORKER_BIN"]
supervisor_bin = os.environ["SUPERVISOR_BIN"]
control_addr = os.environ["CONTROL_ADDR"]

with open(source, "rb") as stream:
    plist = plistlib.load(stream)

arguments = list(plist.get("ProgramArguments") or [])
if len(arguments) < 2 or arguments[1] != "serve":
    raise SystemExit("existing ProgramArguments must start with '<binary> serve'")

worker_args = arguments[2:]
public_addr = "127.0.0.1:31415"
filtered = []
i = 0
while i < len(worker_args):
    argument = worker_args[i]
    if argument == "--addr":
        if i + 1 >= len(worker_args):
            raise SystemExit("existing --addr has no value")
        public_addr = worker_args[i + 1]
        i += 2
        continue
    if argument.startswith("--addr="):
        public_addr = argument.split("=", 1)[1]
        i += 1
        continue
    filtered.append(argument)
    i += 1

plist["Program"] = supervisor_bin
plist["ProgramArguments"] = [
    supervisor_bin,
    "supervise",
    "--addr", public_addr,
    "--control-addr", control_addr,
    "--worker-bin", worker_bin,
    "--",
    *filtered,
]
plist["ProcessType"] = "Interactive"
plist["ThrottleInterval"] = 10
plist["ExitTimeOut"] = 600

temporary = destination + ".new"
with open(temporary, "wb") as stream:
    plistlib.dump(plist, stream, fmt=plistlib.FMT_XML, sort_keys=False)
os.chmod(temporary, 0o644)
os.replace(temporary, destination)
PY

plutil -lint "$prepared"
echo "Prepared $prepared"
if [ "$ACTIVATE" -ne 1 ]; then
  echo "Not activated. Re-run with --activate for the one-time listener transition."
  exit 0
fi

backup="${PLIST}.backup-$(date +%Y%m%d-%H%M%S)"
cp -p "$PLIST" "$backup"
mv -f "$prepared" "$PLIST"
launchctl bootout "system/${LABEL}" 2>/dev/null || true
launchctl bootstrap system "$PLIST"

i=0
until curl -fsS "http://${CONTROL_ADDR}/_subrouter/supervisor-status" >/dev/null 2>&1 \
  && curl -fsS "http://127.0.0.1:31415/_subrouter/health" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "supervised service failed health checks; restore $backup" >&2
    exit 1
  fi
  sleep 1
done
echo "Activated supervised Subrouter. Backup: $backup"
