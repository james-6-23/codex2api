package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestAnthropicCodexTransportMetadataKeepsStablePrefix(t *testing.T) {
	for _, systemArray := range []bool{false, true} {
		t.Run(fmt.Sprintf("system_array=%t", systemArray), func(t *testing.T) {
			var first []byte
			for _, nonce := range []string{"first", "second"} {
				billing := "x-anthropic-billing-header: cc_version=2.1.261." + nonce + "; cch=" + nonce + ";"
				context := "Follow CLAUDE.md.\nWorking directory: D:/project\nKeep the user's instructions."
				var system any = billing + "\n" + context
				if systemArray {
					system = []any{map[string]any{"type": "text", "text": billing}, map[string]any{"type": "text", "text": context}}
				}
				raw, err := json.Marshal(map[string]any{
					"model": "claude-sonnet-4-6", "system": system,
					"messages": []any{map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "text", "text": "<system-reminder>\nYou are powered by the model " + nonce + ".\n</system-reminder>"},
						map[string]any{"type": "text", "text": "<system-reminder>\nYou are powered by the model " + nonce + ".\n# Environment\nGit status: modified\nToday's date: 2026-10-17\nAvailable skills: project-build\n</system-reminder>"},
						map[string]any{"type": "text", "text": "Implement the feature."},
					}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				original := bytes.Clone(raw)
				out, _, err := TranslateAnthropicToCodex(raw, "")
				if err != nil {
					t.Fatal(err)
				}
				input := gjson.GetBytes(out, "input").Raw
				if strings.Contains(input, "x-anthropic-billing-header:") || strings.Contains(input, "You are powered by the model") {
					t.Fatalf("transport metadata reached Codex: %s", input)
				}
				for _, required := range []string{"Follow CLAUDE.md.", "D:/project", "Git status: modified", "2026-10-17", "project-build", "Implement the feature."} {
					if !strings.Contains(input, required) {
						t.Fatalf("necessary context %q was removed: %s", required, input)
					}
				}
				if !bytes.Equal(original, raw) {
					t.Fatal("normalization mutated ingress bytes")
				}
				if first == nil {
					first = out
				} else if !bytes.Equal(first, out) {
					t.Fatalf("transport-only changes changed the outgoing prefix:\n%s\n%s", first, out)
				}
			}
		})
	}
}

func TestAnthropicCodexPreservesQuotedMetadataAndToolPayloads(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":"Review this literal example:\n~~~\nx-anthropic-billing-header: cc_version=2.1.261.example;\n~~~",
		"messages":[
			{"role":"user","content":"x-anthropic-billing-header: cc_version=2.1.261.user;"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"custom","input":{"literal":"<system-reminder>\nYou are powered by the model example.\n</system-reminder>"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"x-anthropic-billing-header: cc_version=2.1.261.result;"}]}
		]
	}`)
	out, _, err := TranslateAnthropicToCodex(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"cc_version=2.1.261.example;", "cc_version=2.1.261.user;", "You are powered by the model example.", "cc_version=2.1.261.result;"} {
		if !strings.Contains(string(out), required) {
			t.Fatalf("quoted or structured payload %q was removed: %s", required, out)
		}
	}
}

func TestAnthropicCodexPreservesInstructionMessages(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		for _, blocks := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/blocks=%t", role, blocks), func(t *testing.T) {
				var first []byte
				for _, nonce := range []string{"first", "second"} {
					instruction := "x-anthropic-billing-header: cc_version=2.1.261.test; cch=" + nonce + ";\nKeep the project instructions."
					var content any = instruction
					if blocks {
						content = []any{map[string]any{"type": "text", "text": instruction}, map[string]any{"type": "text", "text": "Working directory: D:/project"}}
					}
					raw, err := json.Marshal(map[string]any{
						"model": "claude-sonnet-4-6", "system": "Top level instructions.",
						"messages": []any{
							map[string]any{"role": "user", "content": "First user turn."},
							map[string]any{"role": role, "content": content},
							map[string]any{"role": "assistant", "content": "Previous answer."},
							map[string]any{"role": "user", "content": "Continue the task."},
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
					items := gjson.GetBytes(out, "input").Array()
					if len(items) != 5 {
						t.Fatalf("message was dropped or reordered: %s", out)
					}
					for i, expected := range []struct{ role, text string }{
						{"developer", "Top level instructions."}, {"user", "First user turn."},
						{"developer", "Keep the project instructions."}, {"assistant", "Previous answer."}, {"user", "Continue the task."},
					} {
						if items[i].Get("role").String() != expected.role || !strings.Contains(items[i].Get("content.0.text").String(), expected.text) {
							t.Fatalf("message %d lost its role or context: %s", i, out)
						}
					}
					if blocks && !strings.Contains(items[2].Raw, "D:/project") {
						t.Fatalf("instruction block was lost: %s", out)
					}
					if strings.Contains(string(out), "x-anthropic-billing-header:") || !bytes.Equal(original, raw) {
						t.Fatalf("transport normalization failed or mutated ingress: %s", out)
					}
					if first == nil {
						first = out
					} else if !bytes.Equal(first, out) {
						t.Fatalf("instruction metadata changed the cache prefix:\n%s\n%s", first, out)
					}
				}
			})
		}
	}
}

func TestAnthropicCodexPreservesTextDocuments(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("tool_result=%t", nested), func(t *testing.T) {
			document := `{"type":"document","title":"Invoice","context":"Review the total","source":{"type":"text","media_type":"text/plain","data":"INVOICE_TOTAL=900"}}`
			messages := `[{"role":"user","content":[` + document + `]}]`
			if nested {
				messages = `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":[` + document + `]}]}]`
			}
			out, _, err := TranslateAnthropicToCodex([]byte(`{"model":"claude-sonnet-4-6","messages":`+messages+`}`), "")
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range []string{"Invoice", "Review the total", "INVOICE_TOTAL=900"} {
				if !strings.Contains(string(out), required) {
					t.Fatalf("document data %q was lost: %s", required, out)
				}
			}
		})
	}
}

func TestAnthropicCodexPreservesUserImage(t *testing.T) {
	raw := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}`, tinyPNGBase64))
	out, _, err := TranslateAnthropicToCodex(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.image_url").String(); got != "data:image/png;base64,"+tinyPNGBase64 {
		t.Fatalf("image data was lost or changed: %s", out)
	}
}

func TestAnthropicCodexAcceptsRedactedThinkingHistory(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":"Inspect the project."},
			{"role":"assistant","content":[
				{"type":"redacted_thinking","data":"opaque-claude-reasoning"},
				{"type":"text","text":"I will inspect the project."},
				{"type":"tool_use","id":"toolu_a","name":"read","input":{"path":"README.md"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"Project instructions."}]}
		]
	}`)
	original := bytes.Clone(raw)
	out, _, err := TranslateAnthropicToCodex(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"input.#":                "4",
		"input.0.content.0.text": "Inspect the project.",
		"input.1.role":           "assistant",
		"input.1.content.0.text": "I will inspect the project.",
		"input.2.type":           "function_call",
		"input.2.call_id":        "fc_toolu_a",
		"input.2.name":           "read",
		"input.2.arguments":      `{"path":"README.md"}`,
		"input.3.type":           "function_call_output",
		"input.3.call_id":        "fc_toolu_a",
		"input.3.output":         "Project instructions.",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q, want %q: %s", path, got, want, out)
		}
	}
	if strings.Contains(string(out), "redacted_thinking") || strings.Contains(string(out), "opaque-claude-reasoning") {
		t.Fatalf("Claude-only encrypted reasoning reached Codex: %s", out)
	}
	if !bytes.Equal(original, raw) {
		t.Fatal("translation mutated ingress bytes")
	}
}

func TestAnthropicCodexRejectsNonAssistantRedactedThinking(t *testing.T) {
	block := `{"type":"redacted_thinking","data":"opaque-claude-reasoning"}`
	for _, role := range []string{"user", "system", "developer", "tool_result"} {
		t.Run(role, func(t *testing.T) {
			messages := fmt.Sprintf(`[{"role":%q,"content":[%s]}]`, role, block)
			if role == "tool_result" {
				messages = `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":[` + block + `]}]}]`
			}
			out, _, err := TranslateAnthropicToCodex([]byte(`{"model":"claude-sonnet-4-6","messages":`+messages+`}`), "")
			if err == nil || out != nil || !strings.Contains(err.Error(), "unsupported content block") {
				t.Fatalf("non-assistant redacted thinking must fail explicitly: err=%v body=%s", err, out)
			}
		})
	}
}

func TestAnthropicCodexRejectsInvalidMessageRoles(t *testing.T) {
	for _, role := range []string{"", "tool", "unexpected_role"} {
		t.Run(role, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Start."},{"role":%q,"content":"Must not disappear."}]}`, role))
			out, _, err := TranslateAnthropicToCodex(raw, "")
			if out != nil || err == nil || !strings.Contains(err.Error(), "messages[1]") || !strings.Contains(err.Error(), fmt.Sprintf("role %q", role)) {
				t.Fatalf("invalid role must produce an actionable error: err=%v body=%s", err, out)
			}
		})
	}
	for _, role := range []string{"system", "developer"} {
		raw := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":%q,"content":[{"type":"tool_use","id":"call_a","name":"run","input":{}}]}]}`, role))
		if out, _, err := TranslateAnthropicToCodex(raw, ""); err == nil || out != nil {
			t.Fatalf("instruction roles must not drop non-text content: role=%s err=%v body=%s", role, err, out)
		}
	}
}

func TestAnthropicCodexRejectsUnsupportedContent(t *testing.T) {
	for name, block := range map[string]string{
		"missing image source":     `{"type":"image"}`,
		"null image source":        `{"type":"image","source":null}`,
		"empty image source":       `{"type":"image","source":{}}`,
		"URL image":                `{"type":"image","source":{"type":"url","url":"https://example.com/image.png"}}`,
		"non-base64 image":         `{"type":"image","source":{"type":"text","media_type":"image/png","data":"AAAB"}}`,
		"missing image media type": `{"type":"image","source":{"type":"base64","data":"AAAB"}}`,
		"blank image media type":   `{"type":"image","source":{"type":"base64","media_type":" ","data":"AAAB"}}`,
		"empty image data":         `{"type":"image","source":{"type":"base64","media_type":"image/png","data":""}}`,
		"blank image data":         `{"type":"image","source":{"type":"base64","media_type":"image/png","data":" \t\n"}}`,
		"missing document source":  `{"type":"document"}`,
		"PDF":                      `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}`,
		"URL":                      `{"type":"document","source":{"type":"url","url":"https://example.com/file.pdf"}}`,
		"unknown block":            `{"type":"future_attachment","data":"must not disappear"}`,
	} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tool_result=%t", name, nested), func(t *testing.T) {
				messages := `[{"role":"user","content":[` + block + `]}]`
				if nested {
					messages = `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":[` + block + `]}]}]`
				}
				out, _, err := TranslateAnthropicToCodex([]byte(`{"model":"claude-sonnet-4-6","messages":`+messages+`}`), "")
				if err == nil || out != nil {
					t.Fatalf("unsupported input must fail explicitly: err=%v body=%s", err, out)
				}
			})
		}
	}
}

func TestAnthropicCodexPreservesServerSearchHistory(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			completed := []byte(fmt.Sprintf(`{"response":{"output":[{"type":"web_search_call","id":"ws_history","status":%q,"action":{"query":"project specification"},"results":[{"url":"https://example.com/spec","title":"Specification","encrypted_content":"opaque-search-data"}]}]}}`, status))
			response := buildAnthropicResponseFromCompleted(completed, "claude-sonnet-4-6")
			history, err := json.Marshal(response.Content)
			if err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Search for the specification."},{"role":"assistant","content":` + string(history) + `},{"role":"user","content":"Check the cited source."}]}`)
			out, _, err := TranslateAnthropicToCodex(raw, "")
			if err != nil {
				t.Fatal(err)
			}
			input := gjson.GetBytes(out, "input").Raw
			for _, block := range response.Content {
				if !strings.Contains(input, block.Type) {
					t.Fatalf("search history block %q disappeared: %s", block.Type, out)
				}
			}
			if status == "completed" {
				for _, required := range []string{"project specification", "https://example.com/spec", "Specification", "opaque-search-data"} {
					if !strings.Contains(input, required) {
						t.Fatalf("search history data %q disappeared: %s", required, out)
					}
				}
			} else if !strings.Contains(input, "web_search_tool_result_error") {
				t.Fatalf("search failure became a success: %s", out)
			}
			gjson.GetBytes(out, "input").ForEach(func(_, item gjson.Result) bool {
				if item.Get("type").String() == "function_call" || item.Get("type").String() == "function_call_output" {
					t.Fatal("hosted search history must not become a client tool invocation")
				}
				return true
			})
			var workers sync.WaitGroup
			for i := 0; i < 16; i++ {
				workers.Go(func() {
					repeated, _, err := TranslateAnthropicToCodex(raw, "")
					if err != nil || !bytes.Equal(out, repeated) {
						t.Errorf("concurrent translation changed: err=%v body=%s", err, repeated)
					}
				})
			}
			workers.Wait()
		})
	}
}

func TestMessagesCodexUnsupportedContentReturns400(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 913422, AccessToken: "test-token", Status: auth.StatusReady})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	previousRuntime, previousWS := CurrentRuntimeSettings(), WebsocketExecuteFunc
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime); WebsocketExecuteFunc = previousWS })
	runtime := previousRuntime
	runtime.CodexForceWebsocket = true
	ApplyRuntimeSettings(runtime)
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		t.Error("unsupported content reached upstream")
		return nil, fmt.Errorf("unsupported content reached upstream")
	}
	for contentType, block := range map[string]string{
		"document": `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}`,
		"image":    `{"type":"image"}`,
	} {
		t.Run(contentType, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[`+block+`]}]}`))
			handler.Messages(c)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), contentType) {
				t.Fatalf("expected explicit %s error: status=%d body=%s", contentType, recorder.Code, recorder.Body.String())
			}
		})
	}
}
