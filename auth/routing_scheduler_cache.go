package auth

import (
	"sync/atomic"
)

const (
	maxRoutingSchedulerKeys      = 128
	maxRoutingSchedulerAliasKeys = 4096
	maxRoutingSchedulerAccounts  = 250_000
	routingSchedulerSparseRatio  = 4
)

// routingSchedulerEntry wraps a cached per-key scheduler. alias entries point
// at the shared global scheduler and cost nothing to rebuild, so they use a
// separate, larger key budget.
type routingSchedulerEntry struct {
	scheduler *FastScheduler
	accounts  int
	alias     bool
	lastHitNS atomic.Int64
}

// routingFastScheduler returns the global scheduler for unrestricted API keys
// and lazily builds a compact scheduler for sparse routing rules. Without this
// second-level index, a key allowed to use only a tiny group can still walk a
// large portion of the global 50k-account bucket on every request.
//
// The hit path takes only a read lock; cold builds run outside the map lock so
// one cold restricted key cannot stall every concurrent dispatch behind an
// O(pool) scan. A generation counter discards builds that lost a race with an
// invalidation.
func (s *Store) routingFastScheduler(apiKeyID int64) *FastScheduler {
	if s == nil {
		return nil
	}
	global := s.getFastScheduler()
	if global == nil || apiKeyID <= 0 || s.SchedulerEngine() != "indexed" {
		return global
	}

	s.routingSchedulersMu.RLock()
	entry, ok := s.routingSchedulers[apiKeyID]
	s.routingSchedulersMu.RUnlock()
	if ok {
		entry.lastHitNS.Store(s.routingSchedulerAccess.Add(1))
		if s.schedulerMetrics != nil {
			s.schedulerMetrics.routingCacheHits.Add(1)
		}
		return entry.scheduler
	}
	if s.schedulerMetrics != nil {
		s.schedulerMetrics.routingCacheMisses.Add(1)
	}
	generation := s.routingGeneration.Load()

	if !s.apiKeyHasConfiguredRoutingRestriction(apiKeyID) {
		s.publishRoutingScheduler(apiKeyID, global, 0, true, generation)
		if s.schedulerMetrics != nil {
			s.schedulerMetrics.routingCacheFallbacks.Add(1)
		}
		return global
	}

	accounts := s.accountSnapshotAccounts()
	eligible := make([]*Account, 0, min(len(accounts), 256))
	for _, acc := range accounts {
		if s.accountAllowedForAPIKey(acc, apiKeyID) {
			eligible = append(eligible, acc)
		}
	}

	// A dense sub-pool provides little benefit: the global cursor will normally
	// hit an eligible account in a handful of probes. Cache an alias so this
	// density check is paid only once per routing generation.
	if len(eligible) > 0 && len(eligible) > len(accounts)/routingSchedulerSparseRatio {
		s.publishRoutingScheduler(apiKeyID, global, 0, true, generation)
		if s.schedulerMetrics != nil {
			s.schedulerMetrics.routingCacheFallbacks.Add(1)
		}
		return global
	}

	scheduler := NewFastScheduler(atomic.LoadInt64(&s.maxConcurrency), s.GetSchedulerMode())
	// A routing scheduler contains every eligible account, including accounts
	// currently cooling down. Keeping dormant entries lets them become usable
	// again from their live Account state without rebuilding or fan-out updates.
	scheduler.SetRetainUnavailable(true)
	s.configureFastScheduler(scheduler)
	scheduler.Rebuild(eligible)
	if s.schedulerMetrics != nil {
		s.schedulerMetrics.routingCacheBuilds.Add(1)
	}
	// 构建期间若发生失效(成员/路由变化),丢弃缓存但本次仍可用:结果基于
	// 当时的快照,下一请求会按新代重建。
	s.publishRoutingScheduler(apiKeyID, scheduler, len(eligible), false, generation)
	return scheduler
}

func (s *Store) publishRoutingScheduler(apiKeyID int64, scheduler *FastScheduler, accounts int, alias bool, generation uint64) {
	s.routingSchedulersMu.Lock()
	defer s.routingSchedulersMu.Unlock()
	if s.routingGeneration.Load() != generation {
		return
	}
	if s.routingSchedulers == nil {
		s.routingSchedulers = make(map[int64]*routingSchedulerEntry)
	}
	if _, exists := s.routingSchedulers[apiKeyID]; exists {
		return
	}
	s.ensureRoutingSchedulerCapacityLocked(accounts, alias)
	entry := &routingSchedulerEntry{scheduler: scheduler, accounts: accounts, alias: alias}
	entry.lastHitNS.Store(s.routingSchedulerAccess.Add(1))
	s.routingSchedulers[apiKeyID] = entry
	s.routingSchedulerAccounts += accounts
	if alias {
		s.routingSchedulerAliases++
	}
	s.updateRoutingSchedulerGaugeLocked()
}

func (s *Store) apiKeyHasConfiguredRoutingRestriction(apiKeyID int64) bool {
	if s == nil || apiKeyID <= 0 {
		return false
	}
	s.apiKeyGroupsMu.RLock()
	defer s.apiKeyGroupsMu.RUnlock()
	return len(s.apiKeyAllowedGroupSets[apiKeyID]) > 0 ||
		len(s.apiKeyAllowedPlanSets[apiKeyID]) > 0 ||
		s.apiKeyUpstreamChannels[apiKeyID] != ""
}

// ensureRoutingSchedulerCapacityLocked evicts the least-recently-hit entries
// one at a time instead of wiping the whole cache: deployments with more than
// maxRoutingSchedulerKeys active keys would otherwise thrash every sub-pool on
// each new key.
func (s *Store) ensureRoutingSchedulerCapacityLocked(incomingAccounts int, incomingAlias bool) {
	if incomingAlias {
		for s.routingSchedulerAliases >= maxRoutingSchedulerAliasKeys {
			if !s.evictRoutingSchedulerLocked(true) {
				return
			}
		}
		return
	}
	realEntries := len(s.routingSchedulers) - s.routingSchedulerAliases
	for realEntries >= maxRoutingSchedulerKeys ||
		s.routingSchedulerAccounts+incomingAccounts > maxRoutingSchedulerAccounts {
		if !s.evictRoutingSchedulerLocked(false) {
			return
		}
		realEntries--
	}
}

func (s *Store) evictRoutingSchedulerLocked(alias bool) bool {
	var victimKey int64
	var victim *routingSchedulerEntry
	for key, entry := range s.routingSchedulers {
		if entry.alias != alias {
			continue
		}
		if victim == nil || entry.lastHitNS.Load() < victim.lastHitNS.Load() {
			victimKey = key
			victim = entry
		}
	}
	if victim == nil {
		return false
	}
	delete(s.routingSchedulers, victimKey)
	s.routingSchedulerAccounts -= victim.accounts
	if victim.alias {
		s.routingSchedulerAliases--
	}
	if s.schedulerMetrics != nil {
		s.schedulerMetrics.routingCacheEvictions.Add(1)
	}
	s.updateRoutingSchedulerGaugeLocked()
	return true
}

func (s *Store) updateRoutingSchedulerGaugeLocked() {
	if s.schedulerMetrics == nil {
		return
	}
	s.schedulerMetrics.routingCacheEntries.Store(int64(len(s.routingSchedulers)))
	s.schedulerMetrics.routingCacheAccounts.Store(int64(s.routingSchedulerAccounts))
}

// invalidateRoutingSchedulers is used only for membership/routing generation
// changes. Health, load and cooldown updates remain live through the shared
// Account pointers and do not force a 50k-account cache rebuild.
func (s *Store) invalidateRoutingSchedulers() {
	if s == nil {
		return
	}
	// 无论缓存是否为空都推进代际:锁外进行中的构建必须能感知本次失效。
	s.routingGeneration.Add(1)
	s.routingSchedulersMu.Lock()
	defer s.routingSchedulersMu.Unlock()
	if len(s.routingSchedulers) == 0 {
		return
	}
	s.routingSchedulers = make(map[int64]*routingSchedulerEntry)
	s.routingSchedulerAccounts = 0
	s.routingSchedulerAliases = 0
	s.updateRoutingSchedulerGaugeLocked()
	if s.schedulerMetrics != nil {
		s.schedulerMetrics.routingCacheInvalidations.Add(1)
	}
}
