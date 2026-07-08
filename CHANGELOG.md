# Changelog

## v2.4.6 - 2026-07-08

### Features

- **Configurable retry interval and sticky same-account retry for transport errors (#331).** Two new system settings refine retry behavior; defaults preserve the old behavior exactly. `retry_interval_ms` (0–30000, default 0 = immediate) inserts a wait between retry attempts — covering transport errors, upstream non-200 retries, pre-first-token stream-break transparent retries, and WS silent retries across all proxy paths — and aborts immediately if the client disconnects mid-wait (the account's concurrency slot is released before sleeping). First-token-timeout retries skip the extra wait since they already burned a full timeout. `transport_retry_policy` (`rotate` default / `sticky`) changes what happens on connection-level failures (network blips, proxy node switches): in sticky mode the request retries on the *same* account — no hard exclusion, session affinity kept, and no `ReportRequestFailure`, so a local network hiccup no longer burns through the whole account pool while marking every account unhealthy. Upstream HTTP status errors (5xx/401/429) still rotate, since those are per-account verdicts. Both settings live next to "max retries" in the admin settings page.

### Fixes

- **Streaming requests that fail before the first token now return real HTTP errors instead of 200 (#318, reported and first-fixed by @JrCx7scC).** When the upstream terminated a stream with `response.failed` (e.g. `context_length_exceeded`) before any token, codex2api wrapped the failure into a 200 SSE stream (plus `[DONE]` on the chat path), so billing middle layers treating codex2api as an upstream saw a normal-looking success with no usage and billed their users by *estimated input tokens* — "upstream rejected with 0 output, user billed for the full input". The original PR's direction was right but didn't survive the wire: the post-loop flush committed the 200 header before the error JSON could be sent (gin's `Flush()` calls `WriteHeaderNow()` even on an empty buffer), so the fix additionally guards the epilogue flush on "wrote any body", overrides the pre-set SSE Content-Type, and gates the error return on an explicit abort flag. Applied across Responses/ChatCompletions streaming, the OpenAI Responses direct branch, and inbound WebSocket — where there is no HTTP status to fix, so non-retryable pre-first-token failures now send a structured `error` frame and close with a semantic close code (429→1013, other 4xx→1008, 5xx→1011) instead of relaying the raw failure frame and ending the turn as if it succeeded. Retryable failures keep the existing silent-retry/relay contract.
- **wham 401 no longer unilaterally bans an account — the `/responses` probe is the final arbiter (#328).** codex_at (bare AT import) accounts can be permanently 401 on the wham endpoint (token missing a workspace claim) while real `/responses` traffic works fine; the usage probe treated that 401 as dead credentials and put the account into a 24h unauthorized cooldown — re-banning it after every manual reset, or flapping banned/unbanned every probe round when fallback probing was on. A wham 401 is now a sentinel: no failure report, no ban. When fallback is available the `/responses` probe delivers the auth verdict (200 recovers / 401 bans); in wham-only mode the 401 is just logged, leaving genuinely dead tokens to be cooled down by real-traffic 401s.
- **Duplicate-account merge race could soft-delete both accounts.** After a merge, the async probe reload on the surviving account could trigger a reverse merge before the losing row was soft-deleted — the reverse pass merged old→new, then the outer pass soft-deleted the new row too, losing the account entirely (reproduced deterministically in CI where the fake probe returns instantly). Merges are now serialized under a mutex and the losing row is soft-deleted before the probe reload, so concurrent or recursive merges always see each other's results.
- **Online update hardening (#327, #329).** After an in-place update the restart re-exec could still launch the *old* binary — `os.Executable()` resolves `/proc/self/exe`, which after the atomic swap points at the deleted old inode; restart now execs the explicit new binary path. The frontend no longer caches the whole update-info blob (a stale `has_update=true` survived the update and kept showing "new version available" when current == latest); only the remote release fields are cached and the comparison is recomputed against the running bundle version on every check. The update/restart UI also gains client-side timeouts and a "refresh manually" hint when the restarted process doesn't answer within 90s.
- **Usage stats "source account" column shows email again.** A v2.4.5 frontend change made the account *name* take precedence, so unnamed AT-imported accounts all rendered as their placeholder name (`at-account-1`) instead of their email identity; the order is restored to email → name → ID on both the usage and operations views.

### Security

- **Bumped `quic-go` to v0.59.1 and `golang.org/x/crypto` to v0.52.0.** The quic-go bump fixes GO-2026-5676 (HTTP/3 QPACK trailer-expansion memory exhaustion), flagged by govulncheck as reachable through the actual call path; the x/crypto bump clears a Dependabot alert.

## v2.4.5 - 2026-07-07

### Features

- **Per-account model mapping for OpenAI Responses accounts (#325).** Each OpenAI Responses (relay API-key) account can now carry its own model alias map, so different relay channels can expose different model names while routing to the same upstream — e.g. mapping a channel-specific alias onto the upstream's real model name per account, on top of the existing global model mapping. Follow-up hardening: mapping keys and values both run through `ValidateModelName` (length + charset) before being accepted, since mapped source aliases flow into `/v1/models` responses, model validation, and usage logs — arbitrary strings are rejected instead of injected.
- **Connection-test content supports multi-line random pick and variable templates (#320).** Building on #317's configurable single test sentence, the test content setting now accepts multiple lines: each probe randomly picks one non-empty line, and `{{...}}` placeholders in the picked line are expanded before sending — `{{time}}`/`{{date}}`/`{{datetime}}`/`{{timestamp}}`/`{{rand}}`/`{{rand:min-max}}` (min>max auto-swaps; unknown variables are kept verbatim). This removes the "every account in a deployment sends the same probe sentence" fingerprint. Single-line configs behave exactly as before; all three test entrances plus the usage probe share the same rendering.
- **Reset-credit expiry details in the account usage dialog (#322).** The "manual quota reset" section now fetches the per-credit breakdown from the upstream's zero-cost reset-credits list endpoint and shows each credit's expiry time, sorted soonest-first with credits expiring within 7 days highlighted — so you can see not just how many resets are left but when each one becomes worthless, and burn the soonest-expiring ones first. The authoritative available count from the detail endpoint also recalibrates the locally cached number. Detail fetch failures degrade gracefully: the count and the reset button keep working, only the expiry list shows an "unavailable" hint.
- **Content-derived session seeds: multiple users behind one shared API key now stick to accounts per-conversation.** When a single API key is used as a channel for a whole downstream site, sessionless requests all collapsed onto the same key-derived affinity key — every user squeezed onto one upstream account and account switching was all-or-nothing per key. A content-derived seed (sha256 over conversation-stable fields: model + instructions + tools + system/developer + first user message) now slots in between explicit session signals and the per-key fallback, so each conversation converges onto its own sticky account and the pool load spreads out. Requests continuing via `previous_response_id` skip the seed (their input changes every turn and would drift), the seed only participates in local routing (never sent upstream in the default isolation mode), and the session-binding table gains a 65536-entry soft cap with expired-entry sweeping.

### Fixes

- **Forced-WS + shared single API key no longer bleeds context across users.** With `codex_force_websocket` on and one API key shared by multiple end users, user 2's turn could get stitched onto user 1's conversation: the reused stateless connection's handshake headers (`Session_id`/`Conversation_id`) were frozen to the *first* request's cache key and never updated, so an upstream that keeps per-connection conversation state saw every later user under the first user's session identity (same family as #268/#308). Stateless reused connections now send *no* session identity in the handshake at all — per-request isolation rides entirely on the per-request `prompt_cache_key` in the frame body; explicit-session and per-api-key modes are unchanged. A `CODEX_WS_STATELESS_ONESHOT=1` escape hatch disables slot reuse entirely (one connection per request, hard isolation, at the cost of per-request handshakes). Continuation affinity is also fixed: `previous_response_id` context lives only inside the connection that produced the response, so continuation requests now return to their original connection (response→connection bindings with TTL and a 4096 cap, guarded by account + API-key ownership + connection-pointer checks) instead of intermittently hitting "previous response not found" on a random slot.
- **`previous_response_id` context cache is isolated per downstream API key.** The response cache was keyed globally by `response_id` and expansion never checked ownership, so any request presenting (guessing, or accidentally reusing) someone else's response id would get that user's full conversation items injected into its input — a cross-key context leak. Cache entries are now namespaced by owner (`key:<id>`/anon) across all three write/expand paths (HTTP streaming + non-streaming, WS-native ingress, translator inline expansion); an owner mismatch is treated as a plain cache miss.
- **DB failures and upstream account 401s are no longer misreported as client 401s (#323).** Two layers of "server/account-side trouble" surfaced to clients as "your API key is invalid": (1) API-key resolution collapsed "key not found" and "DB failure" (connection exhaustion, timeouts) into the same result, so under load a PG "too many clients" turned into 401 `invalid_api_key` — resolution is now three-state, real DB errors return 503 and skip the `AUTH_FAILED` audit log so credential-attack alerting stays clean; (2) an upstream account's revoked OAuth token, after retries were exhausted, passed through as a raw 401 — non-`missing_scope` upstream 401s are now rewritten to a 503 pool-level error (`account_pool_unauthorized`), clearly distinct from client auth failure. Account-side handling was already correct (401 accounts get banned + cooled down); this fixes only the status-code leak to clients.
- **Free/team monthly-window accounts label their long usage window "30d" instead of "7d" (#324).** v2.4.4 taught the backend to recognize monthly (~30 day) rate-limit windows, but the frontend still hard-coded the long-window label as "7d" — misleading for free and team accounts whose long window really is a month. The account list usage bars, billed column, and scheduler board badges now render the real window length (`30d` for monthly, actual day count for other lengths, `7d` fallback when unknown). Also fixes the 7d billed-cost window start, which was hard-coded to `reset − 7 days`: for a monthly window that start date landed in the future, so `billed_7d` was stuck at 0.

## v2.4.4 - 2026-07-05

### Features

- **One-click online update from the admin console (#319).** The version popover's "new version" hint gains an "Update now" button: the backend (`GET`/`POST /api/admin/system/update`) picks the release asset matching the running OS/arch, downloads it from GitHub only (https allowlist re-validated on every redirect hop, 200MB cap), verifies SHA256 via the release asset digest or `SHA256SUMS.txt`, atomically swaps the binary (keeping a `.backup` for manual rollback, restoring the old binary if the swap fails mid-way), and re-execs in place on Unix; the frontend then polls until the new version answers and reloads. Dev/non-semver builds and Windows are marked unsupported instead of pretending to update, and release/Docker builds now embed the version via ldflags so the backend reports it accurately. Follow-up: container environments (docker/podman/k8s, detected via `/.dockerenv`, `/run/.containerenv`, `/proc/1/cgroup`) get an explicit warning that an in-container update is reverted when the container is recreated and image pull is the right upgrade path — the update itself stays available for temporary hot-fixes.
- **Configurable connection-test content (#317).** The prompt sent by single/batch/recycle-bin connection tests and the usage/plan-sync fallback probes was hard-coded to `"hi"` — an easily fingerprintable traffic signature shared by every codex2api deployment. It is now a system setting (Settings → test content, auto-saved, defaulting to `"hi"`), applied uniformly across all five test/probe paths.
- **Per-account custom upstream headers (#316).** Each account can carry a set of custom headers (create/edit/import/scheduler flows all supported) that are applied to outgoing upstream HTTP and WebSocket requests after the standard headers, so they can also override them — the primary use case being `Chatgpt-Account-Id` to pin a multi-workspace team login onto a specific workspace. Header names/values are validated (RFC token charset, no CR/LF, 64 headers / 8192-char values max) and stored in the existing credentials JSON (no migration); accounts without custom headers behave exactly as before. Follow-up fix: the wham usage/reset-credit probes and Codex invites now use the *effective* workspace id (custom-header override first) instead of the OAuth-decoded one, so when a header reroutes traffic to another workspace, the quota bars, auto-pause, and smart pacing measure the workspace the traffic actually lands on; and wham identity write-back is skipped for overridden accounts so the override can't pollute the OAuth dedup identity.

### Fixes

- **Team monthly (30-day) windows are no longer misclassified as 7d — smart pacing stops over-throttling team accounts.** The usage-window classifier only had 5h/7d slots, but a team plan's second rate-limit window is actually monthly (~2592000s), and it was being stuffed into the 7d slot. Smart pacing computed its natural burn rate against a hard-coded 7 days, making the allowed pace ~4.3× too strict for monthly windows and needlessly throttling team accounts. Windows within a 28–31 day tolerance are now recognized as monthly (`IsMonthlyWindowSeconds` as the single source of truth, on both the wham and passive-header paths), the real window length feeds the pacing math, and the account list labels the window "30d" instead of "7d". Known limit: the window length lives in memory only, so between a restart and the first probe (seconds) pacing falls back to the 7d default.
- **Rate-limit cooldowns and 5h/7d window resets trigger an immediate wham probe when their countdown hits zero.** Usage probes were driven only by the periodic background sweep (default 2 min), so when a cooldown or window reset expired, the account flipped back to "available" instantly but its usage bar kept showing the stale pre-reset value until the next sweep. A boundary timer now stays armed on the earliest upcoming reset/cooldown boundary across all accounts and fires a zero-cost wham probe the moment it passes (with a 2s lag for clock skew), re-arming for the next boundary; cooldown/reset writers wake it lock-safely, and the periodic sweep remains as a fallback.

## v2.4.3 - 2026-07-04

### Features

- **API keys can restrict which account plans they dispatch to (multi-select).** Each API key's advanced limits gain an "Account plans" allowlist: pick one or more of `free` / `plus` / `pro` / `prolite` / `team` / `k12` / `go`, and the key will only ever be scheduled onto accounts whose plan matches one of them (e.g. a key limited to `plus` + `team` never touches free-tier accounts). Empty means no plan restriction. The filter is applied at the account-selection stage — the same layer as the existing group allowlist, with which it combines using AND (an account must satisfy both the plan and group constraints when both are set) — rather than rejecting the request after a pick, so a plan-restricted key simply behaves as if only the matching accounts exist. Matching is by raw lowercased `plan_type`, identical to the Accounts page plan filter, so `pro` and `prolite` stay distinct. Stored in the existing `limits` JSON column (no database migration), and because a plan-only restriction now counts as an access constraint, such keys always resolve their full metadata from the database and stay in sync on each request. The scheduler is only rebuilt when the allowlist actually changes, so the auth hot path isn't disturbed.

## v2.4.2 - 2026-07-04

### Features

- **Import accounts by pasting a full ChatGPT Session JSON (#314, #315).** The Add Account dialog gains a "ChatGPT Session" tab and the Import dialog gains a "paste text" button, so you can paste the raw JSON from `chatgpt.com/api/auth/session` and import in one step instead of manually splitting out `accessToken`/`email`. JSON parsing now accepts camelCase keys (`accessToken`/`idToken`/`planType`), flattens nested `user`/`account` objects (`user.email`→email, `user.name`→name, `user.id`/`account.id`→identity, `account.plan_type`→plan), and reads `expires`/`expired`/`expires_at` as either a numeric or string timestamp. Existing TXT/JSON/sub2api formats are unaffected — the new fallbacks only add information where a field was previously empty — and session imports flow through the normal importer, so they inherit identity dedup and the new/updated counting below.
- **Optional smart pacing spreads remaining quota evenly across a usage window (issue #312).** When enabled, if an account is burning a 5h/7d window faster than an even pace (used% running ahead of elapsed-time%), its dynamic concurrency is scaled down to a sustainable rate so the remaining quota lasts until the window reset instead of hitting the limit early. Pacing is per-account (each account paces on its own window, so the pool is smoothed as a whole), reuses the existing guard-band concurrency machinery (no new background task — it rides the usage snapshots already being refreshed), takes the stricter of the 5h/7d ratios, and never drops below a configurable floor. Off by default; Settings adds an enable toggle, a window selector (5h+7d / 5h / 7d), and a minimum-concurrency floor. Only engages when a valid usage+reset signal exists.

### Fixes

- **Credit accounts skip 5h/7d usage-window rate limiting (#313).** `credit_enabled + credit_skip_usage_window` accounts are meant to bypass usage-window limits, but response-header sync and the WHAM probe still marked them `rate_limited` whenever a 5h/7d window hit 100%, so a credit account was excluded from local scheduling despite the flag. A shared `SkipsUsageWindowLimits()` gate now records the usage snapshot but skips the local rate-limit marking (`MarkUsage7dRateLimited` / premium 5h) for such accounts, while real cooldown/error paths (401, existing cooldowns, genuine upstream failures) are preserved.
- **codex_at accounts merge into an existing same-identity account after the identity probe; import progress separates new vs updated.** codex_at tokens (`at-...`) carry no decodable JWT, so their OAuth identity is unknown at insert and only backfilled later by the WHAM probe — the post-probe merge the RT path already performs was missing here, so re-adding a codex_at could leave a duplicate. Manual codex_at adds now trigger a probe, and once it backfills email+account_id the importer re-checks for a same-identity account and merges into it (soft-deleting the new row, preserving usage stats). Separately, import/add now counts an "updated" bucket distinct from "new": hitting an existing identity only refreshes its credentials and no longer counts as a new add (fixing the success+duplicate double-count where re-importing the same accounts showed a nonzero "new"), and a single pasted access token now streams a progress bar too.
- **A 5h/7d window reset now triggers an immediate verification probe.** After a window's reset time passes an account looks "available" again, but it may actually be banned upstream — and the post-reset refresh was gated behind the usage `maxAge`, leaving up to ~10 minutes in which a bad account kept getting scheduled and failing. `NeedsUsageProbe` now fires a probe promptly when the reset boundary is crossed and the snapshot predates it (no `maxAge` delay), probing once per boundary; a real 401 then marks the account unauthorized.
- **A WebSocket upstream `close 1008 (policy violation)` now triggers async auth verification.** On the forced-WS transport a token that is actually invalid is rejected by the upstream as a `close 1008` rather than an HTTP 401, so it was classified as a generic transport failure — the account only lost health points, was never cooled down, and kept being scheduled and failing (the real `Unauthorized` was visible only on the HTTP transport). A WS upstream close now asynchronously runs a zero-cost WHAM probe for that account: a real 401 marks it `unauthorized` (matching the HTTP path), while a content-policy/transient 1008 leaves a healthy account untouched. Throttled to once per 30s per account.

## v2.4.1 - 2026-07-03

### Features

- **Bare RT imports now auto-merge into an existing same-identity account, preserving usage stats.** Scenario: an account was first imported via AT, then its bare RT was imported later. A bare RT carries no identity (it is not a JWT), so import-time dedup necessarily lets it through, and the identity only becomes known after the first refresh exchanges it for an AT — previously the flow stopped there, leaving two accounts (AT + RT) for the same identity in the pool. After a successful refresh the importer now re-checks for an identity duplicate: on a hit, the new credentials are merged into the existing account (writing the refresh token upgrades it to auto-renewable — RT takes highest priority) and the newly inserted row is soft-deleted. The merge only updates credential/identity keys — `codex_*` usage snapshots are untouched and the surviving account keeps its ID, so usage statistics and per-account request history are fully preserved. Copies imported with "allow duplicate add" are skipped.
- **AT batch add gains a progress bar.** `POST /accounts/at?stream=true` streams per-token SSE progress (aligned with the RT/ST path); the frontend automatically switches to streaming when more than one AT is pasted.

### Fixes

- **AT imports dedup by `user_id` identity — duplicate accounts no longer pile up.** Two stacked holes let the same account enter the pool repeatedly: (1) a personal-plan AT's JWT may carry no workspace `chatgpt_account_id`, only a `user_id` — identity dedup required email+account_id, so it fell back to raw access-token comparison, and since ATs rotate, a re-imported newer AT never matched the stored one; (2) the wham probe used to backfill `user_id` into the `account_id` field when the workspace id was missing, polluting the dedup key so even workspace-bearing JWTs never matched. JWT parsing now extracts `user_id` (with `chatgpt_user_id` fallback), the identity key is email + (account_id or user_id), the wham backfill no longer writes user_id into account_id, and `FindActiveAccountByOAuthIdentity` matches the `user_id` key as well as legacy polluted `account_id` values. Covers both the add dialog and TXT/JSON file imports.
- **Data migration v2 merges existing user_id-shaped duplicate accounts on startup.** The identity aliases now include the `user_id` key, and the dedupe migration reruns once to merge stock duplicates (both correctly-keyed `user_id` rows and rows whose `account_id` was polluted by the old wham backfill) — no manual cleanup needed; losers are soft-deleted into the recycle bin with audit events.
- **Copies imported via "allow duplicate add" are exempt from identity dedup and migration merges.** Forced duplicates (e.g. the same account with different proxies) were indistinguishable from accidental ones, so the dedupe migration would have merged away copies the user kept on purpose. Forced imports are now tagged `allow_duplicate=true` in credentials across all four add/import paths; the dedupe migrations skip tagged rows, and identity matching skips them too so a later normal import updates the primary account instead of writing new credentials into a forced copy. Note: only copies imported after this version carry the tag — force-imported duplicates from v2.3.9/v2.4.0 will still be merged by the v2 migration.
- **`context_length_exceeded` no longer triggers account-rotation retries (#310).** When the upstream returned `context_length_exceeded` as a `response.failed` frame (no explicit status code), the error-code string matcher didn't recognize it, fell through to the default 500 classification, and the request was transparently retried across up to 3 accounts — each attempt necessarily failing again while charging an innocent account a server-failure health penalty. Deterministic client errors (`context_length`/`context_window`, `string_above_max_length`, `model_not_found`, `unsupported_*`) are now classified as 400: no retry, no account penalty, and the error passes through to the client (which is the party that must shorten the input). The HTTP and WebSocket pre-first-token retry decisions share this classification.

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
