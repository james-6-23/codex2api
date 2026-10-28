# Responses 兼容性与验收边界

Codex2API 的 Responses 入口同时服务 Codex 客户端、OpenAI SDK 和不同上游。兼容程度取决于实际选中的账号、协议及上游能力；模型名称、模型目录和 HTTP 200 都不能证明全部官方功能可用。

## 请求字段与严格模式

Responses 中转保留原生 `reasoning.effort`、`text.format`、JSON Schema 约束及大整数，原生 JSON 模式不会自动改写用户提示。`reasoning_effort`、`response_format` 等兼容别名仍有显式转换；原生字段优先。HTTP 出站会删除仅属于 WebSocket 信封的顶层 `type`、`stream_id`。网关内部使用命名流标识进行分流，最终发往 OAuth / Responses relay 的 WS 帧也不携带网关 `stream_id`；嵌套输入或 schema 里的同名字段仍保留。内置 `tool_search` 不会被自动补入其上游协议不接受的 `description`，用户显式字段由上游验证。

ChatGPT/Codex 上游需要额外规范化，部分 Responses 字段或 schema 约束无法原样保留。客户端可发送：

```http
X-Codex2API-Strict-Compatibility: true
```

也可设置服务端环境变量 `CODEX_STRICT_RESPONSES_COMPAT=true`。严格模式按**实际选中的原生 Codex 账号**检查，`auto` 或 Codex 渠道内的 Responses 中转账号不会被这套规则误拦。默认模式继续沿用原有兼容转换。

严格模式在 HTTP 尚未提交时返回 `400 unsupported_feature`；若重试保活已提交 SSE，则发送 `response.failed` 事件；WebSocket 返回 `status:400` 的错误帧。`error.details.fields` 列出有损字段，包括输出长度、采样参数、元数据、上下文管理、存储和缓存控制等。另有以下语义检查：

- `background:false/null`、`conversation:null` 等空操作允许通过；`background:true`、非空 `conversation`、`store:true` 明确拒绝。字符串 `background` 仍是原有图片背景扩展。
- reasoning 和格式别名只在能够完成已知转换时通过；未知或会降档的 effort、被丢弃的服务档位会拒绝。
- 工具或结构化输出 schema 会被 Codex 清洗改变时拒绝；超过 128 个工具、会被删除的工具声明、会被改写的搜索配置也明确报错。`additional_tools` 中的工具同样检查，保留工具命名空间沿用原样透传规则。检查受工具累计数和错误数预算限制；原生 Codex 路径会丢失精度的 schema 数字也拒绝。

这是一套已知有损转换门禁，**不是完整 schema 验证器或全字段无损证明**。账号映射、管理员 Payload Rules、上游模型限制和未知嵌套扩展仍可能影响最终行为。启用严格模式可能使过去被自动修复的请求改为报错，应据字段列表修正请求或改用支持该语义的上游。

## HTTP 续链的恢复要求

HTTP Responses/Compact 在处理原始 JSON 时拒绝重复的顶层 `input` 或 `previous_response_id`，返回 `400 invalid_request`，避免不同解析器选取不同值；JSON 转义后的同名键也算重复。嵌套字段和正常原生 Responses relay 的数值、父引用不受此检查改写。

原生 Codex HTTP 出站需要展开并删除 `previous_response_id`。每个非空父响应引用都必须在当前请求归属的缓存中得到证明；缓存缺失或过期返回 409，后端故障返回 503，不会丢弃引用后继续返回一轮没有历史的成功响应。新工具调用对仍需拼接父快照，不能据此推断客户端重传了所有祖先。只有重叠 `call_id` 且完整父前缀一致时才跳过重复拼接；比较仅忽略已定义的顶层 item ID 和内部元数据。部分重放、调用内容冲突或重复定义父调用返回 `409 previous_response_not_found`，客户端可重发完整历史并省略父 ID。相同消息文本本身不会触发去重。

当前 HTTP 缓存主要保存工具上下文，不为普通消息新增会话存储。普通文本父轮的后续 HTTP 请求应发送完整历史并省略 `previous_response_id`；需要增量续链时使用经过验证的 WS 路径。原生 Responses relay 可把父 ID 交给自己的上游恢复，不套用 Codex 的本地缓存缺失限制；其他仅做协议翻译的渠道不能因此自动获得服务端父响应存储能力。

## WebSocket 流与续链

`GET /v1/responses` 支持 `response.create`：

- 缺省流和相同 `stream_id` 内 FIFO；不同流可并行，独立慢流不会阻塞其他独立流。
- 每连接最多 16 个活动轮次、32 个不同命名流；缺省流不占命名额度。`stream_id` 为 1–256 字节的 ASCII 字母、数字、`_`、`-` 或 `.`。
- 命名流的响应事件和请求错误带回对应 `stream_id`。命名流的请求级失败及抢占不会关闭其他健康流；缺省流保留原有协议错误关闭行为。鉴权失效、连接断开或连接资源耗尽仍可结束整个连接。
- 同道待处理请求最多 16 条、总待处理请求最多 64 条，仍受读队列、字节预算、全局内存和账号/密钥并发限制。断线会取消活动轮次并释放队列与租约。
- 各轮使用独立审计、额度和请求状态，上游连接池按流隔离。网关可能使用不同上游连接实现并行，不承诺所有流复用一条上游连接；指向同一父响应的续链仍可能等待上游原连接租约。

`stream_id` 决定排序，`previous_response_id` 决定历史来源。对于 `store:false` 原生 WS 缓存，同一客户端连接可以从另一个流**已提交**的响应分叉，新响应写入自己的流缓存。连接内索引最多保留 256 个响应标识，仍须命中完整上下文并满足账号/密钥、路由分组、凭据和缓存限制。另一个客户端连接不能凭这个索引读取未观察到的跨流历史；索引淘汰、缓存过期或重启后，客户端应提供完整历史。Memory 与 Redis 的恢复规则详见 [Responses 上下文](API.md#responses-上下文不可用)。

`response.append` 和官方 GPT-6 `response.steer` 目前返回 `status:400`、错误码 `unsupported_feature` 的 WebSocket 错误帧。本次没有实现生成中改向；等待当前轮终止后发送新的 `response.create` 是另一轮请求。真正的 steering 还需要覆盖 accepted/pending/failed、原响应 incomplete、自动后继响应、工具结果、配额与提交状态，不能仅转发一帧控制消息后宣称成功。

Codex 的中断控制事件与 `response.steer` 不同：

```json
{"type":"response.interrupt","response_id":"resp_current","mode":"discard_partial_items"}
```

中断绕过生成队列，按当前下游连接内的 `response_id` 定位上游租约；可选 `stream_id` 进一步限制目标流，发往上游时剥离该网关路由字段。Codex OAuth 与启用 WebSocket 出站的 Responses 中转账号会把控制帧发往当前连接，不重连、不重放、不另开生成请求。上游的 `response.interrupt.accepted` / `response.interrupt.failed`、`response.output_item.interrupted` 和 `response.incomplete` 原样保留，命名流仍带回 `stream_id`。完成帧可以早于中断确认；网关保留事件及 `sequence_number` 的原始顺序，直到终态与对应确认都消费完才归还租约。

已结束轮次的迟到中断会本地确认，重复中断不重复转发；连接内最多记住最近 256 个已结束轮次，不能用旧 ID 取消下一轮或其他连接的请求。中断确认等待上限为 30 秒，超时销毁对应上游连接，避免残留确认进入后续租约。未知目标返回带相同 `response_id` 的 `response.interrupt.failed`；无 WebSocket 控制通道的上游返回 `interrupt_not_supported`。缺少或类型错误的 `response_id`、不支持的 `mode`、非法 JSON、非对象载荷及重复的路由/控制字段返回请求错误。

`response.incomplete` 的 `interrupted`、`steered`、`content_filter` 和 `max_messages` 原因即使输出为零也保留原终态，不进入空回复修复或自动重试。已完成输出与用量继续按原续链和计费路径处理，未完成输出是否丢弃由上游 `discard_partial_items` 语义决定。

## 能力目录与工具边界

账号探测保存有界的 `experimental_supported_tools` 字符串数组，模态只接受 `text/image/audio`。Codex 的 `models/slug` 模型清单按当前密钥范围、渠道及符合目录与模型筛选条件的账号求能力交集，不保证账号此刻存在空闲并发位。普通 OpenAI 的 `data/id` 模型列表不会因此新增能力字段。未知快照、凭据代际变化或不可读取的能力信息不会继承高级声明，旧快照也不能恢复本次取得的上游清单中已经撤销的能力。上游清单和本地回退均应用此规则；ETag 对应最终目录，客户端 304 不能绕过能力收窄。

| 功能 | 网关契约 | 验收限制 |
| --- | --- | --- |
| function / custom 工具 | 保留调用与结果配对，续链时恢复必需历史 | 本地模拟验证协议；不保证模型一定按 schema 生成 |
| strict / parallel 工具 | 保留可传递字段，严格模式拒绝已知有损 schema；不同 WS 流可独立进行工具回合 | 目录中的并行工具声明不代表支持 steering |
| compact / previous_response_id | 按实际上游保留或转换，默认完整快照恢复失败时明确报错；显式启用 `CODEX_WS_CONTINUATION_FAIL_OPEN=true` 会开启有损降级 | 不伪造官方 Conversations 或存储查询生命周期 |
| hosted tools / MCP / 文件 / 图像 / 音频 | 支持的原生请求交给相应上游执行和校验 | 授权、资源归属、模型及协议限制仍由上游决定；不会从本地工具声明合成官方托管能力 |
| background / conversation | Responses 中转保留请求，由上游验证；原生 Codex 严格模式拒绝不支持的语义 | 没有新增完整的查询、取消、Conversations 或后台任务端点 |

## 本地验证与真实验收

自动化回归使用假账号与 loopback 上游，检查 HTTP/WS 派发后的字段、事件、工具回传、并行与 FIFO、跨流历史、断线释放、缓存隔离及故障错误。共享缓存测试覆盖后端接口契约，不等同于独立 Redis 服务的集成验收。这些自动化回归不会访问真实账户或产生模型调用费用；真实账号验收独立进行并记录实际使用的模型及工具。

真实验收仍应对同一个支持该功能的上游进行官方 SDK 直连与网关差分，比较事件序列、终态、工具配对、约束、资源归属和错误；模型升级、凭据变化及缓存/协议切换后应重验。本地自动化回归不代表实际部署或上游全部能力已验证。

官方契约参考：[WebSocket 模式](https://developers.openai.com/api/docs/guides/websocket-mode)、[Steering](https://developers.openai.com/api/docs/guides/steering)、[Codex 模型描述结构](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/openai_models.rs)。

中断协议按 Codex 源码核对：[发送与确认](https://github.com/openai/codex/blob/970111f6cb62cc02fe36a82ea7d71b37a2889c97/codex-rs/codex-api/src/endpoint/responses_websocket.rs)、[中断终态与用量](https://github.com/openai/codex/blob/970111f6cb62cc02fe36a82ea7d71b37a2889c97/codex-rs/codex-api/src/sse/responses.rs)。公开 [Responses 事件定义](https://developers.openai.com/api/reference/resources/responses/streaming-events)及 Steering 文档不能替代 Codex 的 `response.interrupt` 契约。
