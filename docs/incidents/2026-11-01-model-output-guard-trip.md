# 事故报告：Model Output Guard 命中导致生成会话中断

- **日期**：2026-11-01（Asia/Shanghai）
- **状态**：已定位根因；断流错误自动重试标记已实现（随本次提交，部署另行安排）；Guard 命中按设计保持不可重试
- **影响范围**：单个生成会话被中断；无数据损坏、无虚构用量、账号未被惩罚
- **关联规格**：`docs/superpowers/specs/2026-10-28-model-output-guard-design.md`（事故场景与防护设计）

## 1. 事故概述

生产实例（3.1.2-guard）上，客户端发起的长上下文生成请求（`/v1/responses`，
模型 `gpt-6.1-sol`，`effort=high`、`fast` 模式）触发 Model Output Guard 的
`tool_argument_deltas` 预算拦截。Guard 按设计冻结原因并终止上游生成，返回 HTTP 502
与公开错误码 `model_output_guard_triggered`。客户端将该错误归类为 `PI_AI_ERROR`
（不可重试），会话直接失败。

## 2. 时间线（本地时间 +08:00）

| 时间 | 事件 | Guard 触发原因 | 已转发字符 |
|---|---|---|---|
| 05:21 | 部署 `3.1.2-guard`，Guard 默认关闭（`enabled=false`） | — | — |
| 05:2x | 管理员开启 Guard（`config_generation=2`） | — | — |
| 05:31:41 | 首次命中：`/v1/responses` 502，耗时 53.9s | 未记录（当时版本无原因日志） | 0 |
| 05:39:07 | 再次命中：`/v1/responses` 502，耗时 63.9s | **`tool_argument_deltas`** | 约 330 |

> 说明：05:39 的命中由部署的临时观测版本在服务端日志中记录
> `[outputguard] tripped: reason=tool_argument_deltas` 得出；该日志改动**未提交**，
> 已从发布分支移除。

## 3. 证据（已脱敏）

usage 日志（`status_code=502`、`upstream_error_kind=model_output_guard`）：

| 时间 | 模型 | 端点 | 状态码 | 耗时 | 上游错误类型 |
|---|---|---|---|---|---|
| 05:31:41 | gpt-6.1-sol | /v1/responses | 502 | 53.8s | model_output_guard |
| 05:39:07 | gpt-6.1-sol | /v1/responses | 502 | 63.9s | model_output_guard |

- 会话侧统计：该会话累计用量约 **5.2M tokens**，与"模型在工具参数中持续小块输出"
  的失控形态一致。
- 错误对外表现：`model_output_guard_triggered: Upstream model output exceeded the
  gateway safety policy; generation was stopped.`
- 账号、上游代理地址、主机地址、密钥等敏感标识均已脱敏，不在本报告记录。

## 4. 根因分析

1. 生产 Guard 配置（默认值）：
   - `output_guard_max_tool_argument_bytes = 262144`（256 KiB）
   - `output_guard_max_tool_argument_deltas = **4096**`（单次工具调用的非空参数增量事件数上限）
   - `output_guard_max_channel_bytes = 2097152`（2 MiB）
   - `output_guard_max_request_bytes = 4194304`（4 MiB）
2. 该请求中模型对同一工具调用持续产出细碎 arguments 增量，累计超过 4096 个增量事件，
   触发 `tool_argument_deltas` 预算（Spec §5.2 规则，用于限制下游对已累计参数的
   全量重解析成本，正是本 Guard 要防止的事故模式）。
3. Guard 在首包/中途拦截后立即取消上游、丢弃缓冲、返回脱敏终态；**未**发生内容泄漏、
   **未**写入虚假 usage、**未**惩罚账号（符合 Spec §5.4 终态语义）。
4. 客户端（DSH/pi-ai）按错误文本分类错误：`classifyPiAiError` 仅当文本含 `5xx`/429/
   timeout 等才判为可重试（`SERVER`/`RATE_LIMIT`/`TIMEOUT`），而 Guard 返回的公开消息
   是纯文案，不含状态数字，因此被归类为 `PI_AI_ERROR`（不可重试），导致会话直接失败而非自动重试。

## 5. 处置（随本次提交，部署另行安排）

1. **Guard 命中保持不可重试**（有意为之）：Guard 触发代表确定性的失控输出，
   自动重试只会重复消耗时间与 token 并再次命中，因此公开消息不带状态数字，
   客户端（DSH/pi-ai）继续归类为 `PI_AI_ERROR`（不可重试），直接失败。
2. **上游断流错误补充状态标记**（瞬态故障可安全重试）：
   - 断流消息：`… safe to retry (status 502)`（含 Anthropic/Grok 消息变体）
   - 机制：DSH 的 `classifyPiAiError` 将含 `5\d\d` 的文本归类为 `SERVER`，
     默认 `retryableCodes` 包含 `SERVER`，最多自动重试 5 次（退避 0.5s→10s）。
3. **未提交**服务端原因日志（`[outputguard] tripped: reason=…`）：仅用于本次定位，
   已从发布分支移除。

## 6. 影响评估

| 维度 | 结论 |
|---|---|
| 数据完整性 | 无损坏；被判定失控的字节未下发 |
| 用量计费 | 无虚假成功用量、无虚构 token |
| 账号状态 | 未降低健康度、未惩罚（`penalize=false`） |
| 客户端体验 | 会话中断；处置后同类错误将自动重试 |
| 安全 | 错误消息不含模型原文/工具路径/prompt/密钥 |

## 7. 建议

1. 若业务确实需要单次工具调用产出极多增量（如超大文件写入），将
   `output_guard_max_tool_argument_deltas` 从 4096 调高（上限 65536），
   并同步观察下游解析开销。
2. 如重试仍稳定命中同一预算，说明该请求会确定性触发失控输出：优先调整提示词/上下文，
   或对该模型/账号临时关闭 Guard（管理员设置页，高风险提示）。
3. 可考虑后续在服务端日志（非下游响应）中永久记录触发原因，便于运维观测；
   本次临时日志已移除，未纳入发布分支。
