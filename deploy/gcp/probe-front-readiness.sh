#!/usr/bin/env bash
# Emit GCP backend health only when the matching public canary also responds.
set -euo pipefail

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:?set SUBROUTER_GCP_FRONT_BACKEND_SERVICE}"
PUBLIC_BASE_URL="${SUBROUTER_CANARY_PUBLIC_BASE_URL:?set SUBROUTER_CANARY_PUBLIC_BASE_URL}"
CANARY_HOST="${SUBROUTER_CANARY_HOST:?set SUBROUTER_CANARY_HOST}"
CLOUD_CONFIG="${SUBROUTER_CLOUD_CONFIG:?set SUBROUTER_CLOUD_CONFIG}"
SESSION_ID="${SUBROUTER_CANARY_SESSION:?set SUBROUTER_CANARY_SESSION}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"

for command in "${GCLOUD_BINARY}" curl jq; do
  command -v "${command}" >/dev/null 2>&1 || { printf 'front-readiness-probe: command not found: %s\n' "${command}" >&2; exit 1; }
done
[[ "${PUBLIC_BASE_URL}" =~ ^https://[^/?#]+/?$ ]] || { printf 'front-readiness-probe: invalid public URL\n' >&2; exit 1; }
[[ "${CANARY_HOST}" =~ ^[A-Za-z0-9.-]+$ ]] || { printf 'front-readiness-probe: invalid canary host\n' >&2; exit 1; }
[[ "${SESSION_ID}" =~ ^[A-Za-z0-9._-]+$ ]] || { printf 'front-readiness-probe: invalid session ID\n' >&2; exit 1; }
[[ -f "${CLOUD_CONFIG}" && ! -L "${CLOUD_CONFIG}" ]] || { printf 'front-readiness-probe: unsafe cloud config\n' >&2; exit 1; }
tenant_key="$(jq -r '.tenantKey // empty' "${CLOUD_CONFIG}")"
[[ "${tenant_key}" =~ ^srt_[A-Za-z0-9_-]{16,}$ ]] || { printf 'front-readiness-probe: invalid tenant key\n' >&2; exit 1; }
PUBLIC_BASE_URL="${PUBLIC_BASE_URL%/}"

http_code="$(
  printf 'url = "%s/v1/responses"\nheader = "Authorization: Bearer %s"\nheader = "Host: %s"\nheader = "Content-Type: application/json"\nheader = "X-Subrouter-Agent: codex"\nheader = "X-Subrouter-Session: %s"\ndata = "{}"\n' \
    "${PUBLIC_BASE_URL}" "${tenant_key}" "${CANARY_HOST}" "${SESSION_ID}" | \
    curl --config - --http1.1 --silent --show-error --output /dev/null \
      --write-out '%{http_code}' --connect-timeout 5 --max-time 10
)" || http_code=""
[[ "${http_code}" == 400 ]] || { printf 'front-readiness-probe: public canary returned %s\n' "${http_code:-no status}" >&2; exit 1; }

exec "${GCLOUD_BINARY}" compute backend-services get-health "${FRONT_BACKEND_SERVICE}" \
  --project "${PROJECT_ID}" --global --format=json
