# Design Notes

Subrouter should be an account router, not another API-key vault.

## Core model

Each incoming request maps to:

```text
provider + conversation session id -> account credential -> upstream request
```

Existing sessions must keep their account assignment. New sessions can be placed on the best account at that moment.

Subrouter also scopes the session by agent type. Today Codex is the default, but Claude and Gemini use separate namespaces so identical provider session ids cannot collide.

## Session IDs

Preferred source:

```text
X-Subrouter-Session
```

Fallback extraction:

- `x-codex-window-id`
- `x-codex-turn-state`
- `x-codex-parent-thread-id`
- `X-Session-ID`
- `X-Conversation-ID`
- `X-Codex-Session-ID`
- `X-Claude-Session-ID`
- `X-Gemini-Session-ID`
- `X-Gemini-Conversation-ID`
- `OpenAI-Conversation-ID`
- `Anthropic-Conversation-ID`
- `Google-Conversation-ID`
- `Idempotency-Key`
- query params named `session_id`, `conversation_id`, or `thread_id`
- small JSON bodies containing `session_id`, `conversation_id`, or `thread_id`

If none exists, Subrouter creates a fallback ID from remote address, user agent, method, and path. That is acceptable for smoke tests but too coarse for real caching.

## Agent type

Clients can set an explicit agent namespace:

```text
X-Subrouter-Agent
```

Accepted values are lowercase token-style names such as `codex`, `claude`, and `gemini`. If the header is missing, Subrouter infers the type from provider-specific session headers. If nothing identifies the agent, Subrouter defaults to `codex` for current compatibility.

## User attribution

Clients can send a self-reported user email for teammate-level observability:

```text
X-Subrouter-User-Email
```

`X-Subrouter-User` and `X-User-Email` are accepted as aliases. Subrouter normalizes the address, stores it on the session assignment, includes it in proxy logs as `user`, and exposes it in `/_subrouter/sessions`.

This is not authentication. Subrouter strips `X-Subrouter-Session`, `X-Subrouter-Agent`, `X-Subrouter-User-Email`, `X-Subrouter-User`, and `X-User-Email` before forwarding upstream.

Clients can force a selected Subrouter account with:

```text
X-Subrouter-Account-ID
```

`X-Subrouter-Account` is accepted as an alias. This is intended for explicit API-key fallback or targeted debugging. API-key account labels may be sent without the `apikey:` prefix. Subrouter stores the resolved account id on the session assignment and strips both account-selection headers before forwarding upstream.

## Transcript persistence

`subrouter serve --transcripts <dir>` writes raw proxy transcript JSONL files under:

```text
<dir>/by-agent/<agent-type>/by-session/<agent-session-id>.jsonl
```

The `agent_session_id` is the base provider session id from Subrouter's session id. For Codex, `019...:0` maps to `019...`, matching `session_meta.payload.id` in Codex's own JSONL files under `~/.codex/sessions`. Codex events also include `codex_session_id` as a compatibility alias.

Transcript events include:

- `subrouter_meta`: account, user email, transport, path, upstream, and redacted headers.
- `http_body_chunk`: chunked HTTP or SSE body bytes as base64 with stream id, chunk index, offset, chunk byte count, and chunk SHA-256.
- `http_body`: final HTTP or SSE body summary with stream id, total byte count, total SHA-256, and chunk count.
- `websocket_message`: full WebSocket message payload as base64 with byte count, SHA-256, opcode, and direction.

This stores full payloads by design, but HTTP bodies are recorded as bounded chunks while the proxy streams them. Authorization-style headers are redacted, but bodies may contain sensitive plaintext or encrypted Codex transcript material.

## Scheduling

For each account, normalize all known rate windows into headroom:

```text
headroom = min(window_remaining_percent...)
```

New session selection:

1. Exclude accounts whose hard limit is reached.
2. Prefer usable subscription OAuth over API-key fallback.
3. Protect OAuth accounts below 40% bottleneck or short-window headroom.
4. Among healthy accounts, prefer the most bottleneck headroom expiring per second in the short window.
5. Prefer highest headroom.
6. Prefer fewer assigned active sessions.

Codex has both shorter rolling and daily/weekly style windows, so using the minimum headroom prevents saturating one window while another still looks available. Later, this can become weighted by expected task size.

Daemon `sr` auto-switching uses the same score refresh on an interval. It only considers usable OAuth Codex accounts, then writes the selected account auth to Codex's active auth file. If no usable OAuth account exists, it leaves the current active account alone. API-key accounts remain a proxy fallback and are not selected for active `sr` switching.

## Account sources

Codex:

- Read accounts from `~/.subrouter/codex/accounts/*.json`.
- OAuth accounts provide `tokens.access_token` and optional `tokens.account_id`.
- API-key accounts provide `OPENAI_API_KEY`.
- Login, imports, switching, API-key accounts, server install/login, and admin-key usage are native Go commands under `sr` and `subrouter`. The older `cx` and `subrouter cx` forms remain compatibility aliases.

Claude Code:

- Read profile metadata from `~/.subrouter/codex/claude.json`.
- Read per-profile credentials from `~/.subrouter/codex/claude/<profile>` or macOS Keychain using Claude Code's `Claude Code-credentials-<hash>` service naming.
- Profile switching, env output, run, remove, and OAuth login are native Go commands under `sr claude`.

Gemini:

- Use a separate `~/.subrouter/codex/gemini.json` namespace.
- Keep routing/session state separate from Codex and Claude even before Gemini credential import is fully implemented.

## Proxy behavior

The proxy should preserve incoming request shape and only inject the credential headers required for the selected account:

- Codex OAuth: `Authorization: Bearer <access_token>` and `ChatGPT-Account-ID` when available.
- API key: `Authorization: Bearer <api_key>`.
- Claude OAuth: provider-specific bearer token and beta headers.

Provider adapters should own provider-specific headers.

Codex has two upstream path conventions. ChatGPT subscription auth normally talks to `https://chatgpt.com/backend-api/codex/responses`, while API keys talk to `https://api.openai.com/v1/responses`. Subrouter accepts either `/v1/...` or bare paths from the client and normalizes them after selecting the account:

- OAuth account: strip a leading `/v1` and forward to the Codex backend.
- API-key account: add `/v1` if the client sent a bare path and forward to the OpenAI API.

This lets Codex use one `openai_base_url` while Subrouter still chooses the right upstream for each account type.
