# Codex2API 配置说明

本文档详细说明 Codex2API 的所有配置项及其作用。

## 目录

- [配置层级](#配置层级)
- [环境变量配置](#环境变量配置)
- [系统设置（数据库）](#系统设置数据库)
- [API Key 模型周请求次数预算](#api-key-模型周请求次数预算)
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

## 生图按张计费

在管理后台 **模型定价**（`/admin/model-pricing`）找到图片模型，把「用户生图计费」切换为「按成功图片张数」，填写大于 0 的每张美元单价后保存。默认仍为 Token 计费；恢复默认价格会同时恢复 Token 计费。例如单价 `$0.05`，成功返回 2 张图片扣 `$0.10`，Key 的 `$10` 额度剩余 `$9.90`。

- 按实际成功图片张数收费，不按 HTTP 请求数收费；失败、取消和内部重试不收图片费用。多图任务部分成功时只结算成功部分。
- 图片 API（`/v1/images/generations`、`/v1/images/edits`，含流式）在成功响应后记账；工作台任务在图片保存完成后结算，无法保存或解码的结果不收费。
- Token 价格继续核算上游成本（`account_billed`）；用户费用（`user_billed`）按张计算，用于 Key 累计额度、总消费及分组/账号预算，不额外叠加 Token 费用。文本模型通过 Responses 内嵌图片工具的请求仍沿用 Token 计费。
- 每条用量记录保存计费方式、单价和收费张数，调价不重算历史记录。配置按实际生效的模型定价键匹配：GPT Image 2.5 的日期和 2K/4K 别名共用对应 Flare/Sunburst 价格；GPT Image 2 的基础、2K、4K 模型可分别设置。
- 公开工作台在生成按钮上方显示所选模型的每张单价，Key 旁显示剩余额度。额度沿用异步结算机制；这不是预扣余额或并发余额预留，并发/批量请求仍可能超过剩余额度。

## 环境变量配置

### 核心服务配置

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `CODEX_PORT` | 否 | 8080 | HTTP 服务端口 |
| `BIND_HOST` | 否 | `127.0.0.1`（SQLite）/ `0.0.0.0`（PostgreSQL） | Docker 端口发布绑定地址（非进程监听地址，由 `CODEX_BIND` 控制）。SQLite compose 默认 `127.0.0.1` 仅本机访问；标准 compose 默认 `0.0.0.0` 所有网络接口 |
| `CODEX_MAX_REQUEST_BODY_SIZE_MB` | 否 | 48 | HTTP 请求体上限。后台 MP4 动态壁纸上传最大 40MB，默认值为 multipart 上传预留余量 |
| `ADMIN_SECRET` | 否 | - | 管理后台登录密钥 |
| `CODEX_ALLOW_ANONYMOUS` | 否 | `false` | 设为 `true` 时，未配置任何对外 API Key 也允许 `/v1/*` 直接调用（仅限内网测试场景） |
| `CODEX_SCHEDULER_ENGINE` | 否 | 空 | 调度引擎强制值：`legacy` / `shadow` / `indexed`。设置后优先于数据库配置，适合容器级灰度或紧急回退 |
| `CODEX_SCHEDULER_MAX_WAITERS` | 否 | `4096` | 本实例账号调度等待请求总上限，正整数，重启生效。队列满立即返回可重试的 503 |
| `CODEX_SCHEDULER_MAX_WAITERS_PER_KEY` | 否 | `256` | 本实例每个 API Key 的调度等待上限，正整数，重启生效；匿名请求共用一个计数 |
| `FAST_SCHEDULER_ENABLED` | 否 | `false` | 旧版兼容开关；未设置 `CODEX_SCHEDULER_ENGINE` 且数据库没有 `SchedulerEngine` 时，`true` 映射为 `indexed` |
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
| `CODEX_REQUEST_COMPRESSION` | 否 | 跟随系统设置 | 覆盖系统设置「Codex HTTP 请求体压缩」。`zstd`/`on`/`true`/`1` 强制开启，`off`/`false`/`0` 强制关闭，未设置或取值无法识别时以系统设置为准。作为部署级逃生阀存在：DB 不可达或后台打不开时仍可整机切换 |
| `CODEX_TELEMETRY_ENABLED` | 否 | 跟随系统设置 | 设为 `false` 时无视管理后台「客户端遥测」开关，部署层强制关闭模拟遥测外发 |
| `CODEX_STATSIG_API_KEY` | 否 | 内置公开 key | 覆盖 Codex Desktop/CLI 共用的公开 Statsig SDK key，仅遥测开启时使用 |
| `CODEX_SESSION_HEADER_MODE` | 否 | `native` | 出站会话头形态。`native` 发真实客户端的 `session-id` / `thread-id` / `x-client-request-id`；`legacy` 回退到旧的 `Session_id`（WS 另带 `Conversation_id`） |
| `CODEX_SESSION_HEADER_ALIGN_CONVERGED` | 否 | `false` | 开启后 `session-id` 头改用指纹收敛后的会话身份，与 turn metadata 的 `session_id` 对齐。默认关：请求体 `prompt_cache_key` 始终独立隔离，但上游是否也拿该头参与缓存分组无法从客户端源码确认 |
| `DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL` | 否 | `30s` | 下游 HTTP/SSE 保活周期，使用 Go duration；`0` 关闭。流式端点从首个心跳起建立 SSE 200，发送注释或 Messages ping；非流式端点发送 HTTP 102 |
| `DOWNSTREAM_WS_KEEPALIVE_INTERVAL` | 否 | `45s` | 下游 WebSocket Ping 周期，使用 Go duration；`0` 关闭。覆盖 Responses、Realtime 与 Live Sideband |

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
| `CODEX_IMAGES_MAIN_MODEL` | 否 | `gpt-5.6-luna` | 生图文本驱动的部署默认值；后台「Codex → 生图设置」选择具体模型后优先使用后台配置 |
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

#### API Key 鉴权缓存

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `CODEX_API_KEY_AUTH_CACHE_ENABLED` | 否 | `true` | 启用鉴权 L1/L2；设置 `false` 并重启后恢复旧版鉴权缓存策略 |

启用后，带分组、模型权限、有效期和限额配置的 Key 也可缓存。读取顺序为本地 L1 → Redis L2 → 数据库；Memory 模式只有 L1。L1 绝对 TTL 为 15 秒，最多 4,096 条、16 MiB 逻辑 JSON 快照，单条上限 64 KiB；超大条目直接回源。Redis L2 TTL 为 5 分钟，按数据库作用域、鉴权修订号和 Key 摘要隔离。缓存不保存原始 Key 或已用额度；确认不存在的 Key 只进入有界 L1，TTL 为 2 秒。字节预算不包含 Go 对象、map 或 allocator 开销，不是进程 RSS 上限。

启用、停用、删除、修改 Key 配置会在同一数据库事务内推进 `api_key_auth_cache_state` 修订号；累计用量写入不推进修订号。每个实例在活跃鉴权时复核该修订号，复核结果最多复用 250 毫秒。本实例的管理端操作同步清理 L1，并通过 Redis Pub/Sub 通知其他实例；通知丢失或旧版本实例修改数据时，数据库复核仍会发现变化。250 毫秒是修订结果的复用上限，不包含在途请求和基础设施延迟。数据库复核失败返回 503，不使用无法确认的旧快照，也不会误开启匿名访问。

设置了累计额度的 Key 每次鉴权仍查询数据库中的 `quota_used`；模型周预算仍在转发前执行数据库权威计数和请求幂等校验。窗口统计的原有 TTL、已建立长连接的校验策略不变。修订号变更会淘汰当前实例的全部鉴权条目；频繁修改 Key 配置会增加冷缓存回源。快照回填使用版本隔离和本地代际检查，延迟完成的旧查询不能恢复新版本的权限。

启动自动创建修订表和 PostgreSQL/SQLite 触发器，无需手工迁移。关闭两级缓存后仍保留这些表和触发器，便于混合版本部署；旧版策略只对无访问约束的 Key 缓存元数据，并合并同一时刻的相同 Key 查询。

`GET /api/admin/ops/overview` 的 `api_key_auth_cache` 提供开关、L1 条目/字节数、本地/远端命中、数据库配置加载次数、动态额度读取次数、修订号复核次数、失效、淘汰、超限旁路和错误计数。评估收益时应分开看配置回源与动态额度查询。

#### Codex 客户端遥测

**实验性功能，默认关闭。** 开启后，Codex OAuth 的普通 Responses 请求会按所选 Codex Desktop/CLI 指纹异步发送客户端遥测。分析事件发送到 `chatgpt.com/backend-api/codex/analytics-events/events`，OTLP metrics 发送到 `ab.chatgpt.com/otlp/v1/metrics`；失败不会影响代理响应，沿用账号的代理地址，Resin 启用时与 `/responses` 一样经反代发出。注意：工具调用、文件修改、hook 等事件是随机模拟生成的，并非对真实请求的观测，与上游侧可见的请求流可能不一致；是否开启由部署者自行评估。

管理后台「系统设置 → Codex → 客户端遥测」可实时开关，字段为 `codex_telemetry_enabled`（新装与升级安装均默认关闭）。`CODEX_TELEMETRY_ENABLED=false` 是部署层强制关闭开关，无视后台设置。`CODEX_STATSIG_API_KEY` 可覆盖内置的公开 SDK key；当前 Codex Desktop 与 Codex CLI 使用同一个 key。

事件按 Codex CLI 的结构模拟：首次观察到的 thread 使用 `codex_thread_initialized`，每轮生成 `codex_turn_event`，结束时生成 4 个 `codex_hook_run`。`codex_dynamic_tool_call_event` 每轮随机 40%，命中后其中 50% 同时生成 `codex_command_execution_event`；`codex_file_change_event` 每轮随机 20%，并同时生成 `codex_accepted_line_fingerprints`，其 `repo_hash` 固定为 `null`。这些随机事件不解析请求中的命令、工具调用或 diff。

原生 `codex_turn_steer_event` 只对应 App Server 的 `turn/steer` RPC；Responses 请求无法可靠识别，因此不会模拟。普通 turn 固定 `steer_count=0`，历史 assistant/tool 内容、`previous_response_id` 和恢复标记也不会被推断为 resumed。标题和 guardian 子流程使用独立的初始化事件。OTLP 首批发送 HAR 中的 62 个启动指标，随后每 60 秒增量发送本轮产生的 turn/hook/tool 指标；包含仅在后续样本出现的 4 个名称，共覆盖 66 个名称。

#### 窗口用量与连接池

API Key 启用多个 RPM/RPD/费用/Token 窗口时，Redis 会通过一次 `MGET` 读取这些窗口的统计缓存；缺失、损坏或读取失败仍按原有顺序回源数据库。自然日和滑动窗口的定义、60 秒统计缓存 TTL、错误码均保持不变。Memory 驱动提供同等批量读取语义。

分组/账号预算的三个共享分钟桶通过 Pipeline 一起读取，同一个 Key 的并发回源合并为一次，继续复用原有 5 秒本地快照。共享增量最多启动 16 个后台写入，槽位满时由调用方同步写入并承受背压；Redis 故障时仍以数据库用量聚合为后备。

运维概览和运行状态中的 Redis `usage_percent` 表示本进程连接池占用：`(total_conns - idle_conns) / pool_size`，不表示 Redis 服务端 CPU、内存或数据命中率。`stale_conns` 是累计移除连接数，不参与当前占用计算。`wait_count`、`wait_duration_ns`、`timeouts` 为累计连接池等待/超时指标，`pending_requests` 表示当前等待连接的请求数，可用于判断是否需要调整连接池。

---

## 系统设置（数据库）

系统设置存储在数据库的 `SystemSettings` 表中，可通过管理后台 `/admin/settings` 实时修改。

### Codex 生图设置

管理后台「系统设置 → Codex → 生图设置」可选择生图使用的文本驱动模型。选择后自动保存，对本实例之后构造的生图请求立即生效，重启后从数据库恢复。

管理 API `PUT /api/admin/settings` 使用 `codex_images_main_model` 字段，空字符串表示「使用部署默认值」。优先级为：后台配置 → `CODEX_IMAGES_MAIN_MODEL` → 内置 `gpt-5.6-luna`。`GET /api/admin/settings` 同时返回只读的 `codex_images_default_main_model`，用于展示部署默认值。模型名称长度不超过 128 字节，不含空白或控制字符，不能填 `gpt-image-*` 图像模型。

设置覆盖管理与公开生图工作台、`/v1/images/generations`、`/v1/images/edits`，以及顶层 `model` 填写图像模型的 `/v1/responses` 请求。原生 Responses 请求若显式填写文本 `model`，则继续使用该模型。图像模型由工作台或 API 请求选择；Images 链路在上游明确拒绝文本驱动时仍会按现有候选顺序重试。

### 模型列表读取上限

`models_list_read_max_bytes` 限制上游 OpenAI 兼容 `/v1/models` 与 Codex OAuth 模型清单成功响应的最大读取大小。默认 `8,388,608` bytes（8 MiB），管理后台以整数 MiB 展示，允许范围为 1-256 MiB。响应超过上限时请求会明确失败，不会把截断的 JSON 当成完整模型列表解析。

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
| `SchedulerEngine` | string | `legacy` | `legacy` / `shadow` / `indexed` | 调度执行引擎；设置页可热切换，跨实例通过数据库 outbox 增量同步 |
| `FastSchedulerEnabled` | bool | false | - | 旧版兼容字段；新部署应使用 `SchedulerEngine` |
| `CodexForceWebsocket` | bool | false | - | 强制 Codex 上游走 WebSocket 长连接（复用连接池），更接近官方 CLI 体验；关闭时走原有 HTTP 请求 |
| `CodexWSKeepaliveEnabled` | bool | false | - | 启用上游 WS 空闲连接保活（后台仅发 Ping，不发起新请求、不消耗账号额度） |
| `CodexWSKeepaliveIntervalSec` | int | 60 | 10-600 | WS 保活 Ping 间隔（秒），仅在 `CodexWSKeepaliveEnabled` 开启时生效 |
| `CodexWSHideUpstreamErrors` | bool | true | - | WS 上游最终失败时向客户端隐藏原始错误，返回统一友好提示；原始错误仍记录在后台日志/用量记录 |
| `CodexWSSilentRetryEnabled` | bool | true | - | WS 首包前遇到限流、额度耗尽、5xx、读取错误或超时时，静默换账号并重建上游 WS |
| `CodexWSSilentMaxRetries` | int | 2 | 0-10 | WS 首包前静默重试上限；`0` 禁用该预算 |
| `SchedulerMode` | string | `round_robin` | - | 调度模式：`round_robin`（轮询，按调度分权重排序）、`remaining_quota`（优先使用用量少的账号）或 `fill_first`（顺序耗尽：集中使用剩余额度最少的账号，耗尽/限流后切下一个）。索引引擎在同一优先级和健康档位内按最多 8 个可用候选的窗口比较实时占用；配额模式仍优先比较用量。窗口被过滤或并发占满时继续补选，不保证全池绝对最小占用。 |
| `AffinityMode` | string | `bounded` | - | 会话亲和：`bounded`（账号不健康或绑定空闲超过 10 分钟时重新挑号，活跃会话不轮换以保住上游 prompt cache）、`off`（每次重选）、`strict`（长期粘连） |

调度优先级先决定账号层级，同一优先级内再比较健康档位、调度分和当前负载；会话亲和只负责复用已绑定账号。多个最终用户共享同一个 API Key 时，下游可传 `X-Codex2API-Affinity-Key`，值会先哈希且仅用于本地账号绑定，不会转发给上游。

调度引擎的推荐上线顺序是 `legacy → shadow → indexed`：

- `legacy` 保留原有全池扫描，作为无停机回退路径。
- `shadow` 仍由 legacy 选号，每 64 次请求抽样一次索引可用性并在运维页展示一致/差异计数；它用于短时灰度，不建议长期承载全量流量。
- `indexed` 使用分层内存索引、稀疏 API Key 路由子池和事件驱动等待。账号数增长时，稳态选号不再复制或扫描完整账号切片。

索引选号的过滤器与准入回调在调度锁外执行，返回后重新检查候选代次、账号状态和并发；`Disabled` / `DispatchPaused` 同样阻止最终占位。已有会话绑定、容量借号保护和有状态续链的账号约束保持生效。

账号满载时，等待队列同时受全局与单个 API Key 上限约束，所有使用该账号池等待路径的协议和上游共用预算。HTTP 队列溢出返回 `503` 和 `Retry-After: 1`；已提交的 SSE 输出对应协议的失败事件，WebSocket 返回错误帧并以 `1013` 关闭，文案提示 1 秒后重试。该本地过载不会进入持续重试的上游换号循环，也不会被误报为账号额度耗尽。已有等待者不因调低上限而被取消；环境变量不是跨实例配额，也不限制已经在上游执行的请求。

队列内按 API Key 轮转，同一 Key 按可尝试请求的入队顺序唤醒。普通单槽释放只唤醒一个等待者；已有续链绑定和排除账号用于跳过不匹配的通知，剩余模型、分组和 scope 过滤仍在锁外执行，失败后把机会交给后续等待者。一轮通知最多尝试当前等待集合一次，同时最多有 8 个通知驱动的选号；密集释放合并为后续容量检查。公平性针对已排队的请求，不承诺绕过快路径新请求的全局先来先服务，也不保证不同过滤条件获得相同吞吐。SSE/WS 心跳不重新入队。每个有等待者的账号池只使用一个每秒恢复检查定时器，空队列自动停止；账号冷却自身的到期恢复通知仍然生效。

Codex 瞬时账号限流按 `15s → 30s → 60s → 120s → 240s → 300s` 退避。同一冻结窗口的并发 429 只推进一次；较长的真实 `Retry-After` 可延长该窗口（上限 5 分钟），普通重复 429 不顺延截止时间。短时冻结同样阻止 Spark 调度，但普通模型的 5h/7d 配额耗尽仍不占用 Spark 独立配额。短冻结不写数据库、不主动触发 WHAM 探测，到期直接恢复本地索引。原生 Redis/Memory 缓存保留限流类型和退避级别，并原子合并截止时间；迟到的短冻结不能覆盖配额或鉴权冷却。滚动升级期间旧实例无法识别新分类，建议完成全部实例升级后再评估短冻结行为。

运维 API 的 `scheduler` 指标新增 `fast_scanned_accounts`（实际候选检查数）、`fast_filter_checks`、`fast_acquire_failures`、`fast_lock_wait_ns` 和 `model_cooldown_cache_reads`。这些是本进程累计计数，宜取时间差计算每次选号成本；快路径命中不再代表没有扫描。`selection_duration_buckets` 为 `10us/100us/1ms/10ms/100ms/1s/+Inf` 累积直方图，覆盖与 `selection_total` 相同的普通/新会话选号，已有绑定的直接复用不计入该直方图。跨实例共享冷却与 outbox 不提供账号全局并发限制，并发名额仍由每个实例独立计数。

等待队列还暴露 `max_waiters`、`max_waiters_per_key`、`waiters`、`wait_rejected`（全部队列拒绝）、`wait_rejected_per_key`（其中因单 Key 上限被拒绝的子集）、`wait_granted`、`wait_duration_ns`，以及 `10ms/100ms/1s/10s/30s/+Inf` 的 `wait_duration_buckets` 累积直方图。等待耗时统计包含成功、取消和超时，拒绝入队不计入；`wait_wakeups / wait_granted` 的增量比可辅助观察无效唤醒，不能当作上游吞吐指标。Docker 部署应将两个新环境变量传给应用容器；项目标准/SQLite compose 的 `env_file` 会读取 `.env`，2004 专用 compose 可用 `environment` 覆盖。

启动会自动创建 `scheduler_outbox` 和 `maintenance_jobs` 及相应索引/触发器，PostgreSQL 与 SQLite 均无需手工迁移。多实例对账号、API Key、分组、代理和调度设置的变化按 outbox 水位增量重放；高频用量计数不会产生调度事件。环境变量 `CODEX_SCHEDULER_ENGINE` 一旦设置，会固定本实例引擎并覆盖管理后台值。

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

持续重试会把每次流式上游尝试完整暂存；失败整次丢弃，正常终态才一次性回放。目标端点等待上游响应头、读取响应体或流数据时保持下游连接：Responses、Chat Completions 和 Images 在首个保活周期到达时建立 SSE 200 并发送 `: keepalive` 注释，Messages 发送 Anthropic 原生 `event: ping`；Responses、Realtime 与 Live Sideband WebSocket 使用 Ping 控制帧。原生 Grok SSE 仍保留上游帧格式并允许插入保活帧。心跳提交 SSE 后，随后的上游错误会使用协议错误事件，不再改变 HTTP 200。非流式 JSON（包括 relay/native Responses、compact、Images、Grok 图片和 Alpha Search）使用标准 HTTP `102 Processing` 信息响应，不提交最终状态或 JSON；Cloudflare 收到 102 后仍要求在 125 秒内收到最终响应，因此它只能延长等待，不是无限期保活。`max_duration_seconds` 设置无限预算的墙钟时间上限（默认 600 秒，范围 1 到 900 秒），从请求第一次进入无限重试时开始，后续尝试不会重置。期限到达会立即取消上游并返回最近一次真实上游失败；仅在尚无失败可返回时使用 `504 upstream_timeout`。普通自选模式不会无限重试未选中的结构化安全策略拒绝；`catch_all` 可覆盖其他拒绝，但不能覆盖明确的上游 `cyber_policy` 或本地重试期限。

上述 HTTP/SSE 保活覆盖 `/v1/responses`（含 relay/native、stream 与 non-stream）、`/v1/chat/completions`、`/v1/messages`、`/v1/responses/compact`、`/v1/alpha/search`、`/v1/images/generations` 和 `/v1/images/edits`；视频、image jobs 与 `POST /v1/live` 不启用这套保活。Claude 原生 Messages 的首字前及已提交流保活继续由 `stream_keepalive_enabled` 共同控制，缺省为开启。

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

### 5h 窗口自动激活

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `AutoActivate5hWindowEnabled` | bool | false | 5h 窗口重置后自动发送一次最小真实 `/responses`，用于启动下一轮 5h 计时（issue #581）。默认关闭，开启会消耗少量真实额度。只对上游明确返回 5h 窗口和重置时间的账号生效；每个窗口最多一次。账号不可用、被自动暂停或没有 5h 窗口时跳过。不调用 `rate-limit-reset-credits/consume`，也不同于零成本 `wham/usage` 到点探针。 |

### 安全设置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `AdminSecret` | string | "" | 管理后台密码（数据库存储） |
| `AllowRemoteMigration` | bool | false | 允许远程迁移（需设置 AdminSecret） |

---

## API Key 模型周请求次数预算

在管理后台 **API Key → 高级限制** 中，可以为同一个 Key 给指定模型分配每周调用次数。例如 `gpt-6*` 每周合计 50 次，用完后其他未匹配模型仍可调用；不配置 `model_request_limits` 时保持原有行为。次数由管理员分配，系统不会假设某个上游套餐的每周额度。

通过创建或更新 API Key 接口配置 `limits.model_request_limits`：

```json
{
  "limits": {
    "model_request_limits": [
      {
        "model": "gpt-6*",
        "window": "week",
        "max_requests": 50,
        "timezone": "Asia/Shanghai",
        "reset_weekday": 1,
        "reset_time": "00:00"
      }
    ]
  }
}
```

| 字段 | 说明 |
| --- | --- |
| `id` | 服务端生成的稳定规则 ID；新增规则省略，修改次数上限或调整顺序时保留 |
| `model` | 最终映射后的模型名，支持精确名称和 `*` 通配；一条规则的所有匹配模型共享次数 |
| `window` | 当前仅支持 `week`，省略时默认为 `week` |
| `max_requests` | 每个固定周窗口的请求次数上限，必须为正整数 |
| `timezone` | IANA 时区，默认 `Asia/Shanghai`，与服务器 `TZ` 独立 |
| `reset_weekday` | 重置星期，`1` 为周一，`7` 为周日，省略或 `0` 时默认为周一 |
| `reset_time` | 时区内的重置时间，`HH:MM`，默认 `00:00` |

周窗口按指定时区的日历计算，包括夏令时变化。窗口是固定周期，例如上海时间本周一 00:00 到下周一 00:00；它与现有金额、Token 的滑动 `7d` 限额独立，所有限制同时生效。命中多条模型规则时，各条规则都要有余额并各计一次，因此可以同时配置 `gpt-6*` 系列总预算与 `gpt-6-astra` 单模型预算。

计数单位是一个外部 HTTP 请求，或 WebSocket 连接中的一个 `response.create`。全局和账号模型映射完成后，网关在尝试向上游发送时原子扣额；内部换号、传输回退与重试复用同一请求身份，同一规则最多计一次。已经尝试发送的请求即使失败、超时或取消也计入，入口校验、无可用账号和发送前拒绝不计入。客户端重新发起请求属于新请求。若内部重试映射到另一模型，则新命中的规则也需要额度。跨周继续重试不重复扣同一规则，已扣次数仍归属首次扣额窗口。

规则的 `id` 与模型匹配条件、时区及重置安排共同标识一个预算。已有规则只允许修改 `max_requests` 或列表顺序，保留已用次数；模型或重置安排需要变化时，删除旧规则并新增规则，新规则从零计数。提高次数上限立即提供更多余额；将上限调低到已用次数以下会停止放行该规则。更新请求不带 `limits` 时保留全部限制；传入 `limits` 时按完整限制对象替换，`model_request_limits: []` 移除模型周预算。

计数与请求幂等记录保存在 PostgreSQL / SQLite 的独立表中，启动时自动创建。PostgreSQL 多实例共享同一权威计数；SQLite 轻量模式使用相同语义。重启服务、清理 Redis/内存缓存、删除用量日志或重置金额额度不会清零模型周预算，到下一窗口自然获得新额度。扣额数据库不可用时，有模型周预算的相关请求返回服务不可用，避免并发超发。

后台 Key 编辑窗口和公开 `/key-usage` 页面展示本周已用、剩余、上限及下次重置时间。展示不受用量图表的 `today` / `7d` / `30d` / `all` 筛选影响，始终显示规则当前周窗口。该预算用于分配网关调用次数，不保证与上游套餐的计量口径相同；超额响应与查询接口见 [API 文档](API.md#api-key-模型周请求次数预算)。

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

### 惰性模式下的 Codex 授权保活

管理设置 `codex_oauth_keepalive_enabled`（默认 `false`）允许惰性模式单独运行 Codex Token 续期。它使用现有 `background_refresh_interval_minutes` 巡检间隔和 AT 到期前 5 分钟的阈值，不改变额度冷却、不启用生成探针，也不影响 Claude、Grok 或 Antigravity 的刷新策略。普通模式本来就运行 Codex 续期，不依赖此开关。

Codex 刷新新增 `codex_oauth_refresh_attempts` 保护表，启动时自动创建，兼容 PostgreSQL 与 SQLite。表内只保存旧 RT 的 SHA-256 指纹、刷新操作 ID 和开始时间，不保存明文 Token。成功保存全部相关凭据后删除记录；结果不确定的记录保留，防止跨实例或重启后重复消费旧 RT。已有凭据无需重新导入；已经失效的授权需重新登录恢复。
