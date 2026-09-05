package auth

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func TestStoreSkipsCachedAccountCooldown(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	primary := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	fallback.LastFailureAt = time.Now()
	store := &Store{
		accounts:       []*Account{primary, fallback},
		maxConcurrency: 1,
		tokenCache:     tokenCache,
	}
	store.setCachedAccountCooldown(primary.DBID, "rate_limited", time.Now().Add(time.Hour))

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != fallback.DBID {
		t.Fatalf("Next() picked dbID=%d, want %d", got.DBID, fallback.DBID)
	}
	if primary.IsAvailable() {
		t.Fatal("primary account should have been synchronized into cooldown")
	}
}

func TestFastSchedulerSkipsCachedAccountCooldown(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	primary := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	fallback.LastFailureAt = time.Now()
	store := &Store{
		accounts:       []*Account{primary, fallback},
		maxConcurrency: 1,
		tokenCache:     tokenCache,
	}
	store.SetFastSchedulerEnabled(true)
	store.setCachedAccountCooldown(primary.DBID, "rate_limited", time.Now().Add(time.Hour))

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != fallback.DBID {
		t.Fatalf("Next() picked dbID=%d, want %d", got.DBID, fallback.DBID)
	}
	if primary.IsAvailable() {
		t.Fatal("primary account should have been synchronized into cooldown")
	}
}

func TestCachedPremium5hCooldownHydratesRiskyTier(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	account := newFastSchedulerTestAccount(9, HealthTierHealthy, 100, 1)
	store := &Store{accounts: []*Account{account}, maxConcurrency: 1, tokenCache: tokenCache}
	store.setCachedAccountCooldown(account.DBID, "rate_limited_5h", time.Now().Add(time.Hour))

	if !store.accountHasCachedCooldown(account) {
		t.Fatal("accountHasCachedCooldown() = false, want true")
	}
	if got := account.GetSchedulerDebugSnapshot(1).HealthTier; got != string(HealthTierRisky) {
		t.Fatalf("hydrated health tier = %q, want %q", got, HealthTierRisky)
	}
}

func TestStoreSkipsCachedModelCooldown(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	primary := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	fallback.LastFailureAt = time.Now()
	store := &Store{
		accounts:       []*Account{primary, fallback},
		maxConcurrency: 1,
		tokenCache:     tokenCache,
	}
	store.setCachedModelCooldown(primary.DBID, ModelCooldown{
		Model:     "gpt-5.4",
		Reason:    "model_capacity",
		ResetAt:   time.Now().Add(time.Hour),
		UpdatedAt: time.Now(),
	})

	got := store.NextExcludingWithFilter(0, nil, store.WithModelCooldownFilter("GPT-5.4", nil))
	if got == nil {
		t.Fatal("NextExcludingWithFilter() returned nil")
	}
	defer store.Release(got)

	if got.DBID != fallback.DBID {
		t.Fatalf("NextExcludingWithFilter() picked dbID=%d, want %d", got.DBID, fallback.DBID)
	}
	if !primary.IsModelRateLimited("gpt-5.4") {
		t.Fatal("primary model cooldown should have been synchronized from runtime cache")
	}
}

func TestCooldownCacheWritesAndDeletes(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 1)
	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 1,
		tokenCache:     tokenCache,
	}

	store.MarkCooldown(acc, 5*time.Minute, "rate_limited")
	if _, ok, err := tokenCache.GetRuntime(context.Background(), accountCooldownCacheNamespace, accountCooldownRuntimeKey(acc.DBID)); err != nil || !ok {
		t.Fatalf("account cooldown runtime cache ok=%v err=%v, want ok", ok, err)
	}
	store.ClearCooldown(acc)
	if _, ok, err := tokenCache.GetRuntime(context.Background(), accountCooldownCacheNamespace, accountCooldownRuntimeKey(acc.DBID)); err != nil || ok {
		t.Fatalf("account cooldown runtime cache after clear ok=%v err=%v, want miss", ok, err)
	}

	store.MarkModelCooldown(acc, "gpt-5.4", 5*time.Minute, "model_capacity")
	if _, ok, err := tokenCache.GetRuntime(context.Background(), modelCooldownCacheNamespace, modelCooldownRuntimeKey(acc.DBID, "gpt-5.4")); err != nil || !ok {
		t.Fatalf("model cooldown runtime cache ok=%v err=%v, want ok", ok, err)
	}
	store.ClearModelCooldown(acc, "gpt-5.4")
	if _, ok, err := tokenCache.GetRuntime(context.Background(), modelCooldownCacheNamespace, modelCooldownRuntimeKey(acc.DBID, "gpt-5.4")); err != nil || ok {
		t.Fatalf("model cooldown runtime cache after clear ok=%v err=%v, want miss", ok, err)
	}
}

// ageModelCooldown 把已存的冷却记录往前拨，模拟「冷却早已过期、但不久前刚失败过」。
func ageModelCooldown(acc *Account, model string, resetAgo, updatedAgo time.Duration) {
	key := normalizeModelCooldownKey(model)
	acc.mu.Lock()
	defer acc.mu.Unlock()
	now := time.Now()
	cd := acc.ModelCooldowns[key]
	cd.ResetAt = now.Add(-resetAgo)
	cd.UpdatedAt = now.Add(-updatedAgo)
	acc.ModelCooldowns[key] = cd
}

// 低流量部署两次失败的间隔常常比冷却本身还长。退避档位不能因为「冷却已过期」
// 就永远停在第一级——否则退避形同虚设，账号会以固定的基础间隔反复撞上游。
func TestModelCooldownBackoffSurvivesSparseTraffic(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 12}
	const model = "gpt-5.6-sol"
	const base = 5 * time.Minute

	first := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if first.BackoffLevel != 0 {
		t.Fatalf("first BackoffLevel = %d, want 0", first.BackoffLevel)
	}

	// 冷却已过期 5 分钟，但 10 分钟前刚失败过：连击仍在 TTL 内，档位必须继续升级。
	ageModelCooldown(acc, model, 5*time.Minute, 10*time.Minute)
	second := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if second.BackoffLevel != 1 {
		t.Fatalf("second BackoffLevel = %d, want 1 (streak must survive an expired cooldown)", second.BackoffLevel)
	}
	if remaining := time.Until(second.ResetAt); remaining <= base {
		t.Fatalf("second remaining = %v, want longer than the %v base", remaining, base)
	}

	// 再隔一轮同样的稀疏失败，档位继续推进。
	ageModelCooldown(acc, model, 5*time.Minute, 10*time.Minute)
	third := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if third.BackoffLevel != 2 {
		t.Fatalf("third BackoffLevel = %d, want 2", third.BackoffLevel)
	}
	if !third.ResetAt.After(second.ResetAt.Add(-time.Second)) {
		t.Fatalf("third cooldown should be at least as long as the second")
	}
}

// 超过连击 TTL 后回到第一级，避免旧档位无限期挂在账号上。
func TestModelCooldownBackoffResetsAfterStreakTTL(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 13}
	const model = "gpt-5.6-sol"
	const base = 5 * time.Minute

	store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	ageModelCooldown(acc, model, 5*time.Minute, 10*time.Minute)
	escalated := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if escalated.BackoffLevel != 1 {
		t.Fatalf("BackoffLevel = %d, want 1 before the TTL lapses", escalated.BackoffLevel)
	}

	// 上次失败已经是 TTL 之外的事了：连击断开，回到第一级与基础时长。
	ageModelCooldown(acc, model, 2*modelCooldownStreakTTL, 2*modelCooldownStreakTTL)
	revived := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if revived.BackoffLevel != 0 {
		t.Fatalf("BackoffLevel = %d, want 0 after the streak TTL lapsed", revived.BackoffLevel)
	}
	if remaining := time.Until(revived.ResetAt); remaining > base+time.Minute {
		t.Fatalf("remaining = %v, want back to the %v base", remaining, base)
	}
}

// 成功会删掉冷却记录，连击随之归零。
func TestModelCooldownBackoffResetsAfterSuccess(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 14}
	const model = "gpt-5.6-sol"
	const base = 5 * time.Minute

	store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	ageModelCooldown(acc, model, 5*time.Minute, time.Minute)
	if lvl := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true).BackoffLevel; lvl != 1 {
		t.Fatalf("BackoffLevel = %d, want 1", lvl)
	}

	store.ClearModelCooldown(acc, model)
	after := store.MarkModelCooldownWithBackoff(acc, model, base, "rate_limited_model", true)
	if after.BackoffLevel != 0 {
		t.Fatalf("BackoffLevel = %d, want 0 after a success cleared the cooldown", after.BackoffLevel)
	}
}

func TestModelCooldownFixedModeDoesNotBackoff(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 11}

	first := store.MarkModelCooldownWithBackoff(acc, "gpt-5.6-sol", 2*time.Second, "rate_limited_model", false)
	second := store.MarkModelCooldownWithBackoff(acc, "gpt-5.6-sol", 2*time.Second, "rate_limited_model", false)

	if first.Model == "" || second.Model == "" {
		t.Fatal("expected cooldown records")
	}
	if second.BackoffLevel != 0 {
		t.Fatalf("BackoffLevel = %d, want 0", second.BackoffLevel)
	}
	if remaining := time.Until(second.ResetAt); remaining < time.Second || remaining > 3*time.Second {
		t.Fatalf("remaining = %v, want fixed ~2s", remaining)
	}
	if cleared := store.ClearAllModelCooldowns(acc); cleared != 1 {
		t.Fatalf("ClearAllModelCooldowns() = %d, want 1", cleared)
	}
	if acc.IsModelRateLimited("gpt-5.6-sol") {
		t.Fatal("model cooldown should be cleared")
	}
}

// 管理端在库层清掉 unauthorized 后必须能把跨实例冷却缓存一起清掉，否则调度器
// 每次挑号回读缓存又把冷却盖回来。
func TestForgetCachedAccountCooldownDropsRecord(t *testing.T) {
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()

	account := newFastSchedulerTestAccount(11, HealthTierHealthy, 100, 1)
	store := &Store{accounts: []*Account{account}, maxConcurrency: 1, tokenCache: tokenCache}
	store.setCachedAccountCooldown(account.DBID, "unauthorized", time.Now().Add(time.Hour))
	if _, ok := store.getCachedAccountCooldown(account.DBID); !ok {
		t.Fatal("precondition: cached cooldown missing")
	}

	store.ForgetCachedAccountCooldown(account.DBID)

	if _, ok := store.getCachedAccountCooldown(account.DBID); ok {
		t.Fatal("cached cooldown should have been dropped")
	}
	if store.accountHasCachedCooldown(account) {
		t.Fatal("accountHasCachedCooldown() = true after forget, want false")
	}
}
