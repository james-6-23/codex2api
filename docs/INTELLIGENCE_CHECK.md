# 降智检测

在 `/admin/intelligence-check` 管理固定账号的自动或手动检测。每次按题库顺序选取
一道启用题目，将最终可见答案和耗时与管理员配置比较，保存结果及配置快照。
支持 GPT/Codex、Responses 中转、Claude、Grok 和 Antigravity 账号。
检测结果表示当前题目和判定规则下的表现，不用于证明上游实际模型身份。

## 目录

- [启用和配置](#启用和配置)
- [结果与历史](#结果与历史)
- [自动处置](#自动处置)
- [限流与额度耗尽](#限流与额度耗尽)
- [请求与代理](#请求与代理)
- [存储和任务执行](#存储和任务执行)
- [开发验证](#开发验证)

## 启用和配置

首次初始化时自动检测总开关关闭，已有账号不参与检测。保存配置和题库后，开启
所需账号及总开关。新账号默认参与检测，但仍受总开关控制；此默认值可以修改。
手动检测使用已保存配置，不要求开启自动检测。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 自动检测总开关 |
| `channels` | Codex/Claude 开，Grok/Antigravity 关 | 按账号来源分别控制自动检测；省略字段保留旧行为 |
| `gpt_model` / `claude_model` | `gpt-6-astra` / `claude-sonnet-4-6` | 各渠道检测模型，发送前验证账号能力 |
| `grok_model` / `antigravity_model` | `grok-4.5` / `gemini-3.8-flash-medium` | 新增来源的检测模型，仍须属于该账号支持的模型目录 |
| `reasoning_effort` | `high` | GPT 推理强度；Claude 不发送此字段 |
| `judge_mode` | `answer` | `answer`、`latency` 或 `both` |
| `interval_seconds` / `retry_seconds` | 300 / 60 | 正常检测间隔及异常后重试间隔，范围 1–86400 |
| `timeout_seconds` | 180 | 请求超时，范围 1–3600 秒 |
| `concurrency` | 4 | 手动和自动检测共享上限，范围 1–100 |
| `retention` | 200 | 每账号终态历史保留数，范围 1–10000 |
| `new_accounts_enabled` | `true` | 仅影响以后创建的账号 |
| `action` | `none` | `none`、`pause` 或 `switch_group` |
| `degrade_threshold` / `recover_threshold` | 2 / 1 | 连续未通过及恢复阈值，范围 1–1000 |
| `target_group_id` | 0 | 切组时必须指定与账号渠道相容的有效分组 |

“自动检测来源”提供 GPT/Codex、Claude、Grok、Antigravity 四个独立开关。
以账号的来源识别，不按所调用模型的名字识别：Antigravity 内的 Claude 模型仍属于
Antigravity。只自动检测 GPT 时，开启 Codex，关闭其余三项，再开启总开关并保存。
账号自身的自动检测开关也须开启；来源开关不修改账号选择、历史或已有处置。

关闭来源会停止新增、领取该来源的自动任务，并隐藏未运行任务的转圈和下次检测时间。
已执行的请求可正常结束，仍遵守配置版本、凭据代次和人工状态的提交保护。
重新开启来源后，原有账号选择恢复参与；手动“立即检测”不受自动来源开关影响。
新增账号的默认参与状态同时受来源开关和 `new_accounts_enabled` 控制。
Grok、Antigravity 默认关闭，避免升级后意外产生新的上游用量。

```json
{"channels":{"codex":true,"claude":false,"grok":false,"antigravity":false}}
```

以上为设置字段示例，保存 API 时仍须提供完整配置与当前 `revision`。
旧客户端省略新增字段保存时，不会覆盖已保存的来源开关和检测模型。

每道题包含 `id`、`name`、`prompt`、`expected_answer`、`match_mode`、
`max_duration_ms`、`enabled`；最多 100 题。开启自动检测或提交手动任务前必须
有启用的题目。内置糖果配对样例使用预期答案 `21`，管理员可编辑或替换。

答案支持完整匹配 `exact`、包含匹配 `contains`、Go 正则匹配 `regex`。
判定前执行 Unicode NFKC 标准化并去除首尾空白和包裹整个答案的单层代码围栏。
耗时判定要求完整成功响应且耗时严格小于题目阈值；耗时不含本地排队时间。
配置和题库一起保存；API 使用 `revision` 检查并发编辑，冲突返回 409。

## 结果与历史

| 状态 | 显示 | 含义 |
| --- | --- | --- |
| `queued` / `running` | 等待执行 / 检测中 | 尚无判定结果 |
| `normal` | 绿色 `#22c55e` | 满足所选判定规则 |
| `degraded` | 红色 `#ef4444` | 完整响应，但答案或耗时未通过 |
| `error` | 深红色 `#991b1b` | 请求失败、超时、未收到成功终态等，不能判为答错 |
| `interrupted` | 已中断 | 服务停止、执行租约失效、账号限流或额度耗尽等，不参与判定 |

账号表格、卡片及详情直接共用业务健康条的 `AccountStatusBar` 组件，检测条每格
对应一次检测，显示最近 20 次终态结果。通过率只计算正常 /（正常 + 未通过），
没有可判分结果时显示 `--`。悬浮查看摘要，点击色块打开该次详情。

状态徽标后提供紧凑的检测和历史图标；等待及执行期间显示转圈。历史弹窗支持分页，
保留题目、判定配置、最终输出、错误和处置记录，并可切换此账号自动检测。
关闭账号自动检测后隐藏检测条，仍可查看历史或手动检测。
当前页摘要批量轮询，页面隐藏时暂停；摘要查询不加载完整响应正文。

## 自动处置

仅 `normal` 和 `degraded` 改变连续次数；请求错误及中断既不增加也不打断次数。
达到阈值可暂停业务调度或切换分组，账号继续参与已启用的检测以观察恢复。

- 暂停使用独立的检测暂停标记。恢复只解除本功能拥有的暂停，不会启用手动禁用
  的账号或清除鉴权、额度等其他状态。关闭暂停策略后，已有暂停不自动解除。
- 切组保存原完整分组集合，恢复时还原仍存在且渠道未改变的原分组。关闭切组
  策略后，已有切组仍可按恢复阈值还原。
- 手动修改账号调度开关取消旧动作归属并重置连续计数。手动修改分组取消旧分组
  还原资格；包括写入相同值及批量操作。独立的检测暂停不因仅修改分组而解除。
- 运行期间配置或人工状态改变，旧任务可保留历史，但不能用旧配置覆盖新状态。

## 限流与额度耗尽

账号限流或额度耗尽时，自动检测不向上游发送请求，也不产生检测记录；检测条显示
「等待额度恢复」。判定与新请求的调度口径一致，包括：

- 账号处于用量或限流冷却（上游 429、用量上限、瞬时限流）；
- 5h/7d 用量窗口已打满，且未由积分顶替；
- Antigravity 配额门禁、Grok 额度耗尽；
- 检测模型自身处于限流或额度类冷却。

等待期间不改变连续次数、异常重试退避和最近一次结果。到预计恢复时刻、或最晚一个
检测间隔后重新判断，因此提前重置（如使用重置券、手动恢复账号）也能及时恢复检测。
已排队的自动任务在发送前会再判断一次；检测请求本身被上游以限流或额度拒绝时，记为
`interrupted` 而非 `error`。手动「立即检测」不受等待影响。鉴权失败、账号错误，以及
仅由自动暂停阈值挡住的账号不属于此类。

## 请求与代理

检测固定账号，复用现有 Codex/Responses/Claude/Grok 请求执行器和账号身份配置；
Grok 按模型目录选择后端协议和地址；自定义检测模型清空后恢复来源默认值，旧客户端省略字段则保留原值。
Antigravity 走其专属执行器并归一化为 Responses SSE，不套用 Codex 的账号级 429 冷却。
OAuth 401 先刷新一次；最终仍为 401 时记录鉴权错误并暂停该凭据代际的自动检测，
即使未启用自适应调度也生效。更新凭据后恢复自动检测，显式手动检测仍可用于验证修复。
不换号兜底、不注入质量测试的 HTML 指令、不将管理员浏览器身份转发给上游。
成功判定要求协议成功终态，仅使用最终可见答案，不从思考内容或工具参数取答案。

检测仅使用账号绑定代理；未绑定时明确直连，绑定失败时记录错误且不回退直连。
检测不继承分组代理、全局代理池、全局单代理、Resin 或环境代理。
客户端身份、指纹和时区仍按原账号/全局配置解析。普通业务出站规则保持不变。

检测会产生上游用量，日志使用内部来源 `intelligence_check`，不归到用户 API Key。
历史中的错误和输出经过既有凭据脱敏，执行租约令牌不返回管理 API。

## 存储和任务执行

SQLite 和 PostgreSQL 初始化时新增 `intelligence_check_settings`、
`intelligence_check_accounts`、`intelligence_check_runs` 三张表及相应索引、
新账号初始化触发器。迁移可重复执行，已有账号默认关闭检测。

数据库事务约束共享检测并发及每账号最多一个活动任务，并使用租约和执行者令牌
拒绝过期提交。过期运行标记中断；检测也遵守本实例账号硬并发上限，忙时重新排队。
任务领取时刷新配置快照，结果和自动处置同事务提交；调度变化通过现有 outbox 同步。
历史按账号清理，原质量测试的表和接口继续保留。

API 列表和请求格式见 [API 文档](API.md#降智检测)。

## 开发验证

```bash
go test -race -count=1 ./internal/intelligencecheck ./database ./auth ./admin ./proxy ./proxy/wsrelay -run 'Intelligence|AccountBound|BoundEgress|UpstreamTrace'
npm --prefix frontend test
npm --prefix frontend run typecheck
npm --prefix frontend run test:intelligence
npm --prefix frontend run build
```

浏览器用例使用模拟接口，需要先安装 Playwright Chromium。
PostgreSQL 行为集使用隔离 schema，设置 `CODEX2API_TEST_POSTGRES_DSN` 后运行
`go test -race -count=1 ./database -run '^TestIntelligenceCheckPostgres$'`。
