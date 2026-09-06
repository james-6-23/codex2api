package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func responseCacheTestItem(index int, extra string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"message","index":%d,"content":%q}`, index, extra))
}

func responseCacheCallItem(typ, callID string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":%q,"call_id":%q}`, typ, callID))
}

func responseCacheItemsBytes(items []json.RawMessage) int64 {
	var total int64
	for _, item := range items {
		total += int64(len(item))
	}
	return total
}

func testResponseCacheConfig() responseCacheConfig {
	return responseCacheConfig{
		maxBytes:            1 << 20,
		maxEntryBytes:       1 << 20,
		reconstructMaxBytes: 1 << 20,
		maxEntries:          100,
		ttl:                 time.Hour,
		maxItems:            200,
	}
}

func TestResponseCacheDefaultBudget(t *testing.T) {
	config := defaultResponseCacheConfig()
	if config.maxBytes != 64<<20 {
		t.Fatalf("maxBytes = %d, want %d", config.maxBytes, 64<<20)
	}
	if config.maxEntryBytes != 8<<20 {
		t.Fatalf("maxEntryBytes = %d, want %d", config.maxEntryBytes, 8<<20)
	}
	if config.reconstructMaxBytes != 64<<20 {
		t.Fatalf("reconstructMaxBytes = %d, want %d", config.reconstructMaxBytes, 64<<20)
	}
	if config.maxEntries != 2000 {
		t.Fatalf("maxEntries = %d, want 2000", config.maxEntries)
	}
	if config.ttl != 10*time.Minute {
		t.Fatalf("ttl = %s, want 10m", config.ttl)
	}
	if config.maxItems != 200 {
		t.Fatalf("maxItems = %d, want 200", config.maxItems)
	}
}

func TestTrimResponseContextTailItemLimits(t *testing.T) {
	for _, count := range []int{199, 200, 201} {
		t.Run(fmt.Sprintf("%d_items", count), func(t *testing.T) {
			items := make([]json.RawMessage, count)
			for i := range items {
				items[i] = responseCacheTestItem(i, "")
			}

			got := trimResponseContextTail(items, 200)
			wantCount := count
			wantFirst := 0
			if count > 200 {
				wantCount = 200
				wantFirst = count - 200
			}
			if len(got) != wantCount {
				t.Fatalf("trimmed count = %d, want %d", len(got), wantCount)
			}
			if string(got[0]) != string(items[wantFirst]) {
				t.Fatalf("first retained item = %s, want original item %d (%s)", got[0], wantFirst, items[wantFirst])
			}
			if string(got[len(got)-1]) != string(items[len(items)-1]) {
				t.Fatalf("trim must retain a suffix ending at the final item")
			}
		})
	}
}

func TestTrimResponseContextTailDoesNotSplitPairAcrossBoundary(t *testing.T) {
	items := make([]json.RawMessage, 202)
	for i := range items {
		items[i] = responseCacheTestItem(i, "")
	}
	items[1] = responseCacheCallItem("function_call", "crossing")
	items[2] = responseCacheCallItem("function_call_output", "crossing")

	got := trimResponseContextTail(items, 200)
	if len(got) != 199 {
		t.Fatalf("trimmed count = %d, want 199 after excluding crossing pair", len(got))
	}
	if string(got[0]) != string(items[3]) {
		t.Fatalf("first retained item = %s, want item 3", got[0])
	}
}

func TestTrimResponseContextTailHandlesMultipleCrossingGroups(t *testing.T) {
	items := make([]json.RawMessage, 205)
	for i := range items {
		items[i] = responseCacheTestItem(i, "")
	}
	// The initial boundary is 5. Removing pair A moves it to 8, which then
	// crosses pair B and must move it once more to 11.
	items[4] = responseCacheCallItem("function_call", "pair-a")
	items[7] = responseCacheCallItem("function_call_output", "pair-a")
	items[6] = responseCacheCallItem("mcp_tool_call", "pair-b")
	items[10] = responseCacheCallItem("mcp_tool_call_output", "pair-b")

	got := trimResponseContextTail(items, 200)
	if len(got) != 194 {
		t.Fatalf("trimmed count = %d, want 194", len(got))
	}
	if string(got[0]) != string(items[11]) {
		t.Fatalf("first retained item = %s, want item 11", got[0])
	}
}

func TestTrimResponseContextTailRetainsPendingUnmatchedCall(t *testing.T) {
	items := make([]json.RawMessage, 201)
	for i := range items {
		items[i] = responseCacheTestItem(i, "")
	}
	items[1] = responseCacheCallItem("shell_call", "pending")

	got := trimResponseContextTail(items, 200)
	if len(got) != 200 {
		t.Fatalf("trimmed count = %d, want 200", len(got))
	}
	if string(got[0]) != string(items[1]) {
		t.Fatalf("pending unmatched call at boundary was dropped: first=%s", got[0])
	}
}

func TestTrimResponseContextTailMatchesRepeatedCallIDsByOccurrence(t *testing.T) {
	items := make([]json.RawMessage, 205)
	for i := range items {
		items[i] = responseCacheTestItem(i, "")
	}
	items[0] = responseCacheCallItem("function_call", "reused")
	items[1] = responseCacheCallItem("function_call_output", "reused")
	items[6] = responseCacheCallItem("function_call", "reused")
	items[7] = responseCacheCallItem("function_call_output", "reused")

	got := trimResponseContextTail(items, 200)
	if len(got) != 200 || string(got[0]) != string(items[5]) {
		t.Fatalf("independent repeated pairs were merged: got %d items starting %s, want item 5", len(got), got[0])
	}
}

func TestTrimResponseContextTailKeepsPendingRepeatedCall(t *testing.T) {
	items := make([]json.RawMessage, 205)
	for i := range items {
		items[i] = responseCacheTestItem(i, "")
	}
	items[0] = responseCacheCallItem("shell_call", "reused")
	items[1] = responseCacheCallItem("shell_call_output", "reused")
	items[5] = responseCacheCallItem("shell_call", "reused")

	got := trimResponseContextTail(items, 200)
	if len(got) != 200 || string(got[0]) != string(items[5]) {
		t.Fatalf("pending repeated call was paired with an old output: got %d items starting %s", len(got), got[0])
	}
}

func TestTrimResponseContextTailDoesNotMatchDifferentCallFamilies(t *testing.T) {
	items := make([]json.RawMessage, 205)
	for i := range items {
		items[i] = responseCacheTestItem(i, "")
	}
	items[4] = responseCacheCallItem("function_call", "shared")
	items[6] = responseCacheCallItem("mcp_tool_call_output", "shared")

	got := trimResponseContextTail(items, 200)
	if len(got) != 200 || string(got[0]) != string(items[5]) {
		t.Fatalf("different call families were paired: got %d items starting %s, want item 5", len(got), got[0])
	}
}

func TestResponseCacheLRUEvictionAndStats(t *testing.T) {
	config := testResponseCacheConfig()
	config.maxEntries = 2
	resetResponseCacheStateForTest(config)

	setResponseCache("key:1", "a", []json.RawMessage{responseCacheTestItem(1, "a")})
	setResponseCache("key:1", "b", []json.RawMessage{responseCacheTestItem(2, "b")})
	if got := getResponseCache("key:1", "a"); len(got) != 1 {
		t.Fatal("expected cache hit for a")
	}
	setResponseCache("key:1", "c", []json.RawMessage{responseCacheTestItem(3, "c")})

	if got := getResponseCache("key:1", "b"); got != nil {
		t.Fatalf("least-recently-used b was retained: %s", got)
	}
	if got := getResponseCache("key:1", "a"); len(got) != 1 {
		t.Fatal("recently used a was evicted")
	}
	stats := GetResponseCacheStats()
	if stats.Entries != 2 || stats.CountEvictions != 1 {
		t.Fatalf("stats = %+v, want 2 entries and 1 count eviction", stats)
	}
	if stats.Hits != 2 || stats.Misses != 1 {
		t.Fatalf("hits/misses = %d/%d, want 2/1", stats.Hits, stats.Misses)
	}
}

func TestResponseCacheAdmissionUsesPairAwareTailTrim(t *testing.T) {
	config := testResponseCacheConfig()
	resetResponseCacheStateForTest(config)
	items := make([]json.RawMessage, 202)
	for i := range items {
		items[i] = responseCacheTestItem(i, "")
	}
	items[1] = responseCacheCallItem("function_call", "crossing")
	items[2] = responseCacheCallItem("function_call_output", "crossing")

	setResponseCache("key:1", "trimmed", items)

	got := getResponseCache("key:1", "trimmed")
	if len(got) != 199 || string(got[0]) != string(items[3]) {
		t.Fatalf("admitted tail = %d items starting %s, want 199 items starting at item 3", len(got), got[0])
	}
}

func TestResponseCacheLocalHitReturnsIsolatedCopy(t *testing.T) {
	config := testResponseCacheConfig()
	resetResponseCacheStateForTest(config)
	original := responseCacheTestItem(1, "immutable")
	setResponseCache("key:1", "isolated", []json.RawMessage{original})

	first := getResponseCache("key:1", "isolated")
	statsBeforeMutation := GetResponseCacheStats()
	first[0][0] = 'X'
	second := getResponseCache("key:1", "isolated")
	statsAfterMutation := GetResponseCacheStats()

	if string(second[0]) != string(original) {
		t.Fatalf("caller mutation escaped into cache: got %s, want %s", second[0], original)
	}
	if statsAfterMutation.Entries != statsBeforeMutation.Entries ||
		statsAfterMutation.Bytes != statsBeforeMutation.Bytes ||
		statsAfterMutation.HighWaterBytes != statsBeforeMutation.HighWaterBytes ||
		statsAfterMutation.LargestSeenEntryBytes != statsBeforeMutation.LargestSeenEntryBytes {
		t.Fatalf("caller mutation changed retained stats: before=%+v after=%+v", statsBeforeMutation, statsAfterMutation)
	}
}

func TestResponseCacheConcurrentReturnedValueMutationIsIsolated(t *testing.T) {
	config := testResponseCacheConfig()
	resetResponseCacheStateForTest(config)
	original := responseCacheTestItem(1, "concurrent")
	setResponseCache("key:1", "concurrent", []json.RawMessage{original})
	left := getResponseCache("key:1", "concurrent")
	right := getResponseCache("key:1", "concurrent")

	var workers sync.WaitGroup
	start := make(chan struct{})
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 1000; i++ {
			left[0][0] = byte('X' + i%2)
		}
		left[0][0] = 'X'
	}()
	go func() {
		defer workers.Done()
		<-start
		var sink byte
		for i := 0; i < 1000; i++ {
			sink ^= right[0][0]
		}
		_ = sink
	}()
	close(start)
	workers.Wait()

	if got := getResponseCache("key:1", "concurrent"); string(got[0]) != string(original) {
		t.Fatalf("concurrent caller mutation escaped into cache: got %s, want %s", got[0], original)
	}
}

func TestResponseCacheRuntimeRefillDoesNotReturnRetainedCopy(t *testing.T) {
	config := testResponseCacheConfig()
	resetResponseCacheStateForTest(config)
	runtimeCache := newRecordingResponseContextBackend(true)
	runtimeCache.bounded = cache.ResponseContextReadResult{
		Status: cache.ResponseContextReadFound,
		Items:  []json.RawMessage{responseCacheTestItem(1, "runtime")},
	}
	SetResponseContextCache(runtimeCache)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = runtimeCache.TokenCache.Close()
	})

	original := responseCacheTestItem(1, "runtime")
	refilled := getResponseCache("key:1", "runtime")
	refilled[0][0] = 'X'

	if got := getResponseCache("key:1", "runtime"); string(got[0]) != string(original) {
		t.Fatalf("runtime refill exposed retained copy: got %s, want %s", got[0], original)
	}
}

type mutatingResponseContextCache struct {
	cache.TokenCache
	calls atomic.Int32
}

func (c *mutatingResponseContextCache) SharedAcrossInstances() bool { return true }

func (c *mutatingResponseContextCache) SetResponseContext(_ context.Context, _ string, items []json.RawMessage, _ time.Duration) error {
	c.calls.Add(1)
	if len(items) > 0 && len(items[0]) > 0 {
		items[0][0] = 'X'
	}
	return nil
}

func TestResponseCacheRuntimeWriterCannotMutateRetainedCopy(t *testing.T) {
	config := testResponseCacheConfig()
	resetResponseCacheStateForTest(config)
	baseCache := cache.NewMemory(10)
	runtimeCache := &mutatingResponseContextCache{TokenCache: baseCache}
	SetResponseContextCache(runtimeCache)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = baseCache.Close()
	})

	original := responseCacheTestItem(1, "runtime-writer")
	want := append(json.RawMessage(nil), original...)
	setResponseCache("key:1", "runtime-writer", []json.RawMessage{original})
	drainResponseCacheBackendWrites()

	if runtimeCache.calls.Load() != 1 {
		t.Fatal("mutating runtime writer was not invoked")
	}
	// 后台写入拿的是私有克隆：写入方的改动既到不了调用方切片，也到不了 L1。
	if original[0] == 'X' {
		t.Fatal("runtime writer mutated the caller's slice")
	}
	if got := getResponseCache("key:1", "runtime-writer"); string(got[0]) != string(want) {
		t.Fatalf("runtime writer mutated retained copy: got %s, want %s", got[0], want)
	}
}

func TestResponseCacheByteBudgetReplacementAndImmutableCopy(t *testing.T) {
	config := testResponseCacheConfig()
	first := []json.RawMessage{responseCacheTestItem(1, "first")}
	second := []json.RawMessage{responseCacheTestItem(2, "second")}
	replacement := []json.RawMessage{responseCacheTestItem(3, "replacement")}
	config.maxBytes = responseCacheItemsBytes(replacement) + responseCacheItemsBytes(second)
	resetResponseCacheStateForTest(config)

	setResponseCache("key:1", "a", first)
	setResponseCache("key:1", "b", second)
	setResponseCache("key:1", "a", replacement)
	replacement[0][0] = 'X'

	got := getResponseCache("key:1", "a")
	if len(got) != 1 || string(got[0]) != string(responseCacheTestItem(3, "replacement")) {
		t.Fatalf("cached replacement mutated with caller buffer: %s", got)
	}
	stats := GetResponseCacheStats()
	wantBytes := responseCacheItemsBytes([]json.RawMessage{responseCacheTestItem(3, "replacement"), second[0]})
	if stats.Bytes != wantBytes || stats.Entries != 2 {
		t.Fatalf("replacement stats = %+v, want bytes=%d entries=2", stats, wantBytes)
	}
	if stats.HighWaterBytes < stats.Bytes || stats.LargestSeenEntryBytes != responseCacheItemsBytes([]json.RawMessage{responseCacheTestItem(3, "replacement")}) {
		t.Fatalf("high-water/largest stats incorrect: %+v", stats)
	}

	setResponseCache("key:1", "c", []json.RawMessage{responseCacheTestItem(4, "c")})
	if got := getResponseCache("key:1", "b"); got != nil {
		t.Fatal("replacement must make a most-recent entry; b should be evicted first")
	}
	if got := getResponseCache("key:1", "a"); len(got) != 1 {
		t.Fatal("replacement a was not retained as the most-recent entry")
	}
}

func TestResponseCacheByteLRUEviction(t *testing.T) {
	config := testResponseCacheConfig()
	a := []json.RawMessage{responseCacheTestItem(1, "aaaaaaaa")}
	b := []json.RawMessage{responseCacheTestItem(2, "bbbbbbbb")}
	c := []json.RawMessage{responseCacheTestItem(3, "cccccccc")}
	config.maxBytes = responseCacheItemsBytes(a) + responseCacheItemsBytes(c)
	resetResponseCacheStateForTest(config)

	setResponseCache("key:1", "a", a)
	setResponseCache("key:1", "b", b)
	_ = getResponseCache("key:1", "a")
	setResponseCache("key:1", "c", c)

	if got := getResponseCache("key:1", "b"); got != nil {
		t.Fatal("oldest entry b should be byte-evicted")
	}
	stats := GetResponseCacheStats()
	if stats.ByteEvictions != 1 || stats.Bytes > config.maxBytes {
		t.Fatalf("stats = %+v, want one byte eviction within %d bytes", stats, config.maxBytes)
	}
}

func TestResponseCacheOversizeSkip(t *testing.T) {
	config := testResponseCacheConfig()
	oversize := []json.RawMessage{responseCacheTestItem(1, "too-large")}
	config.maxEntryBytes = responseCacheItemsBytes(oversize) - 1
	resetResponseCacheStateForTest(config)

	setResponseCache("key:1", "oversize", oversize)

	if got := getResponseCache("key:1", "oversize"); got != nil {
		t.Fatalf("oversize entry was admitted: %s", got)
	}
	stats := GetResponseCacheStats()
	if stats.Entries != 0 || stats.Bytes != 0 || stats.OversizeSkips != 1 {
		t.Fatalf("oversize stats = %+v, want empty cache and one skip", stats)
	}
	if stats.LargestSeenEntryBytes != responseCacheItemsBytes(oversize) {
		t.Fatalf("largest seen = %d, want %d", stats.LargestSeenEntryBytes, responseCacheItemsBytes(oversize))
	}
}

func TestResponseCacheRuntimeBudgetReductionEvictsImmediately(t *testing.T) {
	config := testResponseCacheConfig()
	big := []json.RawMessage{responseCacheTestItem(1, "big-entry")}
	old := []json.RawMessage{responseCacheTestItem(2, "old")}
	recent := []json.RawMessage{responseCacheTestItem(3, "recent")}
	resetResponseCacheStateForTest(config)
	setResponseCache("key:1", "big", big)
	setResponseCache("key:1", "old", old)
	setResponseCache("key:1", "recent", recent)
	_ = getResponseCache("key:1", "recent")

	shrunk := config
	shrunk.maxEntryBytes = responseCacheItemsBytes(big) - 1
	shrunk.maxEntries = 1
	shrunk.maxBytes = responseCacheItemsBytes(recent)
	configureResponseCacheForTest(shrunk)

	if got := getResponseCache("key:1", "recent"); len(got) != 1 {
		t.Fatal("most recent in-budget entry should survive runtime shrink")
	}
	if stats := GetResponseCacheStats(); stats.Entries != 1 || stats.Bytes > shrunk.maxBytes || stats.ByteEvictions == 0 || stats.CountEvictions == 0 {
		t.Fatalf("post-shrink stats = %+v, want immediate per-entry and count evictions", stats)
	}
}

func TestResponseCacheHitDoesNotExtendAbsoluteTTL(t *testing.T) {
	config := testResponseCacheConfig()
	config.ttl = time.Minute
	resetResponseCacheStateForTest(config)
	setResponseCache("key:1", "ttl", []json.RawMessage{responseCacheTestItem(1, "ttl")})

	storeKey := responseCacheStoreKey("key:1", "ttl")
	respCache.mu.Lock()
	expiresBeforeHit := respCache.store[storeKey].expiresAt
	respCache.mu.Unlock()
	if got := getResponseCache("key:1", "ttl"); len(got) != 1 {
		t.Fatal("entry expired before configured TTL")
	}
	respCache.mu.Lock()
	if expiresAfterHit := respCache.store[storeKey].expiresAt; !expiresAfterHit.Equal(expiresBeforeHit) {
		respCache.mu.Unlock()
		t.Fatalf("hit extended absolute TTL from %s to %s", expiresBeforeHit, expiresAfterHit)
	}
	respCache.store[storeKey].expiresAt = time.Now().Add(-time.Second)
	respCache.mu.Unlock()
	if got := getResponseCache("key:1", "ttl"); got != nil {
		t.Fatal("cache hit incorrectly extended absolute TTL")
	}
	stats := GetResponseCacheStats()
	if stats.Expirations != 1 || stats.Hits != 1 || stats.Misses != 1 {
		t.Fatalf("ttl stats = %+v, want expiration=1 hits=1 misses=1", stats)
	}
}

func TestResponseCacheConcurrentStatsSnapshot(t *testing.T) {
	config := testResponseCacheConfig()
	config.maxEntries = 16
	resetResponseCacheStateForTest(config)

	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for iteration := 0; iteration < 50; iteration++ {
				responseID := fmt.Sprintf("%d-%d", worker, iteration%20)
				setResponseCache("key:1", responseID, []json.RawMessage{responseCacheTestItem(iteration, responseID)})
				_ = getResponseCache("key:1", responseID)
				_ = GetResponseCacheStats()
			}
		}(worker)
	}
	workers.Wait()

	stats := GetResponseCacheStats()
	if stats.Entries > config.maxEntries || stats.Bytes > config.maxBytes {
		t.Fatalf("concurrent cache escaped limits: %+v", stats)
	}
}
