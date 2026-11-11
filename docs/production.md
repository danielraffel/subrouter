# Production readiness

Use this checklist before putting a shared Subrouter on a tailnet or a public-facing VM.

## Listener

- Bind public/shared servers to the tailnet address or a private network. Avoid raw internet exposure.
- Keep `/_subrouter/health` unauthenticated for liveness checks.
- Use `/_subrouter/ready` for readiness checks. It returns 503 while the process is draining.
- Set `SUBROUTER_ADMIN_TOKEN` for any non-loopback listener. Sensitive admin endpoints then require `Authorization: Bearer <token>` or `X-Subrouter-Admin-Token: <token>`.
- Set a distinct `SUBROUTER_ACCOUNT_IMPORT_TOKEN` for account onboarding. It authorizes only `GET` and `POST /_subrouter/account-import`; it cannot read admin APIs or proxy traffic.
- Credential transfer uses authenticated HTTP `GET` and `POST`, never SSH, SCP, or gcloud.

## Linux install

```bash
ADMIN_TOKEN="$(openssl rand -hex 32)"
IMPORT_TOKEN="$(openssl rand -hex 32)"
sudo sr install-systemd \
  --addr 0.0.0.0:31415 \
  --admin-token "$ADMIN_TOKEN" \
  --account-import-token "$IMPORT_TOKEN"
```

Store the same token in local server config for CLI management:

```bash
sr server add team \
  --url http://100.64.0.1:31415 \
  --admin-token "$ADMIN_TOKEN" \
  --account-import-token "$IMPORT_TOKEN" \
  --default
```

`install-systemd` preserves existing admin and account-import tokens when their flags are omitted. `/etc/default/subrouter` is written with mode `0600`.

## Docker

Use [`deploy/docker/compose.yaml`](../deploy/docker/compose.yaml) for local-account or cmux.com team mode. Both profiles run non-root on a read-only root filesystem, mount credentials as Docker secrets, bind loopback by default, and enforce a 256 MiB memory limit. See [`deploy/docker/README.md`](../deploy/docker/README.md).

## Transcripts

Transcript recording is off by default because it stores full request and response payloads and can grow quickly. For a shared server, only enable it with cloud upload and local cleanup:

```bash
sudo sed -i 's|^SUBROUTER_TRANSCRIPTS=.*|SUBROUTER_TRANSCRIPTS=/var/lib/subrouter/transcripts|' /etc/default/subrouter
sudo sed -i 's|^SUBROUTER_TRANSCRIPT_ARGS=.*|SUBROUTER_TRANSCRIPT_ARGS="--transcripts=/var/lib/subrouter/transcripts"|' /etc/default/subrouter
sudo sed -i 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--transcript-gcs-uri=gs://<bucket>/<prefix> --transcript-gcs-sync-interval=5m --transcript-gcs-sync-timeout=30m --transcript-local-retention=24h --transcript-max-local-bytes=2GiB"|' /etc/default/subrouter
sudo install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/transcripts
sudo systemctl restart subrouter
```

Local cleanup runs only after a successful GCS sync. Before deleting a local transcript, Subrouter copies it to an immutable object under the destination `_archive/` prefix.

## Draining

Before replacing a process, ask it to drain:

```bash
curl -fsS -X POST http://127.0.0.1:31415/_subrouter/drain
curl -fsS http://127.0.0.1:31415/_subrouter/drain-status
```

Drain mode rejects new proxy sessions but allows active sessions to continue. This is enough for a controlled shutdown, but it does not keep the listener available for new clients while the old process drains.

## Upgrades

Current production-safe behavior:

- SIGTERM/SIGINT switches the process into drain mode.
- The HTTP server waits up to `--shutdown-timeout` for in-flight proxy requests to finish.
- systemd units use `TimeoutStopSec=10min`.
- On macOS, `subrouter supervise` owns the stable client listener and starts workers on inherited private sockets. Give it an explicit `--local-data-socket` beneath a current-user-owned mode-0700 directory. The v2 binding pins that mode-0600 socket's kernel identity, and every new connection mutually proves the selected store before credential-bearing traffic can use it. Public TCP and Tailscale Serve are never accepted as the v2 credential channel. A v1/direct daemon retains its legacy loopback attestation only for rolling compatibility. `POST /_subrouter/upgrade` starts and health-checks the replacement worker, routes new connections to it, and keeps every existing connection pinned to the old worker until it closes.
- The supervisor binary is installed separately and is not replaced by routine worker updates.
- SIGTERM/SIGINT closes the supervisor listener, waits up to `--drain-timeout` (default `10m`) for accepted connections, then stops its workers.

The macOS updater and one-time LaunchDaemon migration scripts live in `deploy/macos/`. Run the migration without `--activate` first to prepare and validate the supervised plist. Activation is the last upgrade that replaces the public listener; later worker upgrades do not restart the supervisor.

## New-session headroom reserve

Subrouter reads `SUBROUTER_MIN_NEW_SESSION_HEADROOM` once when each process starts. The value must be a float in `(0,1]` and is clamped to `[0.05,0.90]`; invalid values use the default `0.40` and log a warning. If the existing `~/.config/subrouter/cloud.json` is used, its optional `minNewSessionHeadroom` number is used when the environment variable is unset. The effective value is logged at startup, and values below `0.40` log a warning.

For a macOS LaunchAgent, launchd does not inherit the shell environment. Add the variable to the existing plist's `EnvironmentVariables` dictionary, for example:

```bash
/usr/libexec/PlistBuddy -c 'Add :EnvironmentVariables:SUBROUTER_MIN_NEW_SESSION_HEADROOM string 0.15' \
  "$HOME/Library/LaunchAgents/ai.manaflow.subrouter.plist"
```

Then deploy the new worker with `deploy/macos/subrouter-deploy.sh install <candidate-binary>` and request the supervisor upgrade through its control socket. The supervisor inherits the LaunchAgent environment for replacement workers, health-checks the candidate, routes new connections to it, and lets existing connections finish on the old worker. Do not restart the supervisor just to change this value. To revert, set the variable to `0.40` (or remove it and remove `minNewSessionHeadroom` from `cloud.json`), then perform the same worker upgrade.

For the Linux systemd deployment, set `SUBROUTER_MIN_NEW_SESSION_HEADROOM=0.15` in `/etc/default/subrouter` (or the unit's environment source), run the normal worker deployment/upgrade path, and let the supervisor replace the worker. A plain `systemctl restart subrouter` drains/restarts the service and is not connection-preserving.

## Launch checks

Run these before announcing a shared server:

```bash
curl -fsS http://<server>:31415/_subrouter/health
curl -fsS http://<server>:31415/_subrouter/ready
sr server status team
```

These client-side checks are the launch gate. Infrastructure operators can inspect the service journal or Cloud Logging separately, but ordinary users never need shell access to the VM.
