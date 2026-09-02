# Codex2API 架构设计文档

本文档描述 Codex2API 的整体架构设计、核心组件及数据流。

## 目录

- [架构概览](#架构概览)
- [核心组件](#核心组件)
- [数据流](#数据流)
- [调度系统](#调度系统)
- [存储层](#存储层)
- [安全设计](#安全设计)

---

## 架构概览

### 整体架构图

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              客户端 (Client)                                 │
│  - Web 浏览器 (/admin)                                                       │
│  - API 调用 (/v1/*)                                                         │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                              入口层 (Ingress)                                │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  Nginx / 负载均衡器                                                  │   │
│  │  - SSL 终止                                                        │   │
│  │  - 静态资源缓存                                                    │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Codex2API 服务 (Go + Gin)                          │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  HTTP 路由层                                                        │   │
│  │  ├── /admin/*     → React SPA (go:embed)                          │   │
│  │  ├── /api/admin/* → 管理 API (admin.Handler)                       │   │
│  │  ├── /v1/*        → 公共 API (proxy.Handler)                       │   │
│  │  └── /health      → 健康检查                                       │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                       │                                     │
│  ┌────────────────────────────────────┼─────────────────────────────────┐  │
│  │           中间件层 (Middleware)    │                                  │  │
│  │  ┌────────────────┐  ┌───────────┐ │  ┌────────────────────────────┐ │  │
│  │  │ Recovery       │  │ Logger    │ │  │ API Key Auth              │ │  │
│  │  │ ( panic 恢复 )  │  │ (请求日志) │ │  │ (公共 API 鉴权)            │ │  │
│  │  └────────────────┘  └───────────┘ │  └────────────────────────────┘ │  │
│  │  ┌────────────────┐  ┌───────────┐ │  ┌────────────────────────────┐ │  │
│  │  │ Admin Auth     │  │ RPM Limit │ │  │ CORS (开发环境)             │ │  │
│  │  │ (管理 API 鉴权) │  │ (全局限流) │ │  │                           │ │  │
│  │  └────────────────┘  └───────────┘ │  └────────────────────────────┘ │  │
│  └────────────────────────────────────┼─────────────────────────────────┘  │
│                                       │                                     │
│  ┌────────────────────────────────────▼─────────────────────────────────┐  │
│  │                         业务逻辑层                                    │  │
│  │  ┌──────────────────────┐  ┌──────────────────────────────────────┐ │  │
│  │  │   管理后台逻辑        │  │         代理转发逻辑                  │ │  │
│  │  │   (admin.Handler)    │  │        (proxy.Handler)               │ │  │
│  │  │  • 账号 CRUD         │  │                                     │ │  │
│  │  │  • 用量统计          │  │  • 账号选择 (auth.Store)            │ │  │
│  │  │  • 系统设置          │  │  • 请求翻译 (translator)            │ │  │
│  │  │  • 代理池管理        │  │  • 上游请求 (executor)              │ │  │
│  │  └──────────────────────┘  │  • 流式处理 (SSE)                   │ │  │
│  │                            └──────────────────────────────────────┘ │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                       │                                     │
│  ┌────────────────────────────────────▼─────────────────────────────────┐  │
│  │                          数据访问层                                   │  │
│  │  ┌──────────────────┐  ┌──────────────┐  ┌──────────────────────┐   │  │
│  │  │   Database       │  │    Cache     │  │      Token Store     │   │  │
│  │  │  (PostgreSQL/    │  │ (Redis/      │  │  (内存账号池状态)      │   │  │
│  │  │   SQLite)        │  │  Memory)     │  │                      │   │  │
│  │  │                  │  │              │  │  • 调度评分           │   │  │
│  │  │ • Accounts       │  │ • Session    │  │  • 并发计数           │   │  │
│  │  │ • Usage Logs     │  │ • Rate Limit │  │  • 健康状态           │   │  │
│  │  │ • Settings       │  │              │  │  • 冷却时间           │   │  │
│  │  └──────────────────┘  └──────────────┘  └──────────────────────┘   │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           上游服务 (Codex/OpenAI)                            │
│  - api.openai.com                                                          │
│  - api.codex.openai.com                                                    │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 核心组件

### 1. 路由处理器 (Handler)

#### proxy.Handler - 公共 API 处理器

```go
type Handler struct {
    store      *auth.Store          // 账号池
    configKeys map[string]bool      // 静态 API Keys
    db         *database.DB         // 数据库
    dbKeys     map[string]bool      // 动态 API Keys 缓存
    dbKeysUntil time.Time           // 缓存过期时间
}
```

**职责:**
- `/v1/chat/completions` - Chat Completions 接口
- `/v1/responses` - Responses 接口
- `/v1/models` - 模型列表
- API Key 认证
- 请求翻译 (OpenAI → Codex)
- 响应翻译 (Codex → OpenAI)

#### admin.Handler - 管理后台处理器

```go
type Handler struct {
    store          *auth.Store      // 账号池
    cache          cache.TokenCache // 缓存
    db             *database.DB     // 数据库
    rateLimiter    *proxy.RateLimiter // 限流器
    cpuSampler     *cpuSampler      // CPU 采样器
    // ... 其他字段
}
```

**职责:**
- 账号管理 (CRUD、导入导出)
- 用量统计与日志
- 系统设置管理
- 代理池管理
- 运维监控数据

### 2. 账号调度器 (auth.Store)

```go
type Store struct {
    accounts          []*Account                    // 写侧账号列表
    accountSnapshot   atomic.Pointer[accountListSnapshot] // 请求只读快照
    fastScheduler     atomic.Pointer[FastScheduler] // 分优先级/健康层索引
    schedulerEngine   atomic.Value                  // legacy/shadow/indexed
    availability      atomic.Pointer[availabilityHub]
    maxConcurrency    int64
}

type Account struct {
    DBID          int64             // 数据库 ID
    RefreshToken  string            // 刷新令牌
    AccessToken   string            // 访问令牌
    Email         string            // 邮箱
    PlanType      string            // 套餐类型

    // 运行时状态
    activeReqs    int64             // 活跃请求数
    totalReqs     int64             // 总请求数
    healthTier    HealthTier        // 健康层级
    schedulerScore float64          // 调度分数

    // 同步机制
    mu            sync.RWMutex      // 读写锁
}
```

**调度流程:**

```
┌─────────────────────────────────────────────────────────────┐
│                      账号选择流程                            │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  1. 过滤阶段                                                │
│     • 排除 Status = error 的账号                            │
│     • 排除 HealthTier = banned 的账号                       │
│     • 排除处于冷却期的账号                                   │
│     • 排除 AccessToken 为空的账号                           │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  2. 评分阶段                                                │
│     • 计算健康层级 (healthy/warm/risky/banned)              │
│     • 计算调度分数 (100 基线 + 各种惩罚/奖励)                │
│     • 计算动态并发限制                                       │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  3. 选择阶段                                                │
│     • 排除已达并发上限的账号                                 │
│     • 先按调度优先级排序                                     │
│     • 同优先级按健康层级和调度分数排序                       │
│     • Indexed 在最高优先级/健康层内使用游标或亲和哈希         │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│  4. 使用阶段                                                │
│     • 增加活跃请求计数                                       │
│     • 执行上游请求                                           │
│     • 减少活跃请求计数                                       │
│     • 报告成功/失败                                          │
└─────────────────────────────────────────────────────────────┘
```

**大号池执行模型：**

1. 账号集合在写侧变更后发布不可变指针快照；请求读快照不再复制 `[]*Account`。
2. `indexed` 按调度优先级和健康层分桶，稳态通常只检查一个候选；稀疏 API Key/分组规则按需建立有界子池缓存，密集规则直接复用全局索引。
3. 账号并发释放、冷却恢复、outbox 变更通过 generation channel 广播。等待账号的请求订阅事件并重试，不使用固定间隔轮询全池。
4. 数据库触发器把会改变路由的账号、API Key、分组、代理和设置写入 `scheduler_outbox`。每个实例按水位批量合并并增量更新内存投影，选号 miss 不再触发请求级全库 reconcile。
5. Grok 事实/目录维护使用 `maintenance_jobs(job_kind, due_at, lease_until)`；工作进程只 claim 到期行。PostgreSQL 使用 `SKIP LOCKED`，SQLite 使用事务写门，替代每 30 秒扫描全账号并解析多份 JSON。

`legacy` 保留旧扫描实现；`shadow` 由旧路径实际选号并对索引做 1/64 可用性抽样；`indexed` 才让索引结果成为权威。三个模式可在运行时切换，不改变 `/v1/*` 协议。

运维接口 `/api/admin/ops/overview` 的 `scheduler` 对象暴露选号耗时累计值、旧路径扫描账号数、等待/唤醒、路由子池缓存、shadow 差异，以及 outbox 水位、积压、延迟和错误数。

### 3. 请求执行器 (proxy.Executor)

```go
// ExecuteRequest 执行上游请求
func ExecuteRequest(ctx context.Context, account *auth.Account,
    body []byte, sessionID, proxyURL string) (*http.Response, error)
```

**功能:**
- TLS 指纹伪装
- User-Agent 随机化
- 代理支持（全局/账号/代理池）
- 连接池复用
- 受账号动态并发上限约束的持久 WebSocket 连接池
- SSE 流读取

如果上游 WS 在首个下游内容前返回 close 1009，执行器保留当前账号租约和已解析代理，最多用 HTTP 降级一次。该路径不会重新进入账号调度；已经输出内容后不会降级。

### 4. 协议翻译器 (proxy.Translator)

```go
// TranslateRequest OpenAI Chat → Codex Responses
func TranslateRequest(body []byte) ([]byte, error)

// TranslateStreamChunk Codex SSE → OpenAI SSE
func TranslateStreamChunk(data []byte, model, chunkID string) ([]byte, bool)
```

**转换内容:**
- `messages` → `input`
- `max_tokens/temperature` → 删除（Codex 不支持）
- `reasoning_effort` → `reasoning.effort`
- Anthropic `/v1/messages` 在无可用 Claude OAuth 账号时的 `speed:"fast"` → Codex `service_tier:"priority"`（Anthropic 入参 `service_tier` 为 Priority Tier，不参与 fast mode 映射）；Claude OAuth 账号优先走原生 Anthropic Messages 透传，不进入 Codex 转换链
- SSE 事件类型转换

---

## 数据流

### 请求处理流程

```
┌─────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│ Client  │────▶│   Router    │────▶│  Middleware │────▶│   Handler   │
└─────────┘     └─────────────┘     └─────────────┘     └──────┬──────┘
                                                               │
                    ┌──────────────────────────────────────────┼──────────┐
                    │                                          │          │
                    ▼                                          ▼          ▼
            ┌──────────────┐                         ┌──────────────┐  ┌─────────┐
            │ Account Store│                         │   Executor   │  │  Usage  │
            │   (Select)   │────────────────────────▶│   (Upstream) │  │ Logger  │
            └──────────────┘                         └──────┬───────┘  └─────────┘
                                                            │
                                                            ▼
                                                    ┌──────────────┐
                                                    │    Codex     │
                                                    │    API       │
                                                    └──────────────┘
```

### 账号池状态管理

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          账号状态机                                      │
│                                                                         │
│    ┌──────────┐                                                        │
│    │  Initial │                                                        │
│    └────┬─────┘                                                        │
│         │ Import/Add                                                    │
│         ▼                                                               │
│    ┌──────────┐     ┌──────────┐     ┌──────────┐                      │
│    │  ready   │────▶│ cooldown │────▶│  ready   │                      │
│    └────┬─────┘     └──────────┘     └──────────┘                      │
│         │          (429/401 冷却)       (冷却结束)                       │
│         │                                                               │
│         │ Error                                                        │
│         ▼                                                               │
│    ┌──────────┐     ┌──────────┐                                       │
│    │  error   │────▶│ deleted  │                                       │
│    └──────────┘     └──────────┘                                       │
│                    (软删除)                                              │
└─────────────────────────────────────────────────────────────────────────┘
```

### 冷却机制

```go
// 冷却类型与时长
type CooldownRule struct {
    RateLimited  time.Duration  // 429 限流: 从响应头解析或按套餐推断
    Unauthorized time.Duration  // 401 未授权: 5分钟 ~ 24小时
    Timeout      time.Duration  // 超时: 15分钟
    ServerError  time.Duration  // 5xx 错误: 15分钟
}

// 套餐类型冷却策略
const (
    FreeCooldown     = 7 * 24 * time.Hour  // Free 套餐 429: 7天
    TeamCooldown5h   = 5 * time.Hour       // Team 5h 窗口用完: 5小时
    TeamCooldown7d   = 7 * 24 * time.Hour  // Team 7d 窗口用完: 7天
)
```

---

## 调度系统

### 健康层级 (Health Tier)

```go
type HealthTier int

const (
    Healthy HealthTier = iota  // 健康
    Warm                        // 温热（近期有小问题）
    Risky                       // 风险（近期有大问题）
    Banned                      // 封禁
)
```

**层级判定:**

| 层级 | 条件 | 动态并发 |
|------|------|----------|
| Healthy | 无近期错误 | MaxConcurrency |
| Warm | 有轻微错误 | MaxConcurrency / 2 |
| Risky | 有严重错误 | 1 |
| Banned | 401 未授权 | 0 |

### 调度分计算

```go
type SchedulerScore struct {
    BaseScore float64 = 100

    // 惩罚项 (P)
    UnauthorizedPenalty float64  // 401: -50, 24h 衰减
    RateLimitPenalty    float64  // 429: -22, 1h 衰减
    TimeoutPenalty      float64  // 超时: -18, 15min 衰减
    ServerPenalty       float64  // 5xx: -12, 15min 衰减
    FailurePenalty      float64  // 连续失败: -6/次, 最多 -24
    UsagePenalty7d      float64  // 7d 用量: ≥70% -8, ≥100% -40
    LatencyPenalty      float64  // 延迟: ≥5s -4, ≥20s -15
    SuccessRatePenalty  float64  // 成功率: <75% -8, <50% -15

    // 奖励项 (B)
    SuccessBonus        float64  // 连续成功: +2/次, 最多 +12
}

// 最终分数 = BaseScore + ΣB - ΣP
```

### 调度算法伪代码

```
function SelectAccount():
    candidates = []

    for account in accounts:
        // 1. 基础过滤
        if account.Status == "error": continue
        if account.HealthTier == Banned: continue
        if account.IsInCooldown(): continue
        if account.AccessToken == "": continue

        // 2. 计算动态限制
        concurrencyLimit = calculateDynamicLimit(account.HealthTier)
        if account.ActiveRequests >= concurrencyLimit: continue

        // 3. 计算调度分
        account.SchedulerScore = calculateScore(account)

        candidates.append(account)

    if candidates is empty:
        return null

    // 4. 排序
    sort candidates by:
        - HealthTier (Healthy > Warm > Risky)
        - SchedulerScore (desc)
        - ActiveRequests (asc)

    // 5. 随机打散 (15% 概率)
    if random() < 0.15 and len(candidates) > 1:
        return candidates[random(0, min(3, len(candidates)))]

    return candidates[0]
```

---

## 存储层

### 数据库 Schema

```sql
-- 账号表
CREATE TABLE accounts (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    status VARCHAR(50) DEFAULT 'ready',
    credentials JSONB DEFAULT '{}',      -- email, refresh_token, access_token, etc.
    proxy_url VARCHAR(500),
    cooldown_until TIMESTAMP,
    cooldown_reason VARCHAR(50),
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW(),
    deleted_at TIMESTAMP                 -- 软删除
);

-- 用量日志表
CREATE TABLE usage_logs (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT REFERENCES accounts(id),
    account_email VARCHAR(255),
    endpoint VARCHAR(100),
    model VARCHAR(100),
    status_code INTEGER,
    duration_ms INTEGER,
    first_token_ms INTEGER,
    prompt_tokens INTEGER DEFAULT 0,
    completion_tokens INTEGER DEFAULT 0,
    total_tokens INTEGER DEFAULT 0,
    reasoning_effort VARCHAR(20),
    stream BOOLEAN DEFAULT FALSE,
    service_tier VARCHAR(20),
    created_at TIMESTAMP DEFAULT NOW()
);

-- 系统设置表
CREATE TABLE system_settings (
    id INTEGER PRIMARY KEY DEFAULT 1,
    max_concurrency INTEGER DEFAULT 2,
    global_rpm INTEGER DEFAULT 0,
    test_model VARCHAR(100) DEFAULT 'gpt-5.5',
    test_concurrency INTEGER DEFAULT 50,
    proxy_url VARCHAR(500),
    pg_max_conns INTEGER DEFAULT 50,
    redis_pool_size INTEGER DEFAULT 30,
    response_cache_local_max_bytes BIGINT NOT NULL DEFAULT 67108864,
    response_cache_local_max_entry_bytes BIGINT NOT NULL DEFAULT 8388608,
    response_cache_reconstruct_max_bytes BIGINT NOT NULL DEFAULT 67108864,
    response_cache_config_generation BIGINT NOT NULL DEFAULT 1,
    auto_clean_unauthorized BOOLEAN DEFAULT FALSE,
    auto_clean_rate_limited BOOLEAN DEFAULT FALSE,
    admin_secret VARCHAR(255)
);

-- API Keys 表
CREATE TABLE api_keys (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(255),
    key_hash VARCHAR(255) UNIQUE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
);

-- 代理池表
CREATE TABLE proxies (
    id BIGSERIAL PRIMARY KEY,
    url VARCHAR(500) NOT NULL,
    label VARCHAR(255),
    enabled BOOLEAN DEFAULT TRUE,
    last_tested_at TIMESTAMP,
    last_test_result VARCHAR(50),
    latency_ms INTEGER
);

-- 账号事件表（用于趋势分析）
CREATE TABLE account_events (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT REFERENCES accounts(id),
    event_type VARCHAR(50),  -- added, deleted, status_changed
    event_data JSONB,
    created_at TIMESTAMP DEFAULT NOW()
);
```

### 缓存策略

```go
// Redis 缓存结构
const (
    // 限流计数器
    KeyRateLimit = "ratelimit:{window}"

    // 会话缓存
    KeySession = "session:{session_id}"

    // 用量统计缓存
    KeyUsageStats = "usage:stats:{date}"

    // 账号 Token 缓存
    KeyAccessToken = "token:{account_id}"
)

// 缓存 TTL
type CacheTTL struct {
    RateLimit    time.Duration = 1 * time.Minute
    Session      time.Duration = 1 * time.Hour
    UsageStats   time.Duration = 5 * time.Minute
    AccessToken  time.Duration = 30 * time.Minute
}
```

#### Responses 上下文缓存

`previous_response_id` 连续请求使用一层每进程独立的有界 L1。完成响应时先按完整 call/output 组保留尾部最多 200 个 raw item，再写入 L1；默认总量 64 MiB、单条准入 8 MiB、最多 2,000 条、绝对 TTL 10 分钟。LRU 同时受条数和逻辑字节预算约束，命中不会延长绝对 TTL。

```text
完成响应
   │
   ├─ pair-safe 尾部裁剪（最多 200 items）
   │
   ├─ 本进程 L1（64 MiB total / 8 MiB entry / 2,000 entries / 10m）
   │
   └─ Redis 共享 response context（仅 Redis 模式）

previous_response_id 连续请求
   │
   ├─ L1 命中 ───────────────────────────────▶ 重建上下文
   │
   └─ L1 未命中
       ├─ Redis 有界读取 → 规范化 → 重建上限校验
       │    ├─ 符合 L1 准入 → 服务请求并提升到 L1
       │    └─ 超 L1、未超重建上限 → 仅服务本次请求，不提升
       └─ Memory 模式 → 无共享 response context 后备
```

三个数据库预算分别控制 L1 总量、L1 单条准入和共享后端重建上限。只有预算实际变化的事务提交才分配递增 generation，并立即应用到写入实例；同值或空更新不递增。其他实例每 5 秒读取一次，单次最多等待 3 秒，只应用更新的 generation。读取失败时保留最后一次有效配置，并记录同步错误供运维页展示。

Memory 模式只有 L1。已知超限/淘汰，或依赖的必需上下文缺失/过期且没有 relay 后备时，Responses/Compact 连续请求可返回 HTTP `409 response_context_unavailable`。Redis 值损坏或超过重建上限且没有 relay 后备时同样返回 409；共享后端传输故障且请求确实依赖该上下文时可返回 HTTP `503`。Redis 中未超过重建上限的上下文不会仅因超过 L1 准入预算而失败。

客户端原生 Responses WebSocket 入口不执行本地 response-cache 查找，保留 `previous_response_id` 交给上游。上述预算和 409/503 语义适用于本地重建的 HTTP Responses/Compact 路径。

`GET /api/admin/ops/overview` 的 `response_cache` 从一个锁内快照返回 effective/applied generation、当前/上限/高水位/最大观测条目的逻辑字节，以及本地/远程命中与未命中、过期、条数/字节淘汰、超限旁路、Memory 超限拒绝和最终 409 计数。这里的字节是保留 JSON payload 长度之和，不包含 Go 对象、LRU、allocator 或容器开销，不是 RSS 硬上限。进程内存需要结合 Linux/Docker RSS、Go `HeapAlloc` / `HeapInuse` / `HeapReleased` 和 `NumGC` 判断。

---

## 安全设计

### 认证机制

```
┌─────────────────────────────────────────────────────────────┐
│                      认证层级                                │
├─────────────────────────────────────────────────────────────┤
│  Level 1: 传输层                                             │
│  • HTTPS/TLS 加密                                           │
│  • HSTS 头部                                                │
├─────────────────────────────────────────────────────────────┤
│  Level 2: API 认证                                           │
│  • API Key Bearer Token                                     │
│  • 5分钟缓存，减少 DB 查询                                   │
├─────────────────────────────────────────────────────────────┤
│  Level 3: Admin 认证                                         │
│  • X-Admin-Key 请求头                                       │
│  • 支持环境变量或数据库配置                                  │
│  • 所有管理操作审计日志                                      │
└─────────────────────────────────────────────────────────────┘
```

### 敏感数据处理

```go
// Token 存储
func StoreToken(db *DB, accountID int64, token string) {
    // 数据库只存储加密后的 token
    encrypted := encrypt(token, encryptionKey)
    db.Exec("UPDATE accounts SET credentials = jsonb_set(credentials, '{access_token}', $1) WHERE id = $2",
        encrypted, accountID)

    // 同时缓存到 Redis（加密存储）
    redis.Set(ctx, fmt.Sprintf("token:%d", accountID), encrypted, 30*time.Minute)
}

// 日志脱敏
func SanitizeLog(msg string) string {
    // 隐藏 token 详情
    msg = regexp.MustCompile(`token=[^\s]+`).ReplaceAllString(msg, "token=***")
    msg = regexp.MustCompile(`Bearer [^\s]+`).ReplaceAllString(msg, "Bearer ***")
    return msg
}
```

### 限流与防护

| 层级 | 机制 | 说明 |
|------|------|------|
| 全局 | RPM 限流 | 基于令牌桶算法 |
| 账号 | 并发限制 | 动态调整 |
| IP | 连接限制 | 防止单 IP 过多连接 |
| 应用 | 请求大小 | 限制请求体大小 |

---

## 扩展性设计

### 水平扩展

```
┌─────────────────────────────────────────────────────────────┐
│                      负载均衡器                              │
│                   (Nginx / HAProxy)                         │
└─────────────────────────────────────────────────────────────┘
          │              │              │
          ▼              ▼              ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│  Codex2API #1   │ │  Codex2API #2   │ │  Codex2API #3   │
│                 │ │                 │ │                 │
│  + PostgreSQL   │ │  + PostgreSQL   │ │  + PostgreSQL   │
│  + Redis        │ │  + Redis        │ │  + Redis        │
└─────────────────┘ └─────────────────┘ └─────────────────┘
          │              │              │
          └──────────────┼──────────────┘
                         ▼
              ┌─────────────────┐
              │   共享 Redis    │
              │  (Session/锁)   │
              └─────────────────┘
```

### 组件替换

| 组件 | 当前实现 | 可替换为 |
|------|----------|----------|
| 数据库 | PostgreSQL | MySQL, TiDB |
| 缓存 | Redis | Memcached, 本地缓存 |
| HTTP | Gin | Echo, Fiber |
| 前端 | React | Vue, Svelte |
