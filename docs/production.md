# Production readiness

Use this checklist before putting a shared Subrouter on a tailnet or a public-facing VM.

## Listener

- Bind public/shared servers to the tailnet address or a private network. Avoid raw internet exposure.
- Keep `/_subrouter/health` unauthenticated for liveness checks.
- Use `/_subrouter/ready` for readiness checks. It returns 503 while the process is draining.
- Set `SUBROUTER_ADMIN_TOKEN` for any non-loopback listener. Sensitive admin endpoints then require `Authorization: Bearer <token>` or `X-Subrouter-Admin-Token: <token>`.

## Linux install

```bash
TOKEN="$(openssl rand -hex 32)"
sudo sr install-systemd \
  --addr 0.0.0.0:31415 \
  --admin-token "$TOKEN"
```

Store the same token in local server config for CLI management:

```bash
sr server add team \
  --url http://100.64.0.1:31415 \
  --admin-token "$TOKEN" \
  --default
```

`install-systemd` preserves an existing `SUBROUTER_ADMIN_TOKEN` if `--admin-token` is omitted. When a token is configured, `/etc/default/subrouter` is written with mode `0600`.

## Transcripts

Transcript recording is off by default because it stores full request and response payloads and can grow quickly. For a shared server, only enable it with cloud upload and local cleanup:

```bash
sudo sed -i 's|^SUBROUTER_TRANSCRIPTS=.*|SUBROUTER_TRANSCRIPTS=/var/lib/subrouter/transcripts|' /etc/default/subrouter
sudo sed -i 's|^SUBROUTER_TRANSCRIPT_ARGS=.*|SUBROUTER_TRANSCRIPT_ARGS="--transcripts=/var/lib/subrouter/transcripts"|' /etc/default/subrouter
sudo sed -i 's|^SUBROUTER_EXTRA_ARGS=.*|SUBROUTER_EXTRA_ARGS="--transcript-gcs-uri=gs://<bucket>/<prefix> --transcript-gcs-sync-interval=5m --transcript-local-retention=24h --transcript-max-local-bytes=2GiB"|' /etc/default/subrouter
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

True zero-drop binary upgrades still need a stable supervisor listener that owns `:31415` and routes to versioned workers. The worker primitives are now present: `/_subrouter/ready`, `/_subrouter/drain`, and active proxy request accounting.

## Launch checks

Run these before announcing a shared server:

```bash
curl -fsS http://<server>:31415/_subrouter/health
curl -fsS http://<server>:31415/_subrouter/ready
sr server status team
```

Then check logs for refresh and routing failures:

```bash
ssh <server> 'journalctl -u subrouter --since "2 hours ago" --no-pager | grep -Ei "WARN|ERROR|failed|401|502|503|no usable|refresh_token" | tail -n 200'
```
