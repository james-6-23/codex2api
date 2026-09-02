package proxy

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 响应上下文缓存 ====================
// 用于解决 previous_response_id 场景下 tool calling 上下文丢失的问题。
// 代理层设置 store=false 并删除 previous_response_id，导致上游无法恢复历史 function_call。
// 本模块在本地缓存每次响应的累积对话上下文，当下一个请求带 previous_response_id 时，
// 自动将历史 items 注入回 input，使上游无需依赖服务端存储即可匹配 call_id。
//
// 隔离：缓存键 = owner(下游 API Key ID) + response_id。没有 owner 维度时，任何请求
// 只要带上（猜到/复用了）别人的 response_id 就能把别人的对话历史注入自己的 input，
// 造成跨用户上下文泄露。owner 不匹配一律按缓存未命中处理。

const (
	responseCacheTTL        = 10 * time.Minute
	responseCacheMaxBytes   = 64 << 20
	responseCacheMaxEntry   = 8 << 20
	responseCacheMaxItems   = 2000 // 缓存条目上限，防止内存膨胀
	responseCacheMaxPerItem = 200  // 单条缓存最大 raw items 数
	responseCacheMaxMarkers = 2000
	responseCleanupInterval = 2 * time.Minute

	// on_demand 写入策略下，owner 发过 previous_response_id 后保持写入资格的
	// 滑动窗口。续链客户端逐轮续链（间隔秒级），1 小时足以覆盖会话间隙；
	// 窗口过后闸门重新关闭，该 owner 的下一次续链会先未命中一次（与缓存
	// TTL 过期后的行为一致），随后恢复写入。
	responseCacheChainOwnerTTL  = time.Hour
	responseCacheMaxChainOwners = 4096
)

// responseCacheOwner 生成响应上下文缓存的归属命名空间。
// 以下游 API Key ID 为主；无鉴权上下文(0)时退化到共享匿名空间（仅测试/内嵌场景）。
func responseCacheOwner(apiKeyID int64) string {
	if apiKeyID > 0 {
		return fmt.Sprintf("key:%d", apiKeyID)
	}
	return "anon"
}

// responseCacheStoreKey 组合 owner 与 response_id 作为缓存键（本地 map 与 Redis 共用）。
func responseCacheStoreKey(owner, responseID string) string {
	if owner == "" {
		owner = "anon"
	}
	return owner + "|" + responseID
}

type responseCacheEntry struct {
	key       string
	items     []json.RawMessage
	bytes     int64
	expiresAt time.Time
	element   *list.Element
}

type responseCacheConfig struct {
	maxBytes            int64
	maxEntryBytes       int64
	reconstructMaxBytes int64
	maxEntries          int
	ttl                 time.Duration
	maxItems            int
	writePolicy         string
}

func defaultResponseCacheConfig() responseCacheConfig {
	return responseCacheConfig{
		maxBytes:            responseCacheMaxBytes,
		maxEntryBytes:       responseCacheMaxEntry,
		reconstructMaxBytes: responseCacheMaxBytes,
		maxEntries:          responseCacheMaxItems,
		ttl:                 responseCacheTTL,
		maxItems:            responseCacheMaxPerItem,
		writePolicy:         database.DefaultResponseCacheWritePolicy,
	}
}

// ResponseCacheStats is a point-in-time snapshot of the bounded local cache.
// Bytes are logical retained JSON payload bytes; they are not an RSS estimate.
// Hits/Misses 是端到端口径：本次查询最终拿到上下文（本地或共享后端）计 Hit，
// 否则计 Miss（含负标记、后端错误/损坏、重建超限）。Local*/Remote* 是分层口径。
// 不变量：Hits = LocalHits + RemoteHits；Hits + Misses = LocalHits + LocalMisses。
type ResponseCacheStats struct {
	Entries                int
	Bytes                  int64
	HighWaterBytes         int64
	LargestSeenEntryBytes  int64
	Hits                   uint64
	Misses                 uint64
	LocalHits              uint64
	LocalMisses            uint64
	RemoteHits             uint64
	RemoteMisses           uint64
	Expirations            uint64
	CountEvictions         uint64
	ByteEvictions          uint64
	OversizeSkips          uint64
	OversizeBypasses       uint64
	OversizeRejections     uint64
	KnownUnavailableErrors uint64
	// SkippedWrites 是 on_demand 写入策略下因 owner 未续链而跳过的写入次数
	// （本地与 Redis 一并跳过）；ChainOwners 是当前处于写入资格窗口内的 owner 数。
	SkippedWrites uint64
	ChainOwners   int
}

type responseCacheState struct {
	mu           sync.RWMutex
	store        map[string]*responseCacheEntry
	lru          *list.List
	markers      map[string]*responseCacheMarker
	markerLRU    *list.List
	chainOwners  map[string]*responseCacheChainOwner
	chainLRU     *list.List
	config       responseCacheConfig
	generation   int64
	stats        ResponseCacheStats
	runtimeCache cache.TokenCache
	lastSyncAt   time.Time
	lastSyncErr  string
}

// responseCacheChainOwner 记录某 owner 最近一次发起 previous_response_id 续链
// 的到期时间，是 on_demand 写入策略的准入依据。
type responseCacheChainOwner struct {
	owner     string
	expiresAt time.Time
	element   *list.Element
}

type responseCacheLookupKind uint8

const (
	responseCacheLookupMiss responseCacheLookupKind = iota
	responseCacheLookupHit
	responseCacheLookupExpired
	responseCacheLookupKnownEvicted
	responseCacheLookupKnownOversize
	responseCacheLookupReconstructionTooLarge
	responseCacheLookupBackendCorrupt
	responseCacheLookupBackendError
)

type responseCacheLookupSource uint8

const (
	responseCacheSourceNone responseCacheLookupSource = iota
	responseCacheSourceLocal
	responseCacheSourceBackend
)

type responseCacheLookupResult struct {
	Items    []json.RawMessage
	Kind     responseCacheLookupKind
	Source   responseCacheLookupSource
	Promoted bool
	Err      error
	// 记账辅助位：由 lookupResponseCacheResult 填写，getResponseCacheResult
	// 在单一临界区内据此更新全部计数器，保证快照不变量任意瞬间成立。
	remoteMiss     bool
	oversizeBypass bool
}

type responseCacheMarker struct {
	key       string
	kind      responseCacheLookupKind
	expiresAt time.Time
	element   *list.Element
}

var respCache responseCacheState

func init() {
	respCache.store = make(map[string]*responseCacheEntry)
	respCache.lru = list.New()
	respCache.markers = make(map[string]*responseCacheMarker)
	respCache.markerLRU = list.New()
	respCache.chainOwners = make(map[string]*responseCacheChainOwner)
	respCache.chainLRU = list.New()
	respCache.config = defaultResponseCacheConfig()
	go respCacheCleanupLoop()
}

func SetResponseContextCache(tc cache.TokenCache) {
	respCache.mu.Lock()
	respCache.runtimeCache = nil
	if tc != nil && tc.SharedAcrossInstances() {
		respCache.runtimeCache = tc
	}
	respCache.mu.Unlock()
}

// GetResponseCacheStats returns a thread-safe local-cache snapshot.
func GetResponseCacheStats() ResponseCacheStats {
	respCache.mu.RLock()
	stats := respCache.stats
	respCache.mu.RUnlock()
	return stats
}

// resetResponseCacheStateForTest replaces all local state with a deterministic
// test configuration. It deliberately remains package-private.
func resetResponseCacheStateForTest(config responseCacheConfig) {
	respCache.mu.Lock()
	respCache.store = make(map[string]*responseCacheEntry)
	respCache.lru = list.New()
	respCache.markers = make(map[string]*responseCacheMarker)
	respCache.markerLRU = list.New()
	respCache.chainOwners = make(map[string]*responseCacheChainOwner)
	respCache.chainLRU = list.New()
	respCache.config = config
	respCache.generation = 0
	respCache.stats = ResponseCacheStats{}
	respCache.runtimeCache = nil
	respCache.lastSyncAt = time.Time{}
	respCache.lastSyncErr = ""
	respCache.mu.Unlock()
}

// configureResponseCacheForTest exercises the same immediate runtime-shrink
// path future configuration wiring will use, without exposing configuration.
func configureResponseCacheForTest(config responseCacheConfig) {
	respCache.mu.Lock()
	respCache.config = config
	respCache.enforceConfigLocked()
	respCache.mu.Unlock()
}

// markResponseCacheChainOwnerLocked 把 owner 标记为"近期续链者"，续期滑动窗口。
// 容量满时按 LRU 逐出最久未续链的 owner。
func markResponseCacheChainOwnerLocked(owner string) {
	if owner == "" {
		owner = "anon"
	}
	expiresAt := time.Now().Add(responseCacheChainOwnerTTL)
	if existing := respCache.chainOwners[owner]; existing != nil {
		existing.expiresAt = expiresAt
		respCache.chainLRU.MoveToFront(existing.element)
		return
	}
	record := &responseCacheChainOwner{owner: owner, expiresAt: expiresAt}
	record.element = respCache.chainLRU.PushFront(record)
	respCache.chainOwners[owner] = record
	for len(respCache.chainOwners) > responseCacheMaxChainOwners {
		element := respCache.chainLRU.Back()
		if element == nil {
			break
		}
		oldest, _ := element.Value.(*responseCacheChainOwner)
		removeResponseCacheChainOwnerLocked(oldest.owner)
	}
	respCache.stats.ChainOwners = len(respCache.chainOwners)
}

func removeResponseCacheChainOwnerLocked(owner string) {
	record := respCache.chainOwners[owner]
	if record == nil {
		return
	}
	delete(respCache.chainOwners, owner)
	if record.element != nil {
		respCache.chainLRU.Remove(record.element)
	}
	respCache.stats.ChainOwners = len(respCache.chainOwners)
}

// responseCacheWriteAllowed 判断当前写入策略下 owner 是否有写入资格。
// on_demand 下过期记录按不合格处理（惰性删除交给清理循环）。
func responseCacheWriteAllowed(owner string) bool {
	if owner == "" {
		owner = "anon"
	}
	respCache.mu.RLock()
	defer respCache.mu.RUnlock()
	if respCache.config.writePolicy != database.ResponseCacheWritePolicyOnDemand {
		return true
	}
	record := respCache.chainOwners[owner]
	return record != nil && time.Now().Before(record.expiresAt)
}

// setResponseCache 存储响应上下文（按 owner 命名空间隔离）。
// on_demand 写入策略下，未续链过的 owner 直接跳过（本地与 Redis 一并跳过）。
func setResponseCache(owner, responseID string, items []json.RawMessage) {
	if !responseCacheWriteAllowed(owner) {
		respCache.mu.Lock()
		respCache.stats.SkippedWrites++
		respCache.mu.Unlock()
		return
	}
	storeKey := responseCacheStoreKey(owner, responseID)
	runtimeItems, _, _ := admitResponseCache(storeKey, items)

	respCache.mu.RLock()
	runtimeCache := respCache.runtimeCache
	respCache.mu.RUnlock()

	if runtimeCache != nil && len(runtimeItems) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := runtimeCache.SetResponseContext(ctx, storeKey, runtimeItems, responseCacheTTL); err != nil {
			log.Printf("写入 Redis response context 失败: response_id=%s err=%v", responseID, err)
		}
	}
}

func admitResponseCache(storeKey string, items []json.RawMessage) ([]json.RawMessage, bool, bool) {
	respCache.mu.Lock()
	defer respCache.mu.Unlock()

	items = trimResponseContextTail(items, respCache.config.maxItems)
	if normalizedItems, err := cache.NormalizeResponseContextItems(items); err == nil {
		items = normalizedItems
	}
	var entryBytes int64
	for _, item := range items {
		entryBytes += int64(len(item))
	}
	if entryBytes > respCache.stats.LargestSeenEntryBytes {
		respCache.stats.LargestSeenEntryBytes = entryBytes
	}
	if existing := respCache.store[storeKey]; existing != nil {
		respCache.removeEntryLocked(existing, responseCacheRemovalReplace)
	}
	overL1ByteBudget := entryBytes > respCache.config.maxEntryBytes ||
		entryBytes > respCache.config.maxBytes
	if respCache.config.maxEntries <= 0 || overL1ByteBudget {
		respCache.stats.OversizeSkips++
		if respCache.runtimeCache == nil && overL1ByteBudget {
			respCache.stats.OversizeRejections++
		}
		if respCache.runtimeCache == nil {
			respCache.setMarkerLocked(storeKey, responseCacheLookupKnownOversize, time.Now().Add(respCache.config.ttl))
		}
		return items, false, overL1ByteBudget
	}

	retainedItems := cloneResponseContextItems(items)
	for len(respCache.store)+1 > respCache.config.maxEntries ||
		respCache.stats.Bytes+entryBytes > respCache.config.maxBytes {
		oldest := respCache.oldestLocked()
		if oldest == nil {
			overL1ByteBudget = respCache.stats.Bytes+entryBytes > respCache.config.maxBytes
			respCache.stats.OversizeSkips++
			if respCache.runtimeCache == nil && overL1ByteBudget {
				respCache.stats.OversizeRejections++
			}
			if respCache.runtimeCache == nil {
				respCache.setMarkerLocked(storeKey, responseCacheLookupKnownOversize, time.Now().Add(respCache.config.ttl))
			}
			return items, false, overL1ByteBudget
		}
		reason := responseCacheRemovalByteEviction
		if len(respCache.store)+1 > respCache.config.maxEntries {
			reason = responseCacheRemovalCountEviction
		}
		respCache.removeEntryLocked(oldest, reason)
	}

	entry := &responseCacheEntry{
		key:       storeKey,
		items:     retainedItems,
		bytes:     entryBytes,
		expiresAt: time.Now().Add(respCache.config.ttl),
	}
	entry.element = respCache.lru.PushFront(entry)
	respCache.store[storeKey] = entry
	respCache.removeMarkerLocked(storeKey)
	respCache.stats.Entries = len(respCache.store)
	respCache.stats.Bytes += entryBytes
	if respCache.stats.Bytes > respCache.stats.HighWaterBytes {
		respCache.stats.HighWaterBytes = respCache.stats.Bytes
	}
	return items, true, false
}

func cloneResponseContextItems(items []json.RawMessage) []json.RawMessage {
	itemsCopy := make([]json.RawMessage, len(items))
	for i, item := range items {
		itemsCopy[i] = append(json.RawMessage(nil), item...)
	}
	return itemsCopy
}

type responseCacheRemovalReason uint8

const (
	responseCacheRemovalReplace responseCacheRemovalReason = iota
	responseCacheRemovalExpiration
	responseCacheRemovalCountEviction
	responseCacheRemovalByteEviction
)

func (c *responseCacheState) oldestLocked() *responseCacheEntry {
	if c.lru == nil {
		return nil
	}
	element := c.lru.Back()
	if element == nil {
		return nil
	}
	entry, _ := element.Value.(*responseCacheEntry)
	return entry
}

func (c *responseCacheState) removeEntryLocked(entry *responseCacheEntry, reason responseCacheRemovalReason) {
	if entry == nil || c.store[entry.key] != entry {
		return
	}
	delete(c.store, entry.key)
	if entry.element != nil {
		c.lru.Remove(entry.element)
	}
	c.stats.Entries = len(c.store)
	c.stats.Bytes -= entry.bytes
	switch reason {
	case responseCacheRemovalExpiration:
		c.stats.Expirations++
	case responseCacheRemovalCountEviction:
		c.stats.CountEvictions++
		if c.runtimeCache == nil {
			c.setMarkerLocked(entry.key, responseCacheLookupKnownEvicted, entry.expiresAt)
		}
	case responseCacheRemovalByteEviction:
		c.stats.ByteEvictions++
		if c.runtimeCache == nil {
			c.setMarkerLocked(entry.key, responseCacheLookupKnownEvicted, entry.expiresAt)
		}
	}
}

func (c *responseCacheState) setMarkerLocked(key string, kind responseCacheLookupKind, expiresAt time.Time) {
	if key == "" || (kind != responseCacheLookupKnownEvicted && kind != responseCacheLookupKnownOversize) {
		return
	}
	maxExpiry := time.Now().Add(c.config.ttl)
	if expiresAt.IsZero() || expiresAt.After(maxExpiry) {
		expiresAt = maxExpiry
	}
	if existing := c.markers[key]; existing != nil {
		existing.kind = kind
		existing.expiresAt = expiresAt
		c.markerLRU.MoveToFront(existing.element)
		return
	}
	marker := &responseCacheMarker{key: key, kind: kind, expiresAt: expiresAt}
	marker.element = c.markerLRU.PushFront(marker)
	c.markers[key] = marker
	for len(c.markers) > responseCacheMaxMarkers {
		element := c.markerLRU.Back()
		if element == nil {
			break
		}
		oldest, _ := element.Value.(*responseCacheMarker)
		c.removeMarkerLocked(oldest.key)
	}
}

func (c *responseCacheState) removeMarkerLocked(key string) {
	marker := c.markers[key]
	if marker == nil {
		return
	}
	delete(c.markers, key)
	if marker.element != nil {
		c.markerLRU.Remove(marker.element)
	}
}

func (c *responseCacheState) enforceConfigLocked() {
	for element := c.lru.Back(); element != nil; {
		previous := element.Prev()
		entry, _ := element.Value.(*responseCacheEntry)
		if entry != nil && entry.bytes > c.config.maxEntryBytes {
			c.removeEntryLocked(entry, responseCacheRemovalByteEviction)
		}
		element = previous
	}
	for len(c.store) > c.config.maxEntries || c.stats.Bytes > c.config.maxBytes {
		oldest := c.oldestLocked()
		if oldest == nil {
			break
		}
		reason := responseCacheRemovalByteEviction
		if len(c.store) > c.config.maxEntries {
			reason = responseCacheRemovalCountEviction
		}
		c.removeEntryLocked(oldest, reason)
	}
}

// trimResponseContextTail keeps one contiguous suffix no longer than maxItems.
// If the initial boundary splits a known one-to-one call/output match, that
// interval is excluded. Moving the boundary can expose another crossing
// interval, so the scan repeats until every retained matched pair is complete.
func trimResponseContextTail(items []json.RawMessage, maxItems int) []json.RawMessage {
	if maxItems <= 0 {
		return nil
	}
	if len(items) <= maxItems {
		return items
	}

	type callKey struct {
		callID   string
		callType string
	}
	type matchedInterval struct {
		callIndex   int
		outputIndex int
	}
	pendingCalls := make(map[callKey][]int)
	var intervals []matchedInterval
	for index, item := range items {
		typ := gjson.GetBytes(item, "type").String()
		callType, isCall, isOutput := responseContextPairType(typ)
		if callType == "" {
			continue
		}
		callID := gjson.GetBytes(item, "call_id").String()
		if callID == "" {
			continue
		}
		key := callKey{callID: callID, callType: callType}
		if isCall {
			pendingCalls[key] = append(pendingCalls[key], index)
			continue
		}
		if !isOutput || len(pendingCalls[key]) == 0 {
			continue
		}
		callIndex := pendingCalls[key][0]
		pendingCalls[key] = pendingCalls[key][1:]
		intervals = append(intervals, matchedInterval{
			callIndex:   callIndex,
			outputIndex: index,
		})
	}

	start := len(items) - maxItems
	for {
		nextStart := start
		for _, interval := range intervals {
			if interval.callIndex < start && interval.outputIndex >= start && interval.outputIndex+1 > nextStart {
				nextStart = interval.outputIndex + 1
			}
		}
		if nextStart == start {
			return items[start:]
		}
		start = nextStart
	}
}

func responseContextPairType(typ string) (callType string, isCall, isOutput bool) {
	switch typ {
	case "function_call", "tool_call", "local_shell_call", "shell_call",
		"apply_patch_call", "tool_search_call", "custom_tool_call", "mcp_tool_call":
		return typ, true, false
	case "function_call_output":
		return "function_call", false, true
	case "tool_call_output":
		return "tool_call", false, true
	case "local_shell_call_output":
		return "local_shell_call", false, true
	case "shell_call_output":
		return "shell_call", false, true
	case "apply_patch_call_output":
		return "apply_patch_call", false, true
	case "tool_search_call_output":
		return "tool_search_call", false, true
	case "custom_tool_call_output":
		return "custom_tool_call", false, true
	case "mcp_tool_call_output":
		return "mcp_tool_call", false, true
	default:
		return "", false, false
	}
}

// getResponseCache 查找缓存的响应上下文；owner 不匹配等同缓存未命中，
// 防止跨 API Key 用 response_id 拉取他人对话历史。
func getResponseCache(owner, responseID string) []json.RawMessage {
	result := getResponseCacheResult(owner, responseID)
	if result.Kind != responseCacheLookupHit {
		return nil
	}
	return result.Items
}

// getResponseCacheResult preserves enough lookup state for the HTTP handlers
// to distinguish a reconstructable hit from a final local/backend failure.
// 命中/未命中计数只在这个出口、单一临界区内记账一次：聚合 Hits/Misses 是
// 端到端口径，Local*/Remote* 是分层口径，两组在任意快照瞬间保持一致。
func getResponseCacheResult(owner, responseID string) responseCacheLookupResult {
	result := lookupResponseCacheResult(owner, responseID)
	respCache.mu.Lock()
	// 任何续链查询（无论命中与否）都赋予 owner 写入资格：这是 on_demand
	// 写入策略的准入信号，命中率与之无关。
	markResponseCacheChainOwnerLocked(owner)
	if result.Kind == responseCacheLookupHit {
		respCache.stats.Hits++
		if result.Source == responseCacheSourceBackend {
			respCache.stats.LocalMisses++
			respCache.stats.RemoteHits++
			if result.oversizeBypass {
				respCache.stats.OversizeBypasses++
			}
		} else {
			respCache.stats.LocalHits++
		}
	} else {
		respCache.stats.Misses++
		respCache.stats.LocalMisses++
		if result.remoteMiss {
			respCache.stats.RemoteMisses++
		}
	}
	respCache.mu.Unlock()
	return result
}

func lookupResponseCacheResult(owner, responseID string) responseCacheLookupResult {
	storeKey := responseCacheStoreKey(owner, responseID)
	respCache.mu.Lock()
	entry, ok := respCache.store[storeKey]
	runtimeCache := respCache.runtimeCache
	config := respCache.config
	expired := false
	if ok {
		if !time.Now().Before(entry.expiresAt) {
			respCache.removeEntryLocked(entry, responseCacheRemovalExpiration)
			expired = true
		} else {
			respCache.lru.MoveToFront(entry.element)
			// entry.items 插入后不可变（admit 时已存私有克隆），
			// 取引用后在锁外克隆，避免大条目命中时锁内做兆级拷贝。
			items := entry.items
			respCache.mu.Unlock()
			return responseCacheLookupResult{
				Items:  cloneResponseContextItems(items),
				Kind:   responseCacheLookupHit,
				Source: responseCacheSourceLocal,
			}
		}
	}
	if runtimeCache == nil {
		if marker := respCache.markers[storeKey]; marker != nil {
			if time.Now().Before(marker.expiresAt) {
				respCache.markerLRU.MoveToFront(marker.element)
				kind := marker.kind
				respCache.mu.Unlock()
				return responseCacheLookupResult{Kind: kind}
			}
			respCache.removeMarkerLocked(storeKey)
		}
	}
	respCache.mu.Unlock()

	if runtimeCache == nil {
		if expired {
			return responseCacheLookupResult{Kind: responseCacheLookupExpired}
		}
		return responseCacheLookupResult{Kind: responseCacheLookupMiss}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	backendResult, err := readResponseContextBackend(ctx, runtimeCache, storeKey, config)
	if err != nil {
		log.Printf("读取 Redis response context 失败: response_id=%s err=%v", responseID, err)
		return responseCacheLookupResult{Kind: responseCacheLookupBackendError, Err: err}
	}
	switch backendResult.Status {
	case cache.ResponseContextReadMiss:
		if expired {
			return responseCacheLookupResult{Kind: responseCacheLookupExpired, remoteMiss: true}
		}
		return responseCacheLookupResult{Kind: responseCacheLookupMiss, remoteMiss: true}
	case cache.ResponseContextReadTooLarge:
		return responseCacheLookupResult{Kind: responseCacheLookupReconstructionTooLarge}
	case cache.ResponseContextReadCorrupt:
		return responseCacheLookupResult{Kind: responseCacheLookupBackendCorrupt}
	case cache.ResponseContextReadFound:
	default:
		return responseCacheLookupResult{Kind: responseCacheLookupBackendCorrupt}
	}
	items, err := cache.NormalizeResponseContextItems(backendResult.Items)
	if err != nil {
		return responseCacheLookupResult{Kind: responseCacheLookupBackendCorrupt}
	}
	if responseContextLogicalBytes(items) > config.reconstructMaxBytes {
		return responseCacheLookupResult{Kind: responseCacheLookupReconstructionTooLarge}
	}
	items = trimResponseContextTail(items, config.maxItems)
	runtimeItems, promoted, overL1ByteBudget := admitResponseCache(storeKey, items)
	return responseCacheLookupResult{
		Items:          runtimeItems,
		Kind:           responseCacheLookupHit,
		Source:         responseCacheSourceBackend,
		Promoted:       promoted,
		oversizeBypass: !promoted && overL1ByteBudget,
	}
}

func setResponseCacheLocal(storeKey string, items []json.RawMessage) bool {
	_, admitted, _ := admitResponseCache(storeKey, items)
	return admitted
}

func recordResponseCacheKnownUnavailableError() {
	respCache.mu.Lock()
	respCache.stats.KnownUnavailableErrors++
	respCache.mu.Unlock()
}

func readResponseContextBackend(ctx context.Context, runtimeCache cache.TokenCache, storeKey string, config responseCacheConfig) (cache.ResponseContextReadResult, error) {
	if bounded, ok := runtimeCache.(cache.BoundedResponseContextReader); ok {
		return bounded.GetResponseContextBounded(ctx, storeKey, responseContextWireLimit(config))
	}
	// Third-party shared caches retain compatibility through the original
	// TokenCache method. Only the official Redis implementation guarantees the
	// pre-deserialization bound provided by BoundedResponseContextReader.
	items, err := runtimeCache.GetResponseContext(ctx, storeKey)
	if err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return cache.ResponseContextReadResult{Status: cache.ResponseContextReadCorrupt}, nil
		}
		return cache.ResponseContextReadResult{}, err
	}
	if len(items) == 0 {
		return cache.ResponseContextReadResult{Status: cache.ResponseContextReadMiss}, nil
	}
	return cache.ResponseContextReadResult{Status: cache.ResponseContextReadFound, Items: items}, nil
}

func responseContextWireLimit(config responseCacheConfig) int64 {
	return cache.ResponseContextWireLimit(config.reconstructMaxBytes, config.maxItems)
}

func responseContextLogicalBytes(items []json.RawMessage) int64 {
	var total int64
	for _, item := range items {
		total += int64(len(item))
	}
	return total
}

// respCacheCleanupLoop 后台清理过期条目
func respCacheCleanupLoop() {
	ticker := time.NewTicker(responseCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		cleanupResponseCacheExpired(time.Now())
	}
}

func cleanupResponseCacheExpired(now time.Time) {
	respCache.mu.Lock()
	for _, entry := range respCache.store {
		if !now.Before(entry.expiresAt) {
			respCache.removeEntryLocked(entry, responseCacheRemovalExpiration)
		}
	}
	for key, marker := range respCache.markers {
		if !now.Before(marker.expiresAt) {
			respCache.removeMarkerLocked(key)
		}
	}
	for owner, record := range respCache.chainOwners {
		if !now.Before(record.expiresAt) {
			removeResponseCacheChainOwnerLocked(owner)
		}
	}
	respCache.mu.Unlock()
}

// expandPreviousResponse 检查请求中是否有 previous_response_id，
// 如果有且缓存命中（且归属于同一 owner），则将历史对话 items 注入到 input 头部。
// 返回处理后的 body 和提取到的 previous_response_id（用于后续缓存链路）。
func expandPreviousResponse(codexBody []byte, owner string) ([]byte, string) {
	prevID := gjson.GetBytes(codexBody, "previous_response_id").String()
	if prevID == "" {
		return codexBody, ""
	}

	currentInput := gjson.GetBytes(codexBody, "input")

	// 客户端已经自带 function_call 等续链项时，跳过注入。
	// 缓存里只会存 function_call 类项（见 cacheCompletedResponse + isCodexToolCallContextType），
	// 再注入会让同一 call_id 出现两次，上游会以 "duplicate call_id" 等 400 拒绝。
	// 仍返回 prevID，让 cacheCompletedResponse 能把这一轮响应链入缓存。
	if currentInput.IsArray() && inputHasToolCallContext(currentInput) {
		log.Printf("input 已自带工具续链项，跳过 previous_response_id=%s 的历史注入", prevID)
		return codexBody, prevID
	}

	cached := getResponseCache(owner, prevID)
	if cached == nil {
		// 缓存未命中（首次请求 / 过期 / 其他实例），无法展开，按原样继续。
		// 若 input 仅含 function_call_output 又拿不到对应的 function_call，
		// 上游通常会返回 "No tool call found for function call output" 400，
		// 这里打日志便于诊断（不阻断，让上游错误透传给客户端）。
		if currentInput.IsArray() && inputHasFunctionCallOutput(currentInput) {
			log.Printf("缓存未命中且 input 含 function_call_output，previous_response_id=%s，上游可能返回 400", prevID)
		}
		return codexBody, prevID
	}

	// 构建新 input: 缓存的历史 items + 当前 input items
	var merged []json.RawMessage
	merged = append(merged, cached...)
	if currentInput.IsArray() {
		currentInput.ForEach(func(_, v gjson.Result) bool {
			merged = append(merged, json.RawMessage(v.Raw))
			return true
		})
	}

	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		log.Printf("展开 previous_response_id 失败: %v", err)
		return codexBody, prevID
	}

	codexBody, _ = sjson.SetRawBytes(codexBody, "input", mergedJSON)
	log.Printf("已展开 previous_response_id=%s，注入 %d 条历史 items", prevID, len(cached))
	return codexBody, prevID
}

// inputHasToolCallContext 判断 input 数组里是否已包含 function_call 类续链项，
// 这类项一旦同时出现在缓存里会造成 call_id 冲突。
func inputHasToolCallContext(input gjson.Result) bool {
	found := false
	input.ForEach(func(_, v gjson.Result) bool {
		if isCodexToolCallContextType(v.Get("type").String()) {
			found = true
			return false
		}
		return true
	})
	return found
}

// inputHasFunctionCallOutput 判断 input 数组里是否含 *_output 项（缺少配对的 function_call 时上游会 400）。
func inputHasFunctionCallOutput(input gjson.Result) bool {
	found := false
	input.ForEach(func(_, v gjson.Result) bool {
		if isCodexToolCallOutputType(v.Get("type").String()) {
			found = true
			return false
		}
		return true
	})
	return found
}

// isCodexToolCallOutputType 判断 item 类型是否属于工具调用输出项（*_call_output），
// 与 isCodexToolCallContextType 的调用项集合一一对应。
func isCodexToolCallOutputType(typ string) bool {
	switch typ {
	case "function_call_output",
		"tool_call_output",
		"local_shell_call_output",
		"shell_call_output",
		"apply_patch_call_output",
		"tool_search_call_output",
		"custom_tool_call_output",
		"mcp_tool_call_output":
		return true
	default:
		return false
	}
}

// cacheCompletedResponse 从 response.completed 事件中提取 response.id 和 response.output，
// 与当前请求的 expanded input 合并后存入 owner 命名空间的缓存。
// 仅在响应包含需要 call_id 续链的 Codex 工具调用时才缓存，避免为普通对话浪费内存。
func cacheCompletedResponse(owner string, expandedInputRaw []byte, completedData []byte) {
	cacheCompletedResponseWithOutputItems(owner, expandedInputRaw, completedData, nil)
}

// cacheCompletedResponseWithOutputItems 在 response.completed.output 未携带工具调用时，
// 使用流式 response.output_item.done 中已收集的 outputItems 作为兜底。
func cacheCompletedResponseWithOutputItems(owner string, expandedInputRaw []byte, completedData []byte, outputItems []json.RawMessage) {
	respID := gjson.GetBytes(completedData, "response.id").String()
	if respID == "" {
		return
	}

	// 仅在响应包含可安全回放的 Codex 工具调用时才缓存（普通对话无需
	// previous_response_id 展开）。image_generation_call / web_search_call
	// 虽然也是 *_call 结尾，但不属于 call_id 工具续链体系。
	output := gjson.GetBytes(completedData, "response.output")
	if !output.IsArray() && len(outputItems) == 0 {
		return
	}
	var replayableOutput []json.RawMessage
	output.ForEach(func(_, item gjson.Result) bool {
		if replayable, ok := replayableCachedOutputItem(item); ok {
			replayableOutput = append(replayableOutput, replayable)
		}
		return true
	})
	if len(replayableOutput) == 0 {
		for _, raw := range outputItems {
			if replayable, ok := replayableCachedOutputItem(gjson.ParseBytes(raw)); ok {
				replayableOutput = append(replayableOutput, replayable)
			}
		}
	}
	if len(replayableOutput) == 0 {
		return
	}

	var items []json.RawMessage

	// 添加展开后的请求 input items。先成对移除参数被截断的普通
	// function_call/function_call_output，避免缓存把毒历史延长到下一轮。
	inputItems := gjson.ParseBytes(sanitizeResponseCacheInput(expandedInputRaw))
	if inputItems.IsArray() {
		inputItems.ForEach(func(_, v gjson.Result) bool {
			if item, ok := replayableCachedInputItem(v); ok {
				items = append(items, item)
			}
			return true
		})
	}

	// 添加响应 output 中真正需要续链且已验证的工具上下文；reasoning/message
	// 等服务端输出 item 带有 rs_/msg_ id，store=false 时回灌会触发 item not found。
	items = append(items, replayableOutput...)

	if len(items) > 0 {
		setResponseCache(owner, respID, items)
	}
}

func sanitizeResponseCacheInput(raw []byte) []byte {
	var input []any
	if err := json.Unmarshal(raw, &input); err != nil {
		return raw
	}
	body := map[string]any{"input": input}
	normalizeResponsesToolCallArgumentTypes(body)
	if !sanitizeMalformedResponsesFunctionCalls(body) {
		return raw
	}
	sanitized, err := json.Marshal(body["input"])
	if err != nil {
		return raw
	}
	return sanitized
}

func replayableCachedInputItem(item gjson.Result) (json.RawMessage, bool) {
	if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").Exists() {
		return nil, false
	}
	raw, ok := normalizeReplayableCachedFunctionCall(json.RawMessage(item.Raw))
	if !ok {
		return nil, false
	}
	return stripResponseItemID(raw)
}

func replayableCachedOutputItem(item gjson.Result) (json.RawMessage, bool) {
	if !isCodexToolCallContextType(item.Get("type").String()) {
		return nil, false
	}
	raw, ok := normalizeReplayableCachedFunctionCall(json.RawMessage(item.Raw))
	if !ok {
		return nil, false
	}
	return stripResponseItemID(raw)
}

func normalizeReplayableCachedFunctionCall(raw json.RawMessage) (json.RawMessage, bool) {
	if gjson.GetBytes(raw, "type").String() != "function_call" {
		return raw, true
	}
	argumentValue := gjson.GetBytes(raw, "arguments")
	arguments := ""
	if argumentValue.Exists() && argumentValue.Type != gjson.Null {
		if argumentValue.Type == gjson.String {
			arguments = argumentValue.String()
		} else {
			arguments = argumentValue.Raw
		}
	}
	normalized, valid := normalizeOrdinaryFunctionCallArguments(arguments)
	if !valid {
		return nil, false
	}
	if argumentValue.Type == gjson.String && normalized == arguments {
		return raw, true
	}
	updated, err := sjson.SetBytes(raw, "arguments", normalized)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(updated), true
}

func stripResponseItemID(raw json.RawMessage) (json.RawMessage, bool) {
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return raw, true
	}
	if _, exists := item["id"]; !exists {
		return raw, true
	}
	delete(item, "id")
	stripped, err := json.Marshal(item)
	if err != nil {
		return nil, false
	}
	return stripped, true
}

func isCodexToolCallContextType(typ string) bool {
	switch typ {
	case "function_call",
		"tool_call",
		"local_shell_call",
		"shell_call",
		"apply_patch_call",
		"tool_search_call",
		"custom_tool_call",
		"mcp_tool_call":
		return true
	default:
		return false
	}
}
