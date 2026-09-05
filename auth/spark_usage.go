package auth

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

// IsSparkUsagePlan reports whether the account should show a spark usage bar.
// prolite is folded into pro by NormalizePlanType.
func IsSparkUsagePlan(plan string) bool {
	return NormalizePlanType(plan) == "pro"
}

// SetUsageSnapshotSpark updates the independent spark 5h snapshot.
func (a *Account) SetUsageSnapshotSpark(pct float64, resetAt time.Time) {
	a.SetUsageSnapshotSparkAt(pct, resetAt, time.Now())
}

// SetUsageSnapshotSparkAt updates the independent spark 5h snapshot and its refresh time.
func (a *Account) SetUsageSnapshotSparkAt(pct float64, resetAt time.Time, updatedAt time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercentSpark = pct
	a.UsagePercentSparkValid = true
	a.ResetSparkAt = resetAt
	a.UsageUpdatedAtSpark = updatedAt
}

// GetUsagePercentSpark returns the spark usage percentage when a snapshot exists.
func (a *Account) GetUsagePercentSpark() (float64, bool) {
	if a == nil {
		return 0, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsagePercentSpark, a.UsagePercentSparkValid
}

// GetUsageSnapshotSpark returns the spark usage snapshot.
func (a *Account) GetUsageSnapshotSpark() (pct float64, resetAt time.Time, ok bool) {
	if a == nil {
		return 0, time.Time{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.UsagePercentSparkValid {
		return 0, time.Time{}, false
	}
	return a.UsagePercentSpark, a.ResetSparkAt, true
}

// GetResetSparkAt returns the spark window reset time.
func (a *Account) GetResetSparkAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ResetSparkAt
}

// SparkDispatchEligible reports whether a spark request may use this account.
// Account-level 5h/7d usage limits and usage-window cooldowns are ignored.
func (a *Account) SparkDispatchEligible() bool {
	if a == nil {
		return false
	}
	if atomic.LoadInt32(&a.Disabled) != 0 || atomic.LoadInt32(&a.DispatchPaused) != 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sparkDispatchEligibleLocked(time.Now())
}

func (a *Account) sparkDispatchEligibleLocked(now time.Time) bool {
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if !a.hasDispatchCredentialLocked() {
		return false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) && !isUsageLimitCooldownReason(a.CooldownReason) {
		return false
	}
	return !a.sparkUsageExhaustedLocked(now)
}

func (a *Account) sparkUsageExhaustedLocked(now time.Time) bool {
	if !a.UsagePercentSparkValid || a.UsagePercentSpark < 100 {
		return false
	}
	if a.ResetSparkAt.IsZero() {
		return true
	}
	return a.ResetSparkAt.After(now)
}

// SparkDispatchUsageLimited reports that the account is blocked only because
// the independent spark window is full. Used to return 429 instead of 503.
func (a *Account) SparkDispatchUsageLimited() bool {
	if a == nil || atomic.LoadInt32(&a.Disabled) != 0 || atomic.LoadInt32(&a.DispatchPaused) != 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned || !a.hasDispatchCredentialLocked() {
		return false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) && !isUsageLimitCooldownReason(a.CooldownReason) {
		return false
	}
	return a.sparkUsageExhaustedLocked(now)
}

func (a *Account) dispatchableForPolicy(policy DispatchPolicy, traces ...*SelectionTrace) bool {
	if policy == DispatchPolicySpark {
		eligible := a.SparkDispatchEligible()
		if !eligible {
			selectionTrace(traces).Reject("spark_account_unavailable")
		}
		return eligible
	}
	return a.IsAvailable(traces...)
}

func (a *Account) dispatchableForPolicyLocked(now time.Time, policy DispatchPolicy) bool {
	if policy == DispatchPolicySpark {
		return a.sparkDispatchEligibleLocked(now)
	}
	return a.isAvailableLocked(now)
}

func (a *Account) schedulerSnapshotForPolicy(baseLimit int64, policy DispatchPolicy) (AccountHealthTier, float64, float64, int64) {
	if policy != DispatchPolicySpark {
		return a.schedulerSnapshot(baseLimit)
	}
	now := time.Now()
	tier, score, limit, _, _ := a.fastSchedulerSnapshotForSpark(baseLimit, now)
	return tier, score, score, limit
}

// PersistUsageSnapshotSpark writes the spark snapshot to credentials.
func (s *Store) PersistUsageSnapshotSpark(acc *Account) {
	if acc == nil || s == nil {
		return
	}
	pct, resetAt, ok := acc.GetUsageSnapshotSpark()
	if !ok {
		return
	}
	updatedAt := time.Now()
	acc.mu.Lock()
	acc.UsageUpdatedAtSpark = updatedAt
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateUsageSnapshotSpark(ctx, acc.DBID, pct, resetAt, updatedAt); err != nil {
		log.Printf("[账号 %d] 持久化 spark 用量快照失败: %v", acc.DBID, err)
	}
}

// ClearAbsentUsageSnapshotSpark clears a stale spark snapshot when WHAM omits it.
func (s *Store) ClearAbsentUsageSnapshotSpark(acc *Account) bool {
	if acc == nil {
		return false
	}
	observedAt := time.Now()
	cleared := false
	acc.ApplyUsageObservation(observedAt, func() {
		cleared = s.ClearAbsentUsageSnapshotSparkAt(acc, observedAt)
	})
	return cleared
}

// ClearAbsentUsageSnapshotSparkAt clears the in-memory and persisted spark snapshot.
func (s *Store) ClearAbsentUsageSnapshotSparkAt(acc *Account, observedAt time.Time) bool {
	if acc == nil {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	acc.mu.Lock()
	if observedAt.Before(acc.usageObservedAt) {
		acc.mu.Unlock()
		return false
	}
	if !acc.UsagePercentSparkValid {
		acc.mu.Unlock()
		return false
	}
	acc.UsagePercentSpark = 0
	acc.UsagePercentSparkValid = false
	acc.ResetSparkAt = time.Time{}
	acc.UsageUpdatedAtSpark = time.Time{}
	if s != nil {
		acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	}
	acc.mu.Unlock()

	if s == nil {
		return true
	}
	s.fastSchedulerUpdate(acc)
	if s.db == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.ClearUsageSnapshotSpark(ctx, acc.DBID); err != nil {
		log.Printf("[账号 %d] 清除 spark 用量快照失败: %v", acc.DBID, err)
	}
	return true
}

func (s *Store) accountHasBlockingCachedCooldown(acc *Account, policy DispatchPolicy) bool {
	if !s.accountHasCachedCooldown(acc) {
		return false
	}
	if policy == DispatchPolicySpark && acc.SparkDispatchEligible() {
		return false
	}
	return true
}

// MarkSparkUsageExhausted records a spark usage_limit_reached without changing
// the account-level 5h/7d rate-limit status.
func (s *Store) MarkSparkUsageExhausted(acc *Account, resetAt time.Time) {
	if acc == nil {
		return
	}
	now := time.Now()
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(5 * time.Hour)
	}
	acc.SetUsageSnapshotSparkAt(100, resetAt, now)
	if s != nil {
		s.PersistUsageSnapshotSpark(acc)
	}
}
