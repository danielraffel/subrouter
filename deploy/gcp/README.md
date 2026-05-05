# GCP Subrouter Deployment

This deploys Subrouter as `subrouter` on a small Debian VM and exposes the service over Tailscale.

Defaults:

- Instance: `subrouter-team`
- Local server name: `team`
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

Install `sr` locally:

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh
```

Create the VM:

```bash
deploy/gcp/create-subrouter-vm.sh
```

Install or upgrade Subrouter on the VM:

```bash
deploy/gcp/publish-subrouter.sh
```

The publish script configures the server with `sr server add`, then runs `sr server install`. The VM downloads the public release with the same curl installer and runs `sr install-systemd`; no locally built binary is copied to the server. If legacy `switchboard` or `gateway` services exist on the VM, the systemd installer disables them and migrates their state into `/var/lib/subrouter`.

Join or rejoin the host to Tailscale with an auth key:

```bash
export TAILSCALE_AUTH_KEY=<tailscale-auth-key>
deploy/gcp/publish-subrouter.sh
```

The publish script joins with `--accept-routes=false --accept-dns=false` so the VM does not use tailnet routes or tailnet DNS for its own outbound traffic.
The VM also installs a host firewall rule that rejects new outbound connections from `tailscale0` to tailnet IP ranges while still allowing replies to inbound requests.

Add a server-owned Codex OAuth account when the VM should route real Codex traffic:

```bash
sr server login team --device-auth
```

OAuth refresh tokens rotate on use, so do not copy an existing OAuth refresh-token file to the server. `sr server login` performs a fresh Codex login, uploads only that fresh account to `/var/lib/subrouter/codex/accounts`, asks the live Subrouter process to reload accounts in place, then restores your previous local auth so only the server owns the new refresh-token chain. Existing proxy and WebSocket connections keep running.

To compare local OAuth emails with the server and reauth every missing local email on the server, run:

```bash
sr server sync team --device-auth
```

This validates the server refresh-token chains, shows missing or invalid accounts, asks for confirmation, then walks through one fresh login per selected email. Use `sr server diff team` to inspect the diff without logging in, `--email you@example.com` for a single account, `--all` to replace every server copy, or `--yes` to skip the confirmation prompt. The status check may refresh valid server-owned OAuth chains in place because Codex refresh tokens rotate.

The old account-file upload helper is kept only as a compatibility wrapper:

```bash
deploy/gcp/upload-codex-accounts.sh team --device-auth
```

It delegates to `sr server login` and rejects the previous `--move` and `--copy-unsafe` paths.

## Client usage

Use the Tailscale IP or MagicDNS name:

```bash
export SUBROUTER_CODEX_BASE_URL=http://<tailscale-ip>:31415/v1
export SUBROUTER_CODEX_USER_EMAIL=alice@example.com
subrouter codex
```

Or select the named server as your default Codex route:

```bash
sr server use team
sr codex
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
