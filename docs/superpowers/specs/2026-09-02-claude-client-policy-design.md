# Claude Code 客户端平台与版本策略规格

## 背景与根因

Claude OAuth 账号当前会保留入站 User-Agent 并直接转发到 Anthropic。Anthropic 对 Fable 5.1 等新模型执行 Claude Code 客户端版本门控；例如 `claude-cli/2.1.205` 调用 Fable 5.1 会收到要求 `2.1.251 or newer` 的 400。现有网关没有在出站前识别客户端平台/版本，也没有把该类 400 与账号封禁状态区分开。

## 目标

为 ClaudeCode 全局配置和单个 Claude OAuth 账号提供平台锁定与版本策略：仅允许 Claude Code CLI、固定出站版本、或要求最低入站版本；对模型已知的最低版本在本地门控，禁止把客户端兼容性错误变成账号级冷却/封禁。

## 非目标

- 不伪造无法识别的客户端为 CLI。
- 不改变 Claude OAuth token、指纹、代理和安全字段的既有语义。
- 本阶段不改造缓存写入账单的 token schema。

## 配置模型

全局 `ClaudeConfig` 新增：

- `client_platform`: `any` 或 `claude_code_cli_only`。
- `version_policy`: `passthrough`、`fixed` 或 `minimum`。
- `client_version`: 非空时为 `major.minor.patch` SemVer；空值表示不设版本值。

账号凭据新增同名 `claude_client_platform`、`claude_version_policy`、`claude_client_version`。账号字段为空时继承全局；账号级固定版本优先于全局固定版本。

## 识别与门控

- 从入站 User-Agent 识别 `claude-cli/<semver>`、`claude-code/<semver>`、`Claude Code/<semver>` 及带平台后缀的等价格式。
- 无法识别为 CLI 时，在 `claude_code_cli_only` 下返回本地 400；不调用 Anthropic。
- `fixed` 策略把可识别的 CLI User-Agent 版本替换为配置版本；配置版本为空或非法时按配置校验拒绝保存。
- `minimum` 策略要求可识别版本且不低于配置值；低于版本返回本地 426，提示 `claude update`。
- 模型最低版本规则集中在代码表：`claude-fable-5-1`、`claude-fable-5.1` 及其日期变体要求 `2.1.251`；未知模型不额外猜测。
- 对 Anthropic 返回的 `invalid_request_error` 且消息包含 `Claude Code ... does not support this model`/`version ... required` 的响应，只返回兼容性错误，不调用账号封禁或普通限流状态同步。

## 接口与 UI

- `GET/PUT /settings/claude-config` 返回并校验全局字段。
- 账号调度更新接口接受同名账号覆盖字段；账号响应返回规范化生效策略和最近检测到的客户端版本/兼容性错误。
- Settings 的 ClaudeCode 卡片编辑全局平台/版本策略/版本。
- Claude 账号编辑弹窗编辑账号级“跟随全局/覆盖”策略。
- 请求被门控时，管理端可见模型、当前版本、最低版本和升级命令；状态不显示为 banned。

## 错误与兼容性

- `any + passthrough` 保持现有行为。
- 老账号没有新增凭据时继承全局默认（默认 `any + passthrough`）。
- 门控错误只记入请求诊断/账号最近错误，不写 `unauthorized`、`banned` 或账号级 cooldown。

## 测试验收

- SemVer 解析、比较、异常版本拒绝。
- CLI-only 对 CLI/非 CLI/未知 UA 的放行和拒绝。
- fixed/minimum 策略的出站 UA 与本地错误码。
- Fable 5.1 低于 2.1.251 的本地拒绝，且 transport mock 证明未发出上游请求。
- 版本门控 400 不触发账号级 `SyncClaudeUsageState` 封禁/冷却。
- 全局/账号 API 读写和前端字段类型回归。
