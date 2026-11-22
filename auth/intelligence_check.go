package auth

import (
	"context"
	"log"
	"sync/atomic"
)

type intelligencePausesKey struct{}

// Attach one snapshot to bulk account loads instead of querying once per account.
func (s *Store) withIntelligencePauses(ctx context.Context) (context.Context, error) {
	if s.db == nil {
		return ctx, nil
	}
	ids, err := s.db.ListIntelligenceCheckPausedAccountIDs(ctx)
	if err != nil {
		return ctx, err
	}
	paused := make(map[int64]bool, len(ids))
	for _, id := range ids {
		paused[id] = true
	}
	return context.WithValue(ctx, intelligencePausesKey{}, paused), nil
}
func (s *Store) intelligencePaused(ctx context.Context, id int64) bool {
	if paused, ok := ctx.Value(intelligencePausesKey{}).(map[int64]bool); ok {
		return paused[id]
	}
	if s.db == nil {
		return false
	}
	paused, err := s.db.IsIntelligenceCheckPaused(ctx, id)
	if err != nil {
		log.Printf("读取账号 %d 检测暂停失败: %v", id, err)
		return true
	}
	return paused
}

// RefreshIntelligenceAccount publishes committed action/group changes through
// the same snapshot path as cross-instance outbox delivery.
func (s *Store) RefreshIntelligenceAccount(ctx context.Context, id int64) error {
	return s.reloadDispatchAccountByID(ctx, id)
}

// TryAcquireIntelligenceCheck reserves an ordinary occupied slot but ignores
// administrative/quality pauses so recovery probes and explicit manual checks
// can run; the queue already keeps automatic checks off manually disabled
// accounts. It never changes either pause or the account's
// authentication/quota status.
func (s *Store) TryAcquireIntelligenceCheck(a *Account) bool {
	if a == nil || atomic.LoadInt32(&a.Disabled) != 0 {
		return false
	}
	a.mu.RLock()
	limit := a.effectiveBaseConcurrencyLocked(s.maxConcurrency.Load())
	a.mu.RUnlock()
	for {
		occupied := a.OccupiedRequests.Load()
		if occupied >= limit || a.ActiveRequests.Load() >= limit {
			return false
		}
		if a.OccupiedRequests.CompareAndSwap(occupied, occupied+1) {
			a.ActiveRequests.Add(1)
			if atomic.LoadInt32(&a.Disabled) != 0 {
				releaseOccupiedAccountSlot(a)
				return false
			}
			s.fastSchedulerUpdate(a)
			return true
		}
	}
}
