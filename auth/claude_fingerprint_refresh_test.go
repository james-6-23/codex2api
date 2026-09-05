package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type recordingPersister struct {
	calls map[int64]map[string]string
	fail  map[int64]error
}

func (r *recordingPersister) UpdateAccountCustomHeaders(_ context.Context, id int64, headers map[string]string) error {
	if err := r.fail[id]; err != nil {
		return err
	}
	if r.calls == nil {
		r.calls = map[int64]map[string]string{}
	}
	r.calls[id] = headers
	return nil
}

func TestRefreshClaudeFingerprintUserAgent(t *testing.T) {
	old := map[string]string{"user-agent": "claude-cli/2.1.219 (external, cli)", "X-Stainless-OS": "MacOS"}
	next, changed := RefreshClaudeFingerprintUserAgent(old, "2.1.258")
	if !changed || next["user-agent"] != "claude-cli/2.1.258 (external, cli)" {
		t.Fatalf("should bump version only: %v", next)
	}
	if next["X-Stainless-OS"] != "MacOS" || old["user-agent"] != "claude-cli/2.1.219 (external, cli)" {
		t.Fatal("other headers must be kept and input must not be mutated")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "claude-cli/2.1.258 (external, cli)"}, "2.1.258"); changed {
		t.Fatal("equal version must be a no-op")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "claude-cli/2.1.300 (external, cli)"}, "2.1.258"); changed {
		t.Fatal("newer fingerprint must not be downgraded")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"X-App": "cli"}, "2.1.258"); changed {
		t.Fatal("missing UA must be skipped")
	}
	if _, changed := RefreshClaudeFingerprintUserAgent(map[string]string{"User-Agent": "curl/8.7.1"}, "2.1.258"); changed {
		t.Fatal("non-CLI UA must be skipped")
	}
}

func TestRefreshClaudeFingerprintVersions_PersistsAndAppliesInMemory(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	claudeOld := &Account{DBID: 251, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)", "X-App": "cli"}}
	claudeNew := &Account{DBID: 252, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.258 (external, cli)"}}
	claudeBroken := &Account{DBID: 253, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.205 (external, cli)"}}
	codex := &Account{DBID: 1, UpstreamType: "codex", CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.100 (external, cli)"}}
	store.mu.Lock()
	store.accounts = []*Account{claudeOld, claudeNew, claudeBroken, codex}
	store.mu.Unlock()

	persister := &recordingPersister{fail: map[int64]error{253: errors.New("db down")}}
	updated, err := RefreshClaudeFingerprintVersions(context.Background(), store, persister, "2.1.258")
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}
	if err == nil || !errors.Is(err, persister.fail[253]) {
		t.Fatalf("first persist error should surface, got %v", err)
	}
	if got := persister.calls[251]["User-Agent"]; got != "claude-cli/2.1.258 (external, cli)" {
		t.Fatalf("persisted UA = %q", got)
	}
	if persister.calls[251]["X-App"] != "cli" {
		t.Fatal("other fingerprint headers must be persisted unchanged")
	}
	if claudeOld.CustomHeaders["User-Agent"] != "claude-cli/2.1.258 (external, cli)" {
		t.Fatal("in-memory account must be updated after persist")
	}
	if claudeBroken.CustomHeaders["User-Agent"] != "claude-cli/2.1.205 (external, cli)" {
		t.Fatal("failed persist must not update memory")
	}
	if _, called := persister.calls[1]; called {
		t.Fatal("non-Claude accounts must be ignored")
	}
	if _, called := persister.calls[252]; called {
		t.Fatal("up-to-date accounts must not be written")
	}
}

// concurrentMutationPersister simulates a writer (e.g. an admin edit) that
// changes an account's CustomHeaders concurrently with the DB persist
// performed by RefreshClaudeFingerprintVersions, landing in the TOCTOU
// window between the snapshot read and the in-memory write. It mutates only
// on its first call, so the second (retry) attempt observes a stable value
// and the CAS should succeed.
type concurrentMutationPersister struct {
	acc     *Account
	calls   []map[string]string
	mutated bool
}

func (p *concurrentMutationPersister) UpdateAccountCustomHeaders(_ context.Context, _ int64, headers map[string]string) error {
	p.calls = append(p.calls, headers)
	if !p.mutated {
		p.mutated = true
		p.acc.mu.Lock()
		p.acc.CustomHeaders = map[string]string{"User-Agent": p.acc.CustomHeaders["User-Agent"], "X-App": "other"}
		p.acc.mu.Unlock()
	}
	return nil
}

func TestRefreshClaudeFingerprintVersions_RetriesAndAppliesOnSecondAttempt(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	claude := &Account{DBID: 260, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}
	store.mu.Lock()
	store.accounts = []*Account{claude}
	store.mu.Unlock()

	persister := &concurrentMutationPersister{acc: claude}
	updated, err := RefreshClaudeFingerprintVersions(context.Background(), store, persister, "2.1.258")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(persister.calls) != 2 {
		t.Fatalf("persister called %d times, want 2", len(persister.calls))
	}
	if updated != 1 {
		t.Fatalf("updated = %d, want 1 (retry succeeded)", updated)
	}
	claude.mu.RLock()
	finalHeaders := cloneStringMap(claude.CustomHeaders)
	claude.mu.RUnlock()
	if finalHeaders["X-App"] != "other" {
		t.Fatalf("final in-memory headers must keep the concurrent edit, X-App = %q", finalHeaders["X-App"])
	}
	if finalHeaders["User-Agent"] != "claude-cli/2.1.258 (external, cli)" {
		t.Fatalf("final in-memory User-Agent = %q, want bumped version", finalHeaders["User-Agent"])
	}
	if !stringMapEqual(persister.calls[1], finalHeaders) {
		t.Fatalf("second persisted map %v must equal final in-memory headers %v", persister.calls[1], finalHeaders)
	}
}

// alwaysConflictingPersister mutates the account's headers on every call, so
// the bounded CAS retry always observes a stale snapshot and must give up
// without applying its own (now doubly-stale) in-memory write.
type alwaysConflictingPersister struct {
	acc   *Account
	calls int
}

func (p *alwaysConflictingPersister) UpdateAccountCustomHeaders(_ context.Context, _ int64, _ map[string]string) error {
	p.calls++
	p.acc.mu.Lock()
	p.acc.CustomHeaders = map[string]string{"User-Agent": p.acc.CustomHeaders["User-Agent"], "X-Conflict": fmt.Sprintf("v%d", p.calls)}
	p.acc.mu.Unlock()
	return nil
}

func TestRefreshClaudeFingerprintVersions_GivesUpAfterTwoConflicts(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	claude := &Account{DBID: 261, UpstreamType: UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}
	store.mu.Lock()
	store.accounts = []*Account{claude}
	store.mu.Unlock()

	persister := &alwaysConflictingPersister{acc: claude}
	updated, err := RefreshClaudeFingerprintVersions(context.Background(), store, persister, "2.1.258")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated = %d, want 0 (both attempts hit a conflict)", updated)
	}
	if persister.calls != 2 {
		t.Fatalf("persister called %d times, want 2", persister.calls)
	}
	claude.mu.RLock()
	got := claude.CustomHeaders["User-Agent"]
	claude.mu.RUnlock()
	if got != "claude-cli/2.1.219 (external, cli)" {
		t.Fatalf("in-memory UA must be whatever the persister last set (not overwritten), got %q", got)
	}
}

func TestRefreshClaudeFingerprintVersions_RejectsInvalidVersion(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	if _, err := RefreshClaudeFingerprintVersions(context.Background(), store, nil, "nope"); err == nil {
		t.Fatal("invalid target version must error")
	}
}
