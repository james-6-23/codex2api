# Codex 出站身份与 WS 握手

## 默认行为

Codex HTTP `/responses`、HTTP `/responses/compact` 和 WS `response.create` 默认使用 `CODEX_OUTBOUND_SESSION_MODE=preserve`。合法的当前请求身份保持原值：

| 载体 | 出站结果 |
| --- | --- |
| 入站 session_id=S、thread_id=T | 本地权限、签名和账号粘性仍使用入站身份 |
| HTTP / WS Session-Id、Thread-Id | S、T；不再拿隔离缓存键充当 Session-Id |
| 正文 client_metadata 及其内嵌元数据 | 保留 S、T 及原有对象 / JSON 字符串载体类型 |
| 父线程、fork 来源、窗口前缀及序号、逐轮 ID | 保留原有关系，不把子线程折叠成父线程 |
| prompt_cache_key | 独立缓存分区；有已验证 NewAPI 用户时额外按用户隔离 |

当头是旧 WS 升级快照、正文带新一帧的规范元数据时，以当前帧为准。既有平铺投影按规范元数据归一；不从缓存键、scope_hash、thread_id 或 window_number 推算缺失的父会话。不将 window_id:0 当作第一轮的可靠证据。

preserve 模式优先于旧 session/full 会话收敛及会话头形态开关：只保留这些档位的设备级处理，不重写 session/thread/window/lineage。off 仍不改设备；device 仍按账号收敛设备。UA、环境处理、项目字段清理照旧。会话、线程和任务头不允许账号自定义头覆盖成与正文矛盾的值。设备头与设备元数据继续用同一份指纹快照。

不修改 input、压缩项 id、encrypted_content、工具输出和不透明续链令牌。HTTP 已有的 previous_response_id 展开 / 清理机制不变。Responses 中转及其他提供商不使用此 Codex 身份改写。

## WS 三层处理

| 类别 | 处理 |
| --- | --- |
| Session-Id、Thread-Id、X-Client-Request-Id | 发送真实当前身份，纳入连接配置匹配；身份变化不能复用不兼容连接 |
| X-Codex-Window-Id、X-Codex-Turn-Metadata | 建连时发送当前快照；后续窗口、轮次随每帧更新，不因序号推进就重连 |
| 父线程、subagent、memgen、线程来源 | 保留真实任务标记并参与配置匹配，避免普通请求继承后台任务连接 |
| X-Codex-Turn-State | 仅放当前帧，不固化到握手 |
| 工具清单及较大元数据 | 完整内容留在帧；握手删除工具清单，超过 8 KiB 时仅保留有界身份 / 轮次字段 |

设备身份改变也会触发配置不兼容，不把旧设备握手与新设备正文混用。窗口 / turn 的握手快照与后续帧不同是正常的，不等于会话身份不一致。

busy 同会话溢出功能保留，只使用同账号、同隔离分区及兼容握手配置的兄弟连接，本地 #ovf 后缀不写入 session_id。已知连接本地 previous_response_id 仍必须使用原连接；连接丢失、占用或配置不兼容时停止并提示恢复完整上下文，不自动发往兄弟连接重放。

参考官方客户端 `rust-v0.154.0` 的 [握手构建](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/core/src/client.rs) 和 [元数据投影](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/core/src/responses_metadata.rs)。这不意味着网关所有字段与官方字节级一致，也不保证消除上游 500。

## 隔离与冲突保护

- WS 池继续包含已验证归属、下游连接、账号、原始线程、URL、出口和握手配置维度。内部键不写回正文。
- 响应上下文缓存与 response_id 账号归属额外区分共享 API Key 下的已验证用户；拒绝读取旧的共享 Key 缓存作为兜底。加密内容拒绝记忆也区分用户。
- 相同上游账号下，session/thread/父线程/fork 身份的归属使用 SHA-256 摘要落库到 `codex_identity_claims`；同一已验证用户重复使用允许，其他用户碰撞直接报 `codex_session_identity_conflict`（400，不重试、不换号）。重复导入相同 ChatGPT 账号也共享冲突约束。
- 归属登记事务化，无 TTL；服务重启及共享同一数据库的多实例仍有效。数据库异常时拒绝发送，不退化为随机改 ID。只保存归属和身份摘要，不存认证凭据、原始 ID 或正文。
- 无已验证 NewAPI 用户时只能识别已认证下游凭据，不能声称区分同一凭据背后的不同真人。无数据库的嵌入式 Handler 使用有界进程内表，满时拒绝新登记，不驱逐旧归属；它不具备跨进程持久性。直接调用底层执行器须由宿主提供归属上下文，管理员账号测试独立生成诊断会话。

## 部署与回退

- `preserve` 默认；之前未发布的 `aligned` 名称及未知值也按 preserve 处理。
- `observe`、`legacy` / `off` 显式回到旧出站会话与握手策略；只用于兼容回退，不保证头体一致。新增用户缓存隔离不随回退撤销。
- 现有账号绑定、黑名单不迁移、不清空。新归属表从启用后的请求开始登记，不能追溯此前未知的跨用户冲突。
- 部署会改变旧会话的出站握手身份与用户缓存命名空间。旧连接本地上下文不能迁移；部分 previous_response_id 续链可能需要客户端补全上下文或新建会话。不要在两种模式实例间交替路由同一续链；建议在维护窗口部署或按会话灰度。
- 不会因为迁移失败而绕过永久绑定自动换号。

## JSON 日志

“最终出站身份”默认展示脱敏 JSON，`format_version=2` 按实际字段位置保存：

```json
{
  "format_version": 2,
  "session_consistency": "matched",
  "ws_handshake": {
    "headers": {
      "Session-Id": "S",
      "Thread-Id": "T",
      "X-Codex-Turn-Metadata": "{\"session_id\":\"S\",\"thread_id\":\"T\",\"window_number\":0}"
    }
  },
  "body": {
    "client_metadata": {
      "session_id": "S",
      "thread_id": "T",
      "x-codex-turn-metadata": "{\"session_id\":\"S\",\"thread_id\":\"T\",\"window_number\":1}"
    },
    "prompt_cache_key": "hash:..."
  }
}
```

以上 S/T 为说明用占位；日志中非标准标识仍遵循既有脱敏规则。它是字段白名单快照，不是完整请求抓包；不会记录 prompt、Cookie、Authorization、密文或 turn state 明文。JSON 字符串默认仍为字符串，可勾选“解析元数据”便于阅读，此时展示及复制是解码视图而非原始字段类型。

旧日志的 `body.turn_metadata` 和 `body.links` 是历史解析分组，界面明确标为历史摘要，不伪装成真实上游层级。新日志改为 `body.client_metadata["x-codex-turn-metadata"]` 和 `body.prompt_cache_key`。

`session_consistency` 只比较实际捕获的会话字段：matched / mismatched / missing_header / missing_body；不等于请求成功。WS 复用展示实际建连时握手，而不是把本次期望头冒充已经重发的握手。

## 可选账号级稳定映射

设置 `CODEX_OUTBOUND_SESSION_MODE=account` 并重启服务，为尚未登记的 Codex 会话启用 `account-uuid7-v2`。默认仍为 preserve，不自动改变线上策略。不适用于 Responses 中转或其他提供商。

- 新策略生成完整 UUIDv7，不再保留原 UUID 的时间前缀。首次建立出站映射时使用服务端 UTC Unix 毫秒时间，固定 UUIDv7 版本和 RFC variant 位，剩余 74 bit 由 HMAC 派生。只接受合法 UUIDv7 入站身份，其他格式拒绝发送。
- HMAC 密钥为数据库中持久保存的 32 字节随机值，派生域包含版本、身份种类、已验证用户归属、目标实际 `Chatgpt-Account-Id`、完整原始 UUID，以及已有迁移段。第一次生成的完整 UUID 存入 `codex_identity_uuid7_values`；后续请求只读取，不重新取当前时间生成。并发实例通过唯一约束复用同一个获胜值，而不是各自生成后直接发出。
- 同用户、同实际 `Chatgpt-Account-Id`、同完整原始 ID 的结果稳定；HTTP、compact、WS、重试和重启一致。同上游账号重复导入不会因为本地账号编号不同而改变映射。实际账号不同则独立映射，默认仍禁止自动换号；只有显式开启下述开关才允许受保护的迁移。
- Session-Id、Thread-Id、正文 session/thread、父线程、fork 来源、context_window_id 及窗口 ID 前缀共用映射。原本相等仍相等，子线程不会折叠成父线程，窗口序号不变。X-Client-Request-Id 及正文投影只在其值引用这些身份时同步替换，不改写独立的请求跟踪 ID。
- prompt_cache_key 使用独立 `prompt-cache` 派生域并按账号、用户分区，不把缓存键拿来作为握手 Session-Id。日志仍只保存缓存键摘要。
- turn_id 与 root_turn_id 使用独立轮次映射域并记录来源阶段的映射版本；新阶段同样生成完整 UUIDv7。同一个原始轮次同时出现在两种字段时得到同一个出站值。引用旧阶段的轮次沿用旧阶段映射，旧 preserve 会话继续保留轮次值。
- 不改入站 NewAPI 签名、本地主会话解析、黑名单、永久绑定或日志搜索前缀。原始 turn_id/root_turn_id 仍用于本地找根、关联和入口日志，改写只作用于最终 HTTP 头、WS 握手元数据和出站正文副本。parent_turn_id、时间戳、设备级处理、connection_id、上游 response_id、previous_response_id、工具 call_id 和 encrypted_content 不在此映射范围。

### 旧会话与故障保护

首次登记策略时检查已有 `codex_identity_claims`：已发送并登记的会话继续 preserve，避免活跃会话突然变号。账号隔离模式下，新 fork/父引用优先使用对应账号的出站映射；旧 preserve 父会话仅在核实原账号、未迁移阶段后允许保留原始父引用，具体条件见下文。既有 account 映射在环境变量切回 preserve 后仍继续原映射，不破坏恢复链；不要通过删除记录强制重新生成。

已经登记的 `account-suffix-v1` 会话继续使用原算法和原结果，不强制迁移；新会话和下一次成功换号的新段才采用 v2。A→B→A 每个迁移段独立，同一段的主请求、压缩、关联 session 引用、重试和重启复用原映射。父引用使用对应父段的版本，允许新子会话引用旧版本父映射。关联请求原本独立的 thread_id 不会被强制合并为主 session_id。

部署前完整备份数据库，包括 `codex_identity_claims`、`codex_identity_mapping_secret`、`codex_identity_mapping_policies`、`codex_identity_alias_claims`、`codex_identity_epochs`、`codex_identity_references`、`codex_identity_uuid7_values` 和 `codex_session_context_tokens`。映射密钥不得手动轮换、复制到日志或只恢复部分表。新版本多实例应共享数据库；不能与不识别该策略的旧版本混跑同一会话。无法追溯未曾登记的历史请求，因此不保证首次接入服务的外部旧会话保持历史出站身份。

映射/冲突登记有事务约束且无 TTL，存储量随新的身份及轮次增长。密钥丢失、数据库不可用、归属冲突、别名碰撞都停止请求，不退化为原始 ID、随机重抽或自动换号。持久唯一约束仍检查碰撞，不能声称数学上绝无碰撞。没有数据库的嵌入式调用不能启用 account 模式；宿主可用 `WithCodexIdentityStore` 提供存储上下文。

### 改写日志

使用日志“最终出站身份”增加 `account_mapping`，可随原有 JSON 复制/批量导出：

```json
{
  "account_mapping": {
    "version": "account-uuid7-v2",
    "status": "mapped",
    "scope_hash": "脱敏归属摘要",
    "chatgpt_account_id": "实际账号 ID",
    "cache_partitioned": true,
    "changes": [{ "original": "原始 UUIDv7", "outbound": "映射后 UUIDv7", "version": "account-uuid7-v2", "mapped_at": "2026-09-13T12:00:00Z" }]
  }
}
```

`mapped` 表示已执行映射；`preserved_existing` 表示保留既有会话身份；`preserved_with_mapped_references` 表示自身身份与缓存键保留，仅父引用使用对应账号的映射；`mapped_with_legacy_references` / `preserved_with_legacy_references` 表示包含已核实的旧原始父引用，前者自身仍映射，后者自身继续旧策略。`preserved_ids` 列出需继续保留的关联身份。`references` 记录原始父 ID、解析出的 `policy`、迁移代数和段摘要，以及 `action` / `reason`，包括映射拒绝时的结果。失败状态不代表已经发送。映射日志不含 HMAC 密钥或认证凭据。最终 HTTP 头、实际 WS 握手、当前帧正文仍分别展示，不用本次期望头覆盖旧连接的握手快照。

使用日志分开显示“出站身份快照”和“本地改写诊断（不发送上游）”：前者仅包含 `http`、`ws_handshake`、`body` 的脱敏摘录，后者包含 `account_mapping`、日志版本、一致性结果等。`account_mapping.changes` 中轮次记录用 `fields` 标明 `turn_id` / `root_turn_id`。复制与下载仍保留完整诊断结构以兼容旧日志解析器，并非上游请求原文。

轮次映射沿用持久化身份表，按用户、实际 ChatGPT 账号、原始轮次 ID 记录其来源阶段，再按当前会话阶段固定引用。关联后台请求和 fork 查询同一原始根轮次时复用其已登记映射，不将根轮次按每个子线程重新随机生成；来源未登记时先以当前已解析阶段建立映射，冲突则发送前拒绝而非覆盖。当前主会话 A→B→A 后本轮映射随迁移代数变化；仍留在原阶段的历史子会话引用保持固定，不随父会话后来的换号漂移。重启或请求重试不会重抽随机值。

WS 握手中的轮次信息是建连时的已改写快照；复用连接后，本轮值以当前帧正文为准。不会为了更新每轮 turn_id 重建连接，也不会把旧握手快照的轮次值重新灌入当前正文。

`changes[].version` 是该 ID 实际使用的版本，`mapped_at` 是 v2 出站 UUID 中固定的首次映射时间，重试时不更新；它不是请求成功时间或账号切换提交时间。上述字段仅是本地改写诊断，不发送上游。

此功能是账号隔离，不承诺匿名、不可关联或消除 500。未改动的设备、请求时间、内容、账号和出口等仍可能具有相关性；上游不透明响应及加密上下文不能通过改 UUID 迁移到其他账号。

换号失败时，`account_failover.context_blockers` 最多记录 8 个命中的字段路径、类型和输入项类型，例如 `input[3].encrypted_content` / `reasoning`、`input[5].content[0].file_id` 或 `input[7].id` / `item_reference`。不记录字段值、加密正文、文件编号或工具调用编号；未知自定义字段名与类型也不直接记录。此诊断不会放宽迁移校验，`block_reason` 和错误码保持兼容；缺少持久归属的提示明确要求恢复绑定，而非误导为只需补未加密正文。

用户创建的 fork 在签名元数据明确指向父会话且属于普通 user/turn 时，可以申请自己的普通或扩容窗口，不再因 `root_relation=related` 被误当后台请求跳过。首次报价先查子会话归属，无子绑定时用签名父指纹恢复账号，但授权和预留使用子会话自己的键；已开启并接受倍率的扩容优先使用该账号的扩容容量，不借用父窗口预留，也不因此先换号。标题、Guardian、压缩及绕过计数的后台请求不借此创建付费窗口，原有确认、额度和签名校验保留。

## 绑定账号不可用时允许换号

系统设置 → Codex → 全局用量自动暂停阈值，新增 `codex_session_failover_enabled`，默认关闭，可热更新并持久保存。开启后同时为新出站身份启用 account 模式，无需额外配置环境变量。关闭后不再产生新迁移，已经迁移的会话继续使用其当前账号和已登记映射，不自动切回原账号。不要对已映射会话使用旧版 legacy/off 出站策略。

允许额度耗尽、全局/账号用量自动暂停、手动暂停、禁用、授权失效或绑定账号无法接纳当前会话窗口时，迁移**后续独立请求**。只处理有持久主会话归属、有效窗口序号、身份一致的 Codex 请求；允许有损重开，但清理后必须仍有可用输入。不在流输出中途或上游完成状态不明时重放。账号仅请求并发满、模型不支持或临时服务器错误不触发开关；已有窗口仍可复用时，不会仅看账号窗口总数就换号。Spark 按其独立可用性判断。后台/子线程不能自行发起迁移，只跟随已完成迁移的主会话归属。

迁移前仍校验模型、渠道、用户分组、预算、账号容量、出口和黑名单。目标必须是不同的实际 ChatGPT 账号，并且能使用持久化账号级 ID 映射。目标与原账号的账号分组 ID 集合必须完全相等：忽略顺序和重复项，不接受仅交集相同、子集或超集；未分组只匹配未分组。选号时过滤，提交迁移前再次核对；准备过程中任一方分组变化则停止迁移，记录 `account_groups_changed`，保留原粘性。数据库以原账号及迁移代数 CAS 更新主会话和现有窗口授权；同时保留窗口创建/到期时间、序号、扩容倍率等信息，不续期、不新增免费扩容权益。未找到安全候选或校验失败即停止，不随意改号。普通请求省略窗口授权票据时，会从已验证用户的持久授权记录恢复并同步归属；过期或需重新确认的扩容授权不会被跳过。

`previous_response_id`、连接 turn state、不透明输入引用、加密 reasoning/compaction 不能仅靠改 UUID 跨账号迁移。新迁移采用有损重开：只从出站副本移除旧引用、整条旧加密推理/压缩项、无法独立使用的文件节点及孤立工具结果；保留普通消息、内联文件/图片数据和完整工具调用对。原始入站、找根键和用户文件不删除。压缩项可能是历史的唯一保留形式，删除会丢失对应历史；文件引用删除会丢失该资料，不保证任务语义完整或上游一定成功。清理后无可用输入则返回 400，要求补充任务和必要资料，不发送空请求。

迁移提交同时持久化 `lossy_context_restart=true`。之后每次出站仍清理旧段内容，已核实属于当前账号/代数的新加密内容和续链保留；重启及之后关闭换号开关不会让旧引用重新外发。升级前已有的未标记迁移段保留原严格策略，不偷偷改变其上下文。诊断 `account_failover.context_cleanup` 记录 `mode=lossy_restart`、`phase=prepared|outbound` 和 `removed` 各类型数量；准备不等于已经发送，只有完成选号、归属校验后的出站副本执行实际清理。日志不记录密文或文件内容。

日志 `diagnostics.session_continuity.account_failover` 记录 `result`、`reason`、`previous_account_id`、`account_id`、`generation`。`switched` 表示归属迁移已提交，不代表上游请求成功；`restored` 表示恢复已有迁移归属；`blocked` / `no_safe_candidate` 表示未迁移。最终握手和正文的 ID 前后值仍在 `diagnostics.upstream.outbound_identity.account_mapping` 中查看。部分旧请求可能已在途，其账号及出站身份不会被追溯改写。

换号上下文拒绝返回 HTTP 400 / WS 对应错误帧 `codex_session_failover_context_required`，不计为上游 500。响应 `details` 提供具体 `reason`、`trigger_reason`、`phase` 和 `retry: stop`；使用日志 `diagnostics.account_failover` 及服务错误 JSON 的 `account_failover` 同时记录 `trigger_reason`（额度、禁用、容量等换号触发原因）、`block_reason`（续链、加密内容、工具上下文或归属校验）、原账号和代数。`phase=before_switch` 表示本次换号尚未提交，`after_switch` 表示在恢复已迁移会话时拒绝旧上下文，不能仅凭 `blocked` 判断历史上从未换号。不会记录加密内容、turn-state 或文件凭据的原值。

### 后台请求的账号匹配

NewAPI 先在已验证用户范围内按**原始 session_id 前缀**解析唯一主会话，再由 Codex2API 核对该主会话的**当前账号和迁移代数**。NewAPI 的渠道 ID 不是 Codex2API 账号 ID，不能以渠道代替账号，也不能先随机选号再反推主会话；前缀相同但有多个候选时仍拒绝关联，不借账号猜根。

后台/无根关联成功后，选号只允许当前主账号；发送 HTTP、compact 或 WS 帧前再次读取持久归属。排队期间账号变化、A→B→A 迁移代数变化、存储不可用、根窗口失效或账号不匹配均停止发送，调用方须重新发起请求。保留后台专用并发额度，不创建或续期主窗口。入站身份及前缀不改写，出站改写仍按实际账号统一执行；没有可验证 session_id 的请求不会凭空生成可关联的根。

日志 `diagnostics.background_account_match` 展示 `scope_hash`、`session_id_prefix`、`account_id`、`generation`、`owner_source` 和 `result`，用于核实用户范围、前缀和最终账号的匹配。`matched` 不是上游成功标记；`owner_changed` / `account_mismatch` / `ownership_unavailable` / `root_owner_unavailable` 表示发送前校验未通过。既有会话的 `preserved_existing` 策略仍保留，不能将所有后台请求一概视为已经改写。

### 每次迁移的窗口序号

新迁移段在账号级映射中额外加入持久主会话及迁移代数，包含会话/线程、关联字段、窗口前缀和提示缓存分区；A→B→A 不会复用 A 的旧出站段或旧 WS 连接。未迁移的既有会话仍遵循原来的 preserve/account 策略。

新主窗口映射随账号归属在同一事务中保存。例如首次迁移时入站 `window_id=S:47, window_number=47`，出站为新段的 `S′:0, 0`；重复该上下文仍为 0，下一上下文 48 为 `S′:1, 1`。按原始线程及上下文 UUID 分配编号，而非减去固定基准：回退 46 的另一上下文也获得独立非负编号；同一上下文重复/恢复始终使用原映射。同一 UUID 配不同原始序号仍拒绝。子线程独立编号，主会话和关联线程的 session/thread 关系不改变。缺少上下文 UUID 时按原始序号匹配，存在歧义时拒绝。映射持久化，重启或关闭开关不丢失；A→B→A 的新段再次从 0 开始。旧段按已有基准和已记录高水位保留已知偏移编号，再为回退上下文分配未占用编号。握手快照、当前帧及 HTTP 正文使用同一转换；WS 后续帧的窗口推进不改写既有握手快照。

本地入站序号、前缀匹配、窗口票据、使用时长及粘性校验均保留原值。`account_mapping.generation`、`segment_hash`、`windows` 记录迁移段及原始/基准/出站序号，便于审计。响应续链也记录迁移段，切回旧账号不会因此接受旧段的 previous_response_id。

### 首次迁移和后续上下文保护

发送许可与窗口重编号分别处理：普通主请求即使迁移代数为 0，也保存账号和代数快照；首次持久准入后同样附加快照。HTTP、compact、WS 取连及发送前核对当前持久归属，A→B→A 也不能因为账号 ID 相同而通过旧请求。准入提交拒绝旧代数覆盖当前记录。已经发送出去的在途请求不能追溯撤回。

旧号密文、连接状态或未知续链不原样迁移：新有损段在出站清理，旧严格段仍拦截。换号后，由当前账号和当前迁移段产生的加密推理、压缩和已记录输出引用可正常继续使用。HTTP/compact/WS 输出路径记录来源，HTTP 向客户端下发的 turn-state 也记录来源。证明同时区分用户、主会话、实际 ChatGPT 账号、本地账号和迁移代数；不能把仅有账号级来源的旧记录当作当前段证明。

来源记录只存作用域化摘要，不存提示正文、密文或 turn-state 原文；同时持久化至数据库，缓存缺失时回查，因此重启和共享数据库的实例可继续核实。加密上下文来源使用既有 `CODEX_COMPACTION_AFFINITY_TTL`（默认 7 天），turn-state 来源为 1 小时。过期、未记录或跨用户/账号/迁移段的内容不能证明可复用：新有损段会清理这些出站依赖，旧严格段仍拒绝；归属存储不可用仍停止发送。原始入站始终不修改。这些保护不保证上游不会返回 `invalid_encrypted_content` 或其他错误。

### fork 的出站父引用

父会话 S 的出站 ID 若已是结合实际账号和迁移段派生的 S′，新 fork T 的握手和正文父引用均指向 S′，而不是入站 S，也不是套用 T 自己的迁移段重新派生一个错误父 ID。T 自己的会话/线程 ID 仍按自己的账号隔离规则生成，不复用父线程 ID。

父引用按用户、实际账号和 fork 自己的出站段持久固定。父会话之后再次换号或回切，不会使既有 fork 的父引用漂移；新 fork 则使用对应账号已登记的父段。没有历史父段且没有旧 preserve 声明时，使用该账号的稳定派生映射；无法核实已有父段或存在策略冲突时拒绝，不任意回退到原始父 ID。关闭自动换号后，引用已映射父会话的新 fork 仍沿用账号映射。日志 `account_mapping.changes` 和最终握手/正文快照可核对原始与出站父 ID。

旧 fork 自身的 `preserve` 策略不再一律阻止父引用映射：本地仍按用户隔离后的原始 ID 查询，自己的 session/thread/window 与提示缓存键保持历史值，父引用单独解析对应账号的历史段并在握手和正文同步改写。

旧父会话使用 `preserve` 时，只有当前请求具备一致的账号与持久化阶段、当前阶段从未迁移，而且父会话同账号的零代历史归属已核实，才允许在握手和正文保留原始父 ID。首次依据原始父 ID 回查父会话归属，成功后沿用已有引用表按用户、实际 ChatGPT 账号和子会话阶段固定父段；父会话单独迁移不改变仍留在原账号的既有子会话引用。新子会话自身仍使用账号映射，不因为父会话为旧策略而整体回退。日志标记 `action=preserved_legacy_parent`、`reason=original_account_unmigrated`。

子会话一旦换号，兼容许可不随迁移转移：包括 A→B→A，任何已迁移阶段都不再允许旧原始父引用；当前会话及其他关联身份也不能回退 preserve。目标账号有可核实的父映射时使用其映射，没有则发送前拒绝，不自动删除父引用。`request_migrated`、`request_epoch_unavailable`、`parent_owner_unavailable` 等日志说明具体拒绝原因。有损上下文清理不绕过父引用身份校验或安全锁，因此不保证所有旧 fork 都能换号。
