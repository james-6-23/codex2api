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

func TestAnthropicCodexPreservesStructuredToolContent(t *testing.T) {
	for name, block := range map[string]string{
		"search":        `{"type":"search_result","source":"kb://project","title":"Project specification","content":[{"type":"text","text":"Required context."}],"citations":{"enabled":true}}`,
		"browser":       `{"type":"browser_state","tabs":[{"tab_id":"tab_1","title":"Specification","url":"https://example.com/spec","active":true},{"tab_id":"tab_2","title":"","url":""}],"state_changes":[{"type":"tab_opened","tab_id":"tab_2"}],"extra_metadata":{"preserve":"this"}}`,
		"empty browser": `{"type":"browser_state","tabs":[]}`,
	} {
		for _, nested := range []bool{false, true} {
			if !nested && name != "search" {
				continue
			}
			t.Run(fmt.Sprintf("%s/tool_result=%t", name, nested), func(t *testing.T) {
				messages := `[{"role":"user","content":[` + block + `]}]`
				path := "input.0.content.0.text"
				if nested {
					messages = `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":[` + block + `]}]}]`
					path = "input.1.output"
				}
				raw := []byte(`{"model":"claude-sonnet-4-6","messages":` + messages + `}`)
				original := bytes.Clone(raw)
				out, _, err := TranslateAnthropicToCodex(raw, "")
				if err != nil {
					t.Fatal(err)
				}
				var want, got any
				if err := json.Unmarshal([]byte(block), &want); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(gjson.GetBytes(out, path).String()), &got); err != nil {
					t.Fatalf("structured result is not JSON: %v body=%s", err, out)
				}
				wantJSON, _ := json.Marshal(want)
				gotJSON, _ := json.Marshal(got)
				if !bytes.Equal(wantJSON, gotJSON) || !bytes.Equal(original, raw) {
					t.Fatalf("structured data changed or ingress was mutated: %s", out)
				}
				if nested && (gjson.GetBytes(out, "input.1.type").String() != "function_call_output" || gjson.GetBytes(out, "input.1.call_id").String() != "fc_toolu_a") {
					t.Fatalf("structured tool result lost its call identity: %s", out)
				}
				var decoded anthropicContentBlock
				if err := json.Unmarshal([]byte(block), &decoded); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(decoded)
				if err != nil || !gjson.GetBytes(encoded, "type").Exists() {
					t.Fatalf("structured block cannot be reserialized: %v %s", err, encoded)
				}
			})
		}
	}
}

func TestMessagesCodexMixedStructuredToolContent(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-4-6","stream":false,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"browser_lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":[{"type":"text","text":"Lookup context."},{"type":"search_result","source":"kb://spec","title":"Spec","content":[{"type":"text","text":"Search context."}]},{"type":"browser_state","tabs":[]},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + tinyPNGBase64 + `"}}]}]}]}`)
	out, _, err := TranslateAnthropicToCodex(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	output := gjson.GetBytes(out, "input.1.output").String()
	for _, required := range []string{"Lookup context.", "search_result", "kb://spec", "Search context.", "browser_state"} {
		if !strings.Contains(output, required) {
			t.Fatalf("mixed content %q was lost: %s", required, out)
		}
	}
	if gjson.GetBytes(out, "input.2.content.1.image_url").String() != "data:image/png;base64,"+tinyPNGBase64 {
		t.Fatalf("mixed structured content lost its image: %s", out)
	}
	previous := CurrentRuntimeSettings()
	runtime := previous
	runtime.CodexForceWebsocket = false
	ApplyRuntimeSettings(runtime)
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	handler, calls := newAnthropicStreamFailureTestHandler(t, func(_ int32, w http.ResponseWriter) {
		writeCodexSSE(w, `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	handler.Messages(ctx)
	if recorder.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("structured history failed at the handler: calls=%d status=%d body=%s", calls.Load(), recorder.Code, recorder.Body.String())
	}
}

func TestAnthropicCodexRejectsInvalidStructuredContent(t *testing.T) {
	for name, block := range map[string]string{
		"missing source":  `{"type":"search_result","title":"Spec","content":[{"type":"text","text":"ok"}]}`,
		"object source":   `{"type":"search_result","source":{},"title":"Spec","content":[]}`,
		"missing title":   `{"type":"search_result","source":"kb://spec","content":[]}`,
		"string content":  `{"type":"search_result","source":"kb://spec","title":"Spec","content":"must not disappear"}`,
		"non-text search": `{"type":"search_result","source":"kb://spec","title":"Spec","content":[{"type":"image"}]}`,
		"missing tabs":    `{"type":"browser_state"}`,
		"null tabs":       `{"type":"browser_state","tabs":null}`,
		"missing tab id":  `{"type":"browser_state","tabs":[{"title":"Spec","url":"https://example.com","active":true}]}`,
		"no active tab":   `{"type":"browser_state","tabs":[{"tab_id":"1","title":"Spec","url":"https://example.com"}]}`,
		"two active tabs": `{"type":"browser_state","tabs":[{"tab_id":"1","title":"A","url":"","active":true},{"tab_id":"2","title":"B","url":"","active":true}]}`,
		"bad changes":     `{"type":"browser_state","tabs":[],"state_changes":"must not disappear"}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":[` + block + `]}]}]}`)
			if out, _, err := TranslateAnthropicToCodex(raw, ""); err == nil || out != nil {
				t.Fatalf("malformed structured content must fail explicitly: err=%v body=%s", err, out)
			}
		})
	}
	for _, role := range []string{"assistant", "system", "developer"} {
		for _, block := range []string{`{"type":"search_result","source":"kb://spec","title":"Spec","content":[]}`, `{"type":"browser_state","tabs":[]}`} {
			raw := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":%q,"content":[%s]}]}`, role, block))
			if out, _, err := TranslateAnthropicToCodex(raw, ""); err == nil || out != nil {
				t.Fatalf("structured content must respect role restrictions: role=%s err=%v body=%s", role, err, out)
			}
		}
	}
	raw := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"browser_state","tabs":[]}]}]}`)
	if out, _, err := TranslateAnthropicToCodex(raw, ""); err == nil || out != nil || !strings.Contains(err.Error(), "browser_state") {
		t.Fatalf("browser state belongs inside a tool result: err=%v body=%s", err, out)
	}
}
