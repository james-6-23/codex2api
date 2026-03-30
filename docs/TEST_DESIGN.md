# Codex2API 完整测试方案设计

**文档版本**: 1.0
**最后更新**: 2026-03-30
**适用范围**: 所有 Phase 的测试用例和验证方案

---

## 目录

1. [测试目标与范围](#1-测试目标与范围)
2. [测试策略总览](#2-测试策略总览)
3. [单元测试设计](#3-单元测试设计)
4. [集成测试设计](#4-集成测试设计)
5. [端到端测试设计](#5-端到端测试设计)
6. [Mock 设计](#6-mock-设计)
7. [测试数据准备](#7-测试数据准备)
8. [验证检查点](#8-验证检查点)
9. [测试执行计划](#9-测试执行计划)

---

## 1. 测试目标与范围

### 1.1 测试目标

| 目标 | 描述 |
|------|------|
| 功能正确性 | 验证所有 Phase 的核心功能按设计实现 |
| 边界条件 | 验证极端条件下的系统行为（空账号池、高并发、网络异常） |
| 错误处理 | 验证错误检测、分类、传播和恢复机制 |
| 并发安全 | 验证多 goroutine 环境下的数据一致性 |
| 性能基准 | 建立性能基线，防止回归 |

### 1.2 测试范围

```
┌─────────────────────────────────────────────────────────────────────────┐
│                            测试覆盖范围                                  │
├─────────────────────────────────────────────────────────────────────────┤
│  Phase 1: 模型容量错误检测                                               │
│  ├── ModelCapacityError 结构体                                          │
│  ├── isCodexModelCapacityError() 函数                                   │
│  ├── 错误消息解析（大小写、变体）                                          │
│  └── 与 429 错误的区分                                                    │
├─────────────────────────────────────────────────────────────────────────┤
│  Phase 2: 指数退避冷却                                                    │
│  ├── CooldownManager 结构体                                             │
│  ├── 指数退避计算 (1s, 2s, 4s, 8s...)                                    │
│  ├── 最大等级限制 (11级 = 30分钟)                                         │
│  ├── 冷却状态查询                                                       │
│  └── 冷却重置机制                                                       │
├─────────────────────────────────────────────────────────────────────────┤
│  Phase 3: Per-Model 状态隔离                                              │
│  ├── ModelState 结构体                                                  │
│  ├── StrikeCount 计数                                                   │
│  ├── 模型级别健康检查                                                   │
│  ├── 聚合状态推导 (AggregateState)                                       │
│  └── 模型级别冷却                                                       │
├─────────────────────────────────────────────────────────────────────────┤
│  Phase 4: 终端错误接口                                                    │
│  ├── Terminal() 接口设计                                                │
│  ├── 可重试 vs 不可重试错误分类                                           │
│  ├── 错误类型判断                                                       │
│  └── 错误传播链                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│  Phase 5: 调度器增强                                                      │
│  ├── FastScheduler 集成                                                 │
│  ├── 优先级队列                                                         │
│  ├── 模型过滤                                                           │
│  └── 账号选择策略                                                       │
├─────────────────────────────────────────────────────────────────────────┤
│  Phase 6: 429 处理流程                                                    │
│  ├── Handler 错误处理                                                   │
│  ├── 重试机制                                                           │
│  ├── 冷却触发                                                           │
│  └── 响应重写                                                           │
├─────────────────────────────────────────────────────────────────────────┤
│  Phase 7: 持久化                                                          │
│  ├── ModelState 存储/恢复                                               │
│  ├── StrikeCount 持久化                                                 │
│  └── 状态同步                                                           │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 2. 测试策略总览

### 2.1 测试金字塔

```
                    /\
                   /  \
                  / E2E\     (5 tests)   - 全链路场景测试
                 /______\
                /        \
               / Integration\  (15 tests) - 组件交互测试
              /______________\
             /                \
            /     Unit Tests    \ (35 tests) - 函数/方法测试
           /______________________\
```

### 2.2 测试类型分布

| 类型 | 数量 | 覆盖率目标 |
|------|------|-----------|
| 单元测试 | 35 | 80%+ |
| 集成测试 | 15 | 关键路径 |
| 端到端测试 | 5 | 核心场景 |
| **总计** | **55** | - |

---

## 3. 单元测试设计

### 3.1 ModelState 结构测试 (7 tests)

```go
// TestModelStateCreation 测试 ModelState 创建
func TestModelStateCreation(t *testing.T)

// TestModelStateStrikeCountIncrement 测试 StrikeCount 递增
func TestModelStateStrikeCountIncrement(t *testing.T)

// TestModelStateStrikeCountReset 测试 StrikeCount 重置
func TestModelStateStrikeCountReset(t *testing.T)

// TestModelStateCooldownTransition 测试冷却状态转换
func TestModelStateCooldownTransition(t *testing.T)

// TestModelStateHealthCheck 测试健康检查
func TestModelStateHealthCheck(t *testing.T)

// TestModelStateLastErrorTime 测试错误时间记录
func TestModelStateLastErrorTime(t *testing.T)

// TestModelStateConcurrentAccess 测试并发访问安全
func TestModelStateConcurrentAccess(t *testing.T)
```

**测试数据**:
```go
var modelStateTestCases = []struct {
    name        string
    modelID     string
    initialStrikes int
    expectedStrikes int
    cooldownLevel   int
}{
    {"fresh_model", "gpt-5.4", 0, 0, -1},
    {"single_strike", "gpt-5.4", 0, 1, 0},
    {"max_strikes", "gpt-5.4", 10, 10, 6}, // 达到上限
    {"reset_strikes", "gpt-5.4", 5, 0, -1},
}
```

### 3.2 聚合逻辑测试 (6 tests)

```go
// TestAggregateStateDeriveHealthy 测试健康状态推导
func TestAggregateStateDeriveHealthy(t *testing.T)

// TestAggregateStateDeriveDegraded 测试降级状态推导
func TestAggregateStateDeriveDegraded(t *testing.T)

// TestAggregateStateDeriveCritical 测试严重状态推导
func TestAggregateStateDeriveCritical(t *testing.T)

// TestAggregateStateFromSingleModel 测试单模型聚合
func TestAggregateStateFromSingleModel(t *testing.T)

// TestAggregateStateFromMultipleModels 测试多模型聚合
func TestAggregateStateFromMultipleModels(t *testing.T)

// TestAggregateStateEmptyPool 测试空账号池聚合
func TestAggregateStateEmptyPool(t *testing.T)
```

**测试数据**:
```go
var aggregateTestCases = []struct {
    name           string
    modelStates    map[string]*ModelState
    expectedState  AggregateState
    expectedHealth bool
}{
    {
        name: "all_healthy",
        modelStates: map[string]*ModelState{
            "gpt-5.4": {StrikeCount: 0, Status: ModelStatusHealthy},
            "o3-mini": {StrikeCount: 0, Status: ModelStatusHealthy},
        },
        expectedState:  AggregateStateHealthy,
        expectedHealth: true,
    },
    {
        name: "mixed_states",
        modelStates: map[string]*ModelState{
            "gpt-5.4": {StrikeCount: 5, Status: ModelStatusCooldown},
            "o3-mini": {StrikeCount: 0, Status: ModelStatusHealthy},
        },
        expectedState:  AggregateStateDegraded,
        expectedHealth: true,
    },
    {
        name: "all_critical",
        modelStates: map[string]*ModelState{
            "gpt-5.4": {StrikeCount: 10, Status: ModelStatusCooldown},
            "o3-mini": {StrikeCount: 8, Status: ModelStatusCooldown},
        },
        expectedState:  AggregateStateCritical,
        expectedHealth: false,
    },
}
```

### 3.3 指数退避冷却测试 (8 tests)

```go
// TestCooldownManagerCreation 测试冷却管理器创建
func TestCooldownManagerCreation(t *testing.T)

// TestCooldownManagerEnterCooldown 测试进入冷却
func TestCooldownManagerEnterCooldown(t *testing.T)

// TestCooldownManagerExponentialBackoff 测试指数退避序列
func TestCooldownManagerExponentialBackoff(t *testing.T)

// TestCooldownManagerMaxLevel 测试最大等级限制
func TestCooldownManagerMaxLevel(t *testing.T)

// TestCooldownManagerIsInCooldown 测试冷却状态查询
func TestCooldownManagerIsInCooldown(t *testing.T)

// TestCooldownManagerGetRemainingTime 测试剩余时间查询
func TestCooldownManagerGetRemainingTime(t *testing.T)

// TestCooldownManagerReset 测试冷却重置
func TestCooldownManagerReset(t *testing.T)

// TestCooldownManagerConcurrentAccess 测试并发安全
func TestCooldownManagerConcurrentAccess(t *testing.T)
```

**测试数据**:
```go
// 预期的退避序列
var expectedBackoffSequence = []struct {
    level    int
    duration time.Duration
}{
    {-1, 1 * time.Second},   // Level 0
    {0, 2 * time.Second},    // Level 1
    {1, 4 * time.Second},    // Level 2
    {2, 8 * time.Second},    // Level 3
    {3, 16 * time.Second},   // Level 4
    {4, 32 * time.Second},   // Level 5
    {5, 64 * time.Second},   // Level 6
    {6, 128 * time.Second},  // Level 7
    {7, 256 * time.Second},  // Level 8
    {8, 512 * time.Second},  // Level 9
    {9, 1024 * time.Second}, // Level 10
    {10, 1800 * time.Second}, // Level 11 (max, 30min)
    {100, 1800 * time.Second}, // Level > 11 capped
}
```

### 3.4 终端错误接口测试 (5 tests)

```go
// TestTerminalErrorDetection 测试终端错误检测
func TestTerminalErrorDetection(t *testing.T)

// TestRetryableErrorDetection 测试可重试错误检测
func TestRetryableErrorDetection(t *testing.T)

// TestCodexErrorTerminal 测试 CodexError 终端判断
func TestCodexErrorTerminal(t *testing.T)

// TestModelCapacityErrorTerminal 测试 ModelCapacityError 终端判断
func TestModelCapacityErrorTerminal(t *testing.T)

// TestWrappedErrorTerminal 测试包装错误终端判断
func TestWrappedErrorTerminal(t *testing.T)
```

**测试数据**:
```go
var terminalErrorTestCases = []struct {
    name     string
    err      error
    terminal bool
}{
    {"nil_error", nil, false},
    {"auth_error", ErrInvalidAuth(), true},
    {"rate_limit_429", ErrRateLimited("429"), false},
    {"model_capacity", ErrModelCapacity("gpt-5.4"), false},
    {"timeout", ErrTimeout(), false},
    {"server_error", ErrServerError(), false},
    {"client_error", ErrBadRequest(), true},
}
```

### 3.5 模型容量错误检测测试 (9 tests)

```go
// TestIsCodexModelCapacityErrorBasic 测试基础容量错误检测
func TestIsCodexModelCapacityErrorBasic(t *testing.T)

// TestIsCodexModelCapacityErrorVariants 测试变体消息
func TestIsCodexModelCapacityErrorVariants(t *testing.T)

// TestIsCodexModelCapacityErrorCaseInsensitive 测试大小写不敏感
func TestIsCodexModelCapacityErrorCaseInsensitive(t *testing.T)

// TestIsCodexModelCapacityErrorNegative 测试非容量错误
func TestIsCodexModelCapacityErrorNegative(t *testing.T)

// TestIsCodexModelCapacityErrorEdgeCases 测试边界情况
func TestIsCodexModelCapacityErrorEdgeCases(t *testing.T)

// TestModelCapacityErrorStruct 测试错误结构体
func TestModelCapacityErrorStruct(t *testing.T)

// TestModelCapacityErrorRetryAfter 测试 RetryAfter 计算
func TestModelCapacityErrorRetryAfter(t *testing.T)

// TestModelCapacityErrorError 测试 Error() 方法
func TestModelCapacityErrorError(t *testing.T)

// TestModelCapacityErrorTerminal 测试 Terminal() 方法
func TestModelCapacityErrorTerminal(t *testing.T)
```

**测试数据** (基于现有 handler_capacity_test.go 扩展):
```go
var capacityErrorTestCases = []struct {
    name     string
    body     string
    expected bool
    is429    bool // 是否是 429 错误
}{
    // 标准容量错误
    {"standard_message", `{"error": {"message": "Selected model is at capacity"}}`, true, false},
    {"alternate_message", `{"error": {"message": "Model is at capacity. Please try a different model"}}`, true, false},
    {"currently_message", `{"message": "Model is currently at capacity"}`, true, false},

    // 大小写变体
    {"uppercase", `{"error": {"message": "SELECTED MODEL IS AT CAPACITY"}}`, true, false},
    {"mixed_case", `{"error": {"message": "Selected Model Is At Capacity"}}`, true, false},

    // 非容量错误
    {"unrelated_error", `{"error": {"message": "Invalid request"}}`, false, false},
    {"rate_limit", `{"error": {"type": "usage_limit_reached", "message": "Rate limit exceeded"}}`, false, true},
    {"empty_body", ``, false, false},
    {"invalid_json", `{invalid`, false, false},

    // 边界情况
    {"partial_match", `{"error": {"message": "model is at"}}`, false, false},
    {"nested_error", `{"error": {"error": {"message": "Selected model is at capacity"}}}`, true, false},
}
```

---

## 4. 集成测试设计

### 4.1 模型隔离场景测试 (4 tests)

```go
// TestModelIsolationSingleModelFailure 测试单模型失败隔离
func TestModelIsolationSingleModelFailure(t *testing.T)

// TestModelIsolationMultipleModelsPartialFailure 测试部分模型失败
func TestModelIsolationMultipleModelsPartialFailure(t *testing.T)

// TestModelIsolationAllModelsFailure 测试所有模型失败
func TestModelIsolationAllModelsFailure(t *testing.T)

// TestModelIsolationRecovery 测试模型恢复
func TestModelIsolationRecovery(t *testing.T)
```

**测试场景**:
```
Scenario: 单模型失败隔离
  Given 账号池有 3 个账号
  And 所有账号都支持 gpt-5.4 和 o3-mini
  When gpt-5.4 连续返回容量错误
  Then gpt-5.4 应该被标记为冷却状态
  And o3-mini 仍然可用
  And 聚合状态应该为 Degraded
```

### 4.2 429 处理流程测试 (4 tests)

```go
// Test429TriggerCooldown 测试 429 触发冷却
func Test429TriggerCooldown(t *testing.T)

// Test429ExponentialBackoff 测试 429 指数退避
func Test429ExponentialBackoff(t *testing.T)

// Test429ModelSpecificCooldown 测试模型特定冷却
func Test429ModelSpecificCooldown(t *testing.T)

// Test429RetryWithBackoff 测试带退避的重试
func Test429RetryWithBackoff(t *testing.T)
```

**测试场景**:
```
Scenario: 429 触发指数退避冷却
  Given 账号 A 可用
  When 账号 A 收到 429 错误
  Then 账号 A 应该进入冷却状态
  And 冷却时长为 1 秒
  When 账号 A 再次收到 429 错误
  Then 冷却时长应该为 2 秒
  When 账号 A 第三次收到 429 错误
  Then 冷却时长应该为 4 秒
```

### 4.3 持久化测试 (3 tests)

```go
// TestModelStatePersistence 测试 ModelState 持久化
func TestModelStatePersistence(t *testing.T)

// TestStrikeCountPersistence 测试 StrikeCount 持久化
func TestStrikeCountPersistence(t *testing.T)

// TestStateRecoveryAfterRestart 测试重启后状态恢复
func TestStateRecoveryAfterRestart(t *testing.T)
```

**测试场景**:
```
Scenario: 状态持久化和恢复
  Given 账号 A 的 gpt-5.4 有 5 次 strike
  And 账号 A 处于 Level 2 冷却
  When 系统重启
  Then 应该恢复账号 A 的状态
  And gpt-5.4 的 strike count 应该为 5
  And 冷却状态应该保持
```

### 4.4 调度器集成测试 (4 tests)

```go
// TestFastSchedulerWithModelFilter 测试带模型过滤的调度
func TestFastSchedulerWithModelFilter(t *testing.T)

// TestFastSchedulerWithCooldownAccount 测试冷却账号过滤
func TestFastSchedulerWithCooldownAccount(t *testing.T)

// TestFastSchedulerPriorityQueue 测试优先级队列
func TestFastSchedulerPriorityQueue(t *testing.T)

// TestFastSchedulerConcurrentAcquire 测试并发获取
func TestFastSchedulerConcurrentAcquire(t *testing.T)
```

---

## 5. 端到端测试设计

### 5.1 完整请求处理流程 (3 tests)

```go
// TestE2ECapacityErrorRetry 测试容量错误重试流程
func TestE2ECapacityErrorRetry(t *testing.T)

// TestE2E429WithExponentialBackoff 测试 429 指数退避流程
func TestE2E429WithExponentialBackoff(t *testing.T)

// TestE2EModelSwitchOnCapacity 测试容量错误时模型切换
func TestE2EModelSwitchOnCapacity(t *testing.T)
```

**测试场景**:
```
Scenario: 容量错误触发模型切换
  Given 客户端请求 gpt-5.4
  And 账号池有可用账号
  When 第一个账号返回容量错误
  Then 应该自动重试第二个账号
  When 第二个账号也返回容量错误
  Then 应该自动重试第三个账号
  When 所有账号都返回容量错误
  Then 应该返回 503 错误
  And 响应应该包含 Retry-After 头部
```

### 5.2 并发场景测试 (2 tests)

```go
// TestE2EConcurrentRequests 测试并发请求
func TestE2EConcurrentRequests(t *testing.T)

// TestE2EStressTest 测试压力场景
func TestE2EStressTest(t *testing.T)
```

---

## 6. Mock 设计

### 6.1 Mock 组件清单

| Mock 组件 | 用途 | 实现方式 |
|-----------|------|----------|
| MockCodexServer | 模拟 Codex API 响应 | httptest.Server |
| MockStore | 模拟账号存储 | 接口实现 |
| MockDB | 模拟数据库 | sqlmock |
| MockCache | 模拟缓存 | 内存实现 |
| MockRateLimiter | 模拟限流器 | 接口实现 |
| MockProxyPool | 模拟代理池 | 接口实现 |

### 6.2 MockCodexServer 设计

```go
type MockCodexServer struct {
    Server      *httptest.Server
    Responses   []MockResponse
    CallCount   int
    Mutex       sync.Mutex
}

type MockResponse struct {
    StatusCode  int
    Body        string
    Headers     map[string]string
    Delay       time.Duration
}

// 预定义响应
var (
    MockCapacityError = MockResponse{
        StatusCode: http.StatusTooManyRequests,
        Body:       `{"error": {"message": "Selected model is at capacity"}}`,
        Headers:    map[string]string{"Retry-After": "30"},
    }

    Mock429Error = MockResponse{
        StatusCode: http.StatusTooManyRequests,
        Body:       `{"error": {"type": "rate_limit_reached", "message": "Rate limit exceeded"}}`,
        Headers:    map[string]string{"Retry-After": "60"},
    }

    MockSuccess = MockResponse{
        StatusCode: http.StatusOK,
        Body:       `{"id": "resp_123", "output": [{"type": "message", "content": "Hello"}]}`,
    }
)
```

### 6.3 MockStore 设计

```go
type MockStore struct {
    Accounts      []*auth.Account
    ModelStates   map[string]*ModelState
    mu            sync.RWMutex
}

func (m *MockStore) GetAccount(id int64) *auth.Account
func (m *MockStore) GetAllAccounts() []*auth.Account
func (m *MockStore) GetModelState(modelID string) *ModelState
func (m *MockStore) UpdateModelState(modelID string, state *ModelState)
func (m *MockStore) MarkCooldown(acc *auth.Account, duration time.Duration, reason string)
```

---

## 7. 测试数据准备

### 7.1 账号测试数据

```go
// 标准测试账号
var TestAccounts = []struct {
    ID          int64
    Email       string
    Token       string
    Tier        auth.AccountHealthTier
    MaxConcur   int64
    Status      auth.AccountStatus
}{
    {1, "acc1@test.com", "token_1", auth.HealthTierHealthy, 4, auth.StatusReady},
    {2, "acc2@test.com", "token_2", auth.HealthTierWarm, 2, auth.StatusReady},
    {3, "acc3@test.com", "token_3", auth.HealthTierRisky, 1, auth.StatusReady},
    {4, "acc4@test.com", "", auth.HealthTierHealthy, 4, auth.StatusReady},     // 无 token
    {5, "acc5@test.com", "token_5", auth.HealthTierBanned, 0, auth.StatusError}, // 被封禁
}

// 创建测试账号辅助函数
func NewTestAccount(id int64, tier auth.AccountHealthTier, maxConcur int64) *auth.Account {
    return &auth.Account{
        DBID:                    id,
        AccessToken:             fmt.Sprintf("token_%d", id),
        Status:                  auth.StatusReady,
        HealthTier:              tier,
        SchedulerScore:          100.0 - float64(id%11),
        DynamicConcurrencyLimit: maxConcur,
    }
}
```

### 7.2 模型测试数据

```go
// 支持的模型
var TestModels = []string{
    "gpt-5.4",
    "o3-mini",
    "o3",
    "o1-pro",
    "codex-mini",
}

// 模型状态测试数据
var TestModelStates = map[string]*ModelState{
    "gpt-5.4": {
        ModelID:      "gpt-5.4",
        StrikeCount:  0,
        Status:       ModelStatusHealthy,
        LastErrorAt:  time.Time{},
    },
    "o3-mini": {
        ModelID:      "o3-mini",
        StrikeCount:  3,
        Status:       ModelStatusCooldown,
        CooldownUntil: time.Now().Add(5 * time.Minute),
        LastErrorAt:  time.Now().Add(-1 * time.Minute),
    },
}
```

### 7.3 错误响应测试数据

```go
// 容量错误变体
var CapacityErrorVariants = []string{
    `{"error": {"message": "Selected model is at capacity"}}`,
    `{"error": {"message": "Model is at capacity. Please try a different model"}}`,
    `{"message": "Model is currently at capacity"}`,
    `{"error": {"message": "The selected model is at capacity, please try again later"}}`,
    `{"error": {"message": "Model gpt-5.4 is at capacity"}}`,
}

// 非容量错误
var NonCapacityErrors = []string{
    `{"error": {"message": "Invalid request"}}`,
    `{"error": {"type": "usage_limit_reached", "message": "Rate limit exceeded"}}`,
    `{"error": {"message": "Unauthorized"}}`,
    `{"error": {"message": "Internal server error"}}`,
}
```

---

## 8. 验证检查点

### 8.1 功能检查点

| 检查点 | 验证方法 | 通过标准 |
|--------|---------|---------|
| 容量错误检测 | 单元测试 | 所有变体正确识别 |
| 指数退避计算 | 单元测试 | 序列 1s->2s->4s->... |
| 最大等级限制 | 单元测试 | Level 11 = 30分钟 |
| StrikeCount 递增 | 单元测试 | 每次错误 +1 |
| 聚合状态推导 | 单元测试 | 按规则正确推导 |
| 终端错误判断 | 单元测试 | 正确区分可重试/不可重试 |

### 8.2 性能检查点

| 检查点 | 验证方法 | 通过标准 |
|--------|---------|---------|
| 调度器性能 | Benchmark | < 1us/op |
| 并发安全 | 压力测试 | 无竞态条件 |
| 内存使用 | 内存分析 | 无泄漏 |
| 响应时间 | E2E 测试 | P99 < 100ms (无冷却) |

### 8.3 集成检查点

| 检查点 | 验证方法 | 通过标准 |
|--------|---------|---------|
| 429->冷却流程 | 集成测试 | 正确触发冷却 |
| 模型隔离 | 集成测试 | 单模型失败不影响其他 |
| 状态持久化 | 集成测试 | 重启后状态恢复 |
| 调度器集成 | 集成测试 | 过滤和排序正确 |

---

## 9. 测试执行计划

### 9.1 执行顺序

```
Phase 1: 基础单元测试 (T+0)
  ├── ModelState 测试
  ├── CooldownManager 测试
  └── CapacityError 检测测试

Phase 2: 逻辑单元测试 (T+1)
  ├── 聚合逻辑测试
  ├── 终端错误接口测试
  └── 指数退避计算测试

Phase 3: 集成测试 (T+2)
  ├── 模型隔离场景测试
  ├── 429 处理流程测试
  └── 调度器集成测试

Phase 4: 持久化测试 (T+3)
  └── 状态持久化测试

Phase 5: 端到端测试 (T+4)
  ├── 完整请求流程测试
  └── 并发场景测试

Phase 6: 性能基准 (T+5)
  ├── Benchmark 测试
  └── 压力测试
```

### 9.2 CI/CD 集成

```yaml
# .github/workflows/test.yml 建议配置
test:
  strategy:
    matrix:
      test-type: [unit, integration, e2e]
  steps:
    - name: Run Tests
      run: |
        go test -v -race ./... -run "^Test" -count=1
        go test -bench=. -benchmem ./auth -run=^$
```

### 9.3 测试覆盖率目标

| 模块 | 目标覆盖率 | 关键文件 |
|------|-----------|---------|
| auth | 85% | store.go, fast_scheduler.go |
| proxy | 80% | handler.go, executor.go, ratelimit.go |
| cache | 75% | cache.go |
| database | 70% | sqlite.go |
| **总体** | **80%** | - |

---

## 附录 A: 测试用例清单 (55个)

### 单元测试 (35个)

| # | 模块 | 测试名称 | 类型 |
|---|------|---------|------|
| 1 | ModelState | TestModelStateCreation | Unit |
| 2 | ModelState | TestModelStateStrikeCountIncrement | Unit |
| 3 | ModelState | TestModelStateStrikeCountReset | Unit |
| 4 | ModelState | TestModelStateCooldownTransition | Unit |
| 5 | ModelState | TestModelStateHealthCheck | Unit |
| 6 | ModelState | TestModelStateLastErrorTime | Unit |
| 7 | ModelState | TestModelStateConcurrentAccess | Unit |
| 8 | Aggregate | TestAggregateStateDeriveHealthy | Unit |
| 9 | Aggregate | TestAggregateStateDeriveDegraded | Unit |
| 10 | Aggregate | TestAggregateStateDeriveCritical | Unit |
| 11 | Aggregate | TestAggregateStateFromSingleModel | Unit |
| 12 | Aggregate | TestAggregateStateFromMultipleModels | Unit |
| 13 | Aggregate | TestAggregateStateEmptyPool | Unit |
| 14 | Cooldown | TestCooldownManagerCreation | Unit |
| 15 | Cooldown | TestCooldownManagerEnterCooldown | Unit |
| 16 | Cooldown | TestCooldownManagerExponentialBackoff | Unit |
| 17 | Cooldown | TestCooldownManagerMaxLevel | Unit |
| 18 | Cooldown | TestCooldownManagerIsInCooldown | Unit |
| 19 | Cooldown | TestCooldownManagerGetRemainingTime | Unit |
| 20 | Cooldown | TestCooldownManagerReset | Unit |
| 21 | Cooldown | TestCooldownManagerConcurrentAccess | Unit |
| 22 | Terminal | TestTerminalErrorDetection | Unit |
| 23 | Terminal | TestRetryableErrorDetection | Unit |
| 24 | Terminal | TestCodexErrorTerminal | Unit |
| 25 | Terminal | TestModelCapacityErrorTerminal | Unit |
| 26 | Terminal | TestWrappedErrorTerminal | Unit |
| 27 | Capacity | TestIsCodexModelCapacityErrorBasic | Unit |
| 28 | Capacity | TestIsCodexModelCapacityErrorVariants | Unit |
| 29 | Capacity | TestIsCodexModelCapacityErrorCaseInsensitive | Unit |
| 30 | Capacity | TestIsCodexModelCapacityErrorNegative | Unit |
| 31 | Capacity | TestIsCodexModelCapacityErrorEdgeCases | Unit |
| 32 | Capacity | TestModelCapacityErrorStruct | Unit |
| 33 | Capacity | TestModelCapacityErrorRetryAfter | Unit |
| 34 | Capacity | TestModelCapacityErrorError | Unit |
| 35 | Capacity | TestModelCapacityErrorTerminal | Unit |

### 集成测试 (15个)

| # | 模块 | 测试名称 | 类型 |
|---|------|---------|------|
| 36 | Isolation | TestModelIsolationSingleModelFailure | Integration |
| 37 | Isolation | TestModelIsolationMultipleModelsPartialFailure | Integration |
| 38 | Isolation | TestModelIsolationAllModelsFailure | Integration |
| 39 | Isolation | TestModelIsolationRecovery | Integration |
| 40 | 429Flow | Test429TriggerCooldown | Integration |
| 41 | 429Flow | Test429ExponentialBackoff | Integration |
| 42 | 429Flow | Test429ModelSpecificCooldown | Integration |
| 43 | 429Flow | Test429RetryWithBackoff | Integration |
| 44 | Persistence | TestModelStatePersistence | Integration |
| 45 | Persistence | TestStrikeCountPersistence | Integration |
| 46 | Persistence | TestStateRecoveryAfterRestart | Integration |
| 47 | Scheduler | TestFastSchedulerWithModelFilter | Integration |
| 48 | Scheduler | TestFastSchedulerWithCooldownAccount | Integration |
| 49 | Scheduler | TestFastSchedulerPriorityQueue | Integration |
| 50 | Scheduler | TestFastSchedulerConcurrentAcquire | Integration |

### 端到端测试 (5个)

| # | 模块 | 测试名称 | 类型 |
|---|------|---------|------|
| 51 | E2E | TestE2ECapacityErrorRetry | E2E |
| 52 | E2E | TestE2E429WithExponentialBackoff | E2E |
| 53 | E2E | TestE2EModelSwitchOnCapacity | E2E |
| 54 | E2E | TestE2EConcurrentRequests | E2E |
| 55 | E2E | TestE2EStressTest | E2E |

---

**文档结束**
