package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestAPIKeyConcurrencyLimiterAcquireRelease(t *testing.T) {
	limiter := newAPIKeyConcurrencyLimiter()

	release1, current, ok := limiter.acquire(42, 2)
	if !ok || current != 1 || release1 == nil {
		t.Fatalf("first acquire = (%v, %d), want ok current=1", ok, current)
	}
	release2, current, ok := limiter.acquire(42, 2)
	if !ok || current != 2 || release2 == nil {
		t.Fatalf("second acquire = (%v, %d), want ok current=2", ok, current)
	}
	if release3, current, ok := limiter.acquire(42, 2); ok || release3 != nil || current != 2 {
		t.Fatalf("third acquire = (ok=%v, current=%d, releaseNil=%v), want rejected current=2", ok, current, release3 == nil)
	}

	release1()
	release1()
	release3, current, ok := limiter.acquire(42, 2)
	if !ok || current != 2 || release3 == nil {
		t.Fatalf("acquire after release = (%v, %d), want ok current=2", ok, current)
	}

	release2()
	release3()
	limiter.mu.Lock()
	counter := limiter.counters[42]
	limiter.mu.Unlock()
	if counter == nil {
		t.Fatal("counter was removed; keeping it avoids orphan-counter acquire races")
	}
	if got := atomic.LoadInt64(&counter.inflight); got != 0 {
		t.Fatalf("inflight after all releases = %d, want 0", got)
	}
}

func TestAPIKeyConcurrencyLimiterBypassesUnlimited(t *testing.T) {
	limiter := newAPIKeyConcurrencyLimiter()
	for i := 0; i < 3; i++ {
		release, current, ok := limiter.acquire(42, 0)
		if !ok || release != nil || current != 0 {
			t.Fatalf("unlimited acquire = (ok=%v, current=%d, releaseNil=%v), want bypass", ok, current, release == nil)
		}
	}
}

func TestAcquireAPIKeyConcurrencyRejectsWhenFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	row := &database.APIKeyRow{
		ID: 7,
		Limits: database.APIKeyLimits{
			MaxConcurrency: 1,
		},
	}

	firstCtx, firstRecorder := testAPIKeyConcurrencyContext(row)
	firstRelease, ok := handler.acquireAPIKeyConcurrency(firstCtx)
	if !ok || firstRelease == nil {
		t.Fatalf("first acquire ok=%v releaseNil=%v, want acquired", ok, firstRelease == nil)
	}
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first recorder status = %d, want untouched 200", firstRecorder.Code)
	}

	secondCtx, secondRecorder := testAPIKeyConcurrencyContext(row)
	secondRelease, ok := handler.acquireAPIKeyConcurrency(secondCtx)
	if ok || secondRelease != nil {
		t.Fatalf("second acquire ok=%v releaseNil=%v, want rejected", ok, secondRelease == nil)
	}
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second recorder status = %d, want 429", secondRecorder.Code)
	}
	if !strings.Contains(secondRecorder.Body.String(), "API key concurrency limit exceeded") {
		t.Fatalf("second response body = %q, want concurrency error", secondRecorder.Body.String())
	}

	firstRelease()
	thirdCtx, thirdRecorder := testAPIKeyConcurrencyContext(row)
	thirdRelease, ok := handler.acquireAPIKeyConcurrency(thirdCtx)
	if !ok || thirdRelease == nil {
		t.Fatalf("third acquire ok=%v releaseNil=%v, want acquired after release", ok, thirdRelease == nil)
	}
	thirdRelease()
	if thirdRecorder.Code != http.StatusOK {
		t.Fatalf("third recorder status = %d, want untouched 200", thirdRecorder.Code)
	}
}

func TestAcquireAPIKeyConcurrencyUsesPerHandlerLimiter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{MaxConcurrency: 1}}

	firstHandler := &Handler{}
	firstCtx, _ := testAPIKeyConcurrencyContext(row)
	firstRelease, ok := firstHandler.acquireAPIKeyConcurrency(firstCtx)
	if !ok || firstRelease == nil {
		t.Fatalf("first handler acquire ok=%v releaseNil=%v, want acquired", ok, firstRelease == nil)
	}
	defer firstRelease()

	secondHandler := &Handler{}
	secondCtx, secondRecorder := testAPIKeyConcurrencyContext(row)
	secondRelease, ok := secondHandler.acquireAPIKeyConcurrency(secondCtx)
	if !ok || secondRelease == nil {
		t.Fatalf("second handler acquire ok=%v releaseNil=%v, want independent limiter", ok, secondRelease == nil)
	}
	secondRelease()
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second handler recorder status = %d, want untouched 200", secondRecorder.Code)
	}
}

func TestAcquireAPIKeyConcurrencyInheritedByInternalChild(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{MaxConcurrency: 1}}

	parent, _ := testAPIKeyConcurrencyContext(row)
	releaseParent, ok := handler.acquireAPIKeyConcurrency(parent)
	if !ok || releaseParent == nil {
		t.Fatalf("parent acquire ok=%v releaseNil=%v, want acquired", ok, releaseParent == nil)
	}
	defer releaseParent()

	child, recorder := testAPIKeyConcurrencyContext(row)
	child.Set(contextAPIKeyConcurrencyInherited, true)
	releaseChild, ok := handler.acquireAPIKeyConcurrency(child)
	if !ok || releaseChild != nil {
		t.Fatalf("inherited child acquire ok=%v releaseNil=%v, want bypass", ok, releaseChild == nil)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("inherited child status = %d, want untouched 200", recorder.Code)
	}
}

func TestAcquireAPIKeyConcurrencyAllowsVerifiedPassiveGuardianAlongsideParent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{MaxConcurrency: 1}}

	parent, _ := testAPIKeyConcurrencyContext(row)
	releaseParent, ok := handler.acquireAPIKeyConcurrency(parent)
	if !ok || releaseParent == nil {
		t.Fatalf("parent acquire ok=%v releaseNil=%v, want acquired", ok, releaseParent == nil)
	}
	defer releaseParent()

	guardian, recorder := testAPIKeyConcurrencyContext(row)
	setPassiveInternalAuthorization(guardian, true)
	releaseGuardian, ok := handler.acquireAPIKeyConcurrency(guardian)
	if !ok || releaseGuardian == nil {
		t.Fatalf("verified Guardian acquire ok=%v releaseNil=%v, want one protected lease", ok, releaseGuardian == nil)
	}
	defer releaseGuardian()
	if recorder.Code != http.StatusOK {
		t.Fatalf("verified Guardian status = %d, want untouched 200", recorder.Code)
	}
	second, secondRecorder := testAPIKeyConcurrencyContext(row)
	setPassiveInternalAuthorization(second, true)
	if releaseSecond, secondOK := handler.acquireAPIKeyConcurrency(second); secondOK || releaseSecond != nil || secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second Guardian acquire ok=%v releaseNil=%v status=%d, want bounded 429", secondOK, releaseSecond == nil, secondRecorder.Code)
	}
}

func TestAcquireAPIKeyWebSocketConcurrencyAllowsVerifiedPassiveGuardianAlongsideParent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{MaxConcurrency: 1}}

	parent, _ := testAPIKeyConcurrencyContext(row)
	releaseParent, ok := handler.acquireAPIKeyConcurrency(parent)
	if !ok || releaseParent == nil {
		t.Fatalf("parent acquire ok=%v releaseNil=%v, want acquired", ok, releaseParent == nil)
	}
	defer releaseParent()

	guardian, _ := testAPIKeyConcurrencyContext(row)
	setPassiveInternalAuthorization(guardian, true)
	releaseGuardian, apiErr, ok := handler.acquireAPIKeyConcurrencyForWebSocket(guardian)
	if !ok || releaseGuardian == nil || apiErr != nil {
		t.Fatalf("verified Guardian WS acquire ok=%v releaseNil=%v err=%v, want one protected lease", ok, releaseGuardian == nil, apiErr)
	}
	defer releaseGuardian()
	second, _ := testAPIKeyConcurrencyContext(row)
	setPassiveInternalAuthorization(second, true)
	if releaseSecond, secondErr, secondOK := handler.acquireAPIKeyConcurrencyForWebSocket(second); secondOK || releaseSecond != nil || secondErr == nil {
		t.Fatalf("second Guardian WS acquire ok=%v releaseNil=%v err=%v, want bounded rejection", secondOK, releaseSecond == nil, secondErr)
	}
}

func TestFieldClassifiedInternalRequestBypassesAPIKeyModelNameDrift(t *testing.T) {
	row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{ModelAllow: []string{"gpt-5.6-sol"}}}
	internal, _ := testAPIKeyConcurrencyContext(row)
	setPassiveInternalAuthorization(internal, true)
	if status, message := (&Handler{}).enforceAPIKeyLimits(internal, "future-review-model"); status != 0 || message != "" {
		t.Fatalf("field-classified internal model was rejected: status=%d message=%q", status, message)
	}

	ordinary, _ := testAPIKeyConcurrencyContext(row)
	if status, _ := (&Handler{}).enforceAPIKeyLimits(ordinary, "future-review-model"); status != http.StatusForbidden {
		t.Fatalf("ordinary unknown model status=%d, want 403", status)
	}
}

func testAPIKeyConcurrencyContext(row *database.APIKeyRow) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set(contextAPIKeyRow, row)
	return ctx, recorder
}
