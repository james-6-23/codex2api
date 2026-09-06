package proxy

import (
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
)

func TestResponseCacheSharedBodiesAndOwnership(t *testing.T) {
	resetResponseCacheStateForTest(defaultResponseCacheConfig())
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	item := json.RawMessage(`{"type":"function_call","call_id":"c","name":"read","arguments":"{}"}`)
	original := string(item)
	setResponseCache("owner", "a", []json.RawMessage{item})
	setResponseCache("owner", "b", []json.RawMessage{item})
	a := getResponseCacheForReplay("owner", "a").Items
	b := getResponseCacheForReplay("owner", "b").Items
	if &a[0][0] != &b[0][0] {
		t.Fatal("identical history bodies were copied")
	}
	item[0] = '['
	if string(a[0]) != original {
		t.Fatal("caller mutation changed retained history")
	}
	b[0] = json.RawMessage(`{"replacement":true}`)
	if string(a[0]) != original {
		t.Fatal("reader sequence headers were shared")
	}
	private := getResponseCache("owner", "a")
	private[0][0] = '['
	if string(a[0]) != original {
		t.Fatal("mutable lookup changed immutable history")
	}
	if getResponseCacheForReplay("other", "a").Kind == responseCacheLookupHit {
		t.Fatal("cache ownership was bypassed")
	}
	config := defaultResponseCacheConfig()
	config.maxEntries = 0
	configureResponseCacheForTest(config)
	if stats := GetResponseCacheStats(); stats.SharedPayloadBytes != 0 || stats.Bytes != 0 {
		t.Fatalf("eviction leaked ownership: %+v", stats)
	}
	if string(a[0]) != original {
		t.Fatal("eviction invalidated an active replay")
	}
}

func TestResponseCacheReplayAllocationBounded(t *testing.T) {
	turns := replayCacheFixture()
	config := defaultResponseCacheConfig()
	config.maxBytes = 256 << 20
	resetResponseCacheStateForTest(config)
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for turn, items := range turns {
		id := fmt.Sprintf("resp_%d", turn)
		setResponseCache("allocation", id, items)
		got := getResponseCacheForReplay("allocation", id)
		if len(got.Items) != turn+1 {
			t.Fatal("replay history was truncated")
		}
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 12<<20 {
		t.Fatalf("128-turn cache allocated %d bytes, expected <12 MiB", allocated)
	}
	stats := GetResponseCacheStats()
	if stats.SharedPayloadBytes > 2<<20 || stats.SharedPayloadBytes >= stats.Bytes {
		t.Fatalf("history not shared: %+v", stats)
	}
}
