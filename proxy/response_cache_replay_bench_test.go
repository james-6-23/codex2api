package proxy

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func replayCacheFixture() [][]json.RawMessage {
	turns := make([][]json.RawMessage, 128)
	items := make([]json.RawMessage, 128)
	for i := range items {
		arguments, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", 10<<10)})
		items[i], _ = json.Marshal(map[string]string{"type": "function_call", "call_id": fmt.Sprintf("call_%d", i), "name": "read", "arguments": string(arguments)})
	}
	for i := range turns {
		turns[i] = items[:i+1]
	}
	return turns
}

func BenchmarkResponseCacheReplay128Turns(b *testing.B) {
	turns := replayCacheFixture()
	config := defaultResponseCacheConfig()
	config.maxBytes = 256 << 20
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetResponseCacheStateForTest(config)
		for turn, items := range turns {
			id := fmt.Sprintf("resp_%d", turn)
			setResponseCache("replay-bench", id, items)
			result := getResponseCacheForReplay("replay-bench", id)
			runtime.KeepAlive(result)
		}
	}
	b.StopTimer()
	resetResponseCacheStateForTest(defaultResponseCacheConfig())
}
