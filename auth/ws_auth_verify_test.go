package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

// TestVerifyAccountAuthAsync 验证 WS 异常关闭后的鉴权验证探针：
// 触发一次探针，且 30s 内的重复触发被节流跳过。
func TestVerifyAccountAuthAsync(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})

	var calls int32
	done := make(chan struct{}, 4)
	store.SetUsageProbeFunc(func(ctx context.Context, a *Account) error {
		atomic.AddInt32(&calls, 1)
		done <- struct{}{}
		return nil
	})

	acc := &Account{DBID: 1, AccessToken: "at"}

	store.VerifyAccountAuthAsync(acc)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("期望触发一次鉴权验证探针")
	}

	// 节流：30s 内的第二次触发应被跳过。
	store.VerifyAccountAuthAsync(acc)
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("探针调用次数 = %d, want 1（应被节流）", got)
	}
}

// TestVerifyAccountAuthAsyncNoProbeFunc 未注册探针时不 panic、直接返回。
func TestVerifyAccountAuthAsyncNoProbeFunc(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	store.VerifyAccountAuthAsync(&Account{DBID: 1, AccessToken: "at"}) // 不应 panic
}
