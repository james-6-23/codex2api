# 2026-09-06 审查处理记录

来源：`/tmp/c2a-review-dump.md`，审查基线 `b983742b`。原报告含 15 组审查、43 条发现；复核结果不是 43 条全部确认。以下编号沿用原报告，同一问题跨组出现时合并处理。

| 审查组 | 处理结果 |
| --- | --- |
| astra-pricing F1 | 归一化后再校验价格；仅含 Astra 长档字段的编辑返回 400，保留已有有效覆盖；存储层移除归一化后的空条目。 |
| claude-api-key-base-url F1 | 模型发现过滤非 `claude-*` ID；混合网关模型清单的导入、导出、再解析有回归覆盖。 |
| claude-api-key-base-url F2 | 仅当规范化改变提示词内容时再次审查，同一规范化体重试不重复审核；保留对原始体和改变后内容的必要校验。 |
| claude-api-key-base-url F3 | 规范化错误使用已提交 SSE 的错误帧保护。 |
| claude-api-key-base-url F4 | 保留 API Key 原样转发行为；添加导入页说明与 API 文档，明确 Claude Code/OAuth 策略的适用范围，通用提示词过滤仍生效。 |
| claude-auth-kinds-backend F1 | 裸 RT 刷新前查重，导入复用后台刷新租约，租约覆盖交换与落库；重复现有凭据的测试断言零刷新调用。 |
| claude-auth-kinds-backend F2 | 有 `credits_required` 拒绝记录的模型可显式复探，成功后恢复持久化白名单并清冷却；其他白名单限制继续生效。拒绝记录已被清除时应先刷新模型清单。 |
| claude-auth-kinds-backend F3 | OAuth 已知套餐（含 Max 倍率）不被模型调用推断覆盖；Setup Token 仍可修正推断套餐。 |
| claude-auth-kinds-backend F4 | 刷新省略 `scope`，继承原授权范围；桩上游验证请求不扩权。未使用真实 sessionKey/Token 调用上游。 |
| claude-auth-kinds-frontend-and-lists F1–F3 | C=U 分支补回行内请求 ID，无 UA 审计时不显示绿色徽章，Claude 重置时间统一使用设置中的显示时区。时区差异并非本次发布首次引入，仍修正其表现。 |
| codex-test-diagnostics F1 | WS 不采用旧握手头；首帧单列 `first_frame_ms`，请求 ID/CF Ray 取 metadata，响应头按名称更新。 |
| codex-test-diagnostics F2 | 短凭据按完整词边界脱敏，避免破坏 ID、时间戳中的子串；独立凭据仍遮盖，未采用“短凭据一律不脱敏”的建议。 |
| continuation-ultrafast-encrypted F1 | 记忆使用下游稳定会话身份，不再使用随机出站 UUID；各种受支持的鉴权形式共用身份提取。移除“凭据完全不参与通用哈希”的错误注释。 |
| continuation-ultrafast-encrypted F2 | 保留被拒请求内整批密文的恢复策略并明确记录限制。上游通常未指出具体坏条目，无法安全承诺只删除那一项；不根据未知密文格式猜测归属。 |
| custom-tool-calls F1–F2 | 空终态 input 沿用已收到的 custom 流式内容，仍拒绝真正分歧；Grok 接受 custom tool_choice。 |
| custom-tool-calls F3 | 补充工具选择恢复、无效历史拒绝、大小上限及 Messages handler 流式错误终止的覆盖。 |
| model-capability-snapshots F1 | 未知能力继续保守处理，input_modalities 保底 text；不把其他账号的数值限制当成未知账号的安全上限。 |
| model-capability-snapshots F2 | 同代快照按模型与字段合并，旧客户端的部分清单不抹掉新模型；合并后仍受模型数量和字节上限约束。 |
| model-capability-snapshots F3 | 单账号和批量彻底删除均显式清理快照，SQLite 不再依赖未启用的外键级联。 |
| model-capability-snapshots F4 | 移除 scoped 清单中硬编码的 Fast/Ultrafast 声明，使用已学习的账号能力交集。 |
| model-capability-snapshots F5 | 补充从清单字节到持久化/运行态的异步学习测试；CI 增加独立 PostgreSQL service 和专用测试。 |
| response-cache-shared-payload F1 | 共享池为空时释放 map 桶引用；保留逻辑正文预算并明确其不等于 RSS，未将极端负载估算当成通常内存开销。 |
| response-cache-shared-payload F2 | 补部分重叠逐出、重复项、同 key 替换、再驻留、旧读者所有权与避免重解析的测试。 |
| schema-migrations-and-crosscutting F1 | 两个请求 ID 索引移出启动迁移事务，后台 CONCURRENTLY 创建；advisory lock 协调多实例，无效索引在线重建。 |
| schema-migrations-and-crosscutting F2–F3 | API 文档补齐 API Key 三种凭据形态、base_url、summary、追踪字段和查询参数。 |
| schema-policy | 原报告无代码发现；序列化优化不应解读成新的续链语义。保留现有实现。 |
| upstream-trace-request-ids F1 | 保留既有 X-Request-ID 契约，CORS 暴露可检索的 X-Codex2API-Request-ID，并说明两者关系；压缩子请求优先用网关追踪 ID 关联父请求。 |
| upstream-trace-request-ids F2 | live 建连保存追踪快照并在结算时写入；代理标签中嵌入 URL 的口令也会脱敏。 |
| wham-daily-breakdown F1 | 0.5% 边界不再计算无穷大估值，保持整个响应可序列化。 |
| wham-daily-breakdown F2 | SQLite 与独立 PostgreSQL 测试从旧表加列，检查幂等、默认值和已有 counts 保留。深拉条件仍为“进程内未深拉且本地覆盖不足”，不改成二者任一。 |
| ws-context-fail-closed F1–F2 | 在轮开始保留有效 L1 祖先，避免生成期间 TTL 到期破坏快照；incomplete 终态也缓存。缺失的原始历史仍不能自行补全。 |
| ws-context-fail-closed F3 | backend_unavailable 使用 WS 1011，409 使用 1008；对客户端返回的 409 计入 KnownUnavailableErrors。 |
| ws-context-fail-closed F4 | 保留未声明 store 的 WS 根轮获得 on_demand 资格；否则首次续链必定缺根。文档明确写入范围与 store:false 的选择。 |
| ws-context-fail-closed F5 | 有损降级有独立标记，响应不写回放缓存；HTTP/WS 两条路径均有端到端回归。 |
| ws-context-fail-closed F6 | 排空开始后不再新增后台 WaitGroup 任务，后续写入走同步路径；同步与异步路径都复制 payload，避免后端污染 L1。 |
| ws-relay-and-hot-path-regression F1 | 所有写入策略均尊重原生 WS 的 store:false，包括纯消息轮。 |
| ws-relay-and-hot-path-regression F2–F3 | 与上述有损降级/祖先保护合并修复；生成快照不计入客户端回放命中率，实际降级仍记录查找结果。 |
| ws-relay-and-hot-path-regression F4 | 补 1009 保留租约后 HTTP 降级缺失上下文的测试，检查错误帧、关闭码、计数和账号租约释放。 |

## 验证边界

新增测试使用虚构凭据、本地 HTTP/WS 桩以及临时数据库。真实上游 sessionKey 续期、真实账号权益和千万行生产表的索引耗时不在这些验证范围内。已有正式版本的 Changelog 未被追改；当前行为以更新后的 API、部署和兼容性文档为准。

## 本次验证结果

- `go test ./...`：全部通过。
- `go vet ./...`：通过。
- auth/database/admin/proxy/security 相关回归用例的 `go test -race -count=1`：通过，包含新增 Review 用例。
- 独立临时 PostgreSQL 18 的 `TestPostgresTraceAndCapabilities`（`-race`）：通过；临时容器已清理，未使用现有部署数据库跑破坏性迁移测试。
- 前端 `npm test`：253 项通过；`npm run typecheck` 与 `npm run build`：通过。
- 三套语言的新增文案键、`git diff --check`：通过。
- `docker compose -f docker-compose.pgredis2004.yml up -d --build`：构建并启动成功；`http://127.0.0.1:2004/health` 返回 `status: ok`，PostgreSQL/Redis 健康。

新增 CI 配置包含独立 PostgreSQL 测试；本地已运行相同的专用测试。页面操作及真实上游调用仍由用户在 2004 环境进行验收。
