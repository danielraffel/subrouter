#!/usr/bin/env bash
set -euo pipefail

instance_name="${INSTANCE_NAME:-subrouter-community}"
server_name="${SERVER_NAME:-community}"
zone="${ZONE:-us-central1-a}"
tailscale_hostname="${TAILSCALE_HOSTNAME:-${instance_name}}"
server_url="${SERVER_URL:-http://${tailscale_hostname}:31415}"
subrouter_version="${SUBROUTER_VERSION:-latest}"
sr_bin="${SR_BIN:-sr}"

if ! command -v "${sr_bin}" >/dev/null 2>&1; then
  echo "sr is required. Install it with:" >&2
  echo "  curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh" >&2
  exit 1
fi

if ! command -v gcloud >/dev/null 2>&1; then
  echo "gcloud is required. Install Google Cloud CLI first." >&2
  exit 1
fi

active_account="$(gcloud config get-value account 2>/dev/null || true)"
if [[ -z "${active_account}" || "${active_account}" == "(unset)" ]]; then
  echo "No active gcloud account. Run: gcloud auth login" >&2
  exit 1
fi

project_id="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
if [[ -z "${project_id}" || "${project_id}" == "(unset)" ]]; then
  echo "No GCP project configured. Run: gcloud config set project <project-id>" >&2
  exit 1
fi

"${sr_bin}" server add "${server_name}" \
  --url "${server_url}" \
  --gcp-instance "${instance_name}" \
  --gcp-zone "${zone}" \
  --gcp-project "${project_id}"

"${sr_bin}" server install "${server_name}" \
  --version "${subrouter_version}" \
  --tailscale-hostname "${tailscale_hostname}"

if [[ -z "${TAILSCALE_AUTH_KEY:-}" ]]; then
  echo "TAILSCALE_AUTH_KEY is not set. To join or rejoin the VM to Tailscale:"
  echo "  export TAILSCALE_AUTH_KEY=<tailscale-auth-key>"
  echo "  deploy/gcp/publish-subrouter.sh"
fi

echo "Health check from the tailnet:"
echo "  curl ${server_url}/_subrouter/health"
