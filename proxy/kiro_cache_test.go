package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func kiroCacheTestRequest(t *testing.T, body map[string]any) *kiroInboundRequest {
	t.Helper()
	raw, _ := json.Marshal(body)
	inbound, err := parseKiroInboundRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return inbound
}

func withKiroCacheSimulation(t *testing.T) {
	previous, meter := kiroCacheSimulation.Load(), kiroCache
	kiroCache = &kiroCacheMeter{entries: map[uint64]*kiroCacheEntry{}}
	SetKiroCacheSimulation(true)
	t.Cleanup(func() { SetKiroCacheSimulation(previous); kiroCache = meter })
}

func TestKiroCacheSplitIsMutuallyExclusive(t *testing.T) {
	if input, write, read := (kiroCacheUsage{read: 30, covered: 80, total: 100}).split(1000); input != 200 || write != 500 || read != 300 {
		t.Fatalf("split = %d/%d/%d", input, write, read)
	}
	if input, write, read := (kiroCacheUsage{read: 100, covered: 100, total: 100}).split(94_000); input != 1 || write != 0 || read != 93_999 {
		t.Fatalf("full coverage must keep one input token: %d/%d/%d", input, write, read)
	}
	if input, write, read := (kiroCacheUsage{total: 100}).split(500); input != 500 || write != 0 || read != 0 {
		t.Fatalf("no coverage: %d/%d/%d", input, write, read)
	}
}

func TestKiroCacheAutoBreakpointAdvancesAcrossTurns(t *testing.T) {
	withKiroCacheSimulation(t)
	ctx := WithPayloadRuleIdentity(context.Background(), &PayloadRuleIdentity{APIKeyID: 1})
	u1, a1, u2 := strings.Repeat("What is prompt caching? ", 50), strings.Repeat("Prompt caching reuses prefixes. ", 50), strings.Repeat("How does it advance? ", 20)
	auto := map[string]any{"type": "ephemeral", "ttl": "5m"}

	first := kiroComputeCacheUsage(ctx, kiroCacheTestRequest(t, map[string]any{"model": "m", "cache_control": auto,
		"messages": []any{map[string]any{"role": "user", "content": u1}}}))
	if first.covered == 0 || first.read != 0 || len(kiroCache.entries) != 1 {
		t.Fatalf("turn 1 = %+v entries=%d", first, len(kiroCache.entries))
	}
	second := kiroComputeCacheUsage(ctx, kiroCacheTestRequest(t, map[string]any{"model": "m", "cache_control": auto,
		"messages": []any{map[string]any{"role": "user", "content": u1}, map[string]any{"role": "assistant", "content": a1}, map[string]any{"role": "user", "content": u2}}}))
	if second.read != first.covered || second.covered <= second.read || second.total <= second.covered || len(kiroCache.entries) != 2 {
		t.Fatalf("turn 2 = %+v (turn 1 covered %d) entries=%d", second, first.covered, len(kiroCache.entries))
	}

	other := WithPayloadRuleIdentity(context.Background(), &PayloadRuleIdentity{APIKeyID: 2})
	if isolated := kiroComputeCacheUsage(other, kiroCacheTestRequest(t, map[string]any{"model": "m", "cache_control": auto,
		"messages": []any{map[string]any{"role": "user", "content": u1}}})); isolated.read != 0 {
		t.Fatalf("another key must not hit: %+v", isolated)
	}
}

func TestKiroCacheSkipsWithoutValidBreakpoints(t *testing.T) {
	withKiroCacheSimulation(t)
	block := func(ttl string) map[string]any {
		return map[string]any{"type": "text", "text": "hello world", "cache_control": map[string]any{"type": "ephemeral", "ttl": ttl}}
	}
	cases := map[string]map[string]any{
		"no cache_control": {"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
		"1h after 5m":      {"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{block("5m"), block("1h")}}}},
		"unknown ttl":      {"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{block("2h")}}}},
		"five breakpoints": {"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{block("5m"), block("5m"), block("5m"), block("5m"), block("5m")}}}},
	}
	for name, body := range cases {
		if usage := kiroComputeCacheUsage(context.Background(), kiroCacheTestRequest(t, body)); usage.covered != 0 || usage.total == 0 {
			t.Errorf("%s: %+v", name, usage)
		}
	}
	if len(kiroCache.entries) != 0 {
		t.Fatalf("invalid requests must not write entries: %d", len(kiroCache.entries))
	}
	SetKiroCacheSimulation(false)
	if usage := kiroComputeCacheUsage(context.Background(), kiroCacheTestRequest(t, map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{block("5m")}}}})); usage != (kiroCacheUsage{}) {
		t.Fatalf("disabled simulation must report nothing: %+v", usage)
	}
}
