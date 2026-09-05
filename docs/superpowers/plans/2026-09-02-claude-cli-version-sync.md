# Claude CLI 版本同步与指纹版本对齐 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让代理自动跟踪 Claude Code CLI 最新版本、回写所有 Claude 账号指纹 UA、并让版本门控作用于最终出站 UA，同时统一前端下拉组件并把 UI 约束写进 DESIGN.md / CLAUDE.md。

**Architecture:** 版本同步逻辑镜像现有 `proxy/codex_cli_version_sync.go`：GitHub releases/latest 为主源、npm dist-tags 回退，同步值存 `system_settings.claude_synced_cli_version` 单列，运行时取"内置常量与同步值的较大者"。生效版本通过 `auth` 包级原子变量发布（`GenerateClaudeFingerprint` 是无 Store 的自由函数，包级访问器比 Store 方法更合适；这是对 spec "Store 访问器"措辞的实现细化，语义不变）。每次同步与服务启动时把生效版本回写到所有 Claude 账号的 `custom_headers.User-Agent` 版本段。`ExecuteClaudeMessagesRequestWithPolicy` 在指纹改写完成后再对最终出站 UA 做一次版本对齐。

**Tech Stack:** Go 1.2x（gin、tidwall/gjson、modernc sqlite / pgx）、React + TypeScript（Vite、react-i18next、node:test 源码守卫测试）。

**Spec:** `docs/superpowers/specs/2026-09-02-claude-cli-version-sync-design.md`

## Global Constraints

- 内置常量 `auth.BuiltinClaudeCLIVersion = "2.1.258"`；生效版本永不低于内置常量。
- 版本源顺序固定：GitHub `https://api.github.com/repos/anthropics/claude-code/releases/latest` → npm `https://registry.npmjs.org/-/package/@anthropic-ai/claude-code/dist-tags`。
- 环境变量硬开关 `CLAUDE_DISABLE_CLI_VERSION_SYNC=1|true|yes|on`。
- 同步间隔小时钳到 `[1, 720]`，缺失或 0 视为 12；`cli_version_sync_enabled` 缺失视为 true。
- 指纹回写只改 UA 的版本号段；UA 缺失或不可识别为 CLI 的账号跳过。
- 前端禁止手写 `<select>`，一律用 `components/ui/select.tsx` 的 `Select`。
- 新文案必须同时写入 `frontend/src/locales/zh.json`、`en.json`、`zh-TW.json`。
- 每个 Go 符号改动前按项目 CLAUDE.md 要求运行 `gitnexus_impact`；提交前运行 `gitnexus_detect_changes()`。
- Go 测试命令：`go test ./auth/ ./proxy/ ./database/ ./admin/ -run <Name> -count=1`。前端：`cd frontend && npm test && npm run typecheck && npm run build`。
- 提交信息末尾加 `Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ`。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `auth/claude_cli_version.go`（新建） | 内置常量、同步值原子变量、生效版本计算、UA 版本段改写正则 |
| `auth/claude_cli_version_test.go`（新建） | 上述纯函数测试 |
| `auth/claude_fingerprint.go` | 删除随机版本池，UA 版本改用生效版本 |
| `auth/claude_fingerprint_test.go` | 更新指纹版本断言 |
| `auth/claude_fingerprint_refresh.go`（新建） | 遍历 Claude 账号回写指纹 UA 版本 |
| `auth/claude_fingerprint_refresh_test.go`（新建） | 回写逻辑测试 |
| `auth/claude_fingerprint_mode.go` | `ClaudeConfig` 增加同步开关/间隔字段与 Store 访问器 |
| `auth/store.go` | Store 结构体增加两个原子字段 |
| `database/claude_cli_version.go`（新建） | 同步值单列读写、账号 custom_headers 窄更新 |
| `database/claude_cli_version_test.go`（新建） | SQLite 临时库测试 |
| `database/sqlite.go`、`database/postgres.go` | 新列 `claude_synced_cli_version` |
| `proxy/claude_cli_version_sync.go`（新建） | 抓取、同步、后台任务 |
| `proxy/claude_cli_version_sync_test.go`（新建） | httptest 主源/回退测试 |
| `proxy/claude_upstream.go` | 出站 UA 版本对齐；`rewriteClaudeCLIUserAgentVersion` 改为委托 auth |
| `proxy/claude_upstream_test.go` | 对齐逻辑与本地拒绝测试 |
| `admin/claude_config.go` | DTO 新字段、GET 只读字段、PUT 持久化、`SyncClaudeCLIVersion` handler |
| `admin/claude_config_test.go` | 接口测试 |
| `admin/handler.go` | 注册路由 |
| `main.go` | 启动加载同步值、启动后台任务 |
| `frontend/src/types.ts`、`frontend/src/api.ts` | 类型与 API 方法 |
| `frontend/src/pages/Settings.tsx` | ClaudeCode 卡片换 Select、新增同步区块 |
| `frontend/src/pages/ClaudeAccounts.tsx`、`frontend/src/pages/Proxies.tsx` | 换 Select |
| `frontend/src/locales/*.json` | 文案 |
| `frontend/src/lib/uiConventions.test.mjs`（新建）、`frontend/src/lib/claudeParity.test.mjs` | 守卫测试 |
| `DESIGN.md`（新建）、`CLAUDE.md` | UI 约束 |

---

### Task 1: auth 生效版本与 UA 版本段改写

**Files:**
- Create: `auth/claude_cli_version.go`
- Create: `auth/claude_cli_version_test.go`
- Modify: `proxy/claude_upstream.go:296-304`

**Interfaces:**
- Produces:
  - `const BuiltinClaudeCLIVersion = "2.1.258"`
  - `func SetClaudeSyncedCLIVersion(version string)`
  - `func ClaudeSyncedCLIVersion() string`
  - `func EffectiveClaudeCLIVersion() string`
  - `func RewriteClaudeCLIUserAgentVersion(userAgent, version string) string`（版本非法返回空串）

- [ ] **Step 1: 写失败测试**

```go
// auth/claude_cli_version_test.go
package auth

import "testing"

func TestEffectiveClaudeCLIVersion_NeverBelowBuiltin(t *testing.T) {
	t.Cleanup(func() { SetClaudeSyncedCLIVersion("") })
	cases := map[string]string{
		"":             BuiltinClaudeCLIVersion,
		"garbage":      BuiltinClaudeCLIVersion,
		"2.1.100":      BuiltinClaudeCLIVersion,
		"2.1.258":      BuiltinClaudeCLIVersion,
		"2.1.300":      "2.1.300",
		" v2.1.301 ":   "2.1.301",
		"2.1.300-beta": BuiltinClaudeCLIVersion, // 预发布不高于正式版
	}
	for synced, want := range cases {
		SetClaudeSyncedCLIVersion(synced)
		if got := EffectiveClaudeCLIVersion(); got != want {
			t.Errorf("synced=%q effective=%q want %q", synced, got, want)
		}
	}
}

func TestRewriteClaudeCLIUserAgentVersion(t *testing.T) {
	cases := []struct{ ua, version, want string }{
		{"claude-cli/2.1.219 (external, cli)", "2.1.258", "claude-cli/2.1.258 (external, cli)"},
		{"Claude Code/2.1.1 windows", "2.1.258", "Claude Code/2.1.258 windows"},
		{"curl/8.7.1", "2.1.258", "curl/8.7.1"},
		{"claude-cli/2.1.219 (external, cli)", "bad", ""},
	}
	for _, tc := range cases {
		if got := RewriteClaudeCLIUserAgentVersion(tc.ua, tc.version); got != tc.want {
			t.Errorf("Rewrite(%q,%q)=%q want %q", tc.ua, tc.version, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./auth/ -run 'TestEffectiveClaudeCLIVersion|TestRewriteClaudeCLIUserAgentVersion' -count=1`
Expected: FAIL，`undefined: BuiltinClaudeCLIVersion`。

- [ ] **Step 3: 实现**

```go
// auth/claude_cli_version.go
package auth

import (
	"regexp"
	"strings"
	"sync/atomic"
)

// BuiltinClaudeCLIVersion 是编译期内置的 Claude Code CLI 版本下限。
// 生效版本取它与后台同步值中的较大者，远端异常永不导致降级。
const BuiltinClaudeCLIVersion = "2.1.258"

var claudeSyncedCLIVersion atomic.Value // string

// claudeCLIUserAgentVersionPattern 匹配 Claude Code CLI UA 中的版本号段。
var claudeCLIUserAgentVersionPattern = regexp.MustCompile(`(?i)(\bclaude(?:-cli|-code)|\bclaude\s+code)([/\s:_-]*)(?:v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)`)

// SetClaudeSyncedCLIVersion 发布后台同步得到的最新版本；非法值归一为空串。
func SetClaudeSyncedCLIVersion(version string) {
	normalized, ok := ParseClaudeClientVersion("claude-cli/" + strings.TrimSpace(version))
	if !ok {
		normalized = ""
	}
	claudeSyncedCLIVersion.Store(normalized)
}

// ClaudeSyncedCLIVersion 返回已同步的规范化版本（空=尚未同步）。
func ClaudeSyncedCLIVersion() string {
	if v, ok := claudeSyncedCLIVersion.Load().(string); ok {
		return v
	}
	return ""
}

// EffectiveClaudeCLIVersion 返回当前生效的 Claude Code CLI 版本：
// max(内置常量, 同步值)。
func EffectiveClaudeCLIVersion() string {
	synced := ClaudeSyncedCLIVersion()
	if synced == "" {
		return BuiltinClaudeCLIVersion
	}
	if cmp, err := CompareClaudeClientVersions(synced, BuiltinClaudeCLIVersion); err == nil && cmp > 0 {
		return synced
	}
	return BuiltinClaudeCLIVersion
}

// RewriteClaudeCLIUserAgentVersion 只替换 CLI UA 中的版本号段；version 非法返回空串，
// UA 不含 CLI 版本段时原样返回。
func RewriteClaudeCLIUserAgentVersion(userAgent, version string) string {
	version = strings.TrimSpace(version)
	if _, ok := ParseClaudeClientVersion("claude-cli/" + version); !ok {
		return ""
	}
	return claudeCLIUserAgentVersionPattern.ReplaceAllString(userAgent, "${1}${2}"+version)
}
```

把 `proxy/claude_upstream.go` 中的 `claudeCLIUserAgentVersionPattern` 变量删除，`rewriteClaudeCLIUserAgentVersion` 改为：

```go
func rewriteClaudeCLIUserAgentVersion(userAgent, version string) string {
	return auth.RewriteClaudeCLIUserAgentVersion(userAgent, version)
}
```

若 `regexp` 在 `proxy/claude_upstream.go` 中不再使用，删除该 import。

- [ ] **Step 4: 运行测试**

Run: `go test ./auth/ ./proxy/ -run 'TestEffectiveClaudeCLIVersion|TestRewriteClaudeCLIUserAgentVersion|TestApplyClaudeMessagesHeadersRewritesFixedClaudeCLIVersion' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add auth/claude_cli_version.go auth/claude_cli_version_test.go proxy/claude_upstream.go
git commit -m "feat(auth): add effective Claude CLI version and UA version rewrite

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 2: 指纹生成改用生效版本

**Files:**
- Modify: `auth/claude_fingerprint.go:22-32,62-80`
- Modify: `auth/claude_fingerprint_test.go`

**Interfaces:**
- Consumes: `EffectiveClaudeCLIVersion()`（Task 1）
- Produces: `GenerateClaudeFingerprint(timezone string) ClaudeFingerprint` 签名不变，UA 版本 = 生效版本。

- [ ] **Step 1: 写失败测试**

在 `auth/claude_fingerprint_test.go` 末尾追加：

```go
func TestGenerateClaudeFingerprint_UsesEffectiveCLIVersion(t *testing.T) {
	t.Cleanup(func() { SetClaudeSyncedCLIVersion("") })
	SetClaudeSyncedCLIVersion("2.1.300")
	for i := 0; i < 10; i++ {
		fp := GenerateClaudeFingerprint("")
		if fp.UserAgent != "claude-cli/2.1.300 (external, cli)" {
			t.Fatalf("UA 应使用生效版本, got %s", fp.UserAgent)
		}
	}
	SetClaudeSyncedCLIVersion("")
	if fp := GenerateClaudeFingerprint(""); fp.UserAgent != "claude-cli/"+BuiltinClaudeCLIVersion+" (external, cli)" {
		t.Fatalf("无同步值时应使用内置版本, got %s", fp.UserAgent)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./auth/ -run TestGenerateClaudeFingerprint_UsesEffectiveCLIVersion -count=1`
Expected: FAIL，UA 版本来自随机池。

- [ ] **Step 3: 实现**

在 `auth/claude_fingerprint.go`：删除 `claudeCLIVersions = []string{...}` 那一行；`GenerateClaudeFingerprint` 内把 `cliVer := claudePick(claudeCLIVersions)` 改为 `cliVer := EffectiveClaudeCLIVersion()`。文件头注释中"值域取自真实 Claude Code ... 随机挑选"一段补一句："CLI 版本不再随机，始终使用 EffectiveClaudeCLIVersion()，并由后台同步任务回写到已有账号。"

- [ ] **Step 4: 运行测试**

Run: `go test ./auth/ -run 'TestGenerateClaudeFingerprint|TestClaudeFingerprintHeaders' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add auth/claude_fingerprint.go auth/claude_fingerprint_test.go
git commit -m "feat(auth): pin generated Claude fingerprint UA to effective CLI version

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 3: 指纹回写例程

**Files:**
- Create: `auth/claude_fingerprint_refresh.go`
- Create: `auth/claude_fingerprint_refresh_test.go`

**Interfaces:**
- Consumes: `ParseClaudeClientVersion`、`CompareClaudeClientVersions`、`RewriteClaudeCLIUserAgentVersion`、`cloneStringMap`（auth 已有）、`Store.accounts`/`Store.mu`。
- Produces:
  - `type ClaudeCustomHeadersPersister interface { UpdateAccountCustomHeaders(ctx context.Context, id int64, headers map[string]string) error }`
  - `func RefreshClaudeFingerprintUserAgent(headers map[string]string, targetVersion string) (map[string]string, bool)`
  - `func RefreshClaudeFingerprintVersions(ctx context.Context, store *Store, persister ClaudeCustomHeadersPersister, version string) (int, error)`

- [ ] **Step 1: 写失败测试**

```go
// auth/claude_fingerprint_refresh_test.go
package auth

import (
	"context"
	"errors"
	"testing"
)

type recordingPersister struct {
	calls map[int64]map[string]string
	fail  map[int64]error
}

func (r *recordingPersister) UpdateAccountCustomHeaders(_ context.Context, id int64, headers map[string]string) error {
	if err := r.fail[id]; err != nil {
		return err
	}
	if r.calls == nil {
		r.calls = map[int64]map[string]string{}
	}
	r.calls[id] = headers
	return nil
}

func TestRefreshClaudeFingerprintUserAgent(t *testing.T) {
	old := map[string]string{"user-agent": "claude-cli/2.1.219 (external, cli)", "X-Stainless-OS": "MacOS"}
	next, changed := RefreshClaudeFingerprintUserAgent(old, "2.1.258")
	if !changed || next["user-agent"] != "claude-cli/2.1.258 (external, cli)" {
		t.Fatalf("should bump version only: %v", next)
	}
	if next["X-Stainless-OS"] != "MacOS" || old["user-agent"] != "claude-cli/2.1.219 (external, cli)" {
		t.Fatal("other headers must be kept and input must not be mutated")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "claude-cli/2.1.258 (external, cli)"}, "2.1.258"); changed {
		t.Fatal("equal version must be a no-op")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "claude-cli/2.1.300 (external, cli)"}, "2.1.258"); changed {
		t.Fatal("newer fingerprint must not be downgraded")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"X-App": "cli"}, "2.1.258"); changed {
		t.Fatal("missing UA must be skipped")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "curl/8.7.1"}, "2.1.258"); changed {
		t.Fatal("non-CLI UA must be skipped")
	}
}

func TestRefreshClaudeFingerprintVersions_PersistsAndAppliesInMemory(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	claudeOld := &Account{DBID: 251, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)", "X-App": "cli"}}
	claudeNew := &Account{DBID: 252, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.258 (external, cli)"}}
	claudeBroken := &Account{DBID: 253, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.205 (external, cli)"}}
	codex := &Account{DBID: 1, UpstreamType: "codex", CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.100 (external, cli)"}}
	store.mu.Lock()
	store.accounts = []*Account{claudeOld, claudeNew, claudeBroken, codex}
	store.mu.Unlock()

	persister := &recordingPersister{fail: map[int64]error{253: errors.New("db down")}}
	updated, err := RefreshClaudeFingerprintVersions(context.Background(), store, persister, "2.1.258")
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}
	if err == nil || !errors.Is(err, persister.fail[253]) {
		t.Fatalf("first persist error should surface, got %v", err)
	}
	if got := persister.calls[251]["User-Agent"]; got != "claude-cli/2.1.258 (external, cli)" {
		t.Fatalf("persisted UA = %q", got)
	}
	if persister.calls[251]["X-App"] != "cli" {
		t.Fatal("other fingerprint headers must be persisted unchanged")
	}
	if claudeOld.CustomHeaders["User-Agent"] != "claude-cli/2.1.258 (external, cli)" {
		t.Fatal("in-memory account must be updated after persist")
	}
	if claudeBroken.CustomHeaders["User-Agent"] != "claude-cli/2.1.205 (external, cli)" {
		t.Fatal("failed persist must not update memory")
	}
	if _, called := persister.calls[1]; called {
		t.Fatal("non-Claude accounts must be ignored")
	}
	if _, called := persister.calls[252]; called {
		t.Fatal("up-to-date accounts must not be written")
	}
}

func TestRefreshClaudeFingerprintVersions_RejectsInvalidVersion(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	if _, err := RefreshClaudeFingerprintVersions(context.Background(), store, nil, "nope"); err == nil {
		t.Fatal("invalid target version must error")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./auth/ -run 'TestRefreshClaudeFingerprint' -count=1`
Expected: FAIL，`undefined: RefreshClaudeFingerprintUserAgent`。

- [ ] **Step 3: 实现**

```go
// auth/claude_fingerprint_refresh.go
package auth

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// ClaudeCustomHeadersPersister 把账号指纹头持久化到凭据存储（由 database.DB 实现）。
type ClaudeCustomHeadersPersister interface {
	UpdateAccountCustomHeaders(ctx context.Context, id int64, headers map[string]string) error
}

// RefreshClaudeFingerprintUserAgent 在指纹 UA 版本低于 targetVersion 时返回只改了版本段的副本。
// UA 缺失、无法识别为 CLI、或版本不低于目标时返回 (原 map, false)。
func RefreshClaudeFingerprintUserAgent(headers map[string]string, targetVersion string) (map[string]string, bool) {
	uaKey := ""
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "user-agent") {
			uaKey = key
			break
		}
	}
	if uaKey == "" {
		return headers, false
	}
	current, ok := ParseClaudeClientVersion(headers[uaKey])
	if !ok {
		return headers, false
	}
	if cmp, err := CompareClaudeClientVersions(current, targetVersion); err != nil || cmp >= 0 {
		return headers, false
	}
	rewritten := RewriteClaudeCLIUserAgentVersion(headers[uaKey], targetVersion)
	if rewritten == "" {
		return headers, false
	}
	next := cloneStringMap(headers)
	next[uaKey] = rewritten
	return next, true
}

// RefreshClaudeFingerprintVersions 把所有 Claude 账号的指纹 UA 版本抬到 version。
// 返回实际改写的账号数与首个持久化错误；单账号失败不影响其它账号。
func RefreshClaudeFingerprintVersions(ctx context.Context, store *Store, persister ClaudeCustomHeadersPersister, version string) (int, error) {
	target, ok := ParseClaudeClientVersion("claude-cli/" + strings.TrimSpace(version))
	if !ok {
		return 0, fmt.Errorf("invalid Claude CLI version %q", version)
	}
	if store == nil {
		return 0, nil
	}
	store.mu.RLock()
	accounts := append([]*Account(nil), store.accounts...)
	store.mu.RUnlock()

	updated := 0
	var firstErr error
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		acc.mu.RLock()
		isClaude := strings.EqualFold(strings.TrimSpace(acc.UpstreamType), UpstreamClaude)
		headers := cloneStringMap(acc.CustomHeaders)
		dbID := acc.DBID
		acc.mu.RUnlock()
		if !isClaude {
			continue
		}
		next, changed := RefreshClaudeFingerprintUserAgent(headers, target)
		if !changed {
			continue
		}
		if persister != nil {
			if err := persister.UpdateAccountCustomHeaders(ctx, dbID, next); err != nil {
				log.Printf("[claude-cli-version-sync] 账号 %d 指纹版本回写失败: %v", dbID, err)
				if firstErr == nil {
					firstErr = fmt.Errorf("account %d: %w", dbID, err)
				}
				continue
			}
		}
		acc.mu.Lock()
		acc.CustomHeaders = next
		acc.mu.Unlock()
		updated++
	}
	return updated, firstErr
}
```

注意 `persister` 为接口；调用方持有 `*database.DB` 为 nil 时必须传字面 `nil`，不能传 nil 指针（Task 6 会处理）。

- [ ] **Step 4: 运行测试**

Run: `go test ./auth/ -run 'TestRefreshClaudeFingerprint' -count=1 -race`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add auth/claude_fingerprint_refresh.go auth/claude_fingerprint_refresh_test.go
git commit -m "feat(auth): refresh Claude fingerprint UA versions to effective CLI version

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 4: ClaudeConfig 同步开关/间隔与 Store 访问器

**Files:**
- Modify: `auth/claude_fingerprint_mode.go`（`ClaudeConfig` 结构体、`ParseClaudeConfig`、`applyClaudeConfigToStore`）
- Modify: `auth/store.go:3333-3340`（Store 字段）
- Test: `auth/claude_fingerprint_mode_test.go`（已有文件则追加，否则新建）

**Interfaces:**
- Produces:
  - `ClaudeConfig.CLIVersionSyncEnabled *bool`（json `cli_version_sync_enabled`）、`ClaudeConfig.CLIVersionSyncIntervalHours int`（json `cli_version_sync_interval_hours`）
  - `func (c ClaudeConfig) CLIVersionSyncEnabledValue() bool`
  - `func NormalizeClaudeCLIVersionSyncIntervalHours(hours int) int`
  - `func (s *Store) SetClaudeCLIVersionSync(enabled bool, intervalHours int)`
  - `func (s *Store) ClaudeCLIVersionSyncEnabled() bool`
  - `func (s *Store) ClaudeCLIVersionSyncIntervalHours() int`

- [ ] **Step 1: 写失败测试**

```go
// 追加到 auth/claude_fingerprint_mode_test.go（不存在则新建，package auth）
func TestParseClaudeConfig_CLIVersionSyncDefaults(t *testing.T) {
	cfg := ParseClaudeConfig(`{"fingerprint_mode":"force"}`)
	if !cfg.CLIVersionSyncEnabledValue() {
		t.Fatal("missing cli_version_sync_enabled must default to true")
	}
	if cfg.CLIVersionSyncIntervalHours != 12 {
		t.Fatalf("interval = %d, want 12", cfg.CLIVersionSyncIntervalHours)
	}
	cfg = ParseClaudeConfig(`{"cli_version_sync_enabled":false,"cli_version_sync_interval_hours":9999}`)
	if cfg.CLIVersionSyncEnabledValue() {
		t.Fatal("explicit false must be honored")
	}
	if cfg.CLIVersionSyncIntervalHours != 720 {
		t.Fatalf("interval = %d, want 720 clamp", cfg.CLIVersionSyncIntervalHours)
	}
}

func TestStore_ClaudeCLIVersionSyncAccessors(t *testing.T) {
	s := NewStore(nil, nil, nil)
	defer s.Stop()
	if !s.ClaudeCLIVersionSyncEnabled() || s.ClaudeCLIVersionSyncIntervalHours() != 12 {
		t.Fatalf("defaults: enabled=%v hours=%d", s.ClaudeCLIVersionSyncEnabled(), s.ClaudeCLIVersionSyncIntervalHours())
	}
	s.SetClaudeCLIVersionSync(false, 0)
	if s.ClaudeCLIVersionSyncEnabled() || s.ClaudeCLIVersionSyncIntervalHours() != 12 {
		t.Fatal("disabled + zero interval should read false/12")
	}
	applyClaudeConfigToStore(s, `{"cli_version_sync_enabled":true,"cli_version_sync_interval_hours":6}`)
	if !s.ClaudeCLIVersionSyncEnabled() || s.ClaudeCLIVersionSyncIntervalHours() != 6 {
		t.Fatal("applyClaudeConfigToStore must publish sync settings")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./auth/ -run 'TestParseClaudeConfig_CLIVersionSyncDefaults|TestStore_ClaudeCLIVersionSyncAccessors' -count=1`
Expected: FAIL，字段/方法未定义。

- [ ] **Step 3: 实现**

`auth/store.go` Store 结构体，在 `claudeSessionWindowLimit int64` 下一行加：

```go
	claudeCLIVersionSyncDisabled  atomic.Bool  // Claude CLI 版本自动同步是否关闭（零值=开启）
	claudeCLIVersionSyncIntervalH atomic.Int64 // Claude CLI 版本同步间隔小时（0=默认 12）
```

`auth/claude_fingerprint_mode.go`：

```go
// ClaudeConfig 结构体新增两字段（放在 SessionWindowLimit 之后）：
	CLIVersionSyncEnabled       *bool `json:"cli_version_sync_enabled,omitempty"`        // 缺失=true
	CLIVersionSyncIntervalHours int   `json:"cli_version_sync_interval_hours,omitempty"` // 0=12，钳 [1,720]

// CLIVersionSyncEnabledValue 把缺失字段解释为开启，避免老配置静默关闭同步。
func (c ClaudeConfig) CLIVersionSyncEnabledValue() bool {
	return c.CLIVersionSyncEnabled == nil || *c.CLIVersionSyncEnabled
}

// NormalizeClaudeCLIVersionSyncIntervalHours 钳到 [1,720]，0/负数视为默认 12。
func NormalizeClaudeCLIVersionSyncIntervalHours(hours int) int {
	if hours <= 0 {
		return 12
	}
	if hours > 720 {
		return 720
	}
	return hours
}

func (s *Store) SetClaudeCLIVersionSync(enabled bool, intervalHours int) {
	if s == nil {
		return
	}
	s.claudeCLIVersionSyncDisabled.Store(!enabled)
	s.claudeCLIVersionSyncIntervalH.Store(int64(NormalizeClaudeCLIVersionSyncIntervalHours(intervalHours)))
}

func (s *Store) ClaudeCLIVersionSyncEnabled() bool {
	return s != nil && !s.claudeCLIVersionSyncDisabled.Load()
}

func (s *Store) ClaudeCLIVersionSyncIntervalHours() int {
	if s == nil {
		return 12
	}
	return NormalizeClaudeCLIVersionSyncIntervalHours(int(s.claudeCLIVersionSyncIntervalH.Load()))
}
```

`ParseClaudeConfig` 在 `cfg.SessionWindowLimit` 归一之后加：`cfg.CLIVersionSyncIntervalHours = NormalizeClaudeCLIVersionSyncIntervalHours(cfg.CLIVersionSyncIntervalHours)`。
`applyClaudeConfigToStore` 末尾加：`s.SetClaudeCLIVersionSync(cfg.CLIVersionSyncEnabledValue(), cfg.CLIVersionSyncIntervalHours)`。

- [ ] **Step 4: 运行测试**

Run: `go test ./auth/ -run 'ClaudeConfig|ClaudeCLIVersionSync|ClaudeFingerprintMode' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add auth/claude_fingerprint_mode.go auth/store.go auth/claude_fingerprint_mode_test.go
git commit -m "feat(auth): add Claude CLI version sync toggle and interval to ClaudeConfig

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 5: 数据库列与窄更新

**Files:**
- Modify: `database/sqlite.go:335-337`（CREATE TABLE）与 `:622-624`（增量列表）
- Modify: `database/postgres.go:1377-1379`
- Create: `database/claude_cli_version.go`
- Create: `database/claude_cli_version_test.go`

**Interfaces:**
- Produces:
  - `func (db *DB) GetClaudeSyncedCLIVersion(ctx context.Context) (string, error)`
  - `func (db *DB) UpdateClaudeSyncedCLIVersion(ctx context.Context, version string) error`
  - `func (db *DB) UpdateAccountCustomHeaders(ctx context.Context, id int64, headers map[string]string) error`（满足 `auth.ClaudeCustomHeadersPersister`）

- [ ] **Step 1: 写失败测试**

```go
// database/claude_cli_version_test.go
package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestClaudeSyncedCLIVersionRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "claude-cli-version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if got, err := db.GetClaudeSyncedCLIVersion(ctx); err != nil || got != "" {
		t.Fatalf("initial = %q, %v", got, err)
	}
	if err := db.UpdateClaudeSyncedCLIVersion(ctx, " 2.1.300 "); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetClaudeSyncedCLIVersion(ctx); got != "2.1.300" {
		t.Fatalf("after update = %q", got)
	}
	if _, err := db.GetSystemSettings(ctx); err != nil {
		t.Fatalf("narrow write must not break full settings read: %v", err)
	}
}

func TestUpdateAccountCustomHeadersReplacesOnlyHeaders(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "claude-headers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "claude-a", "anthropic", "oauth", map[string]interface{}{
		"upstream_type":  "claude",
		"access_token":   "tok",
		"custom_headers": map[string]interface{}{"User-Agent": "claude-cli/2.1.219 (external, cli)", "X-App": "cli"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAccountCustomHeaders(ctx, id, map[string]string{"User-Agent": "claude-cli/2.1.258 (external, cli)", "X-App": "cli"}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	headers := row.GetCredentialStringMap("custom_headers")
	if headers["User-Agent"] != "claude-cli/2.1.258 (external, cli)" || headers["X-App"] != "cli" {
		t.Fatalf("headers = %v", headers)
	}
	if row.Credentials["upstream_type"] != "claude" {
		t.Fatal("other credential fields must survive")
	}
	if err := db.UpdateAccountCustomHeaders(ctx, 999999, map[string]string{"User-Agent": "x"}); err == nil {
		t.Fatal("unknown account must error")
	}
}
```

若 `AccountRow` 没有 `GetCredentialStringMap` 方法（`auth/store.go:5158` 有调用，应当存在），用 `row.Credentials["custom_headers"].(map[string]interface{})` 断言替代。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./database/ -run 'TestClaudeSyncedCLIVersionRoundTrip|TestUpdateAccountCustomHeadersReplacesOnlyHeaders' -count=1`
Expected: FAIL，方法未定义。

- [ ] **Step 3: 实现**

`database/sqlite.go` CREATE TABLE 中 `codex_cli_version_sync_interval_hours INTEGER DEFAULT 12,` 之后加一行 `claude_synced_cli_version TEXT DEFAULT '',`；增量列表中 `{"system_settings", "codex_cli_version_sync_interval_hours", "INTEGER DEFAULT 12"},` 之后加 `{"system_settings", "claude_synced_cli_version", "TEXT DEFAULT ''"},`。
`database/postgres.go` 在 `ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_cli_version_sync_interval_hours INT DEFAULT 12;` 之后加 `ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS claude_synced_cli_version TEXT DEFAULT '';`。

```go
// database/claude_cli_version.go
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// GetClaudeSyncedCLIVersion 读取后台同步到的 Claude Code CLI 版本（空=尚未同步）。
func (db *DB) GetClaudeSyncedCLIVersion(ctx context.Context) (string, error) {
	if db == nil || db.conn == nil {
		return "", errors.New("database unavailable")
	}
	var version string
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(claude_synced_cli_version, '') FROM system_settings WHERE id = 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(version), nil
}

// UpdateClaudeSyncedCLIVersion 只更新同步版本单列，不回写整行设置。
func (db *DB) UpdateClaudeSyncedCLIVersion(ctx context.Context, version string) error {
	if db == nil || db.conn == nil {
		return errors.New("database unavailable")
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, claude_synced_cli_version) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET claude_synced_cli_version = EXCLUDED.claude_synced_cli_version`,
			strings.TrimSpace(version))
		return err
	})
}

// UpdateAccountCustomHeaders 整体替换账号 credentials.custom_headers，其余凭据字段不动，
// 不递增 credential_generation（指纹版本变化不是身份变化）。
func (db *DB) UpdateAccountCustomHeaders(ctx context.Context, id int64, headers map[string]string) error {
	if db == nil || db.conn == nil {
		return errors.New("database unavailable")
	}
	if id <= 0 {
		return fmt.Errorf("invalid account id %d", id)
	}
	normalized := make(map[string]interface{}, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		normalized[key] = strings.TrimSpace(value)
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		query := `SELECT credentials FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw interface{}
		if err := tx.QueryRowContext(ctx, query, id).Scan(&raw); err != nil {
			return err
		}
		merged := mergeCredentialMaps(cloneCredentialUpdates(decodeCredentials(raw)), map[string]interface{}{"custom_headers": normalized})
		credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
		if err != nil {
			return fmt.Errorf("序列化 credentials 失败: %w", err)
		}
		update := `UPDATE accounts SET credentials = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		if !db.isSQLite() {
			update = `UPDATE accounts SET credentials = $1::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		}
		if _, err := tx.ExecContext(ctx, update, credJSON, id); err != nil {
			return err
		}
		return tx.Commit()
	})
}
```

若 `cloneCredentialUpdates` 不在 `database` 包可见范围（它在 `postgres.go` 中被调用，应当存在），用 `mergeCredentialMaps(decodeCredentials(raw), ...)` 代替。

- [ ] **Step 4: 运行测试**

Run: `go test ./database/ -run 'TestClaudeSyncedCLIVersionRoundTrip|TestUpdateAccountCustomHeadersReplacesOnlyHeaders' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add database/sqlite.go database/postgres.go database/claude_cli_version.go database/claude_cli_version_test.go
git commit -m "feat(database): persist Claude synced CLI version and account custom headers

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 6: proxy 版本抓取、同步与后台任务

**Files:**
- Create: `proxy/claude_cli_version_sync.go`
- Create: `proxy/claude_cli_version_sync_test.go`

**Interfaces:**
- Consumes: `auth.EffectiveClaudeCLIVersion`、`auth.SetClaudeSyncedCLIVersion`、`auth.RefreshClaudeFingerprintVersions`、`auth.ClaudeCustomHeadersPersister`、`db.UpdateClaudeSyncedCLIVersion`、`store.ClaudeCLIVersionSyncEnabled/IntervalHours`、`ApplyGithubAuth`、`GithubProxyOrDefault`、`newCodexStandardTransport`、`db.RunBackgroundTask`。
- Produces:
  - `type ClaudeCLIVersionSyncResult struct { FetchedVersion, EffectiveVersion, BuiltinVersion string; Updated bool; AccountsRefreshed int }`（json `fetched_version`/`effective_version`/`builtin_version`/`updated`/`accounts_refreshed`）
  - `func ClaudeCLIVersionSyncDisabled() bool`
  - `func FetchLatestClaudeCLIVersion(ctx context.Context, proxyURL string) (string, error)`
  - `func SyncClaudeCLIVersion(ctx context.Context, db *database.DB, store *auth.Store, proxyURL string) (*ClaudeCLIVersionSyncResult, error)`
  - `func StartClaudeCLIVersionSync(ctx context.Context, db *database.DB, store *auth.Store, proxyResolver func() string)`
  - 测试接缝：`var claudeReleasesLatestURLForTest, claudeNpmDistTagsURLForTest string`

- [ ] **Step 1: 写失败测试**

```go
// proxy/claude_cli_version_sync_test.go
package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
)

func withClaudeVersionSources(t *testing.T, github, npm string) {
	t.Helper()
	claudeReleasesLatestURLForTest = github
	claudeNpmDistTagsURLForTest = npm
	t.Cleanup(func() {
		claudeReleasesLatestURLForTest = ""
		claudeNpmDistTagsURLForTest = ""
	})
}

func TestExtractClaudeCLIVersion(t *testing.T) {
	cases := map[string]string{"v2.1.258": "2.1.258", "2.1.258": "2.1.258", " V2.1.259 ": "2.1.259", "2.1.260-beta.1": "2.1.260", "rust-v0.1.0": "", "": "", "2.1": ""}
	for in, want := range cases {
		if got := extractClaudeCLIVersion(in); got != want {
			t.Errorf("extract(%q)=%q want %q", in, got, want)
		}
	}
}

func TestFetchLatestClaudeCLIVersion_PrefersGithub(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.258","tag_name":"v2.1.258"}`))
	}))
	defer gh.Close()
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"latest":"2.1.999"}`))
	}))
	defer npm.Close()
	withClaudeVersionSources(t, gh.URL, npm.URL)
	got, err := FetchLatestClaudeCLIVersion(context.Background(), "")
	if err != nil || got != "2.1.258" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchLatestClaudeCLIVersion_FallsBackToNpm(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer gh.Close()
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"stable":"2.1.236","latest":"2.1.258","next":"2.1.258"}`))
	}))
	defer npm.Close()
	withClaudeVersionSources(t, gh.URL, npm.URL)
	got, err := FetchLatestClaudeCLIVersion(context.Background(), "")
	if err != nil || got != "2.1.258" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchLatestClaudeCLIVersion_BothFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer bad.Close()
	withClaudeVersionSources(t, bad.URL, bad.URL)
	if _, err := FetchLatestClaudeCLIVersion(context.Background(), ""); err == nil {
		t.Fatal("expected error when both sources fail")
	}
}

func TestSyncClaudeCLIVersion_RefreshesFingerprintsWithoutDB(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.300"}`))
	}))
	defer gh.Close()
	withClaudeVersionSources(t, gh.URL, gh.URL)
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	store.SetAccountsForTest([]*auth.Account{{DBID: 251, UpstreamType: auth.UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}})

	result, err := SyncClaudeCLIVersion(context.Background(), nil, store, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.EffectiveVersion != "2.1.300" || result.FetchedVersion != "2.1.300" || result.BuiltinVersion != auth.BuiltinClaudeCLIVersion {
		t.Fatalf("result = %+v", result)
	}
	if result.AccountsRefreshed != 1 {
		t.Fatalf("accounts_refreshed = %d", result.AccountsRefreshed)
	}
	if auth.EffectiveClaudeCLIVersion() != "2.1.300" {
		t.Fatal("runtime effective version must be published")
	}
}
```

`store.SetAccountsForTest` 目前不存在。在 `auth/store.go` 中 `Accounts()` 定义之后新增（仅测试用途，放 `auth/testing_helpers.go` 新文件，非 `_test.go`，因为要被 `proxy` 包测试调用）：

```go
// auth/testing_helpers.go
package auth

// SetAccountsForTest 直接替换内存账号列表，仅供其它包的测试使用。
func (s *Store) SetAccountsForTest(accounts []*Account) {
	s.mu.Lock()
	s.accounts = append([]*Account(nil), accounts...)
	s.mu.Unlock()
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./proxy/ -run 'ClaudeCLIVersion' -count=1`
Expected: FAIL，符号未定义。

- [ ] **Step 3: 实现**

```go
// proxy/claude_cli_version_sync.go
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const (
	// ClaudeReleasesLatestURL 是 anthropics/claude-code 最新正式 release 的 GitHub API 端点。
	ClaudeReleasesLatestURL = "https://api.github.com/repos/anthropics/claude-code/releases/latest"
	// ClaudeNpmDistTagsURL 是 npm 上 @anthropic-ai/claude-code 的 dist-tags 端点（GitHub 失败时回退）。
	ClaudeNpmDistTagsURL = "https://registry.npmjs.org/-/package/@anthropic-ai/claude-code/dist-tags"
)

// 测试接缝；生产代码不要赋值。
var (
	claudeReleasesLatestURLForTest = ""
	claudeNpmDistTagsURLForTest    = ""
)

// ClaudeCLIVersionSyncDisabled 报告是否通过 CLAUDE_DISABLE_CLI_VERSION_SYNC 关闭了联网同步。
// 关闭后仍会在启动时用内置版本做一次本地指纹回写；管理端「立即同步」不受影响。
func ClaudeCLIVersionSyncDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_DISABLE_CLI_VERSION_SYNC"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ClaudeCLIVersionSyncResult 是一次同步的结果投影。
type ClaudeCLIVersionSyncResult struct {
	FetchedVersion    string `json:"fetched_version"`
	EffectiveVersion  string `json:"effective_version"`
	BuiltinVersion    string `json:"builtin_version"`
	Updated           bool   `json:"updated"`
	AccountsRefreshed int    `json:"accounts_refreshed"`
}

// extractClaudeCLIVersion 接受 "2.1.258" / "v2.1.258"，丢弃预发布后缀；非法返回空串。
func extractClaudeCLIVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V")
	if idx := strings.IndexAny(raw, "-+"); idx >= 0 {
		raw = raw[:idx]
	}
	if raw == "" {
		return ""
	}
	version, ok := auth.ParseClaudeClientVersion("claude-cli/" + raw)
	if !ok {
		return ""
	}
	return version
}

func fetchClaudeJSON(ctx context.Context, endpoint string, transport http.RoundTripper, github bool, out interface{}) error {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex2api")
	if github {
		req.Header.Set("Accept", "application/vnd.github+json")
		ApplyGithubAuth(req)
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func fetchClaudeVersionFromGithub(ctx context.Context, proxyURL string) (string, error) {
	endpoint := ClaudeReleasesLatestURL
	if claudeReleasesLatestURLForTest != "" {
		endpoint = claudeReleasesLatestURLForTest
	}
	var payload struct {
		Name    string `json:"name"`
		TagName string `json:"tag_name"`
	}
	if err := fetchClaudeJSON(ctx, endpoint, newCodexStandardTransport(GithubProxyOrDefault(endpoint, proxyURL)), true, &payload); err != nil {
		return "", err
	}
	if v := extractClaudeCLIVersion(payload.TagName); v != "" {
		return v, nil
	}
	if v := extractClaudeCLIVersion(payload.Name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no valid version in release (name=%q tag=%q)", payload.Name, payload.TagName)
}

func fetchClaudeVersionFromNpm(ctx context.Context, proxyURL string) (string, error) {
	endpoint := ClaudeNpmDistTagsURL
	if claudeNpmDistTagsURLForTest != "" {
		endpoint = claudeNpmDistTagsURLForTest
	}
	var payload struct {
		Latest string `json:"latest"`
	}
	if err := fetchClaudeJSON(ctx, endpoint, newCodexStandardTransport(proxyURL), false, &payload); err != nil {
		return "", err
	}
	if v := extractClaudeCLIVersion(payload.Latest); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no valid version in dist-tags (latest=%q)", payload.Latest)
}

// FetchLatestClaudeCLIVersion 先查 GitHub releases/latest，失败再查 npm dist-tags。
func FetchLatestClaudeCLIVersion(ctx context.Context, proxyURL string) (string, error) {
	version, ghErr := fetchClaudeVersionFromGithub(ctx, proxyURL)
	if ghErr == nil {
		return version, nil
	}
	version, npmErr := fetchClaudeVersionFromNpm(ctx, proxyURL)
	if npmErr == nil {
		return version, nil
	}
	return "", fmt.Errorf("claude cli version fetch failed: github: %v; npm: %v", ghErr, npmErr)
}

func claudeHeadersPersister(db *database.DB) auth.ClaudeCustomHeadersPersister {
	if db == nil {
		return nil // 必须返回接口 nil，而不是 nil 指针
	}
	return db
}

// SyncClaudeCLIVersion 拉取最新版本，高于当前生效版本时持久化并发布，随后回写所有账号指纹。
func SyncClaudeCLIVersion(ctx context.Context, db *database.DB, store *auth.Store, proxyURL string) (*ClaudeCLIVersionSyncResult, error) {
	result := &ClaudeCLIVersionSyncResult{
		BuiltinVersion:   auth.BuiltinClaudeCLIVersion,
		EffectiveVersion: auth.EffectiveClaudeCLIVersion(),
	}
	fetched, err := FetchLatestClaudeCLIVersion(ctx, proxyURL)
	if err != nil {
		return result, err
	}
	result.FetchedVersion = fetched
	if cmp, cmpErr := auth.CompareClaudeClientVersions(fetched, result.EffectiveVersion); cmpErr == nil && cmp > 0 {
		if db != nil {
			if err := db.UpdateClaudeSyncedCLIVersion(ctx, fetched); err != nil {
				return result, err
			}
		}
		auth.SetClaudeSyncedCLIVersion(fetched)
		result.Updated = true
	}
	result.EffectiveVersion = auth.EffectiveClaudeCLIVersion()
	refreshed, refreshErr := auth.RefreshClaudeFingerprintVersions(ctx, store, claudeHeadersPersister(db), result.EffectiveVersion)
	result.AccountsRefreshed = refreshed
	return result, refreshErr
}

// StartClaudeCLIVersionSync 启动时先用生效版本做一次本地指纹回写（不联网），
// 然后按 ClaudeConfig 的开关与间隔定时联网同步。
func StartClaudeCLIVersionSync(ctx context.Context, db *database.DB, store *auth.Store, proxyResolver func() string) {
	if db == nil || store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	{
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if n, err := auth.RefreshClaudeFingerprintVersions(refreshCtx, store, db, auth.EffectiveClaudeCLIVersion()); err != nil {
			log.Printf("[claude-cli-version-sync] 启动指纹版本回写部分失败: %v", err)
		} else if n > 0 {
			log.Printf("[claude-cli-version-sync] 启动时已回写 %d 个 Claude 账号指纹版本至 %s", n, auth.EffectiveClaudeCLIVersion())
		}
		cancel()
	}
	if ClaudeCLIVersionSyncDisabled() {
		return
	}
	resolveProxy := func() string {
		if proxyResolver == nil {
			return ""
		}
		return proxyResolver()
	}
	runOnce := func(runCtx context.Context) {
		syncCtx, cancel := context.WithTimeout(runCtx, 45*time.Second)
		defer cancel()
		res, err := SyncClaudeCLIVersion(syncCtx, db, store, resolveProxy())
		if err != nil {
			log.Printf("[claude-cli-version-sync] 同步失败（不影响服务）: %v", err)
			return
		}
		if res.Updated || res.AccountsRefreshed > 0 {
			log.Printf("[claude-cli-version-sync] 生效版本 %s，回写账号 %d 个", res.EffectiveVersion, res.AccountsRefreshed)
		}
	}
	currentInterval := func() time.Duration {
		return time.Duration(store.ClaudeCLIVersionSyncIntervalHours()) * time.Hour
	}
	db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, taskCancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(ctx, taskCancel)
		defer func() {
			stopParent()
			taskCancel()
		}()
		if store.ClaudeCLIVersionSyncEnabled() {
			runOnce(taskCtx)
		}
		for {
			select {
			case <-taskCtx.Done():
				return
			case <-time.After(currentInterval()):
				if store.ClaudeCLIVersionSyncEnabled() {
					runOnce(taskCtx)
				}
			}
		}
	})
}
```

- [ ] **Step 4: 运行测试**

Run: `go test ./proxy/ ./auth/ -run 'ClaudeCLIVersion|ClaudeFingerprint' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add proxy/claude_cli_version_sync.go proxy/claude_cli_version_sync_test.go auth/testing_helpers.go
git commit -m "feat(proxy): sync latest Claude Code CLI version and refresh fingerprints

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 7: 出站 UA 版本对齐（故障直接修复）

**Files:**
- Modify: `proxy/claude_upstream.go:135-207`
- Modify: `proxy/claude_upstream_test.go`

**Interfaces:**
- Consumes: `auth.ParseClaudeClientVersion`、`auth.CompareClaudeClientVersions`、`auth.EffectiveClaudeCLIVersion`、`auth.RewriteClaudeCLIUserAgentVersion`、`auth.ClaudeModelMinimumVersion`、`RecordUpstreamUserAgent`。
- Produces: `func alignClaudeOutboundUserAgent(outbound, required string) (finalUA string, denyMessage string)`。

- [ ] **Step 1: 运行影响分析**

按项目规范先执行 `gitnexus_impact({target: "ExecuteClaudeMessagesRequestWithPolicy", direction: "upstream"})`，把直接调用方与风险级别记录到提交说明；HIGH/CRITICAL 时先告知用户。

- [ ] **Step 2: 写失败测试**

追加到 `proxy/claude_upstream_test.go`：

```go
func TestAlignClaudeOutboundUserAgent(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("")
	cases := []struct {
		name, outbound, required, wantUA string
		wantDeny                         bool
	}{
		{"no requirement", "claude-cli/2.1.219 (external, cli)", "", "claude-cli/2.1.219 (external, cli)", false},
		{"already satisfied", "claude-cli/2.1.258 (external, cli)", "2.1.251", "claude-cli/2.1.258 (external, cli)", false},
		{"stale fingerprint bumped to effective", "claude-cli/2.1.219 (external, cli)", "2.1.251", "claude-cli/" + auth.BuiltinClaudeCLIVersion + " (external, cli)", false},
		{"non-cli untouched", "Go-http-client/1.1", "2.1.251", "Go-http-client/1.1", false},
		{"effective still too old", "claude-cli/2.1.219 (external, cli)", "9.9.9", "claude-cli/2.1.219 (external, cli)", true},
	}
	for _, tc := range cases {
		gotUA, deny := alignClaudeOutboundUserAgent(tc.outbound, tc.required)
		if gotUA != tc.wantUA || (deny != "") != tc.wantDeny {
			t.Errorf("%s: ua=%q deny=%q", tc.name, gotUA, deny)
		}
	}
}

func TestExecuteClaudeMessagesRequestWithPolicy_DeniesWhenForcedFingerprintTooOld(t *testing.T) {
	ctx := withUserAgentAudit(context.Background())
	account := &auth.Account{DBID: 251, UpstreamType: auth.UpstreamClaude, AccessToken: "tok", CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/9.9.9 (external, cli)")
	policy := auth.ClaudeClientPolicy{Platform: auth.ClaudeClientPlatformAny, VersionPolicy: auth.ClaudeVersionPolicyMinimum, ClientVersion: "9.9.9"}
	_, err := ExecuteClaudeMessagesRequestWithPolicy(ctx, account, []byte(`{"model":"claude-opus-5","messages":[]}`), "", headers, "force", policy)
	var perr *Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusUpgradeRequired || perr.Code != "claude_client_policy" {
		t.Fatalf("expected local 426 claude_client_policy, got %v", err)
	}
	if !strings.Contains(perr.Message, "2.1.219") || !strings.Contains(perr.Message, "9.9.9") {
		t.Fatalf("message should name outbound and required versions: %s", perr.Message)
	}
}

func TestExecuteClaudeMessagesRequestWithPolicy_UsesModelFloorForNonCLIInbound(t *testing.T) {
	// 入站不是 CLI（无 required），但 force 指纹是旧 CLI UA 且模型有下限：出站仍需对齐。
	gotUA, deny := alignClaudeOutboundUserAgent("claude-cli/2.1.219 (external, cli)", claudeOutboundRequiredVersion(auth.ClaudeClientDecision{}, "claude-fable-5-1"))
	if deny != "" || !strings.Contains(gotUA, auth.BuiltinClaudeCLIVersion) {
		t.Fatalf("ua=%q deny=%q", gotUA, deny)
	}
}
```

确认测试文件已 import `errors`、`strings`、`net/http`、`context`；缺少则补上。

- [ ] **Step 3: 运行确认失败**

Run: `go test ./proxy/ -run 'TestAlignClaudeOutboundUserAgent|TestExecuteClaudeMessagesRequestWithPolicy_' -count=1`
Expected: FAIL，`alignClaudeOutboundUserAgent` 未定义。

- [ ] **Step 4: 实现**

在 `proxy/claude_upstream.go` 新增两个函数（放在 `applyClaudeMessagesHeadersWithVersion` 之后）：

```go
// claudeOutboundRequiredVersion 取入站门控得出的 required 与模型下限中的较大者。
// 入站非 CLI 时 decision.RequiredVersion 为空，但 force 指纹可能把出站改成 CLI UA，
// 此时仍必须遵守模型下限。
func claudeOutboundRequiredVersion(decision auth.ClaudeClientDecision, model string) string {
	required := strings.TrimSpace(decision.RequiredVersion)
	floor := auth.ClaudeModelMinimumVersion(model)
	if floor == "" {
		return required
	}
	if required == "" {
		return floor
	}
	if cmp, err := auth.CompareClaudeClientVersions(floor, required); err == nil && cmp > 0 {
		return floor
	}
	return required
}

// alignClaudeOutboundUserAgent 保证最终出站 CLI UA 版本不低于 required。
// 低于时抬到生效版本；生效版本仍不够则返回拒绝消息（调用方本地 426，不发上游）。
func alignClaudeOutboundUserAgent(outbound, required string) (string, string) {
	if strings.TrimSpace(required) == "" {
		return outbound, ""
	}
	outVersion, isCLI := auth.ParseClaudeClientVersion(outbound)
	if !isCLI {
		return outbound, ""
	}
	if cmp, err := auth.CompareClaudeClientVersions(outVersion, required); err != nil || cmp >= 0 {
		return outbound, ""
	}
	effective := auth.EffectiveClaudeCLIVersion()
	if cmp, err := auth.CompareClaudeClientVersions(effective, required); err != nil || cmp < 0 {
		return outbound, fmt.Sprintf("Claude Code CLI outbound version %s is below required %s (effective %s); update client_version or wait for CLI version sync", outVersion, required, effective)
	}
	rewritten := auth.RewriteClaudeCLIUserAgentVersion(outbound, effective)
	if rewritten == "" {
		return outbound, ""
	}
	return rewritten, ""
}
```

在 `ExecuteClaudeMessagesRequestWithPolicy` 中，`applyClaudeMessagesHeadersWithVersion(...)` 调用之后、`client.Do(req)` 之前插入：

```go
	if finalUA, deny := alignClaudeOutboundUserAgent(req.Header.Get("User-Agent"), claudeOutboundRequiredVersion(decision, model)); deny != "" {
		return nil, &Error{Code: "claude_client_policy", Message: deny, Type: ErrorTypeInvalidRequest, Retryable: false, HTTPStatus: http.StatusUpgradeRequired}
	} else if finalUA != req.Header.Get("User-Agent") {
		req.Header.Set("User-Agent", finalUA)
		RecordUpstreamUserAgent(req.Context(), finalUA)
	}
```

- [ ] **Step 5: 运行测试**

Run: `go test ./proxy/ -run 'Claude' -count=1`
Expected: PASS，含既有 `TestApplyClaudeMessagesHeaders*`。

- [ ] **Step 6: 提交**

```bash
git add proxy/claude_upstream.go proxy/claude_upstream_test.go
git commit -m "fix(proxy): align forced Claude fingerprint UA version with model floor before upstream

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 8: 管理端接口与启动接线

**Files:**
- Modify: `admin/claude_config.go`
- Modify: `admin/handler.go:1185-1186`（路由）
- Modify: `admin/claude_config_test.go`
- Modify: `main.go:312-352`

**Interfaces:**
- Consumes: Task 4 Store 访问器、Task 6 `proxy.SyncClaudeCLIVersion/StartClaudeCLIVersionSync`、Task 5 `db.GetClaudeSyncedCLIVersion`、`auth.SetClaudeSyncedCLIVersion`。
- Produces:
  - `GET /settings/claude-config` 新返回 `cli_version_sync_enabled`、`cli_version_sync_interval_hours`、`synced_cli_version`、`builtin_cli_version`、`effective_cli_version`。
  - `PUT /settings/claude-config` 接受前两者，忽略后三者。
  - `POST /settings/claude-config/cli-version/sync` → `ClaudeCLIVersionSyncResult`。

- [ ] **Step 1: 写失败测试**

追加到 `admin/claude_config_test.go`：

```go
func TestGetClaudeConfigExposesCLIVersionSyncState(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("2.1.300")
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	h := &Handler{store: store}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h.GetClaudeConfig(c)
	body := recorder.Body.Bytes()
	if !gjson.GetBytes(body, "cli_version_sync_enabled").Bool() {
		t.Fatal("cli_version_sync_enabled should default true")
	}
	if got := gjson.GetBytes(body, "cli_version_sync_interval_hours").Int(); got != 12 {
		t.Fatalf("interval = %d", got)
	}
	if got := gjson.GetBytes(body, "synced_cli_version").String(); got != "2.1.300" {
		t.Fatalf("synced = %q", got)
	}
	if got := gjson.GetBytes(body, "builtin_cli_version").String(); got != auth.BuiltinClaudeCLIVersion {
		t.Fatalf("builtin = %q", got)
	}
	if got := gjson.GetBytes(body, "effective_cli_version").String(); got != "2.1.300" {
		t.Fatalf("effective = %q", got)
	}
}

func TestUpdateClaudeConfigPersistsCLIVersionSyncFields(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	h := &Handler{store: store, db: newClaudeConfigTestDB(t)}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("PUT", "/settings/claude-config", strings.NewReader(`{"fingerprint_mode":"force","cli_version_sync_enabled":false,"cli_version_sync_interval_hours":48,"synced_cli_version":"9.9.9"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateClaudeConfig(c)
	if recorder.Code != 200 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if store.ClaudeCLIVersionSyncEnabled() || store.ClaudeCLIVersionSyncIntervalHours() != 48 {
		t.Fatalf("store not updated: enabled=%v hours=%d", store.ClaudeCLIVersionSyncEnabled(), store.ClaudeCLIVersionSyncIntervalHours())
	}
	if auth.ClaudeSyncedCLIVersion() == "9.9.9" {
		t.Fatal("PUT must ignore read-only synced_cli_version")
	}
	settings, err := h.db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg := auth.ParseClaudeConfig(settings.ClaudeConfig)
	if cfg.CLIVersionSyncEnabledValue() || cfg.CLIVersionSyncIntervalHours != 48 {
		t.Fatalf("persisted cfg = %+v", cfg)
	}
}
```

若文件里还没有 `newClaudeConfigTestDB`，在同文件加：

```go
func newClaudeConfigTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
```

并 import `path/filepath` 与 `github.com/codex2api/database`。若 `Handler.db` 字段类型不是 `*database.DB`，按 `admin/handler.go` 中 `db` 字段的实际类型调整。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./admin/ -run 'ClaudeConfig' -count=1`
Expected: FAIL，JSON 字段缺失。

- [ ] **Step 3: 实现**

`admin/claude_config.go`：

```go
// claudeGlobalConfigDTO 增加字段（放在 SessionWindowLimit 之后）：
	CLIVersionSyncEnabled       *bool  `json:"cli_version_sync_enabled"`
	CLIVersionSyncIntervalHours int    `json:"cli_version_sync_interval_hours"`
	// 以下三项只读；PUT 忽略。
	SyncedCLIVersion    string `json:"synced_cli_version"`
	BuiltinCLIVersion   string `json:"builtin_cli_version"`
	EffectiveCLIVersion string `json:"effective_cli_version"`
```

`GetClaudeConfig` 返回体增加：

```go
		CLIVersionSyncEnabled:       boolPtr(h.store.ClaudeCLIVersionSyncEnabled()),
		CLIVersionSyncIntervalHours: h.store.ClaudeCLIVersionSyncIntervalHours(),
		SyncedCLIVersion:            auth.ClaudeSyncedCLIVersion(),
		BuiltinCLIVersion:           auth.BuiltinClaudeCLIVersion,
		EffectiveCLIVersion:         auth.EffectiveClaudeCLIVersion(),
```

文件底部加 `func boolPtr(v bool) *bool { return &v }`（若包内已有同名函数则复用）。

`UpdateClaudeConfig` 中 `security := ...` 之后加：

```go
	syncEnabled := req.CLIVersionSyncEnabled == nil || *req.CLIVersionSyncEnabled
	syncInterval := auth.NormalizeClaudeCLIVersionSyncIntervalHours(req.CLIVersionSyncIntervalHours)
```

`cfg := auth.ClaudeConfig{...}` 增加 `CLIVersionSyncEnabled: boolPtr(syncEnabled), CLIVersionSyncIntervalHours: syncInterval,`；热更新段加 `h.store.SetClaudeCLIVersionSync(syncEnabled, syncInterval)`；响应 `gin.H` 增加 `"cli_version_sync_enabled": syncEnabled, "cli_version_sync_interval_hours": syncInterval`。

新增 handler（同文件末尾，需 import `context`、`github.com/codex2api/proxy`）：

```go
// SyncClaudeCLIVersion 供设置页「立即同步」调用：拉取最新 Claude Code CLI 版本并回写账号指纹。
func (h *Handler) SyncClaudeCLIVersion(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	result, err := proxy.SyncClaudeCLIVersion(ctx, h.db, h.store, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}
```

`admin/handler.go` 在 `api.PUT("/settings/claude-config", h.UpdateClaudeConfig)` 之后加 `api.POST("/settings/claude-config/cli-version/sync", h.SyncClaudeCLIVersion)`。

`main.go`：在 `store := auth.NewStore(db, tc, settings)` 之前加：

```go
	// Claude CLI 同步版本先于账号加载发布，保证 GenerateClaudeFingerprint 与回写使用同一生效版本。
	if synced, err := db.GetClaudeSyncedCLIVersion(sysCtx); err == nil {
		auth.SetClaudeSyncedCLIVersion(synced)
	} else {
		log.Printf("读取 Claude CLI 同步版本失败（使用内置 %s）: %v", auth.BuiltinClaudeCLIVersion, err)
	}
```

若 `sysCtx` 在该处已 cancel，改用 `context.Background()`。在 `proxy.StartCodexCLIVersionSync(backgroundCtx, db, store.GetProxyURL)` 之后加：

```go
	// Claude Code CLI 版本同步：启动先用生效版本回写账号指纹，再按 ClaudeConfig 开关/间隔联网同步。
	proxy.StartClaudeCLIVersionSync(backgroundCtx, db, store, store.GetProxyURL)
```

- [ ] **Step 4: 运行测试与编译**

Run: `go build ./... && go test ./admin/ -run 'ClaudeConfig' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add admin/claude_config.go admin/claude_config_test.go admin/handler.go main.go
git commit -m "feat(admin): expose Claude CLI version sync settings and manual sync endpoint

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 9: 前端类型、API 与文案

**Files:**
- Modify: `frontend/src/types.ts:3739-3754`
- Modify: `frontend/src/api.ts:1211-1218`
- Modify: `frontend/src/locales/zh.json`、`en.json`、`zh-TW.json`（`settings` 命名空间，`claudeVersionPolicyMinimum` 之后）

**Interfaces:**
- Produces:
  - `ClaudeGlobalConfig` 新字段 `cli_version_sync_enabled: boolean; cli_version_sync_interval_hours: number; synced_cli_version?: string; builtin_cli_version?: string; effective_cli_version?: string`
  - `api.syncClaudeCLIVersion(): Promise<{ fetched_version: string; effective_version: string; builtin_version: string; updated: boolean; accounts_refreshed: number }>`
  - i18n key：`settings.claudeCliVersionSync`、`claudeCliVersionSyncDesc`、`claudeCliVersionSyncNow`、`claudeCliVersionSyncing`、`claudeCliVersionSyncSuccess`、`claudeCliVersionSyncFailed`、`claudeCliVersionAutoSync`、`claudeCliVersionAutoSyncDesc`、`claudeCliVersionSyncInterval`、`claudeCliVersionSyncIntervalDesc`、`claudeCliVersionBuiltin`

- [ ] **Step 1: 写失败守卫测试**

追加到 `frontend/src/lib/claudeParity.test.mjs`：

```js
test('Claude settings expose CLI version sync controls and typed API', () => {
  const api = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
  assert.match(types, /cli_version_sync_enabled: boolean/)
  assert.match(types, /cli_version_sync_interval_hours: number/)
  assert.match(types, /synced_cli_version\?: string/)
  assert.match(api, /syncClaudeCLIVersion: \(\) =>/)
  assert.match(api, /\/settings\/claude-config\/cli-version\/sync/)
  for (const key of ['claudeCliVersionSync', 'claudeCliVersionSyncNow', 'claudeCliVersionSyncSuccess', 'claudeCliVersionAutoSync', 'claudeCliVersionSyncInterval']) {
    assert.equal(typeof zh.settings?.[key], 'string', `zh.settings.${key}`)
  }
  const en = JSON.parse(readFileSync(new URL('../locales/en.json', import.meta.url), 'utf8'))
  const tw = JSON.parse(readFileSync(new URL('../locales/zh-TW.json', import.meta.url), 'utf8'))
  for (const locale of [en, tw]) {
    assert.equal(typeof locale.settings?.claudeCliVersionSyncNow, 'string')
  }
})
```

- [ ] **Step 2: 运行确认失败**

Run: `cd frontend && node --experimental-strip-types --test src/lib/claudeParity.test.mjs`
Expected: FAIL。

- [ ] **Step 3: 实现**

`types.ts` `ClaudeGlobalConfig` 在 `session_window_limit: number` 之后加：

```ts
  cli_version_sync_enabled: boolean
  cli_version_sync_interval_hours: number
  synced_cli_version?: string
  builtin_cli_version?: string
  effective_cli_version?: string
```

`api.ts` 在 `updateClaudeConfig` 之后加：

```ts
  syncClaudeCLIVersion: () =>
    request<{
      fetched_version: string
      effective_version: string
      builtin_version: string
      updated: boolean
      accounts_refreshed: number
    }>('/settings/claude-config/cli-version/sync', { method: 'POST' }),
```

`zh.json`（`"claudeVersionPolicyMinimum": "最低版本门控",` 之后）：

```json
    "claudeCliVersionSync": "Claude Code CLI 版本同步",
    "claudeCliVersionSyncDesc": "从 GitHub releases（回退 npm）获取最新 Claude Code 版本，并把所有 Claude 账号指纹的 UA 版本抬到该版本。",
    "claudeCliVersionSyncNow": "立即同步",
    "claudeCliVersionSyncing": "同步中…",
    "claudeCliVersionSyncSuccess": "生效版本 {{version}}，已回写 {{accounts}} 个账号指纹",
    "claudeCliVersionSyncFailed": "Claude Code 版本同步失败",
    "claudeCliVersionAutoSync": "自动同步",
    "claudeCliVersionAutoSyncDesc": "开启后按间隔自动同步；关闭后仅在启动时用内置版本回写指纹。",
    "claudeCliVersionSyncInterval": "同步间隔（小时）",
    "claudeCliVersionSyncIntervalDesc": "两次自动同步之间的等待时长（小时，范围 1-720）。",
    "claudeCliVersionBuiltin": "内置",
```

`en.json`：

```json
    "claudeCliVersionSync": "Claude Code CLI version sync",
    "claudeCliVersionSyncDesc": "Fetch the latest Claude Code version from GitHub releases (npm fallback) and raise every Claude account fingerprint UA to it.",
    "claudeCliVersionSyncNow": "Sync now",
    "claudeCliVersionSyncing": "Syncing…",
    "claudeCliVersionSyncSuccess": "Effective version {{version}}, refreshed {{accounts}} account fingerprints",
    "claudeCliVersionSyncFailed": "Claude Code version sync failed",
    "claudeCliVersionAutoSync": "Auto sync",
    "claudeCliVersionAutoSyncDesc": "When on, syncs on the configured interval; when off, only the built-in version is applied at startup.",
    "claudeCliVersionSyncInterval": "Sync interval (hours)",
    "claudeCliVersionSyncIntervalDesc": "Wait time between automatic syncs (hours, range 1-720).",
    "claudeCliVersionBuiltin": "built-in",
```

`zh-TW.json`：

```json
    "claudeCliVersionSync": "Claude Code CLI 版本同步",
    "claudeCliVersionSyncDesc": "從 GitHub releases（回退 npm）取得最新 Claude Code 版本，並把所有 Claude 帳號指紋的 UA 版本抬到該版本。",
    "claudeCliVersionSyncNow": "立即同步",
    "claudeCliVersionSyncing": "同步中…",
    "claudeCliVersionSyncSuccess": "生效版本 {{version}}，已回寫 {{accounts}} 個帳號指紋",
    "claudeCliVersionSyncFailed": "Claude Code 版本同步失敗",
    "claudeCliVersionAutoSync": "自動同步",
    "claudeCliVersionAutoSyncDesc": "開啟後按間隔自動同步；關閉後僅在啟動時用內建版本回寫指紋。",
    "claudeCliVersionSyncInterval": "同步間隔（小時）",
    "claudeCliVersionSyncIntervalDesc": "兩次自動同步之間的等待時長（小時，範圍 1-720）。",
    "claudeCliVersionBuiltin": "內建",
```

- [ ] **Step 4: 运行测试**

Run: `cd frontend && node --experimental-strip-types --test src/lib/claudeParity.test.mjs && npm run typecheck`
Expected: PASS（typecheck 会因 Settings.tsx 尚未传新字段给 `updateClaudeConfig` 而报错——若报错，在 Task 10 一起解决；此时只提交 types/api/locales 与测试）。

- [ ] **Step 5: 提交**

```bash
git add frontend/src/types.ts frontend/src/api.ts frontend/src/locales/zh.json frontend/src/locales/en.json frontend/src/locales/zh-TW.json frontend/src/lib/claudeParity.test.mjs
git commit -m "feat(frontend): add Claude CLI version sync types, API, and copy

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 10: Settings ClaudeCode 卡片：共享 Select 与同步区块

**Files:**
- Modify: `frontend/src/pages/Settings.tsx:703-900`（`ClaudeCodeSettingsCard`）
- Modify: `frontend/src/lib/claudeParity.test.mjs`

**Interfaces:**
- Consumes: Task 9 类型/API/文案；已在文件内导入的 `Select`、`Switch`、`DraftNumberInput`、`SettingHelp`、`Button`、`RefreshCw`、`cn`。

- [ ] **Step 1: 写失败守卫测试**

追加到 `claudeParity.test.mjs`：

```js
test('Claude settings card uses the shared Select and renders CLI version sync block', () => {
  const start = settings.indexOf('function ClaudeCodeSettingsCard')
  const end = settings.indexOf('\nfunction SettingsCard', start)
  const card = settings.slice(start, end)
  assert.doesNotMatch(card, /<select[\s>]/)
  assert.doesNotMatch(card, /selectCls/)
  assert.ok((card.match(/<Select\b/g) || []).length >= 4, 'fingerprint/platform/policy/timezone must all use <Select>')
  assert.match(card, /api\.syncClaudeCLIVersion\(\)/)
  assert.match(card, /claudeCliVersionSyncNow/)
  assert.match(card, /cli_version_sync_enabled: cliVersionSyncEnabled/)
  assert.match(card, /cli_version_sync_interval_hours: cliVersionSyncIntervalHours/)
  assert.match(card, /<DraftNumberInput[\s\S]*?min=\{1\}[\s\S]*?max=\{720\}/)
})
```

- [ ] **Step 2: 运行确认失败**

Run: `cd frontend && node --experimental-strip-types --test src/lib/claudeParity.test.mjs`
Expected: FAIL。

- [ ] **Step 3: 实现**

在 `ClaudeCodeSettingsCard` 的 state 区加：

```tsx
  const [cliVersionSyncEnabled, setCliVersionSyncEnabled] = useState(true)
  const [cliVersionSyncIntervalHours, setCliVersionSyncIntervalHours] = useState(12)
  const [syncedCliVersion, setSyncedCliVersion] = useState('')
  const [effectiveCliVersion, setEffectiveCliVersion] = useState('')
  const [syncingCliVersion, setSyncingCliVersion] = useState(false)
```

`useEffect` 加载回调里加：

```tsx
        setCliVersionSyncEnabled(cfg.cli_version_sync_enabled ?? true)
        setCliVersionSyncIntervalHours(cfg.cli_version_sync_interval_hours || 12)
        setSyncedCliVersion(cfg.synced_cli_version ?? '')
        setEffectiveCliVersion(cfg.effective_cli_version ?? cfg.builtin_cli_version ?? '')
```

`save` 的 `updateClaudeConfig` 参数加 `cli_version_sync_enabled: cliVersionSyncEnabled, cli_version_sync_interval_hours: cliVersionSyncIntervalHours,`，并把两者加入 `useCallback` 依赖数组。

新增 handler（放在 `save` 之后）：

```tsx
  const handleSyncClaudeCliVersion = useCallback(async () => {
    setSyncingCliVersion(true)
    try {
      const result = await api.syncClaudeCLIVersion()
      setSyncedCliVersion(result.fetched_version || result.effective_version)
      setEffectiveCliVersion(result.effective_version)
      showToast(t('settings.claudeCliVersionSyncSuccess', { version: result.effective_version, accounts: result.accounts_refreshed }), 'success')
    } catch (error) {
      showToast(`${t('settings.claudeCliVersionSyncFailed')}: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSyncingCliVersion(false)
    }
  }, [showToast, t])
```

删除 `const selectCls = ...` 两行。三个 `<select>` 替换为：

```tsx
        <SettingField label={t('settings.claudeFingerprintMode')} description={t('settings.claudeFingerprintModeDesc')}>
          <Select
            value={fingerprintMode}
            onValueChange={(value) => setFingerprintMode(value as 'preserve' | 'force' | '')}
            options={[
              { value: '', label: t('settings.claudeFpPreserve') },
              { value: 'preserve', label: t('settings.claudeFpPreserveExplicit') },
              { value: 'force', label: t('settings.claudeFpForce') },
            ]}
          />
        </SettingField>
        <SettingField label={t('settings.claudeClientPlatform')} description={t('settings.claudeClientPlatformDesc')}>
          <Select
            value={clientPlatform}
            onValueChange={(value) => setClientPlatform(value as 'any' | 'claude_code_cli_only')}
            options={[
              { value: 'any', label: t('settings.claudeClientPlatformAny') },
              { value: 'claude_code_cli_only', label: t('settings.claudeClientPlatformCLIOnly') },
            ]}
          />
        </SettingField>
        <SettingField label={t('settings.claudeVersionPolicy')} description={t('settings.claudeVersionPolicyDesc')}>
          <div className="space-y-1.5">
            <Select
              value={versionPolicy}
              onValueChange={(value) => setVersionPolicy(value as 'passthrough' | 'fixed' | 'minimum')}
              options={[
                { value: 'passthrough', label: t('settings.claudeVersionPolicyPassthrough') },
                { value: 'fixed', label: t('settings.claudeVersionPolicyFixed') },
                { value: 'minimum', label: t('settings.claudeVersionPolicyMinimum') },
              ]}
            />
            {versionPolicy !== 'passthrough' ? <Input value={clientVersion} onChange={(e) => setClientVersion(e.target.value)} placeholder="2.1.251" /> : null}
          </div>
        </SettingField>
```

在时区 `SettingField` 之后、`</div>`（关闭 `SETTINGS_FIELD_GRID_3`）之前加同步区块：

```tsx
        <SettingField label={t('settings.claudeCliVersionSync')} description={t('settings.claudeCliVersionSyncDesc')}>
          <div className="flex items-center gap-2">
            <Button size="sm" variant="outline" onClick={() => void handleSyncClaudeCliVersion()} disabled={syncingCliVersion}>
              <RefreshCw className={cn('size-3.5', syncingCliVersion && 'animate-spin')} />
              {syncingCliVersion ? t('settings.claudeCliVersionSyncing') : t('settings.claudeCliVersionSyncNow')}
            </Button>
            {effectiveCliVersion ? (
              <span className="font-mono text-xs text-muted-foreground">
                {effectiveCliVersion}
                {!syncedCliVersion ? ` · ${t('settings.claudeCliVersionBuiltin')}` : ''}
              </span>
            ) : null}
          </div>
        </SettingField>
        {/* 自动同步开关 + 间隔成对横排，与 Codex 运行时优化保持同一布局 */}
        <div className="sm:col-span-2 grid gap-0 overflow-hidden rounded-lg border border-border/60 bg-muted/15 sm:grid-cols-2 sm:divide-x sm:divide-border/60">
          <div className="flex min-h-[48px] items-center justify-between gap-3 px-3 py-2.5">
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="text-[13px] font-medium leading-snug text-foreground sm:text-sm">{t('settings.claudeCliVersionAutoSync')}</span>
              <SettingHelp text={t('settings.claudeCliVersionAutoSyncDesc')} />
            </div>
            <Switch checked={cliVersionSyncEnabled} onCheckedChange={setCliVersionSyncEnabled} />
          </div>
          <div className={cn('flex min-h-[48px] items-center justify-between gap-3 border-t border-border/60 px-3 py-2.5 sm:border-t-0', !cliVersionSyncEnabled && 'opacity-60')}>
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="text-[13px] font-medium leading-snug text-foreground sm:text-sm">{t('settings.claudeCliVersionSyncInterval')}</span>
              <SettingHelp text={t('settings.claudeCliVersionSyncIntervalDesc')} />
            </div>
            <div className="relative w-[7.25rem] shrink-0">
              <DraftNumberInput
                min={1}
                max={720}
                className="h-9 pr-10 tabular-nums"
                disabled={!cliVersionSyncEnabled}
                value={cliVersionSyncIntervalHours}
                onValueChange={setCliVersionSyncIntervalHours}
              />
              <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-muted-foreground">h</span>
            </div>
          </div>
        </div>
```

若 `SETTINGS_FIELD_GRID_3` 为三列栅格，`sm:col-span-2` 改为 `sm:col-span-3`（查看该常量定义决定）。

- [ ] **Step 4: 运行测试**

Run: `cd frontend && npm test && npm run typecheck`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add frontend/src/pages/Settings.tsx frontend/src/lib/claudeParity.test.mjs
git commit -m "feat(frontend): Claude settings use shared Select and expose CLI version sync

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 11: ClaudeAccounts 与 Proxies 换共享 Select + 全局守卫测试

**Files:**
- Modify: `frontend/src/pages/ClaudeAccounts.tsx:2481-2555`
- Modify: `frontend/src/pages/Proxies.tsx:1505-1522,1988-1997`
- Create: `frontend/src/lib/uiConventions.test.mjs`

- [ ] **Step 1: 写失败守卫测试**

```js
// frontend/src/lib/uiConventions.test.mjs
import assert from 'node:assert/strict'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const srcRoot = fileURLToPath(new URL('..', import.meta.url))

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) walk(full, out)
    else if (full.endsWith('.tsx')) out.push(full)
  }
  return out
}

const SHARED_SELECT = join(srcRoot, 'components', 'ui', 'select.tsx')

test('pages and components use the shared Select instead of a raw <select>', () => {
  const files = [...walk(join(srcRoot, 'pages')), ...walk(join(srcRoot, 'components'))].filter((f) => f !== SHARED_SELECT)
  const offenders = files.filter((f) => /<select[\s>]/.test(readFileSync(f, 'utf8'))).map((f) => relative(srcRoot, f))
  assert.deepEqual(offenders, [], `raw <select> found; use components/ui/select.tsx (see DESIGN.md): ${offenders.join(', ')}`)
})
```

- [ ] **Step 2: 运行确认失败**

Run: `cd frontend && node --experimental-strip-types --test src/lib/uiConventions.test.mjs`
Expected: FAIL，列出 ClaudeAccounts.tsx 与 Proxies.tsx。

- [ ] **Step 3: 实现 ClaudeAccounts**

删除 `const selectCls = ...` 两行。三个 `<select>` 分别替换为：

```tsx
            <Select
              value={fpMode}
              onValueChange={(value) => setFpMode(value as "" | "preserve" | "force")}
              options={[
                { value: "", label: t("claude.fpFollowGlobal") },
                { value: "preserve", label: t("claude.fpPreserve") },
                { value: "force", label: t("claude.fpForce") },
              ]}
            />
```

```tsx
            <Select
              value={clientPlatform}
              onValueChange={(value) => setClientPlatform(value as "" | "any" | "claude_code_cli_only")}
              options={[
                { value: "", label: t("claude.clientPlatformAny") },
                { value: "any", label: t("claude.clientPlatformUnrestricted") },
                { value: "claude_code_cli_only", label: t("claude.clientPlatformCLIOnly") },
              ]}
            />
```

```tsx
              <Select
                value={versionPolicy}
                onValueChange={(value) => setVersionPolicy(value as "" | "passthrough" | "fixed" | "minimum")}
                options={[
                  { value: "", label: t("claude.versionPolicyPassthrough") },
                  { value: "passthrough", label: t("claude.versionPolicyPassthroughExplicit") },
                  { value: "fixed", label: t("claude.versionPolicyFixed") },
                  { value: "minimum", label: t("claude.versionPolicyMinimum") },
                ]}
              />
```

- [ ] **Step 4: 实现 Proxies**

在 import 区加 `import { Select } from "@/components/ui/select";`。

风险筛选 `<select>` 替换为：

```tsx
          <Select
            compact
            value={riskFilter}
            onValueChange={(value) => {
              setRiskFilter(value as RiskFilter);
              setPage(1);
            }}
            triggerClassName="h-8 shrink-0 text-xs font-medium"
            options={[
              { value: "all", label: t("proxies.riskFilterAll") },
              { value: "unscored", label: t("proxies.riskFilterUnscored") },
              { value: "low", label: t("proxies.riskFilterLow") },
              { value: "medium", label: t("proxies.riskFilterMedium") },
              { value: "high", label: t("proxies.riskFilterHigh") },
              { value: "very_high", label: t("proxies.riskFilterVeryHigh") },
              { value: "stale", label: t("proxies.riskFilterStale") },
              { value: "error", label: t("proxies.riskFilterError") },
            ]}
          />
```

风险画像 `<select>` 替换为：

```tsx
              <Select
                value={String(riskProfileDraft.id)}
                onValueChange={(value) => {
                  const selectedProfile = riskProfiles.find((profile) => profile.id === Number(value));
                  if (selectedProfile) openRiskProfile(selectedProfile);
                }}
                triggerClassName="min-w-[220px]"
                options={riskProfiles.map((profile) => ({
                  value: String(profile.id),
                  label: `${profile.name}${profile.enabled ? ` · ${t("proxies.riskEnabled")}` : ` · ${t("proxies.riskDisabled")}`}`,
                }))}
              />
```

若 `Select` 的 `triggerClassName` 不接受这些类名导致视觉异常，改用 `className`（先看 `select.tsx` 中两者分别作用于哪个元素）。

- [ ] **Step 5: 运行测试与构建**

Run: `cd frontend && npm test && npm run typecheck && npm run build`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add frontend/src/pages/ClaudeAccounts.tsx frontend/src/pages/Proxies.tsx frontend/src/lib/uiConventions.test.mjs
git commit -m "refactor(frontend): replace raw selects with shared Select and guard against regressions

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 12: DESIGN.md 与 CLAUDE.md UI 约束

**Files:**
- Create: `DESIGN.md`
- Modify: `CLAUDE.md`（GitNexus 区块之后追加）

- [ ] **Step 1: 写 DESIGN.md**

```markdown
# DESIGN.md — 前端 UI 约束

本文件是 `frontend/` 的组件与布局约束。所有前端改动（含 AI 代理生成的代码）必须遵守；
`frontend/src/lib/uiConventions.test.mjs` 与 `claudeParity.test.mjs` 会在 CI 中强制其中可机检的部分。

## 1. 表单控件：只用共享组件，不手写

| 需求 | 必须使用 | 禁止 |
|---|---|---|
| 下拉选择 | `components/ui/select.tsx` 的 `Select`（`options` 数组，`value` / `onValueChange`） | 原生 `<select>` / 自定义 className 字符串（如 `selectCls`） |
| 开关 | `components/ui/switch` 的 `Switch` | `<input type="checkbox">` |
| 数字输入 | `components/ui/draft-number-input` 的 `DraftNumberInput`（带 `min` / `max`） | `<Input type="number">`（仅历史遗留允许） |
| 文本输入 | `components/ui/input` 的 `Input` | 原生 `<input>` |
| 少量互斥选项 | `Settings.tsx` 的 `SegmentedPillGroup` | 手写按钮组 |
| 按钮 | `components/ui/button` 的 `Button`，图标用 lucide，加载态用 `RefreshCw` + `animate-spin` | 原生 `<button>` |

需要新的表单控件时，先在 `components/ui/` 新增共享组件，再在页面使用；不在页面内部就地实现。

## 2. 设置页布局

- 每个配置模块用 `SettingsCard`（`title` / `description` / `icon` / `footer`）。
- 单个配置项用 `SettingField`（`label` / `description` / `layout="switch"` 可选），说明性提示用 `SettingHelp`。
- 栅格只用 `SETTINGS_FIELD_GRID` / `SETTINGS_FIELD_GRID_3` / `SETTINGS_SWITCH_GRID` 常量，不手写 `grid-cols-*`。
- "开关 + 数值"成对的行（例如自动同步 + 间隔）沿用 Codex 运行时优化区块的两列边框布局；新增同类区块直接复制该结构。
- 版本号、ID 等等宽内容用 `font-mono text-xs text-muted-foreground`。

## 3. 文案

- 所有可见文案走 `t('namespace.key')`；新增 key 必须同时写入 `locales/zh.json`、`en.json`、`zh-TW.json` 三个文件的同一位置。
- 占位符用 `{{name}}`，不用字符串拼接。

## 4. 守卫测试

- 新增或改动设置区块时，在 `frontend/src/lib/claudeParity.test.mjs`（Claude 相关）或对应的源码守卫测试里加断言，覆盖：使用了哪个共享组件、调用了哪个 API 方法、i18n key 存在。
- `uiConventions.test.mjs` 会扫描 `pages/` 与 `components/`（排除 `components/ui/select.tsx`）拒绝任何原生 `<select>`。

## 5. 参照实现

- 共享下拉：`frontend/src/pages/Settings.tsx` ClaudeCode 卡片的时区 / 指纹模式 / 平台 / 版本策略字段。
- 同步按钮 + 自动同步开关 + 间隔：Settings.tsx 中 Codex "运行时优化" 与 ClaudeCode "CLI 版本同步" 区块。
```

- [ ] **Step 2: 更新 CLAUDE.md**

在文件末尾（GitNexus 区块 `<!-- gitnexus:end -->` 之后，若无该标记则直接追加）加：

```markdown

# UI 约束

- **MUST** 在修改 `frontend/` 下任何 `.tsx` 前阅读并遵守仓库根目录的 `DESIGN.md`。
- **NEVER** 在页面或组件中手写 `<select>`、`<input type="checkbox">` 或自定义控件样式字符串；一律使用 `components/ui/` 下的共享组件（下拉用 `Select`）。
- **MUST** 新增文案时同时更新 `zh.json`、`en.json`、`zh-TW.json`。
- **MUST** 新增设置区块时在源码守卫测试中加断言，并运行 `cd frontend && npm test && npm run typecheck`。
```

- [ ] **Step 3: 验证守卫测试仍通过**

Run: `cd frontend && npm test`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add DESIGN.md CLAUDE.md
git commit -m "docs: add frontend UI constraints (DESIGN.md) and enforce in CLAUDE.md

Claude-Session: https://claude.ai/code/session_01R2UHu3k9ZvaACfQY7BciXZ"
```

---

### Task 13: 全量回归、变更范围检查与生产验收

**Files:** 无新增。

- [ ] **Step 1: Go 全量测试**

Run: `go build ./... && go test ./... -count=1 -timeout 8m`
Expected: PASS。若 `auth/claude_fingerprint_test.go` 之外还有断言随机版本池的测试失败，按新语义（版本 = 生效版本）修正断言。

- [ ] **Step 2: 前端全量**

Run: `cd frontend && npm test && npm run typecheck && npm run build`
Expected: PASS。

- [ ] **Step 3: gitnexus 变更范围检查**

运行 `gitnexus_detect_changes()`，确认受影响符号仅限本计划列出的文件；把结果摘要写入最终汇报。

- [ ] **Step 4: 用 /browse 实渲核对设置页**

启动本地前后端，用 gstack `/browse` 打开系统设置页 ClaudeCode 卡片：四个下拉均为共享 Select 外观；"CLI 版本同步"区块与 Codex 运行时优化区块布局一致；点击"立即同步"出现旋转图标与成功 toast。截图留存到 scratchpad。

- [ ] **Step 5: 部署 fr-netcup-new 并验收**

按 `deploy.sh` 既有流程发布。启动后执行：

```bash
ssh fr-netcup-new bash <<'REMOTE'
DB=/opt/ai-stack/apps/codex2api/data/codex2api.db
sqlite3 -readonly -header -column "$DB" "
select id, name, json_extract(credentials,'$.custom_headers.User-Agent') as ua
from accounts where lower(coalesce(json_extract(credentials,'$.upstream_type'),''))='claude' and status<>'deleted';"
sqlite3 -readonly "$DB" "select claude_synced_cli_version from system_settings;"
C=$(docker ps --format '{{.Names}}' | grep -i codex2api-v | head -1)
docker logs --since 10m "$C" 2>&1 | grep claude-cli-version-sync
REMOTE
```

Expected：账号 250、251 的 UA 为 `claude-cli/2.1.258 (external, cli)`；日志出现"启动时已回写 2 个 Claude 账号指纹版本"。随后请用户用 Claude Code 2.1.258 请求一次 Fable 5.1，再查：

```bash
ssh fr-netcup-new sqlite3 -readonly -header -column /opt/ai-stack/apps/codex2api/data/codex2api.db "
select created_at, account_id, model, status_code, upstream_user_agent from usage_logs
where model like 'claude-fable%' and created_at >= datetime('now','-30 minutes') order by created_at desc limit 5;"
```

Expected：`status_code = 200`，`upstream_user_agent = claude-cli/2.1.258 (external, cli)`。
