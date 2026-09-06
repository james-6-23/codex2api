package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkResponsesPreparePipeline(b *testing.B) {
	tools := make([]any, 24)
	for i := range tools {
		tools[i] = map[string]any{"type": "function", "name": fmt.Sprintf("tool_%d", i), "description": strings.Repeat("description ", 48), "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}}
	}
	fixtures := map[string]map[string]any{
		"bootstrap":  {"model": "gpt-6-astra", "input": []any{map[string]any{"type": "additional_tools", "tools": tools}, map[string]any{"role": "user", "content": strings.Repeat("context ", 2048)}}, "store": false},
		"tool_delta": {"model": "gpt-6-astra", "previous_response_id": "resp_previous", "input": []any{map[string]any{"type": "custom_tool_call_output", "call_id": "call_test", "output": strings.Repeat("tool result ", 2048)}}, "store": false},
	}
	for name, value := range fixtures {
		raw, err := json.Marshal(value)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				_, _ = PrepareResponsesWebSocketBody(raw)
			}
		})
	}
}

func BenchmarkResponsesTerminalOutputRestore(b *testing.B) {
	attribution := make(map[string]any, 1024)
	for i := 0; i < 1024; i++ {
		attribution[fmt.Sprintf("item_%04d", i)] = map[string]any{"input_tokens": 100, "cached_tokens": 80, "output_tokens": 5}
	}
	outputs := []json.RawMessage{
		json.RawMessage(`{"type":"message","id":"msg_test","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"done"}]}`),
		json.RawMessage(`{"type":"custom_tool_call","id":"ctc_test","call_id":"call_test","name":"exec","input":"print(1)"}`),
	}
	for _, complete := range []bool{false, true} {
		items := []json.RawMessage{}
		if complete {
			items = outputs
		}
		raw, err := json.Marshal(map[string]any{"id": "resp_test", "usage": map[string]any{"input_tokens": 102400, "attribution": map[string]any{"items": attribution}}, "output": items})
		if err != nil {
			b.Fatal(err)
		}
		name := "empty"
		if complete {
			name = "already_complete"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				_ = restoreMissingResponseOutputs(raw, outputs)
			}
		})
	}
}
