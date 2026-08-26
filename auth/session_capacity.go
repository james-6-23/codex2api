package auth

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strconv"
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

// protectedRelatedSessionCapacityPrefix is reserved for a locally resolved or
// gateway-signed internal child request. It may borrow one extra concurrency
// lease from its exact root account so an internal turn cannot deadlock behind
// the main turn that is waiting for it.
var protectedRelatedSessionCapacityPrefix = "protected-related-affinity:" + uuid.NewString() + ":"

// sessionAccountingBypassCapacityPrefix is process-private so a downstream
// Session-Id cannot impersonate a verified gateway decision. The key remains
// a normal affinity identity, but it never enters account-window accounting.
var sessionAccountingBypassCapacityPrefix = "non-accounting-affinity:" + uuid.NewString() + ":"

const maxRelatedRequestDedupeEntries = 512

const (
	accountSessionRuntimeNamespace      = "account-session-state-v1"
	accountSessionOwnerRuntimeNamespace = "account-session-owner-v1"
	accountSessionCacheTimeout          = 500 * time.Millisecond
	accountSessionPersistInterval       = 30 * time.Second
	accountSessionLockStripes           = 64
)

func RelatedSessionRootKey(sessionKey string) (string, bool) {
	sessionKey = strings.TrimSpace(sessionKey)
	for _, prefix := range []string{protectedRelatedSessionCapacityPrefix, relatedSessionCapacityPrefix} {
		if strings.HasPrefix(sessionKey, prefix) {
			rootKey := strings.TrimSpace(strings.TrimPrefix(sessionKey, prefix))
			return rootKey, rootKey != ""
		}
	}
	return sessionKey, false
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

// ProtectedRelatedSessionAffinityKey creates the process-private marker used
// only after the proxy has classified a related internal session field.
func ProtectedRelatedSessionAffinityKey(rootKey string) string {
	rootKey = strings.TrimSpace(rootKey)
	if rootKey == "" {
		return ""
	}
	return protectedRelatedSessionCapacityPrefix + rootKey
}

func isProtectedRelatedSessionKey(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), protectedRelatedSessionCapacityPrefix)
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
	lastPersisted         time.Time
	owner                 AccountSessionOwner
	relatedRequestCount   int64
	relatedSources        map[string]*AccountSessionRelatedSource
	relatedRequestIDs     map[string]struct{}
	relatedRequestIDOrder []string
}

type persistedAccountSessionState struct {
	SessionID           string                        `json:"session_id"`
	LastSeen            time.Time                     `json:"last_seen"`
	Owner               AccountSessionOwner           `json:"owner,omitempty"`
	RelatedRequestCount int64                         `json:"related_request_count,omitempty"`
	RelatedSources      []AccountSessionRelatedSource `json:"related_sources,omitempty"`
	RelatedRequestIDs   []string                      `json:"related_request_ids,omitempty"`
}

type persistedAccountSessionCollection struct {
	Version  int                            `json:"version"`
	Sessions []persistedAccountSessionState `json:"sessions"`
}

type persistedAccountSessionOwner struct {
	AccountID int64 `json:"account_id"`
}

func accountSessionRuntimeKey(accountID int64) string {
	return strconv.FormatInt(accountID, 10)
}

func accountSessionLockIndex(accountID int64) int {
	return int(uint64(accountID) % accountSessionLockStripes)
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

func (s *Store) ensureAccountSessionsLoaded(account *Account, now time.Time) {
	if s == nil || account == nil || s.tokenCache == nil {
		return
	}
	enabled, _, idleTTL := account.SessionCapacityConfig()
	if !enabled {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	loadMu := &s.accountSessionLoadMu[accountSessionLockIndex(account.DBID)]
	loadMu.Lock()
	defer loadMu.Unlock()
	s.accountSessionMu.Lock()
	if s.accountSessionsHydrated[account.DBID] {
		s.accountSessionMu.Unlock()
		return
	}
	s.accountSessionMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), accountSessionCacheTimeout)
	raw, found, err := s.tokenCache.GetRuntime(ctx, accountSessionRuntimeNamespace, accountSessionRuntimeKey(account.DBID))
	cancel()
	if err != nil {
		log.Printf("读取账号会话窗口缓存失败: account=%d err=%v", account.DBID, err)
		return
	}
	collection := persistedAccountSessionCollection{}
	if found {
		if err := json.Unmarshal(raw, &collection); err != nil {
			log.Printf("解析账号会话窗口缓存失败: account=%d err=%v", account.DBID, err)
			found = false
		}
	}

	recovered := make(map[string]*accountSessionState, len(collection.Sessions))
	if found {
		for _, item := range collection.Sessions {
			sessionID := strings.TrimSpace(item.SessionID)
			if sessionID == "" || item.LastSeen.IsZero() || !item.LastSeen.Add(idleTTL).After(now) ||
				strings.HasPrefix(sessionID, UnstableSessionCapacityPrefix) || isSessionAccountingBypassKey(sessionID) {
				continue
			}
			if _, related := RelatedSessionRootKey(sessionID); related {
				continue
			}
			state := &accountSessionState{
				sessionID:           sessionID,
				lastSeen:            item.LastSeen,
				owner:               item.Owner,
				relatedRequestCount: item.RelatedRequestCount,
			}
			if len(item.RelatedSources) > 0 {
				state.relatedSources = make(map[string]*AccountSessionRelatedSource, len(item.RelatedSources))
				for _, source := range item.RelatedSources {
					sourceCopy := source
					key := source.ThreadSource + "\x00" + source.RequestKind + "\x00" + source.SubagentKind
					state.relatedSources[key] = &sourceCopy
				}
			}
			if len(item.RelatedRequestIDs) > 0 {
				start := 0
				if len(item.RelatedRequestIDs) > maxRelatedRequestDedupeEntries {
					start = len(item.RelatedRequestIDs) - maxRelatedRequestDedupeEntries
				}
				state.relatedRequestIDs = make(map[string]struct{}, len(item.RelatedRequestIDs)-start)
				for _, requestID := range item.RelatedRequestIDs[start:] {
					requestID = normalizeRelatedSessionLabel(requestID, 256)
					if requestID == "" {
						continue
					}
					if _, exists := state.relatedRequestIDs[requestID]; exists {
						continue
					}
					state.relatedRequestIDs[requestID] = struct{}{}
					state.relatedRequestIDOrder = append(state.relatedRequestIDOrder, requestID)
				}
			}
			recovered[sessionID] = state
		}
	}

	s.accountSessionMu.Lock()
	if s.accountSessions == nil {
		s.accountSessions = make(map[int64]map[string]*accountSessionState)
	}
	if s.accountSessionsHydrated == nil {
		s.accountSessionsHydrated = make(map[int64]bool)
	}
	bySession := s.accountSessions[account.DBID]
	if bySession == nil && len(recovered) > 0 {
		bySession = make(map[string]*accountSessionState, len(recovered))
		s.accountSessions[account.DBID] = bySession
	}
	for sessionID, state := range recovered {
		if bySession[sessionID] == nil {
			bySession[sessionID] = state
		}
	}
	s.accountSessionsHydrated[account.DBID] = true
	s.accountSessionMu.Unlock()
}

func (s *Store) persistAccountSessions(accountID int64, now time.Time, reconcileSessionIDs ...string) bool {
	if s == nil || s.tokenCache == nil || accountID <= 0 {
		return false
	}
	persistMu := &s.accountSessionPersistMu[accountSessionLockIndex(accountID)]
	persistMu.Lock()
	defer persistMu.Unlock()
	account := s.FindByID(accountID)
	if account == nil {
		return false
	}
	enabled, _, idleTTL := account.SessionCapacityConfig()
	if !enabled {
		success := true
		ctx, cancel := context.WithTimeout(context.Background(), accountSessionCacheTimeout)
		if err := s.tokenCache.DeleteRuntime(ctx, accountSessionRuntimeNamespace, accountSessionRuntimeKey(accountID)); err != nil {
			log.Printf("删除已关闭账号的会话窗口缓存失败: account=%d err=%v", accountID, err)
			success = false
		}
		for _, sessionID := range reconcileSessionIDs {
			if err := s.tokenCache.DeleteRuntime(ctx, accountSessionOwnerRuntimeNamespace, sessionID); err != nil {
				success = false
			}
		}
		cancel()
		return success
	}
	if now.IsZero() {
		now = time.Now()
	}

	s.accountSessionMu.Lock()
	bySession := s.accountSessions[accountID]
	collection := persistedAccountSessionCollection{Version: 1, Sessions: make([]persistedAccountSessionState, 0, len(bySession))}
	maxRemaining := time.Duration(0)
	remainingBySession := make(map[string]time.Duration, len(bySession))
	for _, state := range bySession {
		if state == nil || !state.lastSeen.Add(idleTTL).After(now) {
			continue
		}
		relatedSources := make([]AccountSessionRelatedSource, 0, len(state.relatedSources))
		for _, source := range state.relatedSources {
			if source != nil {
				relatedSources = append(relatedSources, *source)
			}
		}
		collection.Sessions = append(collection.Sessions, persistedAccountSessionState{
			SessionID: state.sessionID, LastSeen: state.lastSeen, Owner: state.owner,
			RelatedRequestCount: state.relatedRequestCount, RelatedSources: relatedSources,
			RelatedRequestIDs: append([]string(nil), state.relatedRequestIDOrder...),
		})
		if remaining := state.lastSeen.Add(idleTTL).Sub(now); remaining > maxRemaining {
			maxRemaining = remaining
		}
		remainingBySession[state.sessionID] = state.lastSeen.Add(idleTTL).Sub(now)
	}
	s.accountSessionMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), accountSessionCacheTimeout)
	defer cancel()
	success := true
	if len(collection.Sessions) == 0 || maxRemaining <= 0 {
		if err := s.tokenCache.DeleteRuntime(ctx, accountSessionRuntimeNamespace, accountSessionRuntimeKey(accountID)); err != nil {
			log.Printf("删除账号会话窗口缓存失败: account=%d err=%v", accountID, err)
			success = false
		}
	} else {
		payload, err := json.Marshal(collection)
		if err != nil {
			log.Printf("序列化账号会话窗口缓存失败: account=%d err=%v", accountID, err)
			return false
		}
		if err := s.tokenCache.SetRuntime(ctx, accountSessionRuntimeNamespace, accountSessionRuntimeKey(accountID), payload, maxRemaining); err != nil {
			log.Printf("写入账号会话窗口缓存失败: account=%d err=%v", accountID, err)
			success = false
		}
	}
	ownerPayload, err := json.Marshal(persistedAccountSessionOwner{AccountID: accountID})
	if err != nil {
		return false
	}
	seen := make(map[string]struct{}, len(reconcileSessionIDs))
	for _, sessionID := range reconcileSessionIDs {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			continue
		}
		if _, exists := seen[sessionID]; exists {
			continue
		}
		seen[sessionID] = struct{}{}
		remaining := remainingBySession[sessionID]
		if remaining <= 0 {
			if err := s.tokenCache.DeleteRuntime(ctx, accountSessionOwnerRuntimeNamespace, sessionID); err != nil {
				success = false
			}
			continue
		}
		if err := s.tokenCache.SetRuntime(ctx, accountSessionOwnerRuntimeNamespace, sessionID, ownerPayload, remaining); err != nil {
			log.Printf("写入账号会话反向索引失败: account=%d err=%v", accountID, err)
			success = false
		}
	}
	return success
}

func (s *Store) persistedAccountSessionOwner(sessionID string) (int64, bool) {
	if s == nil || s.tokenCache == nil || strings.TrimSpace(sessionID) == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountSessionCacheTimeout)
	raw, found, err := s.tokenCache.GetRuntime(ctx, accountSessionOwnerRuntimeNamespace, sessionID)
	cancel()
	if err != nil || !found {
		return 0, false
	}
	owner := persistedAccountSessionOwner{}
	if json.Unmarshal(raw, &owner) != nil || owner.AccountID <= 0 {
		return 0, false
	}
	return owner.AccountID, true
}

func (s *Store) deletePersistedAccountSession(accountID int64, sessionID string) {
	if s == nil || s.tokenCache == nil {
		return
	}
	s.persistAccountSessions(accountID, time.Now(), sessionID)
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
	s.ensureAccountSessionsLoaded(account, now)

	s.accountSessionMu.Lock()
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
		s.accountSessionMu.Unlock()
		return true
	}
	if int64(len(bySession)) >= limit {
		s.accountSessionMu.Unlock()
		return false
	}
	bySession[sessionKey] = &accountSessionState{sessionID: sessionKey, lastSeen: now}
	s.accountSessionMu.Unlock()
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
	s.ensureAccountSessionsLoaded(account, now)
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
	for accountID, bySession := range s.accountSessions {
		if state := bySession[sessionKey]; state != nil {
			lastSeen := state.lastSeen
			s.accountSessionMu.Unlock()
			account := s.FindByID(accountID)
			if account == nil {
				return 0, false
			}
			enabled, _, idleTTL := account.SessionCapacityConfig()
			if enabled && lastSeen.Add(idleTTL).After(now) {
				return accountID, true
			}
			s.RemoveAccountSession(accountID, sessionKey)
			return 0, false
		}
	}
	s.accountSessionMu.Unlock()
	accountID, found := s.persistedAccountSessionOwner(sessionKey)
	if !found {
		accountID, found = s.SessionAffinityAccountID(sessionKey)
	}
	if !found {
		return 0, false
	}
	account := s.FindByID(accountID)
	if account == nil {
		return 0, false
	}
	s.ensureAccountSessionsLoaded(account, now)
	s.accountSessionMu.Lock()
	state := s.accountSessions[accountID][sessionKey]
	s.accountSessionMu.Unlock()
	if state != nil {
		return accountID, true
	}
	s.deletePersistedAccountSession(accountID, sessionKey)
	return 0, false
}

func (s *Store) SetAccountSessionOwner(accountID int64, sessionKey string, owner AccountSessionOwner) {
	if s == nil || accountID <= 0 || strings.TrimSpace(sessionKey) == "" || isSessionAccountingBypassKey(sessionKey) {
		return
	}
	if _, related := RelatedSessionRootKey(sessionKey); related {
		return
	}
	now := time.Now()
	s.accountSessionMu.Lock()
	shouldPersist := false
	if state := s.accountSessions[accountID][sessionKey]; state != nil {
		ownerChanged := state.owner != owner
		state.owner = owner
		shouldPersist = ownerChanged || state.lastPersisted.IsZero() || now.Sub(state.lastPersisted) >= accountSessionPersistInterval
	}
	s.accountSessionMu.Unlock()
	if shouldPersist && s.persistAccountSessions(accountID, now, sessionKey) {
		s.accountSessionMu.Lock()
		if state := s.accountSessions[accountID][sessionKey]; state != nil && state.lastPersisted.Before(now) {
			state.lastPersisted = now
		}
		s.accountSessionMu.Unlock()
	}
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

	account := s.FindByID(accountID)
	if account != nil {
		s.ensureAccountSessionsLoaded(account, time.Now())
	}
	s.accountSessionMu.Lock()
	state := s.accountSessions[accountID][rootKey]
	if state == nil {
		s.accountSessionMu.Unlock()
		return
	}
	if requestID != "" {
		if state.relatedRequestIDs == nil {
			state.relatedRequestIDs = make(map[string]struct{})
		}
		if _, exists := state.relatedRequestIDs[requestID]; exists {
			s.accountSessionMu.Unlock()
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
	s.accountSessionMu.Unlock()
	s.persistAccountSessions(accountID, time.Now())
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
	bySession := s.accountSessions[accountID]
	if _, ok := bySession[sessionKey]; !ok {
		s.accountSessionMu.Unlock()
		return false
	}
	delete(bySession, sessionKey)
	if len(bySession) == 0 {
		delete(s.accountSessions, accountID)
	}
	s.accountSessionMu.Unlock()
	s.deletePersistedAccountSession(accountID, sessionKey)
	return true
}

func (s *Store) ClearAccountSessions(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	if account := s.FindByID(accountID); account != nil {
		s.ensureAccountSessionsLoaded(account, time.Now())
	}
	s.accountSessionMu.Lock()
	bySession := s.accountSessions[accountID]
	sessionIDs := make([]string, 0, len(bySession))
	for sessionID := range bySession {
		sessionIDs = append(sessionIDs, sessionID)
	}
	delete(s.accountSessions, accountID)
	if s.accountSessionsHydrated == nil {
		s.accountSessionsHydrated = make(map[int64]bool)
	}
	s.accountSessionsHydrated[accountID] = true
	s.accountSessionMu.Unlock()
	s.persistAccountSessions(accountID, time.Now(), sessionIDs...)
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
	s.ensureAccountSessionsLoaded(account, now)

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
			s.ensureAccountSessionsLoaded(account, now)
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
	now := time.Now()
	if !enabled {
		// Hydrate while the old setting is still enabled so ClearAccountSessions
		// can remove every persisted reverse owner key as well as the main state.
		s.ensureAccountSessionsLoaded(account, now)
	}
	account.mu.Lock()
	account.SessionCapacityEnabled = enabled
	account.SessionCapacityMax = normalizeSessionCapacityMax(limit)
	account.SessionCapacityIdleTTLSeconds = normalizeSessionCapacityIdleTTLSeconds(idleTTLSeconds)
	account.mu.Unlock()
	if !enabled {
		s.ClearAccountSessions(dbID)
	} else {
		// A longer idle TTL must extend both the account snapshot and each reverse
		// owner index; otherwise the old Redis TTL can release a live window early
		// after a process restart.
		s.ensureAccountSessionsLoaded(account, now)
		s.accountSessionMu.Lock()
		sessionIDs := make([]string, 0, len(s.accountSessions[dbID]))
		for sessionID := range s.accountSessions[dbID] {
			sessionIDs = append(sessionIDs, sessionID)
		}
		s.accountSessionMu.Unlock()
		if len(sessionIDs) > 0 {
			s.persistAccountSessions(dbID, now, sessionIDs...)
		}
	}
	return true
}
