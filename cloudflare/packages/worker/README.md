# @subrouter/cloudflare-worker

Cloudflare **Durable Objects** subrouter: a per-org subrouter service with
sticky session affinity, per-model-family quota pools, Go-compatible
`/_subrouter/*` admin endpoints, and upstream proxying for Codex/OpenAI and
Claude/Anthropic accounts.

## Environments

| Env        | Worker name                       | URL                                  |
| ---------- | --------------------------------- | ------------------------------------ |
| dev        | `regatta-subrouter-do`            | `wrangler dev` -> http://127.0.0.1:8787 |
| staging    | `regatta-subrouter-do-staging`    | https://subrouter-staging.cmux.dev   |
| production | `regatta-subrouter-do-production` | https://subrouter.cmux.dev           |

Each env is a distinct Worker with its own Durable Object namespace (isolated
SQLite state), its own admin/proxy tokens, and its own Durable Object alarms.

## Deploy

```sh
bun run deploy:staging
bun run deploy:production
```

GitHub Actions runs `Cloudflare Durable Object` for changes under
`cloudflare/**`. Pull requests run install, typecheck, tests, and a Wrangler
dry-run bundle validation. Pushes to `main` deploy staging first, smoke
`https://subrouter-staging.cmux.dev/healthz`, then deploy production and smoke
`https://subrouter.cmux.dev/healthz`.

Required Actions secrets:

- `CLOUDFLARE_ACCOUNT_ID` - Cloudflare account ID for Wrangler.
- `CLOUDFLARE_API_TOKEN` - long-lived Cloudflare API token with Workers deploy
  permissions for both `regatta-subrouter-do-staging` and
  `regatta-subrouter-do-production`.

## Secrets

`ADMIN_TOKEN` gates the `/admin/*`, `/_subrouter/*`, and `/websocket-status`
endpoints. `PROXY_TOKEN` gates upstream proxy requests when present; if it is
unset, proxy requests use `ADMIN_TOKEN`. Set a separate proxy token when clients
should not share the admin credential:

```sh
bun run secret:admin:staging
bun run secret:admin:production
bun run secret:proxy:staging
bun run secret:proxy:production
```

## Local dev

Create `.dev.vars` (gitignored) with `ADMIN_TOKEN=<anything>` and
`PROXY_TOKEN=<anything>` then:

```sh
bun run dev                  # local workerd + DO SQLite on :8787
```

## OAuth refresh

OAuth account credentials can include `accessToken`, `refreshToken`,
`expiresAt`, `tokenEndpoint`, and `clientId`. The Durable Object refreshes
expired Codex and Claude OAuth credentials on use, persists rotated refresh
tokens atomically in SQLite, and schedules the next refresh with the Durable
Object Alarms API. Alarms wake the object in the future; long sleeps are not
used.

## Logs

```sh
bun run tail:staging         # wrangler tail --env staging
bun run tail:production
```

## Endpoints

- `GET /healthz`
- `GET /_subrouter/health`
- `GET /_subrouter/ready`
- `POST /_subrouter/drain`
- `GET /_subrouter/drain-status`
- `GET /_subrouter/accounts`
- `GET|POST /_subrouter/account-status`
- `GET /_subrouter/usage-status`
- `POST /_subrouter/reload-accounts`
- `GET /_subrouter/sessions`
- `GET /_subrouter/dashboard`
- `GET /_subrouter/transcripts`
- `GET /_subrouter/transcripts/:agent/:session`
- `GET /_subrouter/transcripts/:agent/:session/raw`
- `POST /route` — `{ orgId, sessionId, model?, preferAccountId?, quotaKey? }` → selected account
- `POST /usage` — `{ orgId, sessionId, accountId }`
- `GET /status?orgId=` — DO route counters
- `GET /ws?orgId=` — WebSocket; messages `{ type: "ping"|"route"|"usage"|"status", ... }`
- `GET /admin/accounts`, `POST /admin/accounts`, `POST /admin/model-probe`, `GET /websocket-status` — require `Authorization: Bearer $ADMIN_TOKEN`
