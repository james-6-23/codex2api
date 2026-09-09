package auth

import "time"

// BillingWindowSnapshot is the reset metadata needed to derive an account's
// active local billing windows. Keeping the three values in one snapshot avoids
// pairing a reset from one upstream observation with a duration from another.
type BillingWindowSnapshot struct {
	Reset5hAt       time.Time
	Reset7dAt       time.Time
	Window7dSeconds int64
}

// BillingWindowSnapshot returns 5h and long-window reset metadata under one
// read lock. A nil account yields the zero snapshot.
func (a *Account) BillingWindowSnapshot() BillingWindowSnapshot {
	if a == nil {
		return BillingWindowSnapshot{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return BillingWindowSnapshot{
		Reset5hAt:       a.Reset5hAt,
		Reset7dAt:       a.Reset7dAt,
		Window7dSeconds: a.Window7dSeconds,
	}
}

// SetUsageSnapshot7dAt publishes one long-window observation atomically. A
// partial upstream response can still refresh the percentage without erasing
// the last usable reset or duration.
func (a *Account) SetUsageSnapshot7dAt(pct float64, resetAt time.Time, windowSeconds int64, updatedAt time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent7d = pct
	a.UsagePercent7dValid = true
	a.UsageUpdatedAt = updatedAt
	// Reset and duration define one boundary and must move together. Updating
	// only the duration would pair a newly observed monthly/weekly period with
	// an older reset and shift the derived billing-window start.
	if !resetAt.IsZero() && windowSeconds > 0 {
		a.Reset7dAt = resetAt
		a.Window7dSeconds = windowSeconds
	}
	if updatedAt.After(a.usageObservedAt) {
		a.usageObservedAt = updatedAt
	}
}
