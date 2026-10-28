package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/codex2api/cache"
)

const (
	responseCacheBackendWriteSlots   = 16
	responseCacheBackendWriteBytes   = int64(64 << 20)
	responseCacheBackendMaxWaiters   = 64
	responseCacheBackendWaitTimeout  = 5 * time.Second
	responseCacheBackendWriteTimeout = 2 * time.Second
	responseCacheBackendSyncTimeout  = 500 * time.Millisecond
)

var (
	errResponseCacheBackendQueueFull   = errors.New("response context backend write queue is full")
	errResponseCacheBackendWriteFailed = errors.New("response context backend write did not complete")
)

type responseCacheWriteWaiter struct {
	bytes   int64
	ready   chan struct{}
	granted bool
}

// Both asynchronous and synchronous writes acquire the same limits before
// copying or encoding. Bytes count logical item bodies, not wire encoding,
// allocator overhead or request bodies still held by waiting callers.
// A larger item set can run exclusively, preserving existing shared-backend
// entries: the logical bound is max(maxBytes, largest active entry).
type responseCacheBackendWriterState struct {
	mu              sync.Mutex
	draining        bool
	writes          int // includes failure reporting after an admission wait ends
	active          int
	bytes           int64
	waitingBytes    int64
	highWaterActive int
	highWaterBytes  int64
	queueRejections uint64
	waitTimeouts    uint64
	maxActive       int
	maxBytes        int64
	maxWaiters      int
	waiters         []*responseCacheWriteWaiter
	changed         chan struct{}
}

func newResponseCacheBackendWriter(maxActive int, maxBytes int64, maxWaiters int) *responseCacheBackendWriterState {
	return &responseCacheBackendWriterState{maxActive: maxActive, maxBytes: maxBytes, maxWaiters: maxWaiters, changed: make(chan struct{})}
}

var responseCacheBackendWriter = newResponseCacheBackendWriter(responseCacheBackendWriteSlots, responseCacheBackendWriteBytes, responseCacheBackendMaxWaiters)

type ResponseCacheWriterSnapshot struct {
	ActiveWrites          int    `json:"active_writes"`
	WaitingWrites         int    `json:"waiting_writes"`
	InflightLogicalBytes  int64  `json:"inflight_logical_bytes"`
	WaitingLogicalBytes   int64  `json:"waiting_logical_bytes"`
	MaxActiveWrites       int    `json:"max_active_writes"`
	MaxWaitingWrites      int    `json:"max_waiting_writes"`
	MaxLogicalBytes       int64  `json:"max_logical_bytes"`
	HighWaterActiveWrites int    `json:"high_water_active_writes"`
	HighWaterLogicalBytes int64  `json:"high_water_logical_bytes"`
	QueueRejections       uint64 `json:"queue_rejections"`
	WaitTimeouts          uint64 `json:"wait_timeouts"`
	OversizeActive        bool   `json:"oversize_active"`
}

func GetResponseCacheWriterSnapshot() ResponseCacheWriterSnapshot {
	w := responseCacheBackendWriter
	w.mu.Lock()
	defer w.mu.Unlock()
	return ResponseCacheWriterSnapshot{
		ActiveWrites: w.active, WaitingWrites: len(w.waiters),
		InflightLogicalBytes: w.bytes, WaitingLogicalBytes: w.waitingBytes,
		MaxActiveWrites: w.maxActive, MaxWaitingWrites: w.maxWaiters, MaxLogicalBytes: w.maxBytes,
		HighWaterActiveWrites: w.highWaterActive, HighWaterLogicalBytes: w.highWaterBytes,
		QueueRejections: w.queueRejections, WaitTimeouts: w.waitTimeouts,
		OversizeActive: w.active > 0 && w.bytes > w.maxBytes,
	}
}

func (w *responseCacheBackendWriterState) start() {
	w.mu.Lock()
	w.writes++
	w.mu.Unlock()
}
func (w *responseCacheBackendWriterState) finish() {
	w.mu.Lock()
	w.writes--
	w.notifyLocked()
	w.mu.Unlock()
}
func (w *responseCacheBackendWriterState) recordAdmissionLocked(bytes int64) {
	w.active++
	w.bytes += bytes
	if w.active > w.highWaterActive {
		w.highWaterActive = w.active
	}
	if w.bytes > w.highWaterBytes {
		w.highWaterBytes = w.bytes
	}
}

func (w *responseCacheBackendWriterState) notifyLocked() {
	close(w.changed)
	w.changed = make(chan struct{})
}
func (w *responseCacheBackendWriterState) fitsLocked(bytes int64) bool {
	if w.active >= w.maxActive {
		return false
	}
	if bytes > w.maxBytes {
		return w.active == 0
	}
	return w.bytes <= w.maxBytes-bytes
}
func (w *responseCacheBackendWriterState) scheduleLocked() {
	for len(w.waiters) > 0 && w.fitsLocked(w.waiters[0].bytes) {
		waiter := w.waiters[0]
		w.waiters[0] = nil
		w.waiters = w.waiters[1:]
		w.waitingBytes -= waiter.bytes
		w.recordAdmissionLocked(waiter.bytes)
		waiter.granted = true
		close(waiter.ready)
	}
	if len(w.waiters) == 0 {
		w.waiters = nil
	}
	w.notifyLocked()
}

// Backpressured callers stay synchronous; the queue owns no payload copies
// and creates no goroutines. FIFO prevents larger entries from starving.
func (w *responseCacheBackendWriterState) acquire(ctx context.Context, bytes int64) (bool, error) {
	w.mu.Lock()
	if err := ctx.Err(); err != nil {
		w.mu.Unlock()
		return false, err
	}
	if len(w.waiters) == 0 && w.fitsLocked(bytes) {
		w.recordAdmissionLocked(bytes)
		async := !w.draining
		w.notifyLocked()
		w.mu.Unlock()
		return async, nil
	}
	if len(w.waiters) >= w.maxWaiters {
		w.queueRejections++
		w.mu.Unlock()
		return false, errResponseCacheBackendQueueFull
	}
	waiter := &responseCacheWriteWaiter{bytes: bytes, ready: make(chan struct{})}
	w.waiters = append(w.waiters, waiter)
	w.waitingBytes += bytes
	w.notifyLocked()
	w.mu.Unlock()
	select {
	case <-waiter.ready:
		return false, nil
	case <-ctx.Done():
		w.mu.Lock()
		if waiter.granted {
			// A simultaneous release transferred ownership; finish that admitted
			// operation under its separate I/O deadline.
			w.mu.Unlock()
			return false, nil
		}
		for i, pending := range w.waiters {
			if pending == waiter {
				copy(w.waiters[i:], w.waiters[i+1:])
				w.waiters[len(w.waiters)-1] = nil
				w.waiters = w.waiters[:len(w.waiters)-1]
				w.waitingBytes -= bytes
				w.waitTimeouts++
				break
			}
		}
		w.scheduleLocked()
		w.mu.Unlock()
		return false, ctx.Err()
	}
}
func (w *responseCacheBackendWriterState) release(bytes int64) {
	w.mu.Lock()
	w.active--
	w.bytes -= bytes
	w.scheduleLocked()
	w.mu.Unlock()
}
func (w *responseCacheBackendWriterState) drain(ctx context.Context, shutdown bool) bool {
	w.mu.Lock()
	if shutdown {
		w.draining = true
	}
	for w.writes > 0 || w.active > 0 || len(w.waiters) > 0 {
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
		w.mu.Lock()
	}
	w.mu.Unlock()
	return true
}

// This optional capability promises not to mutate item bodies or the sequence.
// Legacy/custom backends continue to receive a private deep copy.
type readOnlyResponseContextWriter interface {
	SetResponseContextReadOnly(context.Context, string, []json.RawMessage, time.Duration) error
}

func writeResponseContextBackend(runtimeCache cache.TokenCache, responseID, storeKey string, items []json.RawMessage) error {
	return writeResponseContextBackendWithOwnership(runtimeCache, responseID, storeKey, items, 0)
}
func writeResponseContextBackendWithOwnership(runtimeCache cache.TokenCache, responseID, storeKey string, items []json.RawMessage, serial uint64) error {
	responseCacheBackendWriter.start()
	bytes := responseContextLogicalBytes(items)
	waitCtx, cancel := context.WithTimeout(context.Background(), responseCacheBackendWaitTimeout)
	async, err := responseCacheBackendWriter.acquire(waitCtx, bytes)
	cancel()
	if err != nil {
		recordResponseCacheBackendWriteResult(storeKey, responseID, serial, err)
		responseCacheBackendWriter.finish()
		return err
	}
	// Cache-owned bodies survive eviction and cannot be changed by callers.
	write := runtimeCache.SetResponseContext
	var payload []json.RawMessage
	if readOnly, ok := runtimeCache.(readOnlyResponseContextWriter); ok && serial != 0 {
		respCache.mu.RLock()
		if entry := respCache.store[storeKey]; entry != nil && entry.serial == serial {
			payload = append([]json.RawMessage(nil), entry.items...)
			write = readOnly.SetResponseContextReadOnly
		}
		respCache.mu.RUnlock()
	}
	if payload == nil {
		payload = cloneResponseContextItems(items)
	}
	run := func() error {
		defer responseCacheBackendWriter.finish()
		defer responseCacheBackendWriter.release(bytes)
		timeout := responseCacheBackendWriteTimeout
		responseCacheBackendWriter.mu.Lock()
		draining := responseCacheBackendWriter.draining
		responseCacheBackendWriter.mu.Unlock()
		if draining {
			timeout = responseCacheBackendSyncTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := write(ctx, storeKey, payload, responseCacheTTL)
		recordResponseCacheBackendWriteResult(storeKey, responseID, serial, err)
		return err
	}
	if async {
		go func() { _ = run() }()
		return nil
	}
	return run()
}

func recordResponseCacheBackendWriteResult(storeKey, responseID string, serial uint64, err error) {
	respCache.mu.Lock()
	if err != nil {
		respCache.stats.BackendWriteFailures++
	}
	current := serial == 0
	if entry := respCache.store[storeKey]; entry != nil {
		current = entry.serial == serial
	} else if marker := respCache.markers[storeKey]; marker != nil {
		current = marker.serial == serial
	}
	if current {
		if err != nil {
			respCache.setWriteMarkerLocked(storeKey, responseCacheLookupBackendError, time.Now().Add(respCache.config.ttl), serial)
		} else if marker := respCache.markers[storeKey]; marker != nil && (marker.kind == responseCacheLookupBackendError || marker.kind == responseCacheLookupBackendPending) {
			respCache.removeMarkerLocked(storeKey)
		}
	}
	respCache.mu.Unlock()
	if err != nil {
		log.Printf("写入 response context 后端失败: response_id=%s err=%v", responseID, err)
	}
}
func drainResponseCacheBackendWrites() { responseCacheBackendWriter.drain(context.Background(), false) }

// Includes waiting and synchronous writers. Newly accepted shutdown writes
// stay synchronous and obey the same budgets; no wait goroutine is leaked.
func DrainResponseCacheBackendWrites(timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return responseCacheBackendWriter.drain(ctx, true)
}
