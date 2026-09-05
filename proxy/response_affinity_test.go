package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func TestResponseAccountAffinityOwnerScopedAndShared(t *testing.T) {
	tc := cache.NewMemory(1)
	h := &Handler{cache: tc}
	h.recordResponseAccountAffinity("key:1", "resp_abc", 42, "session-1", "gpt-5.6-sol", "openai_responses")
	if got, ok := lookupResponseAccountAffinity(context.Background(), tc, "key:1", "resp_abc"); !ok || got.AccountID != 42 {
		t.Fatalf("lookup = %#v, %v; want account 42", got, ok)
	}
	if _, ok := lookupResponseAccountAffinity(context.Background(), tc, "key:2", "resp_abc"); ok {
		t.Fatal("response affinity must not cross API-key owners")
	}
}

func TestResponseAccountAffinityExpires(t *testing.T) {
	responseAffinityLocal.Lock()
	responseAffinityLocal.entries[responseAffinityKey("key:1", "expired")] = responseAccountAffinity{
		AccountID: 7, Owner: "key:1", ExpiresAt: time.Now().Add(-time.Second),
	}
	responseAffinityLocal.Unlock()
	if _, ok := lookupResponseAccountAffinity(context.Background(), nil, "key:1", "expired"); ok {
		t.Fatal("expired response affinity should miss")
	}
}
