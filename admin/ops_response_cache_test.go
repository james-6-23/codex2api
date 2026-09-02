package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestOpsResponseCacheMappingAndExactJSONShape(t *testing.T) {
	lastSync := time.Date(2026, 7, 29, 12, 34, 56, 789, time.UTC)
	snapshot := proxy.ResponseCacheOpsSnapshot{
		Stats: proxy.ResponseCacheStats{
			Entries:                7,
			Bytes:                  11,
			HighWaterBytes:         13,
			LargestSeenEntryBytes:  17,
			LocalHits:              19,
			LocalMisses:            23,
			RemoteHits:             29,
			RemoteMisses:           31,
			Expirations:            37,
			CountEvictions:         41,
			ByteEvictions:          43,
			OversizeBypasses:       47,
			OversizeRejections:     53,
			KnownUnavailableErrors: 59,
			SkippedWrites:          61,
			ChainOwners:            2,
		},
		EffectiveConfig: proxy.ResponseCacheAppliedConfig{
			Generation:          61,
			LocalMaxBytes:       64 << 20,
			LocalMaxEntryBytes:  8 << 20,
			ReconstructMaxBytes: 32 << 20,
			WritePolicy:         "always",
			MaxEntries:          2000,
		},
		AppliedConfig: proxy.ResponseCacheAppliedConfig{
			Generation:          61,
			LocalMaxBytes:       64 << 20,
			LocalMaxEntryBytes:  8 << 20,
			ReconstructMaxBytes: 32 << 20,
			WritePolicy:         "always",
			MaxEntries:          2000,
		},
		LastConfigSyncAt:    lastSync,
		LastConfigSyncError: "synthetic sync failure",
	}

	got := responseCacheOpsResponseFromSnapshot(snapshot)
	if got.EffectiveConfig.Generation != 61 || got.AppliedConfig.Generation != 61 {
		t.Fatalf("mapped generation = effective:%d applied:%d", got.EffectiveConfig.Generation, got.AppliedConfig.Generation)
	}
	if got.Entries != 7 || got.MaxEntries != 2000 || got.CurrentBytes != 11 || got.MaxBytes != 64<<20 {
		t.Fatalf("mapped capacity fields = %+v", got)
	}
	if got.LocalHits != 19 || got.RemoteMisses != 31 ||
		got.OversizeBypasses != 47 || got.KnownUnavailableErrors != 59 {
		t.Fatalf("mapped counters = %+v", got)
	}
	if got.LastConfigSyncAt != lastSync.Format(time.RFC3339Nano) ||
		got.LastConfigSyncError != snapshot.LastConfigSyncError {
		t.Fatalf("mapped sync state = %+v", got)
	}

	data, err := json.Marshal(opsOverviewResponse{ResponseCache: got})
	if err != nil {
		t.Fatalf("json.Marshal(opsOverviewResponse): %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode overview JSON: %v", err)
	}
	var responseCache map[string]json.RawMessage
	if err := json.Unmarshal(root["response_cache"], &responseCache); err != nil {
		t.Fatalf("decode response_cache JSON: %v; body=%s", err, data)
	}
	wantFields := []string{
		"effective_config",
		"applied_config",
		"entries",
		"max_entries",
		"current_bytes",
		"max_bytes",
		"high_water_bytes",
		"largest_entry_bytes",
		"local_hits",
		"local_misses",
		"remote_hits",
		"remote_misses",
		"expirations",
		"count_evictions",
		"byte_evictions",
		"oversize_bypasses",
		"oversize_rejections",
		"known_unavailable_errors",
		"skipped_writes",
		"chain_owners",
		"last_config_sync_at",
		"last_config_sync_error",
	}
	if len(responseCache) != len(wantFields) {
		t.Fatalf("response_cache field count = %d, want %d; body=%s", len(responseCache), len(wantFields), data)
	}
	for _, field := range wantFields {
		if _, ok := responseCache[field]; !ok {
			t.Fatalf("response_cache missing %q; body=%s", field, data)
		}
	}
	for _, field := range []string{"effective_config", "applied_config"} {
		var config map[string]json.RawMessage
		if err := json.Unmarshal(responseCache[field], &config); err != nil {
			t.Fatalf("decode %s: %v", field, err)
		}
		if len(config) != 5 {
			t.Fatalf("%s field count = %d, want 5; body=%s", field, len(config), responseCache[field])
		}
		for _, configField := range []string{
			"generation",
			"local_max_bytes",
			"local_max_entry_bytes",
			"reconstruct_max_bytes",
			"write_policy",
		} {
			if _, ok := config[configField]; !ok {
				t.Fatalf("%s missing %q; body=%s", field, configField, responseCache[field])
			}
		}
	}
}

func TestOpsResponseCacheSyncEmptyAndErrorMapping(t *testing.T) {
	empty := responseCacheOpsResponseFromSnapshot(proxy.ResponseCacheOpsSnapshot{})
	if empty.LastConfigSyncAt != "" || empty.LastConfigSyncError != "" {
		t.Fatalf("empty sync mapping = %+v, want empty strings", empty)
	}

	withError := responseCacheOpsResponseFromSnapshot(proxy.ResponseCacheOpsSnapshot{
		LastConfigSyncError: "database temporarily unavailable",
	})
	if withError.LastConfigSyncAt != "" ||
		withError.LastConfigSyncError != "database temporarily unavailable" {
		t.Fatalf("error sync mapping = %+v", withError)
	}
}

func TestCollectOpsMemoryReadsMemStatsOnceAndMapsHeapFields(t *testing.T) {
	calls := 0
	got := collectOpsMemory(func(mem *runtime.MemStats) {
		calls++
		*mem = runtime.MemStats{
			Alloc:        101,
			Sys:          202,
			HeapAlloc:    303,
			HeapInuse:    404,
			HeapReleased: 505,
			NumGC:        606,
		}
	})
	if calls != 1 {
		t.Fatalf("ReadMemStats calls = %d, want 1", calls)
	}
	if got.HeapAllocBytes != 303 ||
		got.HeapInuseBytes != 404 ||
		got.HeapReleasedBytes != 505 ||
		got.NumGC != 606 {
		t.Fatalf("heap mapping = %+v", got)
	}
}

func TestGetOpsOverviewIncludesResponseCacheAndMemoryShape(t *testing.T) {
	handler, _, _ := newResponseCacheSettingsAdminHandler(t)
	wantSnapshot := proxy.GetResponseCacheOpsSnapshot()
	memStatsCalls := 0
	handler.memReader = func(mem *runtime.MemStats) {
		memStatsCalls++
		runtime.ReadMemStats(mem)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/ops/overview", nil)
	handler.GetOpsOverview(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if memStatsCalls != 1 {
		t.Fatalf("overview ReadMemStats calls = %d, want 1", memStatsCalls)
	}
	body := recorder.Body.String()
	if got := gjson.Get(body, "response_cache.applied_config.generation").Int(); got != wantSnapshot.AppliedConfig.Generation {
		t.Fatalf("applied generation = %d, want %d; body=%s", got, wantSnapshot.AppliedConfig.Generation, body)
	}
	if got := gjson.Get(body, "response_cache.effective_config.generation").Int(); got != wantSnapshot.EffectiveConfig.Generation {
		t.Fatalf("effective generation = %d, want %d; body=%s", got, wantSnapshot.EffectiveConfig.Generation, body)
	}
	for _, path := range []string{
		"response_cache.entries",
		"response_cache.local_hits",
		"response_cache.remote_misses",
		"response_cache.last_config_sync_at",
		"memory.heap_alloc_bytes",
		"memory.heap_inuse_bytes",
		"memory.heap_released_bytes",
		"memory.num_gc",
	} {
		if !gjson.Get(body, path).Exists() {
			t.Fatalf("overview missing %q; body=%s", path, body)
		}
	}
}
