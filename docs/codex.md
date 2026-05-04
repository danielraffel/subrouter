# Codex CLI Integration

Current Codex does not use an `OPENAI_BASE_URL` environment variable for the built-in OpenAI provider. Use the Subrouter wrapper.

## Recommended

Use `subrouter codex` anywhere you would use `codex`:

```bash
subrouter codex
subrouter codex exec "your prompt"
subrouter codex resume --last
subrouter codex --version
```

The wrapper injects this global config override into the child Codex process:

```toml
openai_base_url = "http://127.0.0.1:31415/v1"
```

Subrouter supports Codex WebSocket requests, so the built-in provider can keep its normal transport behavior.

Do not set a dummy `OPENAI_API_KEY` for normal subscription routing. Codex should stay logged in normally, ideally with ChatGPT auth. Subrouter replaces the outbound Authorization and `ChatGPT-Account-ID` headers with the selected `cx` account.

## User Attribution

Use `SUBROUTER_CODEX_USER_EMAIL` when a teammate should be visible in Subrouter logs and session data:

```bash
SUBROUTER_CODEX_USER_EMAIL=alice@example.com subrouter codex exec "your prompt"
```

Use `SUBROUTER_CODEX_ACCOUNT_ID` when a run should use one explicit Subrouter account, including an API-key account:

```bash
SUBROUTER_CODEX_ACCOUNT_ID=team-codex-1 subrouter codex exec "your prompt"
SUBROUTER_CODEX_ACCOUNT_ID=apikey:team-codex-1 subrouter codex exec "your prompt"
```

Codex does not allow overriding arbitrary headers on the built-in `openai` provider. When either variable is set, `subrouter codex` switches the child process to a custom `subrouter` provider with WebSockets enabled and sends `X-Subrouter-Agent: codex`, plus `X-Subrouter-User-Email` and/or `X-Subrouter-Account-ID`. Subrouter still replaces outbound credentials before forwarding upstream. `SUBROUTER_CODEX_USER_EMAIL` is only teammate observability metadata; account selection belongs in `SUBROUTER_CODEX_ACCOUNT_ID`.

## Models

There are two separate Codex concepts:

- `model`: the model slug selected by `/model`.
- `model_provider`: the backend/provider config.

Subrouter keeps `model_provider = "openai"` and does not rewrite the `model` field. `/model` continues to use Codex's own model catalog and auth-mode filtering. If Codex is logged in with ChatGPT auth, subscription-only models stay visible. If Codex is forced into API-key auth by `OPENAI_API_KEY`, Codex filters the picker to API-supported models.

Subrouter accepts `/v1/responses` and `/responses`. For OAuth subscription accounts it forwards to `https://chatgpt.com/backend-api/codex` and strips the `/v1` prefix when present. For API-key accounts it forwards to `https://api.openai.com` and adds `/v1` when needed.

## Custom Provider Fallback

If WebSocket support needs to be disabled for debugging, use a custom provider:

```bash
OPENAI_API_KEY=dummy codex exec \
  -c 'model_provider="subrouter"' \
  -c 'model_providers.subrouter.name="Subrouter"' \
  -c 'model_providers.subrouter.base_url="http://127.0.0.1:31415/v1"' \
  -c 'model_providers.subrouter.env_key="OPENAI_API_KEY"' \
  -c 'model_providers.subrouter.wire_api="responses"' \
  -c 'model_providers.subrouter.supports_websockets=false' \
  "your prompt"
```

## Env Vars

- `SUBROUTER_CODEX_BASE_URL`: base URL injected by `subrouter codex`; defaults to `http://127.0.0.1:31415/v1`.
- `SUBROUTER_CODEX_SERVER`: named server from `sr server add`; ignored when `SUBROUTER_CODEX_BASE_URL` is set.
- `SUBROUTER_CODEX_BIN`: Codex binary used by the wrapper; defaults to `codex`.
- `SUBROUTER_CODEX_USER_EMAIL`: optional self-reported user email. When set, the wrapper sends `X-Subrouter-Agent: codex` and `X-Subrouter-User-Email` through a custom Subrouter provider.
- `SUBROUTER_CODEX_ACCOUNT_ID`: optional Subrouter account id or API-key label. When set, the wrapper sends `X-Subrouter-Account-ID` and Subrouter forces that account for the session.
- `OPENAI_API_KEY`: only for real API-key mode or custom env-key providers. Avoid setting it when you want ChatGPT subscription model behavior.
- `CODEX_HOME`: optional. Use it to test an isolated Codex config.
- `OPENAI_ORGANIZATION` and `OPENAI_PROJECT`: Codex forwards these as OpenAI headers for the built-in OpenAI provider.
- `CODEX_OSS_BASE_URL` and `CODEX_OSS_PORT`: only affect OSS providers such as Ollama or LM Studio, not the OpenAI provider.
