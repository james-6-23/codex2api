# Changelog

## v2.4.0 - 2026-07-02

### Features

- **K12 plan classification and badge in the account list (#307).** The plan filter gains a K12 tab and k12 accounts get a green plan badge. The three separately hard-coded premium-plan checks on the frontend (accounts page, quota distribution chart, rate-limit recovery chart) are unified into a single `isPremiumUsagePlan` aligned with the Go side (including k12/edu/education/go), so k12 accounts now render the 5h usage bar and participate in the 5h quota distribution / rate-limit recovery charts.

### Fixes

- **WebSocket relay no longer reuses a connection whose stream was cut short — eliminating cross-session response mixing (#308).** When the downstream write failed (broken pipe / client disconnect) or the request context was canceled mid-stream, the read loop treated it as a normal end and `Close()` returned the connection to the pool — while the upstream was still pushing the remainder of that response on the same connection. The next request that reused it received the previous user's response frames. The release decision is now a whitelist: a connection is returned to the pool only when the stream was consumed to an explicit terminal boundary (`response.completed` / `response.failed` / upstream error frame) with no anomaly; any early exit (downstream write failure, context cancellation, upstream close, handshake failure with an unread stream) discards the connection. The ctx-cancel watcher also closes the pipe before tearing down the WS response so the client sees the cancellation instead of a spurious read error.
- **k12/edu and other paid plans are now covered by premium 5h rate-limit semantics (#306, #309).** `isPremium5hPlan` only recognized plus/pro/team, so after a k12 account hit 429 and was put on cooldown, the periodic wham usage probe judged it "not a premium plan" and proactively cleared the cooldown — the account flipped back to "available" while still being rate-limited upstream (the "shows available but still 429" report). All paid plans that carry a rolling 5h window (k12/edu/education/go, falling back to `IsPlusOrHigherPlan`) are now included; the plan gate errs broad on purpose since the rate-limit state additionally requires an observed 5h window at 100%.
- **k12 plans fully aligned with team semantics (#282).** k12 is an education team workspace whose quota applies per workspace, but several plan checks missed it: k12 accounts got no scheduler score bias (+50 like team) and effectively sank to the bottom of the pool ordering; 429 cooldown inference fell into the unknown-plan conservative default instead of team's 5h/7d dual-window detection; and the frontend "unsampled" check only looked at 7d usage, so k12-style workspaces that report only a 5h window were stuck showing "unsampled" forever — it now treats data in either window as sampled.
- **First-run setup no longer rejects Docker bridge sources (#199).** The bootstrap endpoint's IP allowlist (introduced in v2.1.4) only accepted loopback or `BOOTSTRAP_ALLOWED_CIDR`, but with Docker port mapping the host browser's source IP is the bridge address (e.g. `172.17.0.1`), so the most common local Docker deployment was blocked from the initialization page with a 403. Private and link-local sources are now allowed by default when `BOOTSTRAP_ALLOWED_CIDR` is unset (public IPs are still rejected; the endpoint remains rate-limited, audited, and only active while no admin secret is configured). Setting `BOOTSTRAP_ALLOWED_CIDR` makes the configured CIDRs authoritative (loopback always allowed), and the new `none` value restores the strict loopback-only mode. The 403 message now includes self-service guidance, and `.env.example` documents the variable.

## v2.3.9 - 2026-07-01

### Features

- **Prompt-filter review supports multiple Moderations API keys (#289).** Low-tier OpenAI accounts only get 50,000 TPM on the Moderations endpoint, so a single key can't keep up with high volume. The prompt-filter review key is upgraded from a single value to a key pool: multiple keys separated by newlines/commas are round-robined (package-level atomic cursor) to spread the quota, and on a `429`/`401`/`403`/`5xx`/network error the call automatically switches to the next key, only handing off to `FailClosed` once every key has failed. Reuses the existing TEXT column to store multiple lines (no DB migration), and the frontend becomes a multi-line textarea showing the configured key count. Single-key configuration behavior is unchanged.
- **Compact endpoint auto-strips the `-openai-compact` model suffix.** Aggregating gateways like newapi append an `-openai-compact` suffix to `/responses/compact` requests (e.g. `gpt-5.4-openai-compact`). The suffix is now stripped before compact model mapping and validation, so the newapi channel keeps that naming while codex2api treats it as the base model (`gpt-5.4`) internally and forwards upstream, avoiding an "unsupported model" rejection. Only applies to `/v1/responses/compact` — plain `/responses` and the global model mapping are untouched.
- **Per-account request count limit.** Added a per-account cumulative request-count cap; accounts that reach the limit drop out of scheduling.
- **Stronger account import/add dedup with optional force-duplicate.** AT imports are deduplicated by OAuth identity, and re-importing valid credentials clears an error/banned (401) state to auto-unban the account; single-account RT/ST adds — which previously had no dedup at all — now dedup by raw credential too. Added an "allow duplicate add" toggle (off by default) that skips dedup and forces a new record, wired through both single-add and batch-import paths. The batch-import dialog gains a unified proxy input (the backend already accepted `proxy_url`; the frontend now sends it).
- **Hardened Codex client identification strategy.** Added strict official-client identification so a UA merely carrying a codex token no longer triggers an automatic compatibility upgrade; improved Codex client version parsing to cover more official UAs and trailer scenarios; added an engine-fingerprint signal model plus client allow/deny-list data structures and validation logic, with unit tests covering strict/legacy behavior, version boundaries, and policy validation.
- **Improved Codex custom tool event compatibility.** Extended compatibility handling for Codex custom tool incremental events (including custom deltas).
- **Removed the usage-reset radar module (`/admin/subscriptions`).** Deleted the Codex Reset Radar implementation on both ends, including the backend `admin/reset_radar.go` with its tests/routes/handler status fields and the frontend Subscriptions page, route, nav item, API, types, and copy.

### Fixes

- **Support socks5/http proxies with special characters in the password (#293).** When a proxy password contained URL-reserved characters like `#` or `?`, both the frontend `new URL` and the backend `url.Parse` failed to parse it, so adding `socks5://user:pass@ip:port` reported "please enter a valid proxy address" and couldn't dial even if added. Added `security.ParseProxyURL` (standard parse first, IPv6-compatible; on failure, percent-encodes the userinfo and retries, then uniformly validates scheme/host/port range), wired into all three dialer parse sites; the frontend validation switches to `new URL` with a lenient regex fallback so special-character passwords are no longer wrongly rejected.
- **Injected image tool no longer forces plain requests off WebSocket (#304, #288).** The WS→HTTP downgrade decision changed from "does `tools[]` contain an `image_generation` tool" to "is there real image-generation intent": HTTP is only forced for image-only models, `tool_choice=image_generation`, top-level image params, or natural-language image-generation intent, while ordinary requests stay on WS and the injected tool is stripped on the WS path via `stripResponsesImageGenerationTool`. The `chat/completions` path uses the same decision, gaining natural-language intent detection along the way.
- **Sync subscription expiry after a manual account refresh (#300).** After a renewal, the token JWT's `chatgpt_subscription_active_until` lags, so relying on token refresh alone kept an account's validity stuck at its stale creation-time value. A successful refresh now also fires a zero-cost wham probe to sync the subscription expiry and usage from the server's authoritative data, so both single and batch refreshes update the validity immediately.
- **Rate-limit cooldown and usage snapshot now track official window resets automatically.** Fixed two opposite-direction state desyncs: after usage is reset early upstream the cooldown wasn't cleared (`probeUsageViaWham` now re-evaluates against wham's authoritative window data even while rate-limited and proactively calls `ClearCooldown` when no longer limited); and after a rate limit expired the usage bar stayed at its old value (`NeedsUsageProbe` now fires a single zero-cost wham refresh when the 5h window reset time has passed but the snapshot still shows pre-reset high usage, guarded by `maxAge` to prevent repeated probing).
- **Fixed engine-fingerprint signal matching boundaries.** The fingerprint signal type is now trimmed before matching so a valid config with surrounding whitespace still hits; `header_exact` now iterates the original header keys with case-insensitive matching; added regression tests for signal types and non-canonical header keys.
- **Prevent Codex User-Agent from falling back to the Go default (#299).** Guards against the upstream Codex request's User-Agent unexpectedly reverting to Go's default value.

### Security

- **Bumped `golang.org/x/image` to v0.43.0.** Clears a security-scan alert.

### Build

- **Container `go mod download` uses the goproxy.cn mirror.** Speeds up dependency fetching during container builds.

## v2.3.8 - 2026-06-20

### Fixes

- **Codex 专属 `at-...` Access Token 不再按 JWT 解码，并可通过 WHAM 自动补齐账号身份。** `at-...` 现在会识别为 `codex_at` 类型，导入/添加时不会再尝试按 JWT 解析；账号列表的 AT 徽章会显示为 `codex_at`。对于已经导入但缺少邮箱或 `account_id` 的 codex_at 账号，后续单账号用量刷新或后台 wham 用量探针会把 WHAM 返回的 `email` / `account_id` / `user_id` 写回运行时和数据库，无需重新导入。

## v2.3.7 - 2026-06-19

### Features

- **Per-account on-demand usage refresh button (accounts).** Added `POST /api/admin/accounts/:id/usage/refresh`, which synchronously runs `ProbeUsageSnapshot` (preferring the wham endpoint — zero quota cost, no test conversation) and returns the latest 5h/7d usage. The usage column now shows a refresh icon next to each progress bar (wired into the desktop table, mobile cards, and personal mode) that re-pulls that account's bars instantly, with a spinner and failure toast. Also fixes progress bars not refreshing after a test connection: accounts that just finished a test are now force-scheduled for a delayed re-pull even when they already have usage data (e.g. showing 100%), bypassing the "has data = fresh" check in `needsUsageReload`.
- **Editable Prompt Filter extra rules.** The prompt-filter "extra rules" can now be edited from the UI (add/update/remove) instead of being config-only, with full validation feedback on the edit form.
- **Sync WHAM subscription expiry.** The subscription expiry time is now parsed from the WHAM usage response and used to refresh `subscription_expires_at` in both the runtime and the database via the wham probe, with added time-format compatibility and persistence-consistency tests.

### Fixes

- **Codex invite dropdown no longer hides disabled/abnormal but credential-usable accounts (#281).** Relaxed `isCodexInviteCandidate` to match the backend: only relay / AT-only accounts are excluded, dropping the `enabled`/`locked`/`status` filters, since `SendCodexInvite` only requires an access token and does not check those fields — otherwise accounts that were merely paused from scheduling or temporarily abnormal (but still credential-usable) were hidden. The account picker also gains status dots + disabled/locked/banned/error badges on items and the selected account, a light warning when an abnormal-but-usable account is selected, and full keyboard navigation (↑↓ to move, Enter to confirm, Esc to close, highlight scrolls into view).
- **"Normal" account card count vs. filter mismatch yielding an empty list.** The "normal accounts" card counts as `total − abnormal − rate-limited` (folding `refreshing` and similar non-abnormal/non-rate-limited states into "normal"), but clicking the "normal" filter applied an extra hard `status ∈ {active, ready}` constraint that excluded `refreshing`/`cooldown` states. With many accounts refreshing this produced a card showing "40k+ normal" that opened to an empty list. Both paths now use the same health semantics (abnormal > rate-limited > normal): the "normal" filter is `not abnormal && not rate-limited`, matching the card exactly.
- **Transient failures no longer force admin logout (#admin auth).** `checkAuth` previously cleared `admin_key` and forced re-login on any `catch` or `!res.ok`, so any network blip / service restart / 5xx during the 30s polling loop logged the user out and required re-entering the key. The key is now cleared only on a genuine 401 (invalid key); under transient network/5xx failures an existing key optimistically stays logged in and the next poll self-corrects.
- **Improved Prompt Filter hit-log context display.** Refined how prompt-filter match context is captured and rendered in the hit logs for clearer surrounding context.

## v2.3.6 - 2026-06-18

### Features

- **API key self-service usage portal `/key-usage` (#271).** Added a public, login-free `/key-usage` page where a carpool/shared-key user can paste their own API key and view that key's usage (totals, model breakdown, recent logs) without admin access. The API key management page now surfaces the portal address with copy and open shortcuts, and each key has a shareable direct link, backed by a dedicated public usage endpoint that only exposes the data for the presented key.
- **Per-key usage reset (#271).** A single API key's accumulated usage can now be reset (`reset_quota`) when editing the key, zeroing just that key's counters without minting a new key — so monthly re-accounting for shared/carpool keys no longer requires recreating keys.
- **5h guard-band slowdown concurrency (#270).** Added a configurable "guard band" before the 5h usage auto-pause threshold: as an account's remaining 5h quota enters the band (default 5 percentage points), its scheduler dispatch score is progressively penalized and its dynamic concurrency is capped to a configured ceiling (default 1), giving accounts a soft landing instead of slamming into the hard auto-pause. Both the band width and the guard concurrency are configurable from Settings (with global defaults), and disabling either turns the slowdown off.

## v2.3.5 - 2026-06-17

### Features

- **Per-request upstream isolation by default (#268).** For requests without an explicit session, the upstream identity key (`prompt_cache_key` + session/conversation id) was previously derived deterministically from the downstream API key, so different requests sharing one API key collapsed onto the same upstream session id and leaked context across requests. The upstream identity is now decoupled from local connection reuse and account affinity: by default each sessionless request gets a unique upstream identity (`isolated` mode), while the 8-slot WS connection pool still routes by a stable API-key-derived pool key (preserving reuse and 503 handshake-throttle resistance) and account affinity is unchanged. Set `CODEX_REQUEST_ISOLATION_MODE=per-api-key` to restore the old shared-cache behavior.
- **Active in-flight request indicator (accounts).** The account list status column now shows a blue pill with a breathing dot and the live in-flight request count (`active_requests`), visible only when greater than zero, to surface which accounts are busy and how concurrent.
- **Display `chatgpt_account_id` (team workspace id).** The account list now shows the `chatgpt_account_id` (the team-plan workspace id decoded from the access-token JWT) under the email in monospace, so multiple workspaces under the same login email can be told apart.
- **Hardened + optimized OpenAI active reset-credits flow.** The wham/usage and reset/consume calls now go through the uTLS Chrome-fingerprint transport (matching the `/responses` gateway) to reduce Cloudflare blocks; consume reuses a single `redeem_request_id` as an idempotency key so a retry after refresh no longer burns an extra reset, a per-account mutex removes the check→consume TOCTOU race, a 401 auto-refreshes the token and retries once, and the post-reset wham round-trip is moved to the background to shorten the response path. Successful resets are written to `account_events` audit records.
- **`CODEX_TRUSTED_PROXIES` env to configure trusted reverse proxies.** PR #265 disabled Gin's default trusted proxies but stripped the real client IP behind reverse proxies; Codex2API now trusts loopback and common private networks by default for same-host/Docker WAF deployments, and operators can tighten or disable this via `CODEX_TRUSTED_PROXIES` (comma/space-separated IP/CIDR list, or `none` to disable). Invalid entries fail fast at startup.
- **Localized upstream reset error codes.** When an active reset is rejected by upstream (e.g. `rate_limit_not_resettable` on business/credits-only plans), known codes are translated to a Chinese explanation with the original upstream JSON appended; unknown codes fall back to the raw upstream text, and empty bodies fall back to the status code.
- **1h metrics sampling granularity.** The 1h time range now samples at 1-minute granularity across 60 buckets.
- **Account card layout refinements.**

### Fixes

- **Discard broken upstream WS connections instead of reusing them.** After an upstream WS error (close 1006/1009/1011, broken pipe, unexpected EOF), `WsResponse.Close()` only removed the pending entry and returned the connection to the pool without closing the underlying socket, so the broken connection was misjudged reusable (a CLOSE_WAIT probe Ping still succeeds, a false positive) — causing slow first tokens / dropped streams and leaking fds in CLOSE_WAIT. Read errors now mark the connection broken, and `Close()` distinguishes normal completion (release for reuse) from abnormal termination (discard: close the socket and remove it from the pool via `CompareAndDelete`). Refs #267.
- **Refresh reset credits independent of usage freshness.** The reset-credits count is only refreshed by the wham/usage probe, but probing was gated on usage-snapshot freshness; since normal `/responses` traffic bumps that snapshot via response headers (which don't carry reset credits), busy accounts looked fresh and never got a wham probe. Reset-credit probe time is now tracked separately, and a zero-cost wham-only probe is allowed during rate-limited cooldown and premium 5h limits.
- **Disabled Gin's default trusted proxies (#265).**
- **Codex invite account dropdown only showed one account.** The account combobox pre-filled and filtered by the selected account's email, so opening it matched only that account; it now shows all eligible Codex OAuth accounts when opened and filters by text only while the user is actively typing.

## v2.3.4 - 2026-06-16

### Features

- **Account health visualization and "personal mode".** The account list/cards gain a "health status" bar (time-bucketed coloring based on recent request success/failure), the dashboard adds a system-level health overview between the top cards and usage stats, and the time-range selector is promoted to a page-level shared control. The accounts page adds a "pool mode / personal mode" toggle — personal mode renders accounts as richer two-column cards (avatar, equal-height alignment, icon+text action area), with the initial mode auto-selected by pool size on first upgrade.
- **Active reset credits for Codex (#249).** The account usage dialog, list badges, desktop table, and card action areas can all view the remaining "active reset count" and reset it in one click (with confirmation, refreshing after consuming 1), reusing the existing wham upstream path instead of the generic passthrough.
- **Prompt-filter review key (#257).** After a local rule match, an optional review model (OpenAI/Anthropic-compatible moderation) can re-check the request to reduce false positives; supports base_url / model / timeout / fail-closed configuration.
- **Blocked requests record full content and are queryable (#259).** Blocked requests now record the full request content in "prompt filter → logs" (no longer just a 500-char preview, truncated at 32K) and are included in search; in usage stats, cyber_policy errors can be clicked to view the full triggering request content.
- **Local token-counting compatibility endpoints (#238).** Added `POST /v1/messages/count_tokens` and `POST /v1/responses/input_tokens`, which return a local `input_tokens` estimate without forwarding upstream or consuming quota, so clients like LiteLLM no longer get a 404 when probing token-counting endpoints.
- **Codex referral invite feature (#260).** Added a Codex referral invite UI and batch invite capability, plus an improved account picker (#264).
- **Deduplicate accounts by OAuth identity (#262).** Imported/added accounts are deduplicated by OAuth identity to avoid the same account entering the pool twice.
- **Account bulk-update API (#263).** Added a bulk account update endpoint supporting batch scheduling and other config changes.
- **Edit OAuth account auth config and re-authorize UI (#250).** Supports editing an existing OAuth account's auth config and re-authorizing from the frontend.
- **External async image task API (#254).** Added an external async image-generation task API.
- **Backend streaming forwarding performance (#252).** Optimized the streaming forwarding path performance.

### Fixes

- **Streaming cyber_policy penalties not recorded (#258).** cyber_policy bans are delivered as `response.failed` (HTTP 200) in Codex streaming responses, but previously only non-2xx errors were written to `prompt_filter_logs`, so streaming-path cyber_policy was invisible in the prompt-filter logs; recording is now added across the 4 streaming failure paths.
- **Failed image request usage not counted (#239).** Fixed error image-request records not showing in usage stats.
- **`/v1/messages` cached-token double billing (#253).** Anthropic usage now deducts cache-hit tokens to avoid double-billing the cached portion.
- **Claude Code tool argument sanitization correction (#251).** Fixed compatibility issues caused by overly broad tool argument sanitization.
- **Upgraded vite to fix npm audit high severity (#261).** Frontend dependency upgrade fixing an npm audit high vulnerability.

## v2.3.3 - 2026-06-13

### Features

- **Cloud storage links for image generation (#240).** When S3-compatible cloud storage is configured and a client requests `response_format=url`, `/v1/images/generations` now uploads each generated image to object storage and returns a time-limited (1h) presigned link instead of a base64 data URL, falling back to the data URL on any upload or signing failure so the API never hard-fails on storage misconfig. Uploaded images are registered into the admin gallery via a lazily created synthetic job + asset record, so they appear in gallery/history and are cleaned up (DB row + backing object) on delete.
- **API key token-limit unit selector (#234).** The API key 5h/7d token limit fields now offer a unit selector (token / K / M / B) so large quotas can be entered and displayed in readable units instead of raw token counts.

### Fixes

- **Request forwarding error handling and success accounting (#246).** Hardened the OpenAI Responses `ttftGuard` call site with explicit nil-safe wrapping, handled OpenAI/Codex compact response-body read failures while preserving the final 502, synced usage headers on Codex compact read failure and reported request success on the happy path, and rebuilt the Anthropic stream error SSE payload via `json.Marshal`.
- **WebSocket relay stability (#247).** Improved `wsrelay` executor, manager, and session handling to keep upstream WebSocket relay connections stable.
- **Function `tool_choice` shape normalized.** A `tool_choice` object missing `type` but carrying a `function` object or top-level `name` is now normalized to `type: "function"` with a flattened `name`, matching OpenAI SDK convention so the upstream no longer rejects the request.
- **Missing encrypted reasoning content (#235).** `missing_required_parameter` errors on a `*.encrypted_content` param are now recognized alongside `invalid_encrypted_content`, so requests with absent encrypted reasoning content are retried instead of failing.
- **5h usage freshness split from 7d (#241).** The 5h usage snapshot now persists its own `codex_5h_usage_updated_at` timestamp instead of overwriting the shared `codex_usage_updated_at`, so 5h and 7d freshness no longer clobber each other.
- **Usage logs page size up to 500 (#244).** The usage logs endpoint now accepts a page size of 500.
- **Read pages sanitizer narrowed (#245).** Tightened the Read pages sanitizer scope.

### Features

- **Group filter: ungrouped and exclude modes (#229).** The account group filter now supports an "ungrouped" quick option and per-group tri-state filtering (only / hide / off), so accounts outside chosen groups can be isolated, selected in bulk, and assigned to a group quickly. The groups column is also shown by default in the table view.

### Fixes

- **`gpt-5.3-codex-spark` no longer receives the default image tool (#230).** The Responses translator auto-injected the hosted `image_generation` tool (plus bridge instructions) into ordinary text requests; spark rejects hosted image tools, so text-only spark requests failed on the HTTP upstream path. The default injection is now skipped for spark while explicitly supplied image tools are still honored.
- **Tools without a `type` field no longer fail requests (#219).** Tool definitions missing `type` (or with `type: null`) were forwarded verbatim and the upstream rejected the whole request with 400 `Unsupported tool type: None`. Function-shaped tools (a nested `function` object or a top-level `name`) are now treated as `type: "function"` per OpenAI SDK convention on both the Chat Completions and Responses paths; unrecognizable typeless tools are dropped instead of failing the request.
- **Usage logs are requeued when a flush fails (#233).** Failed usage-log batches are put back at the front of the buffer and retried on the next flush instead of being dropped, and usage-log inserts plus API key quota updates now run in a single transaction on both SQLite and PostgreSQL.
- **Reduced timer allocations while waiting for Redis token refresh (#231).** `WaitForRefreshComplete` now reuses a single ticker and timeout timer instead of allocating a new timer every 200ms poll (previously up to ~150 timers per 30s wait).

### Performance

- **Request bodies are read once and reused (#232).** `RequestSizeLimiter` caches the body it already reads, and the body-cache middleware plus all JSON proxy handlers reuse it, cutting up to ~2/3 of duplicated request-body buffer allocations on hot paths.

## v2.3.1 - 2026-06-11

### Features

- **Group-level and global auto-pause thresholds (#227).** Usage auto-pause thresholds can now be set globally (Settings) and per account group (group dialog), resolved with account > group > global priority. Account-level settings win when present, otherwise the smallest non-zero group threshold applies, falling back to the global default. Changes take effect immediately without a restart, and out-of-range values are rejected consistently across all three levels.
- **API key concurrency limits (#226).** Each API key can cap its concurrent in-flight requests via `limits.max_concurrency`. Requests over the cap receive 429 with a descriptive message across all proxy entrances (responses, compact, chat completions, Anthropic messages, images, WebSocket turns). Unset or 0 means unlimited.
- **Account group multi-select binding (#217, #222).** Account group assignment now uses a searchable multi-select dropdown instead of assigning new groups to all accounts.

### Fixes

- **WebSocket prompt cache restored (#202).** Since v2.2.7, requests without an explicit session identifier got a random per-request `stateless-` ID on the WS upstream path, which leaked into `prompt_cache_key` and the WS handshake session headers — so the upstream prompt cache never hit in WS mode (HTTP was unaffected). WS now injects a deterministic cache key derived from the downstream API key (matching HTTP behavior), restoring cache hits (~86% of input tokens on repeated prefixes in live testing).
- **WebSocket connection reuse under high RPM.** Sessionless requests previously opened a new WS connection per request; sustained load (~200 RPM) triggered upstream handshake throttling (`bad handshake` → 503). Such requests now reuse warm connections from a per-(account, cache key) slot pool (8 slots, falling back to one-shot connections when all slots are busy). Live testing at 200 RPM went from 128/200 success to 200/200 with zero handshake failures and roughly halved latency.
- **Codex CLI compact v2 input items (#224).** `/responses` no longer returns 400 `Invalid input type 'compaction_trigger'` for Codex CLI compact v2 payloads.
- **Usage probed immediately after OAuth account add.** Newly added OAuth accounts get an immediate usage probe instead of waiting for the next cycle.
- **Account table scrolling.** Adjusted account table scrolling behavior in the admin UI.

## v2.3.0 - 2026-06-10

### Features

- **Account recycle bin.** Soft-deleted accounts now land in a recycle bin instead of vanishing. Accounts can be restored to the runtime pool without a restart, test-connected before restoring (429/usage-limit results are recorded as rate-limited rather than failed), batch-tested with an optional auto-restore-on-pass toggle, filtered by plan type and searched with pagination, and permanently purged behind a typed confirmation enforced on the API as well.
- **Client IP in usage logs.** Usage records now capture the requesting client IP and display it in the usage views.

### Fixes

- **Relay accounts on `/v1/messages` and `/v1/chat/completions` (#181).** Both endpoints now schedule OpenAI Responses (relay API-key) accounts and forward over HTTP like `/v1/responses`, instead of returning 503 "No available accounts" once Codex OAuth accounts were rate-limited on relay-only deployments.
- **Image generation no longer routed over WebSocket (#220).** Requests carrying an `image_generation` tool (including the auto-injected one) are forced onto HTTP, and requests that do go over WS get the auto-injected image tool, its `tool_choice`, and bridge instructions stripped, so image generation can no longer stall WS streams on large base64 payloads.
- **Image SSE keep-alive.** Image generation SSE streams now stay alive through long upstream pauses, preventing idle disconnects mid-generation.
- **Account rate-limit state sync.** Rate-limit state observed by usage probes and WHAM checks is synced back to runtime account state so scheduling and status displays stay consistent.
- **Account status summary counts.** Admin account status summary counters now align with the account list filters.

### Security

- **Code-scanning hardening.** Local image storage rejects keys containing path separators on write and `..` traversal segments on read/delete (legacy relative refs keep working); remote-migration and sub2api import URLs must be complete http/https URLs with a host; SVG logo minification strips comments to a fixed point so `<!--` cannot survive sanitization.

## v2.2.9 - 2026-06-09

### Features

- **Account proxy validation (#212).** Added proxy test controls across account add/edit, OAuth generation, and OpenAI Responses account forms, backed by an admin proxy test endpoint that reports reachability, latency, IP, and location.
- **Usage range and reset-time display.** Dashboard usage cards now label and query the selected time range instead of always showing "today", and account quota reset labels now display precise seconds with full timestamps available in tooltips.

### Fixes

- **WebSocket message-too-big fallback (#214).** Upstream WebSocket `close 1009` / `message too big` failures before the first downstream token now fall back to HTTP for the same request instead of rotating accounts through another WS attempt, with logs classified as `message_too_big`.
- **CPA/sub2api JSON import (#215).** JSON imports now avoid treating repeated or conflicting ChatGPT account IDs as the same account when credentials differ, fixing failed or collapsed imports from CPA/sub2api export files.
- **Batch account test state updates.** Batch connection tests now update account status consistently with single-account tests, including `response.failed`, unauthorized, generic upstream errors, and usage-limit results; successful tests still recover stale banned/cooldown state.
- **Quota auto-pause scheduling (#216).** Accounts that reached configured usage thresholds remain out of scheduling while their quota window is still active, and stale reset timestamps are ignored so reset/cooldown state does not linger incorrectly.

## v2.2.8 - 2026-06-08

### Features

- **Account usage analytics.** Expanded the account usage details modal with Overview, Details, and Quality views, range-aware totals, richer model distribution controls, and quality signals including error rate, retry count, average first-token latency, P95 response time, streaming ratio, and compact ratio.
- **Batch account import workflow.** Refresh Token batch import now has progress feedback and kicks off usage sampling so newly imported accounts do not require a separate manual batch test before usage data becomes visible.
- **First-token mode setting (#207).** Added a system setting for strict vs loose first-token detection, making TTFT behavior configurable while keeping thinking/pre-output events from being counted incorrectly in strict mode.
- **Usage visibility controls (#203, #209, #210).** Usage stats now support additional time ranges, full token-number display, compact request markers, and dedicated WS/Fast request badges.

### Fixes

- **Batch account test consistency (#194).** Batch tests now match single-account tests more closely by using the WHAM usage preflight and parsing real SSE terminal events from a short request; `response.failed`, missing output, and `usage_limit_reached` are no longer treated as successful account recovery.
- **Forced WebSocket usage logs (#205, #210).** Requests forced through upstream WebSocket now persist and display the `via_websocket` marker correctly, with Fast tier shown as its own request badge.
- **Responses compact and quota handling (#174, #201).** OpenAI Responses API-key accounts can forward `/v1/responses/compact`, and nested `usage_limit_reached` errors are handled consistently for cooldown and sampling.
- **Billing tier policy persistence (#206).** `billing_tier_policy` now stays aligned across frontend save behavior, backend normalization, and database persistence.
- **OAuth authorization UX.** OAuth authorization links can be regenerated and copied reliably, including HTTP admin deployments where the Clipboard API may be unavailable.
- **Image and WS routing stability (#198).** Explicit image generation requests bypass forced WS routing, and `gpt-5.3-codex-spark` scheduling/model filtering is hardened.

## v2.2.7 - 2026-06-05

### Features

- **WebSocket silent retry controls (#195).** Added Codex upstream WS settings for hiding upstream errors, silently retrying pre-first-token upstream failures, and capping silent retries (`codex_ws_hide_upstream_errors`, `codex_ws_silent_retry_enabled`, `codex_ws_silent_max_retries`). These settings are available in the admin UI and persist through both Postgres and SQLite.
- **WHAM-only usage probe controls.** Added runtime/admin controls so usage probes can rely on the zero-cost WHAM endpoint without falling back to `/responses` probes when that is preferred.

### Fixes

- **WS upstream failure handling (#195).** Retryable upstream WS failures before the first token, including usage-limit, 429, 5xx, read errors, timeouts, and EOFs, now stay server-side while codex2api switches accounts and rebuilds the upstream WS connection. If retries are exhausted, clients receive a unified friendly message while the original upstream error remains in backend logs and usage records.
- **Responses routing and model hardening (#198).** Hardened `/v1/responses` routing for recent Codex models including `gpt-5.3-codex-spark`, widened local plan gating so stale local `plan_type` records do not incorrectly block real upstream calls, and improved WS/TTFT handling around response payload content.
- **OpenAI Responses compact routing.** OpenAI Responses API accounts added with `base_url` + `api_key` can now use `/v1/responses/compact`; compact bodies are normalized for that upstream path instead of being sent through the ChatGPT-only compact route.
- **Usage-limit detection.** `usage_limit_reached` is now recognized even when wrapped inside `response.error`, `response.status_details`, or upstream 5xx-shaped payloads, so exhausted accounts are treated as quota-limited consistently.
- **Accounts toolbar wrapping.** Improved the admin accounts toolbar layout so search/filter/action controls wrap cleanly on narrower viewports.
- **Security scan recovery.** Updated React Router to a patched release and raised the Go toolchain directive to `1.26.4`, clearing the failing frontend npm audit and backend govulncheck jobs.

## v2.2.6 - 2026-06-03

### Features

- **Codex upstream WebSocket mode.** Added an opt-in `codex_force_websocket` system setting that routes Codex upstream traffic over a persistent WebSocket long-connection (reusing the `wsrelay` connection pool), reducing per-request TLS/handshake overhead to better match the official CLI. Disabled by default; when off, requests keep using the existing HTTP path. A downstream `https://.../v1/responses` (or `/v1/chat/completions`) HTTP POST is transparently forwarded as a `wss://` upstream connection.
- **Idle WS keepalive.** Added `codex_ws_keepalive_enabled` and `codex_ws_keepalive_interval_sec` (default 60s) to keep idle upstream WebSocket connections alive with background Ping frames. Keepalive never opens new connections and never sends business frames, so it consumes zero account quota. Disabled by default.
- **WS request badge.** The Usage log now shows a `ws` badge between the status code and model for requests that went over WebSocket.
- **Dedicated WebSocket settings card.** WS-related settings are split out of the crowded scheduling card into their own full-width "WebSocket (Codex Upstream)" card in the admin UI.
- **Bundled Codex CLI bump.** Updated the bundled Codex CLI version from `0.128.0` to `0.136.0` for a more faithful fingerprint and access to newer models.

### Fixes

- **WS upstream error passthrough.** Upstream WebSocket error frames are now relayed to the client as a `response.failed` SSE event preserving the original error, instead of being turned into a low-level read error that surfaced as a mysterious empty response / 32-90s hang. Any upstream error (rate limit, unsupported model, invalid parameter) is now visible to the client.
- **WS transport robustness.** Fixed the `wsrelay` dialer copy dropping `NetDialContext` (TCP KeepAlive never took effect); added 64KB read/write buffers with a shared write-buffer pool for large 48-91KB upstream frames; replaced the fixed 10ms busy-connection spin with exponential backoff plus a max-wait timeout; raised the read timeout from 60s to 120s so long-reasoning turns are not falsely disconnected; and log a warning when a WS path silently falls back to HTTP.
- **TLS session resumption.** Standard and uTLS transports now share a `ClientSessionCache`, so reconnects use TLS resumption (1-RTT) to cut cold-connection cost. Bumped `MaxIdleConnsPerHost` and `IdleConnTimeout`, and switched the HTTP server to explicit `ReadHeaderTimeout`/`IdleTimeout` with graceful shutdown (keeping `WriteTimeout` at 0 so streaming responses are never cut off).
- **Account lookup O(N) → O(1).** The store now keeps a `DBID → account` index, so session-affinity hot paths and all `FindByID` lookups no longer scan the account slice linearly.

## v2.2.5 - 2026-06-02

### Features

- **Codex model registry and reasoning models (#165).** Added system-setting controls for upstream-synced base models and reasoning-effort variants so model list entries can be configured from the admin UI.
- **Codex model redirect mapping (#189, #190).** Added a dedicated Codex model redirect map that can route downstream model names to another Codex model while preserving request reasoning effort.
- **Account quota auto-pause controls.** Added 5h/7d usage-threshold auto-pause controls with global defaults, per-account disables, edit-account controls, and batch editing.
- **Compact request visibility.** Usage logs now detect compact requests and show a compact badge alongside request model/reasoning information.
- **Email domain account tags (#191).** Account management now derives email domains, supports domain filtering with banned/total stats, searches by domain, and lets users show or hide domain badges.
- **Configuration examples.** Updated Codex config examples to cover the current model mapping and reasoning-effort setup.

### Fixes

- **Service-tier billing semantics (#183).** Split requested, actual, and billing service-tier handling so billing policy stays explicit when upstream reports a different actual tier.
- **SQLite and admin billing stability (#185, #186).** Stabilized account billing window reads and SQLite access paths so large account pools do not turn simple admin/API-key queries into transient 503s.
- **Account status and probe handling.** Unauthorized account errors are recorded more consistently, and batch account rendering has less unnecessary work at scale.
- **Refresh token reuse handling.** Reused refresh tokens are now treated as non-retryable credential errors instead of entering avoidable retry paths.

## v2.2.4 - 2026-05-28

### Features

- **Scheduler warm-tier bypass (#176).** Added a scheduler option to skip warm-tier demotion so selected accounts can stay in the healthy scheduling lane.
- **API Reference image previews.** The Try it panel now renders image responses directly when `/v1/images/generations` or `/v1/images/edits` returns `b64_json`, image URLs, or Responses-style image output, while still keeping the raw JSON visible.
- **Long-context billing details (#178).** Usage cost tooltips now expose when long-context pricing is active (`input_tokens > 272,000`) and show the actual input, output, and cache-read unit prices used for the request.
- **Root changelog.** Moved the project changelog to the repository root so release notes and documentation links point to a stable location.

### Fixes

- **WebSocket response continuity (#182).** Fixed WS mode context loss so `previous_response_id` continuation remains connected across `/v1/responses` WebSocket turns.
- **First-token timeout retries (#172).** First-token timeouts now retry by account-pool round and clear transient unavailable markers after the pool has been tried, reducing false "no available account" failures for small pools.
- **Banned-account test recovery.** Successful account tests now recover stale banned/error state instead of leaving the account marked unhealthy after the probe succeeds.
- **Bounded batch account tests.** Batch account tests now keep their execution bounded so duplicate or repeated test requests do not destabilize account state handling.
- **Version popover clipping.** The version popover is rendered through a portal so sidebar overflow no longer clips the latest-version panel.

## v2.2.3 - 2026-05-27

### Features

- **Usage reset radar.** New `/subscriptions` page consolidates the Codex Reset Radar summary, recent RSS events, and a reset-time hook. When a window-close signal is detected the backend clears stale cooldown/usage caches and re-tests every account so the pool reflects the new reset boundary immediately.
- **Streaming batch operations in admin.** Account batch refresh/test/enable/disable/lock/reset now stream per-account progress events (`success/banned/rate_limited/failed`) to the admin UI instead of waiting for the full operation to complete.
- **Compact usage number setting.** Added a system setting to render token counts with K/M units in the usage table for easier reading at scale.
- **Card view for account management.** Desktop accounts page gains a table/card view toggle (up to 5 cards per row on `xl` screens). Choice is persisted in `localStorage`.
- **Status badge error tooltip.** Hovering an `unauthorized` or `error` status badge now surfaces the full upstream error message in a popover, matching the usage log status code tooltip style.
- **Anthropic `speed: fast` forwarding (#170).** Anthropic-style `speed: fast` requests now map to the Codex priority tier upstream so fast clients get fast tokens end-to-end.

### Fixes

- **Version popover always clickable.** The sidebar version badge now opens the popover even when the GitHub latest-version lookup is still pending or blocked; a "checking…" hint is shown until the remote tag arrives.
- **First-token timeout and scheduler races.** Hardened the proxy path so first-token timeouts and concurrent scheduler races no longer collapse into a spurious "no available account" 503.
- **`/responses` WebSocket ingress.** WebSocket clients hitting `ws://host/v1/responses` are now accepted; the prior 404/101 misclassification has been fixed. Setting `CODEX_UPSTREAM_TRANSPORT=ws` no longer reports the connect handshake as an unknown error.
- **Anthropic content preservation + deactivated probe flagging.** Anthropic-shaped responses keep their original content blocks; deactivated accounts are clearly flagged in probe state instead of silently appearing healthy.
- **Wham window classification.** Usage window classification now uses `limit_window_seconds` rather than field position, so free-tier accounts no longer have a 7d window misclassified as 5h.

## v2.2.2 - 2026-05-26

### Features

- **First-run setup and admin auth polish.** Added setup guidance for unconfigured deployments, improved the admin authentication flow, and added a frontend logout path.
- **Runtime status API and page.** Added machine-readable runtime checks for service, database, cache, usage log writer, probes, account pool, image storage, and admin auth.
- **Background media customization.** Added configurable background image/video support, realtime glass opacity/blur controls, and raised MP4 dynamic wallpaper uploads to 40MB while keeping image uploads capped at 20MB.
- **Quick-start configuration options.** Added fast-mode and reasoning-effort snippets for supported client templates in the usage docs.
- **Issue templates.** Added structured Chinese and English GitHub issue templates for bugs, ideas, UI feedback, deployment help, and questions.

### Fixes

- **Request body sizing for wallpapers.** Raised the default request body limit to 48MB so 40MB MP4 background uploads can pass multipart overhead safely.

# Changelog — iteration/may-2026-v2

Dates: 2026-05-13 to 2026-05-20. 17 commits.

## Features

- **Credit quota support (#141).** Added `credit_enabled` and `credit_skip_usage_window` flags to the accounts table. Credit-marked accounts skip usage-window penalties in the scheduler. Managed via `PATCH /api/admin/accounts/:id/credit`.

- **Scheduler mode (#133).** Added `scheduler_mode` system setting with two modes: `round_robin` (default, weighted by dispatch score) and `remaining_quota` (prioritize accounts with lowest usage percent). Configurable from Admin Settings page.

- **5h/7d windowed USD cost display.** Replaced the single total-cost column with a windowed billing view. Each account now shows `billed_5h` and `billed_7d` fields aligned with the account's usage-reset boundaries. This reflects actual spending, not estimated token costs.

- **Image-to-image in Image Studio (#135, #136).** The admin Image Studio now supports image-to-image generation via `POST /api/admin/images/edit-jobs`, accepting reference image URLs or data URIs. Added text-to-image and image-to-image tabs in the frontend.

- **Billing model expansion.** Added pricing for gpt-5.5-pro and gpt-5.4-pro families. Implemented long context (>272K tokens) premium pricing for gpt-5.5, gpt-5.5-pro, gpt-5.4, gpt-5.4-pro with automatic detection. Fixed gpt-4o and gpt-4o-mini cache-read pricing.

## Fixes

- **GPT-5.5 pricing corrected.** Updated standard-tier billing from old values to $5.00/M input / $30.00/M output (priority: $12.50/M / $75.00/M), matching current official pricing.

- **SSE stream isolation.** Prevented SSE response mixing when retrying across accounts, using `c.Writer.Written()` as the retry guard instead of a package-level flag.

- **Usage logging for image errors.** Added usage-log emits for read-error paths in image generation, ensuring billing records are not silently dropped on stream failures.

- **Model mapping initialization.** Restored `modelMapping` init that was accidentally removed during the scheduler_mode refactor.

- **Credit field Scan order.** Fixed PostgreSQL `Scan` argument ordering for credit fields that was causing silent zero-values.

- **Round 2 review fixes.** Addressed Haiku review findings including api.ts syntax cleanup, billing test corrections, and several CRITICAL/HIGH issues from automated review.

## Security

- **SQLite default binds to localhost.** SQLite compose files (`docker-compose.sqlite.yml`, `docker-compose.sqlite.local.yml`) now bind ports to `127.0.0.1` by default. Previously they bound to `0.0.0.0`, exposing the service on all interfaces. Standard (PostgreSQL) compose files retain the `0.0.0.0` default.

- **BIND_HOST env var.** Added `BIND_HOST` environment variable support to control the HTTP listen address across all deployment modes. Documented in `.env.example`, `.env.sqlite.example`, and `CONFIGURATION.md`.

## Breaking Changes

- **SQLite compose port binding.** SQLite deployments upgrading from a previous version that relied on external access via the default compose configuration must now explicitly set `BIND_HOST=0.0.0.0` in `.env` or override the port binding in the compose file. All other behavior remains backwards-compatible.
