package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestGrokNativeUsage_MessagesParsesCacheCreationBreakdown(t *testing.T) {
	payload := []byte(`{"type":"message","usage":{"input_tokens":2048,"cache_read_input_tokens":1800,"cache_creation_input_tokens":248,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100},"output_tokens":503}}`)
	usage := grokNativeUsage(GrokProtocolMessages, payload)
	if usage == nil {
		t.Fatal("usage must be parsed")
	}
	if usage.CachedTokens != 1800 || usage.CacheWriteTokens != 248 || usage.CacheWrite5mTokens != 148 || usage.CacheWrite1hTokens != 100 {
		t.Fatalf("cache fields = read %d / write %d (5m %d, 1h %d)", usage.CachedTokens, usage.CacheWriteTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens)
	}
	// Raw Anthropic semantics are preserved here; the Claude route converts them.
	if usage.InputTokens != 2048 || usage.OutputTokens != 503 {
		t.Fatalf("raw input/output = %d/%d", usage.InputTokens, usage.OutputTokens)
	}
}

func TestGrokNativeUsage_MessagesFallsBackToTotalCacheCreation(t *testing.T) {
	payload := []byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":4081,"output_tokens":4}}`)
	usage := grokNativeUsage(GrokProtocolMessages, payload)
	if usage.CacheWriteTokens != 4081 || usage.CacheWrite5mTokens != 0 || usage.CacheWrite1hTokens != 0 {
		t.Fatalf("parser must keep only the reported breakdown: write %d (5m %d, 1h %d)", usage.CacheWriteTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens)
	}
}

func TestMergeGrokNativeUsage_KeepsCacheWriteFields(t *testing.T) {
	first := &UsageInfo{InputTokens: 10, CacheWriteTokens: 4081, CacheWrite1hTokens: 4081}
	second := &UsageInfo{InputTokens: 10, OutputTokens: 4}
	merged := mergeGrokNativeUsage(first, second)
	if merged.CacheWriteTokens != 4081 || merged.CacheWrite1hTokens != 4081 || merged.OutputTokens != 4 {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestApplyAnthropicUsageSemantics_TotalsInputAcrossCacheBuckets(t *testing.T) {
	usage := newUsageInfo(2048, 503, 0, 1800)
	usage.CacheWriteTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens = 248, 148, 100
	applyAnthropicUsageSemantics(usage)
	if usage.InputTokens != 4096 || usage.PromptTokens != 4096 {
		t.Fatalf("input must be uncached+read+write = 4096, got input %d prompt %d", usage.InputTokens, usage.PromptTokens)
	}
	if usage.TotalTokens != 4096+503 || usage.CachedTokens != 1800 || usage.CacheWrite1hTokens != 100 {
		t.Fatalf("total %d cached %d write1h %d", usage.TotalTokens, usage.CachedTokens, usage.CacheWrite1hTokens)
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 1800 {
		t.Fatal("cached token details must survive")
	}
	// Idempotent: a second application must not double count.
	applyAnthropicUsageSemantics(usage)
	if usage.InputTokens != 4096 {
		t.Fatalf("second application changed input to %d", usage.InputTokens)
	}
	applyAnthropicUsageSemantics(nil)
}

func TestInjectClaudeCodeSystemPrompt_InheritsClientCacheTTL(t *testing.T) {
	// system.0 是计费块(无 cache_control),声明块在 system.1。
	oneHour := []byte(`{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`)
	out := injectClaudeCodeSystemPrompt(oneHour)
	if got := gjson.GetBytes(out, "system.1.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("injected preamble ttl = %q, want 1h so it does not precede the client's 1h block with a 5m block", got)
	}
	fiveMin := []byte(`{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	out = injectClaudeCodeSystemPrompt(fiveMin)
	if gjson.GetBytes(out, "system.1.cache_control.ttl").Exists() {
		t.Fatal("client without 1h must keep the default preamble block")
	}
	none := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out = injectClaudeCodeSystemPrompt(none)
	if !gjson.GetBytes(out, "system.1.cache_control").Exists() || gjson.GetBytes(out, "system.1.cache_control.ttl").Exists() {
		t.Fatal("no client cache_control: default 5m preamble block")
	}
	messagesOnly := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	out = injectClaudeCodeSystemPrompt(messagesOnly)
	if got := gjson.GetBytes(out, "system.1.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("a 1h block in messages must also make the preamble 1h, got %q", got)
	}
}

func TestStreamMergeDoesNotDoubleCountCacheWrites(t *testing.T) {
	// message_start carries the TTL breakdown, message_delta only the total.
	start := grokNativeUsage(GrokProtocolMessages, []byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":3634,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":3634},"output_tokens":1}}}`))
	delta := grokNativeUsage(GrokProtocolMessages, []byte(`{"type":"message_delta","usage":{"input_tokens":10,"cache_creation_input_tokens":3634,"cache_read_input_tokens":0,"output_tokens":4}}`))
	merged := mergeGrokNativeUsage(start, delta)
	w5m, w1h := splitClaudeCacheWrites(merged)
	if w5m != 0 || w1h != 3634 {
		t.Fatalf("split = 5m %d / 1h %d, want 0 / 3634", w5m, w1h)
	}
	if w5m, w1h := splitClaudeCacheWrites(delta); w5m != 3634 || w1h != 0 {
		t.Fatalf("total-only usage must default to 5m: %d / %d", w5m, w1h)
	}
}
