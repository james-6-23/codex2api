# Codex2API 全局模型输出失控防护规格

> 状态：设计审查验收通过、取舍已由用户确认；新增行为、阈值和接口均为设计规格，尚未实现或经压测验证。
> 日期：2026-10-28（Asia/Shanghai）。
> 工作分支：`fix/dsh-stream-runaway-guard`。
> 实施边界：仅当前 Codex2API 项目；不修改 DSH、客户端、客户端依赖或已安装 npm 包。本轮仅交付 spec，不进入实现计划或编码。

## 1. 背景、证据与保护边界

### 1.1 用户提供的事故证据

模型在 `write` 工具参数的 `content` 中持续生成重复文本，未闭合参数由约 123k 增至 242k chars。用户报告 DSH 每收到一次 arguments delta，都会对已累计的参数全文进行同步解析及修复。debugger 栈、CPU profile、缓冲增长和微基准共同支持以下机制：持续扩大的参数被反复全量扫描，累计计算量可呈 O(n²)，拖慢整个 Node 事件循环。上游 socket 的 Recv-Q 增长，关闭 socket 后进程内积压仍在解析，最终只能重启 DSH。

这些运行指标来自用户事故记录，本次没有重新复现线上卡死；具体 delta 数量、原始完整流和代理端当时的日志尚未取得。不能把 chars 等同于 UTF-8 bytes，也不能从 socket 排队单独证明所有 CPU 占用均由解析造成。

### 1.2 本项目要解决什么

在 Codex2API 收到上游模型输出之后、进入无界聚合/私有 replay/下游转发之前，识别明显的持续重复和超过预算的输出，主动结束当前请求，让异常内容不再持续传入客户端。

“全局”表示所有受支持的文本/工具生成渠道共用默认策略，不依赖模型名、账号类型、User-Agent 或工具名；不表示某个请求异常时切断所有会话。

### 1.3 明确不能保证什么

- 代理不能抢占 DSH 的同步解析、清理客户端已有缓冲、停止已经执行的工具或恢复已经卡死的进程。
- 已写入下游 socket、反向代理及客户端的字节无法撤回；失败终态也可能排在这些字节之后。
- 有界输出仍可能令某个客户端解析缓慢。默认阈值是风险限制，不是“DSH 永不卡死”的数学保证。
- 代理只能保证自己不重试本次 guard 故障，不能控制客户端 SDK、DSH 或外部网关是否另发新请求。
- 重复检测不等于准确判断模型语义是否退化；首期精确识别短周期持续重复，变体循环由硬预算兜底。

## 2. 当前实现事实与设计落点

本节是当前工作区的只读核查结果，不代表防护已经存在。链接按仓库根目录解析。

| 已核对事实 | 对设计的影响 |
| --- | --- |
| [通用 SSE reader](<proxy/executor.go#L1592-L1714>) 对 callback 返回 `false` 的处理是正常返回 `nil`；内部会拼接未完成行及多行 data。 | 仅返回 `false` 不足以保留 guard 原因；必须另有 typed error/结果。拼帧也要有资源边界。 |
| [现有 SSE 大小常量和有界行读取器](<proxy/sse_line.go#L8-L30>) 定义了 8 MiB 的 line/event 上限，但不能据此认定每个 reader 已使用。 | 首期核对并补齐受保护路径，不宣称已有全路径硬上限。 |
| [Responses HTTP 转发](<proxy/handler.go#L5538-L5585>) 在回调最前即通过 `bytes.Clone` 记录 compaction provenance（5543-5546 及 5806），后续解析事件并聚合 output 与下游写入。 | guard 检查必须前置于 compaction provenance 记录与 output 累积，命中事件严禁克隆或落库。 |
| [continuous retry](<proxy/continuous_retry.go#L42-L47>) 开启后整次 attempt 保持私有；[attempt writer](<proxy/stream_flush_writer.go#L134-L173>) 在成功 replay 提交时才执行已有输出策略。 | 新 guard 必须在 attempt 生成期间运行，不能等完整成功后才检查。 |
| [现有输出扫描器](<security/promptfilter/output.go#L13-L67>) 属于可关闭的 prompt filter；[writer 配置](<proxy/stream_flush_writer.go#L123-L131>) 依赖它的开关。 | 新 guard 与 prompt filter 独立，关闭提示词过滤不应关闭资源保护。 |
| [Responses WS 转发](<proxy/responses_ws.go#L1251-L1336>) 有独立的 collector、转换和 replay 写入；[Gemini native](<proxy/handler_gemini.go#L390-L418>) 直接写响应。 | 仅挂在 `streamFlushWriter` 上会漏掉部分协议/transport。 |
| [streamOutcome](<proxy/handler.go#L2343-L2367>) 已有 requestScoped/terminalLocal 等语义；[故障分类](<proxy/handler.go#L2419-L2471>) 可能把读写错误归入 499/598。 | guard 必须独立分类，不能被取消后的读错误覆盖成下游断开或普通上游断流。 |
| [continuous retry 选择器](<proxy/continuous_retry.go#L172-L191>) 存在 catch-all；[自动续想](<proxy/handler.go#L5680-L5697>) 也能再次请求上游。 | guard 的不可重试优先级必须高于所有恢复路径。 |
| [上游 drain context](<proxy/upstream_drain.go#L8-L51>) 会在普通客户端取消后保留最多 5 秒读取 usage。 | guard 命中必须主动调用上游 cancel，不走普通断线后的继续 drain。 |
| [runtime settings](<proxy/runtime_config.go#L68-L100>) 使用内存运行时配置但现无 revision 机制；[response cache settings](<database/response_cache_settings.go#L15-L58>) 展示了 `Generation` 版本控制与 DB 事务合并先例；[轮询器](<proxy/response_cache_settings.go#L12-L175>) 提供了 5 秒轮询同步模式。 | 复用项目设置体系，三态指针存储避免零值覆盖 false；5 秒 revision 轮询为新增机制，需参考 response cache 先例落地。 |

## 3. 目标、非目标与备选方案

### 3.1 首期目标

1. 对重复工具参数、重复文本/思考以及未重复但无界增长的生成实施可解释的切断。
2. 在命中事件进入翻译器聚合、replay、缓存和下游之前拒绝该事件。
3. 检测状态和解析资源有界，避免代理自身重演全文重解析和内存增长问题。
4. 明确失败终态；不伪造正常完成、工具参数完成或成功缓存。
5. 当前请求不换号、不自动续想、不透明重试，不影响其他正常请求。
6. 提供全局设置和脱敏诊断，能够确认策略在哪些渠道生效。

### 3.2 非目标

- 修改 DSH 的 JSON parser、事件循环、重试、goal 或工具执行机制。
- 操作客户端主机、杀进程、重启服务、读取客户端 CPU 或 socket。
- 按某个 provider/model 或 `write` 工具打补丁、全局封禁某个账号。
- 新增 LLM 审查调用、语义分类模型、自动改写 prompt 或自动拆分写文件任务。
- 首期识别所有近似循环、语义循环和恶意编码绕过。
- 全响应缓冲后才放行所有普通流，或默认增加等待窗口来假装可撤回历史输出。

### 3.3 方案比较

| 方案 | 优点 | 缺点 | 结论 |
| --- | --- | --- | --- |
| 只限制最大长度/时长 | 简单、结果确定 | 重复可能早已造成压力；长时间正常推理容易误判；细碎 delta 仍有风险 | 作为兜底，不单独采用 |
| **协议感知的增量重复检测 + 字节/事件硬预算 + 请求级切断** | 在生成期间工作，跨 delta 检测；覆盖私有 replay；成本可界定 | 需逐协议接入，阈值有兼容性取舍 | **推荐首期方案** |
| 全量保留输出，成功后检查再回放 | 不会在检查前向客户端释放内容 | 破坏普通 streaming 延迟；仍须限制代理端聚合；不能替代在线检查 | 不作为全局默认，不新增一套 replay |

## 4. 覆盖范围和数据流

### 4.1 协议/渠道矩阵

| 下游入口 | 首期要求 |
| --- | --- |
| `/v1/responses` HTTP | SSE 与非流式 JSON；直连、中转、协议转换、上游 HTTP/WS 桥接、自动续想及 continuous retry。 |
| `/v1/responses` WebSocket | 每个逻辑 response/stream 独立检测；包含多路复用、私有 replay 和原生转发。 |
| `/v1/realtime` WebSocket 文本子集 | 明确覆盖：通过 [realtimeTextSession](<proxy/realtime_ws.go#L21-L153>) 桥接至 `responsesWSForwardOptions` 的文本/工具生成子集纳入防护；音频 Modality 由 [normalizeRealtimeTextClientEvent](<proxy/realtime_ws.go#L188-L232>) 在 `realtimeTextSession` 显式拒绝、不产生生成通道；session 管理帧不参与语义检测。 |
| `/v1/chat/completions` | 流式与非流式；`content`、可见 reasoning 字段、tool calls 与旧 `function_call` 兼容字段。 |
| `/v1/messages` | 流式与非流式；原生 Claude 及其他渠道转换，含 text/thinking/input_json_delta 和完整 tool input。 |
| Gemini `streamGenerateContent` / `generateContent` | text/thought/functionCall，区分增量和完整快照。 |
| 上述入口上的 Grok、Antigravity 及 API Key/账号渠道 | 不得因 native passthrough 或转换路径不同绕过。 |

专用图片、音频、models、countTokens、用量查询、compact/后台维护接口不在首期语义检测范围。多模态生成中正常的图片/音频二进制或 base64 字段不做文本重复检测，也不计入语义文本预算；仍受各自媒体/传输限制。文本工具参数中携带的 base64 不豁免工具参数预算。Realtime 的音频 Modality 由 `normalizeRealtimeTextClientEvent` 在 `realtimeTextSession` 显式拒绝（返回 `invalid_request_error`）、不产生生成通道，session 管理帧不参与语义检测；其经 `realtimeTextSession` 转发到 Responses WS 的文本/工具 turn 严格继承全局文本与工具 guard 规则，不留语义盲区。

“全局完成”必须以覆盖矩阵集成测试为准，不能只接通事故使用的 Responses SSE 就宣布完成。新增生成渠道必须声明增量/快照语义并接入共享 guard。

### 4.2 推荐组件职责

- **公共策略/状态机**：配置快照、request 预算、channel 状态、重复检测和唯一终止原因；不依赖 Gin、账号调度或 prompt filter。
- **协议投影器**：把单个有界事件投影为 `append`、`snapshot`、`end` 等语义观察；只解码当前事件，不解析累计未完成的工具 JSON。
- **终止适配器**：取消上游、丢弃尚未提交内容、映射协议失败、释放租约并写诊断。

```text
有界读取/组装上游事件
  → 解析当前协议 envelope
  → 投影 text / thinking / tool arguments
  → 共享 guard 检查（命中则停止，整事件不再下发，且不进入 provenance/collector/replay）
  → 记录 compaction provenance（proxy/handler.go:5543-5546, 5806）
  → 既有协议转换/collector/自动续想缓冲/私有 replay
  → 既有 prompt output filter
  → 下游写入
```

若原生渠道必须先经转换才能提取语义，转换前至少保留 framing/计数保护，语义检查紧接转换且早于会累计正文的消费者。不能先完整聚合无界参数再交给 guard。一个事件投影为多个字段时，先检查全部字段，再允许整个事件产生下游或持久化副作用。

### 4.3 状态隔离

- request 预算属于一次入口 HTTP 请求或一个 WS 逻辑 turn；不是整个 API Key、账号或长连接。
- channel key 至少包含 attempt/response 身份、候选项/输出项/工具调用身份及字段种类。text、thinking、不同 tool calls 不拼在一起检测。
- index 与 item/call ID 通过别名映射指向同一状态，不能靠补发 ID 或交错事件重置计数。
- channel 结束后保留小型计数信息以识别最终快照，释放扫描窗口；累计创建数受限，不因 end 归还创建预算。
- 自动续想不重置 request 预算。普通上游故障重试可开启新的 attempt/channel，但仍消耗同一入口请求预算。
- 最终 replay 输出不再次累计同一份语义内容；所有 attempt 的生成阶段均须检测。

## 5. 检测规则与建议默认值

数值为首版保守建议，须在实现前评审、在上线前用正常样本与合成退化流验证，并非从事故精确推导的安全边界。

### 5.1 硬预算

字节按 UTF-8 bytes，使用 int64 累计并在加法前检查溢出；等于上限允许，超过上限拒绝触发事件。

| 配置项（拟新增） | 默认值 | 含义 |
| --- | --- | --- |
| `output_guard_enabled` | `false` | 全局启用；采用三态存储（`*bool` 指针语义），旧库缺失时填充默认 `false`，显式 `true`/`false` 必须可持久化且不被零值覆盖。 |
| `output_guard_max_tool_argument_bytes` | 262144（256 KiB） | 每次工具调用完整序列化参数/自定义工具输入上限，不只检查 `content`。 |
| `output_guard_max_tool_argument_deltas` | 4096 | 每次工具调用非空 arguments append 事件数，限制细碎 delta 引起的下游累计重解析成本。 |
| `output_guard_max_channel_bytes` | 2097152（2 MiB） | 单 text/thinking channel 上限；工具用更严格的工具参数上限。 |
| `output_guard_max_request_bytes` | 4194304（4 MiB） | 当前入口请求跨 channel/attempt/自动续想的累计语义预算。 |

首期固定资源上限：每 request 65536 个业务事件、128 个累计 channel；单 channel 累计扫描预算为 `2 * output_guard_max_channel_bytes`（默认 4 MiB；工具 channel 为更严格的 `2 * output_guard_max_tool_argument_bytes`，默认 512 KiB，见 §5.3）；配合每 channel 至多 3 次合法纯终态确认帧的全文 Hash 校验与下发许可（额外上限至多 `3 * limit`，见 §5.3），单 channel 整体最大数据处理与校验量严格受限于 `5 * limit`（普通 channel 10 MiB，工具 channel 1280 KiB）。心跳/ping/SSE 注释不计业务事件；非空与空内容业务事件都计数，避免空 delta 规避事件预算。wire/framing 上限独立于语义预算，不因关闭 guard 而取消。

【细 delta 与长推理事件预算量化分析】：
65536 个业务事件配合 2 MiB channel 预算，平均每个事件 32 字节。对于流式长思考（thinking）或细 delta（每次仅 1~4 字节），当思考达到 100~200 KiB 时可能消耗数万事件；或者单请求存在多个并行工具调用且每个工具发生 4096 次 delta 时，事件预算可能先于 2 MiB / 4 MiB 字节预算耗尽。这是防御下游 DSH O(n²) 频繁同步解析的主动设计目标（事件数本身就是驱动 DSH 反复触发解析的直接源头），而非设计缺陷。此风险需在 §9.3 细 delta 样本中专门量化并报告。

工具参数长度按 outer envelope 解码后的 arguments 字符串计算，保留其中 JSON 转义，不解析不完整的 inner JSON。原生协议若给出完整 object，使用一次紧凑 JSON 序列化后的 UTF-8 bytes 计数，不逐 delta 重建完整 object。自定义工具原始输入按解码后的 UTF-8 bytes 计数。

默认工具上限会拒绝部分合法大文件写入，不做 `write` 白名单或“看起来合法就忽略”例外。管理员可调高，但界面须提示降低对下游全量重解析问题的保护；建议大文件分段生成。这项取舍已由用户确认。

### 5.2 持续重复检测

首期推荐精确短周期规则，默认启用，独立应用于每个 text/thinking/tool channel：

1. 维护最近 8192 bytes 的 ring buffer，累计每新增 1024 bytes 到达检查点；检查点按 channel 累计字节位置定义，不按 delta 边界定义。
2. 窗口已满（满 8192 bytes）时求最短周期 `p`；`1 <= p <= 256` 且对所有 `p <= i < 8192` 满足 `window[i] == window[i-p]` 时命中。
3. 命中证据是连续 8 KiB 的精确周期窗口；最长周期时也含至少 32 次重复，不要求窗口长度整除周期。由于检查步长为 1 KiB，任意起点的连续周期片段达到 9215 bytes 时，必存在一个完整落入该片段的检查窗口；不声称任意起点恰好 8192 bytes 就一定即时命中。
4. 使用固定窗口上的线性周期算法，例如 prefix function；不可用灾难性回溯正则，不对全部历史穷举子串。
5. 大 delta 按相同检查点消费，不只检查最终尾巴；后续正常文本不能抹掉中间检查点已经产生的命中。
6. **尾部与终态检查规则（end/snapshot 尾检）**：流结束（`end`）或收到非对齐快照（`snapshot`）时，累计字节通常未对齐 1024-byte 检查点或累计长度未满 8192 bytes。为避免短正常文本被误判，同时防止攻击者/异常模型在周期尾部留残缺字节逃逸，尾检规则明确界定如下：
   - **未满 8192 窗口**：如果 channel 累计总字节数 `< 8192`，**不判定重复**，直接放行（由硬字节预算兜底，且 8192 窗口是判定周期性重复的数学基线，严禁在几百字节甚至几千字节的正常短文本上缩短窗口强行求周期，杜绝短文本误判）。
   - **已满 8192 窗口但尾部未对齐 1024**：取当前 ring buffer 中最新的完整 8192 bytes 窗口执行一次求周期检查。周期阈值 `1 <= p <= 256` 保持不变，判定条件仍为窗口内严格满足 `window[i] == window[i-p]`。这保证了即时尾检与主检查点的数学一致性，且任意阶段的判定准则完全同构。
7. 状态跨任意 delta 边界，不依赖“连续几个 delta 相等”，不跨不同字段/调用合并。

规则应能检测事故中的持续 `_auditfree.md_pluginless.tmp.md...` 短周期重复，也会拦连续大量单字符输出。不会仅因几行相似 Markdown、循环代码、license、多语言文本或 JSON 尚未闭合就判错。大段有意重复的测试数据仍可能被拦，这是默认安全取舍。

首期不做模糊相似度、解码所有 JSON string 内容后的重复判定、压缩率或熵阈值。长周期、间歇或带变化循环可能漏检，由字节/事件预算兜底。

### 5.3 delta、done 和 terminal 快照

- 同一调用可能经 delta、arguments.done、output_item.done、response.completed.output 多次呈现全文；完整快照是替换/确认，不是 append，不得反复累计同一参数。
- 每 channel 保留已计费长度高水位；快照增长只补差额，缩短不返还 request 预算。
- **快照扫描预算、终态确认许可与累计成本界**：
  - 审查反馈指出：若单请求有 65536 个事件，每次均发送 2 MiB 替换快照（不增长字节高水位），若全量线性扫描 2 MiB，理论上累计扫描量可能高达 128 GiB 同步全扫；而若采用“首尾指纹一致即短路跳过扫描”的启发式规则，攻击者可反复替换 2 MiB 快照的中部，绕过重复检测并可持续泛洪；进一步地，若对纯终态确认快照实施无界零计费放行，且每次比对需计算 2 MiB 全文 Hash，攻击者可通过 2 MiB 非重复 append 耗尽一半预算，首个 2 MiB 终态快照全扫耗尽剩余扫描预算，随后发送 65536 次同内容 terminal 快照，因 Hash 一致零计费放行，导致代理执行 128 GiB 的 O(n) Hash 计算和巨量下发泛洪。
  - 为此，**彻底废除首尾指纹短路跳过机制，不依赖任何局部采样声称全文相同，且明确 Hash 校验与下发绝非免费工作，必须严格受控**：
    - 任何进入重复扫描器的快照必须全量参与线性扫描与周期检测，并统一由**单 channel 累计扫描处理字节预算（cumulative scan budget）**施加严格硬上界：
      - 普通 text/thinking channel 的扫描预算上限为 `2 * output_guard_max_channel_bytes`（默认 `2 * 2 MiB = 4 MiB`）；
      - 工具 channel 遵循与工具参数相匹配的更严格上限：`2 * output_guard_max_tool_argument_bytes`（默认 `2 * 256 KiB = 512 KiB`）。
      - 对于增量 append，滑动扫描消耗的扫描预算恒等于新增字节数；对于完整快照，每次常规扫描按该快照长度扣减扫描预算。
    - **纯终态确认许可（Terminal Confirmation Quota）与成本定量界**：
      - 为合法终态链（如流式 delta 结束后相继到来的 `arguments.done`、`output_item.done`、`response.completed`）建立严格的小常数许可机制：每个 channel 分配**最多 3 次纯终态确认许可，且每种合法终态帧类型至多 1 次**（仅覆盖白名单终态帧类型：`arguments.done`、`item.done`、`response.completed` 及其跨协议等价物）。
      - **先检查资格再作全文 Hash（一律无资格绝不提前 Hash）**：收到快照时，先核实该帧是否属于上述白名单终态类型且该类型许可尚未消耗。无资格的快照（非白名单帧、重复同类型帧、中间替换快照、别名复用帧或超出 3 次总配额）**严禁提前计算全文 Hash**，一律视为普通快照走常规计费 `cost = len(snapshot)`，受剩余扫描预算硬限制。
      - **Hash 校验与单遍历流水线**：仅当具备资格时，才允许对快照进行全文 Hash 校验：
        - 终态首帧（如 `arguments.done`）在消耗扫描预算执行周期检测时，Hash 计算与周期检测在同一次内存遍历中流水线完成，记录为基准 Hash（`terminal_baseline_hash`）；
        - 后续具备资格的终态帧（如 `output_item.done`），比对其全文 Hash 与基准 Hash：若严格匹配，确定本次扫描成本 `cost = 0`，消耗本类型许可 1 次，直接放行；若 Hash 不匹配（内容发生变化或被篡改），**该类型许可立即标记为已消耗废弃**，本次快照直接判定为变体快照，扫描成本恢复为全量 `cost = len(snapshot)` 并转入常规扫描预算检查。
      - **独立定量的额外成本界**：3 次纯终态确认许可仅豁免扫描预算扣减，其额外引入的物理 Hash/比较/下发工作量严格有界，单 channel 最多额外处理 `3 * channel_limit` 字节（普通 channel 最多 `3 * 2 MiB = 6 MiB`，工具 channel 最多 `3 * 256 KiB = 768 KiB`）。单 channel 包含常规扫描预算（`2 * limit`）与额外终态许可（`3 * limit`）在内的**整体最大数据处理与校验量严格受限于 `5 * limit`**（普通 channel 10 MiB，工具 channel 1280 KiB；单请求 128 channel 累计处理量绝对上限为 1280 MiB ≈ 1.25 GiB），彻底终结无界 Hash 与下发泛洪。
  - **唯一明确的判定优先级与执行顺序**：每个事件（无论是 append 还是快照）在进入 guard 时，严格按以下步骤依次执行判定，杜绝未定义的歧义与判定冲突（将资格/等价性检查与 cost 计算严格前置于预算检查，消除正常终态在预算为 0 时被误拒的矛盾）：
    1. **事件预算检查**：请求级业务事件计数累加 1；若 `request_events > 65536`，立即拒绝，reason 固定为 `request_events`。
    2. **Channel 数量检查**：若为新 channel，检查请求级累计 channel 计数；若 `channel_count > 128`，立即拒绝，reason 固定为 `channel_count`。
    3. **Delta 计数检查（仅工具 channel 且为增量 append）**：若工具调用发生非空 arguments delta append，该 channel 的 delta 计数累加 1；若 `tool_argument_deltas > output_guard_max_tool_argument_deltas`（默认 4096），立即拒绝，reason 固定为 `tool_argument_deltas`。
    4. **语义字节预算检查（已计费长度高水位推进）**：
       - 若为 append：计算拟推进的高水位 `new_highwater = current_highwater + len(delta)`；
       - 若为快照：若快照长度 `len(snapshot) > current_highwater`，则拟推进高水位为 `new_highwater = len(snapshot)`，增量 `growth = len(snapshot) - current_highwater`；若 `len(snapshot) <= current_highwater`，高水位不增（`growth = 0`），且不返还 request 预算。
       - 检查 Channel 字节上限：工具 channel 若 `new_highwater > output_guard_max_tool_argument_bytes`（默认 256 KiB），立即拒绝，reason 固定为 `tool_argument_bytes`；普通 channel 若 `new_highwater > output_guard_max_channel_bytes`（默认 2 MiB），立即拒绝，reason 固定为 `channel_bytes`。
       - 检查 Request 语义字节预算：若 `request_bytes + growth > output_guard_max_request_bytes`（默认 4 MiB），立即拒绝，reason 固定为 `request_bytes`。
       - 仅当上述检查全部通过后，正式提交高水位与 `request_bytes` 的更新。
    5. **资格/等价性核验与扫描成本确定（前置确定成本）**：
       - 若为增量 append：确定扫描成本 `cost = len(delta)`；
       - 若为完整快照：
         - **资格检查**：核实当前 channel 是否已记录 `terminal_baseline_hash`，且当前帧属于合法终态类型（`arguments.done`、`item.done`、`response.completed` 及其协议等价物），且该帧类型许可未被消耗（`terminal_permits_used < 3` 且本类型未见）。
         - **具备资格**：计算当前快照的全文 Hash 并与 `terminal_baseline_hash` 进行比对。
           - 若 Hash 完全一致：判定为合法纯终态确认帧，标记该帧类型许可已消耗（`terminal_permits_used++`），**确定本次扫描成本 `cost = 0`**；
           - 若 Hash 不一致（变体快照）：标记该类型许可已废弃消耗，**确定本次扫描成本 `cost = len(snapshot)`**。
         - **无资格**（未处于终态阶段、非终态类型、重发同类型、超出 3 次许可或别名复用）：严禁提前进行任何全文 Hash 计算，**直接确定本次扫描成本 `cost = len(snapshot)`**。
    6. **扫描预算扣减与检查**：
       - 若 `cost > 0`：检查该 channel 剩余扫描预算。若 `remaining_scan_budget < cost`（包括剩余预算已为 0 或不足以支付本次快照扫描），**拒绝执行超额扫描**，立即拒绝触发事件并终止请求，reason 固定为：工具 channel 为 `tool_argument_bytes`，普通 channel 为 `channel_bytes`（沿用现有稳定 reason，不引入未定义的新 reason）。扣减扫描预算：`remaining_scan_budget -= cost`。当 `remaining_scan_budget == cost` 时扣减后恰为 0，允许本次扫描执行；
       - 若 `cost == 0`（经步骤 5 严格核验通过的纯终态确认快照）：不扣减扫描预算，跳过步骤 7 的重复周期扫描（其基准内容已在首帧通过全量检测），直接放行该终态事件。
    7. **周期性重复检测扫描**：
       - 仅在 `cost > 0` 且扣减对应扫描预算后执行。
       - 执行全量线性周期检测（使用固定 ring buffer 或 prefix function 线性求周期）；若当前为终态首帧，在扫描遍历中同步流水线计算并保存 `terminal_baseline_hash`。若命中连续 8 KiB 短周期（`1 <= p <= 256`），立即拒绝，reason 固定为 `repetition`。
       - 若未命中，放行该事件。
    8. **终态链兼容性保证（正常流不误拒分析）**：
       - 在合法的生命周期链中（例如流式 delta 累计至 256 KiB 工具参数，后随 `arguments.done`、`output_item.done`、`response.completed.output` 3 次相同完整快照确认）：
         - 工具 channel 的累计扫描预算为 `2 * 256 KiB = 512 KiB`。流式 delta append 阶段累计消耗 256 KiB 扫描预算；剩余扫描预算恰好为 `512 - 256 = 256 KiB`。
         - 当紧随其后的首个合法完成快照（如 `arguments.done`，256 KiB）到达时，因尚无 baseline hash，走步骤 5 无免除资格，确定 `cost = 256 KiB`；步骤 6 发现 `cost == remaining_scan_budget`，扣减后剩余预算恰为 0；在步骤 7 完成最后一次全量扫描并通过，同时同遍历计算并记录 `terminal_baseline_hash`，放行该帧；
         - 当紧接着的 `output_item.done` 与 `response.completed` 到达时，在步骤 5 通过资格审查并经 Hash 比对一致，确定 `cost = 0` 并分别消耗 1 次许可；在步骤 6 中即使 `remaining_scan_budget == 0`，因 `cost == 0` 顺利通过并直接放行，彻底消除了旧版在剩余扫描预算为 0 时误拒正常终态帧的顺序矛盾；
         - 若上游在后续终态帧中篡改内容或发送变体快照，步骤 5 比对 Hash 失败，废除许可并确定 `cost = 256 KiB`，在步骤 6 因 `remaining_scan_budget (0) < cost (256 KiB)` 被坚决拦截并以 `tool_argument_bytes`（普通 channel 为 `channel_bytes`）终止；若上游重发同类型终态帧或发送第 4 次终态确认帧，在步骤 5 因无资格直接确定 `cost = 256 KiB`，同样在步骤 6 被立即拦截，杜绝任何利用变体或泛洪快照绕过防护与无界消耗 CPU 的可能。
- 每个接受的业务事件都计事件预算，因此即使是零重复快照也必须计入 `request_events`，反复发送同一快照不能无界消耗事件数。
- done-only、terminal-only、非流式 JSON 同样检查；此前未出现的工具调用/文本创建 channel 并计费。
- 最终内容不一致时额外执行完整检查，不因流式阶段通过就豁免。完整快照可用临时有界检测器，不能释放既有预算。
- 任一项触发则整事件拒绝。绝不截前 N bytes 后当作有效完整工具调用返回。

### 5.4 算法成本与 framing

append 的累计扫描成本应为 O(N)，不因累计参数长度增长而对每个 delta 全文扫描。8192-byte 窗口每 1024-byte 检查一次属于固定倍数工作。完整快照受单 channel `2 * max_channel_bytes` 累计扫描预算硬限制；配合至多 3 次合法纯终态确认许可的全文 Hash 校验与下发（额外至多 `3 * max_channel_bytes`），单 channel 总数据处理与校验量严格受限于 `5 * max_channel_bytes`（普通 channel 最多处理 10 MiB，工具 channel 最多处理 1280 KiB；单请求 128 channel 最多处理 1280 MiB ≈ 1.25 GiB），杜绝 128 GiB 同步全扫与 Hash 泛洪风险。原声称未计费工作也满足 `2 * limit`（如单 channel 4 MiB、单请求 512 MiB）的界不再严格成立，现已统一修正为上述严格定量界（扫描计算量至多 `2 * limit`，校验与处理总量至多 `5 * limit`）。

活动 channel 的窗口与辅助数组按常量空间分配，终止/结束后释放；不能保存全部历史 delta、参数或每个检查点的副本。request 最大额外状态须在实现基准中报告。

受保护 SSE reader 必须在拼接前限制未完成行和 event，默认复用现有 8 MiB 资源上限。native reader/WS/媒体已有更高上限时，保留明确的 transport 限制，不用工具预算代替媒体帧上限，不改变专用媒体接口。先限制 envelope 大小再解析，非法/过大 frame 走资源错误，不尝试 repair。

非流式仍须有该渠道明确的有界读取策略，缺失时在接入前补齐，并为媒体路径保留单独预算。本规格不宣称语义预算限制了所有媒体内存。

## 6. 命中后的强制终止契约

### 6.1 原子、幂等和错误归因

状态为 `active → tripped → closed`，第一个 guard 原因固定；命中后后续模型事件不再进入消费者。释放/取消可重复调用，下游最多尝试写一个 guard 终态。

拟使用稳定公开 code `model_output_guard_triggered`，内部 reason：`repetition`、`tool_argument_bytes`、`tool_argument_deltas`、`channel_bytes`、`request_bytes`、`request_events`、`channel_count`。framing 资源故障保留独立原因，不伪装重复。

命中必须优先于它造成的 `context.Canceled`、read/write error、EOF 和迟到成功终态分类，不得落为普通 499、断流、鉴权失败或成功。typed guard 标记须保留到最外层 request outcome。

### 6.2 立即停止上游与本地缓冲

1. 固定 guard 状态和原因，再主动 cancel **本次上游** context，关闭 HTTP body 或废弃所属上游 WS lease/连接。
2. 不走普通断线后为 usage 继续 drain 5 秒的路径，不等上游 completed。
3. 丢弃尚未提交的 replay、collector、自动续想、pending-first-token、扫描窗及 flush/coalesce 缓冲；不能在 `defer Finalize/Flush` 中再次释放异常内容。
4. callback 停止并保留 typed error。仅返回 false、仅停下游写、只 cancel 下游 context，均不算完整实现。
5. 失败通知使用受限独立写入路径，不重经已拒绝的 guard，也不顺便 flush 旧内容。复用现有写锁与下游有界写超时，断开或阻塞时不无限等通知送达。
   - **执行前置与锁解耦原则**：guard 命中时**第一动作必须是在前主动 cancel 本次上游 context 并丢弃本地在途缓冲（步骤 1-3），绝不等待写锁**；通知写入属于尽力交付（best-effort），锁争用绝不延迟或阻塞上游 cancel 与资源回收。
   - **HTTP SSE 终态写入有界锁获取与安全写超时路径**：不同于 WebSocket transport 原生具备 `responsesWSWriteTimeout`（30s）以及 `conn.SetWriteDeadline`，标准 Go `net/http` ResponseWriter 仅通过抽象接口与下游交互。在当前 Gin 1.12 与 Go 1.26 运行时下，底层网络写入、锁竞争与超时控制遵循明确工程约束与安全路径：
     - **有界锁获取（TryLock / 有界等待）**：终态通知写入尝试获取 `downstreamMu` 互斥锁。为杜绝锁被保活 ticker 或普通写协程长期持有（或发生未预期阻塞）而导致通知协程无限挂起，终态写入**获取锁必须严格有界**：采用 `TryLock` 进行非阻塞加锁（或带短超时尝试）；若加锁失败，立即放弃发送下游错误通知并记录脱敏审计日志，**绝对不引入孤儿等待协程**，直接进入连接关闭与主流程退出。
     - **持锁内全部写操作设 Deadline 规范（约束现有事实）**：在成功取得 `downstreamMu` 后，明确 5 秒写超时**仅约束持锁后的实际写入阶段**。同时规格明确建立约束：**锁内发生的所有写操作（包括日常数据帧写入、保活 ticker 发送注释 keepalive 以及 guard 终态写入）均必须在写操作前调用 `rc.SetWriteDeadline(time.Now().Add(writeTimeout))`**，彻底杜绝任何无 deadline 的持锁写入导致下游慢客户端反向卡死代理写锁。
     - **ResponseController 主路径**：Go 1.20+ 引入且 Gin 1.12 完全支持的 `http.NewResponseController(c.Writer)` 为标准主路径，持锁后终态写入前调用 `rc.SetWriteDeadline(time.Now().Add(writeTimeout))`（如 5 秒），将写超时推入底层 TCP/TLS 连接，随后执行错误帧写入与 Flush。
     - **不可达降级分支与绝对安全退出**：若 `SetWriteDeadline` 返回 `http.ErrNotSupported`（例如某些反向代理封装或自定义 Mock ResponseWriter 不支持 deadline 控制），规格明确**不承诺无法兑现的全局 5 秒写超时保证**。在此类不支持 deadline 的底层上，写入前先检查 `c.Request.Context().Err()`：若下游已断开立即放弃写入；若未断开，亦**严禁尝试潜在无界阻塞的同步 write**，代理直接放弃错误帧写入并安全退出，立即关闭或废弃连接。
     - **严禁使用已失效机制与不受控并发**：Gin 在已写入响应体（SSE headers 与前置数据 chunk 已发出）之后，调用 `c.Writer.Hijack()` 会因底层已被包装接管或状态冲突而失败，因此**彻底废除将 Hijack 作为 SSE 写入后的兜底路径**；同时**严禁通过启动孤儿 goroutine + timer 拼死写入**，杜绝协程泄露与连接竞态。
     - **HTTP/2 协议现实**：在 HTTP/2（h2c / HTTP/2 下游）传输下，`http.ResponseWriter` 不支持 Hijack 且 `SetWriteDeadline` 对单个 stream 的写入控制取决于 Go 标准库具体行为，不能假设支持底层连接级别劫持，遇到不支持即按上述安全退出路径放弃写帧、关闭/重置 stream，保护代理主流程即刻回收。
     - 若写入阻塞、超时或报错，立即关闭连接或标记 `clientGone`，直接退出，不阻塞代理的主流程回收。
6. 释放账号/API Key 并发、WS pending request/lease、replay 文件和 goroutine，不关闭共享账号上的无关连接。
7. 中止且未收到真实终态的上游 WS 不归还健康池，不让迟到帧污染下一请求。多路下游优先只终止所属 stream；若协议无法隔离，关闭相应连接并记录影响范围，不无依据承诺其他复用流不受影响。

### 6.3 禁止代理内部重新拉起

guard 是请求级硬终态，不可由普通重试、首包前静默换号、continuous retry selective/catch-all、WS→HTTP fallback、缺失上下文恢复、自动压缩、自动续想重放。

不可仅设置 `penalize=false` 或在默认 retry code 集合中排除；须在各选择器及 fallback/continuation 入口优先识别 typed guard。取消前排队的本次后续 attempt 不再 dispatch。

不扣账号健康分、不封禁账号、不触发 token refresh 或模型冷却。输出退化不是账号故障的充分证据。

### 6.4 下游协议行为

| 状态/协议 | 结果 |
| --- | --- |
| HTTP 尚未提交头 | HTTP 502，入口协议标准错误 envelope，稳定 guard code 及脱敏消息；不是请求体过大，不用 413。 |
| Responses SSE 已开始 | 一次 `response.failed`，status=failed，保留已知 response identity；`response.error.code=model_output_guard_triggered`；不追加 completed。 |
| Chat SSE 已开始 | 标准 error envelope，`error.code=model_output_guard_triggered`；不追加成功 finish chunk 或 `[DONE]`。 |
| Anthropic SSE 已开始 | `event: error` 标准结构，携带 guard code；不伪造成功 message_stop/tool input 完成。 |
| Gemini 已开始 | 复用项目 native Gemini 客户端兼容的错误输出方式并保留诊断；不能假设新增 `event:error` 会被 SDK 识别。 |
| Responses WS | 当前 response/stream 的失败事件，保留 stream/response 关联，不伪造全连接成功或其他 stream 终态。 |
| 非流式 | 只返回错误，不返回部分工具参数或半个 JSON 成功响应。 |

公开消息示例：`Upstream model output exceeded the gateway safety policy; generation was stopped.` 可公开短 reason 枚举，不回显模型原文、工具路径、prompt 或密钥。连接不可写时只记录无法通知，不把 outcome 改成普通断流。

### 6.5 持久化、缓存和计费

- 不写成功 response cache、续链快照、成功回放或成功上下文；未成功交付的 response ID 不新增为可用续链绑定。
- 已发送的 ID/部分内容无法撤回，本地标明失败，不能在后续重建时当作完整成功响应。
- usage 仅用命中前可靠观测数据；未知保持未知，不伪造精确 token，不为补账继续读取失控流。
- 请求/用量日志记录独立失败类别 `output_guard`；流已发 200 也按失败记，不根据 HTTP 状态判成功。
- guard 不进入 prompt 风险封禁、CYB strike 或账号健康惩罚体系。

## 7. 全局配置与管理界面

新增独立“模型输出失控防护”设置分组，不放在提示词过滤开关下面。提供 §5.1 五项设置、单位、默认值、风险说明、恢复默认。固定重复算法参数及 channel/事件资源上限首期不开放任意调整。

校验建议：工具 bytes 为 16 KiB～4 MiB；工具 deltas 为 256～65536；channel bytes 为 128 KiB～16 MiB；request bytes 为 256 KiB～64 MiB；满足工具 bytes ≤ channel bytes ≤ request bytes。数值项仅接受整数，开关仅接受布尔值；零/负数、溢出和本组未知字段拒绝。调高字节阈值不自动放宽其他上限。

复用 admin settings GET/PUT 和 `RuntimeSettings` 快照，持久化/迁移覆盖 SQLite/PostgreSQL。旧字段缺失补默认，不能用 bool 零值覆盖显式关闭（存储层和内存结构统一采用 `*bool` 三态指针语义，缺失映射为 default `false`，显式 `true`/`false` 则准确反序列化并落库）；写入仅更新本组字段，使用短事务/原子合并（参考 `database/response_cache_settings.go` 的独立窄更新模式），避免陈旧整份 settings 覆盖无关设置。

入口取得不可变快照与 revision（`generation`）；长连接每个新逻辑 turn 取快照，在途不热切换。保存后本实例立即应用；多实例通过每 5 秒重载防护配置 revision 更新（标注：此为新增机制，现有 `RuntimeSettings` 无 revision 机制，本设计明确参考已有的 `database/response_cache_settings.go` 之 `Generation` 自增版本控制及 `proxy/response_cache_settings.go` 的 5 秒后台轮询 poller 先例落地）。更新失败保留 last-known-good 并记录诊断，不回退关闭。

首次启动冷启动无有效配置且 DB 读取失败时不接受生成流量，不能静默关闭。冷启动拒绝时统一遵循协议形态返回 **HTTP 503 Service Unavailable**，并携带稳定公开错误码 `api.ErrCodeServiceUnavailable`（如 `service_unavailable`）与明确描述（如 `Service configuration unavailable; failed to load output guard settings`），而不是混用 guard 运行时的命中错误码（`model_output_guard_triggered`）。对于 WebSocket / Realtime 连接，在连接握手或首帧响应时返回带有 503 / CloseTryAgainLater 语意的协议错误帧并关闭连接。旧字段缺失和 DB 不可用是两种严格区分的情况。

仅管理员能全局关闭；不接受请求头、模型输出、普通 API Key 参数覆盖。关闭状态在管理界面显示高风险提示并有审计，transport/framing 资源限制仍保留。

## 8. 可观测性与隐私

每次命中一次结构化事件，复用请求/用量日志：request ID、stream ID、endpoint、渠道、模型、账号 ID、attempt、配置 revision、reason、limit、observed bytes/events、适用时的重复周期/窗口、已写下游字节数、头是否提交、取消和释放结果。

不默认记录完整参数、尾部样本、文件内容、prompt、API Key、Bearer token 或账号凭据，也不把全文藏进 `failureMessage`/`failurePayload`。

日志按 `output_guard`/reason 筛选；统计区分重复、工具预算、请求预算及无法通知。指标标签只用有界维度，不将 request ID、call ID 或全文 hash 当作高基数 label。

## 9. 验收规格

均使用合成/脱敏 fixture、mock upstream 和测试客户端，不向线上 DSH 注入失控流，不依赖真实账号或付费模型。

### 9.1 检测正确性

1. 有效 outer SSE JSON、未闭合 inner arguments、持续事故样式短周期重复：在重复阈值附近触发，不等到 256 KiB。
2. 同语义流按 1-byte、小/大 delta、随机边界分割均可检测，切分不能让重复消失。
3. 单大 delta 中间有任意起点、长度至少 9215 bytes 的精确短周期片段，尾部正常仍触发，整事件下游不可见；另测恰好 8192 bytes 的检查点对齐与非对齐边界，不断言非对齐片段必命中。
4. 多 tool/text/thinking 交错不互相污染，ID/index 补发不重置计数。
5. 非重复参数超 bytes、细碎参数超 delta、跨 channel 绕单项上限、空 delta 洪泛、新 channel 洪泛，分别由对应预算拦截。
6. 等值允许、超一拒绝；中文、emoji、escapes、int64 overflow 均测试。
7. done-only、terminal-only、native object、非流式、更大/变化的最终快照不能绕过。
8. delta → done → item.done → completed 同全文不重复扣字节；对于具备资格且全文 Hash 完全一致的终态确认快照实施零计费放行，受每 channel 最多 3 次小常数许可约束且严格消耗 `request_events`；构造具体可测用例：
   - 用例 8a（恰好耗尽与超额拒绝）：对于上限 256 KiB 的工具 channel（扫描预算 512 KiB），以 256 次 1 KiB delta 耗尽 256 KiB 扫描预算（剩余 256 KiB），首个 256 KiB 快照到达，因尚无 baseline hash 判定无免除资格，所需扫描成本 `cost = 256 KiB` 恰好等于剩余预算，扣减后剩余预算降为 0 并建立基准 Hash；随后若再次收到一个内容发生微小篡改（如仅修改中部 1 字节）的 256 KiB 变体快照，因 Hash 不匹配废弃该许可并确定 `cost = 256 KiB > remaining_scan_budget (0)`，断言在第 6 步立即拒绝触发事件并终止请求，reason 固定为 `tool_argument_bytes`，杜绝超额扫描；
   - 用例 8b（合法终态链有资格零计费放行与无资格拒绝）：在用例 8a 剩余扫描预算为 0 后，相继送入内容完全一致的 `output_item.done` 与 `response.completed` 快照，断言通过资格检查且 Hash 匹配，均零计费放行（`cost = 0`，仅扣 `request_events` 并消耗对应许可），请求顺利完成不被误拒；若随后再次发送同类型快照（如重复的 `response.completed`）或第 4 个终态确认快照，断言因无资格直接确定 `cost = 256 KiB`，在第 6 步被拒并终止；
   - 用例 8c（中部短周期重复与小长度用例健全性断言）：
     - 构造包含连续短周期片段且长度达到 9215 bytes 的快照（如中部连续 9215B 周期重复片段），断言全量线性周期检测器能确保落入检查窗口并准确触发 `repetition`，杜绝长重复逃逸；
     - 另测单 channel 累计总长度小于 8192 bytes（例如总长约 2128 bytes，中部包含 2000 bytes 周期片段）的用例，断言严格遵循 §5.2 尾检规则未满 8192 窗口不判定重复，仅扣减扫描预算与字节高水位，绝不误触发 `repetition`；
     - 彻底删除已废除的首尾 64B 指纹短路跳过逻辑残留；测试覆盖在变体或高频快照反复推送时受单 channel 累计扫描预算硬上界（`2 * limit`）和纯终态确认许可（最多 3 次 `cost = 0` 全文校验）联合截断，断言单 channel 累计扫描处理量绝不超过 `2 * max_channel_bytes`，全量数据校验与处理总成本绝不超过 `5 * max_channel_bytes`（普通 channel 10 MiB，工具 channel 1280 KiB）。
9. 正常代码、非重复 Markdown、不同文件多工具、合法未闭合 JSON、中英混合、image base64 字段、心跳不因形态相似被拦；明示硬预算仍生效。
10. 有意大段周期数据按规则拦，作为兼容性 fixture，不宣称零误报。
11. 尾检规则断言：对于总长度小于 8192 bytes 的短正常流（如 500 字节的短代码或高频词短回复），在收到 `end` 或快照时断言绝不误判为重复；对于已满 8192 bytes 但尾部未对齐 1024-byte 的末尾流，断言能准确提取最新的 8192-byte 窗口完成严格周期判定。

### 9.2 全链路终止

12. §4.1 每入口 stream/non-stream、native/converted 及实际支持的上游 HTTP/WS 组合有集成用例；不支持的明确标注。
13. collector/replay 大量积累之前检查；continuous retry 开启且 upstream 永不成功结束也在线触发。
14. mock upstream 观察到 cancel/body close，无额外 5 秒 usage drain，原因不被 cancel/EOF 覆盖。
15. 保活/自动续想与 guard 并发无旧缓冲泄露、无成功/重复终态、无 data race。
16. ordinary retry、selective/catch-all、换号、fallback、恢复、续想均不能拉起下一请求，用 upstream 调用次数断言。
17. 头未提交返回协议错误，已提交返回失败；Chat 不附成功 `[DONE]`；Gemini 走既有 SDK 兼容失败路径。WebSocket 命中终态：error 帧（公开码 `model_output_guard_triggered`）+ `CloseTryAgainLater`(1013) 关闭；同一连接多 lane 下终态写经 per-conn 写锁串行。
18. 慢读/断开时取消不被通知写阻塞，按有界写超时结束；HTTP SSE 失败通知写入具备配套断言：断言在执行下游失败通知前已主动 cancel 上游 context 并不受写锁阻塞；断言终态写入以非阻塞方式（TryLock 或带超时）获取 `downstreamMu` 互斥锁，竞争失败时立即放弃写通知并安全退出，不产生孤儿等待协程；断言锁内全部写入（含普通数据帧、keepalive 注释及终态错误帧）均通过 `http.ResponseController.SetWriteDeadline` 设置有界超时（如 5 秒），持锁写超时只约束持锁后的实际写入阶段，在下游客户端挂起时不产生长期悬挂写协程，超时后连接能正确回收且主处理流程立即退出；对于不支持 `SetWriteDeadline` 的环境，断言放弃潜在阻塞写入并安全退出，不使用孤儿 goroutine 拼死写入；命中后不下发新模型事件，不断言能撤回网络内字节。
19. 不落成功 cache/续链，无账号降分/冷却/refresh/prompt strike；缺 usage 不虚构。
20. 两 HTTP 请求/可隔离 WS streams 同时运行，异常只终止自身；上游异常 lease 不复用，资源回基线。

### 9.3 配置、性能与上线门槛

21. SQLite/PostgreSQL 迁移默认关闭，显式开启/关闭持久化；非法配置不影响快照；并发无关保存不丢字段。
22. 本实例即时、其他实例 5 秒轮询更新；在途旧 revision、新 turn 新 revision；DB 故障保留 last-known-good，冷启动故障返回 503 且不静默放行。
23. 未完成 SSE 行/多行 event 拼接前限额；大 WS frame、native reader、非流式有明确 transport 上限。
24. 固定空闲测试机和配置，至少 5 轮比较开关：正常 1 MiB 文本 p95 TTFT 增量目标 ≤10 ms、CPU/吞吐开销目标 ≤20%；不满足先优化或评审修订，不声称已达标。
25. core append 的 1/2/4 MiB 非重复样本 CPU 中位数应近线性，每翻倍比值目标 ≤2.5；分别报告 envelope 投影、扫描、snapshot 成本，不用网络等待掩盖 O(n²)。
26. 每 channel 新增状态目标 ≤64 KiB；128 个累计 channel 无无界状态，结束释放窗口。10 分钟合成异常/并发退出测试无持续堆内存、goroutine、租约或临时文件增长。
27. 提交默认 256 KiB/4096 deltas/8 KiB 重复窗口的正常样本误拦及事故样式提前触发结果；增加细 delta 长推理样本专项压测（测试模拟极端细 delta 如单字节持续流式输出、长思考以及多工具交错触发 4096 deltas 场景，量化事件预算相对字节预算更早到达的耗尽时机与行为），未验证不称通用安全阈值。
28. 不以 DSH CPU 恢复或 GUI 不再卡死作为仅代理可保证的验收项；可另行授权真实客户端验证，不代替本项目契约。

## 10. 已由用户确认的设计取舍

以下设计取舍已由用户明确确认，设计审查验收通过：

1. 默认全局关闭，仅管理员可开，与 prompt filter 独立。
2. 首期精确短周期重复 + 硬预算，不上语义模型，不自动改写/拆分工具。
3. 默认工具参数 256 KiB、4096 非空 deltas；合法大 write 也拦，后台可显式调高。
4. 只结束本次代理请求/逻辑 WS turn，代理不重放，不惩罚账号。
5. 覆盖文本/工具入口 stream/non-stream/native/converted 及 Realtime WebSocket 桥接的文本/工具子集；专用媒体接口、Realtime 原生音频 Modality（显式拒绝）及协议管理帧不纳入语义重复检测。
6. 只承诺阻止进一步下发，不承诺恢复卡死客户端、撤销已执行工具或禁止客户端自行重试。

设计审查验收通过，后续实现计划尚未开展；本轮不生成实施计划、不修改业务代码、不部署或重启服务。
