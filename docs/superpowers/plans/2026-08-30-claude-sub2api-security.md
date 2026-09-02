# Claude/Sub2API 安全增强 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 让 ClaudeCode 原生透传与 NewAPI/Sub2API 渠道共享同一套规范化审核、出口安全策略和按渠道隔离的人物画像。

**Architecture:** 保留入口原始 body 用于 NewAPI 签名校验，将规范化 body 作为 Prompt 审核和 Claude 上游发送的唯一内容；Claude 全局配置提供默认拒绝的敏感字段/Beta Header/工具与输出限制。已验证的 NewAPI `channel_id` 只加入运行时风险和 session scope，持久化人物画像继续以平台用户身份聚合，避免同一平台同一用户跨渠道丢失画像。

**Tech Stack:** Go、SQLite/PostgreSQL、React/TypeScript、现有 Prompt Filter、NewAPI 签名元数据和 GitNexus。

---

### Task 1: 扩展 ClaudeCode 全局安全配置

**Files:**
- Modify: `auth/claude_fingerprint_mode.go`
- Modify: `auth/store.go`
- Modify: `admin/claude_config.go`
- Modify: `frontend/src/types.ts`
- Modify: `frontend/src/pages/Settings.tsx`
- Modify: `frontend/src/locales/zh.json`
- Modify: `frontend/src/locales/zh-TW.json`
- Modify: `frontend/src/locales/en.json`
- Test: `auth/claude_fingerprint_mode_test.go`, `admin/claude_config_test.go`

- [ ] **Step 1: Write failing tests** for parsing secure defaults, Beta allowlist normalization, output/tool limits, and round-trip admin configuration.
- [ ] **Step 2: Run the focused tests** and confirm they fail because the new fields and accessors do not exist.
- [ ] **Step 3: Add immutable runtime config accessors** backed by `atomic.Value`; normalize empty config to secure defaults without changing existing fingerprint/timezone behavior.
- [ ] **Step 4: Extend the admin DTO/API/UI** with clear labels and bounded numeric inputs; reject invalid limits and unsafe header names.
- [ ] **Step 5: Run focused Go and frontend tests** and confirm all pass.

### Task 2: Canonical Claude request and egress policy

**Files:**
- Modify: `proxy/claude_upstream.go`
- Modify: `proxy/handler_anthropic.go`
- Modify: `proxy/prompt_filter.go`
- Test: `proxy/claude_upstream_test.go`, `proxy/prompt_filter_test.go`, `proxy/anthropic_test.go`

- [ ] **Step 1: Write failing tests** proving zero-width/bidi normalization occurs before Prompt Filter, final upstream body matches audited canonical body, sensitive fields are removed by default, allowed fields survive, and disallowed Beta tokens are removed.
- [ ] **Step 2: Run the tests** and confirm they fail on the current raw-before-normalize flow.
- [ ] **Step 3: Add a Claude request canonicalizer** that preserves JSON structure, normalizes text, removes configured sensitive fields, bounds tools/output, and returns a redacted audit digest.
- [ ] **Step 4: Route `/v1/messages` through canonical body** for Prompt Filter, model extraction, learning evidence, and Claude upstream; keep the original ingress body for signature verification and source evidence.
- [ ] **Step 5: Make `anthropic-beta` required-plus-allowlist** and keep `x-api-key`, cookies, authorization overrides, and hop-by-hop headers outside the Claude upstream boundary.
- [ ] **Step 6: Run focused tests and inspect audit fields** to ensure no raw credential or unbounded payload is logged.

### Task 3: Channel-aware NewAPI runtime risk and session isolation

**Files:**
- Modify: `proxy/newapi_policy.go`
- Modify: `proxy/prompt_filter_advanced.go`
- Modify: `proxy/prompt_guard_extensions.go`
- Test: `proxy/newapi_policy_test.go`, `proxy/prompt_guard_extensions_test.go`, `proxy/prompt_conversation_lock_test.go`

- [ ] **Step 1: Write failing tests** showing two signed requests with the same platform/user but different `channel_id` receive distinct runtime risk/session scopes, while the persisted person identity remains discoverable by platform/user.
- [ ] **Step 2: Run the tests** and confirm current scope keys collide because `channel_id` is ignored.
- [ ] **Step 3: Add a normalized channel component** to runtime scope keys and session correlation keys only when signed, valid channel metadata exists; retain a legacy-compatible scope for channel `0`.
- [ ] **Step 4: Ensure verified channel metadata is carried into incident/audit metadata** without exposing secrets or changing unsigned-request behavior.
- [ ] **Step 5: Run the focused risk, lock, and identity tests.**

### Task 4: Full verification and change-scope review

**Files:**
- No production file additions beyond Tasks 1–3.

- [ ] **Step 1: Run** `gofmt -w` on changed Go files and `git diff --check`.
- [ ] **Step 2: Run** `go test ./... -count=1`, `go vet ./...`, `npm test`, `npm run typecheck`, `npm run build`, and `npm run audit:ci`.
- [ ] **Step 3: Run** `npx gitnexus detect-changes --scope unstaged --repo codex2api` and review the affected flows.
- [ ] **Step 4: Verify** no production deployment, credential output, or Git commit occurs unless separately requested.
