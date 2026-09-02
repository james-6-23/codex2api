# 增强代理池管理

本文档描述了 codex2api 的增强代理池管理功能。

## 功能概述

增强代理池管理提供了以下核心功能：

1. **代理健康检查**
   - 定期 HTTP HEAD 检查
   - 响应时间监控
   - HTTP/SOCKS5 代理支持

2. **代理轮换策略**
   - 轮询 (Round-robin)
   - 加权选择 (Weighted)
   - 最少连接 (Least Connections)

3. **代理池状态监控**
   - 健康代理列表
   - 代理统计信息
   - 实时状态更新

4. **账号级代理优先级**
   - 账号单独配置的 `proxy_url` 优先
   - Refresh Token 刷新、测试连接、用量探针会按账号 ID 粘性选择代理池出口
   - 未启用代理池时回退到全局 `ProxyURL`

5. **代理性能统计**
   - 成功率跟踪
   - 延迟监控
   - 活跃连接数
   - 总请求数

6. **故障代理自动隔离**
   - 连续失败阈值检测
   - 自动隔离故障代理
   - 隔离期满后自动恢复

## 核心数据结构

### ProxyPool

```go
type ProxyPool struct {
    proxies   []*ProxyEntry
    stats     map[string]*ProxyStats
    healthy   []*ProxyEntry
    // ... 配置和运行时状态
}
```

### ProxyEntry

```go
type ProxyEntry struct {
    URL                 string
    Healthy             bool
    LastCheck           time.Time
    Latency             time.Duration
    SuccessRate         float64
    Weight              int64
    ActiveConns         int64
    TotalRequests       int64
    FailedRequests      int64
    Status              ProxyHealthStatus
    IsolatedAt          time.Time
    ConsecutiveFailures int
}
```

### ProxyStats

```go
type ProxyStats struct {
    URL                 string
    Healthy             bool
    LastCheck           time.Time
    Latency             time.Duration
    LatencyMs           float64
    SuccessRate         float64
    Weight              int64
    ActiveConns         int64
    TotalRequests       int64
    FailedRequests      int64
    Status              string
    ConsecutiveFailures int
}
```

## API 使用示例

### 创建代理池

```go
config := &auth.ProxyPoolConfig{
    Strategy:           auth.StrategyRoundRobin,
    CheckInterval:      30 * time.Second,
    Timeout:            10 * time.Second,
    IsolationThreshold: 3,
    IsolationDuration:  5 * time.Minute,
    HealthCheckURL:     "http://www.google.com/generate_204",
}

pool := auth.NewProxyPool(config)
```

### 添加代理

```go
pool.AddProxy("http://proxy1:8080", 10)
pool.AddProxy("http://proxy2:8080", 5)
```

### 选择代理

```go
// 使用默认策略（轮询）
entry := pool.Select()
if entry != nil {
    fmt.Println("Selected proxy:", entry.URL)
}

// 使用加权策略
pool.SetStrategy(auth.StrategyWeighted)
entry := pool.Select()

// 使用最少连接策略
pool.SetStrategy(auth.StrategyLeastConnections)
entry := pool.Select()
```

### 标记成功/失败

```go
// 标记代理成功
pool.MarkSuccess("http://proxy1:8080")

// 标记代理失败
pool.MarkFailure("http://proxy1:8080")
```

### 获取统计信息

```go
stats := pool.GetStats()
for url, stat := range stats {
    fmt.Printf("Proxy: %s, Success Rate: %.2f, Latency: %.2fms\n",
        url, stat.SuccessRate, stat.LatencyMs)
}

status := pool.GetPoolStatus()
fmt.Printf("Total: %d, Healthy: %d, Isolated: %d\n",
    status.Total, status.Healthy, status.Isolated)
```

### 启动健康检查

```go
// 设置回调
pool.SetOnHealthCheck(func(result *auth.HealthCheckResult) {
    if !result.Healthy {
        log.Printf("Proxy %s health check failed: %v", result.URL, result.Error)
    }
})

pool.SetOnIsolation(func(entry *auth.ProxyEntry) {
    log.Printf("Proxy %s is isolated due to consecutive failures", entry.URL)
})

pool.SetOnRecovery(func(entry *auth.ProxyEntry) {
    log.Printf("Proxy %s has recovered", entry.URL)
})

// 启动定期健康检查
pool.StartHealthCheck()

// 停止
pool.Stop()
```

### 连接管理

```go
// 获取连接
if pool.AcquireConnection("http://proxy1:8080") {
    // 使用代理...

    // 释放连接
    pool.ReleaseConnection("http://proxy1:8080")
}
```

## 策略说明

### 轮询 (Round-robin)

按顺序轮流选择每个代理，适用于负载均匀分布的场景。

### 加权选择 (Weighted)

根据代理权重进行随机选择，权重高的代理被选中的概率更大。适用于代理性能不均衡的场景。

### 最少连接 (Least Connections)

选择当前活跃连接数最少的代理。适用于长连接场景，可以更好地均衡负载。

## 健康检查机制

1. **定期检查**：按配置的间隔时间定期检查所有代理
2. **响应时间监控**：记录每次检查的响应时间
3. **健康状态更新**：根据检查结果更新代理健康状态
4. **自动隔离**：连续失败超过阈值的代理会被自动隔离
5. **自动恢复**：隔离期满后，代理会重新参与健康检查，成功后自动恢复

## 配置环境变量

```bash
# 代理池策略: round_robin, weighted, least_connections
PROXY_POOL_STRATEGY=round_robin

# 健康检查间隔
PROXY_POOL_CHECK_INTERVAL=30s

# 健康检查超时
PROXY_POOL_TIMEOUT=10s

# 隔离阈值（连续失败次数）
PROXY_POOL_ISOLATION_THRESHOLD=3

# 隔离持续时间
PROXY_POOL_ISOLATION_DURATION=5m

# 健康检查 URL
PROXY_POOL_HEALTH_CHECK_URL=http://www.google.com/generate_204
```

## 代理生效优先级

请求与内部刷新（Refresh Token、账号测试、用量探针）按以下优先级解析出口：

```text
账号 proxy_url > 分组代理 > 账号 ID 粘性代理池 > 全局 ProxyURL > 直连
```

约束（issue #517）：

- 账号 `proxy_url` 指向代理池中的托管代理时，这是钉死出口：禁用/测挂后该账号不会改走其它代理，也不会直连；重新启用后自动恢复。
- 删除托管代理会解绑仍引用它的账号。
- 未绑定账号从当前启用且测试未失败的代理中粘性选择；一条不可用就换池内其它条目。
- **开启代理池后不会直连。** 池空且没有全局 `ProxyURL` 时，该账号不可调度，请求返回无可用账号，刷新拒绝直连。
- 从未加入代理池的自定义 `proxy_url` 仍按原样使用。

全局 `ProxyURL` 或代理池配置更新后会立即影响后续请求，不需要重启服务。

## 随账号导出/导入迁移代理绑定

账号导出默认只写凭据，换机后代理绑定要重配一遍。导出加 `include_proxy=1`、导入加
`import_proxy=true`，可以把「号池 + 代理绑定关系」当作一个整体迁走。三个渠道
（Codex `/accounts/import`、Grok `/accounts/grok/import`、Antigravity
`/accounts/antigravity/import`）都支持，参数细节见 [API.md](API.md)。

### 写入顺序

导入端严格按 **写代理表 → 重载内存代理池 → 才写账号** 的顺序执行，中途重载失败会
中止整次导入。

顺序不能颠倒，原因就是上一节那条 fail-closed 约束：账号一旦绑上「已登记为托管代理、
却还不在启用集里」的 URL，就会被判定为没有可用出口而不可调度。先写账号再重载代理池，
正好在这两步之间开出这样一个窗口，窗口期内导进来的账号全都调度不到。重载失败却继续
写账号，则是把这个窗口永久固定下来。

### 代理入表规则

- 单次最多注册 500 条。超限时**一条都不注册**，全部账号退回表单填写的代理——注册
  一半会让另一半账号绑上未入池的 URL，那种半生效状态比整体回退难解释得多。
- URL 只做去空白 + 合法性校验（与手动添加代理一致），不改写 scheme 大小写、也不补齐
  或裁掉尾斜杠。入表和写进账号 `proxy_url` 的是同一个字符串，两边恒等——差一个尾斜杠
  就会被当成两个不同的代理，账号照样 fail-closed，所以这里的关键不是"规范成什么样"，
  而是"两边必须一模一样"。同一个代理的不同写法会各自入表一行，与手动添加的行为相同。
- 格式非法的条目跳过、对应账号退回表单代理，不会因为一条烂代理毁掉整批导入。
- 入表走 `ON CONFLICT (url) DO NOTHING`。目标端已存在的同 URL 代理**不会**被复活、
  也不会被改标签；它若处于禁用或测试失败状态，绑定它的账号不会被调度，导入响应里
  会给出告警。
- 新注册的代理打上 `imported-<YYYYMMDD-HHmm>` 标签，事后可在代理管理页按批筛选清理。
- 代理注册早于账号写入，所以**即使这一批账号全部导入失败**（凭据不合法等），代理仍会
  留在代理表里。这是上述顺序的必然结果，不是缺陷；按 `imported-` 标签清理即可。

### 启用状态与既有绑定

- 源端标记为禁用的代理**一律以启用态导入**，并在响应里告警。照搬禁用状态会让绑定
  它们的账号立刻 fail-closed，用户看到的是「导入成功但账号全废」；以启用态导入 +
  明确告知，信息没丢，要禁用由操作员自己去点。
- 导入命中目标端已有账号时，文件带来的代理**不覆盖**该账号已有的绑定，只填补空绑定。
  文件里的代理是被动数据，目标端的绑定可能已经做过精细分配。表单填写的 `proxy_url`
  是操作员的显式换绑意图，维持既有的覆盖语义。

### 不随账号导出的部分

只绑到分组、由分组下发的代理不属于账号自身的绑定，账号导出不携带，目标端需要先建好
同名分组。

## 与 Store 集成

项目提供了 `EnhancedProxyPool` 和 `StoreProxyPoolIntegration` 用于与现有 Store 集成：

```go
// 创建增强代理池
enhancedPool := auth.NewEnhancedProxyPool(db, config)

// 初始化
err := enhancedPool.Init(ctx)

// 获取代理
proxyURL := enhancedPool.NextProxy()

// 标记成功/失败
enhancedPool.MarkProxySuccess(proxyURL)
enhancedPool.MarkProxyFailure(proxyURL)
```

## 文件列表

- `auth/proxy_pool.go` - 核心代理池实现
- `auth/proxy_pool_test.go` - 代理池单元测试
- `auth/proxy_pool_integration.go` - Store 集成实现
- `auth/proxy_pool_integration_test.go` - 集成测试

## 测试

运行测试：

```bash
cd /path/to/project
go test -v ./auth/
```

检查覆盖率：

```bash
go test -cover ./auth/
```
