# 使用日志请求诊断

使用统计的“请求类型”列展示每条新日志的网关判定，点击读取该日志的诊断详情。桌面表格和移动端卡片均支持，列可隐藏。详情仅开放给管理员，可以复制脱敏 JSON。

## 分类与证据

| 类型 | 含义 |
| --- | --- |
| `user` | 已解析用户根，来源为 user 或未指定 |
| `related_internal` | 调度时允许被动调用，且身份关联到根 |
| `independent_internal` | 调度时允许被动调用，但没有关联到用户根 |
| `related_unclassified` | 关联请求，没有取得被动授权 |
| `compaction` | 压缩端点或已解析的 compaction 请求 |
| `gateway_internal` | 网关自身标记了 internal_reason 的请求 |
| `unknown` | 证据不足，不能确定类型 |

这是网关实际判定，不是客户端身份认证。UA、指纹、模型名称或提示词不用于诊断层重新推断类型。旧日志显示“未记录”，不回填猜测值；无请求上下文的新日志标记 `capture_status=not_available`。

`incoming` 分别保存允许记录的 HTTP 头、turn metadata、client_metadata（包含有界嵌套来源）、已验证 NewAPI 元数据。缺失、非字符串类型和过大的元数据有明确标记；别名分开保留，避免掩盖冲突。WS 使用已解包的单帧请求元数据，握手头仍单独展示。

`resolved` 保存身份解析结果，包括来源、根状态、关联关系、是否持有用户根、根 ID/指纹、窗口/亲和键摘要、被动授权、窗口豁免、是否进入无根调度、是否要求主根账号。`audit` 和 `dispatch` 分别记录内容审核和模型调度阶段的被动授权及模型豁免开关；未执行阶段不伪造结果。`passive_models_allowed` 表示具备豁免资格，不表示该账号原来的模型列表一定不允许此模型。阶段判定变化时显示警告。

## 选号与窗口

- `root_account_lookup`：`not_checked` 未走到已有查询，`missing` 查询未找到，`found` 查询得到账号。仅复用已有查询结果，不新增查询。
- `selection`：`matches_observed_root` 最终账号与曾观察到的根/亲和账号相同（不单独证明强制锁号）；`recent_account` 命中最近账号；`unlinked_scheduling` 无根普通调度；`scheduled` 其他正常调度；`no_account` 未选中账号。
- `recent_account.result`：`not_attempted` 未尝试；`not_applicable` 不满足条件；`missing` 无记录；`cache_error` 读取失败；`invalid_record` 记录无效；`expired` 超出回溯时间；`after_request_start` 候选记录不早于本次请求；`account_unavailable` 候选未通过现有调度检查；`selected` 已取到候选。
- `candidate_rejections` 是现有选号 trace 收集的本轮候选排除原因，可能包含多个账号的结果，不应全部归因于最终账号；空列表不证明候选都可用。
- `user_window`：`not_reached` 未进入检查；`disabled` 用户限制未启用；`account_windows_disabled` 所选账号窗口未启用；`passive_exempt` 现有分支豁免；`identity_missing` 缺少稳定身份/主体；`identity_conflict` 身份冲突拒绝；`reused` 复用已有窗口；`related_no_new_window` 关联请求不创建新窗口；`created` 本次创建；`cooldown_rejected` 创建 CD 拒绝；`limit_rejected` 数量上限拒绝。
- `account_window` 是所选账号的窗口准入快照：`disabled` 未启用；`rejected` 用户侧准入拒绝；`exempt_or_unstable` 不具备可计数身份或被豁免；`root_reused` 关联请求沿用根；`reused` 之前已有该账号绑定；`admitted` 通过本次准入。它不是事后重新查询的最终窗口持久化结果。

请求保存开始/完成时间（UTC）、请求关联 ID、已验证 NewAPI 请求 ID、重试序号。每次重试清除上一轮最近账号和准入结果；每个 WS 帧清除上一帧诊断，日志落库的是不可变 JSON 快照。日志原有 `created_at` 是写日志时间，不替代请求开始时间。

## 会话内切换模型

有效账号窗口或会话粘性绑定存在时，切换到绑定账号不支持的模型返回 400 `session_model_unavailable`，提示“当前会话绑定的上游账号不支持所选模型，请新开对话后使用该模型。”不会因模型不匹配删除原绑定、重选其他账号或新增账号窗口。切回原账号支持的模型仍可继续；新建会话可以正常选择支持目标模型的账号。过期绑定不被当成仍有效的会话锁。

此检查覆盖 Responses、compact、Chat、Messages 和 Responses WebSocket；复用现有模型白名单及账号级模型映射语义。获得既有被动模型豁免的标题/子请求不因此被拦截。账号冷却、限额、暂停等原有故障处理保持不变，不把它们误报成“没有模型”。检查使用现有绑定和账号模型配置，无数据库迁移或历史用量扫描。

## 被动请求的主账号关联

独立后台、Guardian、子代理、记忆整理、标题和建议等非 `user` 来源均要求明确主根关联，不能在根账号缺失后降级为最近账号或普通选号。NewAPI 在同平台/用户/Token/设备范围内只允许唯一有效主会话候选，并保存原始后台根到主根的关联；明确父子关系沿既有根/Thread/Turn 绑定继承，不以最近的其他会话替代父根。已关联的独立后台在最终诊断中可显示为 `related_internal`，原始 `thread_source` 不变。

两端共享最多 60 秒的找根/账号绑定等待。NewAPI 已消耗的时间通过签名 `root_account_wait_millis` 扣除。无法证明主根关联的旧网关或直连独立请求返回 400 `codex_background_root_unavailable`，不会仅凭一个最近账号记录建立关联；有明确主根的请求才进入账号等待。等待不创建窗口，不持有上游账号或 API Key 并发位，超时返回 400 `codex_root_account_wait_timeout`。主账号不可用、被排除或不能执行当前模型时不改选其他账号；已有被动模型豁免和压缩绑定规则继续生效。

## 根命名与窗口授权

`thread_title`、`thread_title_reconsideration`、`thread_description` 共享最终主根的一次命名机会。账号/窗口准入完成后、上游调用前原子认领，等待超时或准入失败不认领。同一逻辑请求的内部传输重试可继续；新的命名请求返回 400 `codex_root_already_named`，不会跳过该根去选择另一个根。诊断 `naming` 为 `claimed`、`duplicate` 或 `unavailable`。记录按平台/人员/最终主根隔离，跨 API Key、实例和重启保留；设备收敛后的同一根也共享次数。记录从升级后开始，不扫描历史日志反推旧命名。`ambient_suggestions`、Guardian、正常用户请求不受命名次数限制。

未确认的用户窗口预留仍为 30 秒，与后台 60 秒等待分离。实际准入确认后通过签名 `X-Codex2API-Window-Grant` 回传正式授权，NewAPI 仅缓存明确确认的授权到原到期时间，不将请求成功等同于确认。账号未启用会话容量时，普通授权确认成 `no_window`，不占用户普通名额；扩容授权不能用于这种账号。

同根并发预留共用名额和倍率，但各自有独立 `reservation_id`；后台只读取主预留，不增加预留。释放只能移除自己的未确认预留引用，不能移除其他请求或正式授权。已确认窗口不因单个后续失败回滚，保持原到期时间和价格。

旧预留到期时优先识别同 ID 的已确认状态，否则返回 400 `window_billing_refresh_required`（未调用上游）。NewAPI 在未向客户端输出时最多重新申请一次，锁定原根和原渠道，重新计算倍率与预扣后重发，使用新的签名请求 ID 防止重放校验冲突。数据库故障返回 503，不再冒充预留过期。正常正式授权续用无需查询授权数据库，`window_grant` 诊断记录 `pending` / `confirmed`。

本地绑定/账号窗口准入发出按根通知，同一根的并行等待共享检查结果，跨实例每秒只查对应缓存键，不按请求数量重复查询或扫描用量表。等待注册有数量上限，断开请求会取消等待并清理订阅。WebSocket 返回对应错误帧；Messages 错误保留 Anthropic 结构并附带可供网关停止重试的错误码。

已保存的诊断增加 `root_account_wait`（`found`、`timeout`、`canceled`）和 `root_account_wait_millis`（本地实际等待毫秒数）。是否形成用量日志仍遵循原日志保存路径；未选号的超时以请求错误记录为准。绑定已存在但不可调度时仍按原有可用性/权限限制处理，不把所有 503 都改成 400。

`resolved.original_root_fingerprint` 保留 NewAPI 解析前的原始根；`root_fingerprint` 是实际关联主根。`root_association` 记录 `same_scope_unique`、`existing_alias`、`thread_binding`、`turn_binding` 或 `explicit_parent`；`root_candidate_count` 是关联时的候选数。等待结果还包括 `unresolved`（关联未建立）和 `unavailable`（等待容量或服务不可用）。这些字段来自现有解析结果和签名元数据，不额外搜索日志或回填历史。

上线先部署 Codex2API，再部署 NewAPI，须更新全部实例。只升级 NewAPI 不能保证旧 Codex2API 已移除普通选号出口；新 Codex2API 遇到尚未完成主根关联的旧 NewAPI 请求会明确拒绝。原有普通用户请求的选号与非模型故障恢复不变。

## 性能与隐私

- 数据库仅新增 `request_type`、`request_diagnostics` 两列；SQLite/PostgreSQL 均为兼容旧数据的增量迁移，无历史回填、额外索引或关联查询。
- 常规列表只 SELECT 轻量 `request_type`，不读取完整诊断。点击后 `GET /api/admin/usage/logs/:id/diagnostics` 按已有主键读取一条，查询超时 2 秒；关闭/切换弹窗取消旧前端请求，不轮询诊断。
- 请求链路只取已有解析/授权/缓存/调度结果，不为诊断再次查根、查窗口、扫描账号或访问数据库。
- 入口元数据只提取一次，不反序列化整个请求正文。单个元数据对象上限 16 KiB，嵌套和来源数量有界；完整落库快照上限 12 KiB，超限优先删除入口字段并标记截断。正文大小只影响定位 client_metadata 的扫描耗时，不随正文大小保留诊断副本。
- 沿用已有日志批量缓冲写入及 full/errors/off 模式。不需要保存的请求跳过诊断 JSON 序列化；不会改变用量计费、审核或选路。
- 不保存 Authorization、Cookie、API Key、完整请求头或提示词。标准 UUID/窗口 UUID 保留用于关联，自定义身份只保存摘要；签名 NewAPI 字段也只取固定白名单。

验证命令：`go test ./proxy ./database ./admin -run '^TestUsageRequestDiagnostics'`；性能基准：`go test ./proxy -run '^$' -bench '^BenchmarkUsageRequestDiagnostics$' -benchmem`。

本机 AMD Ryzen 7 5800H / Windows amd64 的微基准：32 KiB 正文约 23 μs/op，1 MiB 正文约 426 μs/op，均约 5 KiB、35 次分配/op。元数据刻意放在正文末尾，包含入口提取、决策快照和序列化，不包含原有路由或数据库 I/O；不是线上端到端延迟保证。
