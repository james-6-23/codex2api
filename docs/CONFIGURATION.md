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

### 调度配置

| 字段 | 类型 | 默认值 | 范围 | 说明 |
|------|------|--------|------|------|
| `MaxConcurrency` | int | 2 | 1-50 | 单账号最大并发请求数 |
| `GlobalRPM` | int | 0 | 0-∞ | 全局每分钟请求限制，0 表示不限 |
| `MaxRetries` | int | 3 | 0-10 | 请求失败最大重试次数 |
| `MaxRateLimitRetries` | int | 2 | 0-10 | 遇到 429 限流时的最大额外重试次数 |
| `FastSchedulerEnabled` | bool | false | - | 启用快速调度器 |
| `CodexForceWebsocket` | bool | false | - | 强制 Codex 上游走 WebSocket 长连接（复用连接池），更接近官方 CLI 体验；关闭时走原有 HTTP 请求 |
| `CodexWSKeepaliveEnabled` | bool | false | - | 启用上游 WS 空闲连接保活（后台仅发 Ping，不发起新请求、不消耗账号额度） |
| `CodexWSKeepaliveIntervalSec` | int | 60 | 10-600 | WS 保活 Ping 间隔（秒），仅在 `CodexWSKeepaliveEnabled` 开启时生效 |
| `CodexWSHideUpstreamErrors` | bool | true | - | WS 上游最终失败时向客户端隐藏原始错误，返回统一友好提示；原始错误仍记录在后台日志/用量记录 |
| `CodexWSSilentRetryEnabled` | bool | true | - | WS 首包前遇到限流、额度耗尽、5xx、读取错误或超时时，静默换账号并重建上游 WS |
| `CodexWSSilentMaxRetries` | int | 2 | 0-10 | WS 静默换号最大重试次数 |
| `SchedulerMode` | string | `round_robin` | - | 调度模式：`round_robin`（轮询，按调度分权重排序）或 `remaining_quota`（优先使用用量少的账号） |

### 测试配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `TestModel` | string | "gpt-5.5" | 测试连接使用的模型 |
| `TestContent` | string | "hi" | 测试连接发送给上游的用户输入内容 |
| `TestConcurrency` | int | 50 | 批量测试并发数，范围 1-200 |

### 代理配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `ProxyURL` | string | "" | 全局代理 URL |
| `ProxyPoolEnabled` | bool | false | 启用代理池 |

### 账号级设置（单账号）

以下字段存储在 `accounts` 表中，可通过管理后台账号详情或 API 按账号单独设置：

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `credit_enabled` | bool | false | 标记账号为信用计费模式 |
| `credit_skip_usage_window` | bool | false | 跳过 7 天/5 小时用量窗口惩罚（适用于信用账号） |
| `score_bias_override` | int/null | null | 手工覆盖调度权重分，`null` 跟随套餐默认 |
| `base_concurrency_override` | int/null | null | 手工覆盖基础并发值，`null` 跟随全局默认 |
| `skip_warm_tier` | bool | false | 跳过 warm 层级；仅把 warm 提升为 healthy，不覆盖 risky/banned |

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

**A:** 通过管理后台 `/admin/settings` 页面，可查看所有系统设置及配置来源（env/database）。

### Q: 配置错误导致无法启动怎么办？

**A:** 检查日志输出，常见错误：
- `DATABASE_HOST is empty` - 未配置数据库主机
- `REDIS_ADDR is empty` - Redis 模式下未配置 Redis 地址
- `DATABASE_PATH is empty` - SQLite 模式下未配置数据路径
