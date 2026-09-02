package cache

import (
	"context"
	"testing"
	"time"
)

func TestMemoryRuntimeOwnerStoreStaleRefreshAndDeleteAreSafe(t *testing.T) {
	tokenCache := NewMemory(1)
	store, ok := tokenCache.(RuntimeOwnerStore)
	if !ok {
		t.Fatal("memory cache does not implement RuntimeOwnerStore")
	}
	ctx := context.Background()

	previous, err := store.ClaimRuntimeOwner(ctx, "ws-preempt", "session", []byte("owner-a"), time.Minute)
	if err != nil || len(previous) != 0 {
		t.Fatalf("first claim = %q, %v; want empty", previous, err)
	}
	previous, err = store.ClaimRuntimeOwner(ctx, "ws-preempt", "session", []byte("owner-b"), time.Minute)
	if err != nil || string(previous) != "owner-a" {
		t.Fatalf("replacement claim = %q, %v; want owner-a", previous, err)
	}
	if refreshed, err := store.CompareAndRefreshRuntimeOwner(ctx, "ws-preempt", "session", []byte("owner-a"), time.Minute); err != nil || refreshed {
		t.Fatalf("stale refresh = %t, %v; want false", refreshed, err)
	}
	if deleted, err := store.CompareAndDeleteRuntimeOwner(ctx, "ws-preempt", "session", []byte("owner-a")); err != nil || deleted {
		t.Fatalf("stale delete = %t, %v; want false", deleted, err)
	}
	if refreshed, err := store.CompareAndRefreshRuntimeOwner(ctx, "ws-preempt", "session", []byte("owner-b"), time.Minute); err != nil || !refreshed {
		t.Fatalf("current refresh = %t, %v; want true", refreshed, err)
	}
	if deleted, err := store.CompareAndDeleteRuntimeOwner(ctx, "ws-preempt", "session", []byte("owner-b")); err != nil || !deleted {
		t.Fatalf("current delete = %t, %v; want true", deleted, err)
	}
}
