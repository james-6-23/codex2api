# Claude Code Client Platform and Version Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task with verification checkpoints.

**Goal:** Gate Claude OAuth requests by client platform and SemVer, support fixed/minimum versions, and prevent model-compatibility 400s from becoming account bans.

**Architecture:** Add a small `auth` policy/semver module that owns normalization, effective account/global policy, UA parsing, and model floors. The Anthropic request handler calls this module before transport and rewrites only the outbound UA for fixed policy. Persist global settings in `ClaudeConfig` and account overrides in credentials, expose them through existing admin APIs/UI, and classify matching upstream 400s as compatibility errors.

**Tech Stack:** Go, Gin, existing auth Store/credentials, React + TypeScript + i18next, Go tests and Node source-contract tests.

**Spec:** `docs/superpowers/specs/2026-09-02-claude-client-policy-design.md`

## Global Constraints

- Default behavior remains `client_platform=any`, `version_policy=passthrough`.
- `claude_code_cli_only` rejects unknown/non-CLI clients before Anthropic transport.
- `minimum` rejects missing/older SemVer with HTTP 426 and `claude update` guidance.
- Fable 5.1 model variants require Claude Code `2.1.251` or newer.
- Compatibility errors never mark `banned`, `unauthorized`, or account-level cooldown.
- Account policy overrides global policy; empty account fields inherit global.

### Task 1: SemVer and policy primitives

**Files:**
- Create: `auth/claude_client_policy.go`
- Test: `auth/claude_client_policy_test.go`

**Interfaces:**
- `type ClaudeClientPlatform string` with `any`, `claude_code_cli_only`.
- `type ClaudeVersionPolicy string` with `passthrough`, `fixed`, `minimum`.
- `type ClaudeClientPolicy struct { Platform, VersionPolicy, ClientVersion string }`.
- `func NormalizeClaudeClientPolicy(ClaudeClientPolicy) (ClaudeClientPolicy, error)`.
- `func ParseClaudeClientVersion(userAgent string) (string, bool)`.
- `func CompareClaudeClientVersions(a, b string) (int, error)`.
- `func ClaudeModelMinimumVersion(model string) string`.
- `func ValidateClaudeClientRequest(policy ClaudeClientPolicy, userAgent, model string) (ClaudeClientDecision, error)`.

- [ ] **Step 1: Write failing tests** for valid/invalid SemVer, CLI UA variants, platform rejection, fixed/minimum behavior, and Fable 5.1 floor.
- [ ] **Step 2: Run** `go test ./auth -run ClaudeClient -count=1`; confirm undefined symbols/failing assertions.
- [ ] **Step 3: Implement** strict SemVer parsing (numeric major/minor/patch with optional prerelease ignored for ordering), normalized policy validation, UA extraction, and decision results containing detected/required versions.
- [ ] **Step 4: Run** the focused tests and confirm all pass.
- [ ] **Step 5: Commit** `git add auth/claude_client_policy.go auth/claude_client_policy_test.go && git commit -m "feat: add Claude client policy primitives"`.

### Task 2: Persist global and account policy

**Files:**
- Modify: `auth/claude_fingerprint_mode.go`
- Modify: `admin/claude_config.go`
- Modify: `admin/handler.go`
- Modify: `admin/account_response_builder.go`
- Modify: `frontend/src/types.ts`
- Modify: `frontend/src/api.ts`
- Test: `admin/claude_config_test.go`, `admin/claude_accounts_test.go`

**Interfaces:**
- `ClaudeConfig` and `claudeGlobalConfigDTO` gain `client_platform`, `version_policy`, `client_version`.
- `accountSchedulerUpdate` accepts `claude_client_platform`, `claude_version_policy`, `claude_client_version` and persists them as credentials.
- `accountResponse` returns raw account overrides plus normalized effective policy fields.

- [ ] **Step 1: Add failing API/config tests** for default compatibility, global round-trip, account override validation, and empty-account inheritance.
- [ ] **Step 2: Run** focused admin tests and confirm failures.
- [ ] **Step 3: Add fields, validators, credential keys, effective-policy helper, and response projection. Reject invalid platform/policy/version with HTTP 400.
- [ ] **Step 4: Run** `go test ./admin -run 'ClaudeConfig|Claude.*AccountResponse|AccountScheduler' -count=1`.
- [ ] **Step 5: Commit** `git add auth admin frontend/src/types.ts frontend/src/api.ts && git commit -m "feat: persist Claude client policy"`.

### Task 3: Enforce policy before Anthropic transport

**Files:**
- Modify: `proxy/claude_upstream.go`
- Modify: `proxy/handler_anthropic.go`
- Modify: `proxy/errors.go` if a structured 426 error is needed
- Test: `proxy/claude_upstream_test.go`, `proxy/claude_client_policy_test.go`

**Interfaces:**
- `ExecuteClaudeMessagesRequest` receives the effective `auth.ClaudeClientPolicy` (or obtains it from the account/store) and returns a local compatibility error before `client.Do`.
- Fixed policy rewrites only outbound Claude Code UA version; preserve identity headers and audit recording.

- [ ] **Step 1: Add failing tests** proving CLI-only rejection, minimum/Fable rejection, fixed UA rewrite, and no transport invocation on rejection.
- [ ] **Step 2: Run** `go test ./proxy -run 'Claude.*Policy|ApplyClaudeMessagesHeaders' -count=1`; confirm failures.
- [ ] **Step 3: Add preflight decision and outbound UA version rewrite. Keep default any/passthrough behavior unchanged.
- [ ] **Step 4: Add focused upstream-error classifier for `invalid_request_error` messages requiring a newer Claude Code version; return compatibility metadata without calling account cooldown handlers.
- [ ] **Step 5: Run** focused proxy tests and existing Claude upstream tests.
- [ ] **Step 6: Commit** `git add proxy && git commit -m "feat: enforce Claude client platform and version"`.

### Task 4: Add global and account UI controls

**Files:**
- Modify: `frontend/src/pages/Settings.tsx`
- Modify: `frontend/src/pages/ClaudeAccounts.tsx`
- Modify: `frontend/src/locales/zh.json`, `frontend/src/locales/en.json`, `frontend/src/locales/zh-TW.json`
- Modify: `frontend/src/lib/claudeParity.test.mjs`

- [ ] **Step 1: Add failing source-contract tests** for global platform/version controls, account override controls, effective policy display, and compatibility error copy.
- [ ] **Step 2: Run** `npm test -- src/lib/claudeParity.test.mjs`; confirm failures.
- [ ] **Step 3: Add controlled selects/inputs, validation hints, API payloads, and account detail display. Keep defaults visibly “跟随全局/透传”.
- [ ] **Step 4: Run** the focused test, `npm run typecheck`, and `npm run build`.
- [ ] **Step 5: Commit** `git add frontend && git commit -m "feat: expose Claude client policy controls"`.

### Task 5: Full verification and integration review

**Files:**
- No new production files.

- [ ] **Step 1: Run** `go test ./...`.
- [ ] **Step 2: Run** `npm test && npm run typecheck && npm run build` from `frontend/`.
- [ ] **Step 3: Run** `git diff --check` and `gitnexus_detect_changes(scope=all)`; inspect changed flows for unexpected account-state effects.
- [ ] **Step 4: Run** a manual fixture test for `claude-cli/2.1.205` + `claude-fable-5-1` and verify local 426, no upstream request, and no cooldown mutation.
- [ ] **Step 5: Commit** any test-only adjustments after re-running the complete verification suite.
