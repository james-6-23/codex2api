# BuyCodeKey 到 Codex2API 生产透传修复与验收

本文用于修复和验证 BuyCodeKey NewAPI 到 Codex2API 的身份签名、用户画像、会话关联和上游账号审计链。命令默认通过既有 SSH 别名 `fr-netcup-new` 执行，不需要开放新的公网端口。

## 当前结论

2026-08-06 的生产只读检查表明，主透传链路已经可以正确发送并验证以下信息：

- 平台标识 `buycodekey`
- NewAPI 用户 ID、用户名或邮箱、用户分组
- NewAPI 请求 ID
- Codex2API API Key 关联
- Responses 与 Chat Completions 的签名身份

仍需解决三个问题：

1. Session 覆盖不足。多数请求没有稳定 Session，无法可靠执行跨请求会话锁定。
2. CY 事件已有上游 `account_id` 和账号分组，但详情中的 `account_name` 没有回填。
3. 13003 同时被 `transit-controller` 和 `buycodekey-pond-new.service` 管理，独立 systemd 服务正在反复重启并争用 observability socket。

## 目标数据流

```text
客户端
  -> BuyCodeKey NewAPI :13003
     -> 认证用户并确定用户分组
     -> 提取或恢复稳定 Session
     -> 生成唯一请求 ID
     -> 对身份和策略元数据分别签名
  -> Codex2API
     -> 验证 API Key 对应的绑定密钥
     -> 执行 Prompt 防护
     -> 选择上游账号
     -> 将账号 ID、名称和分组写入同一审计事件
     -> 更新用户、API Key、IP、Session 和上游账号画像
```

职责边界：BuyCodeKey 负责调用方身份和 Session；Codex2API 负责实际选中的上游账号。BuyCodeKey 不应伪造或预测 Codex2API 的作用账号。

## 修改要求

### 1. 补齐 Session 关联

BuyCodeKey 应按以下优先级提取稳定会话标识：

1. 可信请求头：`X-Session-ID`、`Session-ID`、`OpenAI-Session-ID`。
2. Responses 请求中的稳定 `conversation` 标识。
3. 业务明确提供的 `metadata.session_id`。
4. Responses 的 `previous_response_id`：通过 Redis 查询此前保存的“响应 ID -> Session”映射。

首次没有 Session 的请求可以生成随机 Session，并在响应头返回 `X-NewAPI-Session-ID`。只有客户端后续回传该值时，才能建立跨请求关联。不得使用客户端 IP、User-Agent 或 API Key 直接伪造 Session，这会把同一用户的独立会话错误合并。

写入 `X-NewAPI-Policy-Meta` 前只保留 Session 的不可逆哈希，不传输原始会话值。相同 Session 必须得到相同哈希，不同 Session 必须得到不同哈希。

### 2. 回填作用账号名称

Codex2API 在选择上游账号后已经持有 `account_id`。事件持久化前应使用该 ID 获取账号快照，并同时写入：

- `account_id`
- `account_name`
- `account_platform`
- `account_group_ids`
- `account_group_names`

如果事件先于账号详情落库，应在同一关联 ID 的后续 Usage 写入中补齐，而不是创建第二条无法关联的事件。查询 API 还应以 `account_id` 实时关联账号表作为兼容兜底，避免旧记录只能显示数字 ID。

### 3. 保留单一进程管理器

生产的 13003 当前由 `transit-controller.service` 持有。应让它成为唯一管理器，并停止独立服务反复抢占端口：

```bash
ssh fr-netcup-new

systemctl show transit-controller.service \
  -p ActiveState -p SubState -p MainPID -p NRestarts

ss -lntp | grep ':13003 '

# 确认 13003 的进程属于 transit-controller.service 后执行。
sudo systemctl disable --now buycodekey-pond-new.service
sudo systemctl reset-failed buycodekey-pond-new.service
```

部署脚本也必须遵守同一所有权：启用 Transit 管理时只更新 Transit 使用的二进制和环境文件，不能再启动 `buycodekey-pond-new.service`。

### 4. 分阶段强制签名

当前 BuyCodeKey 的绑定密钥与 Codex2API 接收端一致，但接收端仍允许未签名请求。完成下方验收并观察至少一个完整业务周期后，可将该绑定的 `require_signed_identity` 设为开启。

开启前必须确认所有实际入口都使用签名链路，包括 Responses、Chat Completions、SSE、WebSocket、multipart 和异步任务。否则强制签名会把尚未适配的协议直接拒绝。

## 可用测试端口

生产 BuyCodeKey NewAPI 当前监听服务器端口 `13003`：

```text
服务器内部地址：http://127.0.0.1:13003
健康检查：      GET /api/status
业务请求：      POST /v1/responses
                 POST /v1/chat/completions
```

已验证 `/api/status` 返回 `200`，未携带 API Key 的 `/v1/responses` 返回 `401`。

不要把 13003 直接开放到公网。在本机建立 SSH 隧道：

```bash
ssh -N -L 13003:127.0.0.1:13003 fr-netcup-new
```

隧道保持运行后，以下模板统一访问 `http://127.0.0.1:13003`。测试必须使用专用 NewAPI 测试用户和测试 Key，不能使用管理员 Token。

## 发送模板

### 准备变量

在另一个终端执行：

```bash
read -rsp 'BuyCodeKey 测试 Key: ' BUYCODEKEY_TEST_KEY
echo
export BUYCODEKEY_TEST_KEY

export TEST_SESSION_ID="codex2api-e2e-$(date +%s)"
export TEST_MARKER="BCK-E2E-$(date +%Y%m%d-%H%M%S)"
```

使用 `read -s` 可以避免把 Key 写入 shell 历史。不要把真实 Key 保存到脚本、文档或 Git。

### Responses HTTP

```bash
curl --fail-with-body --max-time 60 \
  http://127.0.0.1:13003/v1/responses \
  -H "Authorization: Bearer ${BUYCODEKEY_TEST_KEY}" \
  -H 'Content-Type: application/json' \
  -H "X-Session-ID: ${TEST_SESSION_ID}" \
  -d "{
    \"model\": \"gpt-5.4\",
    \"input\": \"Connectivity test ${TEST_MARKER}. Reply with OK only.\",
    \"stream\": false
  }"
```

### Responses SSE

```bash
curl --fail-with-body --no-buffer --max-time 60 \
  http://127.0.0.1:13003/v1/responses \
  -H "Authorization: Bearer ${BUYCODEKEY_TEST_KEY}" \
  -H 'Content-Type: application/json' \
  -H "X-Session-ID: ${TEST_SESSION_ID}" \
  -d "{
    \"model\": \"gpt-5.4\",
    \"input\": \"Streaming connectivity test ${TEST_MARKER}. Reply with OK only.\",
    \"stream\": true
  }"
```

### Chat Completions HTTP

```bash
curl --fail-with-body --max-time 60 \
  http://127.0.0.1:13003/v1/chat/completions \
  -H "Authorization: Bearer ${BUYCODEKEY_TEST_KEY}" \
  -H 'Content-Type: application/json' \
  -H "X-Session-ID: ${TEST_SESSION_ID}" \
  -d "{
    \"model\": \"gpt-5.4\",
    \"messages\": [
      {\"role\": \"user\", \"content\": \"Chat connectivity test ${TEST_MARKER}. Reply with OK only.\"}
    ],
    \"stream\": false
  }"
```

### 验证 Session 隔离

先用相同的 `TEST_SESSION_ID` 连续发送两次 Responses 请求，再切换 Session 发送一次：

```bash
export SECOND_SESSION_ID="${TEST_SESSION_ID}-other"

curl --fail-with-body --max-time 60 \
  http://127.0.0.1:13003/v1/responses \
  -H "Authorization: Bearer ${BUYCODEKEY_TEST_KEY}" \
  -H 'Content-Type: application/json' \
  -H "X-Session-ID: ${SECOND_SESSION_ID}" \
  -d "{
    \"model\": \"gpt-5.4\",
    \"input\": \"Session isolation test ${TEST_MARKER}. Reply with OK only.\",
    \"stream\": false
  }"
```

验收结果应为：前两次请求使用同一个 `session_hash`，第三次使用另一个 `session_hash`。

## Codex2API 侧验收

### 查看最近透传状态

```bash
ssh fr-netcup-new <<'REMOTE'
sqlite3 -readonly -header -column \
  /opt/ai-stack/apps/codex2api/data/codex2api.db '
SELECT
  created_at,
  endpoint,
  request_protocol,
  newapi_policy_status,
  CASE WHEN newapi_user_id <> "" THEN "yes" ELSE "no" END AS user_id,
  CASE WHEN newapi_request_id <> "" THEN "yes" ELSE "no" END AS request_id,
  CASE WHEN session_hash <> "" THEN "yes" ELSE "no" END AS session_id,
  CASE WHEN api_key_id <> 0 THEN "yes" ELSE "no" END AS api_key
FROM prompt_filter_logs
WHERE newapi_platform = "buycodekey"
  AND created_at >= datetime("now", "-10 minutes")
ORDER BY created_at DESC
LIMIT 30;'
REMOTE
```

测试请求应满足：

- `newapi_policy_status` 为 `verified` 或经过已验签响应形成的 `signed_response`。
- `user_id`、`request_id`、`session_id`、`api_key` 均为 `yes`。
- Responses 和 Chat Completions 的 `request_protocol` 与实际入口一致。

### 验证用户身份目录

```bash
ssh fr-netcup-new <<'REMOTE'
sqlite3 -readonly -header -column \
  /opt/ai-stack/apps/codex2api/data/codex2api.db '
SELECT
  subject_type,
  COUNT(*) AS identities,
  SUM(CASE WHEN external_user_id = "" THEN 1 ELSE 0 END) AS missing_user_id,
  SUM(CASE WHEN user_name = "" AND user_email = "" THEN 1 ELSE 0 END) AS missing_label,
  SUM(CASE WHEN user_group = "" THEN 1 ELSE 0 END) AS missing_group
FROM prompt_risk_identities
WHERE platform = "buycodekey"
GROUP BY subject_type;'
REMOTE
```

`missing_user_id`、`missing_label` 和 `missing_group` 应全部为 `0`。

### 验证进程没有继续重启

```bash
ssh fr-netcup-new \
  'systemctl show transit-controller.service \
    -p ActiveState -p SubState -p MainPID -p NRestarts; \
   systemctl show buycodekey-pond-new.service \
    -p ActiveState -p SubState -p MainPID -p NRestarts'
```

期望结果：

- `transit-controller.service` 为 `active/running`。
- Transit 的 `NRestarts` 不持续增长。
- `buycodekey-pond-new.service` 为 `inactive/dead`，不再反复启动。
- 13003 只有一个监听进程。

## 验收清单

- [ ] Responses HTTP 签名验证成功。
- [ ] Responses SSE 签名验证成功。
- [ ] Chat Completions HTTP 签名验证成功。
- [ ] 用户 ID、请求 ID、API Key、用户名或邮箱、分组均存在。
- [ ] 相同 Session 连续请求得到相同 `session_hash`。
- [ ] 不同 Session 得到不同 `session_hash`。
- [ ] CY 事件或失败 attempt 同时保存上游账号 ID、名称和分组。
- [ ] `transit-controller` 是 13003 的唯一进程管理器。
- [ ] 日志、响应和数据库中没有保存原始签名密钥或原始 Session。
- [ ] 完成全入口覆盖后再开启 `require_signed_identity`。

## 常见失败

### `user_id=no` 或 `request_id=no`

检查 BuyCodeKey 当前进程是否加载了 `CODEX2API_POLICY_ENABLED=true` 和正确的 Key 绑定。还要确认请求使用的实际 Codex2API Key 指纹与绑定一致。

### `session_id=no`

先确认客户端发送了 `X-Session-ID`。如果已发送但仍为空，检查该请求是否经过签名的 NewAPI 出站函数，以及策略元数据签名是否包含 Session 哈希。

### 签名身份存在但账号名称为空

这是 Codex2API 上游账号快照回填问题，不应让 BuyCodeKey 传递账号名称。检查 incident 持久化时是否已经获得 `account_id`，并补充账号查询或后续 Usage 回填。

### 13003 正常但 systemd 重启数持续增加

说明 `transit-controller` 和独立 NewAPI 服务仍在争用同一运行目录或 observability socket。保留一个管理器，并从部署脚本中移除另一个启动动作。
