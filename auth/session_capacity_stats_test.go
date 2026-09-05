package auth

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func TestRelatedRequestSourceCardinalityIsBounded(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprintf("restored=%t", restored), func(t *testing.T) {
			store, account := newSessionCapacityTestStore(1)
			runtimeCache := cache.NewMemory(1)
			t.Cleanup(func() { _ = runtimeCache.Close() })
			store.tokenCache = runtimeCache
			const root = "bounded-sources-root"
			const sourceCount = maxRelatedSessionSources * 2
			sources := make([]AccountSessionRelatedSource, sourceCount)
			for index := range sources {
				sources[index] = AccountSessionRelatedSource{ThreadSource: fmt.Sprintf("future-source-%d", index), Count: 2}
			}
			if restored {
				payload, err := json.Marshal(persistedAccountSessionCollection{Version: 1, Sessions: []persistedAccountSessionState{{
					SessionID: root, LastSeen: time.Now(), RelatedRequestCount: sourceCount * 2, RelatedSources: sources,
				}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := runtimeCache.SetRuntime(t.Context(), accountSessionRuntimeNamespace, accountSessionRuntimeKey(account.DBID), payload, time.Minute); err != nil {
					t.Fatal(err)
				}
			} else {
				if !store.AdmitAccountSession(account, root, time.Now()) {
					t.Fatal("root admission failed")
				}
				for index, source := range sources {
					for request := range 2 {
						store.RecordRelatedAccountSession(account.DBID, RelatedSessionAffinityKey(root), source, fmt.Sprintf("%d-%d", index, request))
					}
				}
			}
			for range 2 {
				store.RecordRelatedAccountSession(account.DBID, RelatedSessionAffinityKey(root), sources[0], "retry-deduped")
			}
			snapshots := store.AccountSessionSnapshots(account.DBID, time.Now())
			if len(snapshots) != 1 || snapshots[0].RelatedRequestCount != sourceCount*2+1 {
				t.Fatalf("snapshots = %#v", snapshots)
			}
			if len(snapshots[0].RelatedSources) != maxRelatedSessionSources {
				t.Fatalf("source buckets = %d, want %d", len(snapshots[0].RelatedSources), maxRelatedSessionSources)
			}
			var total, known, overflow int64
			for _, source := range snapshots[0].RelatedSources {
				total += source.Count
				if source.ThreadSource == sources[0].ThreadSource {
					known = source.Count
				}
				if source.ThreadSource == "other" {
					overflow = source.Count
				}
			}
			if total != sourceCount*2+1 || known != 3 || overflow != (sourceCount-maxRelatedSessionSources+1)*2 {
				t.Fatalf("total=%d known=%d overflow=%d", total, known, overflow)
			}
			raw, found, err := runtimeCache.GetRuntime(t.Context(), accountSessionRuntimeNamespace, accountSessionRuntimeKey(account.DBID))
			if err != nil || !found {
				t.Fatalf("persisted state found=%t err=%v", found, err)
			}
			var persisted persistedAccountSessionCollection
			if err := json.Unmarshal(raw, &persisted); err != nil {
				t.Fatal(err)
			}
			if len(persisted.Sessions) != 1 || len(persisted.Sessions[0].RelatedSources) != maxRelatedSessionSources {
				t.Fatalf("persisted source buckets are not bounded: %#v", persisted)
			}
		})
	}
}

func TestAccountSessionCountPurgesExpiredWithoutChangingActiveStats(t *testing.T) {
	store, account := newSessionCapacityTestStore(2)
	now := time.Now()
	store.AdmitAccountSession(account, "expired", now.Add(-time.Minute))
	store.AdmitAccountSession(account, "active", now)
	store.RecordRelatedAccountSession(account.DBID, RelatedSessionAffinityKey("active"), AccountSessionRelatedSource{ThreadSource: "subagent"}, "request")
	if got := store.AccountSessionCount(account.DBID, now); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
	snapshots := store.AccountSessionSnapshots(account.DBID, now)
	if len(snapshots) != 1 || snapshots[0].SessionID != "active" || snapshots[0].RelatedRequestCount != 1 {
		t.Fatalf("active stats = %#v", snapshots)
	}
	if store.AccountSessionCount(999, now) != 0 || (*Store)(nil).AccountSessionCount(account.DBID, now) != 0 {
		t.Fatal("missing store/account should have zero sessions")
	}
	account.SessionCapacityEnabled = false
	if store.AccountSessionCount(account.DBID, now) != 0 {
		t.Fatal("disabled capacity should have zero sessions")
	}
}

func BenchmarkAccountSessionCount(b *testing.B) {
	store, account := newSessionCapacityTestStore(5)
	now := time.Now()
	for index := range 5 {
		root := fmt.Sprintf("root-%d", index)
		store.AdmitAccountSession(account, root, now)
		for source := range maxRelatedSessionSources {
			store.RecordRelatedAccountSession(account.DBID, RelatedSessionAffinityKey(root), AccountSessionRelatedSource{ThreadSource: fmt.Sprintf("source-%d", source)}, "")
		}
	}
	b.Run("count_only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			store.AccountSessionCount(account.DBID, now)
		}
	})
	b.Run("snapshot_count", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = len(store.AccountSessionSnapshots(account.DBID, now))
		}
	})
}
