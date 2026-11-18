# Kiro integration

> **Status:** integrated. Social (Google) sign-in accounts were verified end to end against the real upstream (Messages, Responses and Chat Completions, streaming and non-streaming, tool calls, token refresh, credits and model sync, connection/quality/intelligence tests). Builder ID, Enterprise (IAM Identity Center), external IdP and Kiro API key credentials are covered by unit tests and mocked upstreams only.

The protocol layer is a Go port of [ZyphrZero/kiro.rs](https://github.com/ZyphrZero/kiro.rs) pinned to revision `c37fe5afe7ee3adba836abfcee9a97aba5ea54f9` (MIT). See [third_party/kiro.rs](../third_party/kiro.rs/NOTICE).

## Overview

Kiro is the fifth upstream channel (`kiro`), next to Codex, Claude, Antigravity and Grok. A Kiro account serves the models in its own Kiro catalog through every public inference surface:

| Surface | Path |
| --- | --- |
| `/v1/messages` | Native: the Kiro core accepts Anthropic Messages and emits Anthropic SSE/JSON directly. |
| `/v1/responses` | Dedicated Kiro bridge: Responses → Messages → Kiro, then back to Responses events. Custom tools (for example `apply_patch`), namespaced MCP tools, hosted `web_search` and Codex inline compaction (`compaction_trigger`) are restored on the way back. |
| `/v1/chat/completions` | Converted to Responses by the existing gateway path, then served by the same bridge. |
| `/v1/models` | Publishes each Kiro account's catalog with `owned_by: "kiro"`. |
| `/v1/messages/count_tokens` | The gateway's existing local estimate, shared by every channel. |

Kiro is HTTP-only. Kiro accounts never enter the Responses WebSocket pool or the dedicated `/v1/responses/compact` executor.

Scheduling follows the rest of the gateway: an API key with upstream channel `auto` puts Kiro accounts in the same pool as the other channels and orders candidates by account priority; on `/v1/messages`, native Claude accounts are preferred. A key restricted to channel `kiro` only dispatches to Kiro accounts.

## Credentials

| Kind | Source | Refresh |
| --- | --- | --- |
| `social` | Google / GitHub sign-in through the Kiro portal | `prod.{region}.auth.desktop.kiro.dev/refreshToken` |
| `idc` | AWS Builder ID or Enterprise (IAM Identity Center) device code | AWS OIDC `CreateToken` with the registered client |
| `external_idp` | Enterprise external identity provider (Microsoft Entra ID) | The provider's token endpoint (validated against an allowlist) |
| `api_key` | Kiro API key (`ksk_…`) | None; the key is the bearer token |

Access tokens are refreshed in the background before expiry and once more after an upstream `401`. Kiro does not rotate social refresh tokens.

### Import

`POST /api/admin/accounts/kiro/import` detects the format automatically:

- kiro.rs `credentials.json` (an object or an array of objects);
- the Kiro IDE `kiro-auth-token.json`;
- a Kiro Account Manager export (`{"version": …, "accounts": [...]}`).

Each account is imported separately: credentials are exchanged, the profile ARN, credits and model catalog are fetched, and duplicates (same refresh token or API key) are rejected. One failing entry does not abort the others.

### Export

`GET /api/admin/accounts/kiro/export?format=kam|kiro-rs&ids=1,2` downloads the Kiro Account Manager format (default) or a kiro.rs `credentials.json` array. Both are secret-bearing and round-trip through the importer.

### Interactive sign-in

- **Social:** `POST /api/admin/accounts/kiro/social/auth-url` returns the Kiro portal URL. The portal redirects to a `localhost` callback that a remote deployment cannot receive, so the operator pastes the final browser URL into `POST /api/admin/accounts/kiro/social/exchange`.
- **Device code:** `POST /api/admin/accounts/kiro/device/start` (Builder ID by default, or an Enterprise `start_url` and `region`) returns a user code; the page polls `POST /api/admin/accounts/kiro/device/poll` until the account is created.

## Models

The catalog comes from Kiro's `ListAvailableModels` and is refreshed with the credits snapshot every 30 minutes, by the per-account **Sync** button, and by **Refresh all models** on the pricing page. An account that has never synced uses a conservative built-in list; it never accepts arbitrary model IDs, so `auto` keys do not route other channels' models to Kiro. `auto` is not published.

Dashed and dotted Claude versions are equivalent (`claude-sonnet-4-6` = `claude-sonnet-4.6`). A `-thinking` suffix enables thinking for clients that cannot send the `thinking` field.

## Protocol behavior

Ported from kiro.rs:

- Anthropic ↔ Kiro conversion: system prompts, history, images (resized/re-encoded as needed), tool definitions and results, long tool names, `tool_choice`, effort, prefill handling.
- Thinking from native `reasoningContentEvent` or `<thinking>` tags, with signatures; when the upstream rejects a historical thinking signature, the request is retried once without historical reasoning.
- Tool-call JSON validation, refusal handling, and filtering of `<invoke>` XML that leaks into assistant text.
- Structured output emulation (`output_config.format` JSON Schema).
- Hosted `web_search` through Kiro's MCP endpoint.
- Repetition guard, context-window-full and content-filter stop reasons.
- Upstream error mapping: invalid bearer → `401` (refresh and retry), monthly request limit → cooldown until the next reset, account throttling → 30-minute cooldown, suspended account → error state, generic `429` → 1-minute cooldown.
- The credits sync applies the same cooldown before any request fails: an account whose credits are used up (with overage disabled) cools as `usage_limited` until Kiro's reported reset, the cooldown follows a changed reset time, and it is released once a later snapshot shows credits again.

When structured output is requested with only `web_search`, the gateway uses the model/search loop to generate the answer before extracting JSON. The plain-search shortcut remains available when no schema is requested. Schema extraction prefers candidates satisfying supported bounds and falls back to kiro.rs's basic rules if none do; it is not a guarantee of strict schema compliance. Numeric bounds and nested equality preserve decimal precision.

Image decoding uses a conservative budget for pixels and decoder working buffers, including transparent 16-bit PNG and progressive JPEG. Images beyond the budget are forwarded without local decoding. The gateway advertises image input and parallel tool calls for all available Kiro models, including mapped models, without per-model or per-account selection. Catalog input-type metadata does not narrow this channel policy. Mixed-channel models retain the gateway's existing capability intersection rules.

During rolling upgrades from older Kiro builds, remove all old instances serving Kiro accounts from routing, stop their background refresh and admin/manual refresh entry points, and wait for in-flight Kiro requests and refreshes to finish before enabling the new deployment. Stopping only background refresh workers is insufficient: old instances can also refresh after an upstream `401`. Keep old instances stopped or unable to serve Kiro traffic throughout the handover; they do not retry under a new lease after observing a rotated token.

Not ported (kiro.rs-specific): its own account pool and load balancing, self-update, Feishu alerts, the `/cc/v1` path, the Claude Code tool-compatibility mode, the overage switch and credit columns in its usage log.

## Usage accounting

Exact usage from Kiro's `metadataEvent` is used when present. Otherwise input tokens come from `contextUsageEvent` (minus output) or a local estimate.

Kiro does not report prompt-cache hits. **Settings → Kiro → Local prompt cache simulation** (off by default) estimates `cache_creation_input_tokens` / `cache_read_input_tokens` from the request's `cache_control` breakpoints (20-block lookback, 5-minute and 1-hour TTLs, at most 4 breakpoints), so `input + cache_creation + cache_read` still equals the total. It only changes usage records; upstream cost is unaffected. The simulation keeps an in-process store per instance (kiro.rs's Redis and on-disk persistence are not ported).

## Settings

| Setting | Where | Default |
| --- | --- | --- |
| Upstream domain | Settings → Kiro (`PUT /api/admin/settings/kiro` `api_domain`) | `amazonaws` (`q.{region}.amazonaws.com`); `kiro` switches to `*.kiro.dev` |
| Prompt cache simulation | Settings → Kiro (`cache_simulation`) | Off |
| Connection test model / prompt / concurrency | Settings → Kiro (`/api/admin/settings/channel-tests` `kiro`) | Model picked from the catalog, haiku first |
| Automatic intelligence checks | Intelligence check settings, source `kiro` (`kiro_model`) | Off; model `claude-sonnet-4.6` |

Environment variables for image handling: `KIRO_IMAGE_RESIZE` (`0`/`false` disables resizing), `KIRO_IMAGE_MAX_LONG_SIDE` (default `1568`), `KIRO_IMAGE_MAX_BYTES` (default `400000`), `KIRO_IMAGE_JPEG_QUALITY` (default `85`).

The Kiro IDE version used in request fingerprints, including the credits and model-catalog calls, is fetched from Kiro's release metadata and cached for 6 hours; a built-in version is used when the lookup fails. Control-plane calls send the profile ARN the IDE uses (the shared Social or Builder ID ARN when the account has none): current versions reject them without it, and Kiro rejects `ListAvailableModels` for Student accounts from outdated versions.

## Admin endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/admin/accounts/kiro/import` | Import credentials (`content`, optional `name`, `proxy_url`, `use_proxy_pool`) |
| `GET` | `/api/admin/accounts/kiro/export` | Export (`format=kam` or `kiro-rs`, optional `ids`) |
| `POST` | `/api/admin/accounts/kiro/social/auth-url` | Start a social sign-in |
| `POST` | `/api/admin/accounts/kiro/social/exchange` | Finish it with the pasted callback URL |
| `POST` | `/api/admin/accounts/kiro/device/start` | Start a Builder ID / Enterprise device-code sign-in |
| `POST` | `/api/admin/accounts/kiro/device/poll` | Poll the device-code sign-in |
| `POST` | `/api/admin/accounts/:id/kiro/sync` | Sync one account's credits and models |
| `POST` | `/api/admin/accounts/kiro/sync` | Sync all enabled Kiro accounts |
| `GET` / `PUT` | `/api/admin/settings/kiro` | Upstream domain and cache simulation |

Account listing, enable/disable, delete, connection tests, quality tests, intelligence checks, groups, proxies and usage statistics use the shared account endpoints with `channel=kiro`.

## Security notes

Kiro credentials (refresh tokens, client secrets, API keys) are stored in the account credentials column like other channels' credentials. Exports and imports carry them in plain text; treat those files as secrets.
