# 使用指南

管理后台的 `/admin/docs` 提供客户端配置、搜索、参数说明和可复制示例。内容分为快速接入、开发指南、接口参考、故障排查、管理与进阶；章节 URL 可以直接收藏。页面的「复制为 Markdown」包含同一份指南和接口数据，密钥使用占位符；客户端配置区的复制按钮则使用当前选中的真实密钥。

## 完成第一次接入

1. 在账号管理中启用至少一个具有目标模型能力的账号。
2. 在 API 密钥中创建客户端专用 Key，核对渠道、模型和分组范围。
3. 在使用文档的快速接入中选择 Codex CLI、Claude Code、CC Switch 或 Cherry Studio，配置服务地址和模型。思考强度与服务档位位于高级选项。
4. 按提示写入配置文件或导入客户端，再运行页面提供的 cURL。
5. 在使用统计中核对请求状态、最终模型、耗时和 token。

`GET /health` 成功只代表服务在线。`GET /v1/models` 可验证地址和认证；完整生成请求才验证模型、账号和上游链路。

## 地址与密钥

假设服务地址为 `https://gateway.example.com`：

| 用途 | 地址或凭据 |
| --- | --- |
| Codex CLI | 本网关兼容 `https://gateway.example.com` 下无 `/v1` 的 Responses 路径 |
| OpenAI SDK | `base_url` / `baseURL` 为 `https://gateway.example.com/v1` |
| Claude Code | `ANTHROPIC_BASE_URL=https://gateway.example.com` |
| 普通 API | `Authorization: Bearer YOUR_API_KEY`；Messages 也接受 `x-api-key` |
| 管理 API | `X-Admin-Key: YOUR_ADMIN_SECRET`，不需要叠加下游 API Key |
| 出站代理 | 账号 `proxy_url`，用于网关访问上游，与客户端 Base URL 分开配置 |

公共 API 默认要求认证。普通公共接口仅在未配置任何 API Key 且显式启用 `CODEX_ALLOW_ANONYMOUS=true` 时允许匿名访问；异步图片任务始终要求后台创建的 API Key。

Claude 上游支持 OAuth、Setup Token 和 API Key + Base URL。后者不走 OAuth 刷新或订阅用量采样；原生 Messages 与 Codex 转换回退取决于密钥渠道、账号和路由配置。详见 [Claude 凭据](API.md#claude-凭据与原生-messages)。

## 开发指南

- `/admin/docs#sdk-examples`：Python 与 Node.js SDK 的最小请求。SDK 密钥从服务端环境变量读取。
- `/admin/docs#streaming`：`stream=false` 返回 JSON；`stream=true` 返回 SSE。cURL 使用 `-N`，客户端检查终止事件，不能将连接中断当成完成。
- `/admin/docs#conversation-tools`：保存原始 input、全部 response.output 和工具结果；使用 `call_id` 配对工具调用与输出。
- `/admin/docs#structured-output`：Responses 使用 `text.format`，Chat 使用 `response_format`；支持的约束取决于最终模型、渠道和传输方式。
- `/admin/docs#protocols`：协议、模型映射、思考强度与服务档位说明。

`previous_response_id` 依赖同一密钥范围内可恢复的上下文。缓存受容量、TTL 和写入策略限制；Memory 缓存配合 SQLite 文件主库时默认有磁盘持久层，关闭后重启会丢失。遇到 `409 previous_response_not_found` 时，重发完整必需历史并开始新链。详见 [Responses 上下文](API.md#responses-上下文不可用)。

SDK 示例的基础调用与流式事件按 [OpenAI SDK 文档](https://developers.openai.com/api/docs/libraries) 和 [流式响应文档](https://developers.openai.com/api/docs/guides/streaming-responses) 编写；模型名、路由及兼容性边界以本网关为准。

## 图片、视频与进阶接口

| 能力 | 入口 | 使用方式 |
| --- | --- | --- |
| 同步生图 / 编辑 | `/v1/images/generations`、`/v1/images/edits` | 返回图片；编辑支持 JSON 和 multipart，遮罩仅 gpt-image 系列支持 |
| 异步生图 / 编辑 | `POST /v1/images/jobs`、`GET /v1/images/jobs/:id` | 返回 `202 {job: ...}`；同一 API Key 查询 `job.id`，成功后使用 `job.assets[].proxy_url` 下载图片 |
| 视频生成 / 编辑 / 延展 | `/v1/videos/generations`、`/edits`、`/extensions` | 需要可用付费 Grok 账号；创建后轮询 request_id 并下载 `/content` |
| 本地 Token 估算 | `/v1/messages/count_tokens`、`/v1/responses/input_tokens` | 不调用上游，用于兼容探测，不作为准确 tokenizer 或计费依据 |
| 上下文压缩 | `POST /v1/responses/compact` | 提交完整历史，保留返回的压缩项和不透明字段 |
| Responses WebSocket | `GET /v1/responses` | 通过认证头建立连接后发送 `response.create`；同 `stream_id` 保序、不同独立流并行（最多 16 个活动轮次、32 个命名流）；支持向当前 WebSocket 上游租约发送 `response.interrupt`；`response.append` 与 `response.steer` 返回 `unsupported_feature`，可用下一轮 `response.create` + `previous_response_id` |

GPT Image 2.5 的 Flare / Sunburst 支持 `auto/low/medium/high/xhigh/max`；省略模型仍默认 `gpt-image-2`。`-2k/-4k` 是网关尺寸档位别名。直接通过 Responses 生图时，顶层 `model` 是文本驱动，`tools[].model` 是图片模型。完整参数见 [Images](API.md#3-images)。

## 错误与额度

| 现象 | 下一步 |
| --- | --- |
| 401 / 403 | 检查凭据、有效期、密钥渠道、模型和分组授权 |
| 429 `rate_limit_reached` | 检查模型周预算的 `error.details` 与 `Retry-After` |
| 503 `no_available_account` | 检查账号启用状态、冷却、并发及可用模型 |
| 503 `account_pool_concurrency_saturated` | 账号在，但并发窗口已满；稍后重试或提高并发上限 |
| 503 `account_pool_usage_limit_reached` | 账号池额度耗尽；根据 `Retry-After` 等待重置或补充账号 |
| 409 `previous_response_not_found` | 重发完整上下文，开始新响应链 |
| 503 `service_unavailable` | 检查共享上下文或预算存储，退避后重试 |
| 404 | 核对是否重复拼接 `/v1`、服务器版本和完整路径；任务查询还要检查所属密钥及有效期 |

金额额度、模型周次数和上游账号额度是独立限制。周次数按最终模型匹配，可指定时区和重置时间。详见 [模型周预算](CONFIGURATION.md#api-key-模型周请求次数预算)。

查看更完整的 [API 参考](API.md)、[配置说明](CONFIGURATION.md)、[部署文档](DEPLOYMENT.md) 和 [故障排查](TROUBLESHOOTING.md)。
