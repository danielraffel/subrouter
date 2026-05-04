# Subrouter

Subrouter is a local AI coding-agent proxy. It routes traffic across Codex accounts with sticky conversation-to-account assignment so cached context stays useful.

## Goals

- Run fast on a Mac Mini.
- Forward requests with normal Go reverse-proxy behavior, including headers and streaming responses.
- Support subscription accounts first, API keys second.
- Keep each conversation pinned to one account.
- Pick a fresh account for a new conversation based on available rate-limit headroom.
- Provide the Codex account manager and daemon in one Go binary.

## Current shape

```bash
make accounts
make run
```

This repo sets `CGO_ENABLED=0` in `Makefile` because the local macOS Go 1.22 toolchain is currently producing cgo test binaries that fail before startup with `missing LC_UUID load command`.

## Install

Install the released Go binary directly:

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sh
```

On a Linux server, install to `/usr/local/bin`:

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sudo sh
```

Install with npm:

```bash
npm install -g subrouter
```

Install with Python:

```bash
pipx install subrouter
```

All install paths provide `subrouter`, `sr`, and `cx`. The npm and Python wrappers download the matching Go release binary for macOS, Linux, Windows, FreeBSD, OpenBSD, or NetBSD on amd64, arm64, or supported 32-bit variants. Set `SUBROUTER_BIN` to use a local binary instead.

## Local macOS daemon

On macOS, install Subrouter as a localhost-only LaunchAgent:

```bash
make build
./bin/subrouter install-daemon
```

This installs the binary to `~/bin/subrouter`, installs `~/bin/cx` as a symlink to the same Go binary, writes `~/Library/LaunchAgents/ai.manaflow.subrouter.plist`, creates `~/.subrouter/transcripts`, starts the service, and runs:

```bash
~/bin/subrouter serve --addr 127.0.0.1:31415 --transcripts ~/.subrouter/transcripts --cx-switch-interval 10m
```

The 10 minute `cx` auto-switch interval is the default. Override it with `subrouter install-daemon --cx-switch-interval 5m`, or disable it with `--cx-switch-interval 0`.

## Linux systemd service

On a Linux server, install the binary and service:

```bash
curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | sudo sh
sudo sr install-systemd --addr 0.0.0.0:31415
```

This creates a `subrouter` system user, stores state under `/var/lib/subrouter`, writes `/etc/systemd/system/subrouter.service`, installs `subrouter`, `sr`, and `cx` in `/usr/local/bin`, and starts:

```bash
/usr/local/bin/subrouter serve --addr 0.0.0.0:31415 --sessions /var/lib/subrouter/sessions.json --transcripts /var/lib/subrouter/transcripts --cx-switch-interval 10m
```

If legacy `switchboard` or `gateway` services exist, `sr install-systemd` stops and disables them, merges their `/var/lib/...` state into `/var/lib/subrouter`, and preserves their extra service args.

Useful endpoints:

```text
GET /_subrouter/health
GET /_subrouter/accounts
POST /_subrouter/account-status
GET /_subrouter/sessions
GET /_subrouter/dashboard
GET /_subrouter/transcripts
```

## GCP deployment

See [deploy/gcp/README.md](deploy/gcp/README.md) for the small GCP + Tailscale Subrouter deployment flow.

To persist raw Subrouter transcripts, pass a transcript directory:

```bash
subrouter serve --transcripts ~/.subrouter/transcripts
```

Transcripts are JSONL files keyed by agent type and session id under `by-agent/<agent-type>/by-session/<agent-session-id>.jsonl`. They include Subrouter metadata, redacted headers, HTTP bodies, SSE bodies, and WebSocket message payloads as base64 with byte counts and SHA-256 hashes. Each event includes `agent_type` and `agent_session_id`; Codex events also include `codex_session_id` for matching `~/.codex/sessions` JSONL files. This is intentionally storage-heavy and can contain sensitive request/response payloads. Authorization-style headers are redacted, but bodies are stored in full.

When transcript recording is enabled, `/_subrouter/dashboard` serves an internal HTML dashboard over the same Subrouter listener. It shows token usage over time, usage by user email, usage by selected account, session assignments, transcript summaries, and links to sanitized transcript event JSON under `/_subrouter/transcripts/<agent-type>/<session-id>`. Raw internal trajectory JSON with decoded body text is available under `/_subrouter/transcripts/<agent-type>/<session-id>/raw`.

To mirror transcripts to GCS without blocking proxy requests, also pass a `gs://` destination:

```bash
subrouter serve --transcripts ~/.subrouter/transcripts --transcript-gcs-uri gs://bucket/prefix
```

The daemon shells out to `gsutil -m rsync -r` on a background interval. Local transcript writes stay on the request path; GCS upload failures are logged and retried later.

For best cache behavior, clients should send a stable header per conversation:

```text
X-Subrouter-Session: <conversation-or-thread-id>
```

If that header is missing, Subrouter checks Codex headers such as `x-codex-window-id` and `x-codex-turn-state`, common session headers, query params, and small JSON bodies for `session_id`, `conversation_id`, or `thread_id`.

Subrouter scopes sticky assignments and transcript files by agent type. It infers `codex`, `claude`, or `gemini` from provider session headers, and clients can set an explicit namespace:

```text
X-Subrouter-Agent: codex
```

For teammate-level graphs, clients can also send a self-reported user header:

```text
X-Subrouter-User-Email: alice@example.com
```

Subrouter stores the normalized email on the session assignment, includes it in proxy logs as `user`, and exposes it in `GET /_subrouter/sessions`. This is observability metadata, not authentication. To force a selected account, send `X-Subrouter-Account-ID`; API-key labels can omit the `apikey:` prefix. Subrouter strips `X-Subrouter-Session`, `X-Subrouter-Agent`, `X-Subrouter-User-Email`, `X-Subrouter-User`, `X-User-Email`, `X-Subrouter-Account-ID`, and `X-Subrouter-Account` before forwarding upstream.

## Codex CLI

`subrouter codex` is a direct Codex wrapper. Use it anywhere you would use `codex`:

```bash
subrouter codex
subrouter codex exec "your prompt"
subrouter codex --version
```

The wrapper injects this config override into the child Codex process:

```toml
openai_base_url = "http://127.0.0.1:31415/v1"
```

It does not edit Codex config or set auth environment variables. Do not set a dummy `OPENAI_API_KEY` for normal subscription routing. Leave Codex logged in the same way it already is. If Codex is in ChatGPT auth mode, `/model` keeps the subscription model picker. Subrouter replaces outbound credentials with the selected `cx` account before forwarding.

Override the subrouter URL with `SUBROUTER_CODEX_BASE_URL` if needed. See [docs/codex.md](docs/codex.md) for details and the custom-provider fallback.

If `SUBROUTER_CODEX_BASE_URL` is not set, the wrapper uses local `127.0.0.1:31415/v1`. To make `sr codex` use a remote Subrouter by default, register and select a named server:

```bash
sr server add team --url http://100.64.0.1:31415 --default
```

The server name is only a local nickname. Use whatever matches your setup, such as `team`, `prod`, or `staging`. For a one-off command, set `SUBROUTER_CODEX_SERVER=team`.
Rename a local server nickname with `sr server rename <old> <new>`.

Set `SUBROUTER_CODEX_USER_EMAIL` to attribute Codex traffic to a teammate:

```bash
SUBROUTER_CODEX_USER_EMAIL=alice@example.com subrouter codex exec "your prompt"
```

Force a specific Subrouter account, including an API-key account, with `SUBROUTER_CODEX_ACCOUNT_ID`:

```bash
SUBROUTER_CODEX_ACCOUNT_ID=team-codex-1 subrouter codex exec "your prompt"
SUBROUTER_CODEX_ACCOUNT_ID=apikey:team-codex-1 subrouter codex exec "your prompt"
```

When either variable is set, the wrapper uses a custom `subrouter` provider with WebSockets enabled so Codex can send `X-Subrouter-User-Email` and `X-Subrouter-Account-ID`. Subrouter still replaces outbound credentials before forwarding upstream. `SUBROUTER_CODEX_USER_EMAIL` is only teammate observability metadata; account selection belongs in `SUBROUTER_CODEX_ACCOUNT_ID`.

## Codex accounts

Subrouter has a native Go implementation of the Codex account manager. It reads and writes the existing `cx` store format:

```text
~/.codex-accounts/accounts/*.json
```

Server-owned OAuth accounts must be created with fresh logins because Codex refresh tokens rotate. Do not copy local OAuth account files to a server. To compare local OAuth emails with a configured server, validate server refresh-token chains, and reauth missing or invalid accounts on the server, run:

```bash
sr server sync team --device-auth
```

To only show the diff:

```bash
sr server diff team
```

`sr server sync` prints the plan and asks before opening login. Use `--yes` for unattended sync, `--email you@example.com` to reauth one email, or `--all` to replace every local OAuth email on the server with a new server-owned refresh-token chain. The server status check may refresh valid server-owned OAuth chains in place because Codex refresh tokens rotate.

Account uploads hot-reload the live server process after writing the new server-owned account file. Existing proxy and WebSocket connections keep running.

Account-management commands are built into the `subrouter` binary:

```bash
go run ./cmd/subrouter add
go run ./cmd/subrouter import
go run ./cmd/subrouter list
go run ./cmd/subrouter status
sr status
```

The supported Codex commands include `add`, `add-key`, `import`, `list`, `switch`, `gui-switch`, `remove`, `status`, `usage`, `server`, `add-admin-key`, `admin-keys`, `remove-admin-key`, `attach-project`, `claude`, and `gemini`. The older `subrouter cx <command>` form remains as a compatibility alias.

`cx switch` also syncs compatible ChatGPT Codex credentials into:

```text
~/.codex/auth.json
~/.local/share/opencode/auth.json      # provider key: openai
~/.pi/agent/auth.json                  # provider key: openai-codex
```

OpenCode uses XDG data home, so `XDG_DATA_HOME` changes its auth path. pi uses `PI_CODING_AGENT_DIR` when set. Existing unrelated provider credentials in those files are preserved.

Claude profiles are also native Go and use the existing `~/.codex-accounts/claude.json` format:

```bash
cx claude list
cx claude switch <profile>
cx claude env
cx claude run <profile>
```

Gemini has its own `cx gemini` namespace and store scaffold so future routing cannot collide with Codex or Claude state.

## Selection policy

On startup, Subrouter fetches current Codex usage for OAuth accounts and scores each account by its most constrained usage window. The scheduler keeps existing sessions sticky. For a new session it protects low-headroom accounts, spends healthy quota that resets soonest, then breaks ties by live assigned-session counts.
If all else ties, subscription OAuth accounts are preferred before API-key accounts.

The daemon also refreshes usage and updates Codex, OpenCode, and pi auth every 10 minutes by default so local agents follow the same OAuth-only policy. Configure it with `subrouter serve --cx-switch-interval 5m`, or disable it with `--cx-switch-interval 0`. If `--fetch-usage=false`, auto-switch is disabled because fresh usage is required.

By default, OAuth accounts are forwarded to `https://chatgpt.com/backend-api/codex` and API-key accounts are forwarded to `https://api.openai.com`. Subrouter accepts either `/v1/responses` or `/responses` from clients and normalizes the path for the selected account type.

Live headroom comes from Codex subscription usage. API-key spend comes from the OpenAI organization usage endpoints through stored `sk-admin-*` keys. Claude profile usage comes from the Anthropic OAuth usage endpoint when profile credentials are readable.

See [docs/saturation.md](docs/saturation.md) for the 5h/7d placement strategy and simulation tests.

## Security defaults

- Bind to `127.0.0.1` unless explicitly exposed.
- Do not log tokens, refresh tokens, API keys, request bodies, or full Authorization headers.
- Keep `~/.codex-accounts` credentials as the canonical local store.
