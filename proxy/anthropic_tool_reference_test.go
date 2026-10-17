package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestAnthropicCodexToolReferenceHistory(t *testing.T) {
	for _, count := range []int{1, 3, 15} {
		t.Run(fmt.Sprintf("references=%d", count), func(t *testing.T) {
			tools := []any{map[string]any{
				"name": "ToolSearch", "input_schema": map[string]any{
					"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"},
				},
			}}
			var references []any
			var names []string
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("mcp__files__read_%d", i)
				if i == 0 {
					name = "Read"
				}
				names = append(names, name)
				references = append(references, map[string]any{"type": "tool_reference", "tool_name": name})
				tools = append(tools, map[string]any{
					"name": name, "defer_loading": true, "input_schema": map[string]any{
						"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []string{"file_path"},
					},
				})
			}
			content := references
			if count == 3 {
				content = append([]any{map[string]any{"type": "text", "text": "Found project tools."}}, references...)
				content = append(content,
					map[string]any{"type": "document", "source": map[string]any{"type": "text", "media_type": "text/plain", "data": "TOOL_DOCUMENT_CONTEXT"}},
					map[string]any{"type": "text", "text": "Use the discovered tools."},
				)
			}
			raw, err := json.Marshal(map[string]any{
				"model": "claude-sonnet-4-6", "stream": false, "max_tokens": 128, "tools": tools,
				"messages": []any{
					map[string]any{"role": "user", "content": "Find project tools."},
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_search", "name": "ToolSearch", "input": map[string]any{"query": "select:" + strings.Join(names, ",")}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_search", "content": content}}},
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_read", "name": "Read", "input": map[string]any{"file_path": "README.md"}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_read", "content": "Project instructions."}}},
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
			for path, want := range map[string]string{
				"input.#": "5", "input.1.name": "ToolSearch", "input.2.type": "function_call_output", "input.2.call_id": "fc_toolu_search",
				"input.3.name": "Read", "input.3.arguments": `{"file_path":"README.md"}`, "input.4.call_id": "fc_toolu_read", "input.4.output": "Project instructions.",
				"tools.0.parameters.properties.query.type": "string", "tools.0.parameters.required.0": "query",
			} {
				if got := gjson.GetBytes(out, path).String(); got != want {
					t.Errorf("%s = %q, want %q: %s", path, got, want, out)
				}
			}
			output := gjson.GetBytes(out, "input.2.output").String()
			var gotNames []string
			for _, line := range strings.Split(output, "\n") {
				if gjson.Valid(line) && gjson.Get(line, "type").String() == "tool_reference" {
					gotNames = append(gotNames, gjson.Get(line, "tool_name").String())
				}
			}
			if strings.Join(gotNames, ",") != strings.Join(names, ",") {
				t.Fatalf("tool references were lost or reordered: %s", output)
			}
			if count == 3 && (!strings.HasPrefix(output, "Found project tools.\n") || !strings.Contains(output, "TOOL_DOCUMENT_CONTEXT") || !strings.HasSuffix(output, "\nUse the discovered tools.")) {
				t.Fatalf("mixed tool result content was lost or reordered: %s", output)
			}
			declarations := gjson.GetBytes(out, "tools").Array()
			if len(declarations) != count+1 {
				t.Fatalf("discovered tool declarations were lost: %s", out)
			}
			for i, name := range names {
				tool := declarations[i+1]
				if tool.Get("name").String() != name || tool.Get("parameters.properties.file_path.type").String() != "string" || tool.Get("parameters.required.0").String() != "file_path" {
					t.Errorf("discovered tool schema changed: %s", tool.Raw)
				}
			}
			if !bytes.Equal(original, raw) {
				t.Fatal("translation mutated ingress bytes")
			}

			t.Run("messages_handler", func(t *testing.T) {
				previousRuntime := CurrentRuntimeSettings()
				runtime := previousRuntime
				runtime.CodexForceWebsocket = false
				ApplyRuntimeSettings(runtime)
				t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
				handler, calls := newAnthropicStreamFailureTestHandler(t, func(_ int32, w http.ResponseWriter) {
					writeCodexSSE(w, `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
				})
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
				ctx.Request.Header.Set("Content-Type", "application/json")
				handler.Messages(ctx)
				if recorder.Code != http.StatusOK || calls.Load() != 1 || gjson.GetBytes(recorder.Body.Bytes(), "content.0.text").String() != "ready" {
					t.Fatalf("tool discovery history failed at the HTTP handler: calls=%d status=%d body=%s", calls.Load(), recorder.Code, recorder.Body.String())
				}
			})
		})
	}
}

func TestAnthropicCodexRejectsInvalidToolReferences(t *testing.T) {
	for name, block := range map[string]string{
		"missing name": `{"type":"tool_reference"}`,
		"null name":    `{"type":"tool_reference","tool_name":null}`,
		"empty name":   `{"type":"tool_reference","tool_name":""}`,
		"blank name":   `{"type":"tool_reference","tool_name":" \t\n"}`,
		"numeric name": `{"type":"tool_reference","tool_name":42}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_search","name":"ToolSearch","input":{"query":"read"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_search","content":[` + block + `]}]}]}`)
			out, _, err := TranslateAnthropicToCodex(raw, "")
			if err == nil || out != nil || !strings.Contains(err.Error(), "tool_name") {
				t.Fatalf("invalid tool reference must identify its missing or invalid name: err=%v body=%s", err, out)
			}
		})
	}
	for _, role := range []string{"user", "assistant", "system", "developer"} {
		t.Run(role, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":%q,"content":[{"type":"tool_reference","tool_name":"Read"}]}]}`, role))
			out, _, err := TranslateAnthropicToCodex(raw, "")
			if err == nil || out != nil || !strings.Contains(err.Error(), `unsupported content block "tool_reference"`) {
				t.Fatalf("tool references must be restricted to tool results: err=%v body=%s", err, out)
			}
		})
	}
}
