# Antigravity integration

> **Status:** OAuth support is integrated and covered by local tests. The Google API Key / Interactions path is **experimental and disabled for ordinary dispatch by default** until its complete request, streaming, tool-call, usage, and error wire contract is verified against a real upstream or an authoritative SDK/protocol sample.

## Overview

Codex2API can manage Google Antigravity accounts as a dedicated upstream channel and expose their models through the OpenAI-compatible `/v1/responses`, `/v1/chat/completions`, `/v1/messages`, and `/v1/models` surfaces. Antigravity accounts are isolated from Codex and Grok account groups and can be selected explicitly with an API key whose upstream channel is `antigravity`.

Two credential shapes are supported:

| Credential | Upstream | Authentication | Status |
| --- | --- | --- | --- |
| Google OAuth | `cloudcode-pa.googleapis.com/v1internal:*` (with the configured fallback hosts) | `Authorization: Bearer ...` and `x-goog-user-project` | Integrated |
| Google API Key | `generativelanguage.googleapis.com/v1beta/interactions` | `x-goog-api-key` | Experimental; dispatch disabled by default; real wire contract not yet verified |

## Default models

When an account has no explicit or synchronized model list, the channel exposes this conservative fallback catalog:

- `gemini-3-pro-preview`
- `gemini-2.5-pro`
- `gemini-2.5-flash`

An API Key account can declare its own model list and optional model mapping. The management `models` endpoint returns the declared list or these defaults; it does not claim to have remotely verified the catalog.

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

## Supported gateway endpoints

Antigravity inference is admitted through `/v1/responses`, `/v1/chat/completions`, and `/v1/messages`; its models are exposed through `/v1/models` and the Codex model manifest. Chat and Messages translate their inbound body into a Responses payload before dispatch, which is exactly what the `v1internal` adapter consumes, so all three transports share one admission gate and one adapter. `/v1/responses/compact` still excludes Antigravity accounts because no compaction adapter exists, and the official Codex executors reject Antigravity credentials before any network request as a provider-boundary safeguard.

## Function tools

Responses function tools are bridged into Gemini `functionDeclarations`. Dropping them is not a safe degradation: the upstream still receives the system instruction describing those tools, answers with a call it was never allowed to declare, and terminates the turn as `MALFORMED_FUNCTION_CALL`. Built-in Codex tools (web search, image generation, computer use) have no `v1internal` equivalent and are still ignored. Operators can pin the bridge off for diagnostics, which makes a `tool_choice` that forces a function fail closed with a 400 instead:

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
