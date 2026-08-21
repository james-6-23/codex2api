# Codex2API 配置说明

本文档详细说明 Codex2API 的所有配置项及其作用。

## 目录

- [配置层级](#配置层级)
- [环境变量配置](#环境变量配置)
- [系统设置（数据库）](#系统设置数据库)
- [配置文件示例](#配置文件示例)
- [配置优先级](#配置优先级)

---

## 配置层级

Codex2API 采用三层配置架构：

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 1: 环境变量 / .env 文件                               │
│  - 数据库连接、端口、基础认证                                 │
│  - 物理层基础设施配置                                        │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│  Layer 2: 系统设置（数据库 SystemSettings 表）               │
│  - 业务参数：并发、限流、测试配置                             │
│  - 运行时可通过管理后台修改                                  │
└─────────────────────────────────────────────────────────────┘
│  Layer 3: 运行时内存状态                                     │
│  - 账号池状态、调度评分、冷却状态                             │
│  - 程序重启后从数据库恢复                                    │
└─────────────────────────────────────────────────────────────┘
```

---

## 环境变量配置

### 核心服务配置

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `CODEX_PORT` | 否 | 8080 | HTTP 服务端口 |
| `BIND_HOST` | 否 | `127.0.0.1`（SQLite）/ `0.0.0.0`（PostgreSQL） | Docker 端口发布绑定地址（非进程监听地址，由 `CODEX_BIND` 控制）。SQLite compose 默认 `127.0.0.1` 仅本机访问；标准 compose 默认 `0.0.0.0` 所有网络接口 |
| `CODEX_MAX_REQUEST_BODY_SIZE_MB` | 否 | 48 | HTTP 请求体上限。后台 MP4 动态壁纸上传最大 40MB，默认值为 multipart 上传预留余量 |
| `ADMIN_SECRET` | 否 | - | 管理后台登录密钥 |
| `CODEX_ALLOW_ANONYMOUS` | 否 | `false` | 设为 `true` 时，未配置任何对外 API Key 也允许 `/v1/*` 直接调用（仅限内网测试场景） |
| `FAST_SCHEDULER_ENABLED` | 否 | `false` | 通过环境变量启用快速调度器（也可在管理后台运行时开启） |
| `TZ` | 否 | UTC | 时区，如 `Asia/Shanghai` |

### Codex 上游稳定性配置

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `CODEX_UPSTREAM_TRANSPORT` | 否 | `http` | Codex 上游协议：`http` / `auto` / `ws`。HTTP 入站在 `auto` 下仍走 HTTP 上游 |
| `CODEX_PROXY_URL` | 否 | - | 全局代理 URL，适用于需要为所有 Codex 上游请求统一配置代理的场景 |
| `USE_WEBSOCKET` | 否 | `false` | 旧版开关；未设置 `CODEX_UPSTREAM_TRANSPORT` 时，`true` 等价于 `CODEX_UPSTREAM_TRANSPORT=ws` |
| `CODEX_TRANSPORT_MODE` | 否 | `standard` | Codex HTTP transport：默认标准 Go TLS；`utls_chrome` 可回滚旧 Chrome uTLS 行为 |
| `CODEX_WS_SEND_USER_AGENT` | 否 | `true` | WS 握手是否发送 Codex `User-Agent`/`Version`；设为 `false` 可关闭 |
| `CODEX_SESSION_AFFINITY_TTL` | 否 | `1h` | Codex 会话到账号/代理的黏性 TTL，支持 `1h`、`90m` 或秒数 |
| `CODEX_COMPACTION_AFFINITY_TTL` | 否 | `168h` | 加密压缩状态的来源亲和 TTL。缓存仅保存密文的 SHA-256 摘要、来源账号和兼容域；已知状态不会跨 Codex 官方、不同 Responses 中转或 Grok 上游流转 |
| `CODEX_FINGERPRINT_DEBUG` | 否 | `false` | 输出脱敏指纹策略诊断日志，不记录 token |

> `CODEX_UPSTREAM_TRANSPORT` 只控制 HTTP 入站请求转发到 Codex 上游时使用 `http` 还是 `ws`。客户端侧 WebSocket 入口独立可用：使用 `GET ws://<host>/v1/responses` 建连，首帧发送 `response.create` JSON，服务端会通过 Codex 上游 WS 返回 Responses 事件帧。

### 数据库配置

#### PostgreSQL 模式

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `DATABASE_DRIVER` | 是 | postgres | 固定值: postgres |
| `DATABASE_HOST` | 是 | - | PostgreSQL 主机地址 |
| `DATABASE_PORT` | 否 | 5432 | PostgreSQL 端口 |
| `DATABASE_USER` | 是 | - | PostgreSQL 用户名 |
| `DATABASE_PASSWORD` | 是 | - | PostgreSQL 密码 |
| `DATABASE_NAME` | 是 | - | PostgreSQL 数据库名 |
| `DATABASE_SCHEMA` | 否 | - | PostgreSQL schema；适合 Supabase 等多项目共享 database 的场景。配置后启动时自动 `CREATE SCHEMA IF NOT EXISTS` 并将所有连接的 `search_path` 指向该 schema。仅允许字母/数字/下划线，长度 ≤63；留空保持默认（通常是 `public`）。|
| `DATABASE_SSLMODE` | 否 | disable | SSL 模式: disable/require/verify-full |

### 生图工作台

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `IMAGE_ASSET_DIR` | 否 | `/data/images` | 管理台生图工作台保存图片文件的服务器目录；Docker 部署建议持久化 `/data` |
| `IMAGE_ASSET_PUBLIC_BASE_URL` | 否 | 空 | 图片代理 URL 的公开基址，例如 `https://cdn.example.com`；仅改变返回地址，需由反向代理将 `/p/img/` 转发到 Codex2Api |
| `IMAGE_ASSET_SIGNING_SECRET` | 否 | 随机值 | 图片代理 URL 的持久化签名密钥；生产环境应配置固定随机值，避免服务重启后历史图片链接失效 |
| `IMAGE_UPSCALER_ENDPOINT` | 否 | 空 | RealESRGAN 服务地址，例如 `http://image-upscaler:8090`；配置后 `upscale=2k/4k` 必须由该服务成功处理，否则异步任务失败 |
| `IMAGE_UPSCALER_FIT` | 否 | `inside` | RealESRGAN 目标尺寸适配方式，可选 `inside` 或 `cover` |
| `BACKGROUND_ASSET_DIR` | 否 | `/data/backgrounds` | 管理台背景图/MP4 上传文件的服务器目录；未配置时优先保存到 `IMAGE_ASSET_DIR` 同级的 `backgrounds` 目录 |

### 日志目录

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `LOG_DIR` | 否 | `logs` | 上游错误日志目录；只允许写临时盘的平台可设为 `/tmp/logs` |
| `LOG_DISABLED` | 否 | `false` | 设为 `true` 时禁用文件型错误日志与安全审计日志 |
| `SECURITY_LOG_DIR` | 否 | `${LOG_DIR}/security` | 安全审计日志目录；未设置时跟随 `LOG_DIR` |

#### SQLite 模式

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `DATABASE_DRIVER` | 是 | sqlite | 固定值: sqlite |
| `DATABASE_PATH` | 是 | - | SQLite 数据库文件路径，如 `/data/codex2api.db` |

### 缓存配置

#### Redis 模式

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `CACHE_DRIVER` | 是 | redis | 固定值: redis |
| `REDIS_ADDR` | 是 | - | Redis 地址，支持 `redis:6379`、`redis://default:pass@host:6379/0`、`rediss://default:pass@host:6379/0` |
| `REDIS_USERNAME` | 否 | - | Redis ACL 用户名；URL 中已包含用户名时可不填 |
| `REDIS_PASSWORD` | 否 | - | Redis 密码；URL 中已包含密码时可不填 |
| `REDIS_DB` | 否 | 0 | Redis 数据库编号 |
| `REDIS_TLS` | 否 | false | 为 `host:port` 形式的 Redis 启用 TLS；`rediss://` 会自动启用 |
| `REDIS_INSECURE_SKIP_VERIFY` | 否 | false | 跳过 TLS 证书校验，仅建议自签证书或排障时使用 |

> Aiven、Upstash 等云 Redis 通常要求 TLS。优先使用平台提供的 `rediss://...` 连接串；如果只填写 `host:port`，请设置 `REDIS_TLS=true`，否则可能在启动时出现 `Redis 连接失败: EOF`。

#### 内存缓存模式

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `CACHE_DRIVER` | 是 | memory | 固定值: memory |

---

## 系统设置（数据库）

系统设置存储在数据库的 `SystemSettings` 表中，可通过管理后台 `/admin/settings` 实时修改。

### Responses 上下文缓存

Responses 连续请求会按 `previous_response_id` 重建上下文。每个 Codex2API 进程都有一层有界 L1 缓存，三个字节预算保存在数据库中；管理台用整数 MiB 展示和修改，管理 API 使用原始字节数。

| 管理 API 字段 | 默认值 | 设置页范围 | 说明 |
|------|------|------|------|
| `response_cache_local_max_bytes` | 67,108,864 bytes（64 MiB） | 整数 8-4096 MiB | 单个进程 L1 可保留的逻辑 JSON payload 总量 |
| `response_cache_local_max_entry_bytes` | 8,388,608 bytes（8 MiB） | 整数 1-256 MiB | 单条上下文进入 L1 的上限，且不能超过本地总量 |
| `response_cache_reconstruct_max_bytes` | 67,108,864 bytes（64 MiB） | 整数 8-512 MiB | 从共享后端读取并重建一条上下文时允许的逻辑 payload 上限 |
| `response_cache_config_generation` | 1 | 只读 | 配置发生实际变化时递增；客户端不能写入 |

设置页会把三个预算作为一个原子更新发送。管理 API 也支持只提交部分预算，服务端会在数据库事务中与当前值合并并校验，提交成功后才应用到本实例。固定边界不随这三个设置变化：最多 2,000 条、10 分钟绝对 TTL、每条最多 200 个 raw item。降低预算会立即收缩本地 L1，并可能淘汰已有上下文。

Redis 模式会把 response context 保存到共享后端。后端值在重建上限内但超过 L1 单条或总量准入预算时，仍可服务当前请求，但不会提升到本地 L1。Memory 模式只保留本进程 L1，不存在第二份共享 response context；已知超限/淘汰，或依赖的必需上下文缺失/过期时，可能导致 HTTP `409 response_context_unavailable`。Redis 值损坏或超过重建上限且无法走 relay 后备时也可能返回 409。共享后端暂时不可用时，依赖该上下文且无法走 relay 后备的请求可能返回 HTTP `503`。

只有预算实际变化时才会分配并递增 generation；同值更新或空更新不会递增。当前实例在数据库提交后立即应用，其他实例每 5 秒轮询一次，只应用更新的 generation；单次读取最多等待 3 秒。同步失败时保留最后一次有效配置，并在运维页显示错误，后续轮询成功后自动恢复。

这些预算只控制本地重建的 HTTP Responses/Compact 上下文。客户端原生 Responses WebSocket 入口不查询本地 response cache，会保留 `previous_response_id` 交给上游处理。

这里的“字节”是保留 `json.RawMessage` 长度之和，不包含 map、切片、LRU、Go 堆或容器开销，因此不是 RSS 或进程内存硬上限。滚动升级时，新前端对旧后端缺失的设置使用 64/8/64 MiB 展示默认值、generation `0`；旧后端缺少 response-cache 运维对象时，前端显示兼容等待状态而不会崩溃。

### 调度配置

| 字段 | 类型 | 默认值 | 范围 | 说明 |
|------|------|--------|------|------|
| `MaxConcurrency` | int | 2 | ≥1（无上限） | 单账号最大并发请求数 |
| `GlobalRPM` | int | 0 | 0-∞ | 全局每分钟请求限制，0 表示不限 |
| `MaxRetries` | int | 2 | 0-10 | 原有有限重试预算，覆盖传输错误及既有可重试的 5xx（含 500/502/503/504）；`0` 禁用该预算 |
| `MaxRateLimitRetries` | int | 1 | 0-10 | 原有有限的 429 独立重试预算；`0` 禁用该预算 |
| `RetryIntervalMS` | int | 0 | 0-30000 | 普通重试前等待的毫秒数；`0` 保持立即重试 |
| `TransportRetryPolicy` | string | `rotate` | `rotate` / `sticky` | 传输错误重试时换号，或保留同一账号重试 |
| `FastSchedulerEnabled` | bool | false | - | 启用快速调度器 |
| `CodexForceWebsocket` | bool | false | - | 强制 Codex 上游走 WebSocket 长连接（复用连接池），更接近官方 CLI 体验；关闭时走原有 HTTP 请求 |
| `CodexWSKeepaliveEnabled` | bool | false | - | 启用上游 WS 空闲连接保活（后台仅发 Ping，不发起新请求、不消耗账号额度） |
| `CodexWSKeepaliveIntervalSec` | int | 60 | 10-600 | WS 保活 Ping 间隔（秒），仅在 `CodexWSKeepaliveEnabled` 开启时生效 |
| `CodexWSHideUpstreamErrors` | bool | true | - | WS 上游最终失败时向客户端隐藏原始错误，返回统一友好提示；原始错误仍记录在后台日志/用量记录 |
| `CodexWSSilentRetryEnabled` | bool | true | - | WS 首包前遇到限流、额度耗尽、5xx、读取错误或超时时，静默换账号并重建上游 WS |
| `CodexWSSilentMaxRetries` | int | 2 | 0-10 | WS 首包前静默重试上限；`0` 禁用该预算 |
| `SchedulerMode` | string | `round_robin` | - | 调度模式：`round_robin`（轮询，按调度分权重排序）、`remaining_quota`（优先使用用量少的账号）或 `fill_first`（顺序耗尽：集中使用剩余额度最少的账号，耗尽/限流后切下一个） |
| `AffinityMode` | string | `bounded` | - | 会话亲和：`bounded`（50 次、5 分钟或账号不健康时重新挑号）、`off`（每次重选）、`strict`（长期粘连） |

调度优先级先决定账号层级，同一优先级内再比较健康档位、调度分和当前负载；会话亲和只负责复用已绑定账号。多个最终用户共享同一个 API Key 时，下游可传 `X-Codex2API-Affinity-Key`，值会先哈希且仅用于本地账号绑定，不会转发给上游。

`ContinuousRetryPolicy` 是默认关闭的独立持续重试策略，持久化为 JSON：

```json
{
  "enabled": false,
  "catch_all": false,
  "categories": ["transport", "http_429", "http_5xx", "stream_error"],
  "status_codes": [],
  "error_codes": []
}
```

启用后，类别、精确 HTTP 状态码或精确上游错误代码任一命中即可持续重试；`http_4xx`、403、404、上下文错误以及“全部 `response.failed`”等宽泛或确定性故障需要管理员显式选择，501 则已由默认的 `http_5xx` 类别覆盖。普通自选模式不会把未选中的 `invalid_request` 或结构化安全策略拒绝自动升级为无限重试。

`catch_all` 是默认关闭的超级模式。开启后不再依赖已知类别或错误码清单；除明确的上游 `cyber_policy` 外，任何真实上游 HTTP、传输、流读取、`error`、`response.failed` 或未知失败都会进入持续重试，包括永久额度、余额、鉴权、无效请求和其他结构化安全策略错误。明确的上游 `cyber_policy` 始终终止当前请求，不换号、不重放。文本推理只接受上游 HTTP `200` 及协议正常终态；其他状态、失败终态及无终态 EOF 都会丢弃整次尝试并继续。管理界面的超级开关会在一次保存中同时设置 `enabled=true` 和 `catch_all=true`；关闭总开关会同步清除 `catch_all`，避免隐藏启用。

持续重试会把每次流式上游尝试完整暂存；失败整次丢弃，正常终态才一次性回放。因此客户端等待时由 SSE 注释或 Responses WebSocket Ping 保活，但不再实时逐 token 收到生成结果。非流式 JSON（包括 Grok media）在进入无限重试后，会在退避、等待账号、等待响应头和读取响应体时发送标准 HTTP `102 Processing` 信息响应；它不会提交最终 JSON 状态，但中间代理可能丢弃 1xx，仍需依赖墙钟上限和客户端超时。`max_duration_seconds` 设置无限预算的墙钟时间上限（默认 600 秒，范围 1 到 900 秒），从请求第一次进入无限重试时开始，后续尝试不会重置。期限到达会立即取消上游并返回最近一次真实上游失败；仅在尚无失败可返回时使用 `504 upstream_timeout`。普通自选模式不会无限重试未选中的结构化安全策略拒绝；`catch_all` 可覆盖其他拒绝，但不能覆盖明确的上游 `cyber_policy` 或本地重试期限。

单次流式尝试的暂存上限为 64 MiB，前 8 MiB 使用内存，之后写入立即 unlink 的 mode-0600 临时文件；暂存超限或存储失败会作为本地错误立即停止。当前没有跨请求的进程级暂存总预算，高并发环境需要另行限制并发并监控内存与临时磁盘。Responses HTTP 等待期间若 SSE 心跳已提交响应头，最终成功账号的 `X-Codex-Turn-State` 无法再补发，因此实现会省略该头而不会转发失败账号的状态；无法安全展开为自包含请求的账号绑定 continuation 也不会强行换号。

客户端取消、下游写失败、WebSocket 断开、入口校验、账号池/并发调度、本地提示词或输出策略拒绝、暂存资源失败和成功回放失败都立即结束，绝不作为上游错误继续轮换。普通图片请求仍以 5 次为上限，普通 Grok 图片/视频创建仍以 3 次为上限；一旦错误被持续策略选中（含 `catch_all`），就会越过普通上限，直到成功或客户端取消。图片/视频创建可能重复生成和重复扣费，因为上游未必支持可靠幂等键。超级模式还可能持续消耗 token、请求次数、余额、账号配额、暂存内存与磁盘，并长期占用 API Key 与 scope 并发槽位、阻塞较新的请求，必须显式承担风险后开启。

明确的上游 `cyber_policy` 是不可覆盖的安全终态：普通自选与超级模式都会立即停止当前请求，保留本地审计与 signed decision，并按现有策略写入 conversation lock 和已验证用户冷却。它不会作为中间失败被透明吞掉，也不会轮换其他账号重放同一请求。

持续重试等待取 `retry_interval_ms`、带抖动的指数退避（250ms 起步，30 秒封顶）以及有效 `Retry-After`（最多 5 分钟）中的较大值。即使上游持续返回很小的 `Retry-After`，本地退避仍会增长，避免无限模式形成高频请求循环。

### 测试配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `TestModel` | string | "gpt-5.5" | 测试连接使用的模型 |
| `TestContent` | string | "hi" | 测试连接发送给上游的用户输入内容。多行时每次随机抽取一行；支持 `{{time}}`、`{{date}}`、`{{datetime}}`、`{{timestamp}}`、`{{rand}}`、`{{rand:min-max}}` 变量 |
| `TestConcurrency` | int | 50 | 批量测试并发数，范围 1-200 |

### 代理配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `ProxyURL` | string | "" | 全局代理 URL |
| `ProxyPoolEnabled` | bool | false | 启用代理池。开启后未绑定账号从启用代理中粘性分配；绑定到已禁用/测挂托管代理的账号不会直连；池空且无全局代理时拒绝调度 |

### 账号级设置（单账号）

以下字段存储在 `accounts` 表中，可通过管理后台账号详情或 API 按账号单独设置：

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `credit_enabled` | bool | false | 标记账号为信用计费模式 |
| `credit_skip_usage_window` | bool | false | 跳过 7 天/5 小时用量窗口惩罚（适用于信用账号） |
| `score_bias_override` | int/null | null | 手工覆盖调度权重分，`null` 跟随套餐默认 |
| `base_concurrency_override` | int/null | null | 手工覆盖基础并发值（`≥1` 无上限）；`null` 时先继承所属分组的最小有效值，再回退到全局默认 |
| `scheduler_priority` | int/null | null | 严格调度优先级（`-100..100`）；`null` 恢复默认值 `0` |
| `skip_warm_tier` | bool | false | 跳过 warm 层级；仅把 warm 提升为 healthy，不覆盖 risky/banned |

账号列表的批量编辑支持分数偏置、基础并发、调度优先级、标签和分组。勾选某个数值字段但保持输入为空时，会发送 `null`，将该字段重置为继承值或默认值；未勾选的字段保持不变。

### 分组级基础并发

账号分组可设置 `base_concurrency_override`（`≥1`，无上限，`null` 表示不覆盖）。基础并发按“账号显式覆盖 > 所属分组中最小的有效值 > 全局 `max_concurrency`”解析；最终动态并发仍会受健康档位、用量保护和智能配速限制。

### WebSocket 连接池与 1009 降级

- 每个账号的上游物理 WebSocket 连接数受其当前 `DynamicConcurrencyLimit` 限制。
- 新建或复用连接时如果超过新上限，只淘汰最老的空闲连接；当前请求使用的连接和其他活跃连接不会被中断。
- 上游在尚未向下游输出内容时返回 close 1009，或本地读取触发等价的 read-limit 错误，网关会保留同一账号租约和已解析代理，最多降级一次 HTTP。
- 1009 属于传输限制，不降低账号健康度，也不触发鉴权探针；一旦已向下游输出内容，就不会再发起 HTTP 降级，避免重复请求和重复计费。

### 连接池配置

| 字段 | 类型 | 默认值 | 范围 | 说明 |
|------|------|--------|------|------|
| `PgMaxConns` | int | 50 | 5-500 | PostgreSQL 最大连接数 |
| `RedisPoolSize` | int | 30 | 5-500 | Redis 连接池大小 |

### 自动清理配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `AutoCleanUnauthorized` | bool | false | 自动清理 401 账号 |
| `AutoCleanRateLimited` | bool | false | 自动清理 429 账号 |
| `AutoCleanFullUsage` | bool | false | 自动清理满用量账号 |
| `AutoCleanError` | bool | false | 自动清理错误状态账号 |

### 安全设置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `AdminSecret` | string | "" | 管理后台密码（数据库存储） |
| `AllowRemoteMigration` | bool | false | 允许远程迁移（需设置 AdminSecret） |

---

## 配置文件示例

### 标准生产环境 (.env)

```bash
# ============================================================
# Codex2API 生产环境配置
# ============================================================

# 服务配置
CODEX_PORT=8080
ADMIN_SECRET=your-secure-admin-password-here
TZ=Asia/Shanghai

# 数据库配置 (PostgreSQL)
DATABASE_DRIVER=postgres
DATABASE_HOST=postgres
DATABASE_PORT=5432
DATABASE_USER=codex2api
DATABASE_PASSWORD=your-strong-db-password
DATABASE_NAME=codex2api
DATABASE_SSLMODE=disable
IMAGE_ASSET_DIR=/data/images
LOG_DIR=logs
LOG_DISABLED=false

# 缓存配置 (Redis)
CACHE_DRIVER=redis
REDIS_ADDR=redis:6379
REDIS_USERNAME=
REDIS_PASSWORD=your-redis-password
REDIS_DB=0
REDIS_TLS=false
REDIS_INSECURE_SKIP_VERIFY=false
```

### SQLite 轻量环境 (.env)

```bash
# ============================================================
# Codex2API SQLite 轻量版配置
# ============================================================

# 服务配置
CODEX_PORT=8080
ADMIN_SECRET=your-admin-password
TZ=Asia/Shanghai

# 数据库配置 (SQLite)
DATABASE_DRIVER=sqlite
DATABASE_PATH=/data/codex2api.db
IMAGE_ASSET_DIR=/data/images
LOG_DIR=logs
LOG_DISABLED=false

# 缓存配置 (内存)
CACHE_DRIVER=memory
```

### 开发环境 (.env)

```bash
# ============================================================
# Codex2API 开发环境配置
# ============================================================

CODEX_PORT=8080
# ADMIN_SECRET=dev  # 开发环境可不设置

# 本地 PostgreSQL
DATABASE_DRIVER=postgres
DATABASE_HOST=localhost
DATABASE_PORT=5432
DATABASE_USER=codex2api
DATABASE_PASSWORD=codex2api
DATABASE_NAME=codex2api

# 本地 Redis
CACHE_DRIVER=redis
REDIS_ADDR=localhost:6379
REDIS_USERNAME=
REDIS_PASSWORD=
REDIS_DB=0
REDIS_TLS=false

TZ=Asia/Shanghai
```

---

## 配置优先级

当同一配置项存在多个来源时，按以下优先级生效：

```
1. 环境变量（最高优先级）
   ↓
2. .env 文件中的变量
   ↓
3. 数据库 SystemSettings（业务配置）
   ↓
4. 程序默认值（最低优先级）
```

### 特殊规则

**Admin Secret 优先级:**

```
1. 环境变量 ADMIN_SECRET
   ↓
2. 数据库 SystemSettings.AdminSecret
   ↓
3. 空值（无认证）
```

**数据库连接池:**

- `PgMaxConns` 修改后立即生效，无需重启
- `RedisPoolSize` 修改后需重启生效

**调度参数:**

- `MaxConcurrency`、`GlobalRPM` 等修改后立即生效
- 通过管理后台修改时会自动持久化到数据库

---

## 配置验证

### 启动时验证

程序启动时会自动验证配置：

```
✓ 数据库连接成功: PostgreSQL
✓ 缓存连接成功: Redis
✓ 账号池初始化完成: 10/10 可用
✓ 系统设置加载完成
✓ HTTP 服务启动: http://0.0.0.0:8080
```

### 配置检查 API

```bash
# 健康检查
curl http://localhost:8080/health

# 系统概览（需 Admin Secret）
curl -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/ops/overview
```

---

## 常见问题

### Q: 修改环境变量后需要重启吗？

**A:** 是的，环境变量在程序启动时加载，修改后需要重启容器才能生效。

### Q: 如何在不重启的情况下修改配置？

**A:** 通过管理后台 `/admin/settings` 修改的业务配置（如 MaxConcurrency、GlobalRPM）会立即生效。

### Q: SQLite 和 PostgreSQL 可以切换吗？

**A:** 可以，但需要：
1. 停止服务
2. 修改 DATABASE_DRIVER 和相关配置
3. 启动服务（新数据库会重新初始化）
4. 重新导入账号数据

### Q: 如何查看当前生效的配置？

**A:** 通过管理后台 `/admin/settings` 页面可查看系统设置及配置来源（env/database）。Responses 上下文缓存的本实例 effective/applied generation、最近同步时间和同步错误可在 `/admin/ops` 查看。

### Q: 配置错误导致无法启动怎么办？

**A:** 检查日志输出，常见错误：
- `DATABASE_HOST is empty` - 未配置数据库主机
- `REDIS_ADDR is empty` - Redis 模式下未配置 Redis 地址
- `DATABASE_PATH is empty` - SQLite 模式下未配置数据路径
