package cache

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// ==================== Memory Cache Tests ====================

func TestNewMemory(t *testing.T) {
	tc := NewMemory(10)
	if tc == nil {
		t.Fatal("NewMemory() returned nil")
	}
	if tc.Driver() != "memory" {
		t.Fatalf("Driver() = %s, want memory", tc.Driver())
	}
	if tc.Label() != "Memory" {
		t.Fatalf("Label() = %s, want Memory", tc.Label())
	}
}

func TestNewMemory_DefaultPoolSize(t *testing.T) {
	tc := NewMemory(0)
	if tc.PoolSize() != 1 {
		t.Fatalf("PoolSize() = %d, want 1", tc.PoolSize())
	}
}

func TestBuildRedisClientOptionsSupportsRedissURL(t *testing.T) {
	opts, err := buildRedisClientOptions(RedisOptions{
		Addr:     "rediss://default:secret@example.upstash.io:6379/2",
		PoolSize: 42,
	})
	if err != nil {
		t.Fatalf("buildRedisClientOptions() error = %v", err)
	}
	if opts.Addr != "example.upstash.io:6379" {
		t.Fatalf("Addr = %q, want example.upstash.io:6379", opts.Addr)
	}
	if opts.Username != "default" {
		t.Fatalf("Username = %q, want default", opts.Username)
	}
	if opts.Password != "secret" {
		t.Fatalf("Password = %q, want secret", opts.Password)
	}
	if opts.DB != 2 {
		t.Fatalf("DB = %d, want 2", opts.DB)
	}
	if opts.PoolSize != 42 {
		t.Fatalf("PoolSize = %d, want 42", opts.PoolSize)
	}
	if opts.TLSConfig == nil {
		t.Fatal("TLSConfig is nil for rediss URL")
	}
	if opts.TLSConfig.ServerName != "example.upstash.io" {
		t.Fatalf("TLS ServerName = %q, want example.upstash.io", opts.TLSConfig.ServerName)
	}
}

func TestBuildRedisClientOptionsSupportsHostPortTLS(t *testing.T) {
	opts, err := buildRedisClientOptions(RedisOptions{
		Addr:               "redis.example.com:6380",
		Username:           "default",
		Password:           "secret",
		DB:                 1,
		TLS:                true,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("buildRedisClientOptions() error = %v", err)
	}
	if opts.Username != "default" || opts.Password != "secret" || opts.DB != 1 {
		t.Fatalf("opts auth/db = %q/%q/%d, want default/secret/1", opts.Username, opts.Password, opts.DB)
	}
	if opts.TLSConfig == nil {
		t.Fatal("TLSConfig is nil when TLS is enabled")
	}
	if opts.TLSConfig.ServerName != "redis.example.com" {
		t.Fatalf("TLS ServerName = %q, want redis.example.com", opts.TLSConfig.ServerName)
	}
	if !opts.TLSConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be true")
	}
}

func TestRedactRedisAddrMasksPassword(t *testing.T) {
	got := RedactRedisAddr("rediss://default:secret@example.upstash.io:6379/0")
	want := "rediss://default:redacted@example.upstash.io:6379/0"
	if got != want {
		t.Fatalf("RedactRedisAddr() = %q, want %q", got, want)
	}
}

func TestRedisConnectionHintForEOFWithoutTLS(t *testing.T) {
	got := redisConnectionHint(io.EOF, false)
	if got == "" {
		t.Fatal("redisConnectionHint() should include TLS hint for EOF without TLS")
	}
	if tlsHint := redisConnectionHint(io.EOF, true); tlsHint != "" {
		t.Fatalf("redisConnectionHint() with TLS = %q, want empty", tlsHint)
	}
}

func TestMemoryTokenCache_SetAndGetAccessToken(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	// Set token
	err := tc.SetAccessToken(ctx, 1, "test-token", time.Hour)
	if err != nil {
		t.Fatalf("SetAccessToken() error = %v", err)
	}

	// Get token
	token, err := tc.GetAccessToken(ctx, 1)
	if err != nil {
		t.Fatalf("GetAccessToken() error = %v", err)
	}
	if token != "test-token" {
		t.Fatalf("GetAccessToken() = %s, want test-token", token)
	}
}

func TestMemoryTokenCache_RuntimeCacheRoundTrip(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()
	value := json.RawMessage(`{"ok":true}`)

	if err := tc.SetRuntime(ctx, "test-runtime", "key-1", value, time.Minute); err != nil {
		t.Fatalf("SetRuntime() error = %v", err)
	}
	got, ok, err := tc.GetRuntime(ctx, "test-runtime", "key-1")
	if err != nil {
		t.Fatalf("GetRuntime() error = %v", err)
	}
	if !ok {
		t.Fatal("GetRuntime() cache miss, want hit")
	}
	if string(got) != string(value) {
		t.Fatalf("GetRuntime() = %s, want %s", got, value)
	}
	if err := tc.DeleteRuntime(ctx, "test-runtime", "key-1"); err != nil {
		t.Fatalf("DeleteRuntime() error = %v", err)
	}
	if _, ok, err := tc.GetRuntime(ctx, "test-runtime", "key-1"); err != nil || ok {
		t.Fatalf("GetRuntime() after delete ok=%v err=%v, want miss without error", ok, err)
	}
}

func TestMemoryTokenCache_GetAccessToken_NotFound(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	token, err := tc.GetAccessToken(ctx, 999)
	if err != nil {
		t.Fatalf("GetAccessToken() error = %v", err)
	}
	if token != "" {
		t.Fatalf("GetAccessToken() = %s, want empty", token)
	}
}

func TestMemoryTokenCache_GetAccessToken_Expired(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	// Set token with negative TTL (already expired)
	err := tc.SetAccessToken(ctx, 1, "expired-token", -time.Hour)
	if err != nil {
		t.Fatalf("SetAccessToken() error = %v", err)
	}

	// Should return empty for expired token
	token, err := tc.GetAccessToken(ctx, 1)
	if err != nil {
		t.Fatalf("GetAccessToken() error = %v", err)
	}
	if token != "" {
		t.Fatalf("GetAccessToken() = %s, want empty for expired token", token)
	}
}

func TestMemoryTokenCache_DeleteAccessToken(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	// Set and verify
	if err := tc.SetAccessToken(ctx, 1, "test-token", time.Hour); err != nil {
		t.Fatalf("SetAccessToken() error = %v", err)
	}
	token, err := tc.GetAccessToken(ctx, 1)
	if err != nil {
		t.Fatalf("GetAccessToken() error = %v", err)
	}
	if token != "test-token" {
		t.Fatal("Token should exist before deletion")
	}

	// Delete
	err = tc.DeleteAccessToken(ctx, 1)
	if err != nil {
		t.Fatalf("DeleteAccessToken() error = %v", err)
	}

	// Verify deletion
	token, err = tc.GetAccessToken(ctx, 1)
	if err != nil {
		t.Fatalf("GetAccessToken() error = %v", err)
	}
	if token != "" {
		t.Fatal("Token should be deleted")
	}
}

func TestMemoryTokenCache_RefreshLock(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	// Acquire lock
	acquired, err := tc.AcquireRefreshLock(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("AcquireRefreshLock() error = %v", err)
	}
	if !acquired {
		t.Fatal("AcquireRefreshLock() should return true for first acquisition")
	}

	// Second acquisition should fail
	acquired, err = tc.AcquireRefreshLock(ctx, 1, time.Minute)
	if err != nil {
		t.Fatalf("AcquireRefreshLock() error = %v", err)
	}
	if acquired {
		t.Fatal("AcquireRefreshLock() should return false when lock is held")
	}
}

func TestMemoryTokenCache_RefreshLock_Expired(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	// Acquire lock with very short TTL
	acquired, _ := tc.AcquireRefreshLock(ctx, 1, time.Millisecond)
	if !acquired {
		t.Fatal("Should acquire lock")
	}

	// Wait for expiration
	time.Sleep(10 * time.Millisecond)

	// Should be able to acquire again
	acquired, _ = tc.AcquireRefreshLock(ctx, 1, time.Minute)
	if !acquired {
		t.Fatal("Should acquire lock after expiration")
	}
}

func TestMemoryTokenCache_ReleaseRefreshLock(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	// Acquire and release
	tc.AcquireRefreshLock(ctx, 1, time.Minute)
	err := tc.ReleaseRefreshLock(ctx, 1)
	if err != nil {
		t.Fatalf("ReleaseRefreshLock() error = %v", err)
	}

	// Should be able to acquire again
	acquired, _ := tc.AcquireRefreshLock(ctx, 1, time.Minute)
	if !acquired {
		t.Fatal("Should acquire lock after release")
	}
}

func TestMemoryTokenCache_LeaseRequiresMatchingOwner(t *testing.T) {
	tc := NewMemory(10)
	t.Cleanup(func() { _ = tc.Close() })
	ctx := context.Background()

	acquired, err := tc.AcquireLease(ctx, "reset-credit", "workspace:1", "owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first AcquireLease = (%v,%v), want (true,nil)", acquired, err)
	}
	acquired, err = tc.AcquireLease(ctx, "reset-credit", "workspace:1", "owner-b", time.Minute)
	if err != nil || acquired {
		t.Fatalf("second AcquireLease = (%v,%v), want (false,nil)", acquired, err)
	}
	if err := tc.ReleaseLease(ctx, "reset-credit", "workspace:1", "owner-b"); err != nil {
		t.Fatalf("ReleaseLease wrong owner: %v", err)
	}
	acquired, err = tc.AcquireLease(ctx, "reset-credit", "workspace:1", "owner-b", time.Minute)
	if err != nil || acquired {
		t.Fatalf("AcquireLease after wrong-owner release = (%v,%v), want (false,nil)", acquired, err)
	}
	if err := tc.ReleaseLease(ctx, "reset-credit", "workspace:1", "owner-a"); err != nil {
		t.Fatalf("ReleaseLease owner-a: %v", err)
	}
	acquired, err = tc.AcquireLease(ctx, "reset-credit", "workspace:1", "owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("AcquireLease after owner release = (%v,%v), want (true,nil)", acquired, err)
	}
}

func TestMemoryTokenCache_WaitForRefreshComplete(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	// Set token and acquire lock
	tc.SetAccessToken(ctx, 1, "new-token", time.Hour)
	tc.AcquireRefreshLock(ctx, 1, time.Hour)

	// Start waiting in background
	var result string
	var err error
	done := make(chan struct{})
	go func() {
		result, err = tc.WaitForRefreshComplete(ctx, 1, time.Second)
		close(done)
	}()

	// Release lock after short delay
	time.Sleep(50 * time.Millisecond)
	tc.ReleaseRefreshLock(ctx, 1)

	// Wait for completion
	select {
	case <-done:
		if err != nil {
			t.Fatalf("WaitForRefreshComplete() error = %v", err)
		}
		if result != "new-token" {
			t.Fatalf("WaitForRefreshComplete() = %s, want new-token", result)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForRefreshComplete() timeout")
	}
}

func TestMemoryTokenCache_WaitForRefreshComplete_Timeout(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	// Acquire lock but don't release
	tc.AcquireRefreshLock(ctx, 1, time.Hour)

	// Should timeout
	_, err := tc.WaitForRefreshComplete(ctx, 1, 50*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForRefreshComplete() should return error on timeout")
	}
}

func TestMemoryTokenCache_WaitForRefreshComplete_ContextCanceled(t *testing.T) {
	tc := NewMemory(10)
	ctx, cancel := context.WithCancel(context.Background())

	// Acquire lock
	tc.AcquireRefreshLock(ctx, 1, time.Hour)

	// Cancel context
	cancel()

	// Should return context error
	_, err := tc.WaitForRefreshComplete(ctx, 1, time.Second)
	if err != ctx.Err() {
		t.Fatalf("WaitForRefreshComplete() error = %v, want context error", err)
	}
}

func TestMemoryTokenCache_SetPoolSize(t *testing.T) {
	tc := NewMemory(10)

	tc.SetPoolSize(20)
	if tc.PoolSize() != 20 {
		t.Fatalf("PoolSize() = %d, want 20", tc.PoolSize())
	}

	// Test zero value defaults to 1
	tc.SetPoolSize(0)
	if tc.PoolSize() != 1 {
		t.Fatalf("PoolSize() = %d, want 1 after setting 0", tc.PoolSize())
	}
}

func TestMemoryTokenCache_Stats(t *testing.T) {
	tc := NewMemory(10)
	stats := tc.Stats()

	if stats.TotalConns != 1 {
		t.Fatalf("Stats().TotalConns = %d, want 1", stats.TotalConns)
	}
	if stats.IdleConns != 1 {
		t.Fatalf("Stats().IdleConns = %d, want 1", stats.IdleConns)
	}
	if stats.StaleConns != 0 {
		t.Fatalf("Stats().StaleConns = %d, want 0", stats.StaleConns)
	}
}

func TestMemoryTokenCache_Ping(t *testing.T) {
	tc := NewMemory(10)
	ctx := context.Background()

	err := tc.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
}

func TestMemoryTokenCache_Close(t *testing.T) {
	tc := NewMemory(10)
	err := tc.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// ==================== Benchmarks ====================

func BenchmarkMemorySetAccessToken(b *testing.B) {
	tc := NewMemory(100)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		tc.SetAccessToken(ctx, int64(i%1000), "token", time.Hour)
	}
}

func BenchmarkMemoryGetAccessToken(b *testing.B) {
	tc := NewMemory(100)
	ctx := context.Background()
	tc.SetAccessToken(ctx, 1, "token", time.Hour)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		tc.GetAccessToken(ctx, 1)
	}
}

func BenchmarkMemoryGetAccessTokenParallel(b *testing.B) {
	tc := NewMemory(100)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		tc.SetAccessToken(ctx, int64(i), "token", time.Hour)
	}

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			tc.GetAccessToken(ctx, int64(i%100))
			i++
		}
	})
}
