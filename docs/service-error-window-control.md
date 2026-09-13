# 服务错误中的窗口控制请求

`POST /v1/session-windows` 是 NewAPI 与 Codex2API 之间的窗口管理/预留/扩容控制接口。NewAPI 使用 `User-Agent: NewAPI-Window-Control/1` 调用，可能发生在查看窗口、正常聊天前的窗口预留、请求结束清理预留或确认扩容时，不是模型生成请求。

`upstream.transport=not_started`、`send_phase=before_payload` 表示本次控制请求没有向模型上游发送正文。仅凭 HTTP 400 不能判断账号异常、上游限流，或认定用户执行了某一种操作。

## 新日志

- `request_type=gateway_internal`，`request_kind=window_control`。
- `client_info["window_control.operation"]` 记录 `list`、`quote`、`release`、`upgrade`；无法解析/不支持的输入记录有限枚举，不保存未知操作原文。
- `code` 和中文 `message` 区分请求格式、倍率、额外窗口数量、预留标识、窗口容量不足、扩容确认不足、授权已失效等原因。
- 本地窗口 400 归类 `invalid_request_error`，不再默认写为 `server_error`。认证和服务端故障保留对应 HTTP 状态及错误类型。
- 顶层 `{message,code,type}` 和 OpenAI `{error:{message,code,type}}` 两种错误格式均能采集；嵌套字段优先。仍然只保存脱敏诊断字段，不保存控制请求正文、ticket、grant_id 或密钥。

窗口接口继续返回顶层 `message`，兼容 NewAPI 现有读取方式；本次不改变容量限制、计费倍率校验或扩容确认规则。

旧记录中的 `http_400 / Service request rejected` 可能是采集器漏读顶层字段造成的。未保存的原始原因无法从旧记录补回，需用更新后的新日志定位。

## 续聊时突然拒绝的诊断

通过身份和参数校验的窗口控制请求，服务错误 JSON 增加独立的 `window_control` 对象。该对象随错误详情展示和复制，不占用有数量限制的 `client_info`。不需要数据库表结构变更。

- `observed_at`、`root_hash`：本次判断时间及根窗口哈希。哈希与窗口授权的根窗口标识一致，不保存原始会话身份。
- `owner_source`、`owner_account_id`：绑定来自 `continuity`（持久会话绑定）、`live_session`（活跃占位）或 `fork_parent`（父会话）。持久绑定还带上 `owner_last_seen`、`owner_last_completed`、`owner_last_status`，便于对照最后活动和完成状态。
- `root_window_state`、`root_window_expires_at`：当前用户根窗口的观测状态及已知到期时间。
- `grant`：在事务清理前记录授权的 `active`、`pending`、`expired`、`reservation_expired` 或 `missing` 状态，及创建、到期、临时预留截止时间、是否确认和绑定账号。不会保存 ticket、grant_id、reservation_id 或 owner_key。
- `account`：在容量检查的同一把锁内记录 `reason`、`slot_state`、已知的占位最后活动/到期时间、总容量/保留容量、实际总占用/保留占用和空闲回收秒数。普通容量为 `total_limit - reserved_limit`，普通占用为 `total_used - reserved_used`。
- `user_limit`、`window_seconds`、`active_windows`、`ordinary_used`、`expanded_used`：用户额度与使用量。只有 `counts_evaluated=true` 才表示后两项已经参与准入判断，未计算时的零不能当作实际使用量。
- `allow_expansion`、`extra_limit`、`multiplier`、`needs_expansion`、`expansion_block`：网关收到的扩容授权及拒绝依据。`not_authorized` 只表示 NewAPI 本次未授权，不能据此断言用户开关关闭；具体是全站策略、渠道配置、用户开关还是倍率未确认，仍需对照 NewAPI 设置。其他拒绝依据为 `extra_limit_exhausted`、`multiplier_not_expanded`。
- `decision`：区分 `owner_admission_rejected`（绑定账号未准入）、`user_window_limit`（用户窗口限额）、`admission_state_limit`（授权状态数量上限）、`owner_lookup_failed`、`storage_failed`。

例如 `grant.state=expired`、`owner_source=continuity`、`account.slot_state=missing`、`account.reason=session_capacity_full` 表示：旧授权已到期，持久绑定仍存在，本次检查没有看到原占位，绑定账号也没有可用普通容量。它能解释为什么重新申请被拒绝，但不能单凭 `missing` 判断占位是此前已回收、重启后未恢复还是根身份发生变化。`expired` 只在本次确实观察到过期记录时记录；已被其他请求清理的记录会显示 `missing`。

`account.reason=account_unavailable` 和 `upgrade_pending` 分别揭示绑定账号不存在/不可读取及占位正在升级，避免将现有通用提示误解成一定是容量满。有效占位即使处在满容量账号中仍应复用，回归测试覆盖该行为。本次只增加诊断，不改变准入、扩容收费或用户提示规则，也不宣称修复了尚未定位的续聊故障。

## 复制 JSON

服务错误页只调用全局 ToastProvider 显示提示，不把 ToastState 对象当成 React 子节点渲染。剪贴板不可用时在当前对话框内降级复制；失败只显示提示，不关闭详情、提交表单或跳转页面。
