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
	priority      int64 // 账号调度优先级（issue #358）：全局降序选择，同优先级内再按健康桶调度
}

type fastSchedulerPosition struct {
	tier  AccountHealthTier
	index int
}

// FastScheduler 是一个仅使用本地内存的调度器 POC。
// 它不在请求热路径内重算全量 score，而是直接复用 Account 上已缓存的
// HealthTier / DispatchScore / DynamicConcurrencyLimit。
//
// 调度策略：调度优先级全局优先；同优先级内按健康层级分桶，
// 桶内按调度分排序后 round-robin。
// 验证过的账号只作为同分 tie-breaker，避免历史请求量盖过额度快重置优先级。
//
// fill_first 模式（issue #501）：同优先级内按 7d 用量降序排列并始终从队首
// 取号，流量集中在剩余额度最少的账号上；该账号限流/耗尽后自然滑落到下一个，
// 恢复后按最新用量重新排队。无用量数据的账号退化为固定顺序（dbID 升序）。
type FastScheduler struct {
	mu            sync.RWMutex
	baseLimit     int64
	schedulerMode string
	buckets       map[AccountHealthTier][]fastSchedulerEntry
	positions     map[int64]fastSchedulerPosition
	// unsorted 记录哪些桶的顺序已被写入侧打乱。写入侧（新增/删除/状态变更）只做
	// O(1) 改动并在这里打标，真正的重排推迟到下一次取号时按桶合并成一次。
	// 号池上万时，"每次账号状态变更都整桶重排 + 全量重建位置索引"会把调度器
	// 独占锁占满（批量导入是最容易触发的场景）。
	unsorted   map[AccountHealthTier]bool
	cursors    [3]atomic.Uint64
	groupCheck func(apiKeyID int64, account *Account) bool
	acquire    func(account *Account, concurrencyLimit int64) bool
	// retainUnavailable keeps dormant entries in sparse routing schedulers. The
	// live Account snapshot still gates acquisition, but recovery no longer
	// requires rebuilding every API-key sub-pool.
	retainUnavailable bool
	// resorts 统计整桶重排次数，用于回归测试锁定"批量写入不再逐条重排"这个性质。
	resorts atomic.Uint64
}

// ResortCount 返回累计的整桶重排次数。
func (s *FastScheduler) ResortCount() uint64 {
	if s == nil {
		return 0
	}
	return s.resorts.Load()
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
		unsorted:  map[AccountHealthTier]bool{},
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

func (s *FastScheduler) SetRetainUnavailable(enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.retainUnavailable = enabled
	s.mu.Unlock()
}

func validFastSchedulerTier(tier AccountHealthTier) bool {
	return tier == HealthTierHealthy || tier == HealthTierWarm || tier == HealthTierRisky
}

// normalizeRetainedTier assigns dormant/banned accounts to a stable bucket in
// routing sub-pools. Once their live tier becomes schedulable, scanRangeLocked
// moves them to the correct bucket before acquisition.
func (s *FastScheduler) normalizeRetainedTier(tier, fallback AccountHealthTier) (AccountHealthTier, bool) {
	if validFastSchedulerTier(tier) {
		return tier, true
	}
	if !s.retainUnavailable {
		return "", false
	}
	if validFastSchedulerTier(fallback) {
		return fallback, true
	}
	return HealthTierRisky, true
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
		s.sortBucketLocked(tier)
	}
}

// entryLessLocked 是所有调度模式共用的桶内排序谓词。调用方必须持有 s.mu。
//
// 各模式的排序键：
//   - fill_first：7d 用量降序，流量集中在剩余额度最少的账号（issue #501）
//   - remaining_quota：7d 用量升序，优先用剩余额度多的账号
//   - round_robin 的 healthy 桶：7d 用量升序，把负载摊平到所有可用账号（issue #150）
//   - 其余：调度分降序
//
// 调度优先级（issue #358）始终全局优先，验证过的账号只作同分 tie-breaker，
// 最后用 dbID 兜底保证全序（因此排序结果与是否稳定排序无关）。
func (s *FastScheduler) entryLessLocked(tier AccountHealthTier, a, b *fastSchedulerEntry) bool {
	if a.priority != b.priority {
		return a.priority > b.priority
	}
	switch {
	case s.schedulerMode == "fill_first":
		usageA, usageB := a.acc.usagePercentForScheduling(), b.acc.usagePercentForScheduling()
		if usageA != usageB {
			return usageA > usageB
		}
	case s.schedulerMode == "remaining_quota":
		usageA, usageB := a.acc.usagePercentForScheduling(), b.acc.usagePercentForScheduling()
		if usageA != usageB {
			return usageA < usageB
		}
	case s.schedulerMode == "round_robin" && tier == HealthTierHealthy:
		usageA, usageB := a.acc.usagePercentForScheduling(), b.acc.usagePercentForScheduling()
		if usageA != usageB {
			return usageA < usageB
		}
		if a.dispatchScore != b.dispatchScore {
			return a.dispatchScore > b.dispatchScore
		}
	default:
		if a.dispatchScore != b.dispatchScore {
			return a.dispatchScore > b.dispatchScore
		}
	}
	if a.proven != b.proven {
		return a.proven
	}
	return a.dbID < b.dbID
}

// sortBucketLocked 重排单个桶并重建其位置索引，清掉待重排标记。
func (s *FastScheduler) sortBucketLocked(tier AccountHealthTier) {
	entries := s.buckets[tier]
	delete(s.unsorted, tier)
	if len(entries) == 0 {
		return
	}
	if len(entries) > 1 {
		s.resorts.Add(1)
		sort.SliceStable(entries, func(i, j int) bool {
			return s.entryLessLocked(tier, &entries[i], &entries[j])
		})
		s.buckets[tier] = entries
	}
	s.rebuildPositionsLocked(tier)
}

// ensureSortedLocked 在取号前把被打乱的桶补排一次。写入侧只打标不排序，
// 一批连续变更（例如批量导入、并发 Release）在这里合并成一次排序。
func (s *FastScheduler) ensureSortedLocked() {
	if len(s.unsorted) == 0 {
		return
	}
	for _, tier := range fastSchedulerTierOrder {
		if s.unsorted[tier] {
			s.sortBucketLocked(tier)
		}
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
	s.unsorted = map[AccountHealthTier]bool{}

	// 批量插入：先全部放入桶中，不逐条排序
	now := time.Now()
	for _, acc := range accounts {
		if acc == nil || acc.DBID == 0 {
			continue
		}
		tier, dispatchScore, limit, proven, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
		if !s.retainUnavailable && !acc.fastSchedulerKeepInPool(s.baseLimit, now, tier, limit, available) {
			continue
		}
		tier, keep := s.normalizeRetainedTier(tier, "")
		if !keep {
			continue
		}
		s.buckets[tier] = append(s.buckets[tier], fastSchedulerEntry{
			acc:           acc,
			dbID:          acc.DBID,
			dispatchScore: dispatchScore,
			proven:        proven,
			priority:      acc.schedulerPriority(),
		})
	}

	// 每个桶只排序一次 + 重建位置索引 + 计算验证账号边界
	for _, tier := range fastSchedulerTierOrder {
		s.sortBucketLocked(tier)
	}
}

func (s *FastScheduler) Update(acc *Account) {
	if s == nil || acc == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.updateLocked(acc, time.Now())
}

// UpdateMany applies one import batch while holding the scheduler lock once.
// A hot Acquire path must not observe the bucket as unsorted between individual
// additions, otherwise it can sort the whole pool once per imported account.
func (s *FastScheduler) UpdateMany(accounts []*Account) {
	if s == nil || len(accounts) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, acc := range accounts {
		s.updateLocked(acc, now)
	}
}

// updateLocked 优先就地更新已在桶内的条目：只有当新的排序键真的破坏了它与相邻
// 条目的顺序时，才给该桶打上待重排标记。这样一次 Release / 用量刷新 / 冷却标记
// 的代价是 O(1)，而不是"整桶重排 + 全量重建位置索引"。
func (s *FastScheduler) updateLocked(acc *Account, now time.Time) {
	if acc == nil || acc.DBID == 0 {
		return
	}

	pos, exists := s.positions[acc.DBID]
	tier, dispatchScore, limit, proven, available := acc.fastSchedulerSnapshot(s.baseLimit, now)
	schedulable := s.retainUnavailable || acc.fastSchedulerKeepInPool(s.baseLimit, now, tier, limit, available)
	tier, validTier := s.normalizeRetainedTier(tier, pos.tier)
	schedulable = schedulable && validTier
	if !schedulable {
		if exists {
			s.removeLocked(acc.DBID)
		}
		return
	}

	entries := s.buckets[pos.tier]
	inPlace := exists && pos.tier == tier &&
		pos.index >= 0 && pos.index < len(entries) && entries[pos.index].dbID == acc.DBID
	if !inPlace {
		// 新账号、换了健康层级，或位置索引与桶不一致：走"摘掉再挂上"的慢路径。
		if exists {
			s.removeLocked(acc.DBID)
		}
		s.appendLocked(acc, tier, dispatchScore, proven)
		return
	}

	entries[pos.index] = fastSchedulerEntry{
		acc:           acc,
		dbID:          acc.DBID,
		dispatchScore: dispatchScore,
		proven:        proven,
		priority:      acc.schedulerPriority(),
	}
	if s.entryOutOfOrderLocked(tier, entries, pos.index) {
		s.unsorted[tier] = true
	}
}

// entryOutOfOrderLocked 只比较左右邻居：桶已排好序时这足以判断该条目是否仍在
// 正确位置；桶已被标记待重排时结论无所谓，反正马上会整桶重排。
func (s *FastScheduler) entryOutOfOrderLocked(tier AccountHealthTier, entries []fastSchedulerEntry, idx int) bool {
	if idx > 0 && s.entryLessLocked(tier, &entries[idx], &entries[idx-1]) {
		return true
	}
	if idx+1 < len(entries) && s.entryLessLocked(tier, &entries[idx+1], &entries[idx]) {
		return true
	}
	return false
}

// appendLocked 把条目挂到桶尾并打上待重排标记，O(1)。
func (s *FastScheduler) appendLocked(acc *Account, tier AccountHealthTier, dispatchScore float64, proven bool) {
	entries := append(s.buckets[tier], fastSchedulerEntry{
		acc:           acc,
		dbID:          acc.DBID,
		dispatchScore: dispatchScore,
		proven:        proven,
		priority:      acc.schedulerPriority(),
	})
	s.buckets[tier] = entries
	s.positions[acc.DBID] = fastSchedulerPosition{tier: tier, index: len(entries) - 1}
	if len(entries) > 1 && s.entryLessLocked(tier, &entries[len(entries)-1], &entries[len(entries)-2]) {
		s.unsorted[tier] = true
	}
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
	return s.AcquireExcludingWithDispatch(apiKeyID, exclude, filter, DispatchPolicyStandard)
}

// AcquireExcludingWithDispatch 按用量策略选号。spark 请求不看账号级 5h/7d。
func (s *FastScheduler) AcquireExcludingWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	return s.acquireExcludingWithDispatch(apiKeyID, exclude, filter, policy, nil)
}

// AcquireForAffinityWithDispatch chooses a deterministic start offset inside
// the highest-priority healthy segment. Unlike the legacy HRW path it does not
// allocate and sort an O(N) candidate slice for every new session; normally it
// inspects one entry and remains O(1) with respect to account-pool size.
func (s *FastScheduler) AcquireForAffinityWithDispatch(affinityHash uint64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) *Account {
	return s.acquireExcludingWithDispatch(apiKeyID, exclude, filter, policy, &affinityHash)
}

// HasAvailableWithDispatch is a read-only shadow check. It deliberately
// compares candidate presence rather than an exact account ID because the
// indexed and legacy round-robin cursors are independent and both choices can
// be policy-correct.
func (s *FastScheduler) HasAvailableWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) bool {
	if s == nil {
		return false
	}
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, tier := range fastSchedulerTierOrder {
		for _, entry := range s.buckets[tier] {
			if entry.acc == nil || exclude != nil && exclude[entry.dbID] {
				continue
			}
			if !entry.acc.AllowsAPIKey(apiKeyID) || s.groupCheck != nil && !s.groupCheck(apiKeyID, entry.acc) {
				continue
			}
			if filter != nil && !filter(entry.acc) {
				continue
			}
			_, _, limit, _, available := entry.acc.fastSchedulerSnapshotForPolicy(s.baseLimit, now, policy)
			if available && limit > 0 && accountOccupiedRequests(entry.acc) < limit {
				return true
			}
		}
	}
	return false
}

func (s *FastScheduler) acquireExcludingWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy, affinityHash *uint64) *Account {
	if s == nil {
		return nil
	}

	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	baseLimit := s.baseLimit
	for {
		// 写入侧只打标不排序，取号前在这里把被打乱的桶补排一次。
		s.ensureSortedLocked()
		changed := false
		// 每个健康桶都已按 priority DESC 排序。用三路归并的方式逐级取全局
		// 最高优先级，并在该优先级内按 healthy -> warm -> risky 扫描。
		// 只有所有健康桶的当前优先级都不可用时，才会回落到下一优先级。
		var segmentStarts [3]int
	priorityLoop:
		for {
			var nextPriority int64
			foundPriority := false
			for tierIdx, tier := range fastSchedulerTierOrder {
				bucket := s.buckets[tier]
				start := segmentStarts[tierIdx]
				if start >= len(bucket) {
					continue
				}
				priority := bucket[start].priority
				if !foundPriority || priority > nextPriority {
					nextPriority = priority
					foundPriority = true
				}
			}
			if !foundPriority {
				break
			}

			for tierIdx, tier := range fastSchedulerTierOrder {
				bucket := s.buckets[tier]
				segStart := segmentStarts[tierIdx]
				if segStart >= len(bucket) || bucket[segStart].priority != nextPriority {
					continue
				}
				segEnd := segStart + 1
				for segEnd < len(bucket) && bucket[segEnd].priority == nextPriority {
					segEnd++
				}

				cursor := &s.cursors[tierIdx]
				var zeroCursor atomic.Uint64
				// remaining_quota / fill_first 不走轮询游标：每次都从排序后的
				// 队首开始扫，保证严格按排序语义取号。
				if s.schedulerMode == "remaining_quota" || s.schedulerMode == "fill_first" {
					cursor = &zeroCursor
				}
				acc, stale := s.scanRangeLocked(tier, segStart, segEnd, cursor, affinityHash, baseLimit, now, apiKeyID, exclude, filter, policy)
				if acc != nil {
					return acc
				}
				if stale {
					changed = true
					break priorityLoop
				}
				segmentStarts[tierIdx] = segEnd
			}
		}
		if !changed {
			return nil
		}
	}
}

// scanRangeLocked 在 bucket[start:end) 范围内 round-robin 扫描可用账号。
// 返回 stale=true 表示桶内缓存已过期，调用方应重新开始扫描。
func (s *FastScheduler) scanRangeLocked(expectedTier AccountHealthTier, rangeStart, rangeEnd int, cursor *atomic.Uint64, affinityHash *uint64, baseLimit int64, now time.Time, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy) (*Account, bool) {
	bucket := s.buckets[expectedTier]
	rangeLen := rangeEnd - rangeStart
	if rangeLen <= 0 {
		return nil, false
	}
	start := int(cursor.Add(1)-1) % rangeLen
	if affinityHash != nil {
		start = int(*affinityHash % uint64(rangeLen))
	}
	for offset := 0; offset < rangeLen; offset++ {
		entry := bucket[rangeStart+(start+offset)%rangeLen]
		if entry.acc == nil {
			continue
		}
		if exclude != nil && exclude[entry.dbID] {
			continue
		}
		authorizationKeyID := apiKeyID
		if authorizationKeyID < 0 {
			authorizationKeyID = -authorizationKeyID
		}
		if !entry.acc.AllowsAPIKey(authorizationKeyID) {
			continue
		}
		if s.groupCheck != nil && !s.groupCheck(apiKeyID, entry.acc) {
			continue
		}
		if filter != nil && !filter(entry.acc) {
			continue
		}
		tier, dispatchScore, limit, proven, available := entry.acc.fastSchedulerSnapshotForPolicy(baseLimit, now, policy)
		tier, keepTier := s.normalizeRetainedTier(tier, expectedTier)
		if !keepTier {
			s.removeLocked(entry.dbID)
			return nil, true
		}
		if tier != expectedTier {
			// 健康层级变了，条目要换桶，桶边界随之变化，必须重新开始扫描。
			s.removeLocked(entry.dbID)
			if s.retainUnavailable || entry.acc.fastSchedulerKeepInPool(baseLimit, now, tier, limit, available) {
				s.insertLocked(entry.acc, now)
			}
			return nil, true
		}
		if proven != entry.proven || math.Abs(dispatchScore-entry.dispatchScore) >= 1 {
			// 同层级内的排序键漂移：就地刷新缓存并标记待重排，继续扫完本轮。
			// 每次漂移都重启整轮扫描会让大号池下的一次取号反复全量重排。
			s.refreshEntryLocked(expectedTier, rangeStart+(start+offset)%rangeLen, dispatchScore, proven)
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

// refreshEntryLocked 就地刷新桶内某个下标的排序键缓存，顺序被破坏时打上待重排标记。
func (s *FastScheduler) refreshEntryLocked(tier AccountHealthTier, idx int, dispatchScore float64, proven bool) {
	entries := s.buckets[tier]
	if idx < 0 || idx >= len(entries) || entries[idx].acc == nil {
		return
	}
	entries[idx].dispatchScore = dispatchScore
	entries[idx].proven = proven
	entries[idx].priority = entries[idx].acc.schedulerPriority()
	if s.entryOutOfOrderLocked(tier, entries, idx) {
		s.unsorted[tier] = true
	}
}

func (s *FastScheduler) Release(acc *Account) {
	releaseOccupiedAccountSlot(acc)
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
	if !s.retainUnavailable && !acc.fastSchedulerKeepInPool(s.baseLimit, now, tier, limit, available) {
		return
	}
	tier, keep := s.normalizeRetainedTier(tier, "")
	if !keep {
		return
	}

	s.appendLocked(acc, tier, dispatchScore, proven)
}

// removeLocked 用"末尾条目补洞"的方式摘除，O(1)。补洞会打乱桶内顺序，
// 因此打上待重排标记，由下一次取号前的 ensureSortedLocked 统一修复。
func (s *FastScheduler) removeLocked(dbID int64) {
	pos, ok := s.positions[dbID]
	if !ok {
		return
	}

	entries := s.buckets[pos.tier]
	if pos.index < 0 || pos.index >= len(entries) || entries[pos.index].dbID != dbID {
		// 位置索引与桶不一致（不应发生）：线性找一遍，宁可慢也不能漏摘。
		pos.index = -1
		for idx := range entries {
			if entries[idx].dbID == dbID {
				pos.index = idx
				break
			}
		}
		if pos.index < 0 {
			delete(s.positions, dbID)
			s.unsorted[pos.tier] = true
			return
		}
	}

	last := len(entries) - 1
	if pos.index != last {
		entries[pos.index] = entries[last]
		s.positions[entries[pos.index].dbID] = fastSchedulerPosition{tier: pos.tier, index: pos.index}
		s.unsorted[pos.tier] = true
	}
	entries[last] = fastSchedulerEntry{}
	s.buckets[pos.tier] = entries[:last]
	delete(s.positions, dbID)
}

func (s *FastScheduler) rebuildPositionsLocked(tier AccountHealthTier) {
	for idx, entry := range s.buckets[tier] {
		s.positions[entry.dbID] = fastSchedulerPosition{
			tier:  tier,
			index: idx,
		}
	}
}

func (a *Account) fastSchedulerKeepInPool(baseLimit int64, now time.Time, tier AccountHealthTier, limit int64, available bool) bool {
	if tier != HealthTierHealthy && tier != HealthTierWarm && tier != HealthTierRisky {
		return false
	}
	if available && limit > 0 {
		return true
	}
	_, _, sparkLimit, _, sparkOK := a.fastSchedulerSnapshotForSpark(baseLimit, now)
	return sparkOK && sparkLimit > 0
}

func (a *Account) fastSchedulerSnapshotForPolicy(baseLimit int64, now time.Time, policy DispatchPolicy) (AccountHealthTier, float64, int64, bool, bool) {
	if policy == DispatchPolicySpark {
		return a.fastSchedulerSnapshotForSpark(baseLimit, now)
	}
	return a.fastSchedulerSnapshot(baseLimit, now)
}

func (a *Account) fastSchedulerSnapshotForSpark(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	tier := a.healthTierLocked()
	score := a.DispatchScore
	proven := atomic.LoadInt64(&a.TotalRequests) > 10
	if score == 0 && a.SchedulerScore != 0 {
		score = a.SchedulerScore
	}
	if score == 0 && tier != HealthTierBanned && a.hasDispatchCredentialLocked() && a.Status != StatusError {
		rawScore := 100.0
		appliedBias := a.effectiveScoreBiasLocked(now, tier)
		score = rawScore + float64(appliedBias)
	}
	baseConcurrencyEffective := a.BaseConcurrencyEffective
	if baseConcurrencyEffective <= 0 {
		baseConcurrencyEffective = a.effectiveBaseConcurrencyLocked(baseLimit)
	}
	limit := concurrencyLimitForTier(baseConcurrencyEffective, tier)
	// sparkDispatchEligibleLocked 与 isAvailableLocked 一样只看锁内状态;
	// DispatchPaused 是锁外原子标志,标准快照在这里显式补一道门,spark 必须
	// 对齐,否则运维手动停调度或过载熔断置位的账号仍会被 spark 请求选中。
	available := a.sparkDispatchEligibleLocked(now) && atomic.LoadInt32(&a.DispatchPaused) == 0
	return tier, score, limit, proven, available
}

func (a *Account) fastSchedulerSnapshot(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool, bool) {
	return a.fastSchedulerSnapshotWithUsageOverride(baseLimit, now, false)
}

func (a *Account) fastSchedulerSnapshotForContinuation(baseLimit int64, now time.Time) (AccountHealthTier, float64, int64, bool, bool) {
	return a.fastSchedulerSnapshotWithUsageOverride(baseLimit, now, true)
}

func (a *Account) fastSchedulerSnapshotWithUsageOverride(baseLimit int64, now time.Time, continuation bool) (AccountHealthTier, float64, int64, bool, bool) {
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

	continuationUsageOverride := continuation && a.usageLimitContinuationEligibleLocked(now)
	available := a.Status != StatusError && tier != HealthTierBanned && a.hasDispatchCredentialLocked()
	if atomic.LoadInt32(&a.DispatchPaused) != 0 {
		available = false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) && !continuationUsageOverride {
		available = false
	}
	if a.quotaAutoPausedLocked(now) {
		available = false
	}
	// Fresh dispatch remains fenced by WHAM-reported usage windows even when
	// IgnoreUsageLimitStatus is enabled. Continuations use a separate, narrow
	// account-selection path in Store.
	if a.usageWindowBlocksFreshDispatchLocked(now) &&
		!continuationUsageOverride {
		available = false
	}

	tier, limit, available = a.applyAntigravitySchedulerOverrideLocked(baseLimit, tier, limit, available)
	return tier, score, limit, proven, available
}

func tryAcquireAccount(acc *Account, limit int64) bool {
	if acc == nil {
		return false
	}

	if limit <= 0 {
		return false
	}

	if !reserveOccupiedAccountSlot(acc, limit) {
		return false
	}
	atomic.AddInt64(&acc.TotalRequests, 1)
	atomic.StoreInt64(&acc.LastUsedAt, time.Now().UnixNano())
	return true
}
