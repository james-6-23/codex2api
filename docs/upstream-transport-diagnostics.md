# 上游出口、连接与错误阶段诊断

## 查看位置

使用日志的请求诊断新增“上游出口、连接与错误阶段”，对应 `diagnostics.upstream`。Codex2API 服务错误详情也附带 `upstream`。数据沿用现有诊断 JSON / 服务错误存储，无需新建表。

仅对部署后的请求采集；历史日志不能补采。未进入上游执行链的本地服务错误标记 `transport=not_started`、`send_phase=before_payload`，不会伪造上游连接信息。账号 ID 继续使用日志中的 `account_id` / `selected_account_id`。

| 字段 | 含义与限制 |
| --- | --- |
| `transport`、`upstream_endpoint` | 实际执行传输及目标协议、主机、端口，不记录 URL 用户信息、路径、查询参数 |
| `egress_kind` | `direct`、`proxy`、`resin`；没有执行证据时为 `unknown` |
| `proxy_id`、`proxy_name`、`proxy_endpoint` | 选用代理及脱敏地址；WS 取连后以该连接的代理配置为准 |
| `tcp_peer` | 底层连接 `RemoteAddr` 所见对端，可能是代理、中转或上游，取决于传输实现 |
| `public_egress_ip_status` | 当前为 `not_observed`；代理配置、TCP 对端均不能证明公网出口 IP。Resin/代理后的实际出口需结合其自身日志，不逐请求额外探测外网 |
| `connection_id` | 每条 WS 连接生成的诊断 ID，可关联不同请求是否使用同一条实际连接 |
| `connection_state`、`connection_reused` | 最近观测的连接状态及是否复用；不是管理员打开日志时的实时状态 |
| `connection_age_millis` | WS 取连时连接已存活的毫秒数 |
| `pool_key_hash` | WS 池键摘要，不回显原始池键中的凭据或用户身份 |
| `requested_connection_profile`、`connection_profile`、`profile_match` | 本次要求与实际连接握手配置的摘要、是否匹配；握手未成功时可能只有要求的摘要 |
| `handshake_status`、`handshake_request_id` | WS 建连握手的 HTTP 状态与请求 ID；复用时仍属于原握手，不是本轮响应 ID |
| `http_status`、`upstream_request_id` | HTTP 响应状态及当前上游请求 ID；WS 只从本轮事件中的请求 ID 取值，不把 `response.id` 或握手 ID 冒充当前请求 ID |
| `error_source`、`error_stage` | 区分网关、HTTP 上游拒绝、SSE / WS 上游错误事件、传输读写、下游写出；证据不足标记未知，不从 500 推断风控 |
| `error_code`、`error_type`、`close_code` | 上游事件的结构化错误标签、WS 关闭码（上游提供时） |
| `authorization_error`、`identity_error_code`、`cf_ray` | 白名单身份错误头；`X-Error-Json` 仅限长解码提取 `error.code`，不记录原始头或错误正文 |

诊断不复制 Authorization、Cookie、代理密码、请求正文。异常长或非简单标签的身份错误字段和请求 ID 只保留摘要。握手配置摘要用于判断配置相同与否，不是明文认证信息。

## 最终出站身份字段

请求诊断中的“最终出站身份”对应 `diagnostics.upstream.outbound_identity`，服务错误沿用同一上游诊断。只采集部署后的请求，不能回填历史日志。

- `http.headers` / `http.turn_metadata`：HTTP 业务改写、设备收敛、账号自定义头完成后，进入传输层前的请求头及头内元数据。
- `ws_handshake.headers`：从 HTTP 握手写出回调采集白名单，包含 WebSocket 库生成的头，随连接保存脱敏快照；复用时读取原连接快照，不根据当前配置重新推导。头内 turn metadata 保持现有有界、脱敏 JSON 表示。没有历史快照的连接不伪造数据。
- `body.client_metadata` / `body.turn_metadata`：HTTP 最终 JSON 或 WS 本次业务帧内的身份字段；WS 在环境改写后、写帧前采集。不同窗口或轮次可能复用同一握手，但帧元数据仍按当前请求记录。
- `body.links`：仅记录 `prompt_cache_key`、`previous_response_id` 的摘要，用于比对是否相同，不保存原始值。

白名单包含 UA、Originator、Version、设备、会话、线程、窗口、轮次及部分协议开关。标准 UUID 和 `UUID:窗口号` 保留便于核对，非标准身份值只保留摘要，重复头额外标记 `_multiple=true`。正文和头内同名字段分别展示，即使不一致也不互相覆盖。这里只记录，不因不一致擅自修改出站值。

不记录 Authorization、Cookie、API Key、代理凭据、attestation 的原文、提示词、完整请求正文或加密内容。超长元数据标记不可采集，客户端文本做限长与敏感内容遮盖。日志反映网关准备或提交给传输层的值，不等于上游已经收到；外部代理后续修改也不在此采集范围，仍需结合 `send_phase`、上游请求 ID 和错误来源判断。

### WebSocket 握手头与脱敏

“最终出站身份”的 `ws_handshake.headers` 覆盖以下字段，使用日志和服务错误诊断共用此快照：

| 字段 | 记录方式 |
| --- | --- |
| `Host` | 实际 Host 主机及端口；不记录 URL 路径、查询、用户名或密码 |
| `Authorization`、`Sec-WebSocket-Key`、`X-Oai-Attestation` | 非空只记 `[present]`，空值记 `[empty]`；不保存原文、前后缀或摘要 |
| `Chatgpt-Account-Id`、`Session-Id` 及旧会话别名、`X-Resin-Account` | `hash:` 摘要；此处标准 UUID 也不回显，仍支持与帧会话一致性比较 |
| `Sec-WebSocket-Protocol` | 摘要；自定义子协议可能包含不透明认证数据，不回显 |
| `Connection`、`Upgrade`、`Sec-WebSocket-Version`、`Sec-WebSocket-Extensions` | 实际写出值，经限长和敏感文本遮盖 |
| `User-Agent`、`Version`、`Originator`、`OpenAI-Beta`、`X-Codex-Beta-Features` | 沿用客户端文本的脱敏、限长规则 |
| `X-Codex-Routing-Hint`、`X-Responsesapi-Include-Timing-Metrics` | 实际写出值，经限长和敏感文本遮盖 |

只收集白名单，不复制 Cookie、Proxy-Authorization 或未知自定义头。`duplicate_headers` 保留多值/重复头标记；摘要及文本取首值，不拼接潜在敏感的额外值。UA 被关闭、可选字段未发送时不填造值。扩展字段记录客户端的压缩提议，不代表服务端最终接受的结果。

`ws_handshake.capture_stage` 区分证据阶段：`prepared` 为建连前准备值（尚未观察到 WS 请求写出，库生成的头可能缺失）；`headers_partial` 为部分头的写出回调；`headers_serialized` 为头已序列化、尚未观察到整个握手请求写出完成；`written` 为请求写出回调成功；`write_failed` 为写请求出错。缓冲区回调不证明远端已收到，HTTP 403 拒绝也可能对应 `written`，须同时看 `handshake_status`。代理 CONNECT 的头不会被当成 WS 握手头。拨号结束后快照冻结，迟到回调不能改写已保存诊断。

以上只补采新建连接；历史日志无法回填。既有入站/帧元数据及设备字段仍遵循原来的 UUID 关联策略，这里的账号和会话摘要规则专用于出站 WS 握手头。

请求诊断仍遵循 12 KiB 总限制。超过限制时先省略入站明细，仍超限则省略出站身份明细并标记 `outbound_identity.truncated=true`（界面显示 `capture_truncated=true`），避免仅因身份明细过长丢失整个请求的路由和错误诊断。缺失或截断字段不应解读为实际上游未收到该字段。

## 正文发送阶段

- `before_payload`：尚未观测到业务正文开始发送；WS 握手被 429 / 403 拒绝也属于此阶段。
- `ambiguous`：HTTP 正文已被传输层读取或 WS 已开始写帧，但尚未确认完整写出；无法确定上游收到多少。
- `after_payload`：HTTP 写请求回调确认完成、WS 写帧成功，或已收到本轮业务响应。**不等于上游已经处理完成，也不保证业务操作只执行一次。**

HTTP 响应体非 EOF 读取错误单独标记 `http_body_read`；Responses SSE 没有终态就 EOF 标记 `sse_unexpected_eof`。经过统一 SSE 读取器的上游错误事件标记 `upstream_sse/sse_event`。没有明确来源证据的错误保留未知分类。

非幂等请求在正文开始写出后发生不确定的传输失败，不再自动重放：`replay_blocked=true`。WS 只有能确认发生于写帧之前的失败才允许原有的有限重连；HTTP POST 关闭自动重放正文的 `GetBody`。发送状态不明、已写完但响应中断、缺失终态等失败不能被有限重试、无限重试或 WS→HTTP 降级绕过。GET/HEAD/OPTIONS 查询不受此限制。明确返回的 HTTP 错误或业务错误帧仍由原有错误策略决定，不因这个改动额外增加重试。客户端自行重新提交不等于服务端自动重试，本功能不提供跨请求的业务去重保证。

### WS 消息超限与 HTTP 降级

唯一新增的读失败例外是对端明确关闭 `1009`，且本轮尚未读取任何非空响应文本帧：按对端拒收过大消息处理，允许原有的同账号 HTTP 降级，不重新选号。原 WS 连接仍销毁，不复用已关闭连接。已知 `connection_local` 续链、已收到响应事件、本地 `read limit exceeded`、发送情况已标记不明的请求仍禁止自动重放；下游已写出或请求已取消也不降级。此规则不把任意断线视为拒收，也不提供跨请求的绝对去重保证。

`message_too_big_source` 区分 `peer_close`（收到对端 1009）和 `local_read_limit`（本地接收消息超限）。`replay_decision` 记录 `peer_rejected_before_response` 或具体阻止原因；`failure_category=message_too_big` 与 `failure_evidence` 保留分类依据。

消息超限的大小学习与当前请求是否允许重放分开。开启 WS 大小自适应路由时，使用最终出站 JSON 字节数学习，最小样本 64 KiB、阈值保留 5% 余量；`http_size_route_learned=true` 表示后续同类大请求已可优先走 HTTP。即使当前请求因响应已开始或本地接收超限而停止，也不会阻止后续新请求学习避开 WS。阈值为本进程内全局状态，有效期 6 小时，重启后需重新学习；设置关闭或 `CODEX_WS_SIZE_ROUTER=off` 时不生效。这是经验路由，不代表上游公布的固定大小上限，也不调整账号粘性或额度。

## WS 握手配置变更

保留既有账号、URL、代理及请求独占读租约；会话/槽位键额外加入可信所有者分区和下游 WS 连接 ID，在普通取连、无状态槽位复用、忙时溢出路径校验握手配置摘要。

摘要包含最终握手头（UA、Originator、Version、认证、实验开关及自定义头等），不包含逐轮放进帧体的元数据、Installation-ID、随机握手 key 与请求追踪 ID。只推进 turn/window 不应轮转连接；真实握手配置发生变化时，不再给新请求复用不匹配的旧连接。空闲旧连接可被替换；在途连接不强行中断。

`transport_owner_hash` 使用下游 API Key 和已验证 NewAPI 平台/渠道/用户/token 的命名空间派生。未经验证的用户头、设备 ID、客户端自报 scope 不参与可信用户分区；直接调用以 API Key 作为所有者边界。同一共享 Key 后若没有可信用户身份，不能宣称已识别各终端用户。无 API Key 且无可信身份的调用使用一次性分区，不进入公共复用池。

`downstream_connection_id` 由服务器每次 WS 升级独立生成，不接受客户端覆盖；同一连接的后续帧继承，同一会话的不同下游连接分池。分区只用于本地传输，不改写发往上游的业务会话 ID，也不改变账号绑定、配额或扩容收费。

## previous_response_id 续链

| 模式 | 处理 |
| --- | --- |
| `connection_local` | 本进程已记录的响应，未得到上游明确 `response.store=true`；必须回到原连接，并校验账号、API Key、可信所有者/请求 scope、下游连接、URL、代理、握手配置 |
| `persisted` | 上游完成事件明确返回布尔 `response.store=true`，可在同一已选账号下尝试新连接恢复；不是保证上游一定能恢复 |
| `external_unknown` | 本进程无该响应的记录，可在同一已选账号下交给上游验证，不能宣称已持久化或已找到原连接 |

已知连接本地续链遇到原连接丢失/忙碌、归属不一致、配置变化、记录过期时停止，不再降级成普通槽位随机取连；写失败也不在新 socket 上重发这类续链。返回 HTTP 400，代码 `response_context_unavailable`，提示“原响应的连接上下文不可用，请恢复完整上下文或新开对话。”错误标记不可重试，不换账号。

完成响应在转交下游前登记续链归属。`continuation_mode` 与 `continuation_result` 记录具体决策。连接退出、绑定过期或绑定表容量不足时，额外保留最多 4096 条、30 分钟的失联记录，继续校验原账号/API Key/请求 scope。记录命中时返回 `lost:<reason>` 并拒绝连接本地续链，不因原绑定被清理而立刻变成 `external_unknown`；`lost_connection` 带原连接诊断信息。明确持久化的响应仍可在同一已选账号下重新建连。

这是有界的进程内记录，不是永久存储。重启、30 分钟过期、容量淘汰或请求落到其他实例后仍可能成为 `external_unknown`。既有共享响应历史缓存与这里的原 socket 绑定是不同机制，不能用缓存存在来宣称原连接恢复成功。

## 连接生命周期与错误分类

服务日志中的 `[WS lifecycle]` 输出结构化建连/退出事件，包含连接 ID、账号、池键摘要、时间、年龄、空闲时长、在途数量及关闭原因，不包含原始池键、认证头或对端关闭文本。记录上游关闭、读写失败、心跳失败、响应放弃、配置变化、容量回收等原因；普通 500/429 业务终态本身不销毁健康连接。

管理员重新打开请求诊断或服务错误日志时，`upstream.connection_lifecycle` 补充本实例最近观测到的连接状态，最多缓存 4096 条、30 分钟。其 `observed_at` 与原请求快照分开，原数据库快照不被修改。重启或缓存淘汰后需查询服务端日志；多实例场景该实时补充不跨节点聚合。

`failure_category` / `failure_evidence` 将错误类别与判断依据分开。精确的身份错误头、授权错误头、结构化错误 code/type 优先于状态码；不扫描任意正文文本来断言风控。普通 429 不自动等同于额度耗尽。WS 旧握手头不能用于判定后续业务帧的身份故障。分类用于诊断，不新增自动禁用或换账号行为。
