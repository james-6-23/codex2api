package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

type recordingResponseContextBackend struct {
	cache.TokenCache
	mu         sync.Mutex
	shared     bool
	setCalls   int
	getCalls   int
	bounded    cache.ResponseContextReadResult
	boundedErr error
	writes     map[string][]json.RawMessage
	// blockWrites 非 nil 时，SetResponseContext 先等它关闭再落盘，用于验证后台写入。
	blockWrites chan struct{}
}

type legacySharedResponseContextBackend struct {
	cache.TokenCache
	mu       sync.Mutex
	items    []json.RawMessage
	err      error
	getCalls int
}

func newLegacySharedResponseContextBackend(items []json.RawMessage, err error) *legacySharedResponseContextBackend {
	return &legacySharedResponseContextBackend{
		TokenCache: cache.NewMemory(1),
		items:      cloneResponseContextItems(items),
		err:        err,
	}
}

func (b *legacySharedResponseContextBackend) SharedAcrossInstances() bool { return true }

func (b *legacySharedResponseContextBackend) GetResponseContext(context.Context, string) ([]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getCalls++
	return cloneResponseContextItems(b.items), b.err
}

func newRecordingResponseContextBackend(shared bool) *recordingResponseContextBackend {
	return &recordingResponseContextBackend{
		TokenCache: cache.NewMemory(1),
		shared:     shared,
		writes:     make(map[string][]json.RawMessage),
	}
}

func (b *recordingResponseContextBackend) SharedAcrossInstances() bool { return b.shared }

func (b *recordingResponseContextBackend) SetResponseContext(_ context.Context, key string, items []json.RawMessage, _ time.Duration) error {
	if b.blockWrites != nil {
		<-b.blockWrites
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.setCalls++
	b.writes[key] = cloneResponseContextItems(items)
	return nil
}

func (b *recordingResponseContextBackend) GetResponseContext(_ context.Context, _ string) ([]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getCalls++
	return cloneResponseContextItems(b.bounded.Items), b.boundedErr
}

func (b *recordingResponseContextBackend) GetResponseContextBounded(_ context.Context, _ string, _ int64) (cache.ResponseContextReadResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getCalls++
	result := b.bounded
	result.Items = cloneResponseContextItems(result.Items)
	return result, b.boundedErr
}

func (b *recordingResponseContextBackend) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.setCalls, b.getCalls
}

func TestResponseCacheMemoryBackendIsNeverUsed(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(false)
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})

	setResponseCache("key:1", "small", []json.RawMessage{responseCacheTestItem(1, "small")})
	if got := getResponseCacheResult("key:1", "small"); got.Kind != responseCacheLookupHit || got.Source != responseCacheSourceLocal {
		t.Fatalf("lookup = %+v, want local hit", got)
	}
	if got := getResponseCacheResult("key:1", "missing"); got.Kind != responseCacheLookupMiss {
		t.Fatalf("lookup = %+v, want ordinary miss", got)
	}
	drainResponseCacheBackendWrites()
	if sets, gets := backend.counts(); sets != 0 || gets != 0 {
		t.Fatalf("non-shared backend calls = set:%d get:%d, want zero", sets, gets)
	}
}

func TestResponseCacheSharedWritesSmallAndLocallyOversizedEntries(t *testing.T) {
	config := testResponseCacheConfig()
	small := []json.RawMessage{responseCacheTestItem(1, "small")}
	oversize := []json.RawMessage{responseCacheTestItem(2, "oversize")}
	config.maxEntryBytes = responseCacheItemsBytes(small)
	resetResponseCacheStateForTest(config)
	backend := newRecordingResponseContextBackend(true)
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})

	setResponseCache("key:1", "small", small)
	setResponseCache("key:1", "oversize", oversize)

	drainResponseCacheBackendWrites()
	if sets, _ := backend.counts(); sets != 2 {
		t.Fatalf("backend set calls = %d, want 2", sets)
	}
	if got := getResponseCacheResult("key:1", "small"); got.Kind != responseCacheLookupHit || got.Source != responseCacheSourceLocal {
		t.Fatalf("small lookup = %+v, want local hit", got)
	}
	drainResponseCacheBackendWrites()
	backend.mu.Lock()
	written := cloneResponseContextItems(backend.writes[responseCacheStoreKey("key:1", "oversize")])
	backend.mu.Unlock()
	if len(written) != 1 || string(written[0]) != string(oversize[0]) {
		t.Fatalf("oversize backend write = %s, want %s", written, oversize)
	}
}

func TestResponseCacheBackendPromotionAndServeWithoutPromotion(t *testing.T) {
	t.Run("small promotes", func(t *testing.T) {
		config := testResponseCacheConfig()
		resetResponseCacheStateForTest(config)
		backend := newRecordingResponseContextBackend(true)
		backend.bounded = cache.ResponseContextReadResult{
			Status: cache.ResponseContextReadFound,
			Items:  []json.RawMessage{responseCacheTestItem(1, "remote")},
		}
		SetResponseContextCache(backend)
		t.Cleanup(func() {
			SetResponseContextCache(nil)
			_ = backend.TokenCache.Close()
		})

		first := getResponseCacheResult("key:1", "remote")
		second := getResponseCacheResult("key:1", "remote")
		if first.Kind != responseCacheLookupHit || first.Source != responseCacheSourceBackend || !first.Promoted {
			t.Fatalf("first lookup = %+v, want promoted backend hit", first)
		}
		if second.Kind != responseCacheLookupHit || second.Source != responseCacheSourceLocal {
			t.Fatalf("second lookup = %+v, want local hit", second)
		}
		drainResponseCacheBackendWrites()
		if _, gets := backend.counts(); gets != 1 {
			t.Fatalf("backend gets = %d, want 1", gets)
		}
	})

	t.Run("local oversize serves without promotion", func(t *testing.T) {
		config := testResponseCacheConfig()
		items := []json.RawMessage{responseCacheTestItem(1, "remote-oversize")}
		config.maxEntryBytes = responseCacheItemsBytes(items) - 1
		config.reconstructMaxBytes = responseCacheItemsBytes(items)
		resetResponseCacheStateForTest(config)
		backend := newRecordingResponseContextBackend(true)
		backend.bounded = cache.ResponseContextReadResult{Status: cache.ResponseContextReadFound, Items: items}
		SetResponseContextCache(backend)
		t.Cleanup(func() {
			SetResponseContextCache(nil)
			_ = backend.TokenCache.Close()
		})

		for lookup := 0; lookup < 2; lookup++ {
			got := getResponseCacheResult("key:1", "remote")
			if got.Kind != responseCacheLookupHit || got.Source != responseCacheSourceBackend || got.Promoted {
				t.Fatalf("lookup %d = %+v, want non-promoted backend hit", lookup+1, got)
			}
		}
		drainResponseCacheBackendWrites()
		if _, gets := backend.counts(); gets != 2 {
			t.Fatalf("backend gets = %d, want 2", gets)
		}
	})
}

func TestResponseCacheReconstructionBoundaryAndBackendOutcomes(t *testing.T) {
	item := responseCacheTestItem(1, "boundary")
	itemBytes := responseCacheItemsBytes([]json.RawMessage{item})
	tests := []struct {
		name        string
		result      cache.ResponseContextReadResult
		err         error
		reconstruct int64
		want        responseCacheLookupKind
	}{
		{name: "exact logical boundary", result: cache.ResponseContextReadResult{Status: cache.ResponseContextReadFound, Items: []json.RawMessage{item}}, reconstruct: itemBytes, want: responseCacheLookupHit},
		{name: "one byte over logical boundary", result: cache.ResponseContextReadResult{Status: cache.ResponseContextReadFound, Items: []json.RawMessage{item}}, reconstruct: itemBytes - 1, want: responseCacheLookupReconstructionTooLarge},
		{name: "backend miss", result: cache.ResponseContextReadResult{Status: cache.ResponseContextReadMiss}, reconstruct: itemBytes, want: responseCacheLookupMiss},
		{name: "backend too large", result: cache.ResponseContextReadResult{Status: cache.ResponseContextReadTooLarge}, reconstruct: itemBytes, want: responseCacheLookupReconstructionTooLarge},
		{name: "backend corrupt", result: cache.ResponseContextReadResult{Status: cache.ResponseContextReadCorrupt}, reconstruct: itemBytes, want: responseCacheLookupBackendCorrupt},
		{name: "backend transport error", err: errors.New("redis unavailable"), reconstruct: itemBytes, want: responseCacheLookupBackendError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testResponseCacheConfig()
			config.reconstructMaxBytes = tt.reconstruct
			config.maxEntryBytes = tt.reconstruct
			resetResponseCacheStateForTest(config)
			backend := newRecordingResponseContextBackend(true)
			backend.bounded = tt.result
			backend.boundedErr = tt.err
			SetResponseContextCache(backend)
			t.Cleanup(func() {
				SetResponseContextCache(nil)
				_ = backend.TokenCache.Close()
			})
			if got := getResponseCacheResult("key:1", "remote"); got.Kind != tt.want {
				t.Fatalf("lookup = %+v, want kind %v", got, tt.want)
			}
		})
	}
}

func TestResponseCacheLegacySharedBackendChecksFullLogicalSizeBeforeTrim(t *testing.T) {
	config := testResponseCacheConfig()
	config.maxItems = 1
	tail := responseCacheTestItem(2, "tail")
	prefix := responseCacheTestItem(1, "large-prefix")
	config.reconstructMaxBytes = responseCacheItemsBytes([]json.RawMessage{tail})
	resetResponseCacheStateForTest(config)
	backend := newLegacySharedResponseContextBackend([]json.RawMessage{prefix, tail}, nil)
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})

	if got := getResponseCacheResult("key:1", "legacy"); got.Kind != responseCacheLookupReconstructionTooLarge {
		t.Fatalf("legacy backend lookup = %+v, want rejection based on full pre-trim logical bytes", got)
	}
}

func TestResponseCacheLegacySharedBackendExactBoundaryAndCorruptError(t *testing.T) {
	item := responseCacheTestItem(1, "legacy")
	config := testResponseCacheConfig()
	config.reconstructMaxBytes = responseCacheItemsBytes([]json.RawMessage{item})

	t.Run("exact boundary", func(t *testing.T) {
		resetResponseCacheStateForTest(config)
		backend := newLegacySharedResponseContextBackend([]json.RawMessage{item}, nil)
		SetResponseContextCache(backend)
		t.Cleanup(func() {
			SetResponseContextCache(nil)
			_ = backend.TokenCache.Close()
		})
		if got := getResponseCacheResult("key:1", "legacy"); got.Kind != responseCacheLookupHit {
			t.Fatalf("legacy exact-boundary lookup = %+v, want hit", got)
		}
	})

	t.Run("syntax error is corrupt", func(t *testing.T) {
		resetResponseCacheStateForTest(config)
		backend := newLegacySharedResponseContextBackend(nil, &json.SyntaxError{})
		SetResponseContextCache(backend)
		t.Cleanup(func() {
			SetResponseContextCache(nil)
			_ = backend.TokenCache.Close()
		})
		if got := getResponseCacheResult("key:1", "legacy"); got.Kind != responseCacheLookupBackendCorrupt {
			t.Fatalf("legacy corrupt lookup = %+v, want corrupt", got)
		}
	})
}

func TestResponseCacheConcurrentBackendPromotionEvictionMarkersAndStats(t *testing.T) {
	config := testResponseCacheConfig()
	config.maxEntries = 8
	resetResponseCacheStateForTest(config)
	backend := newRecordingResponseContextBackend(true)
	backend.bounded = cache.ResponseContextReadResult{
		Status: cache.ResponseContextReadFound,
		Items:  []json.RawMessage{responseCacheTestItem(1, "shared")},
	}
	SetResponseContextCache(backend)

	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_ = getResponseCacheResult("key:1", "shared")
			_ = GetResponseCacheStats()
		}()
	}
	workers.Wait()
	SetResponseContextCache(nil)
	_ = backend.TokenCache.Close()

	resetResponseCacheStateForTest(config)
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for iteration := 0; iteration < 20; iteration++ {
				id := fmt.Sprintf("%d-%d", worker, iteration)
				setResponseCache("key:1", id, []json.RawMessage{responseCacheTestItem(iteration, id)})
				_ = getResponseCacheResult("key:1", id)
				_ = GetResponseCacheStats()
			}
		}(i)
	}
	workers.Wait()
	respCache.mu.RLock()
	markerCount := len(respCache.markers)
	respCache.mu.RUnlock()
	if markerCount == 0 || markerCount > responseCacheMaxMarkers {
		t.Fatalf("concurrent marker count = %d, want bounded non-zero markers", markerCount)
	}
}

func TestResponseCacheMemoryKnownUnavailableMarkersAreBoundedAndExpire(t *testing.T) {
	config := testResponseCacheConfig()
	config.maxEntries = 2
	small := []json.RawMessage{responseCacheTestItem(1, "small")}
	config.maxEntryBytes = responseCacheItemsBytes(small)
	config.maxBytes = responseCacheItemsBytes(small) * 2
	config.ttl = time.Minute
	resetResponseCacheStateForTest(config)

	setResponseCache("key:1", "a", small)
	setResponseCache("key:1", "b", []json.RawMessage{responseCacheTestItem(2, "small")})
	setResponseCache("key:1", "c", []json.RawMessage{responseCacheTestItem(3, "small")})
	if got := getResponseCacheResult("key:1", "a"); got.Kind != responseCacheLookupKnownEvicted {
		t.Fatalf("count-evicted lookup = %+v, want known eviction", got)
	}

	config.maxEntries = 10
	config.maxBytes = responseCacheItemsBytes(small)
	config.maxEntryBytes = 1 << 20
	configureResponseCacheForTest(config)
	if got := getResponseCacheResult("key:1", "b"); got.Kind != responseCacheLookupKnownEvicted {
		t.Fatalf("byte-evicted lookup = %+v, want known eviction", got)
	}

	config.maxEntryBytes = responseCacheItemsBytes(small)
	config.maxBytes = 1 << 20
	configureResponseCacheForTest(config)
	setResponseCache("key:1", "oversize", []json.RawMessage{responseCacheTestItem(4, "definitely-oversized")})
	if got := getResponseCacheResult("key:1", "oversize"); got.Kind != responseCacheLookupKnownOversize {
		t.Fatalf("oversize lookup = %+v, want known oversize", got)
	}
	if got := getResponseCacheResult("key:2", "oversize"); got.Kind != responseCacheLookupMiss {
		t.Fatalf("cross-owner marker lookup = %+v, want ordinary miss", got)
	}

	config.maxEntryBytes = 1 << 20
	configureResponseCacheForTest(config)
	setResponseCache("key:1", "oversize", small)
	if got := getResponseCacheResult("key:1", "oversize"); got.Kind != responseCacheLookupHit {
		t.Fatalf("successful admission did not clear marker: %+v", got)
	}

	config.maxEntryBytes = 1
	configureResponseCacheForTest(config)
	lastMarkerID := ""
	for i := 0; i < responseCacheMaxMarkers+17; i++ {
		lastMarkerID = fmt.Sprintf("marker-%d", i)
		setResponseCache("key:1", lastMarkerID, small)
	}
	if got := getResponseCacheResult("key:1", lastMarkerID); got.Kind != responseCacheLookupKnownOversize {
		t.Fatalf("latest bounded marker lookup = %+v, want known oversize", got)
	}

	respCache.mu.Lock()
	markerCount := len(respCache.markers)
	if markerCount != responseCacheMaxMarkers {
		respCache.mu.Unlock()
		t.Fatalf("markers = %d, want exact cap %d after overflow", markerCount, responseCacheMaxMarkers)
	}
	for _, marker := range respCache.markers {
		marker.expiresAt = time.Now().Add(-time.Second)
	}
	respCache.mu.Unlock()
	cleanupResponseCacheExpired(time.Now())
	respCache.mu.RLock()
	remainingMarkers := len(respCache.markers)
	respCache.mu.RUnlock()
	if remainingMarkers != 0 {
		t.Fatalf("cleanup retained %d expired markers with empty store", remainingMarkers)
	}
	if got := getResponseCacheResult("key:1", lastMarkerID); got.Kind != responseCacheLookupMiss {
		t.Fatalf("expired marker lookup = %+v, want ordinary miss", got)
	}
}

func TestResponseCacheOversizeReplacementRemovesStaleLocalEntry(t *testing.T) {
	config := testResponseCacheConfig()
	small := []json.RawMessage{responseCacheTestItem(1, "small")}
	resetResponseCacheStateForTest(config)
	setResponseCache("key:1", "replacement", small)

	config.maxEntryBytes = responseCacheItemsBytes(small)
	configureResponseCacheForTest(config)
	setResponseCache("key:1", "replacement", []json.RawMessage{responseCacheTestItem(2, "oversized-replacement")})

	if got := getResponseCacheResult("key:1", "replacement"); got.Kind != responseCacheLookupKnownOversize {
		t.Fatalf("replacement lookup = %+v, want newest oversize marker instead of stale local hit", got)
	}
}

// 聚合 Hits/Misses 是端到端口径：远端命中计 Hit 而非 Miss。
// 钉住不变量 Hits = LocalHits + RemoteHits、Hits + Misses = LocalHits + LocalMisses。
func TestResponseCacheAggregateHitMissEndToEnd(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})

	setResponseCache("key:1", "local", []json.RawMessage{responseCacheTestItem(1, "local")})
	if got := getResponseCacheResult("key:1", "local"); got.Kind != responseCacheLookupHit || got.Source != responseCacheSourceLocal {
		t.Fatalf("local lookup = %+v, want local hit", got)
	}

	backend.mu.Lock()
	backend.bounded = cache.ResponseContextReadResult{
		Status: cache.ResponseContextReadFound,
		Items:  []json.RawMessage{responseCacheTestItem(2, "remote")},
	}
	backend.mu.Unlock()
	if got := getResponseCacheResult("key:1", "remote"); got.Kind != responseCacheLookupHit || got.Source != responseCacheSourceBackend {
		t.Fatalf("remote lookup = %+v, want backend hit", got)
	}

	backend.mu.Lock()
	backend.bounded = cache.ResponseContextReadResult{Status: cache.ResponseContextReadMiss}
	backend.mu.Unlock()
	if got := getResponseCacheResult("key:1", "missing"); got.Kind != responseCacheLookupMiss {
		t.Fatalf("missing lookup = %+v, want miss", got)
	}

	stats := GetResponseCacheStats()
	if stats.Hits != 2 || stats.Misses != 1 {
		t.Fatalf("aggregate hits/misses = %d/%d, want 2/1", stats.Hits, stats.Misses)
	}
	if stats.LocalHits != 1 || stats.LocalMisses != 2 || stats.RemoteHits != 1 || stats.RemoteMisses != 1 {
		t.Fatalf("layer stats = %+v, want local 1/2 remote 1/1", stats)
	}
	if stats.Hits != stats.LocalHits+stats.RemoteHits {
		t.Fatalf("invariant broken: hits=%d, local+remote=%d", stats.Hits, stats.LocalHits+stats.RemoteHits)
	}
	if stats.Hits+stats.Misses != stats.LocalHits+stats.LocalMisses {
		t.Fatalf("invariant broken: hits+misses=%d, lookups=%d", stats.Hits+stats.Misses, stats.LocalHits+stats.LocalMisses)
	}
}

// 后台写入：调用方不等共享后端往返，L1 立即可读；槽位耗尽时退回同步写（背压）。
func TestResponseCacheBackendWritesAreAsynchronousWithBackpressure(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	backend.blockWrites = make(chan struct{})
	var release sync.Once
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		release.Do(func() { close(backend.blockWrites) })
		drainResponseCacheBackendWrites()
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})

	done := make(chan struct{})
	go func() {
		setResponseCache("key:1", "async", []json.RawMessage{responseCacheTestItem(1, "async")})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("setResponseCache waited for the shared backend write")
	}
	if got := getResponseCacheResult("key:1", "async"); got.Kind != responseCacheLookupHit || got.Source != responseCacheSourceLocal {
		t.Fatalf("local admission must not wait for the backend: %+v", got)
	}
	if sets, _ := backend.counts(); sets != 0 {
		t.Fatalf("backend write landed before release: %d", sets)
	}

	// 填满剩余槽位后，下一次写入必须同步等待，而不是丢弃。
	for i := 1; i < responseCacheBackendWriteSlots; i++ {
		setResponseCache("key:1", fmt.Sprintf("fill-%d", i), []json.RawMessage{responseCacheTestItem(i+1, "fill")})
	}
	blocked := make(chan struct{})
	go func() {
		setResponseCache("key:1", "overflow", []json.RawMessage{responseCacheTestItem(99, "overflow")})
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("write beyond the async slots returned without waiting for the backend")
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(func() { close(backend.blockWrites) })
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("synchronous fallback write never completed")
	}
	drainResponseCacheBackendWrites()
	if sets, _ := backend.counts(); sets != responseCacheBackendWriteSlots+1 {
		t.Fatalf("backend set calls = %d, want %d", sets, responseCacheBackendWriteSlots+1)
	}
	backend.mu.Lock()
	_, async := backend.writes[responseCacheStoreKey("key:1", "async")]
	_, overflow := backend.writes[responseCacheStoreKey("key:1", "overflow")]
	backend.mu.Unlock()
	if !async || !overflow {
		t.Fatalf("writes missing: async=%v overflow=%v", async, overflow)
	}
	if !DrainResponseCacheBackendWrites(time.Second) {
		t.Fatal("drain reported pending writes after completion")
	}
}
