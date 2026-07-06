package auth

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

const premium5hFallbackWindow = 5 * time.Hour

// NormalizePlanType canonicalizes a plan string for behavior-level comparisons.
// OpenAI reports the $100 Pro tier as "prolite"; functionally it is a Pro plan
// with a smaller usage cap, so we fold it into "pro" so that downstream plan
// gating (premium 5h rate-limit, Spark routing, scheduler bias, 429 cooldown
// window) treats it identically. The raw value is kept in Account.PlanType so
// the UI can still render "prolite" for operator visibility.
func NormalizePlanType(plan string) string {
	normalized := strings.ToLower(strings.TrimSpace(plan))
	switch normalized {
	case "prolite", "pro_lite", "pro-lite":
		return "pro"
	default:
		return normalized
	}
}

func normalizePlanType(plan string) string {
	return NormalizePlanType(plan)
}

// isPremium5hPlan reports whether the plan carries a rolling 5h usage window
// (premium 5h rate-limit semantics). Free-tier plans only have the 7d window.
// Education (k12/edu) and other paid workspace plans do have the 5h window —
// excluding them made 429'd k12 accounts show as available and let the wham
// usage probe clear their cooldown while still limited (issue #306/#309).
// The plan gate errs broad on purpose: MarkPremium5hRateLimited and
// premium5hRateLimitedLocked additionally require an actually observed 5h
// window at 100%, so a plan without a real 5h window can never get stuck.
func isPremium5hPlan(plan string) bool {
	switch normalizePlanType(plan) {
	case "plus", "pro", "team", "k12", "edu", "education", "go":
		return true
	default:
		return IsPlusOrHigherPlan(plan)
	}
}

// IsPlusOrHigherPlan reports whether a plan should be treated as paid for
// image-generation routing. Keep this broader than premium 5h rate-limit
// semantics so variants such as teamplus/enterprise can be preferred too.
func IsPlusOrHigherPlan(plan string) bool {
	normalized := normalizePlanType(plan)
	if normalized == "" || normalized == "free" {
		return false
	}
	switch normalized {
	case "plus", "pro", "team", "teamplus", "enterprise", "business", "edu", "education", "k12", "go":
		return true
	default:
		return strings.Contains(normalized, "plus") ||
			strings.HasPrefix(normalized, "pro") ||
			strings.HasPrefix(normalized, "team") ||
			strings.Contains(normalized, "enterprise") ||
			strings.Contains(normalized, "business")
	}
}

// IsPremium5hPlan 判断当前账号是否属于 premium 5h 限流语义范围。
func (a *Account) IsPremium5hPlan() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return isPremium5hPlan(a.PlanType)
}

func (a *Account) premium5hRateLimitedLocked(now time.Time) bool {
	if a.creditSkipsUsageWindowLocked() {
		return false
	}
	if !isPremium5hPlan(a.PlanType) {
		return false
	}
	if !a.UsagePercent5hValid || a.UsagePercent5h < 100 {
		return false
	}
	if a.Reset5hAt.IsZero() {
		return false
	}
	return a.Reset5hAt.After(now)
}

// IsPremium5hRateLimited 判断账号当前是否处于 premium 5h 限流态。
func (a *Account) IsPremium5hRateLimited() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.premium5hRateLimitedLocked(time.Now())
}

// GetUsageSnapshot5h 返回当前 5h 用量快照。
func (a *Account) GetUsageSnapshot5h() (pct float64, resetAt time.Time, ok bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.UsagePercent5hValid {
		return 0, time.Time{}, false
	}
	return a.UsagePercent5h, a.Reset5hAt, true
}

// PersistUsageSnapshot5hOnly 持久化仅包含 5h 数据的用量快照。
func (s *Store) PersistUsageSnapshot5hOnly(acc *Account) {
	if acc == nil || s == nil {
		return
	}

	pct5h, reset5hAt, ok := acc.GetUsageSnapshot5h()
	if !ok {
		return
	}

	updatedAt := time.Now()
	acc.mu.Lock()
	acc.UsageUpdatedAt5h = updatedAt
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateUsageSnapshot5h(ctx, acc.DBID, pct5h, reset5hAt, updatedAt); err != nil {
		log.Printf("[账号 %d] 持久化 5h 用量快照失败: %v", acc.DBID, err)
	}
}

// MarkPremium5hRateLimited 将账号标记为 premium 5h 限流态，并按 resetAt 驱动恢复。
func (s *Store) MarkPremium5hRateLimited(acc *Account, resetAt time.Time) {
	if acc == nil || s == nil {
		return
	}

	now := time.Now()
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(premium5hFallbackWindow)
	}

	acc.mu.Lock()
	acc.UsagePercent5h = 100
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = resetAt
	acc.UsageUpdatedAt5h = now
	acc.LastRateLimitedAt = now
	acc.Status = StatusCooldown
	acc.CooldownUtil = resetAt
	acc.CooldownReason = "rate_limited"
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierRisky
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)
	s.setCachedAccountCooldown(acc.DBID, "rate_limited", resetAt)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.SetCooldown(ctx, acc.DBID, "rate_limited", resetAt); err != nil {
		log.Printf("[账号 %d] 持久化 premium 5h 限流冷却状态失败: %v", acc.DBID, err)
	}
	if err := s.db.UpdateUsageSnapshot5h(ctx, acc.DBID, 100, resetAt, now); err != nil {
		log.Printf("[账号 %d] 持久化 premium 5h 限流快照失败: %v", acc.DBID, err)
	}
}
