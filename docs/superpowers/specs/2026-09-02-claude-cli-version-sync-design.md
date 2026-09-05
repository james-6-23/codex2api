# Claude Code CLI 版本同步与指纹版本对齐规格

## 背景与根因

2026-09-02 生产（fr-netcup-new）出现 Claude Code v2.1.258 客户端请求 `claude-fable-5-1` 被 Anthropic 以 400 拒绝，提示 `Claude Code 2.1.219 does not support this model; version 2.1.251 or newer is required`。usage_logs 显示入站 UA 为 `claude-cli/2.1.258`，出站 UA 为 `claude-cli/2.1.219`，`user_agent_overridden = 1`。

根因由三个因素叠加：

1. 全局 `claude_config.fingerprint_mode = force`，`applyClaudeMessagesHeaders` 无条件用账号绑定指纹覆盖入站身份头。
2. 账号指纹 UA 版本来自导入时的随机池 `{2.1.220, 2.1.219, 2.1.205, 2.0.14}`，全部低于 Fable 5.1 要求的 2.1.251，且落库后永不更新。
3. `ValidateClaudeClientRequest` 只校验入站 UA 版本，不校验指纹改写后的最终出站 UA，门控与改写两步互不知情。

上一版规格（`2026-09-02-claude-client-policy-design.md`）明确"不改变指纹既有语义"，本规格补上这一块。

## 目标

- 服务端自动跟踪 Claude Code CLI 最新版本，并把该版本回写到所有 Claude 账号的指纹 UA，使 force 模式下出站版本始终不过期。
- 版本门控对最终出站 UA 生效，保证"门控看到的版本"与"Anthropic 看到的版本"一致。
- 管理端提供与 Codex 运行时优化一致的"立即同步 / 自动同步 / 同步间隔"操作。
- 统一前端下拉组件用法，并把 UI 约束写入 `DESIGN.md` 与 `CLAUDE.md`。

## 非目标

- 不改变 preserve 模式下"入站真实身份头优先"的语义。
- 不自动修改 `version_policy = minimum` 的 `client_version` 配置值。
- 不同步 x-stainless-package-version、node runtime 等其它指纹字段。
- 不改变 Codex 侧任何逻辑。

## 版本同步

### 版本源

1. 主源：`https://api.github.com/repos/anthropics/claude-code/releases/latest`，解析 `name` / `tag_name`（形如 `v2.1.258`）。复用 `ApplyGithubAuth` 与 `GithubProxyOrDefault`。
2. 回退：`https://registry.npmjs.org/-/package/@anthropic-ai/claude-code/dist-tags`，取 `latest` 字段。仅在主源请求失败或解析失败时使用。
3. 两源都失败时返回错误，本轮不写入任何值。

版本号必须能被 `auth.ParseClaudeClientVersion("claude-cli/" + v)` 解析为 `major.minor.patch`；预发布后缀一律丢弃。

### 生效版本

- 内置常量 `auth.BuiltinClaudeCLIVersion = "2.1.258"`。
- 生效版本 `auth.EffectiveClaudeCLIVersion()` = max(内置常量, 已同步值)。同步值缺失、非法或低于内置常量时回落内置常量，远端异常永不导致降级。
- 同步值通过 `Store.SetClaudeSyncedCLIVersion / ClaudeSyncedCLIVersion` 以原子方式发布，`auth` 包不反向依赖 `proxy`。

### 持久化

- `system_settings` 新增列 `claude_synced_cli_version TEXT DEFAULT ''`（SQLite 与 Postgres 各自的增量迁移列表）。
- 新增窄更新 `db.UpdateClaudeSyncedCLIVersion(ctx, version)`，只写该列。
- `ClaudeConfig` JSON 新增 `cli_version_sync_enabled *bool`（字段缺失视为 true，避免老配置静默关闭同步）与 `cli_version_sync_interval_hours int`（0 或缺失视为 12，钳到 [1, 720]）。
- `GET /settings/claude-config` 额外返回只读字段 `synced_cli_version`、`builtin_cli_version`、`effective_cli_version`；`PUT` 忽略这三个字段。

### 后台任务

- `proxy.StartClaudeCLIVersionSync(ctx, db, store, proxyResolver)` 在 `main.go` 中与 Codex 同步任务并列启动。
- 启动时：无条件执行一次 `RefreshClaudeFingerprintVersions(EffectiveClaudeCLIVersion())`（不联网），随后若 `cli_version_sync_enabled` 则执行一次联网同步。
- 之后按 `cli_version_sync_interval_hours` 循环，每轮重新读取开关与间隔，新间隔下一轮生效。
- 环境变量 `CLAUDE_DISABLE_CLI_VERSION_SYNC=1|true|yes|on` 关闭启动与定时联网同步，不影响启动时的本地回写，也不影响管理端"立即同步"。

### 管理端接口

`POST /settings/claude-config/cli-version/sync` 返回：

```json
{
  "fetched_version": "2.1.258",
  "effective_version": "2.1.258",
  "builtin_version": "2.1.258",
  "updated": false,
  "accounts_refreshed": 2
}
```

抓取失败返回 502，body 含错误信息；`accounts_refreshed` 为本次实际改写了指纹的账号数。

## 指纹回写与生成

### 回写

`auth.RefreshClaudeFingerprintVersions(ctx, store, db, version) (int, error)`：

- 遍历 `upstream_type = claude` 且未删除的账号。
- 读取 `credentials.custom_headers` 中 UA（键大小写不敏感）。能解析出 CLI 版本且低于目标版本时，仅替换版本号段（复用 `rewriteClaudeCLIUserAgentVersion` 的正则），其它字符与其它指纹头保持不变。
- UA 缺失或无法解析为 CLI UA 的账号跳过，不合成新指纹。
- 持久化到 `credentials.custom_headers`，并通过 `ApplyAccountCustomHeaders` 更新内存态。
- 幂等：目标版本不高于现有版本时不产生写入。
- 单账号失败记录日志并继续，最终返回成功改写数与首个错误。

### 生成

- 删除随机池 `claudeCLIVersions`。`GenerateClaudeFingerprint(timezone)` 的 UA 版本改用 `EffectiveClaudeCLIVersion()`。
- 其余字段（OS、arch、node、SDK 版本）的随机逻辑不变。

## force 模式与版本门控对齐

在 `ExecuteClaudeMessagesRequestWithPolicy` 中：

1. 入站校验保持不变（`ValidateClaudeClientRequest` 对入站 UA），真实旧客户端仍在入口返回 426。
2. `applyClaudeMessagesHeadersWithVersion` 处理完 preserve / force / fixed 后，解析最终出站 UA 版本。
3. 若出站 UA 可识别为 CLI 且 `decision.RequiredVersion` 非空且出站版本低于 required：
   - 把出站 UA 版本改写为 `EffectiveClaudeCLIVersion()`，并重新记录 `RecordUpstreamUserAgent`。
   - 若生效版本仍低于 required，本地返回 426，错误码 `client_version_too_old`，消息注明出站版本与要求版本，不发出上游请求。
4. `fixed` 策略配置的版本低于模型下限时同样受第 3 步约束，管理端保存时不额外拒绝。

## 前端

### Settings ClaudeCode 卡片

- `fingerprint_mode`、`client_platform`、`version_policy` 三个手写 `<select>` 替换为 `components/ui/select.tsx` 的共享 `Select`；删除局部 `selectCls`。
- 新增"CLI 版本同步"区块，布局与 Codex 运行时优化一致：
  - 当前同步版本（`font-mono text-xs text-muted-foreground`），无同步值时显示内置版本。
  - "立即同步"按钮，`RefreshCw` 图标同步中旋转，成功 toast 含版本与回写账号数，失败 toast 含错误。
  - 自动同步 `Switch`。
  - 同步间隔 `DraftNumberInput`，范围 1 到 720。
- 新增 API 客户端方法 `syncClaudeCLIVersion()` 与对应类型。

### ClaudeAccounts 账号编辑弹窗

三个手写 `<select>` 替换为共享 `Select`，删除局部 `selectCls`。

### Proxies

两个手写 `<select>` 替换为共享 `Select`，以满足守卫测试。

### i18n

新增 key 同时写入 `zh.json`、`en.json`、`zh-TW.json`：`claudeCliVersionSync`、`claudeCliVersionSyncDesc`、`claudeCliVersionSyncNow`、`claudeCliVersionSyncing`、`claudeCliVersionSyncSuccess`（含 `{{version}}`、`{{accounts}}`）、`claudeCliVersionSyncFailed`、`claudeCliVersionAutoSync`（+Desc）、`claudeCliVersionSyncInterval`（+Desc）。

## UI 约束文档

### DESIGN.md（仓库根）

新建，内容为前端组件约束清单：

- 下拉必须用 `components/ui/select.tsx` 的 `Select`，禁止手写 `<select>`。
- 开关用 `Switch`，数字输入用 `DraftNumberInput`，文本用 `Input`，互斥少量选项可用 `SegmentedPillGroup`。
- 设置页区块必须用 `SettingsCard` / `SettingField` / `SettingHelp` 与 `SETTINGS_FIELD_GRID*` 常量组织布局，不得自写栅格。
- 新文案必须同时进三套 locale。
- 新增设置区块必须在源码守卫测试（`frontend/src/lib/*.test.mjs`）中加断言。

### CLAUDE.md

在 GitNexus 段落之外新增"UI 约束"一节，MUST 语气：前端改动前必须阅读并遵守 `DESIGN.md`。

### 守卫测试

新增 `frontend/src/lib/uiConventions.test.mjs`：扫描 `frontend/src/pages/**/*.tsx` 与 `frontend/src/components/**/*.tsx`（排除 `components/ui/select.tsx`），断言不含 `<select`。

## 测试验收

Go：

- GitHub 成功解析 `v2.1.258`；GitHub 失败回退 npm 成功；两者失败返回错误且不写库。
- 生效版本：同步值高于内置取同步值，低于或非法取内置。
- 回写：只改版本段、其它头不变；UA 缺失跳过；幂等不重复写。
- force 模式：指纹 2.1.219 请求 fable 时出站 UA 被抬到生效版本，transport mock 收到的 UA 版本 ≥ 2.1.251；生效版本低于 required 时本地 426 且未发出请求。
- 现有入站门控与 `claude_client_policy_test.go` 全部通过。

前端：

- `uiConventions.test.mjs` 通过；`claudeParity.test.mjs` 新增对共享 Select 与同步区块的断言。
- `npm test`、`npm run typecheck`、`npm run build` 通过。

生产验收（fr-netcup-new）：

- 部署启动后账号 250、251 的 `custom_headers.User-Agent` 版本为 2.1.258。
- usage_logs 中 fable 请求 `upstream_user_agent` 为 `claude-cli/2.1.258 (external, cli)` 且状态 200。
