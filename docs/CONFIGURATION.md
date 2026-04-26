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
| `ADMIN_SECRET` | 否 | - | 管理后台登录密钥 |
| `TZ` | 否 | UTC | 时区，如 `Asia/Shanghai` |

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
| `DATABASE_SSLMODE` | 否 | disable | SSL 模式: disable/require/verify-full |

### 生图工作台

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `IMAGE_ASSET_DIR` | 否 | `/data/images` | 管理台生图工作台保存图片文件的服务器目录；Docker 部署建议持久化 `/data` |

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
| `FastSchedulerEnabled` | bool | false | - | 启用快速调度器 |

### 测试配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `TestModel` | string | "gpt-5.4" | 测试连接使用的模型 |
| `TestConcurrency` | int | 50 | 批量测试并发数，范围 1-200 |

### 代理配置

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `ProxyURL` | string | "" | 全局代理 URL |
| `ProxyPoolEnabled` | bool | false | 启用代理池 |

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
