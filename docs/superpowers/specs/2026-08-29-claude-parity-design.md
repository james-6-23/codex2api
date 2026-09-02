# Claude 渠道对等适配设计

## 目标

让 Claude Code OAuth 账号在账号管理、用量采样、看板统计、Usage、模型目录、API Key/代理/调度筛选中拥有与其能力相匹配的完整可见性；保证 Claude 请求只进入 Anthropic Messages 原生链路，不被误归类或误送到 Codex/Grok/ChatGPT 探针。

## 已确认根因

1. `insertClaudeAccount` 写入账号后没有进入导入预热队列；`Account.NeedsUsageProbe` 又把 Claude 当 relay 跳过，导致新账号永远没有初始用量快照。
2. `summarizeDashboardAccounts`、Usage 日志归因和通用渠道过滤没有统一识别 Claude，Claude 账号会混入 Codex 统计。
3. Claude 的 5h 分析复用了 Codex 套餐名判断，Claude 的 `max-*`、`enterprise` 和默认档位会被排除。
4. Dashboard/Usage/ API Key/代理/调度页面的渠道选项和模型目录缺少 Claude，部分账号操作没有明确的 provider 能力边界。

## 方案

### 采样与状态

- 新增 Claude 专用异步采样入口，复用现有导入探针队列、并发闸和生命周期管理。
- 采样只调用 Anthropic 原生 Messages 能力，使用最小、明确的测试请求读取统一限流头；绝不调用 ChatGPT WHAM 或 Codex Responses 探针。
- 成功后持久化 5h/7d 用量快照、采样时间和 provider 状态；失败保留 `unsampled`/错误原因，按现有重试策略排队，不改变请求转发结果。
- OAuth 导入、Token 导入、批量导入和手动刷新统一触发一次采样；并发去重，避免重复扣费。

### Provider 归属

- 后端所有账号统计和 UsageLog channel 统一通过 `upstream_type`/运行时账号判定 Claude。
- API Key、Responses、Chat Completions 等不支持 Claude 的路径明确排除 Claude；原生 `/v1/messages` 保持 Claude 路由。
- Claude 的模型目录从账号真实模型集合和缓存生成，空账号时返回明确空状态。

### 页面

- Dashboard/Usage 的共享渠道筛选加入 Claude，渠道徽标、模型过滤和空状态同步加入。
- Claude 账号页展示采样状态、最后采样时间、失败原因和 5h/7d/今日数据；已有分析卡片复用 provider-aware 数据。
- API Key、代理、调度和模型目录筛选加入 Claude；不适用的 Codex 专属操作继续隐藏并给出原因。

## 数据流

```text
Claude OAuth/Token 导入
  -> insertClaudeAccount
  -> store.AddAccount
  -> Claude probe queue (deduplicated)
  -> Anthropic Messages probe
  -> SyncClaudeUsageState
  -> DB/runtime snapshot + cache invalidation
  -> Dashboard / Usage / ClaudeAccounts
```

## 错误与安全边界

- 采样失败不封禁账号、不阻塞导入响应、不将 Claude 凭据发送给其他 provider。
- API 错误只保存脱敏状态和截断原因，不记录 access/refresh token。
- 真实请求的限流、失败和成功状态仍由 Claude 原生响应处理；采样仅作为额度可见性补充。

## 验收

- 新增 Claude 账号后在不重新导入的情况下从 `unsampled` 进入 `sampled` 或显示明确失败状态。
- Dashboard/Usage 按 Claude 筛选时统计、模型、日志渠道和图标均正确，Codex 统计不增加 Claude 数据。
- Claude 5h/7d 分析覆盖 Max/Enterprise/默认档位；API Key/代理/调度筛选不会把 Claude 当 Codex。
- Go 全量测试、前端类型检查/测试/构建通过；使用现有本地环境验证，不新增端口。
