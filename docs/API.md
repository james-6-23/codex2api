# Codex2API API 文档

本文档详细描述 Codex2API 的所有 API 端点、请求/响应格式以及错误码说明。

## 目录

- [概述](#概述)
- [认证](#认证)
- [公共 API](#公共-api)
  - [Chat Completions](#1-chat-completions)
  - [Responses](#2-responses)
  - [Images](#3-images)
  - [List Models](#4-list-models)
  - [Health Check](#5-health-check)
- [管理 API](#管理-api)
  - [统计接口](#统计接口)
  - [账号管理](#账号管理) — 添加 RT / AT 账号、批量导入、导出、迁移
  - [用量统计](#用量统计)
  - [API Key 管理](#api-key-管理)
  - [系统设置](#系统设置)
  - [代理池管理](#代理池管理)
  - [运维监控](#运维监控)
  - [模型管理](#模型管理)
  - [生图工作台](#生图工作台) — 文生图、图生图任务与图库管理
  - [OAuth 授权](#oauth-授权) — PKCE 流程获取 Token
- [支持模型](#支持模型)
- [错误码](#错误码)
- [限流说明](#限流说明)

---

## 概述

Codex2API 提供兼容 OpenAI 风格的 API 接口，同时包含完整的管理后台 API。

Anthropic `/v1/messages` 仅将官方 `speed:"fast"` 映射为上游 Codex `service_tier:"priority"`；Anthropic 请求侧 `service_tier`（Priority Tier）不在此映射范围内。用量日志的 `service_tier` / `fast` 过滤反映该解析结果。

**Base URL:** `http://localhost:8080` (默认端口)

**请求格式:**

- 请求头: `Content-Type: application/json`
- 认证头: `Authorization: Bearer <api_key>`

---

## 认证

### API Key 认证

公共 API (`/v1/*`) 需要 API Key 进行认证。

**请求头:**

```http
Authorization: Bearer sk-xxxxxxxxxxxxxxxxxxxxxxxx
```

**配置方式:**

1. 通过管理后台 `/admin/settings` 页面配置
2. 如果没有配置任何 API Key，则 `/v1/*` 接口跳过鉴权（开发模式）

### Admin Secret 认证

管理 API (`/api/admin/*`) 需要 Admin Secret 进行认证。

**请求头:**

```http
X-Admin-Key: your-admin-secret
```

或

```http
Authorization: Bearer your-admin-secret
```

**配置方式:**

- 环境变量: `ADMIN_SECRET`
- 数据库: 通过管理后台设置

---

## 公共 API

### 1. Chat Completions

**端点:** `POST /v1/chat/completions`

**说明:** OpenAI 风格的 Chat Completions 接口，支持流式和非流式响应。

**请求示例:**

```json
{
  "model": "gpt-5.5",
  "messages": [
    { "role": "system", "content": "You are a helpful assistant." },
    { "role": "user", "content": "Hello!" }
  ],
  "stream": false,
  "reasoning_effort": "medium",
  "service_tier": "fast"
}
```

**参数说明:**

| 参数             | 类型    | 必填 | 说明                                        |
| ---------------- | ------- | ---- | ------------------------------------------- |
| model            | string  | 是   | 模型名称，见 [支持模型](#支持模型)          |
| messages         | array   | 是   | 消息列表                                    |
| stream           | boolean | 否   | 是否启用流式响应，默认 false                |
| reasoning_effort | string  | 否   | 推理强度: low/medium/high                   |
| service_tier     | string  | 否   | 服务等级: fast/auto                         |
| max_tokens       | integer | 否   | 最大输出 token 数（Codex 不支持，会被过滤） |
| temperature      | float   | 否   | 温度参数（Codex 不支持，会被过滤）          |

**非流式响应示例:**

```json
{
  "id": "chatcmpl-xxxxxxxx",
  "object": "chat.completion",
  "created": 1712345678,
  "model": "gpt-5.5",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I help you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 25,
    "completion_tokens": 15,
    "total_tokens": 40
  }
}
```

**流式响应示例:**

```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.5","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.5","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.5","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

### 2. Responses

**端点:** `POST /v1/responses`

**说明:** Codex 原生 Responses 接口，直接透传，无需协议翻译。

**请求示例:**

```json
{
  "model": "gpt-5.5",
  "input": [
    { "role": "system", "content": "You are a helpful assistant." },
    { "role": "user", "content": "Hello!" }
  ],
  "stream": false,
  "reasoning": {
    "effort": "medium"
  },
  "service_tier": "fast",
  "include": ["reasoning.encrypted_content"]
}
```

**参数说明:**

| 参数                 | 类型         | 必填 | 说明                                                                                               |
| -------------------- | ------------ | ---- | -------------------------------------------------------------------------------------------------- |
| model                | string       | 是   | 模型名称                                                                                           |
| input                | array/string | 是   | 输入内容（支持数组或字符串）                                                                       |
| stream               | boolean      | 否   | 是否启用流式响应，默认 false。仅当显式传 `stream=true` 时返回 SSE（流式响应），否则返回普通 JSON。 |
| reasoning.effort     | string       | 否   | 推理强度: low/medium/high                                                                          |
| service_tier         | string       | 否   | 服务等级: fast/auto                                                                                |
| include              | array        | 否   | 包含的额外字段                                                                                     |
| previous_response_id | string       | 否   | 上一响应 ID，用于上下文连续                                                                        |

**响应示例:**

```json
{
  "id": "resp_xxxxxxxx",
  "object": "response",
  "created": 1712345678,
  "model": "gpt-5.5",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [
        {
          "type": "output_text",
          "text": "Hello! How can I help you today?"
        }
      ]
    }
  ],
  "usage": {
    "input_tokens": 25,
    "output_tokens": 15,
    "total_tokens": 40
  }
}
```

### 3. Images

#### 生成图片

**端点:** `POST /v1/images/generations`

**说明:** OpenAI Images 兼容入口。外部请求使用 `gpt-image-2`，内部按 `CLIProxyAPI/` 与 `sub2api/` 的链路转换为 Codex `/responses`：主模型为 `gpt-5.4-mini`，图像模型写入 `tools[0].model`。

**请求示例:**

```json
{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "size": "1024x1024",
  "quality": "high",
  "response_format": "b64_json"
}
```

#### 编辑图片

**端点:** `POST /v1/images/edits`

**说明:** 支持 JSON `images[].image_url` 和 multipart `image` / `image[]` 上传。`mask.image_url` 或 multipart `mask` 可用于遮罩编辑。

**JSON 请求示例:**

```json
{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [{ "image_url": "https://example.com/source.png" }],
  "output_format": "png"
}
```

**响应示例:**

```json
{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [
    {
      "b64_json": "..."
    }
  ],
  "usage": {
    "images": 1
  }
}
```

### 4. List Models

**端点:** `GET /v1/models`

**说明:** 获取支持的模型列表。

**响应示例:**

```json
{
  "object": "list",
  "data": [
    { "id": "gpt-5.5", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.4", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.4-mini", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.3-codex", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.3-codex-spark", "object": "model", "owned_by": "openai" },
    { "id": "gpt-5.2", "object": "model", "owned_by": "openai" },
    { "id": "gpt-image-2", "object": "model", "owned_by": "openai" }
  ]
}
```

### 5. Health Check

**端点:** `GET /health`

**说明:** 健康检查端点，返回服务状态。

**响应示例:**

```json
{
  "status": "ok",
  "available": 5,
  "total": 8
}
```

---

## 管理 API

所有管理 API 需要 `X-Admin-Key` 请求头进行认证。

### 统计接口

#### GET /api/admin/stats

获取仪表盘统计数据。

**响应:**

```json
{
  "total": 10,
  "available": 8,
  "error": 2,
  "today_requests": 1234
}
```

#### GET /api/admin/health

系统健康检查（扩展版）。

**响应:**

```json
{
  "status": "ok",
  "available": 8,
  "total": 10
}
```

### 账号管理

#### GET /api/admin/accounts

获取账号列表。

**响应:**

```json
{
  "accounts": [
    {
      "id": 1,
      "name": "account-1",
      "email": "user@example.com",
      "plan_type": "pro",
      "status": "ready",
      "health_tier": "healthy",
      "scheduler_score": 100,
      "dispatch_score": 150,
      "score_bias_override": null,
      "score_bias_effective": 50,
      "base_concurrency_override": null,
      "base_concurrency_effective": 2,
      "skip_warm_tier": false,
      "dynamic_concurrency_limit": 2,
      "allowed_api_key_ids": [1, 3],
      "proxy_url": "http://proxy.example.com:8080",
      "created_at": "2024-01-01T00:00:00Z",
      "updated_at": "2024-01-01T12:00:00Z",
      "active_requests": 0,
      "total_requests": 100,
      "last_used_at": "2024-01-01T11:00:00Z",
      "success_requests": 95,
      "error_requests": 5,
      "credit_enabled": false,
      "credit_skip_usage_window": false,
      "billed_5h": 0.25,
      "billed_7d": 3.50,
      "usage_percent_7d": 45.2,
      "usage_percent_5h": 12.5,
      "reset_5h_at": "2024-01-01T17:00:00Z",
      "reset_7d_at": "2024-01-08T00:00:00Z",
      "scheduler_breakdown": {
        "unauthorized_penalty": 0,
        "rate_limit_penalty": 0,
        "timeout_penalty": 0,
        "server_penalty": 0,
        "failure_penalty": 0,
        "success_bonus": 12,
        "usage_penalty_7d": -5,
        "usage_urgency_bonus_5h": 0,
        "latency_penalty": 0,
        "success_rate_penalty": 0
      }
    }
  ]
}
```

字段说明补充：

| 字段                       | 类型         | 说明                                                              |
| -------------------------- | ------------ | ----------------------------------------------------------------- |
| scheduler_score            | number       | 原始健康分，仅反映动态调度健康状态                                |
| dispatch_score             | number       | 最终用于调度排序的分数；优先读取运行时快照                        |
| score_bias_override        | integer/null | 手工配置的总加权分覆盖值，`null` 表示跟随套餐默认                 |
| score_bias_effective       | integer      | 当前生效的加权分                                                  |
| base_concurrency_override  | integer/null | 手工配置的基础并发覆盖值，`null` 表示跟随全局 `max_concurrency`   |
| base_concurrency_effective | integer      | 当前生效的基础并发值                                              |
| skip_warm_tier             | bool         | 是否跳过 warm 层级；仅把 warm 提升为 healthy，不覆盖 risky/banned |
| allowed_api_key_ids        | integer[]    | 允许调用该账号的 API Key ID 列表；空数组表示所有 API Key 均可调用 |
| credit_enabled             | bool         | 是否为信用计费模式账号                                            |
| credit_skip_usage_window   | bool         | 是否跳过 7 天/5 小时用量窗口惩罚                                  |
| billed_5h                  | number/null  | 过去 5 小时窗口内的累计计费金额（USD）                            |
| billed_7d                  | number/null  | 过去 7 天窗口内的累计计费金额（USD）                              |

#### PATCH /api/admin/accounts/:id/scheduler

更新账号调度配置。

**请求:**

```json
{
  "score_bias_override": 80,
  "base_concurrency_override": 6,
  "skip_warm_tier": true,
  "allowed_api_key_ids": [1, 3]
}
```

字段可分别传 `null` 恢复自动值：

```json
{
  "score_bias_override": null,
  "base_concurrency_override": null,
  "allowed_api_key_ids": null
}
```

**参数说明:**

| 参数                      | 类型           | 必填 | 说明                                                                                                       |
| ------------------------- | -------------- | ---- | ---------------------------------------------------------------------------------------------------------- |
| score_bias_override       | integer/null   | 否   | 总加权分覆盖值，范围 `-200..200`，`null` 表示恢复套餐默认                                                  |
| base_concurrency_override | integer/null   | 否   | 基础并发覆盖值，范围 `1..50`，`null` 表示恢复全局默认                                                      |
| skip_warm_tier            | boolean/null   | 否   | 是否跳过 warm 层级；`null` 等同 `false`，字段省略时保持原值                                                |
| allowed_api_key_ids       | integer[]/null | 否   | 允许调用该账号的 API Key ID 列表，去重升序保存；字段省略时保持原值，传 `null` 或 `[]` 表示恢复为全部可调用 |

**响应:**

```json
{
  "message": "账号调度配置已更新"
}
```

#### PATCH /api/admin/accounts/:id/credit

更新账号信用设置。

**请求:**

```json
{
  "credit_enabled": true,
  "credit_skip_usage_window": true
}
```

**参数说明:**

| 参数                       | 类型  | 必填 | 说明                                     |
| -------------------------- | ----- | ---- | ---------------------------------------- |
| credit_enabled             | bool  | 否   | 标记账号为信用计费模式，省略时保持原值   |
| credit_skip_usage_window   | bool  | 否   | 跳过 7 天/5 小时用量窗口惩罚（仅在 `credit_enabled=true` 时生效），省略时保持原值 |

**响应:**

```json
{
  "message": "信用设置已更新",
  "credit_enabled": true,
  "credit_skip_usage_window": true
}
```

#### POST /api/admin/accounts/batch-update

批量更新账号启用、锁定、标签、分组和调度元信息。字段省略时保持原值；`ids` 会自动去重，已删除或不存在的账号计入 `failed`。

**请求:**

```json
{
  "ids": [1, 2, 3],
  "enabled": false,
  "locked": true,
  "tags": ["ops", "paid"],
  "group_ids": [1],
  "auto_pause_5h_threshold": 0.8,
  "auto_pause_7d_disabled": true
}
```

**响应:**

```json
{
  "message": "已更新 3 个账号，失败 0 个",
  "success": 3,
  "failed": 0
}
```

#### POST /api/admin/accounts

添加 Refresh Token 账号（支持批量）。

**请求:**

```json
{
  "name": "my-account",
  "refresh_token": "rt_xxxxxxxxxxxx",
  "proxy_url": "http://proxy.example.com:8080"
}
```

**参数说明:**

| 参数          | 类型   | 必填 | 说明                                                   |
| ------------- | ------ | ---- | ------------------------------------------------------ |
| name          | string | 否   | 账号名称，批量时自动追加序号，默认 `account-{n}`       |
| refresh_token | string | 是   | Refresh Token，多个用 `\n` 换行分隔（单次最多 100 个） |
| proxy_url     | string | 否   | 代理 URL                                               |

批量添加（使用换行分隔）:

```json
{
  "name": "batch",
  "refresh_token": "rt_xxx1\nrt_xxx2\nrt_xxx3",
  "proxy_url": ""
}
```

**响应:**

```json
{
  "message": "成功添加 3 个账号",
  "success": 3,
  "failed": 0
}
```

**curl 示例:**

单个添加:

```bash
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-account", "refresh_token": "rt_xxxxxxxxxxxx", "proxy_url": ""}'
```

批量添加（换行分隔）:

```bash
curl -X POST http://localhost:8080/api/admin/accounts \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "batch", "refresh_token": "rt_xxx1\nrt_xxx2\nrt_xxx3"}'
```

> 添加后系统自动在后台刷新 Access Token，无需手动触发。

#### POST /api/admin/accounts/at

添加 Access Token（AT-only）账号（支持批量）。适用于只有 AT 没有 RT 的场景。

**请求:**

```json
{
  "name": "my-at-account",
  "access_token": "eyJhbGciOiJSUzI1NiIs...",
  "proxy_url": "http://proxy.example.com:8080"
}
```

**参数说明:**

| 参数         | 类型   | 必填 | 说明                                                  |
| ------------ | ------ | ---- | ----------------------------------------------------- |
| name         | string | 否   | 账号名称，批量时自动追加序号，默认 `at-account-{n}`   |
| access_token | string | 是   | Access Token，多个用 `\n` 换行分隔（单次最多 100 个） |
| proxy_url    | string | 否   | 代理 URL                                              |

批量添加:

```json
{
  "name": "batch-at",
  "access_token": "eyJtoken1...\neyJtoken2...\neyJtoken3...",
  "proxy_url": ""
}
```

**响应:**

```json
{
  "message": "成功添加 3 个 AT 账号",
  "success": 3,
  "failed": 0
}
```

**curl 示例:**

```bash
curl -X POST http://localhost:8080/api/admin/accounts/at \
  -H "X-Admin-Key: your-admin-secret" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-at", "access_token": "eyJhbGciOiJSUzI1NiIs..."}'
```

> AT-only 账号无法自动刷新，过期后需重新添加。系统会自动解析 JWT 提取 email、plan_type 等信息。

#### DELETE /api/admin/accounts/:id

删除账号（软删除，标记为 deleted）。

**响应:**

```json
{
  "message": "账号已删除"
}
```

#### POST /api/admin/accounts/:id/refresh

手动刷新账号 Access Token。

**响应:**

```json
{
  "message": "账号刷新成功"
}
```

#### GET /api/admin/accounts/:id/test

测试账号连接。

**响应:**

```json
{
  "success": true,
  "latency_ms": 523,
  "message": "连接正常"
}
```

#### GET /api/admin/accounts/:id/usage

获取单个账号用量统计。

**响应:**

```json
{
  "id": 1,
  "name": "account-1",
  "total_requests": 100,
  "total_tokens": 5000,
  "last_7d_requests": 500,
  "last_7d_tokens": 25000
}
```

#### POST /api/admin/accounts/import

批量导入账号（支持 TXT/JSON/AT-TXT 三种格式）。

**请求:**

- Method: POST
- Content-Type: multipart/form-data

**Form 字段:**

| 字段      | 类型   | 必填 | 说明                                      |
| --------- | ------ | ---- | ----------------------------------------- |
| file      | file   | 是   | 上传文件（最大 20MB，JSON 格式支持多文件） |
| format    | string | 否   | 文件格式：`txt`（默认）、`json`、`at_txt` |
| proxy_url | string | 否   | 代理 URL                                  |

**format 格式说明:**

- **`txt`** — 每行一个 Refresh Token:

  ```text
  rt_xxxxxx1
  rt_xxxxxx2
  rt_xxxxxx3
  ```

- **`json`** — CLIProxyAPI 凭证 JSON 格式（支持数组或单对象）:

  ```json
  [
    { "refresh_token": "rt_xxx1", "email": "user1@example.com" },
    { "refresh_token": "rt_xxx2", "email": "user2@example.com" }
  ]
  ```

- **`at_txt`** — 每行一个 Access Token（AT-only 模式）:
  ```text
  eyJhbGciOiJSUzI1NiIs...token1
  eyJhbGciOiJSUzI1NiIs...token2
  ```

> 所有格式均自动文件内去重 + 数据库去重，已存在的 Token 计入 `duplicate` 不重复导入。

**curl 示例:**

导入 RT（TXT 格式）:

```bash
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@tokens.txt" \
  -F "format=txt" \
  -F "proxy_url=http://proxy.example.com:8080"
```

导入 RT（JSON 格式）:

```bash
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@credentials.json" \
  -F "format=json"
```

导入 AT（AT-TXT 格式）:

```bash
curl -X POST http://localhost:8080/api/admin/accounts/import \
  -H "X-Admin-Key: your-admin-secret" \
  -F "file=@access_tokens.txt" \
  -F "format=at_txt"
```

**响应:** SSE 流式进度

```text
data: {"type":"progress","current":5,"total":10,"success":3,"duplicate":1,"failed":1}

data: {"type":"complete","current":10,"total":10,"success":8,"duplicate":1,"failed":1}
```

若所有 Token 均已存在，返回普通 JSON（非 SSE）:

```json
{
  "message": "所有 10 个 RT 已存在，无需导入",
  "success": 0,
  "duplicate": 10,
  "failed": 0,
  "total": 10
}
```

#### POST /api/admin/accounts/batch-test

批量测试账号连接。

**请求:**

```json
{
  "ids": [1, 2, 3],
  "concurrency": 5
}
```

**响应:** SSE 流式进度

```
data: {"type":"progress","current":3,"total":3,"success":2,"failed":1}

data: {"type":"complete","current":3,"total":3,"success":2,"failed":1}
```

#### POST /api/admin/accounts/clean-banned

清理 Unauthorized（401）账号。

**响应:**

```json
{
  "message": "已清理 5 个账号",
  "cleaned": 5
}
```

#### POST /api/admin/accounts/clean-rate-limited

清理 Rate Limited（429）账号。

#### POST /api/admin/accounts/clean-error

清理 Error 状态账号。

#### GET /api/admin/accounts/export

导出账号（标准 JSON 格式）。

**查询参数:**

- `filter`: healthy (只导出健康账号)
- `ids`: 1,2,3 (指定 ID 列表)
- `remote`: true (远程迁移模式)

**响应:**

```json
[
  {
    "type": "codex",
    "email": "user@example.com",
    "expired": "2024-12-31T23:59:59Z",
    "id_token": "id_xxx",
    "account_id": "acc_xxx",
    "access_token": "at_xxx",
    "last_refresh": "2024-01-01T12:00:00Z",
    "refresh_token": "rt_xxx"
  }
]
```

#### POST /api/admin/accounts/migrate

从远程 codex2api 实例迁移账号。

**请求:**

```json
{
  "url": "http://remote-instance:8080",
  "admin_key": "remote-admin-secret"
}
```

**响应:** SSE 流式进度

#### GET /api/admin/accounts/event-trend

获取账号增删趋势。

**查询参数:**

- `start`: RFC3339 格式开始时间
- `end`: RFC3339 格式结束时间
- `bucket_minutes`: 聚合桶大小（默认 60）

**响应:**

```json
{
  "trend": [{ "timestamp": "2024-01-01T00:00:00Z", "added": 5, "deleted": 0 }]
}
```

### 用量统计

#### GET /api/admin/usage/stats

获取使用统计。

**响应:**

```json
{
  "total_requests": 10000,
  "total_tokens": 500000,
  "today_requests": 500,
  "today_tokens": 25000,
  "rpm": 50,
  "tpm": 2500,
  "error_rate": 0.02
}
```

#### GET /api/admin/usage/logs

获取使用日志。

**查询参数:**

- `start`: RFC3339 开始时间
- `end`: RFC3339 结束时间
- `page`: 页码
- `page_size`: 每页条数 (最大 200)
- `email`: 按账号邮箱过滤
- `model`: 按模型过滤
- `endpoint`: 按端点过滤
- `api_key_id`: 按 API 密钥 ID 过滤
- `fast`: true/false (是否 fast 服务)
- `stream`: true/false (是否流式)

**响应:**

```json
{
  "logs": [
    {
      "id": 1,
      "account_id": 1,
      "account_email": "user@example.com",
      "api_key_id": 3,
      "api_key_name": "Team A",
      "api_key_masked": "sk-t****...****1234",
      "endpoint": "/v1/chat/completions",
      "model": "gpt-5.5",
      "status_code": 200,
      "duration_ms": 523,
      "first_token_ms": 150,
      "prompt_tokens": 25,
      "completion_tokens": 15,
      "total_tokens": 40,
      "created_at": "2024-01-01T12:00:00Z"
    }
  ],
  "total": 1000
}
```

#### GET /api/admin/usage/chart-data

获取图表聚合数据。

**查询参数:**

- `start`: RFC3339 开始时间
- `end`: RFC3339 结束时间
- `bucket_minutes`: 聚合桶大小（默认 5）

**响应:**

```json
{
  "buckets": [
    {
      "time": "2024-01-01T12:00:00Z",
      "requests": 50,
      "tokens": 2500,
      "latency_ms": 500
    }
  ],
  "total_requests": 1000,
  "total_tokens": 50000
}
```

#### DELETE /api/admin/usage/logs

清空使用日志。

**响应:**

```json
{
  "message": "日志已清空"
}
```

### API Key 管理

#### GET /api/admin/keys

获取所有 API 密钥。管理接口需要 `X-Admin-Key`，这些接口不属于对外 `/v1/*` 客户端 API。该接口会在 `raw_key` 返回完整密钥，只能在受信任后台使用。

**响应:**

```json
{
  "keys": [
    {
      "id": 1,
      "name": "Claude Code",
      "key": "sk-****...abcd",
      "raw_key": "sk-live-full-key",
      "quota_limit": 10,
      "quota_used": 1.25,
      "expires_at": "2026-06-01T00:00:00Z",
      "allowed_group_ids": [1],
      "status": "active",
      "created_at": "2024-01-01T00:00:00Z"
    }
  ]
}
```

#### POST /api/admin/keys

创建新 API 密钥。

**请求:**

```json
{
  "name": "production",
  "key": "sk-custom-key",
  "quota_limit": 10,
  "expires_in_days": 30,
  "allowed_group_ids": [1]
}
```

| 字段                | 类型      | 必填 | 说明                                   |
| ------------------- | --------- | ---- | -------------------------------------- |
| name                | string    | 是   | 显示名称                               |
| key                 | string    | 否   | 自定义密钥；省略则自动生成             |
| quota_limit / quota | number    | 否   | 额度上限，0 或省略表示不限额           |
| expires_at          | string    | 否   | RFC3339 或本地日期时间                 |
| expires_in_days     | number    | 否   | N 天后过期；0 表示不过期               |
| allowed_group_ids   | integer[] | 否   | 允许调度的账号分组；空数组表示全部分组 |

**响应:**

```json
{
  "id": 2,
  "key": "sk-xxxxxxxxxxxxxxxxxxxxxxxx",
  "name": "production",
  "quota_limit": 10,
  "quota_used": 0,
  "expires_at": "2026-06-12T00:00:00Z",
  "allowed_group_ids": [1]
}
```

#### PATCH /api/admin/keys/:id

编辑 API 密钥名称、额度、过期时间和允许账号分组。字段省略时保持原值。

**请求:**

```json
{
  "name": "Cherry Studio",
  "quota_limit": 25,
  "expires_at": null,
  "allowed_group_ids": []
}
```

| 字段                | 类型        | 说明                                   |
| ------------------- | ----------- | -------------------------------------- |
| name                | string      | 新显示名称                             |
| quota_limit / quota | number/null | 新额度上限；0 或 null 清除额度限制     |
| expires_at          | string/null | 新过期时间；null 清除过期时间          |
| expires_in_days     | number      | N 天后过期；0 清除过期时间             |
| allowed_group_ids   | integer[]   | 允许调度的账号分组；空数组表示全部分组 |

**响应:**

```json
{
  "message": "API Key 已更新"
}
```

#### DELETE /api/admin/keys/:id

删除 API 密钥。

**响应:**

```json
{
  "message": "已删除"
}
```

### 账号分组管理

账号分组用于把账号池划分为多个可调度集合。API Key 的 `allowed_group_ids` 可以限制下游密钥只能使用指定分组；账号自己的 `allowed_api_key_ids` 也可以反向限制哪些 API Key 能调度该账号。

#### GET /api/admin/account-groups

获取账号分组。

**响应:**

```json
{
  "groups": [
    {
      "id": 1,
      "name": "Team",
      "description": "付费团队账号",
      "color": "#2563eb",
      "sort_order": 0,
      "member_count": 8,
      "created_at": "2026-05-13T00:00:00Z",
      "updated_at": "2026-05-13T00:00:00Z"
    }
  ]
}
```

#### POST /api/admin/account-groups

创建账号分组。

**请求:**

```json
{
  "name": "Team",
  "description": "付费团队账号",
  "color": "#2563eb",
  "sort_order": 0
}
```

**响应:**

```json
{
  "id": 1,
  "message": "分组已创建"
}
```

#### PATCH /api/admin/account-groups/:id

编辑账号分组。

**请求:**

```json
{
  "name": "Team Plus",
  "description": "高优先级账号",
  "color": "#16a34a",
  "sort_order": 10
}
```

**响应:**

```json
{
  "message": "分组已更新"
}
```

#### DELETE /api/admin/account-groups/:id

删除账号分组。分组仍有成员时需要 `?force=true`；删除后会从账号关系中移除该 ID，并尽量从 API Key 允许分组中清理。若某个 API Key 仅绑定该分组，为避免权限被意外放大，会保留为缺失分组状态。

```bash
curl -X DELETE "http://localhost:8080/api/admin/account-groups/1?force=true" \
  -H "X-Admin-Key: your-secret"
```

**响应:**

```json
{
  "message": "分组已删除"
}
```

### 系统设置

#### GET /api/admin/settings

获取系统设置。

**响应:**

```json
{
  "max_concurrency": 2,
  "global_rpm": 0,
  "test_model": "gpt-5.5",
  "test_concurrency": 50,
  "proxy_url": "",
  "pg_max_conns": 50,
  "redis_pool_size": 30,
  "auto_clean_unauthorized": false,
  "auto_clean_rate_limited": false,
  "auto_clean_full_usage": false,
  "auto_clean_error": false,
  "proxy_pool_enabled": false,
  "fast_scheduler_enabled": false,
  "max_retries": 3,
  "max_rate_limit_retries": 2,
  "scheduler_mode": "round_robin",
  "allow_remote_migration": false,
  "database_driver": "postgres",
  "database_label": "PostgreSQL",
  "cache_driver": "redis",
  "cache_label": "Redis",
  "admin_secret": "",
  "admin_auth_source": "env"
}
```

#### PUT /api/admin/settings

更新系统设置。

**请求:**

```json
{
  "max_concurrency": 4,
  "global_rpm": 100,
  "test_model": "gpt-5.5",
  "test_concurrency": 50,
  "proxy_url": "http://proxy.example.com:8080",
  "auto_clean_unauthorized": true,
  "auto_clean_rate_limited": false,
  "fast_scheduler_enabled": true,
  "scheduler_mode": "remaining_quota",
  "max_rate_limit_retries": 2
}
```

**响应:** 更新后的完整设置对象

### 代理池管理

#### GET /api/admin/proxies

获取代理列表。

**响应:**

```json
{
  "proxies": [
    {
      "id": 1,
      "url": "http://proxy1.example.com:8080",
      "label": "US Proxy",
      "enabled": true,
      "last_tested_at": "2024-01-01T12:00:00Z",
      "last_test_result": "ok",
      "latency_ms": 150
    }
  ]
}
```

#### POST /api/admin/proxies

添加代理（支持批量）。

**请求:**

```json
{
  "urls": ["http://proxy1.example.com:8080", "http://proxy2.example.com:8080"],
  "label": "Batch Add"
}
```

或单条:

```json
{
  "url": "http://proxy.example.com:8080",
  "label": "US Proxy"
}
```

#### DELETE /api/admin/proxies/:id

删除代理。

#### PATCH /api/admin/proxies/:id

更新代理。

**请求:**

```json
{
  "label": "New Label",
  "enabled": false
}
```

#### POST /api/admin/proxies/batch-delete

批量删除代理。

**请求:**

```json
{
  "ids": [1, 2, 3]
}
```

#### POST /api/admin/proxies/test

测试代理连通性。

**请求:**

```json
{
  "url": "http://proxy.example.com:8080",
  "id": 1, // 可选，用于持久化测试结果
  "lang": "zh-CN"
}
```

**响应:**

```json
{
  "success": true,
  "ip": "1.2.3.4",
  "country": "United States",
  "region": "California",
  "city": "Los Angeles",
  "isp": "Example ISP",
  "latency_ms": 150,
  "location": "United States·California·Los Angeles"
}
```

### 运维监控

#### GET /api/admin/ops/overview

获取系统运维概览。

**响应:**

```json
{
  "updated_at": "2024-01-01T12:00:00Z",
  "uptime_seconds": 86400,
  "database_driver": "postgres",
  "database_label": "PostgreSQL",
  "cache_driver": "redis",
  "cache_label": "Redis",
  "cpu": {
    "percent": 25.5,
    "cores": 8
  },
  "memory": {
    "percent": 60.2,
    "used_bytes": 6442450944,
    "total_bytes": 10737418240
  },
  "runtime": {
    "goroutines": 50,
    "available_accounts": 8,
    "total_accounts": 10
  },
  "requests": {
    "active": 5,
    "total": 10000
  },
  "postgres": {
    "healthy": true,
    "open": 10,
    "in_use": 5,
    "idle": 5,
    "max_open": 50,
    "wait_count": 0,
    "usage_percent": 20
  },
  "redis": {
    "healthy": true,
    "total_conns": 10,
    "idle_conns": 5,
    "stale_conns": 0,
    "pool_size": 30,
    "usage_percent": 33.3
  },
  "traffic": {
    "qps": 10.5,
    "qps_peak": 50.0,
    "tps": 500.0,
    "tps_peak": 2000.0,
    "rpm": 600,
    "tpm": 30000,
    "error_rate": 0.02,
    "today_requests": 5000,
    "today_tokens": 250000,
    "rpm_limit": 0
  }
}
```

### 模型管理

#### GET /api/admin/models

获取当前启用的模型列表，并返回模型注册表元数据。

**响应:**

```json
{
  "models": [
    "gpt-5.5",
    "gpt-5.4",
    "gpt-5.4-mini",
    "gpt-5.3-codex",
    "gpt-5.3-codex-spark",
    "gpt-5.2",
    "gpt-image-2"
  ],
  "items": [
    {
      "id": "gpt-5.3-codex-spark",
      "enabled": true,
      "category": "codex",
      "source": "official_codex_docs",
      "pro_only": true,
      "api_key_auth_available": true
    }
  ],
  "last_synced_at": "2026-04-24T00:00:00Z",
  "source_url": "https://developers.openai.com/codex/models"
}
```

#### POST /api/admin/models/sync

从 OpenAI 官方 Codex 模型页同步模型注册表。同步只新增或更新模型元数据，不会自动删除本地模型；`gpt-image-2` 始终作为内置图像模型保留。

**响应:**

```json
{
  "added": 0,
  "updated": 2,
  "unchanged": 5,
  "skipped": ["gpt-5.2-codex"],
  "models": [
    "gpt-5.5",
    "gpt-5.4",
    "gpt-5.4-mini",
    "gpt-5.3-codex",
    "gpt-5.3-codex-spark",
    "gpt-5.2",
    "gpt-image-2"
  ],
  "last_synced_at": "2026-04-24T00:00:00Z",
  "source_url": "https://developers.openai.com/codex/models"
}
```

### 生图工作台

管理后台生图工作台的 API，支持文生图（`/images/jobs`）和图生图（`/images/edit-jobs`）任务，以及图库管理。

#### POST /api/admin/images/jobs

创建文生图任务。

**请求:**

```json
{
  "prompt": "A small orange cat sitting on a cloud",
  "model": "gpt-image-2",
  "size": "1024x1024",
  "quality": "high",
  "output_format": "png",
  "style": "photorealistic",
  "api_key_id": 0
}
```

**参数说明:**

| 参数          | 类型   | 必填 | 说明                          |
| ------------- | ------ | ---- | ----------------------------- |
| prompt        | string | 是   | 提示词，最长 8000 字符        |
| model         | string | 否   | 模型，默认 `gpt-image-2`      |
| size          | string | 否   | 输出尺寸                      |
| quality       | string | 否   | 质量等级                      |
| output_format | string | 否   | 输出格式，默认 `png`          |
| style         | string | 否   | 风格说明                      |
| upscale       | string | 否   | 超分选项                      |
| api_key_id    | int    | 否   | 指定 API Key ID               |

**响应:**

```json
{
  "id": 1,
  "status": "pending"
}
```

#### POST /api/admin/images/edit-jobs

创建图生图（image-to-image）任务。与文生图参数类似，但需要额外提供参考图片。

**请求:**

```json
{
  "prompt": "Replace the background with a sunset scene",
  "model": "gpt-image-2",
  "size": "1024x1024",
  "output_format": "png",
  "input_images": [
    "https://example.com/source.png",
    "data:image/png;base64,iVBORw0KGgo..."
  ]
}
```

**参数说明:**

| 参数          | 类型     | 必填 | 说明                                 |
| ------------- | -------- | ---- | ------------------------------------ |
| prompt        | string   | 是   | 编辑提示词，最长 8000 字符           |
| model         | string   | 否   | 模型，默认 `gpt-image-2`             |
| input_images  | string[] | 是   | 参考图片 URL 或 data URI 列表        |
| size          | string   | 否   | 输出尺寸                             |
| quality       | string   | 否   | 质量等级                             |
| output_format | string   | 否   | 输出格式，默认 `png`                 |
| api_key_id    | int      | 否   | 指定 API Key ID                      |

#### GET /api/admin/images/jobs

获取生图任务列表。支持分页和状态过滤。

**响应:**

```json
{
  "jobs": [
    {
      "id": 1,
      "prompt": "A small orange cat",
      "model": "gpt-image-2",
      "status": "completed",
      "created_at": "2024-01-01T12:00:00Z"
    }
  ],
  "total": 50
}
```

#### GET /api/admin/images/jobs/:id

获取单个生图任务详情及结果。

#### DELETE /api/admin/images/jobs/:id

删除一个生图任务及其关联的所有图库资产。

```bash
curl -X DELETE http://localhost:8080/api/admin/images/jobs/1 \
  -H "X-Admin-Key: your-secret"
```

#### GET /api/admin/images/assets

获取图库资产列表。

#### GET /api/admin/images/assets/:id/file

获取单个图库资产文件（返回图片二进制或签名 URL）。

#### DELETE /api/admin/images/assets/:id

删除单个图库资产。

### OAuth 授权

通过 OAuth PKCE 流程授权获取 Codex 账号的 Refresh Token，适用于无法手动获取 RT 的场景。

**流程:** 生成授权 URL → 用户在浏览器中完成授权 → 用授权码兑换 Token 并写入系统

#### POST /api/admin/oauth/generate-auth-url

生成 OAuth 授权 URL（PKCE 模式）。

**请求:**

```json
{
  "proxy_url": "http://proxy.example.com:8080",
  "redirect_uri": "https://example.com/callback"
}
```

**参数说明:**

| 参数         | 类型   | 必填 | 说明                         |
| ------------ | ------ | ---- | ---------------------------- |
| proxy_url    | string | 否   | 账号使用的代理 URL           |
| redirect_uri | string | 否   | 回调地址，默认为系统内置地址 |

**响应:**

```json
{
  "auth_url": "https://auth.openai.com/authorize?response_type=code&client_id=...&state=...",
  "session_id": "a1b2c3d4e5f6..."
}
```

> 将 `auth_url` 在浏览器中打开，完成授权后获取回调 URL 中的 `code` 和 `state` 参数。`session_id` 有效期 30 分钟。

#### POST /api/admin/oauth/exchange-code

用授权码兑换 Token，自动创建新账号并加入号池。

**请求:**

```json
{
  "session_id": "a1b2c3d4e5f6...",
  "code": "auth_code_from_callback",
  "state": "state_from_callback",
  "name": "my-oauth-account",
  "proxy_url": "http://proxy.example.com:8080"
}
```

**参数说明:**

| 参数       | 类型   | 必填 | 说明                                     |
| ---------- | ------ | ---- | ---------------------------------------- |
| session_id | string | 是   | `generate-auth-url` 返回的 session_id    |
| code       | string | 是   | 授权回调 URL 中的 `code` 参数            |
| state      | string | 是   | 授权回调 URL 中的 `state` 参数           |
| name       | string | 否   | 账号名称，默认使用邮箱或 `oauth-account` |
| proxy_url  | string | 否   | 代理 URL，覆盖生成 URL 时的设置          |

**响应:**

```json
{
  "message": "OAuth 账号 user@example.com 添加成功",
  "id": 42,
  "email": "user@example.com",
  "plan_type": "pro"
}
```

---

## 支持模型

| 模型                | 说明                                                        |
| ------------------- | ----------------------------------------------------------- |
| gpt-5.5             | 最新旗舰模型。计费：$5.00/M 输入 / $30.00/M 输出（标准），priority 分别为 $12.50/M / $75.00/M |
| gpt-5.4             | 旗舰模型                                                    |
| gpt-5.4-mini        | 轻量版                                  |
| gpt-5.3-codex       | 较新版本                                |
| gpt-5.3-codex-spark | Codex Spark 模型，仅 Pro 订阅账号可调用 |
| gpt-5.2             | 兼容保留模型                            |
| gpt-image-2         | GPT Image 2 图像生成模型                |

> 提示：实际支持的模型以 `/v1/models` 接口返回为准，文档可能未及时更新。

---

## 错误码

### HTTP 状态码

| 状态码 | 说明                     |
| ------ | ------------------------ |
| 200    | 请求成功                 |
| 400    | 请求参数错误             |
| 401    | 认证失败                 |
| 403    | 权限不足                 |
| 404    | 资源不存在               |
| 429    | 请求过于频繁（限流）     |
| 499    | 客户端断开连接           |
| 500    | 服务器内部错误           |
| 502    | 网关错误（上游服务异常） |
| 503    | 服务不可用（账号池耗尽） |
| 598    | 上游流中断               |

### 错误响应格式

```json
{
  "error": {
    "message": "错误描述",
    "type": "错误类型",
    "code": "错误代码"
  }
}
```

### 常见错误代码

| 代码                             | 说明             | 处理建议                         |
| -------------------------------- | ---------------- | -------------------------------- |
| missing_api_key                  | 缺少 API Key     | 添加 Authorization 请求头        |
| invalid_api_key                  | API Key 无效     | 检查密钥是否正确                 |
| authentication_error             | 认证错误         | 检查 Admin Secret 或 API Key     |
| invalid_request_error            | 请求参数错误     | 检查请求体格式                   |
| server_error                     | 服务器错误       | 查看日志排查问题                 |
| upstream_error                   | 上游服务错误     | 检查 Codex 服务状态              |
| no_available_account             | 当前无可调度账号 | 稍后重试、启用账号或补充可用账号 |
| account_pool_usage_limit_reached | 账号池额度耗尽   | 等待冷却或添加新账号             |
| rate_limit_exceeded              | 限流触发         | 降低请求频率                     |

---

## 限流说明

### 全局 RPM 限流

通过 `global_rpm` 设置限制全局每分钟请求数。

- `global_rpm = 0`: 无限流
- `global_rpm > 0`: 启用 RPM 限流

### 账号级别限流

系统会自动根据账号状态进行限流：

- **Healthy**: 正常并发
- **Warm**: 并发减半
- **Risky**: 固定 1 并发
- **Banned**: 0 并发，不参与调度

### 无可调度账号响应

当账号池中没有账号可被调度时，接口返回 `503 Service Unavailable`：

```json
{
  "error": {
    "message": "无可用账号，请稍后重试",
    "type": "server_error",
    "code": "no_available_account"
  }
}
```

### 账号池额度耗尽响应

当上游返回账号额度耗尽类 `429` 时，系统会对外改写为 `503 Service Unavailable`，并保留 `Retry-After` 头：

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 3600

{
  "error": {
    "message": "账号池额度已耗尽，请稍后重试",
    "type": "server_error",
    "code": "account_pool_usage_limit_reached",
    "plan_type": "free",
    "resets_at": 1712345678,
    "resets_in_seconds": 3600
  }
}
```

### 建议

1. 监控 `X-RateLimit-*` 响应头（如有）
2. 实现指数退避重试策略
3. 处理 429/503 状态码，根据 `Retry-After` 等待后重试
4. 避免在短时内发送大量请求
