package proxy

import (
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

// apiKeyConcurrencyLimiter tracks per-API-key inflight proxy requests in this
// process only. A request occupies one slot after API Key limits pass and keeps
// it until the handler returns, covering account scheduling, upstream retries,
// streaming, and WebSocket turn processing time.
type apiKeyConcurrencyLimiter struct {
	mu       sync.Mutex
	counters map[int64]*apiKeyConcurrencyCounter
}

type apiKeyConcurrencyCounter struct {
	inflight atomic.Int64
}

func newAPIKeyConcurrencyLimiter() *apiKeyConcurrencyLimiter {
	return &apiKeyConcurrencyLimiter{counters: make(map[int64]*apiKeyConcurrencyCounter)}
}

func (l *apiKeyConcurrencyLimiter) acquire(apiKeyID int64, limit int) (func(), int64, bool) {
	if l == nil || apiKeyID <= 0 || limit <= 0 {
		return nil, 0, true
	}
	counter := l.counter(apiKeyID)
	limit64 := int64(limit)
	for {
		current := counter.inflight.Load()
		if current >= limit64 {
			return nil, current, false
		}
		if counter.inflight.CompareAndSwap(current, current+1) {
			released := atomic.Bool{}
			return func() {
				if released.CompareAndSwap(false, true) {
					l.release(counter)
				}
			}, current + 1, true
		}
	}
}

func (l *apiKeyConcurrencyLimiter) counter(apiKeyID int64) *apiKeyConcurrencyCounter {
	l.mu.Lock()
	defer l.mu.Unlock()
	counter := l.counters[apiKeyID]
	if counter == nil {
		counter = &apiKeyConcurrencyCounter{}
		l.counters[apiKeyID] = counter
	}
	return counter
}

func (l *apiKeyConcurrencyLimiter) release(counter *apiKeyConcurrencyCounter) {
	if l == nil || counter == nil {
		return
	}
	if current := counter.inflight.Add(-1); current < 0 {
		counter.inflight.Store(0)
	}
}

func (h *Handler) apiKeyConcurrencyLimiter() *apiKeyConcurrencyLimiter {
	if h == nil {
		return nil
	}
	h.apiKeyGateMu.Lock()
	defer h.apiKeyGateMu.Unlock()
	if h.apiKeyGate == nil {
		h.apiKeyGate = newAPIKeyConcurrencyLimiter()
	}
	return h.apiKeyGate
}

func (h *Handler) AcquireAPIKeyConcurrency(c *gin.Context) (func(), bool) {
	return h.acquireAPIKeyConcurrency(c)
}

func (h *Handler) acquireAPIKeyConcurrency(c *gin.Context) (func(), bool) {
	if c != nil {
		if inherited, exists := c.Get(contextAPIKeyConcurrencyInherited); exists {
			if value, ok := inherited.(bool); ok && value {
				return nil, true
			}
		}
	}
	row := apiKeyRowFromContext(c)
	if row == nil || row.ID <= 0 {
		return nil, true
	}
	limiter := h.apiKeyConcurrencyLimiter()
	release, current, ok := limiter.acquireTracked(row.ID, row.Limits.MaxConcurrency)
	if ok {
		return release, true
	}
	msg := fmt.Sprintf("API key concurrency limit exceeded: %d inflight requests (max %d)", current, row.Limits.MaxConcurrency)
	api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeRateLimitReached, msg, api.ErrorTypeRateLimit), http.StatusTooManyRequests)
	return nil, false
}

func (h *Handler) acquireAPIKeyConcurrencyForWebSocket(c *gin.Context) (func(), *api.APIError, bool) {
	row := apiKeyRowFromContext(c)
	if row == nil || row.ID <= 0 {
		return nil, nil, true
	}
	limiter := h.apiKeyConcurrencyLimiter()
	release, current, ok := limiter.acquireTracked(row.ID, row.Limits.MaxConcurrency)
	if ok {
		return release, nil, true
	}
	msg := fmt.Sprintf("API key concurrency limit exceeded: %d inflight requests (max %d)", current, row.Limits.MaxConcurrency)
	return nil, api.NewAPIError(api.ErrCodeRateLimitReached, msg, api.ErrorTypeRateLimit), false
}

// acquireTracked 为不限并发的密钥保留计数，不改变限流器的旁路约定。
func (l *apiKeyConcurrencyLimiter) acquireTracked(id int64, limit int) (func(), int64, bool) {
	if limit <= 0 {
		limit = math.MaxInt
	}
	return l.acquire(id, limit)
}

// APIKeyConcurrency 只读取指定密钥的并发数，不为闲置密钥创建计数器。
func (h *Handler) APIKeyConcurrency(apiKeyID int64) int64 {
	if h == nil {
		return 0
	}
	limiter := h.apiKeyConcurrencyLimiter()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if counter := limiter.counters[apiKeyID]; counter != nil {
		return counter.inflight.Load()
	}
	return 0
}

// APIKeyConcurrencySnapshot 返回当前进程中各密钥的并发快照。
func (h *Handler) APIKeyConcurrencySnapshot() map[int64]int64 {
	result := make(map[int64]int64)
	if h == nil {
		return result
	}
	limiter := h.apiKeyConcurrencyLimiter()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	for id, counter := range limiter.counters {
		result[id] = counter.inflight.Load()
	}
	return result
}
