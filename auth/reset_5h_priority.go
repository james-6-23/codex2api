package auth

import (
	"math"
	"time"
)

// Keep the persisted value within system_settings.scheduler_mode VARCHAR(20).
const SchedulerModeReset5hFirst = "reset_5h_first"

// reset5hSchedulingDeadline uses the observed Codex window, never a synthetic
// now+5h cycle. Eligibility, permissions and concurrency remain separate gates.
func (a *Account) reset5hSchedulingDeadline(now time.Time) time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.isRelayStyleLocked() || a.creditSkipsUsageWindowLocked() ||
		!a.UsagePercent5hValid || math.IsNaN(a.UsagePercent5h) || math.IsInf(a.UsagePercent5h, 0) ||
		a.UsagePercent5h < 0 || a.UsagePercent5h >= 100 || !a.Reset5hAt.After(now) {
		return time.Time{}
	}
	return a.Reset5hAt
}

// compareReset5hDeadlines orders known future windows before unknown ones.
// Equal/unknown keys defer to the existing score/load ordering.
func compareReset5hDeadlines(a, b time.Time) int {
	if a.Equal(b) {
		return 0
	}
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return 1
	}
	return -1
}

// refreshReset5hOrderLocked snapshots all keys against one clock instant so
// sorting is transitive even at a reset boundary. Only a changed bucket or the
// next expired deadline needs this O(N) refresh; ordinary selections do not.
func (s *FastScheduler) refreshReset5hOrderLocked(tier AccountHealthTier, now time.Time) {
	var next time.Time
	for i := range s.buckets[tier] {
		entry := &s.buckets[tier][i]
		entry.reset5hAt = entry.acc.reset5hSchedulingDeadline(now)
		if !entry.reset5hAt.IsZero() && (next.IsZero() || entry.reset5hAt.Before(next)) {
			next = entry.reset5hAt
		}
	}
	if s.reset5hOrderExpires == nil {
		s.reset5hOrderExpires = make(map[AccountHealthTier]time.Time)
	}
	s.reset5hOrderExpires[tier] = next
}

func (s *FastScheduler) trackReset5hDeadlineLocked(tier AccountHealthTier, deadline time.Time) {
	if s.schedulerMode != SchedulerModeReset5hFirst || deadline.IsZero() {
		return
	}
	if s.reset5hOrderExpires == nil {
		s.reset5hOrderExpires = make(map[AccountHealthTier]time.Time)
	}
	if next := s.reset5hOrderExpires[tier]; next.IsZero() || deadline.Before(next) {
		s.reset5hOrderExpires[tier] = deadline
	}
}

// Sparse API-key schedulers cache ordering independently of the global index.
// Update only already-present accounts; inserting here would bypass membership.
func (s *Store) refreshRoutingReset5hAccount(acc *Account) {
	s.routingSchedulersMu.RLock()
	var schedulers []*FastScheduler
	for _, entry := range s.routingSchedulers {
		if !entry.alias {
			schedulers = append(schedulers, entry.scheduler)
		}
	}
	s.routingSchedulersMu.RUnlock()
	for _, scheduler := range schedulers {
		scheduler.mu.Lock()
		if _, present := scheduler.positions[acc.ID()]; present {
			scheduler.updateLocked(acc, time.Now())
		}
		scheduler.mu.Unlock()
	}
}
