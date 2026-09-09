package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type rootAccountWaitState struct {
	changed         chan struct{}
	waiters         int
	revision        uint64
	checkedRevision uint64
	checkedAt       time.Time
	checking        bool
	checkLocal      bool
	accountID       int64
}

func (store *Store) notifyRootAccountWaiters(key string) {
	store.rootAccountWaitMu.Lock()
	defer store.rootAccountWaitMu.Unlock()
	if state := store.rootAccountWaiters[key]; state != nil {
		state.revision++
		state.checkLocal = true
		close(state.changed)
		state.changed = make(chan struct{})
	}
}

func (store *Store) WaitForRootAccount(requestContext context.Context, key string) (int64, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, errors.New("main root key is missing")
	}
	if err := requestContext.Err(); err != nil {
		return 0, err
	}
	store.rootAccountWaitMu.Lock()
	if store.rootAccountWaiters == nil {
		store.rootAccountWaiters = make(map[string]*rootAccountWaitState)
	}
	state := store.rootAccountWaiters[key]
	if (state == nil && len(store.rootAccountWaiters) >= 4096) || (state != nil && state.waiters >= 128) {
		store.rootAccountWaitMu.Unlock()
		return 0, errors.New("too many pending background root requests")
	}
	if state == nil {
		state = &rootAccountWaitState{changed: make(chan struct{}), checkLocal: true}
		store.rootAccountWaiters[key] = state
	}
	state.waiters++
	store.rootAccountWaitMu.Unlock()
	defer func() {
		store.rootAccountWaitMu.Lock()
		defer store.rootAccountWaitMu.Unlock()
		state.waiters--
		if state.waiters == 0 {
			delete(store.rootAccountWaiters, key)
		}
	}()
	for {
		if err := requestContext.Err(); err != nil {
			return 0, err
		}
		store.rootAccountWaitMu.Lock()
		fresh := state.checkedRevision == state.revision && !state.checkedAt.IsZero() && time.Since(state.checkedAt) < time.Second
		if fresh && state.accountID > 0 {
			accountID := state.accountID
			store.rootAccountWaitMu.Unlock()
			return accountID, requestContext.Err()
		}
		if !fresh && !state.checking {
			state.checking = true
			revision, checkLocal := state.revision, state.checkLocal
			state.checkLocal = false
			store.rootAccountWaitMu.Unlock()
			accountID := store.loadRootAccountWaitBinding(requestContext, key, checkLocal)
			store.rootAccountWaitMu.Lock()
			state.checking = false
			if revision == state.revision && requestContext.Err() == nil {
				state.accountID = accountID
				state.checkedAt = time.Now()
				state.checkedRevision = revision
			}
			close(state.changed)
			state.changed = make(chan struct{})
			store.rootAccountWaitMu.Unlock()
			continue
		}
		changed := state.changed
		remaining := time.Second
		if fresh {
			remaining = max(time.Until(state.checkedAt.Add(time.Second)), time.Millisecond)
		}
		store.rootAccountWaitMu.Unlock()
		timer := time.NewTimer(remaining)
		select {
		case <-requestContext.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (store *Store) loadRootAccountWaitBinding(requestContext context.Context, key string, checkLocal bool) int64 {
	if checkLocal {
		if accountID, found := store.localRootAccountWindowID(key); found {
			return accountID
		}
	}
	store.sessionMu.RLock()
	binding, found := store.sessionBindings[key]
	store.sessionMu.RUnlock()
	if found && binding.expiresAt.After(time.Now()) {
		return binding.accountID
	}
	if store.tokenCache == nil || isProcessLocalSessionAffinityKey(key) {
		return 0
	}
	lookupContext, cancelLookup := context.WithTimeout(requestContext, accountSessionCacheTimeout)
	defer cancelLookup()
	raw, ownerFound, ownerErr := store.tokenCache.GetRuntime(lookupContext, accountSessionOwnerRuntimeNamespace, key)
	var owner persistedAccountSessionOwner
	if ownerErr == nil && ownerFound && json.Unmarshal(raw, &owner) == nil && owner.AccountID > 0 {
		return owner.AccountID
	}
	cached, cachedFound, cachedErr := store.tokenCache.GetSessionAffinity(lookupContext, key)
	if cachedErr == nil && cachedFound && cached.AccountID > 0 {
		return cached.AccountID
	}
	return 0
}

func (store *Store) localRootAccountWindowID(key string) (int64, bool) {
	store.accountSessionMu.Lock()
	var ownerID int64
	var lastSeen time.Time
	for accountID, sessions := range store.accountSessions {
		if state := sessions[key]; state != nil {
			ownerID, lastSeen = accountID, state.lastSeen
			break
		}
	}
	store.accountSessionMu.Unlock()
	if ownerID == 0 {
		return 0, false
	}
	account := store.FindByID(ownerID)
	if account == nil {
		return 0, false
	}
	enabled, _, idleTTL := account.SessionCapacityConfig()
	return ownerID, enabled && lastSeen.Add(idleTTL).After(time.Now())
}
