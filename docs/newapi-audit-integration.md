# NewAPI 审计身份透传与二次封禁接入

Codex2API 的 `advanced.newapi.enabled` 是服务端总开关。关闭时提示词过滤仍然工作，但不会信任 NewAPI 身份头，也不会对用户/IP累计违规次数。

## 双方配置

Codex2API：

```env
PROMPT_FILTER_NEWAPI_SECRET=至少32字节的随机密钥
```

NewAPI：

```env
CODEX2API_POLICY_ENABLED=true
CODEX2API_POLICY_TARGETS=http://127.0.0.1:18095
CODEX2API_POLICY_SECRET=与Codex2API完全相同的密钥
CODEX2API_POLICY_BAN_AFTER=2
CODEX2API_POLICY_WINDOW_SECONDS=86400
```

密钥只通过部署环境注入，不能写入请求正文、数据库配置或管理页面。

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
X-NewAPI-Signature
```

## NewAPI 二开建议

1. 在 OpenAI 兼容渠道创建上游请求头时签名，不接受客户端提交的同名头。
2. 必须设置目标地址允许列表，只向 Codex2API 主机发送身份头。
3. 收到 HTTP 503 且 `X-Codex2API-Policy-Violation: true` 时保存用户、IP、请求 ID、时间、模型、接口和原始 Prompt。
4. 第一次记录警告；达到 `CODEX2API_POLICY_BAN_AFTER` 后停用普通用户并持久化 IP 黑名单。
5. 管理员和超级管理员应设置自动封禁保护，避免误报造成管理后台失联。
6. 审计页面必须使用管理员鉴权；Prompt 证据应限制长度并设置数据保留周期。

本地 NewAPI 示例实现位于：

- `service/codex_policy.go`：目标校验、签名和违规响应处理。
- `model/prompt_policy.go`：审计记录、次数统计、封号和 IP 黑名单。
- `middleware/prompt_policy.go`：黑名单请求拦截。
- `controller/prompt_policy.go`：管理员审计接口。
