package auth

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	SessionCapacityEnabledCredentialKey = "session_capacity_enabled"
	SessionCapacityMaxCredentialKey     = "session_capacity_max"
	SessionCapacityIdleTTLSecondsKey    = "session_capacity_idle_ttl_seconds"

	DefaultSessionCapacityMax            = int64(5)
	DefaultSessionCapacityIdleTTLSeconds = int64(3600)
	MinSessionCapacityIdleTTLSeconds     = int64(60)
	MaxSessionCapacityIdleTTLSeconds     = int64(30 * 24 * 60 * 60)
)

const UnstableSessionCapacityPrefix = "unstable-affinity:"

// relatedSessionCapacityPrefix includes a process-private nonce so an
// untrusted Session-Id cannot impersonate the internal marker. Related keys are
// runtime-only; their root key remains stable and is the only part used to
// borrow an account/window.
var relatedSessionCapacityPrefix = "related-affinity:" + uuid.NewString() + ":"

// sessionAccountingBypassCapacityPrefix is process-private so a downstream
// Session-Id cannot impersonate a verified gateway decision. The key remains
// a normal affinity identity, but it never enters account-window accounting.
var sessionAccountingBypassCapacityPrefix = "non-accounting-affinity:" + uuid.NewString() + ":"

const maxRelatedRequestDedupeEntries = 512

func RelatedSessionRootKey(sessionKey string) (string, bool) {
	sessionKey = strings.TrimSpace(sessionKey)
	if !strings.HasPrefix(sessionKey, relatedSessionCapacityPrefix) {
		return sessionKey, false
	}
	rootKey := strings.TrimSpace(strings.TrimPrefix(sessionKey, relatedSessionCapacityPrefix))
	return rootKey, rootKey != ""
}

// RelatedSessionAffinityKey creates an authenticated in-process marker for a
// root affinity key. Callers must first prove the native/signed relationship.
func RelatedSessionAffinityKey(rootKey string) string {
	rootKey = strings.TrimSpace(rootKey)
	if rootKey == "" {
		return ""
	}
	return relatedSessionCapacityPrefix + rootKey
}

// SessionAccountingBypassAffinityKey marks a gateway-authenticated background
// request as affinity-capable but excluded from account session capacity.
func SessionAccountingBypassAffinityKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	return sessionAccountingBypassCapacityPrefix + key
}

func isSessionAccountingBypassKey(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), sessionAccountingBypassCapacityPrefix)
}

// AccountSessionOwner is optional, verified downstream identity shown only to
// administrators. Empty fields are expected for standalone Codex2API users.
type AccountSessionOwner struct {
	Platform   string `json:"platform,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	UserName   string `json:"user_name,omitempty"`
	UserEmail  string `json:"user_email,omitempty"`
	APIKeyID   int64  `json:"api_key_id,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
}

// AccountSessionSnapshot is a sanitized runtime view of one account-bound
// conversation. SessionID contains the downstream affinity identity, never a
// bearer credential.
type AccountSessionSnapshot struct {
	SessionID           string                        `json:"session_id"`
	LastSeen            time.Time                     `json:"last_seen"`
	ExpiresAt           time.Time                     `json:"expires_at"`
	RemainingSeconds    int64                         `json:"remaining_seconds"`
	Owner               AccountSessionOwner           `json:"owner,omitempty"`
	RelatedRequestCount int64                         `json:"related_request_count,omitempty"`
	RelatedSources      []AccountSessionRelatedSource `json:"related_sources,omitempty"`
}

type AccountSessionRelatedSource struct {
	ThreadSource string `json:"thread_source,omitempty"`
	RequestKind  string `json:"request_kind,omitempty"`
	SubagentKind string `json:"subagent_kind,omitempty"`
	Count        int64  `json:"count"`
}

type accountSessionState struct {
	sessionID             string
	lastSeen              time.Time
	owner                 AccountSessionOwner
	relatedRequestCount   int64
	relatedSources        map[string]*AccountSessionRelatedSource
	relatedRequestIDs     map[string]struct{}
	relatedRequestIDOrder []string
}

func normalizeSessionCapacityMax(value int64) int64 {
	if value <= 0 {
		return DefaultSessionCapacityMax
	}
	return value
}

func normalizeSessionCapacityIdleTTLSeconds(value int64) int64 {
	if value <= 0 {
		return DefaultSessionCapacityIdleTTLSeconds
	}
	if value < MinSessionCapacityIdleTTLSeconds {
		return MinSessionCapacityIdleTTLSeconds
	}
	if value > MaxSessionCapacityIdleTTLSeconds {
		return MaxSessionCapacityIdleTTLSeconds
	}
	return value
}

func (a *Account) SessionCapacityConfig() (enabled bool, limit int64, idleTTL time.Duration) {
	if a == nil || a.IsRelayStyle() {
		return false, 0, 0
	}
	a.mu.RLock()
	enabled = a.SessionCapacityEnabled
	limit = normalizeSessionCapacityMax(a.SessionCapacityMax)
	idleSeconds := normalizeSessionCapacityIdleTTLSeconds(a.SessionCapacityIdleTTLSeconds)
	a.mu.RUnlock()
	return enabled, limit, time.Duration(idleSeconds) * time.Second
}

func (s *Store) purgeExpiredAccountSessionsLocked(accountID int64, idleTTL time.Duration, now time.Time) {
	bySession := s.accountSessions[accountID]
	for key, state := range bySession {
		if state == nil || !state.lastSeen.Add(idleTTL).After(now) {
			delete(bySession, key)
		}
	}
	if len(bySession) == 0 {
		delete(s.accountSessions, accountID)
	}
}

// AdmitAccountSession atomically reuses or creates an idle-expiring session
// slot for an account. Disabled and relay accounts are intentionally no-ops.
func (s *Store) AdmitAccountSession(account *Account, sessionKey string, now time.Time) bool {
	if s == nil || account == nil {
		return false
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if strings.HasPrefix(sessionKey, UnstableSessionCapacityPrefix) || isSessionAccountingBypassKey(sessionKey) {
		return true
	}
	if _, related := RelatedSessionRootKey(sessionKey); related {
		return true
	}
	enabled, limit, idleTTL := account.SessionCapacityConfig()
	if !enabled || sessionKey == "" {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}

	s.accountSessionMu.Lock()
	defer s.accountSessionMu.Unlock()
	if s.accountSessions == nil {
		s.accountSessions = make(map[int64]map[string]*accountSessionState)
	}
	s.purgeExpiredAccountSessionsLocked(account.DBID, idleTTL, now)
	bySession := s.accountSessions[account.DBID]
	if bySession == nil {
		bySession = make(map[string]*accountSessionState)
		s.accountSessions[account.DBID] = bySession
	}
	if state := bySession[sessionKey]; state != nil {
		state.lastSeen = now
		return true
	}
	if int64(len(bySession)) >= limit {
		return false
	}
	bySession[sessionKey] = &accountSessionState{sessionID: sessionKey, lastSeen: now}
	return true
}

func (s *Store) CanAdmitAccountSession(account *Account, sessionKey string, now time.Time) bool {
	if s == nil || account == nil {
		return false
	}
	sessionKey = strings.TrimSpace(sessionKey)
	enabled, limit, idleTTL := account.SessionCapacityConfig()
	if !enabled || sessionKey == "" || strings.HasPrefix(sessionKey, UnstableSessionCapacityPrefix) || isSessionAccountingBypassKey(sessionKey) {
		return true
	}
	if _, related := RelatedSessionRootKey(sessionKey); related {
		return true
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.accountSessionMu.Lock()
	defer s.accountSessionMu.Unlock()
	s.purgeExpiredAccountSessionsLocked(account.DBID, idleTTL, now)
	bySession := s.accountSessions[account.DBID]
	if bySession[sessionKey] != nil {
		return true
	}
	return int64(len(bySession)) < limit
}

// HasSessionCapacityExhaustionWithDispatch reports capacity exhaustion only
// when every account that would otherwise be eligible for this request denies
// the new session. This prevents an unrelated full account from turning a
// model/channel availability failure into a misleading session-capacity 429.
func (s *Store) HasSessionCapacityExhaustionWithDispatch(apiKeyID int64, exclude map[int64]bool, filter AccountFilter, policy DispatchPolicy, sessionKey string, now time.Time) bool {
	if s == nil || strings.TrimSpace(sessionKey) == "" || strings.HasPrefix(sessionKey, UnstableSessionCapacityPrefix) || isSessionAccountingBypassKey(sessionKey) {
		return false
	}
	if _, related := RelatedSessionRootKey(sessionKey); related {
		return false
	}
	filter = s.withUsableEgressFilter(filter)
	maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)
	foundCandidate := false
	for _, account := range s.Accounts() {
		if account == nil || (exclude != nil && exclude[account.DBID]) {
			continue
		}
		if !account.dispatchableForPolicy(policy) {
			continue
		}
		if policy == DispatchPolicyStandard && s.GetLazyMode() && !s.accountLazySelectable(account) {
			continue
		}
		if s.accountHasBlockingCachedCooldown(account, policy) || !s.accountAllowedForAPIKey(account, apiKeyID) {
			continue
		}
		if filter != nil && !filter(account) {
			continue
		}
		_, _, _, concurrencyLimit := account.schedulerSnapshotForPolicy(maxConcurrency, policy)
		if concurrencyLimit <= 0 {
			continue
		}
		foundCandidate = true
		if s.CanAdmitAccountSession(account, sessionKey, now) {
			return false
		}
	}
	return foundCandidate
}

func (s *Store) TouchAccountSession(accountID int64, sessionKey string, now time.Time) {
	if s == nil || accountID <= 0 || strings.TrimSpace(sessionKey) == "" || isSessionAccountingBypassKey(sessionKey) {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if _, related := RelatedSessionRootKey(sessionKey); related {
		return
	}
	s.accountSessionMu.Lock()
	if state := s.accountSessions[accountID][sessionKey]; state != nil {
		state.lastSeen = now
	}
	s.accountSessionMu.Unlock()
}

// AccountSessionAccountID returns the account that currently owns sessionKey.
// Expired entries are removed before matching.
func (s *Store) AccountSessionAccountID(sessionKey string, now time.Time) (int64, bool) {
	if isSessionAccountingBypassKey(sessionKey) {
		return 0, false
	}
	if rootKey, related := RelatedSessionRootKey(sessionKey); related {
		sessionKey = rootKey
	}
	if s == nil || strings.TrimSpace(sessionKey) == "" {
		return 0, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	s.accountSessionMu.Lock()
	defer s.accountSessionMu.Unlock()
	for accountID := range s.accountSessions {
		account := s.FindByID(accountID)
		if account == nil {
			delete(s.accountSessions, accountID)
			continue
		}
		enabled, _, idleTTL := account.SessionCapacityConfig()
		if !enabled {
			delete(s.accountSessions, accountID)
			continue
		}
		s.purgeExpiredAccountSessionsLocked(accountID, idleTTL, now)
		if state := s.accountSessions[accountID][sessionKey]; state != nil {
			return accountID, true
		}
	}
	return 0, false
}

func (s *Store) SetAccountSessionOwner(accountID int64, sessionKey string, owner AccountSessionOwner) {
	if s == nil || accountID <= 0 || strings.TrimSpace(sessionKey) == "" || isSessionAccountingBypassKey(sessionKey) {
		return
	}
	if _, related := RelatedSessionRootKey(sessionKey); related {
		return
	}
	s.accountSessionMu.Lock()
	if state := s.accountSessions[accountID][sessionKey]; state != nil {
		state.owner = owner
	}
	s.accountSessionMu.Unlock()
}

// RecordRelatedAccountSession attributes one actually dispatched internal
// request to its active root window. requestID is a logical request identity;
// retries with the same value are counted once. Missing/expired roots are
// deliberately ignored rather than manufacturing a new account window.
func (s *Store) RecordRelatedAccountSession(accountID int64, sessionKey string, source AccountSessionRelatedSource, requestID string) {
	rootKey, related := RelatedSessionRootKey(sessionKey)
	if s == nil || accountID <= 0 || !related {
		return
	}
	source.ThreadSource = normalizeRelatedSessionLabel(source.ThreadSource, 128)
	source.RequestKind = normalizeRelatedSessionLabel(source.RequestKind, 128)
	source.SubagentKind = normalizeRelatedSessionLabel(source.SubagentKind, 64)
	requestID = normalizeRelatedSessionLabel(requestID, 256)

	s.accountSessionMu.Lock()
	defer s.accountSessionMu.Unlock()
	state := s.accountSessions[accountID][rootKey]
	if state == nil {
		return
	}
	if requestID != "" {
		if state.relatedRequestIDs == nil {
			state.relatedRequestIDs = make(map[string]struct{})
		}
		if _, exists := state.relatedRequestIDs[requestID]; exists {
			return
		}
		state.relatedRequestIDs[requestID] = struct{}{}
		state.relatedRequestIDOrder = append(state.relatedRequestIDOrder, requestID)
		if len(state.relatedRequestIDOrder) > maxRelatedRequestDedupeEntries {
			oldest := state.relatedRequestIDOrder[0]
			delete(state.relatedRequestIDs, oldest)
			state.relatedRequestIDOrder = state.relatedRequestIDOrder[1:]
		}
	}
	state.relatedRequestCount++
	key := source.ThreadSource + "\x00" + source.RequestKind + "\x00" + source.SubagentKind
	if state.relatedSources == nil {
		state.relatedSources = make(map[string]*AccountSessionRelatedSource)
	}
	item := state.relatedSources[key]
	if item == nil {
		item = &AccountSessionRelatedSource{
			ThreadSource: source.ThreadSource,
			RequestKind:  source.RequestKind,
			SubagentKind: source.SubagentKind,
		}
		state.relatedSources[key] = item
	}
	item.Count++
}

func normalizeRelatedSessionLabel(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if value == "" || maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	for _, char := range runes {
		if char < 0x20 || char == 0x7f {
			return ""
		}
	}
	return string(runes)
}

func (s *Store) RemoveAccountSession(accountID int64, sessionKey string) bool {
	if s == nil || accountID <= 0 || strings.TrimSpace(sessionKey) == "" || isSessionAccountingBypassKey(sessionKey) {
		return false
	}
	if _, related := RelatedSessionRootKey(sessionKey); related {
		return false
	}
	s.accountSessionMu.Lock()
	defer s.accountSessionMu.Unlock()
	bySession := s.accountSessions[accountID]
	if _, ok := bySession[sessionKey]; !ok {
		return false
	}
	delete(bySession, sessionKey)
	if len(bySession) == 0 {
		delete(s.accountSessions, accountID)
	}
	return true
}

func (s *Store) ClearAccountSessions(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.accountSessionMu.Lock()
	delete(s.accountSessions, accountID)
	s.accountSessionMu.Unlock()
}

func (s *Store) AccountSessionSnapshots(accountID int64, now time.Time) []AccountSessionSnapshot {
	if s == nil || accountID <= 0 {
		return nil
	}
	account := s.FindByID(accountID)
	if account == nil {
		return nil
	}
	enabled, _, idleTTL := account.SessionCapacityConfig()
	if !enabled {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	s.accountSessionMu.Lock()
	s.purgeExpiredAccountSessionsLocked(accountID, idleTTL, now)
	bySession := s.accountSessions[accountID]
	items := make([]AccountSessionSnapshot, 0, len(bySession))
	for _, state := range bySession {
		expiresAt := state.lastSeen.Add(idleTTL)
		remaining := int64(expiresAt.Sub(now).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		relatedSources := make([]AccountSessionRelatedSource, 0, len(state.relatedSources))
		for _, source := range state.relatedSources {
			if source != nil {
				relatedSources = append(relatedSources, *source)
			}
		}
		sort.Slice(relatedSources, func(i, j int) bool {
			if relatedSources[i].Count != relatedSources[j].Count {
				return relatedSources[i].Count > relatedSources[j].Count
			}
			left := relatedSources[i].ThreadSource + "\x00" + relatedSources[i].RequestKind + "\x00" + relatedSources[i].SubagentKind
			right := relatedSources[j].ThreadSource + "\x00" + relatedSources[j].RequestKind + "\x00" + relatedSources[j].SubagentKind
			return left < right
		})
		items = append(items, AccountSessionSnapshot{
			SessionID: state.sessionID, LastSeen: state.lastSeen, ExpiresAt: expiresAt,
			RemainingSeconds: remaining, Owner: state.owner,
			RelatedRequestCount: state.relatedRequestCount, RelatedSources: relatedSources,
		})
	}
	s.accountSessionMu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].LastSeen.After(items[j].LastSeen) })
	return items
}

func (s *Store) AccountSessionCount(accountID int64, now time.Time) int64 {
	return int64(len(s.AccountSessionSnapshots(accountID, now)))
}

// accountWindowCountsForScheduling returns one local active-window count per
// candidate account. Accounts with the explicit capacity feature enabled use
// its idle-expiring records (the same source shown by the account badge).
// Other accounts fall back to active affinity bindings so window balancing is
// still meaningful without forcing every account to enable a hard limit.
func (s *Store) accountWindowCountsForScheduling(accounts []*Account, now time.Time) map[int64]int64 {
	counts := make(map[int64]int64, len(accounts))
	if s == nil {
		return counts
	}
	if now.IsZero() {
		now = time.Now()
	}

	capacityEnabled := make(map[int64]time.Duration, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		if enabled, _, idleTTL := account.SessionCapacityConfig(); enabled {
			capacityEnabled[account.DBID] = idleTTL
		}
	}

	// Do not hold sessionMu and accountSessionMu at the same time. Keeping the
	// snapshots separate preserves the Store lock order used by bind/unbind.
	s.sessionMu.Lock()
	for key, binding := range s.sessionBindings {
		if !binding.expiresAt.After(now) {
			delete(s.sessionBindings, key)
			continue
		}
		if _, related := RelatedSessionRootKey(key); related {
			continue
		}
		if isSessionAccountingBypassKey(key) {
			continue
		}
		if _, explicit := capacityEnabled[binding.accountID]; !explicit {
			counts[binding.accountID]++
		}
	}
	s.sessionMu.Unlock()

	s.accountSessionMu.Lock()
	for accountID, idleTTL := range capacityEnabled {
		s.purgeExpiredAccountSessionsLocked(accountID, idleTTL, now)
		counts[accountID] = int64(len(s.accountSessions[accountID]))
	}
	s.accountSessionMu.Unlock()
	return counts
}

func (s *Store) ApplyAccountSessionCapacity(dbID int64, enabled bool, limit, idleTTLSeconds int64) bool {
	if s == nil || dbID <= 0 {
		return false
	}
	account := s.FindByID(dbID)
	if account == nil {
		return false
	}
	account.mu.Lock()
	account.SessionCapacityEnabled = enabled
	account.SessionCapacityMax = normalizeSessionCapacityMax(limit)
	account.SessionCapacityIdleTTLSeconds = normalizeSessionCapacityIdleTTLSeconds(idleTTLSeconds)
	account.mu.Unlock()
	if !enabled {
		s.ClearAccountSessions(dbID)
	}
	return true
}
