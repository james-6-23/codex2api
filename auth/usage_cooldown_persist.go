package auth

import (
	"context"
	"log"
	"time"
)

// persistUsageCooldown retries a failed usage-owned cooldown write without
// treating the retry as a new failure or changing the runtime deadline.
func (s *Store) persistUsageCooldown(acc *Account) {
	s.persistAccountCooldownState(acc, false, false)
}

// All native account cooldown writers/clearers publish through this critical
// section. Read current state only after acquiring the publication lock: a
// delayed frame, 401, or clear must never publish a captured older state.
// Never hold this lock while acquiring acc.usageSnapshotPersistMu.
func (s *Store) persistAccountCooldownState(acc *Account, force, clearError bool) {
	if s == nil || acc == nil {
		return
	}
	acc.usageCooldownPersistMu.Lock()
	defer acc.usageCooldownPersistMu.Unlock()
	acc.mu.RLock()
	pending := acc.usageCooldownPersistPending
	status, errorMsg := acc.Status, acc.ErrorMsg
	reason, until := acc.CooldownReason, acc.CooldownUtil
	acc.mu.RUnlock()
// A failed clear must also be retried: if the DB write of a runtime clear is
// lost, the stale persisted cooldown survives and any later scheduler-outbox
// reload (issue #813) resurrects it, blocking a healthy account with 429s.
// The guard therefore only requires a pending write, for any runtime status.
	if !force && !pending {
		return
	}
	if status == StatusCooldown {
		s.setCachedAccountCooldown(acc.DBID, reason, until)
	} else {
		s.deleteCachedAccountCooldown(acc.DBID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if s.db != nil {
		switch status {
		case StatusCooldown:
			if errorMsg != "" {
				err = s.db.SetCooldownWithError(ctx, acc.DBID, reason, until, errorMsg)
			} else {
				err = s.db.SetCooldown(ctx, acc.DBID, reason, until)
			}
		case StatusError:
			err = s.db.SetError(ctx, acc.DBID, errorMsg)
		default:
			if clearError {
				err = s.db.ClearError(ctx, acc.DBID)
			} else {
				err = s.db.ClearCooldown(ctx, acc.DBID)
			}
		}
	}
	if err != nil {
		log.Printf("[账号 %d] 持久化用量冷却失败: %v", acc.DBID, err)
		// Keep a failed write pending so a later usage-frame publication
		// retries it (issue #813). Otherwise a lost clear leaves the stale
		// cooldown in the database forever.
		acc.mu.Lock()
		acc.usageCooldownPersistPending = true
		acc.mu.Unlock()
		return
	}
	acc.mu.Lock()
	if acc.Status == status && acc.CooldownReason == reason && acc.CooldownUtil.Equal(until) {
		acc.usageCooldownPersistPending = false
	}
	acc.mu.Unlock()
}
