package auth

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var fastSchedulerTierOrder = []AccountHealthTier{
	HealthTierHealthy,
	HealthTierWarm,
	HealthTierRisky,
}

type fastSchedulerEntry struct {
	acc           *Account
	dbID          int64
	dispatchScore float64
	proven        bool
}

type fastSchedulerPosition struct {
	tier  AccountHealthTier
	index int
}

// FastScheduler 是一个仅使用本地内存的调度器 POC。
// 它不在请求热路径内重算全量 score，而是直接复用 Account 上已缓存的
// HealthTier / DispatchScore / DynamicConcurrencyLimit。
//
// 调度策略：按健康层级分桶，桶内按调度分排序后 round-robin。
// 验证过的账号只作为同分 tie-breaker，避免历史请求量盖过额度快重置优先级。
type FastScheduler struct {
	mu            sync.RWMutex
	baseLimit     int64
	schedulerMode string
	buckets       map[AccountHealthTier][]fastSchedulerEntry
	positions     map[int64]fastSchedulerPosition
	cursors       [3]atomic.Uint64
	groupCheck    func(apiKeyID int64, account *Account) bool
	acquire       func(account *Account, concurrencyLimit int64) bool
}

func NewFastScheduler(baseLimit int64, schedulerMode string) *FastScheduler {
	if baseLimit <= 0 {
		baseLimit = 1
	}
	if schedulerMode == "" {
		schedulerMode = "round_robin"
	}
	return &FastScheduler{
		baseLimit:     baseLimit,
		schedulerMode: schedulerMode,
		buckets: map[AccountHealthTier][]fastSchedulerEntry{
			HealthTierHealthy: nil,
			HealthTierWarm:    nil,
			HealthTierRisky:   nil,
		},
		positions: map[int64]fastSchedulerPosition{},
	}
}

func (s *FastScheduler) SetGroupCheck(check func(apiKeyID int64, account *Account) bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.groupCheck = check
	s.mu.Unlock()
}

func (s *FastScheduler) SetAcquireFunc(acquire func(account *Account, concurrencyLimit int64) bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.acquire = acquire
	s.mu.Unlock()
}

func (s *FastScheduler) SetSchedulerMode(mode string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode == "" {
		mode = "round_robin"
	}
	s.schedulerMode = mode

	// Re-sort all tier buckets according to the new mode.
	for _, tier := range fastSchedulerTierOrder {
		entries := s.buckets[tier]
		if len(entries) == 0 {
			continue
		}
		if mode == "remaining_quota" {
			sort.SliceStable(entries, func(i, j int) bool {
				usageI := entries[i].acc.usagePercentForScheduling()
				usageJ := entries[j].acc.usagePercentForScheduling()
				if usageI == usageJ {
					if entries[i].proven != entries[j].proven {
						return entries[i].proven
					}
					return entries[i].dbID < entries[j].dbID
				}
				return usageI < usageJ
			})
		} else {
			sort.SliceStable(entries, func(i, j int) bool {
				if entries[i].dispatchScore == entries[j].dispatchScore {
					if entries[i].proven != entries[j].proven {
						return entries[i].proven
					}
					return entries[i].dbID < entries[j].dbID
				}
				return entries[i].dispatchScore > entries[j].dispatchScore
			})
		}
		s.buckets[tier] = entries
		s.rebuildPositionsLocked(tier)
	}
}

func (s *FastScheduler) SchedulerMode() string {
	if s == nil {
		return "round_robin"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schedulerMode
}

// BuildFastScheduler 用当前 Store 快照构建一个独立 scheduler。
// 该方法不会影响现有生产流量路径，只用于 POC/benchmark/灰度验证。
func (s *Store) BuildFastScheduler() *FastScheduler {
	if s == nil {
		return NewFastScheduler(1, "round_robin")
	}
	scheduler := NewFastScheduler(atomic.LoadInt64(&s.maxConcurrency), s.GetSchedulerMode())
	s.configureFastScheduler(scheduler)

	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	scheduler.Rebuild(accounts)
	return scheduler
}

func (s *FastScheduler) Rebuild(accounts []*Account) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.buckets = map[AccountHealthTier][]fastSchedulerEntry{
		HealthTierHealthy: nil,
		HealthTierWarm:    nil,
		HealthTierRisky:   nil,
	}
	s.positions = make(map[int64]fastSchedulerPosition, len(accounts))

	// 批量插入：先全部放入桶中，不逐条排序
	now := time.Now()
	for _, acc := range accounts {
		if acc == nil || acc.DBID == 0 {
			continue
		}
		tier, dispatchScore, limit, proven, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
		if !available || limit <= 0 {
			continue
		}
		if tier != HealthTierHealthy && tier != HealthTierWarm && tier != HealthTierRisky {
			continue
		}
		s.buckets[tier] = append(s.buckets[tier], fastSchedulerEntry{
			acc:           acc,
			dbID:          acc.DBID,
			dispatchScore: dispatchScore,
			proven:        proven,
		})
	}

	// 每个桶只排序一次 + 重建位置索引 + 计算验证账号边界
	for _, tier := range fastSchedulerTierOrder {
		entries := s.buckets[tier]
		if len(entries) == 0 {
			continue
		}
		if s.schedulerMode == "remaining_quota" {
			sort.SliceStable(entries, func(i, j int) bool {
				usageI := entries[i].acc.usagePercentForScheduling()
				usageJ := entries[j].acc.usagePercentForScheduling()
				if usageI == usageJ {
					if entries[i].proven != entries[j].proven {
						return entries[i].proven
					}
					return entries[i].dbID < entries[j].dbID
				}
				return usageI < usageJ
			})
		} else {
			sort.SliceStable(entries, func(i, j int) bool {
				if entries[i].dispatchScore == entries[j].dispatchScore {
					if entries[i].proven != entries[j].proven {
						return entries[i].proven
					}
					return entries[i].dbID < entries[j].dbID
				}
				return entries[i].dispatchScore > entries[j].dispatchScore
			})
		}
		s.buckets[tier] = entries
		s.rebuildPositionsLocked(tier)
	}
}

func (s *FastScheduler) Update(acc *Account) {
	if s == nil || acc == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.removeLocked(acc.DBID)
	s.insertLocked(acc, time.Now())
}

func (s *FastScheduler) Remove(dbID int64) {
	if s == nil || dbID == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(dbID)
}

func (s *FastScheduler) SetBaseLimit(baseLimit int64) {
	if s == nil {
		return
	}
	if baseLimit <= 0 {
		baseLimit = 1
	}
	s.mu.Lock()
	s.baseLimit = baseLimit
	s.mu.Unlock()
}

func (s *FastScheduler) Acquire() *Account {
	return s.AcquireExcluding(0, nil)
}

// AcquireExcluding 获取下一个可用账号，排除指定的账号 ID 集合
func (s *FastScheduler) AcquireExcluding(apiKeyID int64, exclude map[int64]bool) *Account {
	return s.AcquireExcludingWithFilter(apiKeyID, exclude, nil)
}

// AcquireExcludingWithFilter 获取下一个可用账号，并应用请求级账号过滤器。
func (s *FastScheduler) AcquireExcludingWithFilter(apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	if s == nil {
		return nil
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	baseLimit := s.baseLimit
	var zeroCursor atomic.Uint64
	for {
		changed := false
		for tierIdx, tier := range fastSchedulerTierOrder {
			bucket := s.buckets[tier]
			if len(bucket) == 0 {
				continue
			}

			cursor := &s.cursors[tierIdx]
			if s.schedulerMode == "remaining_quota" {
				zeroCursor.Store(0)
				cursor = &zeroCursor
			}
			acc, stale := s.scanRangeLocked(tier, 0, len(bucket), cursor, baseLimit, now, apiKeyID, exclude, filter)
			if acc != nil {
				return acc
			}
			if stale {
				changed = true
				break
			}
		}
		if !changed {
			return nil
		}
	}
}

// scanRangeLocked 在 bucket[start:end) 范围内 round-robin 扫描可用账号。
// 返回 stale=true 表示桶内缓存已过期，调用方应重新开始扫描。
func (s *FastScheduler) scanRangeLocked(expectedTier AccountHealthTier, rangeStart, rangeEnd int, cursor *atomic.Uint64, baseLimit int64, now time.Time, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, bool) {
	bucket := s.buckets[expectedTier]
	rangeLen := rangeEnd - rangeStart
	if rangeLen <= 0 {
		return nil, false
	}
	start := int(cursor.Add(1)-1) % rangeLen
	for offset := 0; offset < rangeLen; offset++ {
		entry := bucket[rangeStart+(start+offset)%rangeLen]
		if entry.acc == nil {
			continue
		}
		if exclude != nil && exclude[entry.dbID] {
			continue
		}
		if !entry.acc.AllowsAPIKey(apiKeyID) {
			continue
		}
		if s.groupCheck != nil && !s.groupCheck(apiKeyID, entry.acc) {
			continue
		}
		if filter != nil && !filter(entry.acc) {
			continue
		}
		tier, dispatchScore, limit, proven, available := entry.acc.fastSchedulerSnapshot(baseLimit, now)
		if tier != expectedTier || proven != entry.proven || math.Abs(dispatchScore-entry.dispatchScore) >= 1 {
			s.removeLocked(entry.dbID)
			if available && limit > 0 {
				s.insertLocked(entry.acc, now)
			}
			return nil, true
		}
		if !available || limit <= 0 {
			continue
		}
		if !s.tryAcquireAccount(entry.acc, limit) {
			continue
		}
		return entry.acc, false
	}
	return nil, false
}

func (s *FastScheduler) Release(acc *Account) {
	if acc == nil {
		return
	}
	atomic.AddInt64(&acc.ActiveRequests, -1)
}

func (s *FastScheduler) tryAcquireAccount(acc *Account, limit int64) bool {
	if s != nil && s.acquire != nil {
		return s.acquire(acc, limit)
	}
	return tryAcquireAccount(acc, limit)
}

func (s *FastScheduler) BucketSizes() map[AccountHealthTier]int {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[AccountHealthTier]int{
		HealthTierHealthy: len(s.buckets[HealthTierHealthy]),
		HealthTierWarm:    len(s.buckets[HealthTierWarm]),
		HealthTierRisky:   len(s.buckets[HealthTierRisky]),
	}
}

func (s *FastScheduler) insertLocked(acc *Account, now time.Time) {
	if acc == nil || acc.DBID == 0 {
		return
	}

	tier, dispatchScore, limit, proven, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
	if !available || limit <= 0 {
		return
	}
	if tier != HealthTierHealthy && tier != HealthTierWarm && tier != HealthTierRisky {
		return
	}

	entries := append(s.buckets[tier], fastSchedulerEntry{
		acc:           acc,
		dbID:          acc.DBID,
		dispatchScore: dispatchScore,
		proven:        proven,
	})
	if s.schedulerMode == "remaining_quota" {
		sort.SliceStable(entries, func(i, j int) bool {
			usageI := entries[i].acc.usagePercentForScheduling()
			usageJ := entries[j].acc.usagePercentForScheduling()
			if usageI == usageJ {
				if entries[i].proven != entries[j].proven {
					return entries[i].proven
				}
				return entries[i].dbID < entries[j].dbID
			}
			return usageI < usageJ
		})
	} else if s.schedulerMode == "round_robin" && tier == HealthTierHealthy {
		// round_robin 模式下,healthy 桶按 7d 用量 ASC 排序后再走轮询。
		// 这样同一个 round 里,用得少的账号被先轮到,自然把负载摊平到所有可用账号上,
		// 避免出现"轮询模式仍然一直薅同一个号"的现象 (issue #150)。
		sort.SliceStable(entries, func(i, j int) bool {
			usageI := entries[i].acc.usagePercentForScheduling()
			usageJ := entries[j].acc.usagePercentForScheduling()
			if usageI == usageJ {
				if entries[i].dispatchScore != entries[j].dispatchScore {
					return entries[i].dispatchScore > entries[j].dispatchScore
				}
				if entries[i].proven != entries[j].proven {
					return entries[i].proven
				}
				return entries[i].dbID < entries[j].dbID
			}
			return usageI < usageJ
		})
	} else {
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].dispatchScore == entries[j].dispatchScore {
				if entries[i].proven != entries[j].proven {
					return entries[i].proven
				}
				return entries[i].dbID < entries[j].dbID
			}
			return entries[i].dispatchScore > entries[j].dispatchScore
		})
	}
	s.buckets[tier] = entries
	s.rebuildPositionsLocked(tier)
}

func (s *FastScheduler) removeLocked(dbID int64) {
	pos, ok := s.positions[dbID]
	if !ok {
		return
	}

	entries := s.buckets[pos.tier]
	if pos.index < 0 || pos.index >= len(entries) {
		delete(s.positions, dbID)
		return
	}

	copy(entries[pos.index:], entries[pos.index+1:])
	entries = entries[:len(entries)-1]
	s.buckets[pos.tier] = entries
	delete(s.positions, dbID)
	s.rebuildPositionsLocked(pos.tier)
}

func (s *FastScheduler) rebuildPositionsLocked(tier AccountHealthTier) {
	for idx, entry := range s.buckets[tier] {
		s.positions[entry.dbID] = fastSchedulerPosition{
			tier:  tier,
			index: idx,
		}
	}
}

func (a *Account) fastSchedulerSnapshot(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if (isPremium5hPlan(a.PlanType) && a.UsagePercent5hValid) ||
		(IsPlusOrHigherPlan(a.PlanType) && a.UsagePercent7dValid) {
		a.recomputeSchedulerLocked(baseLimit)
	}

	tier := a.healthTierLocked()
	score := a.DispatchScore
	limit := a.DynamicConcurrencyLimit
	proven := atomic.LoadInt64(&a.TotalRequests) > 10

	if score == 0 && a.SchedulerScore != 0 {
		score = a.SchedulerScore
	}
	if score == 0 && tier != HealthTierBanned && a.hasDispatchCredentialLocked() && a.Status != StatusError {
		rawScore := 100.0
		appliedBias := a.effectiveScoreBiasLocked(now, tier)
		score = rawScore + float64(appliedBias)
	}
	if limit <= 0 {
		baseConcurrencyEffective := a.BaseConcurrencyEffective
		if baseConcurrencyEffective <= 0 {
			baseConcurrencyEffective = a.effectiveBaseConcurrencyLocked(baseLimit)
		}
		limit = a.quotaAutoPause5hGuardConcurrencyLimitLocked(concurrencyLimitForTier(baseConcurrencyEffective, tier), now)
		limit = a.smartPacingConcurrencyLimitLocked(limit, now)
	}

	available := a.Status != StatusError && tier != HealthTierBanned && a.hasDispatchCredentialLocked()
	if atomic.LoadInt32(&a.DispatchPaused) != 0 {
		available = false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		available = false
	}
	if a.premium5hRateLimitedLocked(now) {
		available = false
	}
	if a.quotaAutoPausedLocked(now) {
		available = false
	}
	// Free 账号 7d 用量耗尽，不参与调度
	if a.usageExhaustedLocked() {
		available = false
	}

	return tier, score, limit, proven, available
}

func tryAcquireAccount(acc *Account, limit int64) bool {
	if acc == nil {
		return false
	}

	if limit <= 0 {
		return false
	}

	for {
		current := atomic.LoadInt64(&acc.ActiveRequests)
		if current >= limit {
			return false
		}
		if atomic.CompareAndSwapInt64(&acc.ActiveRequests, current, current+1) {
			atomic.AddInt64(&acc.TotalRequests, 1)
			atomic.StoreInt64(&acc.LastUsedAt, time.Now().UnixNano())
			return true
		}
	}
}
