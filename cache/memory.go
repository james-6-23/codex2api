package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

type memoryTokenEntry struct {
	token     string
	expiresAt time.Time
}

type memorySessionAffinityEntry struct {
	binding   SessionAffinityBinding
	expiresAt time.Time
}

type memoryResponseContextEntry struct {
	items     []json.RawMessage
	expiresAt time.Time
}

type memoryRuntimeEntry struct {
	value     json.RawMessage
	expiresAt time.Time
}

type memoryLeaseEntry struct {
	owner     string
	expiresAt time.Time
}

// MemoryTokenCache 为单机轻量部署提供进程内 token 缓存和刷新锁。
// 重启后缓存丢失属于预期行为。
type MemoryTokenCache struct {
	mu        sync.RWMutex
	tokens    map[int64]memoryTokenEntry
	locks     map[int64]time.Time
	leases    map[string]memoryLeaseEntry
	sessions  map[string]memorySessionAffinityEntry
	responses map[string]memoryResponseContextEntry
	runtime   map[string]memoryRuntimeEntry
	poolSize  int
}

var _ RuntimeOwnerStore = (*MemoryTokenCache)(nil)

// NewMemory 创建内存缓存实现。
func NewMemory(poolSize int) TokenCache {
	if poolSize <= 0 {
		poolSize = 1
	}
	tc := &MemoryTokenCache{
		tokens:    make(map[int64]memoryTokenEntry),
		locks:     make(map[int64]time.Time),
		leases:    make(map[string]memoryLeaseEntry),
		sessions:  make(map[string]memorySessionAffinityEntry),
		responses: make(map[string]memoryResponseContextEntry),
		runtime:   make(map[string]memoryRuntimeEntry),
		poolSize:  poolSize,
	}
	// 启动后台定时清理过期 token 和过期锁，防止已删除账号的条目永驻内存
	go tc.cleanupLoop()
	return tc
}

// cleanupLoop 每 60 秒清理一次过期 token 和过期锁
func (tc *MemoryTokenCache) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		tc.mu.Lock()
		for id, entry := range tc.tokens {
			if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
				delete(tc.tokens, id)
			}
		}
		for id, until := range tc.locks {
			if now.After(until) {
				delete(tc.locks, id)
			}
		}
		for key, entry := range tc.leases {
			if now.After(entry.expiresAt) {
				delete(tc.leases, key)
			}
		}
		for key, entry := range tc.sessions {
			if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
				delete(tc.sessions, key)
			}
		}
		for key, entry := range tc.responses {
			if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
				delete(tc.responses, key)
			}
		}
		for key, entry := range tc.runtime {
			if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
				delete(tc.runtime, key)
			}
		}
		tc.mu.Unlock()
	}
}

func (tc *MemoryTokenCache) Driver() string {
	return "memory"
}

func (tc *MemoryTokenCache) Label() string {
	return "Memory"
}

func (tc *MemoryTokenCache) Close() error {
	return nil
}

func (tc *MemoryTokenCache) Ping(ctx context.Context) error {
	return nil
}

func (tc *MemoryTokenCache) Stats() PoolStats {
	return PoolStats{
		TotalConns: 1,
		IdleConns:  1,
		StaleConns: 0,
	}
}

func (tc *MemoryTokenCache) PoolSize() int {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return tc.poolSize
}

func (tc *MemoryTokenCache) SetPoolSize(n int) {
	if n <= 0 {
		n = 1
	}
	tc.mu.Lock()
	tc.poolSize = n
	tc.mu.Unlock()
}

func (tc *MemoryTokenCache) GetAccessToken(ctx context.Context, accountID int64) (string, error) {
	tc.mu.RLock()
	entry, ok := tc.tokens[accountID]
	tc.mu.RUnlock()
	if !ok {
		return "", nil
	}
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		// 同步删除过期条目，避免删除刚刷新的 token
		tc.mu.Lock()
		current, ok := tc.tokens[accountID]
		if ok && current == entry && !current.expiresAt.IsZero() && time.Now().After(current.expiresAt) {
			delete(tc.tokens, accountID)
		}
		tc.mu.Unlock()
		return "", nil
	}
	return entry.token, nil
}

func (tc *MemoryTokenCache) SetAccessToken(ctx context.Context, accountID int64, token string, ttl time.Duration) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	} else if ttl < 0 {
		// 负数 TTL 表示已经过期，直接设置为过去的时间
		expiresAt = time.Now().Add(ttl)
	}
	tc.tokens[accountID] = memoryTokenEntry{
		token:     token,
		expiresAt: expiresAt,
	}
	return nil
}

func (tc *MemoryTokenCache) DeleteAccessToken(ctx context.Context, accountID int64) error {
	tc.mu.Lock()
	delete(tc.tokens, accountID)
	tc.mu.Unlock()
	return nil
}

func (tc *MemoryTokenCache) AcquireRefreshLock(ctx context.Context, accountID int64, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	tc.mu.Lock()
	defer tc.mu.Unlock()

	if until, ok := tc.locks[accountID]; ok && time.Now().Before(until) {
		return false, nil
	}
	tc.locks[accountID] = time.Now().Add(ttl)
	return true, nil
}

func (tc *MemoryTokenCache) ReleaseRefreshLock(ctx context.Context, accountID int64) error {
	tc.mu.Lock()
	delete(tc.locks, accountID)
	tc.mu.Unlock()
	return nil
}

func memoryLeaseKey(namespace, key string) string {
	return strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(key)
}

func (tc *MemoryTokenCache) AcquireLease(ctx context.Context, namespace, key, owner string, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return false, fmt.Errorf("lease owner is empty")
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	leaseKey := memoryLeaseKey(namespace, key)
	now := time.Now()
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if entry, ok := tc.leases[leaseKey]; ok && now.Before(entry.expiresAt) {
		return false, nil
	}
	tc.leases[leaseKey] = memoryLeaseEntry{owner: owner, expiresAt: now.Add(ttl)}
	return true, nil
}

func (tc *MemoryTokenCache) ReleaseLease(ctx context.Context, namespace, key, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	leaseKey := memoryLeaseKey(namespace, key)
	tc.mu.Lock()
	if entry, ok := tc.leases[leaseKey]; ok && entry.owner == strings.TrimSpace(owner) {
		delete(tc.leases, leaseKey)
	}
	tc.mu.Unlock()
	return nil
}

func (tc *MemoryTokenCache) WaitForRefreshComplete(ctx context.Context, accountID int64, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	deadline := time.Now().Add(timeout)
	pollTimer := time.NewTimer(200 * time.Millisecond)
	defer pollTimer.Stop()

	for time.Now().Before(deadline) {
		tc.mu.Lock()
		lockUntil, locked := tc.locks[accountID]
		if locked && time.Now().After(lockUntil) {
			delete(tc.locks, accountID)
			locked = false
		}
		entry, hasToken := tc.tokens[accountID]
		if hasToken && !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
			delete(tc.tokens, accountID)
			hasToken = false
			entry = memoryTokenEntry{}
		}
		tc.mu.Unlock()

		if !locked && hasToken && entry.token != "" {
			return entry.token, nil
		}

		pollTimer.Reset(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-pollTimer.C:
		}
	}

	return "", fmt.Errorf("等待刷新超时")
}

func (tc *MemoryTokenCache) SetSessionAffinity(ctx context.Context, key string, binding SessionAffinityBinding, ttl time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" || binding.AccountID == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	tc.mu.Lock()
	tc.sessions[key] = memorySessionAffinityEntry{
		binding:   binding,
		expiresAt: time.Now().Add(ttl),
	}
	tc.mu.Unlock()
	return nil
}

func (tc *MemoryTokenCache) GetSessionAffinity(ctx context.Context, key string) (SessionAffinityBinding, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return SessionAffinityBinding{}, false, nil
	}
	tc.mu.RLock()
	entry, ok := tc.sessions[key]
	tc.mu.RUnlock()
	if !ok {
		return SessionAffinityBinding{}, false, nil
	}
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		tc.mu.Lock()
		if current, exists := tc.sessions[key]; exists && current.expiresAt.Equal(entry.expiresAt) {
			delete(tc.sessions, key)
		}
		tc.mu.Unlock()
		return SessionAffinityBinding{}, false, nil
	}
	return entry.binding, true, nil
}

func (tc *MemoryTokenCache) DeleteSessionAffinity(ctx context.Context, key string, accountID int64) error {
	key = strings.TrimSpace(key)
	if key == "" || accountID == 0 {
		return nil
	}
	tc.mu.Lock()
	if entry, ok := tc.sessions[key]; ok && entry.binding.AccountID == accountID {
		delete(tc.sessions, key)
	}
	tc.mu.Unlock()
	return nil
}

func (tc *MemoryTokenCache) SetResponseContext(ctx context.Context, responseID string, items []json.RawMessage, ttl time.Duration) error {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	itemsCopy := make([]json.RawMessage, len(items))
	for i, item := range items {
		itemsCopy[i] = append(json.RawMessage(nil), item...)
	}
	tc.mu.Lock()
	tc.responses[responseID] = memoryResponseContextEntry{
		items:     itemsCopy,
		expiresAt: time.Now().Add(ttl),
	}
	tc.mu.Unlock()
	return nil
}

func (tc *MemoryTokenCache) GetResponseContext(ctx context.Context, responseID string) ([]json.RawMessage, error) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil, nil
	}
	tc.mu.RLock()
	entry, ok := tc.responses[responseID]
	tc.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		tc.mu.Lock()
		if current, exists := tc.responses[responseID]; exists && current.expiresAt.Equal(entry.expiresAt) {
			delete(tc.responses, responseID)
		}
		tc.mu.Unlock()
		return nil, nil
	}
	itemsCopy := make([]json.RawMessage, len(entry.items))
	for i, item := range entry.items {
		itemsCopy[i] = append(json.RawMessage(nil), item...)
	}
	return itemsCopy, nil
}

func runtimeMapKey(namespace, key string) string {
	namespace = strings.TrimSpace(namespace)
	key = strings.TrimSpace(key)
	if namespace == "" || key == "" {
		return ""
	}
	return namespace + "\x00" + key
}

func (tc *MemoryTokenCache) SetRuntime(ctx context.Context, namespace string, key string, value json.RawMessage, ttl time.Duration) error {
	mapKey := runtimeMapKey(namespace, key)
	if mapKey == "" || len(value) == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	valueCopy := append(json.RawMessage(nil), value...)
	tc.mu.Lock()
	tc.runtime[mapKey] = memoryRuntimeEntry{
		value:     valueCopy,
		expiresAt: time.Now().Add(ttl),
	}
	tc.mu.Unlock()
	return nil
}

func (tc *MemoryTokenCache) GetRuntime(ctx context.Context, namespace string, key string) (json.RawMessage, bool, error) {
	mapKey := runtimeMapKey(namespace, key)
	if mapKey == "" {
		return nil, false, nil
	}
	tc.mu.RLock()
	entry, ok := tc.runtime[mapKey]
	tc.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		tc.mu.Lock()
		if current, exists := tc.runtime[mapKey]; exists && current.expiresAt.Equal(entry.expiresAt) {
			delete(tc.runtime, mapKey)
		}
		tc.mu.Unlock()
		return nil, false, nil
	}
	return append(json.RawMessage(nil), entry.value...), true, nil
}

func (tc *MemoryTokenCache) DeleteRuntime(ctx context.Context, namespace string, key string) error {
	mapKey := runtimeMapKey(namespace, key)
	if mapKey == "" {
		return nil
	}
	tc.mu.Lock()
	delete(tc.runtime, mapKey)
	tc.mu.Unlock()
	return nil
}

func (tc *MemoryTokenCache) ClaimRuntimeOwner(ctx context.Context, namespace, key string, owner []byte, ttl time.Duration) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mapKey := runtimeMapKey(namespace, key)
	if mapKey == "" || len(owner) == 0 {
		return nil, nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	now := time.Now()
	tc.mu.Lock()
	defer tc.mu.Unlock()
	var previous []byte
	if entry, ok := tc.runtime[mapKey]; ok && (entry.expiresAt.IsZero() || now.Before(entry.expiresAt)) {
		previous = append([]byte(nil), entry.value...)
	}
	tc.runtime[mapKey] = memoryRuntimeEntry{
		value:     append(json.RawMessage(nil), owner...),
		expiresAt: now.Add(ttl),
	}
	return previous, nil
}

func (tc *MemoryTokenCache) CompareAndRefreshRuntimeOwner(ctx context.Context, namespace, key string, expected []byte, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	mapKey := runtimeMapKey(namespace, key)
	if mapKey == "" || len(expected) == 0 {
		return false, nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	now := time.Now()
	tc.mu.Lock()
	defer tc.mu.Unlock()
	entry, ok := tc.runtime[mapKey]
	if !ok || (!entry.expiresAt.IsZero() && !now.Before(entry.expiresAt)) || !bytes.Equal(entry.value, expected) {
		return false, nil
	}
	entry.expiresAt = now.Add(ttl)
	tc.runtime[mapKey] = entry
	return true, nil
}

func (tc *MemoryTokenCache) CompareAndDeleteRuntimeOwner(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	mapKey := runtimeMapKey(namespace, key)
	if mapKey == "" || len(expected) == 0 {
		return false, nil
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	entry, ok := tc.runtime[mapKey]
	if !ok || !bytes.Equal(entry.value, expected) {
		return false, nil
	}
	delete(tc.runtime, mapKey)
	return true, nil
}

// IncrRuntimeCounters 在单锁内累加计数器。计数器以 JSON 对象存在 runtime 表里，
// 语义与 Redis 的 HINCRBYFLOAT 一致，但只对本进程可见。
func (tc *MemoryTokenCache) IncrRuntimeCounters(ctx context.Context, namespace string, key string, deltas map[string]float64, ttl time.Duration) error {
	mapKey := runtimeMapKey(namespace, key)
	if mapKey == "" || len(deltas) == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	counters := make(map[string]float64)
	if entry, ok := tc.runtime[mapKey]; ok {
		if entry.expiresAt.IsZero() || time.Now().Before(entry.expiresAt) {
			_ = json.Unmarshal(entry.value, &counters)
		}
	}
	for field, delta := range deltas {
		if delta == 0 {
			continue
		}
		counters[field] += delta
	}
	encoded, err := json.Marshal(counters)
	if err != nil {
		return err
	}
	tc.runtime[mapKey] = memoryRuntimeEntry{
		value:     encoded,
		expiresAt: time.Now().Add(ttl),
	}
	return nil
}

func (tc *MemoryTokenCache) GetRuntimeCounters(ctx context.Context, namespace string, key string) (map[string]float64, error) {
	raw, ok, err := tc.GetRuntime(ctx, namespace, key)
	if err != nil || !ok || len(raw) == 0 {
		return nil, err
	}
	counters := make(map[string]float64)
	if err := json.Unmarshal(raw, &counters); err != nil {
		return nil, nil
	}
	return counters, nil
}

// SharedAcrossInstances 报告进程内内存缓存不跨实例共享。
func (tc *MemoryTokenCache) SharedAcrossInstances() bool { return false }
