# 账号锁定功能

## 功能说明

账号锁定功能保护重要账号不被自动删除。

**锁定账号**：
- 自有重要账号
- 长期使用账号
- 不会被自动清理删除

**未锁定账号**：
- 注册机临时账号
- 可被自动清理删除

## 使用方法

### Web界面操作

1. **锁定单个账号**：
   - 进入账号管理页面
   - 点击账号的"锁定"按钮
   - 账号显示"已锁定"标签

2. **解锁单个账号**：
   - 点击账号的"解锁"按钮
   - 账号可被自动清理删除

3. **批量锁定**：
   - 选择多个账号
   - 点击"批量锁定"按钮
   - 所有选中账号被锁定

### API接口

#### 锁定账号
```bash
POST /admin/accounts/:id/lock
```

响应：
```json
{
  "success": true,
  "message": "账号已锁定，不会被自动删除"
}
```

#### 解锁账号
```bash
POST /admin/accounts/:id/unlock
```

响应：
```json
{
  "success": true,
  "message": "账号已解锁，可以被自动删除"
}
```

#### 批量锁定/解锁
```bash
POST /admin/accounts/batch-lock

{
  "account_ids": [1, 2, 3],
  "locked": true
}
```

响应：
```json
{
  "success": true
}
```

## 自动清理保护

锁定账号在以下清理场景中被保护：

- **auto_clean_unauthorized** (401账号) - 锁定账号不被删除
- **auto_clean_rate_limited** (限速账号) - 锁定账号不被删除
- **auto_clean_full_usage** (额度用完) - 锁定账号不被删除
- **auto_clean_error** (错误账号) - 锁定账号不被删除
- **auto_clean_expired** (过期账号) - 锁定账号不被删除

## 技术实现

### 核心机制
- **原子操作**：并发安全，使用atomic.LoadInt32/StoreInt32
- **数据库持久化**：locked字段存储在accounts表（INTEGER DEFAULT 0）
- **向后兼容**：默认值0，现有账号自动标记为未锁定

### Store层方法
```go
// 设置锁定状态（原子操作 + 持久化）
func (s *Store) SetLocked(dbID int64, locked bool) error

// 检查锁定状态（原子读取）
func (acc *Account) IsLocked() bool
```

### Database层方法
```go
// 持久化锁定状态（PostgreSQL/SQLite）
func (db *DB) SetAccountLocked(ctx context.Context, accountID int64, locked bool) error
```

### Admin API接口
- `POST /admin/accounts/:id/lock` - 锁定单个账号
- `POST /admin/accounts/:id/unlock` - 解锁单个账号
- `POST /admin/accounts/batch-lock` - 批量锁定/解锁

### 自动清理修改
- `RemoveAccounts`: 检查locked标志，跳过锁定账号
- `CleanExpiredAccounts`: 跳过锁定账号

## 数据流程

```
用户操作
  ↓
前端: 点击"锁定"按钮
  ↓
API: POST /admin/accounts/:id/lock
  ↓
Handler: LockAccount()
  ↓
Store: SetLocked(id, true)
  ↓
Database: UPDATE accounts SET locked=1
  ↓
Account.Locked = 1 (原子标志)
  ↓
自动清理触发
  ↓
检查: if acc.IsLocked() { skip }
  ↓
锁定账号被保留 ✅
```

## 注意事项

- 锁定标志只影响自动清理，不影响手动删除
- 解锁后账号可被任何清理场景删除
- 锁定状态立即生效，无需重启服务
- 批量操作支持最多选中账号数量不限

## 测试验证

### 单元测试
- TestAccount_IsLocked - 原子读取测试
- TestStore_SetLocked - 设置锁定状态测试
- TestStore_SetLocked_NotFound - 不存在账号测试
- TestRemoveAccounts_SkipLocked - RemoveAccounts跳过测试
- TestCleanExpiredAccounts_SkipLocked - 过期清理跳过测试

### 集成测试
- TestHandler_LockAccount - 锁定接口测试
- TestHandler_UnlockAccount - 解锁接口测试
- TestHandler_BatchLockAccount - 批量锁定测试
- TestHandler_LockAccount_NotFound - 不存在账号测试

### E2E测试
- TestAccountLock_E2E - 完整生命周期测试
- TestBatchLock_E2E - 批量锁定流程测试

## 文件变更

| 文件 | 变更内容 |
|------|---------|
| database/postgres.go | PostgreSQL迁移 + SetAccountLocked方法 |
| database/sqlite.go | SQLite迁移 + setAccountLockedSQLite方法 |
| auth/store.go | Locked字段 + SetLocked/IsLocked方法 + 清理逻辑修改 |
| admin/handler.go | 3个API接口 + accountResponse.Locked字段 |
| frontend/src/types.ts | AccountRow.locked字段 |
| frontend/src/api.ts | 3个API方法 |
| frontend/src/pages/Accounts.tsx | UI组件 + 事件处理 |
| auth/store_lock_test.go | 单元测试（5个） |
| admin/handler_lock_test.go | 集成测试（4个） |
| integration/account_lock_e2e_test.go | E2E测试（2个） |

**总代码**: 270行新增（172后端 + 98前端）
**总测试**: 11个测试用例

---

**实施日期**: 2026-03-31
**状态**: COMPLETED ✅
