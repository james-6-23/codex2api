# Model State E2E 测试指南

## 测试概述

本文档描述了 `integration/model_state_e2e_test.go` 中的端到端测试，用于验证模型状态管理系统的完整行为。

## 测试文件位置

```
D:/Code/Projects/C2A/01-device-fingerprint/integration/model_state_e2e_test.go
```

## 运行测试

### 运行所有 E2E 测试

```bash
cd D:/Code/Projects/C2A/01-device-fingerprint
go test -v ./integration/ -run TestE2E
```

### 运行特定测试

```bash
# 多模型并发测试
go test -v ./integration/ -run TestE2E_MultiModelConcurrent

# 状态恢复测试
go test -v ./integration/ -run TestE2E_StateRecovery_AfterRestart

# 完整请求链测试
go test -v ./integration/ -run TestE2E_CompleteRequestChain

# 并发访问测试
go test -v ./integration/ -run TestE2E_ConcurrentModelAccess

# Admin API 测试
go test -v ./integration/ -run TestE2E_AdminAPI_ModelStates
```

### 带竞态检测的测试

```bash
go test -race -v ./integration/ -run TestE2E
```

## 测试详情

### 1. TestE2E_MultiModelConcurrent

**目的**: 验证不同模型之间的状态隔离

**场景**:
- 账号同时拥有 gpt-4 和 gpt-3.5-turbo 的状态
- 对 gpt-4 应用冷却（429错误）
- 验证 gpt-3.5-turbo 仍然可用

**验证点**:
- 模型 A 进入 cooldown 状态
- 模型 B 保持 ready 状态
- `NextForModel("gpt-3.5-turbo")` 能选到账号

### 2. TestE2E_StateRecovery_AfterRestart

**目的**: 验证模型状态在应用重启后能够恢复

**场景**:
- 应用冷却到 gpt-4
- 验证状态已持久化到数据库
- 模拟应用重启（重新创建 Store）
- 手动加载模型状态
- 验证状态已恢复

**注意**: 当前测试需要手动加载模型状态。Phase 1 实现将自动恢复状态。

**验证点**:
- 冷却状态写入数据库
- 重启后状态可读取
- LastError 等字段被保留

### 3. TestE2E_CompleteRequestChain

**目的**: 验证完整的请求生命周期

**场景**:
- 初始请求选择账号
- 模拟 429 响应，应用模型冷却
- 排除冷却账号，选择其他账号
- 等待冷却过期
- 清除冷却状态
- 再次选择原账号

**验证点**:
- 冷却正确应用
- 排除列表生效
- 冷却过期后自动恢复
- 清除冷却后账号可用

### 4. TestE2E_ConcurrentModelAccess

**目的**: 验证模型状态操作的线程安全

**场景**:
- 10 个 goroutine 并发应用冷却
- 5 个 goroutine 并发清除冷却
- 10 个 goroutine 并发读取状态

**验证点**:
- 无竞态条件
- 无死锁
- 最终状态一致

### 5. TestE2E_AdminAPI_ModelStates

**目的**: 验证 Admin API 对模型状态的查询和管理

**场景**:
- 创建多个测试账号
- 对不同模型应用不同冷却
- 通过 Store 查询账号列表
- 验证模型状态可见
- 通过数据库验证持久化
- 清除模型状态

**验证点**:
- 账号列表包含模型状态
- 数据库正确存储多模型状态
- 清除操作生效

### 6. TestE2E_ExponentialBackoff (辅助测试)

**目的**: 验证指数退避逻辑

**场景**:
- 连续多次应用冷却
- 观察 BackoffLevel 和 StrikeCount

**验证点**:
- 退避级别递增
- 打击计数累计

### 7. TestE2E_AggregatedAccountState (辅助测试)

**目的**: 验证账号级聚合状态

**场景**:
- 初始状态验证
- 单模型冷却
- 多模型冷却

**验证点**:
- 单模型冷却不触发账号级冷却
- 聚合逻辑正确运行

## 测试架构

### E2ETestSuite 结构

```go
type E2ETestSuite struct {
    db         *database.DB      // SQLite 数据库
    store      *auth.Store       // 认证存储
    tokenCache cache.TokenCache  // Token 缓存
    ctx        context.Context   // 测试上下文
}
```

### 测试辅助方法

- `SetupE2ETest(t *testing.T) *E2ETestSuite`: 创建测试套件，初始化真实数据库
- `Cleanup(t *testing.T)`: 清理资源，关闭数据库
- `CreateTestAccount(t *testing.T, name string) *auth.Account`: 创建测试账号
- `SimulateRestart(t *testing.T)`: 模拟应用重启

## 环境要求

- Go 1.21+
- SQLite3
- 无外部依赖（数据库为文件型）

## 注意事项

1. **Phase 0 限制**: 当前 `loadFromDB()` 不自动恢复 ModelStates，需要手动加载
2. **Phase 1 改进**: 重启后将自动恢复模型状态
3. **并发测试**: ConcurrentModelAccess 测试使用短时冷却避免测试过慢
4. **临时文件**: 测试使用 `t.TempDir()` 创建临时数据库，测试结束后自动清理

## 故障排查

### 测试超时

如果 `TestE2E_CompleteRequestChain` 超时，检查：
- 冷却时间是否被修改（默认 2 秒冷却 + 3 秒等待）
- 系统负载是否过高

### 竞态检测失败

如果 `-race` 检测到竞态：
- 检查 Account.mu 是否正确加锁
- 检查 Store 操作是否在锁内进行

### 数据库锁定

如果遇到 SQLite 数据库锁定：
- 确保没有其他进程占用测试数据库
- 检查 Cleanup 是否正确调用

## 扩展测试

添加新测试时，遵循以下模式：

```go
func TestE2E_YourTestName(t *testing.T) {
    suite := SetupE2ETest(t)
    defer suite.Cleanup(t)

    // 创建测试账号
    acc := suite.CreateTestAccount(t, "test_name")

    // 执行测试逻辑
    // ...

    // 验证结果
    if condition {
        t.Fatal("测试失败信息")
    }

    t.Log("测试通过信息")
}
```

## 相关文件

- `auth/model_state.go`: 模型状态定义
- `auth/store.go`: Store 实现
- `database/sqlite.go`: SQLite 存储实现
- `auth/fast_scheduler.go`: 快速调度器

## 更新日志

- 2026-03-30: 初始版本，7个测试用例
  - 5 个核心 E2E 测试
  - 2 个辅助测试（指数退避、聚合状态）
