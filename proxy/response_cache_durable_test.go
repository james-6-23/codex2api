package proxy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

type deadlineResponseContextBackend struct {
	cache.TokenCache
	reader cache.BoundedResponseContextReader
	budget time.Duration
}

func (b *deadlineResponseContextBackend) DurableResponseContexts() bool {
	return cache.IsDurableResponseContextCache(b.TokenCache)
}

func (b *deadlineResponseContextBackend) GetResponseContextBounded(ctx context.Context, key string, maxBytes int64) (cache.ResponseContextReadResult, error) {
	deadline, ok := ctx.Deadline()
	if ok {
		b.budget = time.Until(deadline)
	}
	return b.reader.GetResponseContextBounded(ctx, key, maxBytes)
}

func TestResponseCacheDurableFallbackAfterEvictionAndRestart(t *testing.T) {
	config := testResponseCacheConfig()
	config.maxEntries = 1
	resetResponseCacheStateForTest(config)
	base := cache.NewMemory(1)
	t.Cleanup(func() { _ = base.Close() })
	path := filepath.Join(t.TempDir(), "contexts.db")
	store, err := cache.OpenDurableResponseContextStore(path, cache.DurableResponseContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Cleanup(func() {
		drainResponseCacheBackendWrites()
		SetResponseContextCache(nil)
		resetResponseCacheStateForTest(defaultResponseCacheConfig())
	})
	install := func() *deadlineResponseContextBackend {
		backend := cache.WithDurableResponseContexts(base, store)
		tracked := &deadlineResponseContextBackend{TokenCache: backend, reader: backend.(cache.BoundedResponseContextReader)}
		SetResponseContextCache(tracked)
		return tracked
	}
	tracked := install()
	items := []json.RawMessage{responseCacheTestItem(1, "durable original")}
	setResponseCache("key:1", "durable", items)
	setResponseCache("key:1", "evictor", []json.RawMessage{responseCacheTestItem(2, "evictor")})
	drainResponseCacheBackendWrites()
	assertBackendHit := func() {
		t.Helper()
		got := getResponseCacheResult("key:1", "durable")
		if got.Kind != responseCacheLookupHit || got.Source != responseCacheSourceBackend ||
			len(got.Items) != 1 || string(got.Items[0]) != string(items[0]) {
			t.Fatalf("durable fallback = %+v", got)
		}
		if tracked.budget < 4*time.Second || tracked.budget > 5*time.Second {
			t.Fatalf("local durable read budget=%v, want bounded 5s", tracked.budget)
		}
	}
	assertBackendHit()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = cache.OpenDurableResponseContextStore(path, cache.DurableResponseContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resetResponseCacheStateForTest(config)
	tracked = install()
	assertBackendHit()
}

func TestResponseCacheSharedBackendKeepsShortReadBudget(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	backend.bounded = cache.ResponseContextReadResult{Status: cache.ResponseContextReadMiss}
	tracked := &deadlineResponseContextBackend{TokenCache: backend, reader: backend}
	SetResponseContextCache(tracked)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
		resetResponseCacheStateForTest(defaultResponseCacheConfig())
	})
	if got := getResponseCacheResult("key:1", "missing"); got.Kind != responseCacheLookupMiss {
		t.Fatalf("shared lookup=%+v, want miss", got)
	}
	if tracked.budget <= 0 || tracked.budget > 500*time.Millisecond {
		t.Fatalf("shared backend read budget=%v, want bounded 500ms", tracked.budget)
	}
}
