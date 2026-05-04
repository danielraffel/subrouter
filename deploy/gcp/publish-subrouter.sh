#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

instance_name="${INSTANCE_NAME:-subrouter-community}"
zone="${ZONE:-us-central1-a}"
tailscale_hostname="${TAILSCALE_HOSTNAME:-subrouter-community}"

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

cd "${repo_root}"
make build-linux

gcloud compute scp bin/subrouter-linux-amd64 "${instance_name}:/tmp/subrouter" \
  --project "${project_id}" \
  --zone "${zone}"

gcloud compute ssh "${instance_name}" \
  --project "${project_id}" \
  --zone "${zone}" \
  --command "sudo install -o root -g root -m 0755 /tmp/subrouter /usr/local/bin/subrouter && sudo ln -sf /usr/local/bin/subrouter /usr/local/bin/cx && sudo systemctl daemon-reload && sudo systemctl enable --now subrouter && sudo systemctl restart subrouter && sudo systemctl --no-pager --full status subrouter"

if [[ -n "${TAILSCALE_AUTH_KEY:-}" ]]; then
  printf '%s\n' "${TAILSCALE_AUTH_KEY}" | gcloud compute ssh "${instance_name}" \
    --project "${project_id}" \
    --zone "${zone}" \
    --command "read -r tailscale_auth_key && sudo tailscale up --auth-key \"\${tailscale_auth_key}\" --hostname \"${tailscale_hostname}\" --ssh --accept-routes=false --accept-dns=false && tailscale ip -4"
else
  echo "TAILSCALE_AUTH_KEY is not set. To join the VM to Tailscale:"
  echo "  export TAILSCALE_AUTH_KEY=tskey-auth-..."
  echo "  deploy/gcp/publish-subrouter.sh"
fi

echo "Health check from the tailnet:"
echo "  curl http://<tailscale-ip>:31415/_subrouter/health"
