# Codex2API 故障排查指南

本文档提供 Codex2API 常见问题的排查方法和解决方案。

## 目录

- [服务启动问题](#服务启动问题)
- [数据库问题](#数据库问题)
- [账号池问题](#账号池问题)
- [API 请求问题](#api-请求问题)
  - [Responses 上下文连续请求](#症状-409-response_context_unavailable)
- [性能问题](#性能问题)
- [网络/代理问题](#网络代理问题)
- [日志分析](#日志分析)
- [WS 1009 与首事件延迟](#ws-1009-与首事件延迟)

---

## 服务启动问题

### 症状: 容器无法启动

**排查步骤:**

1. **查看容器日志**
```bash
docker compose logs -f codex2api
```

2. **检查常见错误**

| 错误信息 | 原因 | 解决方案 |
|----------|------|----------|
| `DATABASE_HOST is empty` | 未配置数据库主机 | 检查 `.env` 文件 |
| `REDIS_ADDR is empty` | Redis 模式未配置地址 | 检查 `.env` 文件 |
| `Redis 连接失败: EOF` | 云 Redis 要求 TLS，但当前按明文连接 | 使用 `rediss://...` 地址，或设置 `REDIS_TLS=true` |
| `DATABASE_PATH is empty` | SQLite 未配置路径 | 检查 `.env` 文件 |
| `connection refused` | 数据库未就绪 | 等待依赖服务启动 |

**健康检查脚本:**

```bash
#!/bin/bash
# health-check.sh

echo "Checking services..."

# 检查容器状态
if ! docker compose ps | grep -q "Up"; then
    echo "ERROR: Containers not running"
    docker compose ps
    exit 1
fi

# 检查健康端点
if ! curl -sf http://localhost:8080/health > /dev/null; then
    echo "ERROR: Health check failed"
    docker compose logs --tail=50 codex2api
    exit 1
fi

echo "All services healthy!"
```

### 症状: 端口被占用

**排查:**
```bash
# 查看端口占用
sudo lsof -i :8080
sudo netstat -tlnp | grep 8080

# 解决方案: 修改 .env 中的 CODEX_PORT
CODEX_PORT=8081
```

---

## 数据库问题

### 症状: 数据库连接失败

**PostgreSQL 模式:**

```bash
# 1. 检查 PostgreSQL 容器
docker compose ps postgres
docker compose logs postgres

# 2. 测试连接
docker exec -it codex2api-postgres psql -U codex2api -d codex2api -c "SELECT 1;"

# 3. 检查网络
docker compose exec codex2api ping postgres
```

**SQLite 模式:**

```bash
# 1. 检查数据目录权限
ls -la /path/to/sqlite/data/

# 2. 检查数据库文件
sqlite3 /data/codex2api.db ".tables"

# 3. 修复权限
chmod 755 /data
chmod 644 /data/codex2api.db
```

### 症状: 数据库性能差

**排查:**

```bash
# 查看慢查询（PostgreSQL）
docker exec codex2api-postgres psql -U codex2api -c "
SELECT query, calls, mean_time, total_time
FROM pg_stat_statements
ORDER BY mean_time DESC
LIMIT 10;
"

# 查看连接数
docker exec codex2api-postgres psql -U codex2api -c "
SELECT count(*) FROM pg_stat_activity;
"
```

**优化建议:**

1. 调整 `PgMaxConns` 设置
2. 添加数据库索引
3. 定期清理旧日志

```sql
-- 添加索引优化
CREATE INDEX CONCURRENTLY idx_usage_logs_created_at ON usage_logs(created_at);
CREATE INDEX CONCURRENTLY idx_usage_logs_account_id ON usage_logs(account_id);
```

---

## 账号池问题

### 症状: 所有账号不可用

**排查:**

```bash
# 1. 检查账号状态
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts | jq

# 2. 查看健康检查
curl http://localhost:8080/health
```

**常见原因:**

| 状态 | 原因 | 解决方案 |
|------|------|----------|
| error | 账号验证失败 | 检查 Refresh Token，手动刷新 |
| cooldown | 触发限流/错误冷却 | 等待冷却结束或手动清理 |
| banned | 401 未授权 | 检查账号是否被封禁 |

如果刷新账号时报 `unsupported_country_region_territory` 或 `Country, region, or territory not supported`，通常是刷新请求没有从支持地区出口发出。请检查账号自身 `proxy_url`、代理池和全局 `ProxyURL`。内部刷新链路按 `账号 proxy_url > 分组代理 > 账号 ID 粘性代理池 > 全局 ProxyURL` 生效；**开启代理池后不会直连**，绑定到已禁用托管代理的账号会拒绝刷新直到该代理重新启用。

**批量刷新脚本:**

```bash
#!/bin/bash
# refresh-all.sh

ADMIN_KEY="your-secret"
BASE_URL="http://localhost:8080"

# 获取所有账号 ID
ids=$(curl -s -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/accounts" | jq -r '.accounts[].id')

# 逐个刷新
for id in $ids; do
    echo "Refreshing account $id..."
    curl -s -X POST -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/accounts/$id/refresh"
    sleep 1
done
```

### 症状: 调度异常，总是选到同一个账号

**排查:**

1. 检查账号健康层级分布
2. 查看调度分数详情

```bash
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts | jq '.accounts[] | {id, health_tier, scheduler_score}'
```

**解决方案:**

- 触发账号重新评分: 重启服务或等待自动恢复探测
- 检查是否有账号长期处于 cooldown 状态

---

## API 请求问题

### 症状: 401 Invalid API Key

**排查:**

```bash
# 1. 检查是否配置了 API Key
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/keys

# 2. 如果没有配置，请求不需要认证
# 3. 如果配置了，确认请求头格式
curl -H "Authorization: Bearer sk-your-key" http://localhost:8080/v1/models
```

### 症状: 429 Too Many Requests

**排查:**

```bash
# 检查全局 RPM 设置
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/settings | jq '.global_rpm'

# 检查账号池状态
curl http://localhost:8080/health
```

**原因:**

1. **Global RPM 限制**: 增加 `global_rpm` 或设为 0
2. **账号池耗尽**: 所有账号都在冷却中
3. **单账号并发限制**: 增加 `max_concurrency`

### 症状: 502 Bad Gateway

**排查:**

```bash
# 查看错误日志
docker compose logs -f codex2api | grep -i "upstream\|error"

# 检查账号 Token 是否过期
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts | jq '.accounts[].status'
```

**常见原因:**

- 上游 Codex API 不可用
- 账号 Access Token 过期
- 网络连接问题

### 症状: 503 Service Unavailable

**排查:**

```bash
# 检查账号池可用数量
curl http://localhost:8080/health

# 检查是否有可用账号
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts | jq '.accounts[] | select(.status == "ready")'
```

**解决方案:**

1. 添加新账号
2. 清理冷却状态的账号
3. 等待冷却结束

### 症状: 409 response_context_unavailable

该错误表示 `previous_response_id` 所需上下文已经确定无法在当前路径重建。常见原因包括 Memory 模式下单条上下文超过 L1 准入预算、依赖上下文已被条数/字节 LRU 淘汰、必需上下文普通缺失或已经过期，或 Redis 值损坏、超过重建上限。

```bash
# 查看当前预算、逻辑占用、淘汰和不可用计数
curl -s -H "X-Admin-Key: your-secret" \
  http://localhost:8080/api/admin/ops/overview |
  jq '.response_cache | {
    effective_config,
    applied_config,
    entries,
    max_entries,
    current_bytes,
    max_bytes,
    count_evictions,
    byte_evictions,
    oversize_bypasses,
    oversize_rejections,
    known_unavailable_errors,
    last_config_sync_at,
    last_config_sync_error
  }'
```

**处理建议:**

1. 不要对同一个已确定不可用的 `previous_response_id` 做无条件重试；重新发送完整必需上下文或开始新的响应链。
2. Memory 模式重点查看 `oversize_rejections`、`count_evictions` 和 `byte_evictions`。可在设置页调整本地总量/单条准入，但总量不是 RSS 硬上限。
3. Redis 模式重点查看 `last_config_sync_error`、Redis 健康状态和 `reconstruct_max_bytes`。共享值在重建上限内但超过 L1 准入时会计入 `oversize_bypasses`，仍能服务请求，不需要仅为该计数扩大 L1。

### 症状: previous response context backend 返回 503

当请求确实依赖上一响应的工具调用上下文，而共享 response-context 后端暂时不可用且没有可用 relay 后备时，会返回 HTTP 503、错误码 `service_unavailable`。

```bash
curl -s -H "X-Admin-Key: your-secret" \
  http://localhost:8080/api/admin/ops/overview |
  jq '{redis: .redis, response_cache: {
    remote_hits: .response_cache.remote_hits,
    remote_misses: .response_cache.remote_misses,
    last_config_sync_at: .response_cache.last_config_sync_at,
    last_config_sync_error: .response_cache.last_config_sync_error
  }}'
```

先检查 Redis 网络、TLS、认证和连接池，再按退避策略重试。后端传输错误不会计入 `remote_misses`，因为它不是明确的缓存未命中。

---

## 性能问题

### 症状: 响应延迟高

**排查:**

```bash
# 1. 检查系统资源
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/ops/overview | jq '.cpu, .memory, .postgres, .redis'

# 2. 查看延迟分布
curl -s -H "X-Admin-Key: your-secret" "http://localhost:8080/api/admin/usage/logs?start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z" | jq '.logs[] | .duration_ms'

# 3. 检查数据库连接池
curl -s -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/ops/overview | jq '.postgres'
```

**优化建议:**

1. **增加连接池大小**
```bash
# 管理后台设置
PgMaxConns: 100
RedisPoolSize: 50
```

2. **切换索引调度器**
```bash
CODEX_SCHEDULER_ENGINE=indexed
```

3. **优化代理配置**
- 使用更近的代理节点
- 启用代理池

### 症状: RPM 不变，但账号越多 CPU 越高或直接满核

先查看调度热路径，而不是只看总 RPM：

```bash
curl -s -H "X-Admin-Key: your-secret" \
  http://localhost:8080/api/admin/ops/overview |
  jq '.scheduler | {
    engine,
    selection_total,
    selection_fast_hit,
    selection_slow_hit,
    slow_scanned_accounts,
    waiters,
    wait_wakeups,
    routing_cache_entries,
    routing_cache_accounts,
    shadow_checks,
    shadow_mismatches,
    outbox_backlog,
    outbox_lag_ms,
    outbox_errors
  }'
```

判断方法：

- `engine=legacy` 且 `slow_scanned_accounts` 随请求快速增长，说明 CPU 主要消耗在每次选号的 O(N) 全池过滤/评分。
- `engine=indexed` 时 `selection_slow_hit` 或 `slow_scanned_accounts` 仍增长，检查是否启用了 lazy fallback，或是否存在尚未迁移的调用路径。
- `routing_cache_entries=0` 不一定异常；只有候选比例不超过全池约 1/4 的稀疏 API Key 才建立子池，密集规则复用全局索引。
- `outbox_backlog` 持续增长、`outbox_lag_ms > 5000` 或错误数增加，说明跨实例内存投影落后，应先检查数据库连接、触发器和实例日志。
- `waiters` 高但 `wait_wakeups` 不增长，检查是否所有账号都被永久禁用；正常的并发释放或冷却恢复应产生事件唤醒。

生产切换建议先在管理后台设为 `shadow`，观察一段真实流量下 `shadow_mismatches` 是否保持为 0 或能由瞬时并发竞争解释，再切到 `indexed`。如出现选号异常，可立即切回 `legacy`；数据库 outbox 和维护任务表可以保留，不需要回滚 schema。环境变量 `CODEX_SCHEDULER_ENGINE` 的优先级高于管理后台，若页面切换不生效，请先检查容器环境。

### 调度 outbox 的版本要求与回滚

- **PostgreSQL 最低版本为 11**：outbox 触发器使用 `CREATE TRIGGER ... EXECUTE FUNCTION` 语法（PG 10 及更早只认 `EXECUTE PROCEDURE`），且 `accounts` 表新增的 `credential_generation NOT NULL DEFAULT` 列在 PG 11+ 才是元数据级变更。自管 PG ≤ 10 的部署升级前需先升级数据库。官方 compose 使用的 PostgreSQL 版本不受影响。
- **回滚到旧版本二进制**：触发器会留在数据库里继续向 `scheduler_outbox` 写事件，而旧版本没有消费者和清理任务，表会持续增长。短期回滚可以不处理（重新升级后自动消费并清理，也可直接 `TRUNCATE scheduler_outbox`）；长期回滚请执行以下清理（PostgreSQL）：

```sql
DROP TRIGGER IF EXISTS scheduler_outbox_accounts_insert ON accounts;
DROP TRIGGER IF EXISTS scheduler_outbox_accounts_update ON accounts;
DROP TRIGGER IF EXISTS scheduler_outbox_accounts_delete ON accounts;
DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_insert ON api_keys;
DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_update ON api_keys;
DROP TRIGGER IF EXISTS scheduler_outbox_api_keys_delete ON api_keys;
DROP TRIGGER IF EXISTS scheduler_outbox_groups_insert ON account_groups;
DROP TRIGGER IF EXISTS scheduler_outbox_groups_update ON account_groups;
DROP TRIGGER IF EXISTS scheduler_outbox_groups_delete ON account_groups;
DROP TRIGGER IF EXISTS scheduler_outbox_members_insert ON account_group_members;
DROP TRIGGER IF EXISTS scheduler_outbox_members_delete ON account_group_members;
DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_insert ON account_model_cooldowns;
DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_update ON account_model_cooldowns;
DROP TRIGGER IF EXISTS scheduler_outbox_cooldowns_delete ON account_model_cooldowns;
DROP TRIGGER IF EXISTS scheduler_outbox_proxies_insert ON proxies;
DROP TRIGGER IF EXISTS scheduler_outbox_proxies_update ON proxies;
DROP TRIGGER IF EXISTS scheduler_outbox_proxies_delete ON proxies;
DROP TRIGGER IF EXISTS scheduler_outbox_settings_update ON system_settings;
DROP TRIGGER IF EXISTS grok_maintenance_accounts_insert ON accounts;
DROP TRIGGER IF EXISTS grok_maintenance_accounts_update ON accounts;
DROP TRIGGER IF EXISTS grok_maintenance_accounts_delete ON accounts;
DROP FUNCTION IF EXISTS codex2api_scheduler_outbox_row();
DROP FUNCTION IF EXISTS codex2api_grok_maintenance_account();
DROP TABLE IF EXISTS scheduler_outbox;
DROP TABLE IF EXISTS maintenance_jobs;
```

SQLite 部署执行对应的 `DROP TRIGGER IF EXISTS <同名触发器>;` 与 `DROP TABLE IF EXISTS scheduler_outbox; DROP TABLE IF EXISTS maintenance_jobs;` 即可。

### 症状: 内存占用高

**排查:**

```bash
# 查看容器内存
docker stats codex2api --no-stream

# 对照进程 RSS、Go heap/GC 和 response-context 逻辑字节
curl -s -H "X-Admin-Key: your-secret" \
  http://localhost:8080/api/admin/ops/overview |
  jq '{memory: .memory, response_cache: .response_cache}'

# 查看 Go 内存分析（如启用 pprof）
curl http://localhost:8080/debug/pprof/heap > heap.prof
```

`response_cache.current_bytes`、`high_water_bytes` 和 `largest_entry_bytes` 是 JSON payload 的逻辑字节，不包含 Go 对象、切片、LRU、allocator 或容器开销，不能与 RSS 直接相加，也不能作为进程内存硬上限。Linux/Docker 的 `memory.process_bytes` 是进程 RSS；非 Linux 回退为 Go `Sys`。结合 `heap_alloc_bytes`、`heap_inuse_bytes`、`heap_released_bytes` 和 `num_gc` 判断堆增长与回收情况。

**优化:**

1. 限制日志保留时间
2. 如果 L1 逻辑占用和高水位持续接近上限，可在设置页降低 `response_cache_local_max_bytes`；降低后会立即淘汰超出新预算的条目，并可能增加 Memory 模式的 409
3. 减少并发连接数

---

## 网络/代理问题

### 症状: 上游请求超时

**排查:**

```bash
# 1. 测试直连 Codex API
curl -v https://api.openai.com/v1/models -H "Authorization: Bearer your-token"

# 2. 测试代理连通性
curl -x http://proxy:port https://api.openai.com/v1/models

# 3. 在容器内测试
docker compose exec codex2api wget -O- https://api.openai.com/v1/models
```

**解决方案:**

1. 检查代理配置
2. 更换代理节点
3. 增加超时时间（代码级别）

### 症状: SSL/TLS 错误

**排查:**

```bash
# 检查证书
echo | openssl s_client -connect api.openai.com:443 2>/dev/null | openssl x509 -noout -dates

# 更新 CA 证书
docker compose exec codex2api apk add --no-cache ca-certificates
```

### 代理测试脚本

```bash
#!/bin/bash
# test-proxy.sh

PROXY_URL="http://proxy.example.com:8080"

echo "Testing proxy: $PROXY_URL"

# 测试代理连通性
start_time=$(date +%s%N)
response=$(curl -s -o /dev/null -w "%{http_code}|%{time_total}" -x "$PROXY_URL" --max-time 10 https://api.openai.com/v1/models)
end_time=$(date +%s%N)

http_code=$(echo $response | cut -d'|' -f1)
time_total=$(echo $response | cut -d'|' -f2)

if [ "$http_code" = "200" ]; then
    echo "✓ Proxy is working (latency: ${time_total}s)"
else
    echo "✗ Proxy failed (HTTP $http_code)"
fi
```

---

## 日志分析

### 日志级别

| 级别 | 说明 | 示例 |
|------|------|------|
| INFO | 正常操作 | 账号刷新成功、请求完成 |
| WARN | 警告 | 账号进入冷却、重试请求 |
| ERROR | 错误 | 数据库连接失败、上游错误 |
| FATAL | 致命错误 | 启动失败、panic |

### 关键日志模式

```bash
# 实时监控错误
docker compose logs -f codex2api | grep -i "error\|fail\|panic"

# 统计状态码分布（按日志示例，第 5 列为 HTTP 状态码）
docker compose logs codex2api | awk '{print $5}' | sort | uniq -c | sort -rn

# 查找慢请求（按日志示例，第 6 列为延迟，如 523ms；这里筛选 > 1000ms 的请求）
docker compose logs codex2api | awk '{lat=$6; gsub(/ms/,"",lat); if (lat+0 > 1000) print $0}' | sort -k6,6n | tail -20

# 统计账号请求量（按日志示例，邮箱在方括号中，且包含 @）
docker compose logs codex2api | awk '{for(i=1;i<=NF;i++){if($i ~ /@/){gsub(/[\[\]]/,"",$i); print $i}}}' | sort | uniq -c | sort -rn
```

### 日志字段说明

```
2024/01/01 12:00:00 api/server.go:123: POST /v1/chat/completions 200 523ms gpt-5.5 effort=medium [user@example.com] [proxy1]
│                 │                │    │                        │   │     │          │          │                │
│                 │                │    │                        │   │     │          │          │                └── 使用的代理
│                 │                │    │                        │   │     │          │          └── 账号邮箱
│                 │                │    │                        │   │     │          └── 推理强度标签
│                 │                │    │                        │   │     └── 模型名称
│                 │                │    │                        │   └── 响应时间
│                 │                │    │                        └── HTTP 状态码
│                 │                │    └── 请求路径
│                 │                └── HTTP 方法
│                 └── 源码文件和行号
```

### WS 1009 与首事件延迟

上游 WebSocket 可能在等待较长时间后才返回 close 1009。若此时尚未向客户端输出内容，Codex2API 会保留同一账号和代理并降级一次 HTTP；已经输出内容后不会降级。

搜索包含 `WebSocket 1009 HTTP 降级尝试结束` 的日志，并按以下字段拆分耗时：

```text
fallback_id
source=peer_close|local_read_limit
attempt
account
endpoint
status
ws_elapsed_ms
http_elapsed_ms
http_first_event_ms
total_first_event_ms
total_elapsed_ms
```

`ws_elapsed_ms` 是降级前不可避免的 WS 等待，`http_first_event_ms` 是 HTTP 真正开始后的首事件时间；同一 `fallback_id` 可关联后续 HTTP 重试。当前没有可靠证据表明 1009 与 token 数或最终请求体字节数存在统一阈值，因此排障时不要据此猜测或提前切换协议。

### 诊断脚本

```bash
#!/bin/bash
# diagnose.sh - 综合诊断脚本

BASE_URL="http://localhost:8080"
ADMIN_KEY="${ADMIN_KEY:-default}"

echo "========== Codex2API 诊断报告 =========="
echo "时间: $(date)"
echo ""

echo "[1/6] 服务健康检查..."
health=$(curl -sf "$BASE_URL/health" 2>/dev/null && echo "OK" || echo "FAILED")
echo "Health: $health"
if [ "$health" = "FAILED" ]; then
    echo "错误: 服务未响应，请检查容器状态"
    docker compose ps
    exit 1
fi
echo ""

echo "[2/6] 账号池状态..."
curl -sf -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/stats" 2>/dev/null | jq .
echo ""

echo "[3/6] 数据库连接..."
db_status=$(curl -sf -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/ops/overview" 2>/dev/null | jq -r '.postgres.healthy')
echo "PostgreSQL: $db_status"
echo ""

echo "[4/6] 缓存状态..."
cache_status=$(curl -sf -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/ops/overview" 2>/dev/null | jq -r '.redis.healthy')
echo "Redis: $cache_status"
echo ""

echo "[5/6] 系统资源..."
curl -sf -H "X-Admin-Key: $ADMIN_KEY" "$BASE_URL/api/admin/ops/overview" 2>/dev/null | jq '{cpu: .cpu, memory: .memory, response_cache: .response_cache}'
echo ""

echo "[6/6] 最近错误..."
docker compose logs --since=1h codex2api 2>/dev/null | grep -i "error" | tail -5 || echo "无近期错误"
echo ""

echo "========== 诊断完成 =========="
```

---

## 常见问题速查

### Q: 如何重置 Admin Secret？

```bash
# 方法1: 通过环境变量
docker compose down
export ADMIN_SECRET=new-secret
docker compose up -d

# 方法2: 直接修改数据库
docker exec -it codex2api-postgres psql -U codex2api -c "UPDATE system_settings SET admin_secret = 'new-secret';"
```

### Q: 如何清理所有日志？

```bash
# 通过 API
curl -X DELETE -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/usage/logs

# 或直接清理数据库
docker exec codex2api-postgres psql -U codex2api -c "TRUNCATE usage_logs;"
```

### Q: 如何导出所有账号？

```bash
curl -H "X-Admin-Key: your-secret" "http://localhost:8080/api/admin/accounts/export" > accounts_backup.json
```

### Q: 服务启动后账号显示 error？

1. 检查网络连通性
2. 手动刷新账号 Token
3. 检查账号是否被封禁

```bash
# 批量刷新
curl -X POST -H "X-Admin-Key: your-secret" http://localhost:8080/api/admin/accounts/batch-test \
  -H "Content-Type: application/json" \
  -d '{"ids": [1, 2, 3], "concurrency": 3}'
```
