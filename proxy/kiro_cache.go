package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash"
	"image"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kiro 不回报 prompt cache 命中。上游没给精确用量时，按 Anthropic Prompt Caching 规则
// （cache_control 断点、20 块回溯、5m/1h TTL 滑动续期、最多 4 个断点）在本地估算
// cache_creation / cache_read 的拆分。只影响用量上报，不减少上游开销；默认关闭。
// 移植自 kiro.rs src/anthropic/cache_metering.rs，只保留进程内存储，不搬 Redis、落盘与诊断计数。

const (
	kiroCacheCapacity       = 262_144
	kiroCacheDefaultTTL     = 300
	kiroCacheMaxTTL         = 3600
	kiroCacheMaxBreakpoints = 4
	kiroCacheLookback       = 20
)

var kiroCacheSimulation atomic.Bool

// SetKiroCacheSimulation 开关本地 cache 模拟（Kiro 设置项）。
func SetKiroCacheSimulation(enabled bool) { kiroCacheSimulation.Store(enabled) }

// KiroCacheSimulationEnabled 返回本地 cache 模拟是否开启。
func KiroCacheSimulationEnabled() bool { return kiroCacheSimulation.Load() }

// kiroCacheUsage 是估算口径的覆盖量：read 为命中的最长前缀，covered 为最深断点覆盖的前缀，
// total 为整个 prompt。最终按真实 total 做无量纲比例分摊。
type kiroCacheUsage struct{ read, covered, total int }

// split 返回互斥的 (input, cache_creation, cache_read)，三者之和等于 totalReal。
func (u kiroCacheUsage) split(totalReal int) (int, int, int) {
	total := max(totalReal, 0)
	if u.covered <= 0 || u.total <= 0 {
		return total, 0, 0
	}
	ratio := min(max(float64(u.covered)/float64(u.total), 0), 1)
	cached := min(int(math.Round(float64(total)*ratio)), total)
	tail := total - cached
	read := min(max(int(math.Round(float64(cached)*float64(u.read)/float64(u.covered))), 0), cached)
	write := cached - read
	// 客户端按「零值不覆盖非零」合并 message_start 与 message_delta 的 usage，input 归零会残留估算值。
	if total > 0 && tail == 0 {
		if write > 0 {
			write--
		} else {
			read--
		}
		tail = 1
	}
	return tail, write, read
}

type kiroCacheEntry struct {
	tokens                  int
	ttl, expiresAt, lastHit int64
}

type kiroCacheMeter struct {
	mu      sync.Mutex
	entries map[uint64]*kiroCacheEntry
}

var kiroCache = &kiroCacheMeter{entries: map[uint64]*kiroCacheEntry{}}

// probe 命中时按条目自身 TTL 滑动续期。
func (m *kiroCacheMeter) probe(key uint64, now int64) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[key]
	if entry == nil || entry.expiresAt <= now {
		return 0, false
	}
	entry.lastHit, entry.expiresAt = now, now+entry.ttl
	return entry.tokens, true
}

// record 写入断点；超过 1.2 倍容量时先清过期，仍超限则按最近命中时间淘汰到 0.9 倍。
func (m *kiroCacheMeter) record(key uint64, tokens int, ttl, now int64) {
	ttl = min(max(ttl, 60), kiroCacheMaxTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = &kiroCacheEntry{tokens: tokens, ttl: ttl, expiresAt: now + ttl, lastHit: now}
	if len(m.entries) <= kiroCacheCapacity*6/5 {
		return
	}
	for k, entry := range m.entries {
		if entry.expiresAt <= now {
			delete(m.entries, k)
		}
	}
	if len(m.entries) <= kiroCacheCapacity*6/5 {
		return
	}
	keys := make([]uint64, 0, len(m.entries))
	for k := range m.entries {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b uint64) int { return int(m.entries[a].lastHit - m.entries[b].lastHit) })
	for _, k := range keys[:len(keys)-kiroCacheCapacity/10*9] {
		delete(m.entries, k)
	}
}

type kiroPromptBlock struct {
	signature   []byte
	tokens      int
	ttl         int64 // 0 表示该块没有 cache_control 断点
	invalid     bool
	cacheable   bool
	group       string // 连续 tool_use / tool_result 各算一个回溯位置
	currentTurn bool
}

type kiroBreakpoint struct {
	index int
	ttl   int64
}

// kiroComputeCacheUsage 查找并写入断点。关闭、无断点或请求体无法解析时返回零覆盖。
func kiroComputeCacheUsage(ctx context.Context, inbound *kiroInboundRequest) kiroCacheUsage {
	if !kiroCacheSimulation.Load() || inbound == nil {
		return kiroCacheUsage{}
	}
	var req map[string]any
	decoder := json.NewDecoder(bytes.NewReader(inbound.raw))
	decoder.UseNumber()
	if decoder.Decode(&req) != nil {
		return kiroCacheUsage{}
	}
	blocks := kiroExtractPromptBlocks(req)
	if len(blocks) == 0 {
		return kiroCacheUsage{}
	}
	usage := kiroCacheUsage{}
	for _, block := range blocks {
		usage.total += block.tokens
	}
	topTTL, topPresent, topValid := kiroCacheControlTTL(req["cache_control"])
	if topPresent && !topValid {
		return usage
	}
	breakpoints := kiroResolveBreakpoints(blocks, topTTL)
	if len(breakpoints) == 0 {
		return usage
	}

	hasher := sha256.New()
	kiroHashFrame(hasher, []byte(kiroCacheSeed(ctx, inbound, req)))
	kiroHashFrame(hasher, kiroCacheGlobalContext(req))
	cumTokens, cumHashes := make([]int, len(blocks)), make([]uint64, len(blocks))
	running := 0
	for i, block := range blocks {
		kiroHashFrame(hasher, block.signature)
		running += block.tokens
		cumTokens[i], cumHashes[i] = running, binary.BigEndian.Uint64(hasher.Sum(nil)[:8])
	}

	now := time.Now().Unix()
	for _, bp := range breakpoints {
		// 从断点往前最多回溯 20 个位置，找此前真正写入过的最长前缀。
		start := kiroLookbackStart(blocks, bp.index)
		for check := bp.index; check >= start; check-- {
			if tokens, ok := kiroCache.probe(cumHashes[check], now); ok {
				usage.read = max(usage.read, tokens)
				break
			}
		}
	}
	for _, bp := range breakpoints {
		kiroCache.record(cumHashes[bp.index], cumTokens[bp.index], bp.ttl, now)
	}
	usage.covered = cumTokens[breakpoints[len(breakpoints)-1].index]
	return usage
}

// kiroExtractPromptBlocks 按 tools → system → messages 的顺序拍平 prompt。
// 末尾连续 assistant 是 prefill，与 converter 一样不进前缀，其断点挪到保留前缀末尾。
func kiroExtractPromptBlocks(req map[string]any) []kiroPromptBlock {
	var blocks []kiroPromptBlock
	if tools, ok := req["tools"].([]any); ok {
		for _, tool := range tools {
			content := kiroWithoutCacheControl(tool)
			serialized, _ := json.Marshal(content)
			ttl, _, valid := kiroCacheControlTTL(kiroMapValue(tool, "cache_control"))
			blocks = append(blocks, kiroPromptBlock{signature: kiroBlockSignature("tool", nil, content), tokens: kiroEstimateTokens(string(serialized)), ttl: ttl, invalid: !valid, cacheable: true})
		}
	}
	switch system := req["system"].(type) {
	case string:
		blocks = append(blocks, kiroPromptBlock{signature: kiroBlockSignature("system", nil, map[string]any{"type": "text", "text": system}), tokens: kiroEstimateTokens(system), cacheable: strings.TrimSpace(system) != ""})
	case []any:
		for _, item := range system {
			text, _ := kiroMapValue(item, "text").(string)
			ttl, _, valid := kiroCacheControlTTL(kiroMapValue(item, "cache_control"))
			blocks = append(blocks, kiroPromptBlock{signature: kiroBlockSignature("system", nil, kiroWithoutCacheControl(item)), tokens: kiroEstimateTokens(text), ttl: ttl, invalid: !valid, cacheable: strings.TrimSpace(text) != ""})
		}
	}

	messages, _ := req["messages"].([]any)
	kept := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if role, _ := kiroMapValue(messages[i], "role").(string); role != "assistant" {
			kept = i + 1
			break
		}
	}
	for i, message := range messages[:kept] {
		role, _ := kiroMapValue(message, "role").(string)
		switch content := kiroMapValue(message, "content").(type) {
		case string:
			blocks = append(blocks, kiroPromptBlock{signature: kiroBlockSignature("message", role, map[string]any{"type": "text", "text": content}), tokens: kiroEstimateTokens(content), cacheable: strings.TrimSpace(content) != "", currentTurn: i+1 == kept && role == "user"})
		case []any:
			toolResult := false
			for _, item := range content {
				toolResult = toolResult || kiroMapValue(item, "type") == "tool_result"
			}
			for _, item := range content {
				ttl, _, valid := kiroCacheControlTTL(kiroMapValue(item, "cache_control"))
				stripped := kiroWithoutCacheControl(item)
				group, _ := kiroMapValue(stripped, "type").(string)
				if group != "tool_use" && group != "tool_result" {
					group = ""
				}
				blocks = append(blocks, kiroPromptBlock{signature: kiroBlockSignature("message", role, stripped), tokens: kiroPromptBlockTokens(stripped), ttl: ttl, invalid: !valid, cacheable: kiroBlockCacheable(stripped), group: group, currentTurn: i+1 == kept && role == "user" && !toolResult})
			}
		}
	}
	if kept == len(messages) || len(blocks) == 0 {
		return blocks
	}

	explicit, seen5m := 0, false
	for _, block := range blocks {
		if block.ttl > 0 {
			explicit++
		}
		seen5m = seen5m || block.ttl == kiroCacheDefaultTTL
	}
	last := &blocks[len(blocks)-1]
	for _, message := range messages[kept:] {
		content, _ := kiroMapValue(message, "content").([]any)
		for _, item := range content {
			ttl, present, valid := kiroCacheControlTTL(kiroMapValue(item, "cache_control"))
			if !present {
				continue
			}
			explicit++
			if !valid {
				last.invalid = true
				continue
			}
			last.invalid = last.invalid || (seen5m && ttl > kiroCacheDefaultTTL) || !kiroBlockCacheable(item)
			seen5m = seen5m || ttl == kiroCacheDefaultTTL
			if last.ttl == 0 {
				last.ttl = ttl
			}
		}
	}
	last.invalid = last.invalid || explicit > kiroCacheMaxBreakpoints
	return blocks
}

// kiroResolveBreakpoints 合并显式断点与顶层自动断点（topTTL>0）。非法配置整次不模拟：
// 上游真实行为是报错，本地不能虚报一个不会成功的缓存结果。
func kiroResolveBreakpoints(blocks []kiroPromptBlock, topTTL int64) []kiroBreakpoint {
	var breakpoints []kiroBreakpoint
	for i, block := range blocks {
		if block.invalid || block.ttl > 0 && !block.cacheable {
			return nil
		}
		if block.ttl > 0 {
			breakpoints = append(breakpoints, kiroBreakpoint{i, block.ttl})
		}
	}
	if topTTL > 0 {
		// 自动断点挂在当前提问之前，最新一条 user 提问始终算未缓存的 input。
		target := -1
		for i := len(blocks) - 1; i >= 0 && target < 0; i-- {
			if blocks[i].cacheable && !blocks[i].currentTurn {
				target = i
			}
		}
		for i := len(blocks) - 1; i >= 0 && target < 0; i-- {
			if blocks[i].cacheable {
				target = i
			}
		}
		if target >= 0 {
			if existing := slices.IndexFunc(breakpoints, func(bp kiroBreakpoint) bool { return bp.index == target }); existing >= 0 {
				if breakpoints[existing].ttl != topTTL {
					return nil
				}
			} else {
				breakpoints = append(breakpoints, kiroBreakpoint{target, topTTL})
			}
		}
	}
	slices.SortFunc(breakpoints, func(a, b kiroBreakpoint) int { return a.index - b.index })
	if len(breakpoints) > kiroCacheMaxBreakpoints {
		return nil
	}
	// 1h 断点必须在 5m 断点之前。
	seen5m := false
	for _, bp := range breakpoints {
		if bp.ttl <= kiroCacheDefaultTTL {
			seen5m = true
		} else if seen5m {
			return nil
		}
	}
	return breakpoints
}

func kiroLookbackStart(blocks []kiroPromptBlock, index int) int {
	positions := 1
	for index > 0 {
		if blocks[index].group == "" || blocks[index].group != blocks[index-1].group {
			if positions == kiroCacheLookback {
				break
			}
			positions++
		}
		index--
	}
	return index
}

// kiroCacheControlTTL 返回 (ttl 秒, 是否带 cache_control, 是否合法)。
func kiroCacheControlTTL(value any) (int64, bool, bool) {
	if value == nil {
		return 0, false, true
	}
	control, ok := value.(map[string]any)
	kind, _ := kiroMapValue(control, "type").(string)
	if !ok || !strings.EqualFold(kind, "ephemeral") {
		return 0, true, false
	}
	switch ttl := control["ttl"].(type) {
	case nil:
		return kiroCacheDefaultTTL, true, true
	case string:
		switch strings.ToLower(ttl) {
		case "5m":
			return kiroCacheDefaultTTL, true, true
		case "1h":
			return kiroCacheMaxTTL, true, true
		}
	}
	return 0, true, false
}

func kiroCacheSeed(ctx context.Context, inbound *kiroInboundRequest, req map[string]any) string {
	var keyID int64
	if identity := PayloadRuleIdentityFromContext(ctx); identity != nil {
		keyID = identity.APIKeyID
	}
	userID, _ := kiroMapValue(req["metadata"], "user_id").(string)
	if session := kiroCacheSessionID(userID); session != "" {
		if keyID == 0 {
			return "sess:" + session
		}
		return "key:" + strconv.FormatInt(keyID, 10) + ":sess:" + session
	}
	if keyID == 0 {
		// 无 Key 身份时多个客户端共用，按内容派生的会话 id 隔离。
		return "derived:" + kiroDeriveConversationID(inbound)
	}
	return "key:" + strconv.FormatInt(keyID, 10)
}

// kiroCacheSessionID 接受 {"session_id":...} 或 ..._session_<id> / session_<id>。
func kiroCacheSessionID(userID string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(userID), &parsed) == nil {
		session, _ := parsed["session_id"].(string)
		return strings.TrimSpace(session)
	}
	if _, session, ok := strings.Cut(userID, "session_"); ok {
		return strings.TrimSpace(session)
	}
	return ""
}

func kiroCacheGlobalContext(req map[string]any) []byte {
	var thinking, outputConfig any
	if value, ok := req["thinking"].(map[string]any); ok {
		thinking = map[string]any{"type": value["type"], "budget_tokens": value["budget_tokens"]}
	}
	if value, ok := req["output_config"].(map[string]any); ok {
		outputConfig = map[string]any{"effort": value["effort"]}
	}
	encoded, _ := json.Marshal(map[string]any{"model": req["model"], "tool_choice": req["tool_choice"], "thinking": thinking, "output_config": outputConfig})
	return encoded
}

func kiroBlockSignature(section string, role any, content any) []byte {
	encoded, _ := json.Marshal(map[string]any{"section": section, "role": role, "content": content})
	return encoded
}

func kiroBlockCacheable(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	switch kind, _ := object["type"].(string); kind {
	case "thinking", "redacted_thinking", "":
		return false
	case "text":
		text, _ := object["text"].(string)
		return strings.TrimSpace(text) != ""
	}
	return true
}

func kiroPromptBlockTokens(value any) int {
	object, _ := value.(map[string]any)
	switch kind, _ := object["type"].(string); kind {
	case "image":
		source, _ := object["source"].(map[string]any)
		data, _ := source["data"].(string)
		return kiroEstimateImageTokens(data)
	case "text", "thinking":
		if text, ok := object[kind].(string); ok {
			return kiroEstimateTokens(text)
		}
	}
	serialized, _ := json.Marshal(value)
	return kiroEstimateTokens(string(serialized))
}

// kiroEstimateImageTokens 按 Anthropic 公式：长边缩到 1568 后 宽×高/750；读不出尺寸按 1600。
func kiroEstimateImageTokens(data string) int {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return 1600
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width == 0 || config.Height == 0 {
		return 1600
	}
	width, height := float64(config.Width), float64(config.Height)
	if long := max(width, height); long > 1568 {
		width, height = width*1568/long, height*1568/long
	}
	return max(int(math.Round(width*height/750)), 1)
}

func kiroWithoutCacheControl(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	out := make(map[string]any, len(object))
	for key, item := range object {
		if key != "cache_control" {
			out[key] = item
		}
	}
	return out
}

func kiroMapValue(value any, key string) any {
	object, _ := value.(map[string]any)
	return object[key]
}

func kiroHashFrame(hasher hash.Hash, data []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(data)))
	hasher.Write(length[:])
	hasher.Write(data)
}
