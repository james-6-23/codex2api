# 会话锁与派生会话

## 上游提示安全拒绝

结构化上游错误 `code=invalid_prompt` 且消息以 `Invalid prompt: your prompt was flagged as potentially violating our usage policy`（或省略 `Invalid prompt:`）开头时，单独识别为 `upstream_invalid_prompt_policy`。只解析上游错误对象，不扫描请求正文；普通 `invalid_prompt`、工具参数过大、缺少输入、日志中引用类似文本均不触发此锁。显式 CYB/BIO 优先，不能降级成此类普通安全拒绝。

此类失败始终停止本次自动重试和换号重放，包括开启 catch-all、HTTP 5xx 包装及 WS 错误帧。它是请求内容拒绝，不应标记上游账号异常或清除会话粘性。HTTP 返回 400，已经开始的 SSE/WS 通过终止错误帧返回 `invalid_prompt` 及 `retry=stop`；不能把已发送的 HTTP 200 状态追溯改为 400。

复用“上游安全拒绝后锁定当前对话”开关和会话锁 TTL。开启且能够核实原始会话身份时，使用独立锁键持久化，防止覆盖同会话已有 CYB/BIO 锁。限制同会话及已知派生关系；不产生 CYB strike、不进入用户级 CYB 冷却，也不表示 OpenAI 已封禁上游账号。关闭开关仍停止当前请求重试，但不建立或执行会话锁。未能确认身份或持久化失败时明确记录未锁定，不猜测用户、不宣称成功。

使用日志的 `prompt_safety` 记录 `reason`、`upstream_code`、`lock_result`、`conversation_locked`、`retry`、`strike_eligible`、`expires_at`。`locked` 表示已持久化；`identity_unavailable`、`lineage_unavailable`、`storage_unavailable`、`disabled` 等不是锁定成功。首次拒绝在 Prompt 审计中使用独立来源 `upstream_invalid_prompt_policy`，只记录固定诊断文案，不收集原始提示内容；可按来源筛选。后续本地拦截错误码为 `conversation_safety_locked`，提供剩余时间、审计编号和解锁路径。

管理员确认误判后可在“Prompt 检查 → 风险画像 → 会话详情”解锁，或等待 TTL 到期。解除父锁会解除继承限制，但不会同时解除另一个独立 CYB/BIO 锁。不要通过换号、改 UUID 或 fork 绕过安全拒绝；账号停用风险由上游独立判断，本地不计 CYB 并不代表上游不记录或不处罚。

## 原始身份与父链

CYB 会话锁、管理员窗口锁和会话黑名单检查原始入站身份，不使用出站 UUID、上游账号或换号代数作为锁定范围。已验证 NewAPI 用户按平台、用户及原始根指纹隔离；未签名的 Codex CYB 会话锁仍按 API Key 隔离，不扩大为共享 Key 的全局处罚。

侧边对话、fork 及多层 fork 会检查已知父链。用户级 CYB 冷却结束后，只要父会话本身仍在锁定期，派生会话也不能继续转发。锁定拦截不重新累计 CYB 次数或处罚；不因换号、窗口重编号、出站 UUID 更新而解除锁。

`session_identity_links` 保存规范化的父链，`session_policy_lock_identities` 将根身份连接到既有 CYB/窗口锁查找键。已登记的子会话后续不再携带父字段，也会通过持久父链检查。数据库重启后仍可使用；父链冲突、循环、过深或查询失败时停止请求，不跳过锁检查。HTTP 和 WS 当前帧均检查，WS 不使用旧握手正文代替新帧的父信息。

解锁或锁到期后，子会话的继承限制随父锁解除，不额外复制一份需要逐个解除的子锁；子会话自己的独立锁和用户冷却仍按各自规则检查。

旧锁兼容：已有锁表不重写。已知原始父 UUID 且能匹配签名父指纹时，用同一签名密钥、用户及渠道恢复旧 CYB 查找键；窗口锁可直接按签名父指纹恢复。新访问会登记现有锁键，因此后续可跨请求沿父链查找。历史父关系完全缺失、原始父 ID 未提供、旧签名密钥或渠道已经变化且未登记过关联时，不能凭空还原所有历史 CYB 锁；不会用 UUID 前缀猜父会话。应保留完整数据库，不能只恢复锁表而丢失父链和身份桥接表。
