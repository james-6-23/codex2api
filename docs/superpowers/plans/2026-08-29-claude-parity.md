# Claude 渠道对等适配实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让已导入的 Claude OAuth 账号自动产生真实用量快照，并在统计、路由和管理页面中与支持范围匹配地展示。

**Architecture:** 保留现有导入探针队列和生命周期管理，新增 Claude 专用 Anthropic Messages 采样器；所有统计通过 `upstream_type`/运行时账号统一归属。Claude 只走原生 Messages，Dashboard/Usage/API Key/代理/调度页面共享同一渠道枚举和模型目录。

**Tech Stack:** Go、SQLite/PostgreSQL、React、TypeScript、Node test runner、GitNexus。

---

### Task 1: Claude provider-aware sampling

**Files:**
- Modify: `admin/claude_accounts.go:308-393`
- Modify: `admin/usage_probe.go:54-180`
- Modify: `auth/store.go:2950-2970,10170-10320`
- Modify: `auth/account.go` provider predicates
- Test: `admin/usage_probe_test.go`, `admin/claude_accounts_test.go`, `auth/store_scheduler_test.go`

- [ ] **Step 1: Write failing tests**

Add tests asserting that a Claude import enqueues exactly one provider-specific probe, that the probe never calls WHAM/Responses, and that Anthropic rate-limit headers persist 5h/7d state.

- [ ] **Step 2: Run the focused tests and verify RED**

Run `go test ./admin ./auth -run 'Claude|UsageProbe' -count=1`. Expected failure: no Claude probe is scheduled or the generic probe rejects the provider.

- [ ] **Step 3: Implement the minimal provider path**

Add `ProbeClaudeUsageSnapshot(ctx, account)` using `proxy.ExecuteClaudeMessagesRequest` with a bounded minimal request, call `proxy.SyncClaudeUsageState`, and route `insertClaudeAccount` through `scheduleImportedAccountWarmup`. Keep Claude out of `ProbeUsageSnapshot`'s WHAM/Responses branches and add in-flight deduplication through the existing import queue.

- [ ] **Step 4: Verify GREEN and regression coverage**

Run `go test ./admin ./auth -run 'Claude|UsageProbe' -count=1`, then `go test ./...`. Expected: focused tests and all existing tests pass.

- [ ] **Step 5: Commit**

`git add admin/claude_accounts.go admin/usage_probe.go auth/account.go auth/store.go admin/*test.go auth/*test.go && git commit -m "fix(claude): sample usage after account import"`

### Task 2: Backend channel attribution and analysis

**Files:**
- Modify: `admin/handler.go:1360-1495`
- Modify: `admin/account_analysis.go:300-500`
- Modify: `admin/accounts_paged.go:1180-1220`
- Modify: `proxy/handler.go` UsageLog channel selection
- Modify: `proxy/handler_anthropic.go` Claude success/error log paths
- Modify: `proxy/handler.go:370-410` provider channel filter
- Modify: `proxy/model_registry.go`, `admin/handler.go` model catalog response
- Test: `admin/handler_test.go`, `admin/account_analysis_test.go`, `proxy/handler_test.go`

- [ ] **Step 1: Write failing tests**

Cover Claude-only dashboard counts, Claude not being treated as an unsampled Codex account after a valid snapshot, Claude 5h plan families, UsageLog `channel=claude`, and exclusion of Claude from Responses/Chat routing.

- [ ] **Step 2: Run focused tests and verify RED**

Run `go test ./admin ./proxy -run 'Claude|Dashboard|UsageLog|Channel' -count=1`. Expected failures show Claude counted as Codex or routed to the wrong protocol.

- [ ] **Step 3: Implement provider-aware classification**

Initialize `channelCounts` with `database.UpstreamChannelClaude`; detect `auth.UpstreamClaude` before the Codex fallback; treat Claude snapshots as sampled; add a Claude-specific subscription/5h capability predicate; set UsageLog channel from the account provider; and return `claude_models` from the model catalog while rejecting unsupported protocol routes.

- [ ] **Step 4: Verify GREEN**

Run the focused command and `go test ./...`; assert no Codex/Grok regression.

- [ ] **Step 5: Commit**

`git add admin proxy && git commit -m "fix(claude): keep channel stats and routing isolated"`

### Task 3: Frontend channel and page parity

**Files:**
- Modify: `frontend/src/components/ChannelFilter.tsx`
- Modify: `frontend/src/pages/Dashboard.tsx`
- Modify: `frontend/src/pages/Usage.tsx`
- Modify: `frontend/src/pages/APIKeys.tsx`
- Modify: `frontend/src/pages/Proxies.tsx`, `frontend/src/components/SchedulerBoard.tsx`
- Modify: `frontend/src/pages/ClaudeAccounts.tsx`, `frontend/src/types.ts`, `frontend/src/locales/en.json`, `frontend/src/locales/zh.json`, `frontend/src/locales/zh-TW.json`
- Test: `frontend/src/lib/claudeParity.test.mjs`, existing page helper tests

- [ ] **Step 1: Write failing frontend tests**

Assert that shared channel options include Claude, Dashboard breakdown renders Claude, Usage model filters and log badges recognize Claude, and Claude rows display sampled/unsampled/error state with last sample time.

- [ ] **Step 2: Run `npm test` and verify RED**

Run `npm test -- src/lib/claudeParity.test.mjs`; expected failures show missing Claude options and labels.

- [ ] **Step 3: Implement UI parity**

Extend `UsageChannel` and shared options to `"claude"`; add Claude to Dashboard breakdown and Usage model catalogs; add channel/logo labels to API Keys, Proxies, Scheduler; expose provider-specific empty/loading/sample states in `ClaudeAccounts`; hide unsupported Codex-only actions with localized explanations.

- [ ] **Step 4: Verify GREEN**

Run `npm test`, `npm run typecheck`, and `npm run build`.

- [ ] **Step 5: Commit**

`git add frontend && git commit -m "feat(ui): expose Claude channel across admin views"`

### Task 4: Existing-environment verification and handoff

**Files:**
- Modify: `docs/CLAUDE.md` or `docs/CONFIGURATION.md` only if an actual setting/API changed
- Test: existing local environment, no new port

- [ ] **Step 1: Run complete verification**

Run `go test ./...`, `go vet ./...`, `cd frontend && npm run typecheck && npm test && npm run build`.

- [ ] **Step 2: Verify existing local service**

Probe the already-running local service URL/port discovered from the current environment; check Claude account list, Dashboard Claude filter, Usage Claude filter, and one account's sample state. Do not create another listener or re-import credentials.

- [ ] **Step 3: Run GitNexus change detection**

Run `gitnexus_detect_changes(scope: "all")`, review affected flows, and ensure only Claude provider, statistics, sampling, and UI modules changed.

- [ ] **Step 4: Commit documentation if needed**

Only commit an actual documentation change with `git add docs/... && git commit -m "docs(claude): document provider parity and sampling"`.
