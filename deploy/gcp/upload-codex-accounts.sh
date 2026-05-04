#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  deploy/gcp/upload-codex-accounts.sh [server-name] [--device-auth]

This compatibility wrapper delegates to:

  sr server login <server-name> [--device-auth]

Subrouter no longer copies existing OAuth refresh-token files to the server.
Rotating OAuth refresh tokens must be created for the server with a fresh login
so only the server owns that refresh-token chain.
USAGE
}

sr_bin="${SR_BIN:-sr}"
default_server_name="${SERVER_NAME:-team}"
server_name="${default_server_name}"
args=()

while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --move|--copy-unsafe|--replace-remote|--all|--account)
      echo "The old account-file upload path has been removed." >&2
      echo "Use: sr server login ${server_name} --device-auth" >&2
      exit 1
      ;;
    --device-auth)
      args+=("$1")
      shift
      ;;
    *)
      if [[ "${server_name}" == "${default_server_name}" ]]; then
        server_name="$1"
        shift
      else
        echo "Unknown argument: $1" >&2
        usage >&2
        exit 1
      fi
      ;;
  esac
done

if ! command -v "${sr_bin}" >/dev/null 2>&1; then
  echo "sr is required. Install it with:" >&2
  echo "  curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh" >&2
  exit 1
fi

exec "${sr_bin}" server login "${server_name}" "${args[@]}"
