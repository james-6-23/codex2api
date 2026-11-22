# Antigravity integration

> **Status:** OAuth support is integrated and covered by local tests. The Google API Key / Interactions path is **experimental and disabled for ordinary dispatch by default** until its complete request, streaming, tool-call, usage, and error wire contract is verified against a real upstream or an authoritative SDK/protocol sample.

## Overview

Codex2API can manage Google Antigravity accounts as a dedicated upstream channel and expose their models through the OpenAI-compatible `/v1/responses`, `/v1/chat/completions`, `/v1/messages`, and `/v1/models` surfaces. Antigravity accounts are isolated from Codex and Grok account groups and can be selected explicitly with an API key whose upstream channel is `antigravity`.

Two credential shapes are supported:

| Credential | Upstream | Authentication | Status |
| --- | --- | --- | --- |
| Google OAuth | `cloudcode-pa.googleapis.com/v1internal:*` (with the configured fallback hosts) | `Authorization: Bearer ...` and `x-goog-user-project` | Integrated |
| Google API Key | `generativelanguage.googleapis.com/v1beta/interactions` | `x-goog-api-key` | Experimental; dispatch disabled by default; real wire contract not yet verified |

## Model discovery and defaults

OAuth model availability comes from each account's Google control-plane catalog. The gateway refreshes enabled OAuth accounts' model/quota catalogs every 30 minutes, and retains the last successful catalog when a refresh fails or returns an empty catalog. A model appearing in another account or in a reference project's static list does not grant access to it. Model discovery is not a successful generation test.

When an account has no explicit or synchronized model list, its fallback catalog projects to these public gateway IDs:

- `gemini-3.8-flash-low`, `gemini-3.8-flash-medium`, `gemini-3.8-flash-high`
- `gemini-3.7-flash-low`, `gemini-3.7-flash-medium`, `gemini-3.7-flash-high`
- `gemini-3.6-flash-low`, `gemini-3.6-flash-medium`, `gemini-3.6-flash-high`
- `gemini-3.1-pro-low`, `gemini-3.1-pro-high`
- `gpt-oss-120b-medium`

These defaults are compatibility fallbacks, not a statement of current plan entitlements. Argon and Claude are intentionally absent from the fallback. Claude 5.5 Opus/Sonnet low, medium, and high tiers, and the older Claude 4.6 IDs, are published only when their backing models exist in the account catalog. Older Gemini mappings remain available for accounts whose catalogs still contain them; updating the static catalog in another project does not remove those mappings here.

An API Key account can declare its own model list and optional model mapping. Its management `models` endpoint returns the declared list or these defaults without claiming remote verification. Frontend model selectors prefer the account-derived channel catalog over their static fallback.

### Gemini 4 Argon

An OAuth account must discover the backing model `gemini-4-argon-high` before the gateway publishes these fixed-tier IDs:

| Public model ID | Upstream model ID | `thinkingLevel` |
| --- | --- | --- |
| `gemini-4-argon-low` | `gemini-4-argon-high` | `LOW` |
| `gemini-4-argon-medium` | `gemini-4-argon-high` | `MEDIUM` |
| `gemini-4-argon-high` | `gemini-4-argon-high` | `HIGH` |

Low and medium are gateway tier names, not separately discovered upstream models. For Responses requests, the bare alias `gemini-4-argon` accepts `reasoning.effort` values `low`, `medium`, and `high`, defaulting to `high` when omitted. An administrator's model redirect can override the bare alias; a fixed-tier suffix takes precedence over a conflicting Responses effort. Chat Completions and Messages use the same conversion path. Native Gemini requests preserve an explicit `generationConfig.thinkingConfig`; only when absent does the gateway supply the requested tier's `thinkingLevel`.

Disabled placeholders such as `gemini-4-argon-disabled` remain visible in the account's model list with a disabled overlay. Connection-test model selectors hide disabled placeholders, and automatic tests do not select them. Models without quota metadata show an unknown quota rather than an invented remaining percentage. Disabled state is derived from the current ID's `disabled` segment, not persisted separately: when a later catalog replaces the placeholder with a normal model ID, the overlay disappears and the new model appears as a normal test choice, even before quota metadata arrives. Placeholders are not advertised as callable models through the public inference API and cannot enter ordinary scheduling.

The Codex model manifest folds available Argon tiers into one `gemini-4-argon` choice with separate reasoning controls and a 1,048,576-token context window. The named levels and context metadata follow [CPA v8.0.28's Argon registry entry](https://github.com/router-for-me/CLIProxyAPI/commit/866d84d086749c1c1e8b32dac93f46230a5d43a0). The manifest advertises text input to match the gateway's Responses adapter; this does not claim image-input support on that path.

As of 2026-10-14, [Google's model availability documentation](https://antigravity.google/docs/models/) lists Argon for Google AI Ultra and Enterprise, and Claude 5.5 for non-trial Google AI Pro subscriptions, Ultra, and Enterprise. Actual routing still requires the account's discovered backing model. Argon generation against a real upstream has not yet been verified by this project; the available local validation accounts are Pro accounts whose refreshed catalogs contain the disabled placeholder rather than the callable Argon backing.

### Model pricing

There is no verified Argon-specific built-in tariff. A custom or synchronized pricing entry for the billed model ID can override the gateway's default; without one, Argon uses the generic fallback estimate. Do not interpret that estimate as Google's published Argon price. After discovery, the model appears in the model-pricing page where an operator can configure its price.

## Adding accounts

### Browser OAuth

OAuth uses the official Antigravity Desktop client when nothing else is configured, so **Start authorization** works out of the box. Custom clients override that fallback. Precedence is environment variable > admin settings > built-in official client:

1. **Admin settings page** (Settings → Antigravity): add `key` / `client_id` / `client_secret` entries and optionally pick the default active client. Changes are stored in `system_settings.antigravity_oauth_config`, take effect immediately without a restart, and the secret is never echoed back by the API (leave the secret blank when editing to keep the stored value).
2. **Environment variables**: `ANTIGRAVITY_OAUTH_CLIENTS` is a semicolon-separated list of `key|client_id|client_secret` entries; `ANTIGRAVITY_OAUTH_CLIENT_KEY` optionally selects the active key.
3. **Built-in official Desktop client** (`key=official`): used only when both sources above are empty. This is the same public installed-app credential shipped in the Antigravity desktop app and reused by `sub2api`. Google may revoke a shared client; add your own entry if authorization starts returning `invalid_client`.

```bash
ANTIGRAVITY_OAUTH_CLIENTS='primary|your-client-id|your-client-secret'
ANTIGRAVITY_OAUTH_CLIENT_KEY='primary'
```

Environment entries take precedence for a matching key, and the active key resolves as environment variable > admin setting > first configured entry > built-in `official`, so a deployment-level override always wins over a misconfigured database value. Keep custom credentials in the deployment secret store or the admin settings page rather than source control.

Open **Accounts → Antigravity → Google OAuth**, start authorization, and complete Google's consent flow. If the loopback callback cannot complete automatically, paste the full callback URL back into the dialog.

OAuth accounts synchronize a Google subject, project, permissions, quota snapshots, and model metadata when the upstream supplies them. A 401 may trigger one OAuth token refresh/retry. This recovery is never used for API Key accounts.

### Workspace / enterprise accounts

Google provisions a managed Cloud Code project only for consumer tiers (free tier, Google One AI Pro/Ultra). Workspace accounts on Gemini Code Assist Standard or Enterprise (`standard-tier`, `userDefinedCloudaicompanionProject: true`) are never given one: `onboardUser` completes without a project, the account cannot be dispatched, and quota-summary calls fail with "You do not have a valid license of this product".

For these accounts, open **Edit** on the account and set **GCP project ID** to a Google Cloud project that has Gemini for Google Cloud enabled and that the account can use. Saving stores it as `antigravity_user_project_id` and immediately resyncs. The project is sent to `loadCodeAssist` and (for non-free tiers) `onboardUser`, matching Gemini CLI's `GOOGLE_CLOUD_PROJECT` behavior; Google-returned projects still take precedence. The free tier never receives the user project because naming one there fails with `FAILED_PRECONDITION`. Clear the field to return to Google-managed projects.

### Imported OAuth credentials

The Antigravity account dialog accepts a single credential JSON / refresh token or multiple JSON files. Imported credentials are synchronized before normal use when possible. Account-list responses never return access tokens, refresh tokens, client secrets, or API keys.

### Google API Key

Choose **API Key** in the add-account dialog and provide:

- Google API Key;
- an optional explicit model list;
- an optional JSON model-mapping object;
- optional proxy and Antigravity account groups.

API Key accounts use `plan_type=api`, do not require an OAuth project ID, and do not expose OAuth token/quota refresh actions in the admin UI. They remain visible to administration and explicit capability probes while ordinary `/v1/responses` scheduling stays fail-closed unless the operator sets `ANTIGRAVITY_ENABLE_EXPERIMENTAL_INTERACTIONS=true`.

## Downstream API key channel restriction

When creating or editing a Codex2API downstream API key, choose **Antigravity** as its upstream channel to restrict dispatch to compatible Antigravity accounts. `auto` keys may route by model across Codex, Grok, and OAuth Antigravity accounts according to the available scoped model catalog. Experimental API-key accounts join either catalog only after the explicit environment opt-in above.

Account groups are channel-isolated: an Antigravity account can only join an Antigravity group.

## Balanced affinity switch

First binding and fresh session selection bias Antigravity candidates by concurrency occupancy, used quota, and the account's recent rate-limit penalty, so a credential Google just throttled sinks to the back of the queue while healthy ones keep serving new sessions.

```
ANTIGRAVITY_BALANCED_AFFINITY=false
```

The switch is on by default. `0`, `false`, `off`, `none`, and `no` (case-insensitive) turn the weighting off and restore plain round-robin; any other value keeps it on. It is read once when the process starts, so changing it requires a restart.

Cooldown correctness is independent of this switch and always applies: the public and shared wire keys are cooled together (the Gemini-family key is added when the quota bucket is exhausted), a shared wire/family deadline is never shortened by a briefer event on the same account, and cooldowns written by another instance are honoured during selection. Entries whose candidate pool excludes Antigravity accounts (`/v1/responses/compact`, native remote compaction, the `/v1/responses` WebSocket upgrade path) never apply the weighting, even behind an Antigravity key.

## Supported gateway endpoints

Antigravity inference is admitted through `/v1/responses`, `/v1/chat/completions`, and `/v1/messages`; its models are exposed through `/v1/models` and the Codex model manifest. Chat and Messages translate their inbound body into a Responses payload before dispatch, which is exactly what the `v1internal` adapter consumes, so all three transports share one admission gate and one adapter. `/v1/responses/compact` still excludes Antigravity accounts because no compaction adapter exists, and the official Codex executors reject Antigravity credentials before any network request as a provider-boundary safeguard.

## Native Gemini API (`/v1beta`)

Antigravity OAuth accounts also support the native Gemini REST shape for clients that speak `generativelanguage.googleapis.com/v1beta` directly (for example ADK, LangChain Gemini adapters, or MCP stacks that post raw Gemini JSON).

| Endpoint | Method | Notes |
| --- | --- | --- |
| `/v1beta/models` | GET | Lists scoped Antigravity model IDs in Gemini `models[]` shape (`name`, `displayName`, `supportedGenerationMethods`). |
| `/v1beta/models/{model}` | GET | Returns one model record; accepts IDs with or without the `models/` prefix. |
| `/v1beta/models/{model}:generateContent` | POST | Non-streaming generate; unwraps the Cloud Code envelope back to Gemini JSON. |
| `/v1beta/models/{model}:streamGenerateContent` | POST | SSE stream; supports `?alt=sse`. |
| `/v1beta/models/{model}:countTokens` | POST | Local token estimate (no upstream round trip). |

Requirements and behavior:

- **OAuth only.** API Key / Interactions accounts are rejected; bind a downstream key to the `antigravity` upstream channel (or use an `auto` key whose scoped catalog includes Antigravity models).
- **Authentication.** Send the Codex2API downstream key as `Authorization: Bearer <key>`, `x-api-key: <key>`, or `x-goog-api-key: <key>`. The last form is what the `google-genai` SDK, ADK, and the Gemini channel of aggregator gateways send by default, so pointing their base URL at Codex2API works without a custom header. The `?key=` query-string form is not accepted (the key would land in URLs and access logs).
- **Model IDs** match the published Antigravity catalog (`gemini-3.7-flash-high`, `gemini-3.8-flash-low`, `gemini-4-argon-high`, Claude tiers exposed by the channel, and so on), not raw Google wire IDs.
- **Tool schemas** are normalized before upstream dispatch: orphan `required` entries are dropped, `anyOf` / `oneOf` / `allOf` unions are flattened, invalid function names are sanitized with response-name restore, and schemas are sent as `parametersJsonSchema` (Cloud Code expectation).
- **Multi-turn tools:** `functionResponse` turns are regrouped for the adapter; leading `inlineData` parts in the same user turn are attached under the matching `functionResponse.parts`.
- **Thinking:** client-supplied `generationConfig.thinkingConfig` is preserved; when absent, the gateway applies the tier default for the requested public model.
- **Usage accounting:** both generate methods record upstream token usage, including cached input and thinking. Logged output includes thinking tokens, with reasoning also shown as a subset; native response fields remain unchanged. Streaming usage is read through the final usage frame, even when it follows `finishReason`, and repeated cumulative snapshots are not added together. First-token latency measures the time from upstream dispatch to the first content part (text, thinking, tools, or media), excluding keepalives and usage-only frames. Missing upstream usage is not estimated; historical zero-token logs cannot be reconstructed from those logs alone.
- **Thought signatures:** missing stubs are injected on outbound `functionCall` parts for every `gemini-*` backing, including models reached through raw catalog pass-through; stale signatures on inbound `functionResponse` parts are stripped to avoid replay failures.
- **countTokens:** OAuth accounts call upstream `/v1internal:countTokens` with the same request normalization as generateContent (minus `project`, `model`, `sessionId`, `toolConfig`, and safety settings). When no account is available or upstream returns 404, the gateway falls back to a local token estimate.
- **Tool schema cleanup:** MCP-style bare property maps, boolean `required` flags, local `$ref` / `$defs` JSON Pointer inline (including nested pointers, `anyOf` `$ref` branches, sibling overrides, and cycle fallback), orphan `required` entries, union flattening, missing array `items`, and unsupported enums are normalized before upstream dispatch.

Example:

```bash
curl -s http://127.0.0.1:2004/v1beta/models/gemini-3.7-flash-high:generateContent \
  -H "Authorization: Bearer $CODEX2API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"contents":[{"role":"user","parts":[{"text":"Say hello"}]}]}'
```

Gemini-native clients authenticate with the same key through `x-goog-api-key`; for example with the `google-genai` Python SDK:

```python
import os
from google import genai

client = genai.Client(api_key=os.environ["CODEX2API_KEY"], http_options={"base_url": "http://127.0.0.1:2004"})
client.models.generate_content(model="gemini-3.7-flash-high", contents="Say hello")
```

Not yet implemented on this surface: batch/embed APIs, Imagen `predict`, and full parity with every `generativelanguage` metadata field on model list responses.

## Function tools

Responses function tools are bridged into Gemini `functionDeclarations`. Dropping them is not a safe degradation: the upstream still receives the system instruction describing those tools, answers with a call it was never allowed to declare, and terminates the turn as `MALFORMED_FUNCTION_CALL`. Built-in Codex tools (web search, image generation, computer use) are not translated by this adapter and are still ignored. Adding Argon or discovering a model with upstream search support does not enable Responses `web_search` in the gateway. Operators can pin the bridge off for diagnostics, which makes a `tool_choice` that forces a function fail closed with a 400 instead:

```bash
ANTIGRAVITY_FUNCTION_TOOLS_ENABLED=false
```

## API Key Interactions request assumption

Production dispatch through this executor requires the explicit process environment opt-in:

```bash
ANTIGRAVITY_ENABLE_EXPERIMENTAL_INTERACTIONS=true
```

Without that setting, API-key accounts are excluded from scheduling and model discovery. Explicit admin capability probes still work so an operator can test a key without silently enabling customer traffic.

The current experimental executor sends:

```http
POST https://generativelanguage.googleapis.com/v1beta/interactions
Content-Type: application/json
Accept: application/json | text/event-stream
x-goog-api-key: <google-api-key>
```

It currently preserves the OpenAI Responses-style request body and injects:

```json
{
  "model": "gemini-*",
  "agent": "antigravity-preview-05-2026",
  "stream": true
}
```

Unit tests use an `httptest` endpoint override and verify that Codex2API constructs this endpoint, header set, and payload. They do **not** prove that Google accepts this body or returns OpenAI Responses-compatible JSON/SSE.

An opt-in production-executor integration test is available:

```bash
ANTIGRAVITY_INTERACTIONS_TEST_API_KEY='...' go test ./proxy -run TestAntigravityInteractionsRealUpstream -v
```

Optional safe controls are `ANTIGRAVITY_INTERACTIONS_TEST_MODEL`, `ANTIGRAVITY_INTERACTIONS_TEST_INPUT` (maximum 512 bytes), and `ANTIGRAVITY_INTERACTIONS_TEST_STREAM=true` for the separately gated SSE subtest. The test skips with an explicit message when the key is absent, never prints the key, caps/redacts failure diagnostics, checks the actual status and content type, and parses the native JSON/SSE envelope without assuming OpenAI compatibility. A successful real-upstream run is required before production certification. A local skip is not a pass and does not remove this experimental warning.

Before production use, verify at least:

- whether `input` uses OpenAI Responses semantics;
- whether `agent` belongs in the body, a header, or a query parameter;
- whether streaming requires `alt=sse` or another parameter;
- JSON and SSE event envelopes;
- usage/token fields;
- function/tool call request and response shapes;
- error envelopes and retry semantics;
- accepted model IDs for `/v1beta/interactions`.

Until that verification exists, keep the default disabled policy for production traffic. Enabling the environment flag is an explicit acceptance of the incomplete wire adapter and its compatibility risk.

## Security and service-risk notes

- Credentials are persisted by the gateway. In the current account credential design, a Google API Key is stored in plaintext in the database credential payload. Protect database backups, admin access, logs, and host filesystem permissions; restrict the key at Google Cloud and rotate it regularly.
- Do not paste production credentials into a demo or untrusted deployment.
- Use of Google services must comply with the applicable Google terms, product policies, quotas, and account restrictions. Automated or unsupported access can result in throttling, suspension, or account loss.
- Preview models and estimated prices may change. Confirm current Google pricing before using the values for billing or customer charging.
- Credential exports contain live OAuth tokens/client secrets or a plaintext Google API Key. The admin-only export route uses attachment responses with `Cache-Control: no-store`, `Pragma: no-cache`, and `X-Content-Type-Options: nosniff`; browsers, reverse proxies, backups, and operators must still treat the downloaded JSON/ZIP as a high-value secret. Delete or encrypt it after use.
- Duplicate checks for manual add/import are serialized inside one process, but the database does not yet enforce a cross-replica uniqueness constraint for every OAuth/API-key credential family. Multiple gateway replicas sharing one database can therefore race if operators submit the same new credential concurrently. Serialize credential creation through one administrative replica (or otherwise avoid concurrent duplicate submissions) and audit/merge duplicates before enabling multi-replica writes.

## Credential export

`GET /api/admin/accounts/antigravity/export` requires normal admin authorization. With one matching account it returns `application/json; charset=utf-8`; with multiple accounts it returns `application/zip` containing one sanitized-filename JSON member per account. `ids=1,2` limits the selection; omitting `ids` exports all active Antigravity accounts. A selection containing only missing or wrong-channel accounts returns `404`.

OAuth documents include the access/refresh/ID tokens, project, OAuth client selection/client credentials, scope, and expiry needed by the existing Antigravity importer. API-key documents explicitly use `auth_kind: "api_key"` and `api_key`; they also preserve declared models and model mapping. Ordinary list/state endpoints never include these secret fields.

## State, sync, and capability management

- `GET /api/admin/accounts/:id/antigravity/state` is read-only and never calls Google. It returns credential kind, catalog source/verification, identity/project status, sanitized permissions/quota, generation-fenced capability observations, timestamps, and warnings.
- `POST /api/admin/accounts/:id/antigravity/sync` refreshes the Google identity/control plane for OAuth accounts. For API-key accounts it only persists the declared/default local catalog with `verified: false`; it does not fabricate remote catalog, quota, permission, project, or Interactions verification.
- `POST /api/admin/accounts/:id/antigravity/capabilities/probe` performs one bounded non-stream request against the first configured/default model with a one-token output limit. This explicit action consumes a small amount of generation quota. Only a successful HTTP/JSON response persists `verified: true`; transport, status, content-type, or envelope failures remain unverified. State reads and sync never silently run it.
- `GET /api/admin/accounts/:id/test?model=<published-id>` is the shared connection test, now wired for Antigravity. It sends the configured test prompt through the same Cloud Code executor as live dispatch (endpoint fallback, 429/503 quota mapping, one refresh-and-retry on 401 for OAuth accounts) and streams `test_start` / `content` / `test_complete` or `error` SSE events, so the account page's "Test connection" button shows the model's actual reply. The model must be one of the account's published IDs; without `model` it prefers the global test model, then the cheapest `*-flash-*-low` tier. A success clears cooldown/error state; 429/503 record a per-model cooldown instead of marking the account failed. Batch and recycle-bin tests use the same path.
- `GET/PUT /api/admin/settings/channel-tests` stores per-channel connection-test defaults for Antigravity and Claude (`{"antigravity":{"test_model","test_content","test_concurrency"},"claude":{...}}`, persisted in `system_settings.channel_test_config`). `test_concurrency` (0~200, 0 = global) is applied to batch tests whose accounts all belong to that channel; mixed batches keep the global concurrency. The model must be in the tested account's catalog or automatic selection is used; empty content falls back to the global test content. The response also carries `model_choices` (union of the pool's catalogs) and `default_test_content` for the settings page. Grok and Codex keep using the global `test_model` / `test_content`.
- `GET/PUT /api/admin/settings/antigravity` stores channel settings in `system_settings.antigravity_config`. `model_redirects` maps a bare logical model to one of its own fixed tiers (`{"gemini-3.8-flash":"gemini-3.8-flash-high"}`); a request that names the bare model without `reasoning.effort` then runs as that tier on every entry point (Responses fold, Chat Completions, Messages, API-key Interactions). `redirect_overrides_effort` makes the redirect win even when the request carries an explicit effort. Suffixed tier IDs are never redirected, and the response lists `choices` (each logical model with its default level and tier IDs) for the settings page. `expose_thoughts` (default `false`, settings page "Thinking output") forwards Gemini thinking on OAuth accounts: the converted Cloud Code request carries `includeThoughts: true` for both the `thinkingLevel` (3.8) and `thinkingBudget` tiers, and `thought` parts become a leading `reasoning` output item with `summary_text` (streamed as `response.reasoning_summary_text.delta`), which Chat Completions exposes as `reasoning_content` and Messages as a `thinking` block. A `/v1/responses` request with `reasoning.summary: "none"` opts out (Chat Completions has no such field; its translation always asks for `summary: "auto"`). The reasoning item carries no `encrypted_content`; replayed reasoning items are dropped before the next upstream call, so tool calls keep using the stub thought signature. Cloud Code's `streamGenerateContent` rarely emits thought text even with `includeThoughts` (verified 2026-10-03: thought tokens are counted but the summary is almost never streamed) while `generateContent` always returns it, so non-streaming Chat Completions and Messages requests switch to `generateContent` and replay the answer as one SSE event (no first-token timeout applies); streaming requests keep upstream streaming and usually get no reasoning. The native `/v1beta` path already passes `thought` parts through and only gains `includeThoughts` on the injected `thinkingLevel` config. API-key Interactions are unaffected.

## Relevant admin endpoints

- `POST /api/admin/accounts/antigravity`
- `PATCH /api/admin/accounts/:id/antigravity`
- `POST /api/admin/accounts/antigravity/import`
- `POST /api/admin/accounts/antigravity/models`
- `POST /api/admin/accounts/antigravity/batch-models`
- `GET /api/admin/accounts/antigravity/export`
- `POST /api/admin/accounts/antigravity/oauth/start`
- `POST /api/admin/accounts/antigravity/oauth/complete`
- `POST /api/admin/accounts/:id/antigravity/refresh`
- `POST /api/admin/accounts/:id/antigravity/quota`
- `GET /api/admin/accounts/:id/antigravity/state`
- `POST /api/admin/accounts/:id/antigravity/sync`
- `POST /api/admin/accounts/:id/antigravity/capabilities/probe`
- `POST /api/admin/accounts/antigravity/clean-banned`
- `POST /api/admin/accounts/antigravity/clean-error`

All endpoints above use the existing `/api/admin` authorization middleware. Export responses are secret-bearing; state/sync/probe responses are sanitized.
