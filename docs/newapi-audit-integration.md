# NewAPI 审计身份透传与可选处罚接入

Codex2API 的 `advanced.newapi.enabled` 是服务端身份审计总开关。关闭时提示词过滤仍然工作，但不会信任 NewAPI 身份头。违规累计、账号封禁和 IP 限制由 NewAPI 的独立开关控制，默认全部关闭。

## 双方配置

Codex2API 可在管理页生成、保存和替换至少 32 字节的共享密钥；也可用 `PROMPT_FILTER_NEWAPI_SECRET` 作为部署环境配置。数据库值和环境值均不得写入日志或响应正文。

NewAPI：

```env
CODEX2API_POLICY_ENABLED=true
CODEX2API_POLICY_IDENTITY_FORWARD_ENABLED=true
CODEX2API_POLICY_TARGETS=http://127.0.0.1:18095
CODEX2API_POLICY_SECRET=与Codex2API完全相同的密钥
CODEX2API_POLICY_AUDIT_ENABLED=true
CODEX2API_POLICY_STRIKE_ENABLED=false
CODEX2API_POLICY_ACCOUNT_BAN_ENABLED=false
CODEX2API_POLICY_IP_BLOCK_ENABLED=false
CODEX2API_POLICY_BAN_AFTER=2
CODEX2API_POLICY_WINDOW_SECONDS=86400
```

首次部署建议保持 `CODEX2API_POLICY_ENABLED=false`，完成连通性和签名测试后再按需要开启；处罚子开关继续保持关闭。

## 请求签名协议

NewAPI 生成唯一请求 ID 和 Unix 秒时间戳，构造签名原文：

```text
v1\n<timestamp>\n<request_id>\n<user_id>\n<client_ip>\n<http_method>\n<request_path>\n<body_sha256>
```

使用共享密钥计算 HMAC-SHA256，并以小写十六进制写入以下请求头：

```text
X-NewAPI-User-ID
X-NewAPI-Client-IP
X-NewAPI-Request-ID
X-NewAPI-Timestamp
X-NewAPI-Method
X-NewAPI-Path
X-NewAPI-Body-SHA256
X-NewAPI-Signature-Version: 1
X-NewAPI-Signature
```

原始端点、协议、模型提供商和审核档位放入 `X-NewAPI-Policy-Meta`，并使用独立的 `policy-meta-v1` HMAC 签名。扩展元数据不会改变 V1 身份签名格式。

## NewAPI 二开建议

1. 在 OpenAI 兼容渠道创建上游请求头时签名，不接受客户端提交的同名头。
2. 必须设置目标地址允许列表，只向 Codex2API 主机发送身份头。
3. 收到签名 HTTP 400 策略决策或 WebSocket 策略事件时，必须先校验决策签名和事件签名，再保存用户、IP、请求 ID、模型、接口和受限长度的 Prompt 证据。
4. 审计、违规累计、账号封禁、IP 限制必须是相互独立且默认关闭处罚的配置；不能因为启用审计就自动封禁。
5. 管理员和超级管理员应保留自动处罚保护，避免误报造成管理后台失联。
6. 审计页面必须使用管理员鉴权；Prompt 证据应限制长度并设置数据保留周期。

本地 NewAPI 示例实现位于：

- `service/codex_policy.go`：目标校验、签名和违规响应处理。
- `model/prompt_policy.go`：审计记录、次数统计、封号和 IP 黑名单。
- `middleware/prompt_policy.go`：黑名单请求拦截。
- `controller/prompt_policy.go`：管理员审计接口。
