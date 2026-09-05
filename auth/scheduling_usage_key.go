package auth

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// 调度模式（remaining_quota / fill_first / round_robin 的 healthy 桶）的排序键是
// "账号已用配额百分比"。各渠道的数据来源不同：
//   - Codex / Claude：5h/7d 用量窗口（wham 探针、响应头）。
//   - Grok：免费额度耗尽 429 里的权威 tokens 用量，其次是逐请求 x-ratelimit-* 余量头。
//   - Antigravity：控制面同步的配额快照（quota_groups 桶或模型级剩余比例）。
//
// 观测过期后视为未知（0），避免陈旧快照把已恢复的账号长期压在队尾。
const (
	grokSchedulingRateLimitTTL    = 6 * time.Hour
	grokSchedulingFreeQuotaTTL    = 24 * time.Hour
	antigravitySchedulingQuotaTTL = 24 * time.Hour
)

// usagePercentForScheduling 返回调度排序用的已用配额百分比。
func (a *Account) usagePercentForScheduling() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.usagePercentForSchedulingLocked(time.Now())
}

func (a *Account) usagePercentForSchedulingLocked(now time.Time) float64 {
	switch {
	case a.isGrokAPILocked():
		return a.grokUsagePercentForSchedulingLocked(now)
	case a.isAntigravityAPILocked():
		return a.antigravityUsagePercentForSchedulingLocked(now)
	}
	if a.UsagePercent7dValid {
		return a.UsagePercent7d
	}
	return 0
}

func clampSchedulingPercent(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func clampRemainingFraction(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// grokUsagePercentForSchedulingLocked 优先使用免费额度耗尽快照（该窗口的权威用量，
// 24h 内有效），其次使用逐请求余量头（6h 内有效）。
func (a *Account) grokUsagePercentForSchedulingLocked(now time.Time) float64 {
	if q := a.grokFreeQuota; q != nil && q.LimitTokens > 0 && !q.ExhaustedAt.IsZero() &&
		now.Sub(q.ExhaustedAt) < grokSchedulingFreeQuotaTTL {
		return clampSchedulingPercent(float64(q.UsedTokens) / float64(q.LimitTokens) * 100)
	}
	if rl := a.grokRateLimit; rl != nil && rl.LimitTokens > 0 && !rl.UpdatedAt.IsZero() &&
		now.Sub(rl.UpdatedAt) < grokSchedulingRateLimitTTL {
		return clampSchedulingPercent(float64(rl.LimitTokens-rl.RemainingTokens) / float64(rl.LimitTokens) * 100)
	}
	return 0
}

// antigravitySchedulingUsedPercent 把配额快照折算成已用百分比。有 quota_groups 时取
// 各桶里剩余最少的一个（桶是真实的共享配额池，最紧的桶决定账号何时被挡）；没有分组
// 时取各模型剩余比例的平均值，避免个别冷门模型清零把整个账号判成已耗尽。
func antigravitySchedulingUsedPercent(q AntigravityQuotaSnapshot) (float64, bool) {
	if q.Forbidden {
		return 0, false
	}
	minRemaining := math.Inf(1)
	found := false
	for _, group := range q.Groups {
		for _, bucket := range group.Buckets {
			remaining := clampRemainingFraction(bucket.RemainingFraction)
			if remaining < minRemaining {
				minRemaining = remaining
			}
			found = true
		}
	}
	if found {
		return clampSchedulingPercent((1 - minRemaining) * 100), true
	}
	if len(q.Models) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, model := range q.Models {
		sum += clampRemainingFraction(model.RemainingFraction)
	}
	return clampSchedulingPercent((1 - sum/float64(len(q.Models))) * 100), true
}

// applyAntigravityQuotaSchedulingLocked 把持久化的 antigravity_quota 凭据投影成
// 内存里的调度排序键。调用方持有 a.mu 或账号尚未发布。
func (a *Account) applyAntigravityQuotaSchedulingLocked(raw string) {
	a.antigravityQuotaValid = false
	a.antigravityQuotaUsedPercent = 0
	a.antigravityQuotaObservedAt = time.Time{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	var snapshot AntigravityQuotaSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return
	}
	pct, ok := antigravitySchedulingUsedPercent(snapshot)
	if !ok {
		return
	}
	a.antigravityQuotaUsedPercent = pct
	a.antigravityQuotaObservedAt = snapshot.UpdatedAt
	a.antigravityQuotaValid = true
}

func (a *Account) antigravityUsagePercentForSchedulingLocked(now time.Time) float64 {
	if !a.antigravityQuotaValid {
		return 0
	}
	if !a.antigravityQuotaObservedAt.IsZero() && now.Sub(a.antigravityQuotaObservedAt) > antigravitySchedulingQuotaTTL {
		return 0
	}
	return a.antigravityQuotaUsedPercent
}

// schedulerUsageRefresher 由 Store 实现：账号侧的用量观测变化后让调度器重新评估该
// 账号的桶内位置。未挂到 Store 的账号（探测/临时账号）什么也不做。
type schedulerUsageRefresher interface {
	refreshSchedulerUsage(*Account)
}

func (s *Store) refreshSchedulerUsage(acc *Account) {
	s.fastSchedulerUpdate(acc)
}

// notifySchedulerUsageChanged 在账号锁之外调用。
func (a *Account) notifySchedulerUsageChanged() {
	if a == nil {
		return
	}
	a.mu.RLock()
	sink := a.grokRuntimeSink
	a.mu.RUnlock()
	if refresher, ok := sink.(schedulerUsageRefresher); ok && refresher != nil {
		refresher.refreshSchedulerUsage(a)
	}
}
