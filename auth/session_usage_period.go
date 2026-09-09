package auth

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

type AccountSessionUsagePeriod struct {
	AccountID int64
	ID        string
	StartedAt time.Time
}

func (s *Store) ActiveAccountUsagePeriods(accountID int64, now time.Time) map[string]bool {
	active := make(map[string]bool)
	account := s.FindByID(accountID)
	if account == nil {
		return active
	}
	s.ensureAccountSessionsLoaded(account, now)
	_, _, idleTTL := account.SessionCapacityConfig()
	s.accountSessionMu.Lock()
	defer s.accountSessionMu.Unlock()
	for _, state := range s.accountSessions[accountID] {
		if state != nil && state.lastSeen.Add(idleTTL).After(now) {
			active[state.usagePeriodID] = true
		}
	}
	return active
}

func (s *Store) AccountSessionUsagePeriod(accountID int64, sessionKey string, now time.Time) AccountSessionUsagePeriod {
	if s == nil || accountID <= 0 || isSessionAccountingBypassKey(sessionKey) || isProcessLocalSessionAffinityKey(sessionKey) {
		return AccountSessionUsagePeriod{}
	}
	rootKey, _ := RelatedSessionRootKey(sessionKey)
	if strings.TrimSpace(rootKey) == "" {
		return AccountSessionUsagePeriod{}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.accountSessionMu.Lock()
	state := s.accountSessions[accountID][rootKey]
	if state == nil {
		s.accountSessionMu.Unlock()
		return AccountSessionUsagePeriod{}
	}
	created := state.usagePeriodID == "" || state.usageStartedAt.IsZero()
	if created {
		state.usagePeriodID = uuid.NewString()
		state.usageStartedAt = now
	}
	period := AccountSessionUsagePeriod{AccountID: accountID, ID: state.usagePeriodID, StartedAt: state.usageStartedAt}
	s.accountSessionMu.Unlock()
	if created {
		s.persistAccountSessions(accountID, now, rootKey)
	}
	return period
}
