package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestAdminAuthThrottleBlocksAfterFailures(t *testing.T) {
	throttle := newAdminAuthThrottle()
	now := time.Unix(1_700_000_000, 0)

	for i := 0; i < adminAuthMaxFailures-1; i++ {
		if blocked := throttle.recordFailure("203.0.113.9", now); blocked != 0 {
			t.Fatalf("failure %d blocked early: %v", i+1, blocked)
		}
	}
	if retry := throttle.retryAfter("203.0.113.9", now); retry != 0 {
		t.Fatalf("retryAfter before threshold = %v, want 0", retry)
	}

	if blocked := throttle.recordFailure("203.0.113.9", now); blocked != adminAuthBlockDuration {
		t.Fatalf("threshold failure blocked for %v, want %v", blocked, adminAuthBlockDuration)
	}
	if retry := throttle.retryAfter("203.0.113.9", now.Add(time.Minute)); retry != adminAuthBlockDuration-time.Minute {
		t.Fatalf("retryAfter = %v, want %v", retry, adminAuthBlockDuration-time.Minute)
	}
	if retry := throttle.retryAfter("203.0.113.10", now); retry != 0 {
		t.Fatalf("other IP should not be blocked, got %v", retry)
	}
	if retry := throttle.retryAfter("203.0.113.9", now.Add(adminAuthBlockDuration)); retry != 0 {
		t.Fatalf("block should expire, got %v", retry)
	}
}

func TestAdminAuthThrottleResetsOnSuccess(t *testing.T) {
	throttle := newAdminAuthThrottle()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < adminAuthMaxFailures-1; i++ {
		throttle.recordFailure("203.0.113.11", now)
	}
	throttle.reset("203.0.113.11")
	if blocked := throttle.recordFailure("203.0.113.11", now); blocked != 0 {
		t.Fatalf("reset should clear failure count, blocked for %v", blocked)
	}
}

func TestAdminAuthThrottleExpiresOldFailures(t *testing.T) {
	throttle := newAdminAuthThrottle()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < adminAuthMaxFailures-1; i++ {
		throttle.recordFailure("203.0.113.12", now)
	}
	later := now.Add(adminAuthFailureWindow + time.Second)
	if blocked := throttle.recordFailure("203.0.113.12", later); blocked != 0 {
		t.Fatalf("failures outside the window should not accumulate, blocked for %v", blocked)
	}
}

func TestAdminAuthMiddlewareThrottlesFailedAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newSelfServiceHandler(t, false)
	router := gin.New()
	handler.RegisterRoutes(router)

	request := func(key string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/stats", nil)
		req.Header.Set("X-Admin-Key", key)
		router.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < adminAuthMaxFailures-1; i++ {
		if rec := request("wrong-key"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d returned %d, want 401", i+1, rec.Code)
		}
	}
	if rec := request("wrong-key"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("threshold failure returned %d, want 429", rec.Code)
	}
	if rec := request("wrong-key"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("failure during block returned %d, want 429", rec.Code)
	} else if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After")
	}
	if rec := request("admin-secret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct key during block returned %d, want 429 (no password oracle)", rec.Code)
	}
	// Expiry must allow normal authentication again, including no-store headers.
	handler.adminAuthThrottle.mu.Lock()
	for _, entry := range handler.adminAuthThrottle.entries {
		entry.blockedUntil = time.Now().Add(-time.Second)
	}
	handler.adminAuthThrottle.mu.Unlock()
	if rec := request("admin-secret"); rec.Code != http.StatusOK {
		t.Fatalf("correct key after expiry returned %d, want 200", rec.Code)
	} else if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("admin response Cache-Control = %q, want no-store", got)
	}
	if rec := request("wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("failure after successful login returned %d, want 401", rec.Code)
	}
}

func TestAdminAuthMiddlewareMissingKeyDoesNotCountAsFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newSelfServiceHandler(t, false)
	router := gin.New()
	handler.RegisterRoutes(router)

	// 未登录的管理页每次加载都会无密钥探测 health；反复刷新不能把 IP 锁住。
	for i := 0; i < adminAuthMaxFailures*3; i++ {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/health", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("keyless probe %d returned %d, want 401", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct key after keyless probes returned %d, want 200", rec.Code)
	}
}

func TestAdminAuthThrottleBoundsTrackedIPs(t *testing.T) {
	throttle := newAdminAuthThrottle()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < adminAuthMaxEntries+200; i++ {
		throttle.recordFailure(fmt.Sprintf("198.51.100.%d", i), now)
	}
	if got := len(throttle.entries); got > adminAuthMaxEntries {
		t.Fatalf("tracked IPs = %d, want <= %d", got, adminAuthMaxEntries)
	}
}

func TestAdminAuthThrottleSaturationPreservesLiveEntries(t *testing.T) {
	throttle := newAdminAuthThrottle()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < adminAuthMaxEntries; i++ {
		ip := fmt.Sprintf("source-%d", i)
		throttle.entries[ip] = &adminAuthThrottleEntry{windowStart: now, failures: adminAuthMaxFailures - 1}
	}
	throttle.recordFailure("source-0", now)
	if retry := throttle.retryAfter("untracked", now.Add(time.Minute)); retry != 9*time.Minute {
		t.Fatalf("saturated admission retry = %v, want 9m", retry)
	}
	if retry := throttle.recordFailure("untracked", now.Add(time.Minute)); retry == 0 {
		t.Fatal("saturated failure tracking accepted a new source")
	}
	if retry := throttle.retryAfter("source-0", now.Add(time.Minute)); retry != 9*time.Minute {
		t.Fatalf("live block was evicted: %v", retry)
	}
	if got := throttle.recordFailure("source-1", now.Add(time.Minute)); got != adminAuthBlockDuration {
		t.Fatalf("live failure count was lost: %v", got)
	}
	if len(throttle.entries) != adminAuthMaxEntries {
		t.Fatalf("table size = %d", len(throttle.entries))
	}
	if retry := throttle.retryAfter("untracked", now.Add(adminAuthFailureWindow)); retry != 0 {
		t.Fatalf("expired slots did not recover: %v", retry)
	}
	if retry := throttle.retryAfter("source-1", now.Add(adminAuthFailureWindow)); retry != time.Minute {
		t.Fatalf("later block must survive cleanup: %v", retry)
	}
}

func TestAdminAuthMiddlewareRejectsUntrackedSourceWhenFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newSelfServiceHandler(t, false)
	now := time.Now()
	for i := 0; i < adminAuthMaxEntries; i++ {
		handler.adminAuthThrottle.entries[fmt.Sprintf("source-%d", i)] = &adminAuthThrottleEntry{windowStart: now}
	}
	router := gin.New()
	handler.RegisterRoutes(router)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/stats", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("untracked valid candidate at capacity = %d %s, want 429 with Retry-After", rec.Code, rec.Body.String())
	}
}
