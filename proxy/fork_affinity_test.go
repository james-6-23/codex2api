package proxy

import (
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestTakeForkSourceAccountInheritsAccountWithoutMergingAffinityKeys(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	source := &auth.Account{DBID: 101, AccessToken: "source-token", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	other := &auth.Account{DBID: 202, AccessToken: "other-token", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	store.AddAccounts([]*auth.Account{source, other})
	handler := &Handler{store: store}

	const apiKeyID int64 = 7
	sourceKey := sessionAffinityKey(testRootSessionA, apiKeyID)
	targetKey := sessionAffinityKey(testLeafSessionA, apiKeyID)
	store.BindSessionAffinity(sourceKey, source, "")

	selected, _ := handler.takeForkSourceAccount(
		requestSessionIdentity{forkSourceAffinityID: testRootSessionA},
		targetKey,
		apiKeyID,
		nil,
		accountFilterForModel("gpt-5.6-sol"),
		auth.DispatchPolicyStandard,
	)
	if selected == nil || selected.ID() != source.ID() {
		t.Fatalf("selected account = %+v, want source account %d", selected, source.ID())
	}
	if sourceKey == targetKey {
		t.Fatal("fork source and target affinity keys must remain independent")
	}
	handler.bindAccountSession(nil, targetKey, selected, "")
	if accountID, ok := store.SessionAffinityAccountID(targetKey); !ok || accountID != source.ID() {
		t.Fatalf("target binding = (%d, %t), want account %d", accountID, ok, source.ID())
	}
	store.Release(selected)
}

func TestTakeForkSourceAccountFallsBackWhenSourceAccountUnavailable(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	source := &auth.Account{DBID: 101, AccessToken: "source-token", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	other := &auth.Account{DBID: 202, AccessToken: "other-token", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	store.AddAccounts([]*auth.Account{source, other})
	handler := &Handler{store: store}

	const apiKeyID int64 = 7
	sourceKey := sessionAffinityKey(testRootSessionA, apiKeyID)
	targetKey := sessionAffinityKey(testLeafSessionA, apiKeyID)
	store.BindSessionAffinity(sourceKey, source, "")
	atomic.StoreInt32(&source.Disabled, 1)

	selected, _ := handler.takeForkSourceAccount(
		requestSessionIdentity{forkSourceAffinityID: testRootSessionA},
		targetKey,
		apiKeyID,
		nil,
		accountFilterForModel("gpt-5.6-sol"),
		auth.DispatchPolicyStandard,
	)
	if selected != nil {
		t.Fatalf("unavailable source account selected: %d", selected.ID())
	}

	fallback, _ := store.NextForSessionWithDispatch(targetKey, apiKeyID, nil, accountFilterForModel("gpt-5.6-sol"), auth.DispatchPolicyStandard)
	if fallback == nil || fallback.ID() != other.ID() {
		t.Fatalf("fallback account = %+v, want account %d", fallback, other.ID())
	}
	store.Release(fallback)
}
