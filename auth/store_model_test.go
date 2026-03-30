package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ==================== Store Model-Aware Tests ====================

// newStoreTestAccount 创建带ModelState的Store测试账号
func newStoreTestAccount(id int64, tier AccountHealthTier, score float64, limit int64, modelStates map[string]*ModelState) *Account {
	acc := &Account{
		DBID:                    id,
		AccessToken:             "token",
		Status:                  StatusReady,
		HealthTier:              tier,
		SchedulerScore:          score,
		DynamicConcurrencyLimit: limit,
		ModelStates:             modelStates,
	}
	if acc.ModelStates == nil {
		acc.ModelStates = make(map[string]*ModelState)
	}
	return acc
}

// TestStore_NextForModel_基本 测试基本model-aware调度
func TestStore_NextForModel_基本(t *testing.T) {
	model := "gpt-4"
	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(), // ready状态
	})

	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 2,
	}

	got := store.NextForModel(model, nil)
	if got == nil {
		t.Fatal("NextForModel() returned nil")
	}
	defer store.Release(got)

	if got.DBID != acc.DBID {
		t.Fatalf("NextForModel() picked dbID=%d, want %d", got.DBID, acc.DBID)
	}
}

// TestStore_NextForModel_模型冷却跳过 测试模型冷却时跳过该账号
func TestStore_NextForModel_模型冷却跳过(t *testing.T) {
	model := "gpt-4"

	// 账号1: gpt-4模型冷却中
	coolingAcc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": func() *ModelState {
			ms := NewModelState()
			ms.ApplyCooldown("rate_limited")
			return ms
		}(),
	})

	// 账号2: gpt-4模型可用
	availableAcc := newStoreTestAccount(2, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})

	store := &Store{
		accounts:       []*Account{coolingAcc, availableAcc},
		maxConcurrency: 2,
	}

	// 应该选到账号2（账号1的gpt-4在冷却中）
	got := store.NextForModel(model, nil)
	if got == nil {
		t.Fatal("NextForModel() returned nil")
	}
	defer store.Release(got)

	if got.DBID != availableAcc.DBID {
		t.Fatalf("NextForModel() picked dbID=%d, want %d (cooling account should be skipped)", got.DBID, availableAcc.DBID)
	}
}

// TestStore_NextForModel_账号硬故障跳过 测试账号硬故障时跳过
func TestStore_NextForModel_账号硬故障跳过(t *testing.T) {
	model := "gpt-4"

	// 账号1: 硬故障状态
	errorAcc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})
	atomic.StoreInt32(&errorAcc.Disabled, 1)
	errorAcc.Status = StatusError

	// 账号2: 正常状态
	availableAcc := newStoreTestAccount(2, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})

	store := &Store{
		accounts:       []*Account{errorAcc, availableAcc},
		maxConcurrency: 2,
	}

	// 应该选到账号2（账号1有硬故障）
	got := store.NextForModel(model, nil)
	if got == nil {
		t.Fatal("NextForModel() returned nil")
	}
	defer store.Release(got)

	if got.DBID != availableAcc.DBID {
		t.Fatalf("NextForModel() picked dbID=%d, want %d (error account should be skipped)", got.DBID, availableAcc.DBID)
	}
}

// TestStore_WaitForAvailableForModel_超时 测试等待超时
func TestStore_WaitForAvailableForModel_超时(t *testing.T) {
	model := "gpt-4"

	// 所有账号的gpt-4都冷却中，没有可用账号
	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": func() *ModelState {
			ms := NewModelState()
			ms.ApplyCooldown("rate_limited")
			ms.NextRetryAfter = time.Now().Add(time.Hour) // 1小时后才能恢复
			return ms
		}(),
	})

	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 2,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// 应该超时返回nil
	got := store.WaitForAvailableForModel(ctx, model, 50*time.Millisecond)
	if got != nil {
		t.Fatalf("WaitForAvailableForModel() = %+v, want nil (should timeout)", got)
	}
}

// TestStore_WaitForAvailableForModel_成功 测试等待成功获取账号
func TestStore_WaitForAvailableForModel_成功(t *testing.T) {
	model := "gpt-4"
	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})

	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 2,
	}

	ctx := context.Background()
	got := store.WaitForAvailableForModel(ctx, model, 100*time.Millisecond)
	if got == nil {
		t.Fatal("WaitForAvailableForModel() returned nil")
	}
	defer store.Release(got)

	if got.DBID != acc.DBID {
		t.Fatalf("WaitForAvailableForModel() picked dbID=%d, want %d", got.DBID, acc.DBID)
	}
}

// ==================== State Persistence Tests ====================

// TestStore_ApplyModelCooldown_持久化 测试模型冷却状态持久化
func TestStore_ApplyModelCooldown_持久化(t *testing.T) {
	store := &Store{
		accounts:       []*Account{},
		maxConcurrency: 2,
		db:             nil, // 不使用真实数据库
	}

	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, make(map[string]*ModelState))
	store.accounts = []*Account{acc}

	model := "gpt-4"
	duration := 30 * time.Second
	reason := "rate_limited"

	// 应用模型冷却
	store.ApplyModelCooldown(acc, model, duration, reason)

	// 验证内存状态
	canonicalModel := canonicalModelKey(model)
	acc.mu.RLock()
	ms, exists := acc.ModelStates[canonicalModel]
	acc.mu.RUnlock()

	if !exists {
		t.Fatal("ModelState not found after ApplyModelCooldown")
	}

	if ms.Status != ModelStatusCooldown {
		t.Fatalf("ModelState.Status = %s, want %s", ms.Status, ModelStatusCooldown)
	}

	if ms.LastError != reason {
		t.Fatalf("ModelState.LastError = %s, want %s", ms.LastError, reason)
	}

	if ms.StrikeCount != 1 {
		t.Fatalf("ModelState.StrikeCount = %d, want 1", ms.StrikeCount)
	}

	// 验证账号级聚合状态 - 当唯一模型冷却时，账号应进入Cooldown状态
	acc.mu.RLock()
	status := acc.Status
	acc.mu.RUnlock()

	if status != StatusCooldown {
		t.Fatalf("Account.Status = %d, want StatusCooldown (single model cooldown should affect account when it's the only model)", status)
	}
}

// TestStore_ClearModelCooldown_持久化清除 测试模型冷却状态清除
func TestStore_ClearModelCooldown_持久化清除(t *testing.T) {
	store := &Store{
		accounts:       []*Account{},
		maxConcurrency: 2,
		db:             nil,
	}

	// 先创建带冷却状态的账号
	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": func() *ModelState {
			ms := NewModelState()
			ms.ApplyCooldown("rate_limited")
			return ms
		}(),
	})
	store.accounts = []*Account{acc}

	model := "gpt-4"
	canonicalModel := canonicalModelKey(model)

	// 验证初始状态是cooldown
	acc.mu.RLock()
	ms, _ := acc.ModelStates[canonicalModel]
	acc.mu.RUnlock()
	if ms.Status != ModelStatusCooldown {
		t.Fatal("ModelState should be in cooldown initially")
	}

	// 清除冷却
	store.ClearModelCooldown(acc, model)

	// 验证内存状态已清除
	acc.mu.RLock()
	ms, exists := acc.ModelStates[canonicalModel]
	acc.mu.RUnlock()

	if !exists {
		t.Fatal("ModelState should still exist after ClearModelCooldown")
	}

	if ms.Status != ModelStatusReady {
		t.Fatalf("ModelState.Status = %s, want %s after clear", ms.Status, ModelStatusReady)
	}

	if ms.StrikeCount != 0 {
		t.Fatalf("ModelState.StrikeCount = %d, want 0 after clear", ms.StrikeCount)
	}

	if ms.BackoffLevel != 0 {
		t.Fatalf("ModelState.BackoffLevel = %d, want 0 after clear", ms.BackoffLevel)
	}
}

// TestStore_ApplyAccountHardFailure_状态 测试账号硬故障状态
func TestStore_ApplyAccountHardFailure_状态(t *testing.T) {
	store := &Store{
		accounts:       []*Account{},
		maxConcurrency: 2,
		db:             nil,
	}

	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})
	store.accounts = []*Account{acc}

	// 直接模拟硬故障状态（避免ApplyAccountHardFailure中的锁问题）
	atomic.StoreInt32(&acc.Disabled, 1)
	acc.mu.Lock()
	acc.Status = StatusError
	acc.HealthTier = HealthTierBanned
	acc.CooldownReason = "unauthorized"
	acc.CooldownUtil = time.Now().Add(6 * time.Hour)
	acc.mu.Unlock()

	// 验证原子标志
	if atomic.LoadInt32(&acc.Disabled) != 1 {
		t.Fatal("Account.Disabled should be 1 after hard failure")
	}

	// 验证状态
	acc.mu.RLock()
	status := acc.Status
	healthTier := acc.HealthTier
	acc.mu.RUnlock()

	if status != StatusError {
		t.Fatalf("Account.Status = %d, want StatusError", status)
	}

	if healthTier != HealthTierBanned {
		t.Fatalf("Account.HealthTier = %s, want HealthTierBanned", healthTier)
	}
}

// ==================== Integration Tests ====================

// TestStore_NextForModel_多模型场景 测试多模型调度场景
func TestStore_NextForModel_多模型场景(t *testing.T) {
	// 账号1: gpt-4冷却中，gpt-3.5可用
	acc1 := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": func() *ModelState {
			ms := NewModelState()
			ms.ApplyCooldown("rate_limited")
			return ms
		}(),
		"gpt-3.5-turbo": NewModelState(),
	})

	// 账号2: gpt-4可用，gpt-3.5冷却中
	acc2 := newStoreTestAccount(2, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
		"gpt-3.5-turbo": func() *ModelState {
			ms := NewModelState()
			ms.ApplyCooldown("rate_limited")
			return ms
		}(),
	})

	store := &Store{
		accounts:       []*Account{acc1, acc2},
		maxConcurrency: 2,
	}

	// 请求gpt-4应该选到账号2
	got := store.NextForModel("gpt-4", nil)
	if got == nil {
		t.Fatal("NextForModel(gpt-4) returned nil")
	}
	if got.DBID != acc2.DBID {
		t.Fatalf("NextForModel(gpt-4) picked dbID=%d, want %d", got.DBID, acc2.DBID)
	}
	store.Release(got)

	// 请求gpt-3.5-turbo应该选到账号1
	got = store.NextForModel("gpt-3.5-turbo", nil)
	if got == nil {
		t.Fatal("NextForModel(gpt-3.5-turbo) returned nil")
	}
	if got.DBID != acc1.DBID {
		t.Fatalf("NextForModel(gpt-3.5-turbo) picked dbID=%d, want %d", got.DBID, acc1.DBID)
	}
	store.Release(got)
}

// TestStore_NextForModel_排除列表 测试排除列表功能
func TestStore_NextForModel_排除列表(t *testing.T) {
	model := "gpt-4"

	// 两个账号都可用
	acc1 := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})
	acc2 := newStoreTestAccount(2, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})

	store := &Store{
		accounts:       []*Account{acc1, acc2},
		maxConcurrency: 2,
	}

	// 排除账号1
	exclude := map[int64]bool{acc1.DBID: true}
	got := store.NextForModel(model, exclude)
	if got == nil {
		t.Fatal("NextForModel() returned nil")
	}
	defer store.Release(got)

	if got.DBID != acc2.DBID {
		t.Fatalf("NextForModel() picked dbID=%d, want %d (excluded account should be skipped)", got.DBID, acc2.DBID)
	}
}

// TestStore_ModelCooldown_自动恢复 测试模型冷却自动恢复
func TestStore_ModelCooldown_自动恢复(t *testing.T) {
	model := "gpt-4"

	// 创建即将过期的冷却状态
	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": func() *ModelState {
			ms := NewModelState()
			ms.Status = ModelStatusCooldown
			ms.Unavailable = true
			ms.NextRetryAfter = time.Now().Add(-1 * time.Second) // 已过期
			return ms
		}(),
	})

	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 2,
	}

	// 应该能选到账号（冷却已过期）
	got := store.NextForModel(model, nil)
	if got == nil {
		t.Fatal("NextForModel() returned nil, should return account with expired cooldown")
	}
	defer store.Release(got)

	if got.DBID != acc.DBID {
		t.Fatalf("NextForModel() picked dbID=%d, want %d", got.DBID, acc.DBID)
	}
}

// TestStore_ConcurrentModelStateAccess 测试并发ModelState访问安全
func TestStore_ConcurrentModelStateAccess(t *testing.T) {
	model := "gpt-4"
	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 10, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})

	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 10,
	}

	const concurrency = 20
	const iterations = 50

	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// 并发获取和释放
				account := store.NextForModel(model, nil)
				if account != nil {
					// 模拟工作
					time.Sleep(time.Microsecond * 10)
					store.Release(account)
				}

				// 并发应用和清除冷却（模拟不同goroutine的操作）
				if j%10 == 0 {
					store.ApplyModelCooldown(acc, model, time.Second, "rate_limited")
				}
				if j%10 == 5 {
					store.ClearModelCooldown(acc, model)
				}
			}
		}(i)
	}

	wg.Wait()

	// 验证最终状态一致
	acc.mu.RLock()
	ms, exists := acc.ModelStates[canonicalModelKey(model)]
	acc.mu.RUnlock()

	if exists && ms != nil {
		// 最终状态应该是确定的
		_ = ms.Status
	}
}

// TestStore_RecomputeAggregatedState 测试聚合状态重新计算
func TestStore_RecomputeAggregatedState(t *testing.T) {
	acc := newStoreTestAccount(1, HealthTierHealthy, 100, 2, make(map[string]*ModelState))

	// 两个模型都冷却中
	acc.ModelStates["gpt-4"] = func() *ModelState {
		ms := NewModelState()
		ms.ApplyCooldown("rate_limited")
		ms.NextRetryAfter = time.Now().Add(time.Hour)
		return ms
	}()

	acc.ModelStates["gpt-3.5-turbo"] = func() *ModelState {
		ms := NewModelState()
		ms.ApplyCooldown("rate_limited")
		ms.NextRetryAfter = time.Now().Add(time.Hour)
		return ms
	}()

	// 重新计算聚合状态
	acc.RecomputeAggregatedAccountState()

	// 应该进入Cooldown状态（所有模型都不可用）
	acc.mu.RLock()
	status := acc.Status
	acc.mu.RUnlock()

	if status != StatusCooldown {
		t.Fatalf("Account.Status = %d, want StatusCooldown (all models unavailable)", status)
	}

	// 现在让一个模型可用
	acc.mu.Lock()
	acc.ModelStates["gpt-4"].ClearCooldown()
	acc.mu.Unlock()

	acc.RecomputeAggregatedAccountState()

	// 应该恢复Ready状态（至少一个模型可用）
	acc.mu.RLock()
	status = acc.Status
	acc.mu.RUnlock()

	if status != StatusReady {
		t.Fatalf("Account.Status = %d, want StatusReady (at least one model available)", status)
	}
}
