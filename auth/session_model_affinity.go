package auth

import "time"

func (trace *SelectionTrace) hasSessionModelFilter() bool {
	if trace == nil {
		return false
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.sessionModelFilter != nil
}

func (trace *SelectionTrace) SetSessionModelFilter(filter AccountFilter) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	trace.sessionModelFilter = filter
	trace.mu.Unlock()
}

func (trace *SelectionTrace) CheckSessionModel(account *Account) bool {
	if trace == nil || account == nil {
		return true
	}
	trace.mu.Lock()
	filter := trace.sessionModelFilter
	trace.mu.Unlock()
	if filter == nil || filter(account) {
		return true
	}
	trace.mu.Lock()
	trace.sessionModelDenied = true
	trace.mu.Unlock()
	trace.Bind(account.ID())
	trace.Reject("session_model_unavailable")
	return false
}

func (trace *SelectionTrace) SessionModelDenied() bool {
	if trace == nil {
		return false
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.sessionModelDenied
}

func (store *Store) LiveSessionAccountID(key string, now time.Time) (int64, bool) {
	if accountID, found := store.AccountSessionAccountID(key, now); found {
		return accountID, true
	}
	key, _ = RelatedSessionRootKey(key)
	store.sessionMu.RLock()
	binding, found := store.sessionBindings[key]
	store.sessionMu.RUnlock()
	if found {
		return binding.accountID, binding.expiresAt.After(now)
	}
	binding, found = store.getCachedSessionAffinity(key)
	return binding.accountID, found
}
