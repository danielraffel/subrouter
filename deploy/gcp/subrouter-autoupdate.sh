#!/usr/bin/env bash
# Pull-based auto-updater for the Subrouter VM.
#
# Polls the latest published GitHub release and, when it differs from the
# installed version, downloads the matching binary, verifies its checksum,
# swaps it in, and restarts the service. Restarts are zero-interruption: the
# listening socket is owned by systemd (subrouter.socket), the old process
# drains in-flight requests on SIGTERM (TimeoutStopSec=10min), and the new
# process starts accepting immediately (it no longer blocks startup on usage
# fetches). A release only exists after CI ran `go test`, so this never deploys
# untested code.
set -euo pipefail

REPO="${SUBROUTER_REPO:-manaflow-ai/subrouter}"
BIN="${SUBROUTER_BIN:-/usr/local/bin/subrouter}"
VERSION_FILE="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
SERVICE="${SUBROUTER_SERVICE:-subrouter.service}"

log() { echo "subrouter-autoupdate: $*"; }

case "$(uname -m)" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) log "unsupported arch $(uname -m)"; exit 1 ;;
esac

latest_tag="$(curl -fsSL -H 'Accept: application/vnd.github+json' \
  "https://api.github.com/repos/${REPO}/releases/latest" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])')"
if [ -z "${latest_tag}" ]; then
  log "could not resolve latest release tag"; exit 1
fi

installed=""
[ -f "${VERSION_FILE}" ] && installed="$(cat "${VERSION_FILE}" 2>/dev/null || true)"

if [ "${latest_tag}" = "${installed}" ]; then
  exit 0
fi

version="${latest_tag#v}"
asset="subrouter_${version}_linux_${arch}"
base="https://github.com/${REPO}/releases/download/${latest_tag}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

log "updating ${installed:-none} -> ${latest_tag} (${asset})"
curl -fsSL -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL -o "${tmp}/SHA256SUMS" "${base}/SHA256SUMS"
( cd "${tmp}" && grep " ${asset}\$" SHA256SUMS | sha256sum -c - )

cp -p "${BIN}" "${BIN}.bak-$(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
install -m 0755 -o root -g root "${tmp}/${asset}" "${BIN}"
systemctl restart "${SERVICE}"

i=0
until curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "${i}" -ge 30 ]; then
    log "health check failed after restart; leaving new binary in place"
    exit 1
  fi
  sleep 1
done

echo "${latest_tag}" > "${VERSION_FILE}"
log "updated to ${latest_tag} and healthy"
