#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

instance_name="${INSTANCE_NAME:-subrouter-team}"
zone="${ZONE:-us-central1-a}"
machine_type="${MACHINE_TYPE:-e2-micro}"
disk_size="${DISK_SIZE:-10GB}"
disk_type="${DISK_TYPE:-pd-standard}"
image_family="${IMAGE_FAMILY:-debian-12}"
image_project="${IMAGE_PROJECT:-debian-cloud}"
tags="${TAGS:-subrouter}"
network="${NETWORK:-default}"
subnet="${SUBNET:-}"
allow_ssh="${ALLOW_SSH:-1}"

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

gcloud services enable compute.googleapis.com --project "${project_id}" >/dev/null

if gcloud compute instances describe "${instance_name}" \
  --project "${project_id}" \
  --zone "${zone}" >/dev/null 2>&1; then
  echo "Instance already exists: ${instance_name} (${zone})"
else
  args=(
    compute instances create "${instance_name}"
    --project "${project_id}"
    --zone "${zone}"
    --machine-type "${machine_type}"
    --image-family "${image_family}"
    --image-project "${image_project}"
    --boot-disk-size "${disk_size}"
    --boot-disk-type "${disk_type}"
    --boot-disk-auto-delete
    --tags "${tags}"
    --labels "app=subrouter,managed-by=subrouter"
    --metadata-from-file "startup-script=${script_dir}/startup.sh"
    --network "${network}"
  )
  if [[ -n "${subnet}" ]]; then
    args+=(--subnet "${subnet}")
  fi
  gcloud "${args[@]}"
fi

if [[ "${allow_ssh}" == "1" ]]; then
  source_range="${SSH_SOURCE_RANGE:-}"
  if [[ -z "${source_range}" ]]; then
    public_ip="$(curl -fsS https://api.ipify.org 2>/dev/null || true)"
    if [[ -n "${public_ip}" ]]; then
      source_range="${public_ip}/32"
    fi
  fi

  if [[ -n "${source_range}" ]]; then
    if ! gcloud compute firewall-rules describe subrouter-allow-ssh \
      --project "${project_id}" >/dev/null 2>&1; then
      gcloud compute firewall-rules create subrouter-allow-ssh \
        --project "${project_id}" \
        --network "${network}" \
        --priority 800 \
        --allow tcp:22 \
        --source-ranges "${source_range}" \
        --target-tags "${tags}" \
        --description "SSH to Subrouter bootstrap hosts from the configured source range"
    else
      gcloud compute firewall-rules update subrouter-allow-ssh \
        --project "${project_id}" \
        --priority 800 \
        --source-ranges "${source_range}" >/dev/null
    fi
  else
    echo "Could not infer SSH source IP. Set SSH_SOURCE_RANGE=x.x.x.x/32 if SSH is needed." >&2
  fi
fi

if ! gcloud compute firewall-rules describe subrouter-deny-public-ingress \
  --project "${project_id}" >/dev/null 2>&1; then
  gcloud compute firewall-rules create subrouter-deny-public-ingress \
    --project "${project_id}" \
    --network "${network}" \
    --priority 900 \
    --action DENY \
    --rules tcp,udp,icmp \
    --source-ranges 0.0.0.0/0 \
    --target-tags "${tags}" \
    --description "Deny public ingress to Subrouter hosts except higher-priority explicit allows"
fi

echo "Instance:"
gcloud compute instances describe "${instance_name}" \
  --project "${project_id}" \
  --zone "${zone}" \
  --format='table(name,zone.basename(),machineType.basename(),networkInterfaces[0].accessConfigs[0].natIP,status)'

echo
echo "Next:"
echo "  curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh"
echo "  deploy/gcp/publish-subrouter.sh"
echo
echo "Subrouter listens on port 31415. This script does not open that port publicly."
