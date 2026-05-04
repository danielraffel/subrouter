#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  deploy/gcp/upload-codex-accounts.sh --move --account <email-or-id> [--account <email-or-id> ...] [--replace-remote]
  deploy/gcp/upload-codex-accounts.sh --move --all [--replace-remote]
  deploy/gcp/upload-codex-accounts.sh --copy-unsafe --account <email-or-id> [--account <email-or-id> ...]

Uploads Codex account auth files to the Subrouter VM.

Use --move for OAuth accounts. It uploads selected local account files, restarts
Subrouter, then moves those local files into a timestamped local backup so the
same rotating refresh tokens are not active on both machines.

Use --copy-unsafe only for non-rotating API-key accounts or an intentional
break-glass copy.

Environment:
  PROJECT_ID     GCP project, defaults to current gcloud project
  INSTANCE_NAME  VM name, defaults to subrouter-community
  ZONE           VM zone, defaults to us-central1-a
USAGE
}

instance_name="${INSTANCE_NAME:-subrouter-community}"
zone="${ZONE:-us-central1-a}"
mode=""
replace_remote=0
all_accounts=0
accounts=()

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --move)
      mode="move"
      shift
      ;;
    --copy-unsafe)
      mode="copy"
      shift
      ;;
    --replace-remote)
      replace_remote=1
      shift
      ;;
    --all)
      all_accounts=1
      shift
      ;;
    --account)
      if [[ "$#" -lt 2 || -z "$2" ]]; then
        echo "--account requires a value" >&2
        exit 1
      fi
      accounts+=("$2")
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "${mode}" ]]; then
  echo "Choose --move or --copy-unsafe." >&2
  usage >&2
  exit 1
fi

if [[ "${all_accounts}" == "1" && "${#accounts[@]}" -gt 0 ]]; then
  echo "Use either --all or --account, not both." >&2
  exit 1
fi

if [[ "${all_accounts}" != "1" && "${#accounts[@]}" -eq 0 ]]; then
  echo "Select accounts with --account, or pass --all explicitly." >&2
  usage >&2
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

account_dir="${HOME}/.codex-accounts/accounts"
if [[ ! -d "${account_dir}" ]]; then
  echo "No local account store found at ${account_dir}" >&2
  exit 1
fi

filename_for_account() {
  local value="$1"
  local out=""
  local i ch
  for ((i = 0; i < ${#value}; i++)); do
    ch="${value:i:1}"
    case "${ch}" in
      [a-zA-Z0-9._@-]) out+="${ch}" ;;
      *) out+="_" ;;
    esac
  done
  printf '%s.json' "${out}"
}

selected_paths=()
if [[ "${all_accounts}" == "1" ]]; then
  while IFS= read -r -d '' path; do
    selected_paths+=("${path}")
  done < <(find "${account_dir}" -maxdepth 1 -type f ! -name '.*' -name '*.json' -print0 | sort -z)
else
  for account in "${accounts[@]}"; do
    path="${account_dir}/$(filename_for_account "${account}")"
    if [[ ! -f "${path}" ]]; then
      echo "No local account file for ${account}: ${path}" >&2
      exit 1
    fi
    selected_paths+=("${path}")
  done
fi

if [[ "${#selected_paths[@]}" -eq 0 ]]; then
  echo "No account files selected." >&2
  exit 1
fi

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

mkdir -p "${tmp_dir}/.codex-accounts/accounts"
for path in "${selected_paths[@]}"; do
  cp "${path}" "${tmp_dir}/.codex-accounts/accounts/"
done

COPYFILE_DISABLE=1 tar -C "${tmp_dir}" -czf "${tmp_dir}/codex-accounts.tgz" .codex-accounts/accounts

echo "Uploading ${#selected_paths[@]} Codex account file(s) to ${instance_name} (${zone})."
if [[ "${mode}" == "move" ]]; then
  echo "Mode: move. Local selected files will be moved to a backup after the server accepts them."
else
  echo "Mode: copy-unsafe. Rotating OAuth refresh tokens may break if used on both machines."
fi

gcloud compute scp "${tmp_dir}/codex-accounts.tgz" "${instance_name}:/tmp/codex-accounts-${timestamp}.tgz" \
  --project "${project_id}" \
  --zone "${zone}"

remote_script="$(cat <<REMOTE
set -euo pipefail
sudo install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/.codex-accounts/accounts
sudo install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/codex-store-backups
if sudo test -d /var/lib/subrouter/.codex-accounts/accounts; then
  sudo tar -C /var/lib/subrouter -czf /var/lib/subrouter/codex-store-backups/${timestamp}.tgz .codex-accounts/accounts 2>/dev/null || true
fi
if [[ "${replace_remote}" == "1" ]]; then
  sudo find /var/lib/subrouter/.codex-accounts/accounts -maxdepth 1 -type f ! -name '.*' -name '*.json' -delete
fi
sudo tar -C /var/lib/subrouter -xzf /tmp/codex-accounts-${timestamp}.tgz
sudo find /var/lib/subrouter/.codex-accounts -name '._*' -delete
sudo chown -R subrouter:subrouter /var/lib/subrouter/.codex-accounts /var/lib/subrouter/codex-store-backups
sudo rm -f /tmp/codex-accounts-${timestamp}.tgz
sudo systemctl restart subrouter
curl -fsS http://127.0.0.1:31415/_subrouter/health >/dev/null
sudo -u subrouter HOME=/var/lib/subrouter /usr/local/bin/cx list
REMOTE
)"

gcloud compute ssh "${instance_name}" \
  --project "${project_id}" \
  --zone "${zone}" \
  --command "${remote_script}"

if [[ "${mode}" == "move" ]]; then
  backup_dir="${HOME}/.codex-accounts/uploaded-to-subrouter/${timestamp}/accounts"
  mkdir -p "${backup_dir}"
  for path in "${selected_paths[@]}"; do
    mv "${path}" "${backup_dir}/"
  done
  echo
  echo "Moved local selected account files to:"
  echo "  ${backup_dir}"
fi

echo
echo "Subrouter account upload complete."
