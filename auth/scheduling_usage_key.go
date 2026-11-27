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
	case a.isOpenCodeLocked():
		return a.openCodeUsagePercentForSchedulingLocked(now)
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
			if !bucket.FractionPresent || bucket.RemainingFraction == nil {
				continue
			}
			remaining := clampRemainingFraction(*bucket.RemainingFraction)
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
	count := 0
	for _, model := range q.Models {
		if !model.FractionPresent || model.RemainingFraction == nil {
			continue
		}
		sum += clampRemainingFraction(*model.RemainingFraction)
		count++
	}
	if count == 0 {
		return 0, false
	}
	return clampSchedulingPercent((1 - sum/float64(count)) * 100), true
}

// AntigravityQuotaUsedPercent 把持久化的 antigravity_quota 凭据折算成已用百分比，
// 口径与调度排序键一致（不看观测时效）。快照为空、无法解析或被拒绝访问时 ok=false。
func AntigravityQuotaUsedPercent(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	var snapshot AntigravityQuotaSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return 0, false
	}
	return antigravitySchedulingUsedPercent(snapshot)
}

// applyAntigravityQuotaSchedulingLocked 把持久化的 antigravity_quota 凭据投影成
// 内存里的调度排序键,并顺带记下各模型的最大输出上限。调用方持有 a.mu 或账号尚未发布。
func (a *Account) applyAntigravityQuotaSchedulingLocked(raw string) {
	a.antigravityQuotaValid = false
	a.antigravityQuotaUsedPercent = 0
	a.antigravityQuotaObservedAt = time.Time{}
	a.antigravityModelMaxOutput = nil
	a.AntigravityQuota = AntigravityQuotaSnapshot{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	var snapshot AntigravityQuotaSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return
	}
	a.AntigravityQuota = snapshot
	for _, model := range snapshot.Models {
		id := strings.ToLower(strings.TrimSpace(model.ModelID))
		if id == "" || model.MaxOutputTokens == nil || *model.MaxOutputTokens <= 0 {
			continue
		}
		if a.antigravityModelMaxOutput == nil {
			a.antigravityModelMaxOutput = make(map[string]int)
		}
		a.antigravityModelMaxOutput[id] = *model.MaxOutputTokens
	}
	pct, ok := antigravitySchedulingUsedPercent(snapshot)
	if !ok {
		return
	}
	a.antigravityQuotaUsedPercent = pct
	a.antigravityQuotaObservedAt = snapshot.UpdatedAt
	a.antigravityQuotaValid = true
}

// antigravityQuotaGateFacts 报告快照是否仍携带硬门禁与软权重可求值的配额事实。clear
// 操作只删除 quota_groups/source_metadata/ai_credits 而保留 models 目录分区（Spec §8.2），
// 因此「空事实」按门禁相关分区判定，与 database 侧的清除语义保持一致。
func antigravityQuotaGateFacts(snapshot AntigravityQuotaSnapshot) bool {
	return len(snapshot.Groups) > 0 || snapshot.AICredits != nil || snapshot.Forbidden
}

// antigravityQuotaAppliedSeq 返回快照里各来源最大的 applied_seq，即内存当前序列。
func antigravityQuotaAppliedSeq(snapshot AntigravityQuotaSnapshot) int64 {
	var seq int64
	for _, meta := range snapshot.SourceMetadata {
		if meta.AppliedSeq > seq {
			seq = meta.AppliedSeq
		}
	}
	return seq
}

// cloneAntigravityModelMaxOutput 深拷贝 Antigravity 模型的最大输出 token 映射表。
func cloneAntigravityModelMaxOutput(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]int, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

// mergeAntigravityQuotaProjectionLocked 把 outbox 行携带的 antigravity_quota(V2 快照)
// 与 antigravity_quota_epoch 按 Spec §4.3 的原位单调规则合并进已有内存对象。src 是
// 刚从数据库行装载的临时投影（其排序键已由 applyAntigravityQuotaSchedulingLocked 求值）。
// 调用方持有 a.mu；Account 指针、在途并发计数与会话绑定一律不动。
//
//   - 代际变化：行就是新代际的事实，无条件采用并重新绑定纪元基线；
//   - src 纪元更新：**序号空间重新初始化**，空事实（clear 动作）同样直接覆盖并清空
//     门禁派生键——这是远端内存里的 known_zero 能被「删除」单调覆盖的唯一路径，也是
//     本机曾因纪元落后而丢弃的观测的自愈点（Spec §4.3 自愈义务）；
//   - src 纪元更旧：迟到的旧纪元，整份丢弃；
//   - 纪元相同：空事实一律丢弃（删除不得以相同纪元生效）；非空事实要求 applied_seq
//     不小于内存序列，相等即幂等重放。
//
// 纪元递增不等同于代际变化：不清 scope、不动凭据（清 scope 只由 §4.1 ReplaceCAS 触发）。
func (a *Account) mergeAntigravityQuotaProjectionLocked(src *Account, identityChanged bool) {
	if a == nil || src == nil {
		return
	}
	if !identityChanged {
		switch {
		case src.antigravityQuotaEpoch < a.antigravityQuotaEpoch:
			return
		case src.antigravityQuotaEpoch == a.antigravityQuotaEpoch:
			if !antigravityQuotaGateFacts(src.AntigravityQuota) {
				return
			}
			if antigravityQuotaAppliedSeq(src.AntigravityQuota) < antigravityQuotaAppliedSeq(a.AntigravityQuota) {
				return
			}
		}
	}
	a.AntigravityQuota = src.AntigravityQuota
	a.antigravityQuotaEpoch = src.antigravityQuotaEpoch
	a.antigravityQuotaValid = src.antigravityQuotaValid
	a.antigravityQuotaUsedPercent = src.antigravityQuotaUsedPercent
	a.antigravityQuotaObservedAt = src.antigravityQuotaObservedAt
	a.antigravityModelMaxOutput = cloneAntigravityModelMaxOutput(src.antigravityModelMaxOutput)
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
