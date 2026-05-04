# GCP Subrouter Deployment

This deploys Subrouter as `subrouter` on a small Debian VM and exposes the service over Tailscale.

Defaults:

- Instance: `subrouter-community`
- Zone: `us-central1-a`
- Machine: `e2-micro`
- Disk: 10 GB standard persistent disk
- Service port: `31415`

The scripts do not open port `31415` publicly. Use Tailscale for teammate access.
They also add a target-tagged deny rule for public ingress, with only a source-limited bootstrap SSH allow rule above it.

## Local setup

Install and authenticate the Google Cloud CLI:

```bash
gcloud auth login
gcloud config set project <project-id>
```

Create the VM:

```bash
deploy/gcp/create-subrouter-vm.sh
```

Build and publish the binary:

```bash
deploy/gcp/publish-subrouter.sh
```

Join the host to Tailscale with an auth key:

```bash
export TAILSCALE_AUTH_KEY=tskey-auth-...
deploy/gcp/publish-subrouter.sh
```

The publish script joins with `--accept-routes=false --accept-dns=false` so the VM does not use tailnet routes or tailnet DNS for its own outbound traffic.
The VM also installs a host firewall rule that rejects new outbound connections from `tailscale0` to tailnet IP ranges while still allowing replies to inbound requests.

Move Codex account auth to the VM when it should route real Codex traffic:

```bash
deploy/gcp/upload-codex-accounts.sh --move --account bob@example.com
```

OAuth refresh tokens rotate on use, so do not keep the same OAuth account active on both a laptop and the server. The upload script writes selected account files into `/var/lib/subrouter/.codex-accounts/accounts`, restarts Subrouter, then moves the selected local files into `~/.codex-accounts/uploaded-to-subrouter/<timestamp>/accounts`.

To replace the server pool with the selected local accounts:

```bash
deploy/gcp/upload-codex-accounts.sh --move --replace-remote --account bob@example.com --account lc@example.com
```

Use `--all` only when intentionally transferring every local account file to the VM.

`--copy-unsafe` exists only for API-key accounts or break-glass cases where duplicate rotating OAuth refresh tokens are intentional.

## Client usage

Use the Tailscale IP or MagicDNS name:

```bash
export SUBROUTER_CODEX_BASE_URL=http://<tailscale-ip>:31415/v1
export SUBROUTER_CODEX_USER_EMAIL=alice@example.com
subrouter codex
```

Traffic attribution is self-reported with `X-Subrouter-User-Email`, or through `SUBROUTER_CODEX_USER_EMAIL` when using `subrouter codex`.

Health check:

```bash
curl http://<tailscale-ip>:31415/_subrouter/health
```

Sessions:

```bash
curl http://<tailscale-ip>:31415/_subrouter/sessions
```

Trajectory dashboard:

```bash
open http://<tailscale-ip>:31415/_subrouter/dashboard
curl http://<tailscale-ip>:31415/_subrouter/transcripts
```

The dashboard reads transcript JSONL files from `/var/lib/subrouter/transcripts`
on the VM. It shows token usage over time, usage by user email, usage by selected
account, sanitized detail JSON, and raw internal trajectory JSON with decoded
body text under `/raw`.

Background GCS mirror:

```bash
sudo sed -i 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--transcript-gcs-uri=gs://<bucket>/<prefix> --transcript-gcs-sync-interval=5m"|' /etc/default/subrouter
sudo systemctl restart subrouter
```

The mirror runs inside the Subrouter daemon by calling `gsutil -m rsync -r`.
Upload failures are logged and retried; request proxying never waits for GCS.
