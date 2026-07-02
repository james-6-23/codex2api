package auth

import "time"

const dispatchCountFallbackWindow = 24 * time.Hour

type DispatchCountSnapshot struct {
	Limit   int64
	Used    int64
	ResetAt time.Time
	Limited bool
}

type dispatchCountReservation struct {
	Allowed  bool
	HitLimit bool
	Limit    int64
	Used     int64
	ResetAt  time.Time
}

func normalizeDispatchCountLimit(limit int64) int64 {
	switch {
	case limit <= 0:
		return 0
	case limit > 1000000:
		return 1000000
	default:
		return limit
	}
}

func (a *Account) SetDispatchCountLimit(limit int64) {
	if a == nil {
		return
	}
	limit = normalizeDispatchCountLimit(limit)
	a.mu.Lock()
	a.DispatchCountLimit = limit
	a.mu.Unlock()
	if limit == 0 {
		a.dispatchCountMu.Lock()
		a.dispatchWindowUsed = 0
		a.dispatchWindowResetAt = time.Time{}
		a.dispatchCountMu.Unlock()
	}
}

func (a *Account) dispatchCountConfig(now time.Time) (int64, time.Time) {
	if a == nil {
		return 0, time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return normalizeDispatchCountLimit(a.DispatchCountLimit), a.dispatchCountResetAtLocked(now)
}

func (a *Account) dispatchCountResetAtLocked(now time.Time) time.Time {
	var resetAt time.Time
	for _, candidate := range []time.Time{a.Reset5hAt, a.Reset7dAt} {
		if candidate.IsZero() || !candidate.After(now) {
			continue
		}
		if resetAt.IsZero() || candidate.Before(resetAt) {
			resetAt = candidate
		}
	}
	if resetAt.IsZero() {
		resetAt = now.Add(dispatchCountFallbackWindow)
	}
	return resetAt
}

func (a *Account) syncDispatchCountWindowLocked(now, resetAt time.Time) {
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(dispatchCountFallbackWindow)
	}
	if a.dispatchWindowResetAt.IsZero() || !now.Before(a.dispatchWindowResetAt) {
		a.dispatchWindowUsed = 0
		a.dispatchWindowResetAt = resetAt
		return
	}
	if !resetAt.Equal(a.dispatchWindowResetAt) {
		a.dispatchWindowResetAt = resetAt
	}
}

func (a *Account) reserveDispatchCount(now time.Time) dispatchCountReservation {
	limit, resetAt := a.dispatchCountConfig(now)
	if limit <= 0 {
		return dispatchCountReservation{Allowed: true}
	}

	a.dispatchCountMu.Lock()
	defer a.dispatchCountMu.Unlock()
	a.syncDispatchCountWindowLocked(now, resetAt)
	if a.dispatchWindowUsed >= limit {
		return dispatchCountReservation{
			Allowed:  false,
			HitLimit: true,
			Limit:    limit,
			Used:     a.dispatchWindowUsed,
			ResetAt:  a.dispatchWindowResetAt,
		}
	}

	a.dispatchWindowUsed++
	return dispatchCountReservation{
		Allowed:  true,
		HitLimit: a.dispatchWindowUsed >= limit,
		Limit:    limit,
		Used:     a.dispatchWindowUsed,
		ResetAt:  a.dispatchWindowResetAt,
	}
}

func (a *Account) GetDispatchCountSnapshot() DispatchCountSnapshot {
	if a == nil {
		return DispatchCountSnapshot{}
	}
	now := time.Now()
	limit, resetAt := a.dispatchCountConfig(now)
	if limit <= 0 {
		return DispatchCountSnapshot{}
	}

	a.dispatchCountMu.Lock()
	defer a.dispatchCountMu.Unlock()
	a.syncDispatchCountWindowLocked(now, resetAt)
	return DispatchCountSnapshot{
		Limit:   limit,
		Used:    a.dispatchWindowUsed,
		ResetAt: a.dispatchWindowResetAt,
		Limited: a.dispatchWindowUsed >= limit,
	}
}
