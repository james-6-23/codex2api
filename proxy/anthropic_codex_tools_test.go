package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAnthropicCodexLargeToolCatalogRetainsActiveTools(t *testing.T) {
	var tools []any
	for i := 0; i < maxTools+32; i++ {
		tools = append(tools, map[string]any{"name": fmt.Sprintf("lookup_%03d", i), "defer_loading": i != 0, "input_schema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}})
	}
	raw, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6", "tools": tools, "tool_choice": map[string]any{"type": "tool", "name": "lookup_159"},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_a", "name": "lookup_150", "input": map[string]any{"query": "project"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_a", "content": []any{map[string]any{"type": "tool_reference", "tool_name": "lookup_155"}}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(raw)
	out, _, err := TranslateAnthropicToCodex(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	declarations := gjson.GetBytes(out, "tools").Array()
	if len(declarations) != maxTools {
		t.Fatalf("tool capacity changed unexpectedly: %d", len(declarations))
	}
	names := make(map[string]bool)
	previous := ""
	for _, tool := range declarations {
		name := tool.Get("name").String()
		if name <= previous || tool.Get("parameters.properties.query.type").String() != "string" || tool.Get("parameters.required.0").String() != "query" {
			t.Fatalf("tool order or schema changed: %s", tool.Raw)
		}
		previous = name
		names[name] = true
	}
	for _, name := range []string{"lookup_000", "lookup_150", "lookup_155", "lookup_159"} {
		if !names[name] {
			t.Errorf("active tool %s was truncated", name)
		}
	}
	if gjson.GetBytes(out, "tool_choice.name").String() != "lookup_159" {
		t.Fatalf("named tool selection changed: %s", out)
	}
	prepared, _ := PrepareResponsesBody(out)
	preparedNames := make(map[string]bool)
	for _, tool := range gjson.GetBytes(prepared, "tools").Array() {
		preparedNames[tool.Get("name").String()] = true
	}
	for name := range names {
		if !preparedNames[name] {
			t.Errorf("upstream preparation displaced tool %s", name)
		}
	}
	repeated, _, err := TranslateAnthropicToCodex(raw, "")
	if err != nil || !bytes.Equal(out, repeated) || !bytes.Equal(original, raw) {
		t.Fatal("tool selection was unstable or mutated ingress bytes")
	}
}

func TestAnthropicCodexRejectsActiveToolOverflow(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			var tools, references []any
			for i := 0; i <= maxTools; i++ {
				name := fmt.Sprintf("lookup_%03d", i)
				tools = append(tools, map[string]any{"name": name, "defer_loading": deferred})
				references = append(references, map[string]any{"type": "tool_reference", "tool_name": name})
			}
			raw, err := json.Marshal(map[string]any{"model": "claude-sonnet-4-6", "tools": tools, "messages": []any{
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_a", "name": "lookup_000", "input": map[string]any{}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_a", "content": references}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := TranslateAnthropicToCodex(raw, "")
			if err == nil || out != nil || !strings.Contains(err.Error(), "128") || !strings.Contains(err.Error(), "active tools") {
				t.Fatalf("active tools must not be silently truncated: err=%v body_bytes=%d", err, len(out))
			}
		})
	}
}
