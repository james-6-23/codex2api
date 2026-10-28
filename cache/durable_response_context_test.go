package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openTestDurableResponseContextStore(t *testing.T, maxBytes int64) *DurableResponseContextStore {
	t.Helper()
	store, err := OpenDurableResponseContextStore(filepath.Join(t.TempDir(), "contexts.db"), DurableResponseContextOptions{
		TTL: time.Hour, MaxBytes: maxBytes, SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestDurableResponseContextExpirationPreservesRenewedCandidates(t *testing.T) {
	for _, renewal := range []string{"rewrite", "read_touch", "pending_read_touch"} {
		t.Run(renewal, func(t *testing.T) {
			ctx := context.Background()
			store := openTestDurableResponseContextStore(t, 0)
			shared := json.RawMessage(`{"role":"user","content":"shared"}`)
			old := json.RawMessage(`{"role":"assistant","content":"old"}`)
			want := []json.RawMessage{shared, old}
			for _, key := range []string{"renewed", "expired", "retained"} {
				items := []json.RawMessage{shared}
				if key == "renewed" {
					items = want
				}
				if err := store.SetResponseContext(ctx, key, items, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			if renewal != "rewrite" {
				if _, err := store.GetResponseContext(ctx, "renewed"); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			if _, err := store.db.ExecContext(ctx, `UPDATE response_contexts SET expires_at = ? WHERE key IN ('renewed', 'expired')`, now.Add(-time.Minute).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			keys, err := store.selectKeys(ctx, `SELECT key FROM response_contexts WHERE expires_at <= ?`, now.UnixMilli())
			if err != nil || len(keys) != 2 {
				t.Fatalf("expiration candidates=%v err=%v", keys, err)
			}
			// Renew after selection to reproduce a stale expiration candidate.
			if renewal == "rewrite" {
				want = []json.RawMessage{shared, json.RawMessage(`{"role":"assistant","content":"new"}`)}
				if err := store.SetResponseContext(ctx, "renewed", want, time.Hour); err != nil {
					t.Fatal(err)
				}
			} else if renewal == "read_touch" {
				store.flushTouches()
			}
			deleted, err := store.deleteExpiredBatch(ctx, keys, now)
			if err != nil || deleted != 1 {
				t.Fatalf("expired deletions=%d err=%v, want only the still-expired row", deleted, err)
			}
			got, err := store.GetResponseContext(ctx, "renewed")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("renewed context=%s err=%v, want %s", got, err, want)
			}
			got, err = store.GetResponseContext(ctx, "retained")
			if err != nil || !reflect.DeepEqual(got, []json.RawMessage{shared}) {
				t.Fatalf("shared retained context=%s err=%v", got, err)
			}
			got, err = store.GetResponseContext(ctx, "expired")
			if err != nil || got != nil {
				t.Fatalf("expired context=%s err=%v, want removed", got, err)
			}
			stats, err := store.Stats(ctx)
			if err != nil || stats.Contexts != 2 || stats.Items != 2 || stats.ItemBytes != int64(len(shared)+len(want[1])) {
				t.Fatalf("retained stats=%+v err=%v", stats, err)
			}
			hash := durableResponseContextHash(shared)
			var refs int
			if err := store.db.QueryRowContext(ctx, `SELECT refs FROM response_context_items WHERE hash = ?`, hash[:]).Scan(&refs); err != nil || refs != 2 {
				t.Fatalf("shared refs=%d err=%v, want 2", refs, err)
			}
			if expired, evicted, err := store.Sweep(now); err != nil || expired != 0 || evicted != 0 {
				t.Fatalf("sweep after renewal: expired=%d evicted=%d err=%v", expired, evicted, err)
			}
			// Explicit invalidation must still remove a live context and its refs.
			if err := store.SetResponseContext(ctx, "renewed", nil, time.Hour); err != nil {
				t.Fatal(err)
			}
			stats, err = store.Stats(ctx)
			if err != nil || stats.Contexts != 1 || stats.Items != 1 || stats.ItemBytes != int64(len(shared)) {
				t.Fatalf("stats after explicit deletion=%+v err=%v", stats, err)
			}
		})
	}
}

func TestDurableResponseContextSweepEvictsLiveContextsOverBudget(t *testing.T) {
	ctx := context.Background()
	store := openTestDurableResponseContextStore(t, 64)
	for _, key := range []string{"a", "b"} {
		item := json.RawMessage(`{"text":"` + strings.Repeat(key, 32) + `"}`)
		if err := store.SetResponseContext(ctx, key, []json.RawMessage{item}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	expired, evicted, err := store.Sweep(time.Now())
	if err != nil || expired != 0 || evicted != 1 {
		t.Fatalf("sweep: expired=%d evicted=%d err=%v", expired, evicted, err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Contexts != 1 || stats.Items != 1 || stats.ItemBytes > store.MaxBytes() {
		t.Fatalf("stats after capacity eviction=%+v err=%v", stats, err)
	}
}

func TestDurableResponseContextReadDoesNotWaitForWriter(t *testing.T) {
	store := openTestDurableResponseContextStore(t, 0)
	items := []json.RawMessage{json.RawMessage(`{"text":"live"}`)}
	if err := store.SetResponseContext(context.Background(), "live", items, time.Hour); err != nil {
		t.Fatal(err)
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	done := make(chan error, 1)
	go func() {
		got, err := store.GetResponseContext(context.Background(), "live")
		if err == nil && !reflect.DeepEqual(got, items) {
			err = fmt.Errorf("context=%s, want %s", got, items)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read waited for the writer lock")
	}
}

func TestDurableResponseContextSweepDefersInFlightReader(t *testing.T) {
	ctx := context.Background()
	store := openTestDurableResponseContextStore(t, 0)
	items := []json.RawMessage{json.RawMessage(`{"text":"reading"}`)}
	if err := store.SetResponseContext(ctx, "reading", items, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Pause a reader after it observed a live snapshot, before recording its touch.
	store.beginRead("reading")
	now := time.Now()
	if _, err := store.db.ExecContext(ctx, `UPDATE response_contexts SET expires_at = ?`, now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		expired, evicted, err := store.Sweep(now)
		if err == nil && (expired != 0 || evicted != 0) {
			err = fmt.Errorf("expired=%d evicted=%d while reader is active", expired, evicted)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sweep spun on the in-flight reader")
	}
	store.recordTouch("reading")
	store.endRead("reading")
	if expired, evicted, err := store.Sweep(time.Now()); err != nil || expired != 0 || evicted != 0 {
		t.Fatalf("sweep after reader finished: expired=%d evicted=%d err=%v", expired, evicted, err)
	}
	got, err := store.GetResponseContext(ctx, "reading")
	if err != nil || !reflect.DeepEqual(got, items) {
		t.Fatalf("reader's renewal was lost: context=%s err=%v", got, err)
	}
}

func TestDurableResponseContextFailedTouchWritesRemainPending(t *testing.T) {
	ctx := context.Background()
	store := openTestDurableResponseContextStore(t, 0)
	items := []json.RawMessage{json.RawMessage(`{"text":"retry touch"}`)}
	if err := store.SetResponseContext(ctx, "renewed", items, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetResponseContext(ctx, "renewed"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := store.db.ExecContext(ctx, `UPDATE response_contexts SET expires_at = ?`, now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_touch BEFORE UPDATE OF expires_at ON response_contexts
		BEGIN SELECT RAISE(FAIL, 'touch write blocked'); END`); err != nil {
		t.Fatal(err)
	}
	store.flushTouches()
	if deleted, err := store.deleteExpiredBatch(ctx, []string{"renewed"}, now); err == nil || deleted != 0 {
		t.Fatalf("expected touch transaction failure, deleted=%d err=%v", deleted, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_touch`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.deleteExpiredBatch(ctx, []string{"renewed"}, now); err != nil || deleted != 0 {
		t.Fatalf("retry lost the pending renewal: deleted=%d err=%v", deleted, err)
	}
	got, err := store.GetResponseContext(ctx, "renewed")
	if err != nil || !reflect.DeepEqual(got, items) {
		t.Fatalf("renewed context after retry=%s err=%v", got, err)
	}
}

func TestDurableResponseContextLRUEvictionSerializesConcurrentRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := openTestDurableResponseContextStore(t, 64)
	for _, key := range []string{"a", "b"} {
		item := json.RawMessage(`{"text":"` + strings.Repeat(key, 32) + `"}`)
		if err := store.SetResponseContext(ctx, key, []json.RawMessage{item}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE response_contexts SET accessed_at = 0 WHERE key = 'a'`); err != nil {
		t.Fatal(err)
	}
	// Pause maintenance on its first DB operation. A refresh must wait until
	// selection and deletion both finish, then persist the new value.
	store.db.SetMaxOpenConns(1)
	connection, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	evictionDone := make(chan error, 1)
	go func() {
		deleted, err := store.evictLRUBatch(ctx, false)
		if err == nil && deleted != 1 {
			err = fmt.Errorf("evicted=%d, want 1", deleted)
		}
		evictionDone <- err
	}()
	for store.writeMu.TryLock() {
		store.writeMu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatal("LRU batch did not acquire the writer lock")
		case <-time.After(time.Millisecond):
		}
	}
	want := []json.RawMessage{json.RawMessage(`{"text":"` + strings.Repeat("x", 32) + `"}`)}
	writeDone := make(chan error, 1)
	go func() { writeDone <- store.SetResponseContext(ctx, "a", want, time.Hour) }()
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	for _, done := range []<-chan error{evictionDone, writeDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("eviction/refresh did not complete")
		}
	}
	got, err := store.GetResponseContext(ctx, "a")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent refresh was deleted: context=%s err=%v", got, err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Contexts != 2 || stats.Items != 2 {
		t.Fatalf("stats after eviction and refresh=%+v err=%v", stats, err)
	}
}
