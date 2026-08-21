# Codex2API API 文档

本文档详细描述 Codex2API 的所有 API 端点、请求/响应格式以及错误码说明。

## 目录

- [概述](#概述)
- [认证](#认证)
- [公共 API](#公共-api)
  - [Chat Completions](#1-chat-completions)
  - [Responses](#2-responses)
  - [Images](#3-images)
  - [Videos (Grok 生视频)](#4-videos-grok-生视频)
  - [List Models](#5-list-models)
  - [Health Check](#6-health-check)
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

**Service Tier 语义说明**：请求侧 `fast` / `priority` 会统一以 `priority` 转发上游，其余取值（`auto`/`default`/`flex`/`scale` 等）不转发。用量日志区分三个字段：`requested_service_tier`（客户端请求意图）、`actual_service_tier`（上游回传 Tier，原样取自 `response.completed.response.service_tier`）、`billing_service_tier`（计费采用值，由 Tier 计费策略 `BillingTierPolicy` 决定，默认 `actual`，上游未回传时回退按请求意图计）。注意：在 ChatGPT OAuth / Codex backend 路径上，Fast 由上游服务端路由处理，`service_tier` 不是端到端可校验字段——上游回传 `default` 并不代表 Fast 未生效（openai/codex#14204 官方说明；#494 的交错 A/B 实测在回传 `default` 时仍有约 1.5× 生成吞吐提升）。因此"上游回传 Tier"仅反映上游申报值，不能单独用于判断加速是否生效；`BillingTierPolicy=actual` 下此类请求按标准价计费。

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

多个最终用户共享同一个 API Key 时，可选传入稳定的本地亲和标识：

```http
X-Codex2API-Affinity-Key: tenant-user-or-conversation-id
```

该请求头优先于其他会话亲和信号。Codex2API 会先对原始值做 SHA-256 派生，只保留本地路由标识；原始值不会保存，也不会转发给上游。

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

`previous_response_id` 的上下文先查当前进程的有界 L1。已认证请求按 API Key ID 隔离；未配置任何 API Key、显式启用 `CODEX_ALLOW_ANONYMOUS=true` 后放行的请求共用 `anon` 命名空间。Redis 模式在 L1 未命中时可从共享后端重建；后端值未超过重建上限但超过 L1 准入预算时仍可服务本次请求，只是不提升到 L1。Memory 模式没有共享 response context 后备，依赖上下文被判定为超限、已淘汰或缺失时可能返回 HTTP `409 response_context_unavailable`。共享后端暂时不可用且请求依赖该上下文时可能返回 HTTP `503 service_unavailable`。如果账号池存在可用的 relay-style 后备，网关可保留原始 `previous_response_id` 继续转发，而不是立即返回上述错误。客户端原生 Responses WebSocket 入口不执行这次本地查找，会保留 `previous_response_id` 交给上游。

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

#### Grok 生图（grok-imagine 系列）

同一个 `/v1/images/generations` / `/v1/images/edits` 端点也接受 Grok Imagine 模型，按模型名自动分派到 Grok 媒体上游（官方 REST），响应为标准 OpenAI Images 形状原样透传。

**可用模型:**

| 模型 | 说明 |
| --- | --- |
| `grok-imagine-image` | 标准档生图 |
| `grok-imagine-image-quality` | 质量档生图（`grok-imagine` 是它的别名） |

**Grok 专属参数:**

| 参数 | 说明 |
| --- | --- |
| `aspect_ratio` | 宽高比，如 `16:9` / `1:1` |
| `resolution` | `1k` / `2k`；未显式给出时，`size` 任一边 ≥2048 自动映射为 `2k`（`size` 字段本身不发给上游） |
| `quality` | 上游质量档位 |
| `n` | 生成张数 |
| `response_format` | `url` / `b64_json` |

**请求示例:**

```json
{
  "model": "grok-imagine-image",
  "prompt": "a red apple on a wooden table, studio lighting",
  "response_format": "url",
  "n": 1
}
```

**响应示例（上游原样透传，含成本信息）:**

```json
{
  "data": [
    { "url": "https://imgen.x.ai/xai-imgen/....jpeg", "mime_type": "image/jpeg" }
  ],
  "usage": { "cost_in_usd_ticks": 200000000 }
}
```

**注意事项:**

- 需要**付费 Grok 账号**（API Key 或 SuperGrok/Heavy 等付费 OAuth 订阅）；free 计划账号上游直接 403。调度层优先选择付费凭据账号。
- 不支持 `stream=true`（返回 400）。
- 编辑（`/v1/images/edits`）源图最多 **3 张**（`images[].image_url` 支持 https URL 与 data URL）。
- `upstream_channel=codex` 的 API Key 无法调用 grok-imagine 模型（403）。

#### 编辑图片

**端点:** `POST /v1/images/edits`

**说明:** 支持 JSON `images[].image_url` 和 multipart `image` / `image[]` 上传。`mask.image_url` 或 multipart `mask` 可用于遮罩编辑（遮罩仅 gpt-image 系列支持）。

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

### 4. Videos (Grok 生视频)

基于 Grok Imagine 的视频生成，异步任务模式：创建返回 `request_id`，客户端轮询状态，产物经网关代理下载。需要**付费 Grok 账号**（free 计划上游 403）。

**可用模型与操作支持矩阵**（上游实测）:

| 操作 | `grok-imagine-video` | `grok-imagine-video-1.5` |
| --- | --- | --- |
| generations | ✓ | ✓（默认） |
| edits | ✓（默认） | ✗ 上游 400 "not supported for this model" |
| extensions | ✓（默认） | ✗ 同上 |

`model` 省略时按操作自动选默认模型；xAI 公开 API 上的 `grok-imagine-video-1.5-preview` 也接受，转发时自动归一。

#### 创建视频任务

**端点:**

- `POST /v1/videos/generations` — 文生视频 / 图生视频
- `POST /v1/videos/edits` — 视频编辑（`video` 字段必填）
- `POST /v1/videos/extensions` — 视频延展（`video` 字段必填）

**请求参数:**

| 参数 | 说明 |
| --- | --- |
| `model` | 可省略，generations 默认 `grok-imagine-video-1.5`，edits/extensions 默认 `grok-imagine-video` |
| `prompt` | 提示词（generations 无图片输入时必填） |
| `duration` | 时长秒数，1–15，默认 8 |
| `resolution` | `480p` / `720p` / `1080p`（1080p 仅 1.5 且无参考图） |
| `aspect_ratio` | 如 `16:9` / `9:16` / `1:1` |
| `image` | 首帧图（图生视频），`{"url": "https://... 或 data:..."}` |
| `reference_images` | 参考图数组（最多 7 张，与 `image` 互斥），元素为 URL 字符串或 `{"url": ...}` |
| `video` | edits/extensions 的源视频，`{"url": ...}` |

**请求示例:**

```json
{
  "model": "grok-imagine-video-1.5",
  "prompt": "ocean waves rolling onto a sandy beach at sunset, cinematic",
  "duration": 4,
  "resolution": "480p",
  "aspect_ratio": "16:9"
}
```

**响应:** `{"request_id": "1a293702-..."}`

#### 查询任务状态

**端点:** `GET /v1/videos/:request_id`

由客户端轮询（建议间隔 2–5 秒）。状态机：`pending → done | failed | expired`。进行中响应形如 `{"status":"pending","progress":42}`（上游以 202 返回，网关统一按 200 透传，客户端只需看 `status` 字段）。**必须用创建任务的同一个 API Key 查询**，否则 404；任务绑定创建时选中的上游账号，绑定有效期 24 小时。

**完成响应示例:**

```json
{
  "status": "done",
  "progress": 100,
  "model": "grok-imagine-video-1.5",
  "video": {
    "url": "http://<gateway>/v1/videos/1a293702-.../content",
    "duration": 4,
    "respect_moderation": true
  },
  "usage": { "cost_in_usd_ticks": 3200000000 }
}
```

`video.url` 已被重写为网关自己的 `/content` 代理地址（上游签名 URL 会过期，统一走网关下载）。

#### 下载视频产物

**端点:** `GET /v1/videos/:request_id/content`

返回 `video/mp4` 字节流，支持 `Range` 请求（206）。网关优先匿名拉取上游签名资产 URL（仅限官方资产域白名单、禁跳转），失败时回退带凭据的上游下载端点。

**注意事项:**

- 网关不做后台轮询与产物落盘；重启后（内存缓存模式）或超过 24 小时，任务绑定丢失，状态查询返回 404。Redis 部署的绑定跨实例、跨重启有效。
- `upstream_channel=codex` 的 API Key 无法调用视频端点（403）。

### 5. List Models

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
    { "id": "gpt-image-2", "object": "model", "owned_by": "openai" },
    { "id": "grok-imagine-image", "object": "model", "owned_by": "xai" },
    { "id": "grok-imagine-video-1.5", "object": "model", "owned_by": "xai" }
  ]
}
```

池内存在 Grok 账号时会一并列出其文本模型（如 `grok-4.6`）与媒体模型（`grok-imagine-*`）。媒体模型与账号的文本模型白名单相互独立：白名单只声明文本模型不会关闭媒体能力；白名单里显式写了 `grok-imagine` 条目时以声明为准收窄。

#### Grok 的 GPT 兼容别名

Grok 账号编辑页支持账号级模型映射，可让只请求 GPT 模型名的客户端改走 Grok。例如：

```json
{
  "gpt-5.5": "grok-4.5",
  "gpt-5.4": "grok-4.5",
  "gpt-5.3-codex": "grok-4.5"
}
```

推荐逐个配置精确别名，不要默认使用 `gpt-*`，以免把未来的专用或媒体模型也纳入映射。别名目标必须存在于该 Grok 账号的可见模型目录中；显式 `models` 白名单会进一步收窄目标，隐藏或目录外模型不会因映射重新开放。账号尚未同步目录且未声明白名单时，仅使用保守的 Grok 默认模型集。满足这些条件的精确别名会出现在该 API Key 的 `GET /v1/models` 结果中。

映射适用于普通 HTTP `POST /v1/responses`、`POST /v1/chat/completions` 和 `POST /v1/messages`。Responses WebSocket 与 `/v1/responses/compact` 不会路由到 Grok。Codex 客户端的 function、namespace、custom、deferred `additional_tools` 和 `tool_search` 可经现有协议桥接；Web Search、File Search、Code Interpreter、Shell、MCP、图片生成等托管工具仍取决于具体 Grok 上游及协议能力，不能仅靠模型别名获得 OpenAI 后端的等价能力。

### 6. Health Check

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
      "token_workspace_id": "personal-workspace-id",
      "workspace_id_override": "team-workspace-id",
      "effective_workspace_id": "team-workspace-id",
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
| token_workspace_id         | string       | Token 中识别出的默认工作区 ID                                    |
| workspace_id_override      | string       | `Chatgpt-Account-Id` 指定的目标工作区；未指定时为空               |
| effective_workspace_id     | string       | 实际路由工作区；优先使用覆盖值，否则使用 Token 默认工作区         |
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
| base_concurrency_override | integer/null   | 否   | 基础并发覆盖值，`≥1` 无上限，`null` 表示恢复全局默认                                                      |
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

必须至少提供一个更新字段；仅提交 `ids` 会返回 `400 Bad Request`：

```json
{
  "error": "请提供要更新的字段"
}
```

**请求:**

```json
{
  "ids": [1, 2, 3],
  "enabled": false,
  "locked": true,
  "score_bias_override": 25,
  "base_concurrency_override": 4,
  "scheduler_priority": 10,
  "tags": ["ops", "paid"],
  "group_ids": [1],
  "auto_pause_5h_threshold": 0.8,
  "auto_pause_7d_disabled": true
}
```

常用批量调度字段：

| 字段 | 类型 | 范围/语义 |
|------|------|-----------|
| `score_bias_override` | integer/null | `-200..200`；`null` 恢复套餐默认分数偏置 |
| `base_concurrency_override` | integer/null | `≥1` 无上限；`null` 恢复分组或全局继承值 |
| `scheduler_priority` | integer/null | `-100..100`；`null` 恢复默认优先级 `0` |
| `tags` | string[] | 替换账号标签；空数组清空 |
| `group_ids` | integer[] | 替换账号分组；空数组清空 |

**响应:**

```json
{
  "message": "已更新 3 个账号，失败 0 个",
  "success": 3,
  "failed": 0
}
```

#### POST /api/admin/accounts/grok/batch-models

批量替换 Grok 账号的模型白名单。`ids` 会自动去重；非 Grok 或不存在的账号计入 `failed`，不中断整批。空数组表示清空显式白名单，之后按账号可见目录或首次同步前的保守默认模型集准入。

**请求:**

```json
{
  "ids": [1, 2, 3],
  "models": ["grok-4.5"]
}
```

| 参数 | 类型 | 必填 | 说明 |
| ---- | ---- | ---- | ---- |
| ids | integer[] | 是 | 要更新的 Grok 账号 ID |
| models | string[] | 否 | 替换后的模型白名单；省略或空数组表示清空显式白名单并恢复目录/默认集准入 |

**响应:**

```json
{
  "message": "已更新 3 个账号，失败 0 个",
  "success": 3,
  "failed": 0,
  "models": ["grok-4.5"]
}
```

#### POST /api/admin/accounts

添加 Refresh Token 账号（支持批量）。

**请求:**

```json
{
  "name": "my-account",
  "refresh_token": "rt_xxxxxxxxxxxx",
  "proxy_url": "http://proxy.example.com:8080",
  "custom_headers": {
    "Chatgpt-Account-Id": "team-workspace-id"
  }
}
```

**参数说明:**

| 参数          | 类型   | 必填 | 说明                                                   |
| ------------- | ------ | ---- | ------------------------------------------------------ |
| name          | string | 否   | 账号名称，批量时自动追加序号，默认 `account-{n}`       |
| refresh_token | string | 是   | Refresh Token，多个用 `\n` 换行分隔（单次最多 100 个） |
| proxy_url     | string | 否   | 代理 URL                                               |
| custom_headers | object | 否   | 自定义上游请求头；`Chatgpt-Account-Id` 用于指定目标工作区 |

同一份 RT 可以分别以“默认工作区”和多个 `Chatgpt-Account-Id` 路由加入账号池。额度、冷却、调度和统计按账号记录独立维护；同一登录身份下的相同目标工作区仍会去重。

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
  "proxy_url": "http://proxy.example.com:8080",
  "custom_headers": {
    "Chatgpt-Account-Id": "team-workspace-id"
  }
}
```

**参数说明:**

| 参数         | 类型   | 必填 | 说明                                                  |
| ------------ | ------ | ---- | ----------------------------------------------------- |
| name         | string | 否   | 账号名称，批量时自动追加序号，默认 `at-account-{n}`   |
| access_token | string | 是   | Access Token，多个用 `\n` 换行分隔（单次最多 100 个） |
| proxy_url    | string | 否   | 代理 URL                                              |
| custom_headers | object | 否 | 自定义上游请求头；`Chatgpt-Account-Id` 用于指定目标工作区 |

同一个 AT 可以按不同目标工作区保存为多条独立路由。对于无法从 AT 解析登录身份的情况，系统至少会按“AT 原文 + 目标工作区”避免同一路由重复写入。

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

账号分组用于把账号池划分为多个可调度集合。API Key 的 `allowed_group_ids` 可以限制下游密钥只能使用指定分组；账号自己的 `allowed_api_key_ids` 也可以反向限制哪些 API Key 能调度该账号。分组还可以通过 `base_concurrency_override` 为成员账号提供基础并发继承值：账号级覆盖优先，账号属于多个分组时取最小的有效分组值，均未设置时回退到全局 `max_concurrency`。

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
      "base_concurrency_override": 4,
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
  "sort_order": 0,
  "base_concurrency_override": 4
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
  "sort_order": 10,
  "base_concurrency_override": 2
}
```

`base_concurrency_override` 最小为 `1`，无上限。创建时省略或传 `null` 表示不设置分组覆盖；PATCH 时传 `null` 会清除已有值并恢复继承。该值只决定基础并发，健康档位、用量保护和智能配速仍可能继续下调实际并发。

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
  "test_content": "hi",
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
  "max_retries": 2,
  "max_rate_limit_retries": 1,
  "retry_interval_ms": 0,
  "transport_retry_policy": "rotate",
  "continuous_retry_enabled": false,
  "continuous_retry_catch_all": false,
  "continuous_retry_categories": ["transport", "http_429", "http_5xx", "stream_error"],
  "continuous_retry_status_codes": [],
  "continuous_retry_error_codes": [],
  "continuous_retry_max_duration_seconds": 600,
  "codex_fingerprint_default_mode": "off",
  "scheduler_mode": "round_robin",
  "allow_remote_migration": false,
  "database_driver": "postgres",
  "database_label": "PostgreSQL",
  "cache_driver": "redis",
  "cache_label": "Redis",
  "response_cache_local_max_bytes": 67108864,
  "response_cache_local_max_entry_bytes": 8388608,
  "response_cache_reconstruct_max_bytes": 67108864,
  "response_cache_config_generation": 1,
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
  "test_content": "say pong",
  "test_concurrency": 50,
  "proxy_url": "http://proxy.example.com:8080",
  "auto_clean_unauthorized": true,
  "auto_clean_rate_limited": false,
  "fast_scheduler_enabled": true,
  "scheduler_mode": "remaining_quota",
  "max_rate_limit_retries": 2,
  "retry_interval_ms": 500,
  "transport_retry_policy": "sticky",
  "codex_fingerprint_default_mode": "session",
  "response_cache_local_max_bytes": 134217728,
  "response_cache_local_max_entry_bytes": 8388608,
  "response_cache_reconstruct_max_bytes": 134217728
}
```

**响应:** 更新后的完整设置对象

`codex_fingerprint_default_mode`（`off`/`device`/`session`/`full`，默认 `off`）是新导入或新建 Codex 账号默认盖上的设备指纹收敛档位，只影响之后新加入的账号；已有账号档位不变，入库后仍可在账号级单独调整。非法取值返回 HTTP 400。

`max_retries`、`max_rate_limit_retries` 与 `codex_ws_silent_max_retries` 是原有的有限重试预算，管理界面和 API 范围均为 `0` 到 `10`（`0` 禁用对应预算）。需要持续重试时使用独立的 `continuous_retry_enabled` 开关；它不会改变这些有限预算的含义。开关打开后，可在 `continuous_retry_categories` 选择 `transport`、`http_429`、`http_4xx`、`http_5xx`、`stream_error`、`response_failed`、`context_error`，并用 `continuous_retry_status_codes`（例如 `[403,404,501]`）或 `continuous_retry_error_codes`（例如 `["rate_limited","context_length_exceeded"]`）精确追加匹配。类别、状态码、错误代码任一命中即可进入持续重试；403、404、上下文错误及“全部 `response.failed`”默认不选中（501 已由默认的 `http_5xx` 类别覆盖）。普通自选模式不会把结构化安全策略拒绝升级为无限重试。永久额度/余额错误（如 `insufficient_quota`、`quota_exceeded`、`billing_hard_limit`、`billing_limit_reached`、`spend_limit`、`credit_balance`、`insufficient_balance`、`usage_limited`）不会因为通用类别进入无限循环；只有管理员明确选择对应额度错误代码，或明确启用下述超级开关时，才会进入持续重试。

`continuous_retry_catch_all` 是默认关闭的超级开关。请求同时提交 `continuous_retry_enabled=true` 与 `continuous_retry_catch_all=true` 后，除明确的上游 `cyber_policy` 外，所有被代理识别为真实上游失败的 HTTP 状态、传输/读取失败、`error` 帧、`response.failed` 以及以后出现的未知失败都会进入持续重试，不依赖当前已知错误码清单；永久额度、余额、鉴权、无效请求和其他结构化安全策略错误也包含在内。明确的上游 `cyber_policy` 始终终止当前请求，不换号、不重放。文本推理请求只把上游 HTTP `200` 及协议正常终态视为可提交结果；201、202、204、重定向、`response.failed`、独立 `error` 帧及无终态 EOF 都会丢弃整次尝试并继续重试。无状态请求会轮换可用账号；持有账号绑定加密状态的请求只有在能够安全展开为自包含请求时才换号，否则沿用相应协议的绑定语义。`continuous_retry_enabled=false` 会规范化为 `continuous_retry_catch_all=false`。

开启持续重试后，流式上游尝试会先完整暂存，只有正常终态才一次性回放给客户端。因此失败尝试的半截文本、工具调用和错误帧不会泄漏，但客户端也不再实时逐 token 收到该次结果。SSE 路径在退避、等待账号、等待响应头和读取暂存流时写注释心跳并 flush；Responses WebSocket 使用 Ping。非流式 JSON（包括 Grok 图片/视频创建）在已进入无限重试后，会在退避、等待账号、等待响应头和读取响应体时发送标准 HTTP `102 Processing` 信息响应；它不会提交最终 JSON 的状态码或响应体，但中间代理可能丢弃 1xx，因此仍需依赖墙钟上限和客户端超时。`retry_interval_ms` 是本地等待下限；实际等待取该值、带抖动的指数退避（250ms 起步、30 秒封顶）和有效 `Retry-After`（最多 5 分钟）中的较大值。

`continuous_retry_max_duration_seconds`（默认 600，范围 1-900）是无限预算的墙钟上限，从请求第一次进入 `retryLimit=-1` 时开始，后续尝试不会重置。到期会取消正在进行的上游请求，并返回最近一次真实上游失败；已提交的 SSE 将该失败转换为协议错误事件，Responses WebSocket 使用错误帧并以 1013 关闭。若尚无上游失败可返回，才回退到 504 `upstream_timeout`。Grok 图片/视频创建保持非流式 JSON；无限重试期间使用 HTTP `102 Processing` 保活，不把 SSE 注释混入最终 JSON。该信息响应是 best-effort 的，中间代理仍可能过滤它。

每次流式尝试最多暂存 64 MiB：前 8 MiB 保存在内存，超过后写入立即 unlink 的 mode-0600 临时文件。达到单次上限或本地存储失败会立即结束请求，不会再次调用上游；当前没有跨请求的进程级暂存总预算，高并发部署仍需自行限制并发并监控内存和临时磁盘。Responses HTTP 的 `X-Codex-Turn-State` 只能在心跳尚未提交响应头时从最终成功账号转发；若等待期间已经发出 SSE 心跳，该响应头会被省略，不能用失败账号的值替代。账号绑定的 continuation 在无法安全展开为自包含请求时也会保留原账号语义。

只有真实上游失败能够触发持续重试。客户端取消、持续重试期限到达、下游写入失败、入口校验、账号池/并发调度、本地提示词或输出策略拒绝、暂存资源失败以及成功结果回放失败都会立即结束，不会伪装成上游错误继续消耗账号。期限会保证等待中的 API Key 与 scope 并发槽位最终释放。

普通图片请求仍受 5 次总尝试上限约束，普通 Grok 图片/视频创建请求仍受 3 次总尝试上限约束；错误被持续重试策略选中后（含超级模式）会越过普通上限，直到成功、客户端取消或期限到达。由于上游未必提供可靠幂等键，图片/视频创建可能重复生成和重复扣费。Grok 视频状态/内容查询沿用绑定账号与各自的请求语义。任何入口收到明确的上游 `cyber_policy` 都会立即停止，并保留本地审计、signed decision、conversation lock 与已验证用户冷却；超级模式不能覆盖此安全终态。初始账号池为空、模型无任何合格账号、scope/并发预算拒绝等本地调度失败不是“上游返回错误”，仍会返回明确的本地错误。

Responses 上下文缓存字段使用原始字节数：

| 字段 | 类型 | 默认值 | 有效范围 |
| --- | --- | --- | --- |
| `response_cache_local_max_bytes` | integer | 67,108,864（64 MiB） | 8 MiB-4 GiB |
| `response_cache_local_max_entry_bytes` | integer | 8,388,608（8 MiB） | 1-256 MiB，且不能超过本地总量 |
| `response_cache_reconstruct_max_bytes` | integer | 67,108,864（64 MiB） | 8-512 MiB |
| `response_cache_config_generation` | integer | 1 | 只读；任何显式写入，包括 `null`，都返回 HTTP 400 |

PUT 可只提交其中一部分可写预算，服务端会在数据库事务中与当前值合并并校验。管理台使用整数 MiB 输入，并把三个可写字段作为一次原子更新发送。成功修改后 generation 递增；本实例立即应用，其他实例每 5 秒同步。

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
      "created_at": "2024-01-01T12:00:00Z",
      "test_ip": "1.2.3.4",
      "test_location": "United States·California·Los Angeles",
      "test_latency_ms": 150,
      "test_status": "success"
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

删除代理，并清空仍引用该 URL 的账号绑定。提交后立即从当前进程的运行时代理池剔除；若数据库快照重载失败，接口返回 HTTP `500` 和已完成的 `deleted` / `unbound` 数量，但不会把已删除代理重新投入调度。

#### PATCH /api/admin/proxies/:id

更新代理。禁用会立刻从运行时代理池剔除该 URL，但保留账号上的 `proxy_url` 绑定——这些账号在重新启用前不会改走其它代理，也不会直连。修改 URL 时，仍指向旧 URL 的账号绑定会改写为新 URL。

**请求:**

```json
{
  "label": "New Label",
  "enabled": false
}
```

#### POST /api/admin/proxies/batch-delete

批量删除代理，并解绑仍引用这些 URL 的账号。重载失败时的语义与单条删除相同。

**请求:**

```json
{
  "ids": [1, 2, 3]
}
```

#### POST /api/admin/proxies/test

测试代理连通性。传入 `id` 时会持久化 `test_status`；可归因于代理的失败状态（包括代理端 TCP 建立失败/超时、HTTPS/SOCKS 协商失败/超时和 HTTP `407` 代理认证失败）为 `error`，成功复测后恢复为 `success`。调用方取消、已连上代理后的传输错误、SOCKS 代理开始连接目标站点后的不可达或超时、探测服务返回 429/5xx 或无效响应时，响应中的 `conclusive` 为 `false`，原测试状态保持不变。

**请求:**

```json
{
  "url": "http://proxy.example.com:8080",
  "id": 1,
  "lang": "zh-CN"
}
```

`id` 可选；省略时仅测试，不持久化结果。

传入 `id` 时，服务端先读取该 ID 当前保存的 URL，仅在其与请求 URL 一致时发起测试；写入结果时再次进行 ID + 原始 URL 比较，测试期间 URL 被修改会返回 HTTP `409`，避免旧结果覆盖新配置。首尾空白只在拨号时去除，兼容历史数据中的非规范 URL。

**响应:**

```json
{
  "success": true,
  "conclusive": true,
  "ip": "1.2.3.4",
  "country": "United States",
  "region": "California",
  "city": "Los Angeles",
  "isp": "Example ISP",
  "latency_ms": 150,
  "location": "United States·California·Los Angeles"
}
```

#### POST /api/admin/proxies/test-all

由服务端以最多 4 路并发测试指定代理，并通过 SSE 逐项返回进度。请求只传代理 ID，服务端使用数据库中的当前 URL；全部测试结果保存完成后只重载一次运行时代理池。代理池刷新由后台收尾路径执行，不依赖客户端持续读取 SSE。

**请求:**

```json
{
  "ids": [1, 2, 3],
  "lang": "zh-CN"
}
```

`ids` 必填、必须为正整数，单次最多 100 个；空数组、未知请求字段、超限请求均返回 HTTP `400`。管理后台在代理超过 100 个时会自动拆成多个顺序批次。同一服务实例同一时间只运行一个批量代理测试，已有任务运行时返回 HTTP `409`。SSE `progress` 事件示例：

```text
data: {"type":"progress","proxy_id":1,"current":1,"total":3,"success":1,"result":{"success":true,"conclusive":true,"ip":"1.2.3.4","latency_ms":150}}
```

流结束时发送 `complete` 事件；如果数据库结果已经保存但运行时代理池刷新失败，事件的 `error` 字段会说明该异常。客户端断开会取消尚未完成的探测；已经落库的结果仍会触发一次代理池刷新。

#### POST /api/admin/proxies/clean-error

固定本次操作开始时 `test_status=error` 的代理集合，删除这些代理，并清空实际引用它们的账号绑定。清理期间新变为 `error` 的代理留到下一次清理。提交后会立即从当前进程的运行时代理池剔除实际删除的 URL；若数据库快照重载失败，接口返回 HTTP `500` 和已完成的 `cleaned` / `unbound` 数量，但不会把已删除代理重新投入调度。

**响应:**

```json
{
  "message": "已清理 2 个错误代理并解绑 3 个账号",
  "cleaned": 2,
  "unbound": 3
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
    "total_bytes": 10737418240,
    "process_bytes": 268435456,
    "heap_alloc_bytes": 100663296,
    "heap_inuse_bytes": 117440512,
    "heap_released_bytes": 33554432,
    "num_gc": 421
  },
  "response_cache": {
    "effective_config": {
      "generation": 3,
      "local_max_bytes": 67108864,
      "local_max_entry_bytes": 8388608,
      "reconstruct_max_bytes": 67108864
    },
    "applied_config": {
      "generation": 3,
      "local_max_bytes": 67108864,
      "local_max_entry_bytes": 8388608,
      "reconstruct_max_bytes": 67108864
    },
    "entries": 120,
    "max_entries": 2000,
    "current_bytes": 33554432,
    "max_bytes": 67108864,
    "high_water_bytes": 50331648,
    "largest_entry_bytes": 7340032,
    "local_hits": 920,
    "local_misses": 80,
    "remote_hits": 60,
    "remote_misses": 20,
    "expirations": 12,
    "count_evictions": 2,
    "byte_evictions": 8,
    "oversize_bypasses": 4,
    "oversize_rejections": 0,
    "known_unavailable_errors": 1,
    "last_config_sync_at": "2024-01-01T11:59:58Z",
    "last_config_sync_error": ""
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

`response_cache.current_bytes`、`max_bytes`、`high_water_bytes` 和 `largest_entry_bytes` 都是 JSON payload 的逻辑字节，不是 RSS。`local_*` 统计 L1 查找；`remote_*` 统计共享后端的有效命中和明确未命中；`oversize_bypasses` 表示共享后端命中可服务但未提升到 L1；`oversize_rejections` 只统计 Memory 模式因字节预算无法保留的写入；`known_unavailable_errors` 只统计最终发出的上下文不可用 HTTP 409。

`memory.process_bytes` 在 Linux/Docker 使用进程 RSS，非 Linux 无法读取 `/proc` 时回退到 Go `Sys`；`heap_alloc_bytes`、`heap_inuse_bytes`、`heap_released_bytes` 和 `num_gc` 来自同一次 Go runtime 采样。缓存逻辑预算不包含 Go 对象或 allocator 开销，不能当作进程内存硬上限。

滚动升级时，旧后端响应可能不包含 `response_cache` 或新的 heap/GC 字段；新版管理前端会显示兼容等待状态或隐藏缺失的 heap 行。

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
| 409    | 资源冲突或上一响应上下文不可用 |
| 429    | 请求过于频繁（限流）     |
| 499    | 客户端断开连接           |
| 500    | 服务器内部错误           |
| 502    | 网关错误（上游服务异常） |
| 503    | 服务不可用（账号池耗尽或依赖的共享上下文后端暂时故障） |
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
| response_context_unavailable     | `previous_response_id` 所需上下文不可用 | 重新发送完整上下文或开始新的响应链 |
| service_unavailable              | 依赖的共享上下文后端暂时不可用 | 退避后重试并检查 Redis 状态 |

### Responses 上下文不可用

在没有可用 relay fallback 时，以下情况会返回 HTTP 409：

- Memory 模式命中已知超限/淘汰，或依赖的必需上下文普通缺失/已经过期。
- Redis 模式读取到损坏的值，或逻辑上下文超过重建上限。

```json
{
  "error": {
    "code": "response_context_unavailable",
    "message": "Previous response context is unavailable",
    "type": "invalid_request_error",
    "details": {
      "field": "previous_response_id",
      "message": "local_context_evicted"
    }
  }
}
```

同一 `previous_response_id` 已确定不可用时，不要无条件重试；应重新发送完整必需上下文、开始新的响应链，或为后续请求调整缓存预算。若只是共享后端传输故障，依赖上下文的请求返回 HTTP 503、错误码为 `service_unavailable`，适合退避后重试并检查 Redis。

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
