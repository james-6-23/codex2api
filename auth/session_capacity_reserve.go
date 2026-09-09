package auth

import "time"

const SessionCapacityReservedCredentialKey = "session_capacity_reserved"

type AccountSessionCapacity struct {
	Enabled  bool
	Total    int64
	Reserved int64
	IdleTTL  time.Duration
}

func (account *Account) SessionCapacityLimits() AccountSessionCapacity {
	if account == nil || account.IsRelayStyle() {
		return AccountSessionCapacity{}
	}
	account.mu.RLock()
	defer account.mu.RUnlock()
	total := normalizeSessionCapacityMax(account.SessionCapacityMax)
	return AccountSessionCapacity{
		Enabled: account.SessionCapacityEnabled, Total: total,
		Reserved: max(0, min(total, account.SessionCapacityReserved)),
		IdleTTL:  time.Duration(normalizeSessionCapacityIdleTTLSeconds(account.SessionCapacityIdleTTLSeconds)) * time.Second,
	}
}

func accountSessionSlotAvailable(total, reserved int64, limits AccountSessionCapacity, expanded bool) (bool, bool) {
	if total >= limits.Total {
		return false, false
	}
	if expanded && reserved < limits.Reserved {
		return true, true
	}
	return false, total-reserved < limits.Total-limits.Reserved
}

func (store *Store) AccountSessionSlotCounts(accountID int64, now time.Time) (int64, int64) {
	if store == nil || accountID <= 0 {
		return 0, 0
	}
	account := store.FindByID(accountID)
	limits := account.SessionCapacityLimits()
	if !limits.Enabled {
		return 0, 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	store.ensureAccountSessionsLoaded(account, now)
	store.accountSessionMu.Lock()
	defer store.accountSessionMu.Unlock()
	reserved := store.purgeExpiredAccountSessionsLocked(accountID, limits.IdleTTL, now)
	return int64(len(store.accountSessions[accountID])), reserved
}
