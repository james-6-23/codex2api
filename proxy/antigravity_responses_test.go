package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestAntigravityResponsesReasoningEffortMapsToSafeThinkingBudget(t *testing.T) {
	for _, test := range []struct {
		effort string
		budget int
	}{
		{effort: "none", budget: 4096},
		{effort: "minimal", budget: 4096},
		{effort: "low", budget: 4096},
		{effort: "medium", budget: 8192},
		{effort: "high", budget: 24576},
		{effort: "xhigh", budget: 24576},
		{effort: "ultra", budget: 24576},
	} {
		t.Run(test.effort, func(t *testing.T) {
			got, err := responsesToGeminiInternal([]byte(`{"input":"hello","max_output_tokens":32,"reasoning":{"effort":"`+test.effort+`","summary":"auto"}}`), "project", "gemini-3.7-flash-tiered")
			if err != nil {
				t.Fatal(err)
			}
			request := got["request"].(map[string]any)
			config := request["generationConfig"].(map[string]any)
			thinking := config["thinkingConfig"].(map[string]any)
			if thinking["thinkingBudget"] != test.budget || thinking["includeThoughts"] != true {
				t.Fatalf("thinkingConfig = %#v, want budget=%d includeThoughts=true", thinking, test.budget)
			}
			if _, exists := config["maxOutputTokens"]; exists {
				t.Fatalf("Gemini maxOutputTokens must be omitted: %#v", config)
			}
		})
	}
}

func TestAntigravityResponsesLogicalModelsSelectBackingAndBudgetByEffort(t *testing.T) {
	for _, test := range []struct {
		model  string
		effort string
		wire   string
		budget int
	}{
		{model: "gemini-3.5-flash", effort: "none", wire: "gemini-3.5-flash-extra-low", budget: 1000},
		{model: "gemini-3.5-flash", effort: "medium", wire: "gemini-3.5-flash-low", budget: 4000},
		{model: "gemini-3.5-flash", effort: "max", wire: "gemini-3-flash-agent", budget: 10000},
		{model: "gemini-3.6-flash", effort: "minimal", wire: "gemini-3.6-flash-low", budget: 4096},
		{model: "gemini-3.6-flash", effort: "medium", wire: "gemini-3.6-flash-medium", budget: 8192},
		{model: "gemini-3.6-flash", effort: "xhigh", wire: "gemini-3.6-flash-high", budget: 24576},
		{model: "gemini-3.7-flash", effort: "low", wire: "gemini-3.7-flash-tiered", budget: 4096},
		{model: "gemini-3.7-flash", effort: "medium", wire: "gemini-3.7-flash-tiered", budget: 8192},
		{model: "gemini-3.7-flash", effort: "ultra", wire: "gemini-3.7-flash-tiered", budget: 24576},
		{model: "gemini-3.1-pro", effort: "low", wire: "gemini-3.1-pro-low", budget: 1001},
		{model: "gemini-3.1-pro", effort: "high", wire: "gemini-pro-agent", budget: 10001},
		{model: "gemini-3.1-pro", effort: "medium", wire: "gemini-pro-agent", budget: 10001},
	} {
		t.Run(test.model+"/"+test.effort, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"input":"hello","reasoning":{"effort":%q}}`, test.effort))
			got, err := responsesToGeminiInternal(body, "project", test.model)
			if err != nil {
				t.Fatal(err)
			}
			if got["model"] != test.wire {
				t.Fatalf("wire model = %v, want %s", got["model"], test.wire)
			}
			config := got["request"].(map[string]any)["generationConfig"].(map[string]any)
			thinking := config["thinkingConfig"].(map[string]any)
			if thinking["thinkingBudget"] != test.budget {
				t.Fatalf("thinkingConfig = %#v, want budget=%d", thinking, test.budget)
			}
		})
	}

	for _, test := range []struct {
		model, wire string
		budget      int
	}{
		{model: "gemini-3.5-flash", wire: "gemini-3.5-flash-low", budget: 4000},
		{model: "gemini-3.6-flash", wire: "gemini-3.6-flash-medium", budget: 8192},
		{model: "gemini-3.7-flash", wire: "gemini-3.7-flash-tiered", budget: 8192},
		{model: "gemini-3.1-pro", wire: "gemini-pro-agent", budget: 10001},
	} {
		t.Run(test.model+"/default", func(t *testing.T) {
			got, err := responsesToGeminiInternal([]byte(`{"input":"hello"}`), "project", test.model)
			if err != nil {
				t.Fatal(err)
			}
			if got["model"] != test.wire {
				t.Fatalf("wire model = %v, want %s", got["model"], test.wire)
			}
			config := got["request"].(map[string]any)["generationConfig"].(map[string]any)
			thinking := config["thinkingConfig"].(map[string]any)
			if thinking["thinkingBudget"] != test.budget {
				t.Fatalf("thinkingConfig = %#v, want budget=%d", thinking, test.budget)
			}
		})
	}
}

func TestAntigravityResponsesUsesOfficialEnvelopeWithoutForcedCredits(t *testing.T) {
	got, err := responsesToGeminiInternal([]byte(`{"input":"hello"}`), "project", "gemini-3.7-flash-low")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := got["enabledCreditTypes"]; exists {
		t.Fatalf("Gemini request forced Google One credits: %#v", got)
	}
	if got["userAgent"] != antigravityOfficialBodyUserAgent {
		t.Fatalf("userAgent = %#v", got["userAgent"])
	}
	request := got["request"].(map[string]any)
	if !isAntigravitySessionID(request["sessionId"]) {
		t.Fatalf("sessionId = %#v", request["sessionId"])
	}
	requestID, _ := got["requestId"].(string)
	parts := strings.Split(requestID, "/")
	if len(parts) != 3 || parts[0] != "agent" || len(parts[2]) != 8 {
		t.Fatalf("requestId = %q", requestID)
	}
}

func TestAntigravityResponsesReasoningUsesVariantSpecificBudgets(t *testing.T) {
	for _, test := range []struct {
		model  string
		effort string
		budget int
		max    int
		wire   string
	}{
		{model: "gemini-3.5-flash-low", effort: "high", budget: 1000, max: 65536, wire: "gemini-3.5-flash-extra-low"},
		{model: "gemini-3.5-flash-medium", effort: "low", budget: 4000, max: 65536, wire: "gemini-3.5-flash-low"},
		{model: "gemini-3.5-flash-high", effort: "low", budget: 10000, max: 65536, wire: "gemini-3-flash-agent"},
		{model: "gemini-3.6-flash-low", effort: "high", budget: 4096, max: 65535, wire: "gemini-3.6-flash-low"},
		{model: "gemini-3.6-flash-medium", effort: "low", budget: 8192, max: 65535, wire: "gemini-3.6-flash-medium"},
		{model: "gemini-3.6-flash-high", effort: "low", budget: 24576, max: 65535, wire: "gemini-3.6-flash-high"},
		{model: "gemini-3.7-flash-low", effort: "high", budget: 4096, max: 65535, wire: "gemini-3.7-flash-tiered"},
		{model: "gemini-3.7-flash-medium", effort: "low", budget: 8192, max: 65535, wire: "gemini-3.7-flash-tiered"},
		{model: "gemini-3.7-flash-high", effort: "low", budget: 24576, max: 65535, wire: "gemini-3.7-flash-tiered"},
		{model: "gemini-3.1-pro-low", effort: "high", budget: 1001, max: 65535, wire: "gemini-3.1-pro-low"},
		{model: "gemini-3.1-pro-high", effort: "low", budget: 10001, max: 65535, wire: "gemini-pro-agent"},
	} {
		t.Run(test.model+"/"+test.effort, func(t *testing.T) {
			for _, request := range []struct {
				name string
				body string
			}{
				{name: "model-default", body: `{"input":"hello","max_output_tokens":32}`},
				{name: "conflicting-body-effort", body: `{"input":"hello","max_output_tokens":32,"reasoning":{"effort":"` + test.effort + `"}}`},
				{name: "oversized-output-cap", body: `{"input":"hello","max_output_tokens":999999}`},
			} {
				t.Run(request.name, func(t *testing.T) {
					got, err := responsesToGeminiInternal([]byte(request.body), "project", test.model)
					if err != nil {
						t.Fatal(err)
					}
					config := got["request"].(map[string]any)["generationConfig"].(map[string]any)
					thinking := config["thinkingConfig"].(map[string]any)
					if thinking["thinkingBudget"] != test.budget {
						t.Fatalf("thinkingConfig = %#v, want budget=%d", thinking, test.budget)
					}
					if _, exists := config["maxOutputTokens"]; exists {
						t.Fatalf("Gemini maxOutputTokens must be omitted: %#v", config)
					}
					if got["model"] != test.wire {
						t.Fatalf("wire model = %v, want %s", got["model"], test.wire)
					}
				})
			}
		})
	}
}

func TestAntigravityResponsesNonGeminiModelsDoNotExposeThinkingControls(t *testing.T) {
	for _, model := range []string{
		"claude-opus-4-6-thinking",
		"claude-sonnet-4-6",
		"gpt-oss-120b-medium",
	} {
		t.Run(model, func(t *testing.T) {
			got, err := responsesToGeminiInternal([]byte(`{"input":"hello","reasoning":{"effort":"high"}}`), "project", model)
			if err != nil {
				t.Fatal(err)
			}
			request := got["request"].(map[string]any)
			if config, ok := request["generationConfig"].(map[string]any); ok {
				if _, exists := config["thinkingConfig"]; exists {
					t.Fatalf("non-Gemini model received thinkingConfig: %#v", config)
				}
			}
			if got["model"] != model {
				t.Fatalf("wire model = %v, want %s", got["model"], model)
			}
		})
	}
}

func TestAntigravityResponsesNonGeminiPreservesMaxOutputTokens(t *testing.T) {
	got, err := responsesToGeminiInternal([]byte(`{"input":"hello","max_output_tokens":128}`), "project", "claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	config := got["request"].(map[string]any)["generationConfig"].(map[string]any)
	if config["maxOutputTokens"] != 128 {
		t.Fatalf("generationConfig = %#v", config)
	}
}

func TestAntigravityResponsesConvertsFunctionDeclarations(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "true")
	got, err := responsesToGeminiInternal([]byte(`{
		"input":"hello",
		"tools":[{
			"type":"function",
			"name":"lookup",
			"description":"Look up a value",
			"strict":true,
			"parameters":{
				"type":"object",
				"additionalProperties":false,
				"$defs":{"query":{"type":"string","format":"uri"}},
				"properties":{"query":{"$ref":"#/$defs/query"}},
				"required":["query"]
			}
		}]
	}`), "project", "gemini-3-flash-agent")
	if err != nil {
		t.Fatal(err)
	}
	request := got["request"].(map[string]any)
	tools := request["tools"].([]any)
	declarations := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(declarations) != 1 {
		t.Fatalf("functionDeclarations = %#v", declarations)
	}
	declaration := declarations[0].(map[string]any)
	if declaration["name"] != "lookup" || declaration["description"] != "Look up a value" {
		t.Fatalf("declaration = %#v", declaration)
	}
	parameters := declaration["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	query := properties["query"].(map[string]any)
	if parameters["type"] != "OBJECT" || query["type"] != "STRING" {
		t.Fatalf("parameters = %#v", parameters)
	}
	if _, ok := parameters["additionalProperties"]; ok {
		t.Fatalf("unsupported additionalProperties survived: %#v", parameters)
	}
	if mode := request["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]; mode != "VALIDATED" {
		t.Fatalf("function calling mode = %#v", mode)
	}
}

func TestAntigravityResponsesConvertsFunctionCallRoundTripInput(t *testing.T) {
	got, err := responsesToGeminiInternal([]byte(`{
		"input":[
			{"type":"message","role":"user","content":"look it up"},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"query\":\"value\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"found"}
		]
	}`), "project", "gemini-3-flash-agent")
	if err != nil {
		t.Fatal(err)
	}
	contents := got["request"].(map[string]any)["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %#v", contents)
	}
	functionPart := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	functionCall := functionPart["functionCall"].(map[string]any)
	if contents[1].(map[string]any)["role"] != "model" || functionCall["name"] != "lookup" || functionCall["id"] != "call_1" {
		t.Fatalf("function call content = %#v", contents[1])
	}
	if functionPart["thoughtSignature"] != "skip_thought_signature_validator" || functionPart["thought_signature"] != "skip_thought_signature_validator" {
		t.Fatalf("thinking signature sentinel missing: %#v", functionPart)
	}
	response := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if contents[2].(map[string]any)["role"] != "user" || response["name"] != "lookup" || response["id"] != "call_1" || response["response"].(map[string]any)["result"] != "found" {
		t.Fatalf("function response content = %#v", contents[2])
	}
}

func TestAntigravityResponsesLargeMixedToolsKeepOrdinaryHiUsable(t *testing.T) {
	tools := []any{
		map[string]any{"type": "web_search_preview"},
		map[string]any{"type": "computer_use_preview"},
		map[string]any{"type": "image_generation"},
	}
	for index := 0; index < 96; index++ {
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        fmt.Sprintf("tool_%03d", index),
			"description": strings.Repeat("large Codex tool description ", 32),
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{"type": "string", "description": strings.Repeat("input ", 64)},
				},
			},
		})
	}
	body, err := json.Marshal(map[string]any{"input": "hi", "tools": tools})
	if err != nil {
		t.Fatal(err)
	}
	got, err := responsesToGeminiInternal(body, "project", "gemini-2.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	request := got["request"].(map[string]any)
	if _, ok := request["tools"]; ok {
		t.Fatalf("tool-disabled Flash model received function declarations: %#v", request["tools"])
	}
	contents := request["contents"].([]any)
	if len(contents) != 1 || extractGeminiRequestText(contents[0]) != "hi" {
		t.Fatalf("ordinary input was not preserved: %#v", contents)
	}
}

// Silently dropping declarations still ships the tool-describing system
// instruction upstream, so the model answers with a call it was never allowed
// to declare and the turn dies as MALFORMED_FUNCTION_CALL (issue #595).
func TestAntigravityResponsesForwardsFunctionToolsByDefault(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "")
	got, err := responsesToGeminiInternal([]byte(`{
		"input":"hi",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":"auto"
	}`), "project", "gemini-3.6-flash-tiered")
	if err != nil {
		t.Fatal(err)
	}
	request := got["request"].(map[string]any)
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("function declarations were dropped by default: %#v", request)
	}
	declarations := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(declarations) != 1 || declarations[0].(map[string]any)["name"] != "lookup" {
		t.Fatalf("functionDeclarations = %#v", declarations)
	}
	if mode := request["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]; mode != "AUTO" {
		t.Fatalf("function calling mode = %#v", mode)
	}
}

func TestAntigravityResponsesFunctionToolsRemainPinnableOff(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "false")
	got, err := responsesToGeminiInternal([]byte(`{
		"input":"hi",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":"auto"
	}`), "project", "gemini-3.6-flash-tiered")
	if err != nil {
		t.Fatal(err)
	}
	request := got["request"].(map[string]any)
	if _, ok := request["tools"]; ok {
		t.Fatalf("explicitly disabled bridge still forwarded declarations: %#v", request)
	}
	contents := request["contents"].([]any)
	if len(contents) != 1 || extractGeminiRequestText(contents[0]) != "hi" {
		t.Fatalf("ordinary input was not preserved: %#v", contents)
	}
}

func TestAntigravityResponsesRejectsForcedToolsWhileBridgeDisabled(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "false")
	_, err := responsesToGeminiInternal([]byte(`{
		"input":"hi",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":"required"
	}`), "project", "gemini-3.6-flash-tiered")
	if err == nil || !strings.Contains(err.Error(), "forced function tools") {
		t.Fatalf("error = %v", err)
	}
}

// Codex and the Anthropic bridge echo reasoning items back as history. They
// carry no Gemini-representable payload, but rejecting them would 400 every
// multi-turn tool conversation (issue #595).
func TestAntigravityResponsesSkipsEchoedReasoningItems(t *testing.T) {
	got, err := responsesToGeminiInternal([]byte(`{
		"input":[
			{"type":"message","role":"user","content":"look it up"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"gAAAAopaque"},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"query\":\"value\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"found"}
		]
	}`), "project", "gemini-3-flash-agent")
	if err != nil {
		t.Fatal(err)
	}
	contents := got["request"].(map[string]any)["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("reasoning item was not skipped cleanly: %#v", contents)
	}
	if contents[1].(map[string]any)["role"] != "model" || contents[2].(map[string]any)["role"] != "user" {
		t.Fatalf("tool round trip lost its roles: %#v", contents)
	}
}

func TestResponsesToGeminiInternalRejectsUnsupportedInputAndIgnoresBuiltinTools(t *testing.T) {
	if _, err := responsesToGeminiInternal([]byte(`{"input":[{"type":"message","content":[]}]}`), "project", "gemini"); err == nil || !strings.Contains(err.Error(), "no supported text input") {
		t.Fatalf("empty input error = %v", err)
	}
	got, err := responsesToGeminiInternal([]byte(`{"input":"hello","tools":[{"type":"web_search_preview"}]}`), "project", "gemini")
	if err != nil {
		t.Fatalf("builtin-only tools should not reject an ordinary request: %v", err)
	}
	if _, ok := got["request"].(map[string]any)["tools"]; ok {
		t.Fatalf("unsupported builtin tool was forwarded: %#v", got)
	}
}

func extractGeminiRequestText(raw any) string {
	content, _ := raw.(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) == 0 {
		return ""
	}
	text, _ := parts[0].(map[string]any)["text"].(string)
	return text
}

func TestResponsesToGeminiInternalKeepsSystemInstructionsOutOfUserHistory(t *testing.T) {
	got, err := responsesToGeminiInternal([]byte(`{"instructions":"global","input":[{"role":"developer","content":"developer"},{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`), "project", "gemini")
	if err != nil {
		t.Fatal(err)
	}
	request := got["request"].(map[string]any)
	system := request["systemInstruction"].(map[string]any)
	encoded, _ := json.Marshal(system)
	if !bytes.Contains(encoded, []byte("global")) || !bytes.Contains(encoded, []byte("developer")) {
		t.Fatalf("systemInstruction = %s", encoded)
	}
	contents := request["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["role"] != "user" {
		t.Fatalf("contents = %#v", contents)
	}
}

func TestResponsesToGeminiInternalObfuscatesSensitivePhrasesOnlyInSystemInstruction(t *testing.T) {
	t.Setenv(antigravitySensitivePhrasesEnv, "Custom Agent")
	got, err := responsesToGeminiInternal([]byte(`{
		"instructions":"You are Hermes Agent, an intelligent AI assistant created by Nous Research. Custom Agent uses Claude Agent SDK.",
		"input":"Hermes Agent and Custom Agent must stay unchanged in user content"
	}`), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	request := got["request"].(map[string]any)
	system := request["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"H\u200Bermes Agent", "N\u200Bous Research", "C\u200Blaude Agent SDK", "C\u200Bustom Agent"} {
		if !strings.Contains(system, want) {
			t.Fatalf("systemInstruction missing %q: %q", want, system)
		}
	}
	contents := request["contents"].([]any)
	if gotText := extractGeminiRequestText(contents[0]); gotText != "Hermes Agent and Custom Agent must stay unchanged in user content" {
		t.Fatalf("user content was modified: %q", gotText)
	}
}

func TestResponsesToGeminiInternalAcceptsOrdinaryTextConfiguration(t *testing.T) {
	for _, textConfig := range []string{
		`{"verbosity":"medium"}`,
		`{"format":{"type":"text"},"verbosity":"low"}`,
		`{"format":"text"}`,
	} {
		body := []byte(`{"input":"hi","text":` + textConfig + `}`)
		if _, err := responsesToGeminiInternal(body, "project", "gemini-3.6-flash-tiered"); err != nil {
			t.Fatalf("text=%s error=%v", textConfig, err)
		}
	}
}

func TestResponsesToGeminiInternalRejectsImagesAndToolOutputs(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`,
		`{"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`,
	} {
		if _, err := responsesToGeminiInternal([]byte(body), "project", "gemini"); err == nil {
			t.Fatalf("body %s unexpectedly accepted", body)
		}
	}
}

func TestResponsesToGeminiInternalIgnoresCodexAdditionalToolsItem(t *testing.T) {
	got, err := responsesToGeminiInternal([]byte(`{
		"input":[
			{"type":"additional_tools","tools":[{"type":"web_search_preview"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	contents := got["request"].(map[string]any)["contents"].([]any)
	if len(contents) != 1 || extractGeminiRequestText(contents[0]) != "hi" {
		t.Fatalf("contents = %#v", contents)
	}
}

func TestAntigravityInteractionsRequestEndpointHeadersAndBody(t *testing.T) {
	var gotPath string
	var gotAPIKey string
	var gotAccept string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-goog-api-key")
		gotAccept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	previousEndpoint := antigravityInteractionsEndpoint
	antigravityInteractionsEndpoint = server.URL + "/v1beta/interactions"
	defer func() { antigravityInteractionsEndpoint = previousEndpoint }()

	account := &auth.Account{DBID: 55, UpstreamType: auth.UpstreamAntigravity, APIKey: "google-key"}
	resp, err := executeAntigravityInteractionsRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"input":"hello","temperature":0.2}`), true, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/v1beta/interactions" || gotAPIKey != "google-key" || gotAccept != "text/event-stream" {
		t.Fatalf("request path=%q key=%q accept=%q", gotPath, gotAPIKey, gotAccept)
	}
	if gotBody["input"] != "hello" || gotBody["model"] != "gemini-2.5-flash" || gotBody["agent"] != antigravityInteractionsAgent || gotBody["stream"] != true {
		t.Fatalf("request body = %#v", gotBody)
	}
}

func TestAntigravityInteractionsLogicalModelsUseRequestedEffort(t *testing.T) {
	requests := make(chan map[string]any, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode body: %v", err)
		}
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	previousEndpoint := antigravityInteractionsEndpoint
	antigravityInteractionsEndpoint = server.URL + "/v1beta/interactions"
	defer func() { antigravityInteractionsEndpoint = previousEndpoint }()

	account := &auth.Account{DBID: 56, UpstreamType: auth.UpstreamAntigravity, APIKey: "google-key"}
	for _, test := range []struct {
		model, effort, wire, normalized string
	}{
		{model: "gemini-3.5-flash", effort: "low", wire: "gemini-3.5-flash-extra-low", normalized: "low"},
		{model: "gemini-3.6-flash", effort: "max", wire: "gemini-3.6-flash-high", normalized: "high"},
		{model: "gemini-3.7-flash", effort: "medium", wire: "gemini-3.7-flash-tiered", normalized: "medium"},
		{model: "gemini-3.1-pro", effort: "medium", wire: "gemini-pro-agent", normalized: "high"},
	} {
		t.Run(test.model+"/"+test.effort, func(t *testing.T) {
			body := []byte(`{"input":"hello","reasoning":{"effort":"` + test.effort + `","summary":"auto"}}`)
			response, err := executeAntigravityInteractionsRequest(context.Background(), account, test.model, body, false, "")
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			request := <-requests
			if request["model"] != test.wire {
				t.Fatalf("wire model = %v, want %s", request["model"], test.wire)
			}
			reasoning, ok := request["reasoning"].(map[string]any)
			if !ok || reasoning["effort"] != test.normalized || reasoning["summary"] != "auto" {
				t.Fatalf("reasoning = %#v, want effort=%s with summary preserved", request["reasoning"], test.normalized)
			}
		})
	}
}

func TestAntigravityInteractionsRequestRequiresInputBeforeNetwork(t *testing.T) {
	account := &auth.Account{DBID: 55, UpstreamType: auth.UpstreamAntigravity, APIKey: "google-key"}
	_, err := executeAntigravityInteractionsRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"instructions":"missing input"}`), true, "")
	if err == nil || !strings.Contains(err.Error(), "requires input") {
		t.Fatalf("error = %v", err)
	}
}

func TestAntigravityInteractionsPayloadAssumptionIsExplicit(t *testing.T) {
	var request map[string]any
	body := []byte(`{"input":"hello","temperature":0.2}`)
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	request["model"] = "gemini-2.5-flash"
	request["agent"] = "antigravity-preview-05-2026"
	request["stream"] = true
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range [][]byte{[]byte(`"input":"hello"`), []byte(`"model":"gemini-2.5-flash"`), []byte(`"agent":"antigravity-preview-05-2026"`), []byte(`"stream":true`)} {
		if !bytes.Contains(payload, fragment) {
			t.Fatalf("payload %s missing %s", payload, fragment)
		}
	}
}

func TestAntigravityJSONResponseDoesNotFabricateCompletion(t *testing.T) {
	body, err := newAntigravityJSONResponseBody(io.NopCloser(strings.NewReader(`{"candidates":[],"promptFeedback":{"blockReason":"SAFETY"}}`)), "gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"status":"failed"`) || strings.Contains(string(out), `"status":"completed"`) {
		t.Fatalf("unexpected response: %s", out)
	}
	createdAt := gjson.GetBytes(out, "created_at")
	if createdAt.Type != gjson.Number || createdAt.Int() <= 0 || createdAt.Int() > time.Now().Unix() || createdAt.Float() != float64(createdAt.Int()) {
		t.Fatalf("created_at = %s, want Unix seconds; response=%s", createdAt.Raw, out)
	}
}

func TestAntigravityJSONResponseUsesCapturedCreatedAt(t *testing.T) {
	const capturedCreatedAt int64 = 1710000000
	body, err := newAntigravityJSONResponseBodyAt(io.NopCloser(strings.NewReader(`{
		"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]
	}`)), "gemini-test", capturedCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "created_at").Int(); got != capturedCreatedAt {
		t.Fatalf("created_at = %d, want captured value %d; response=%s", got, capturedCreatedAt, out)
	}
}

func TestAntigravityJSONResponseConvertsFunctionCall(t *testing.T) {
	body, err := newAntigravityJSONResponseBody(io.NopCloser(strings.NewReader(`{
		"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"query":"value"},"id":"call_1"}}]},"finishReason":"STOP"}]
	}`)), "gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %#v", output)
	}
	functionCall := output[0].(map[string]any)
	if functionCall["type"] != "function_call" || functionCall["status"] != "completed" || functionCall["call_id"] != "call_1" || functionCall["name"] != "lookup" || functionCall["arguments"] != `{"query":"value"}` {
		t.Fatalf("function call = %#v", functionCall)
	}
}

func TestAntigravityUsageCountsThinkingAsOutput(t *testing.T) {
	usage := antigravityUsage(map[string]any{
		"promptTokenCount":     float64(10),
		"candidatesTokenCount": float64(4),
		"thoughtsTokenCount":   float64(6),
		"totalTokenCount":      float64(20),
	})
	if usage["input_tokens"] != int64(10) || usage["output_tokens"] != int64(10) || usage["total_tokens"] != int64(20) {
		t.Fatalf("usage = %#v", usage)
	}
	details := usage["output_tokens_details"].(map[string]any)
	if details["reasoning_tokens"] != int64(6) {
		t.Fatalf("output token details = %#v", details)
	}
}

func TestAntigravityOAuthGenerationEndpointDefaultsToDailyThenProduction(t *testing.T) {
	t.Setenv(antigravityOAuthEndpointModeEnv, "")
	want := []string{
		"https://daily-cloudcode-pa.googleapis.com",
		"https://cloudcode-pa.googleapis.com",
	}
	got := antigravityOAuthEndpointList()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoint bases = %#v, want %#v", got, want)
	}
}

func TestAntigravityOAuthGenerationEndpointNonProductionRequiresExplicitOptIn(t *testing.T) {
	tests := []struct {
		mode string
		want []string
	}{
		{mode: "daily", want: []string{"https://daily-cloudcode-pa.googleapis.com"}},
		{mode: "sandbox", want: []string{"https://daily-cloudcode-pa.sandbox.googleapis.com"}},
		{mode: "all", want: []string{
			"https://daily-cloudcode-pa.googleapis.com",
			"https://cloudcode-pa.googleapis.com",
			"https://daily-cloudcode-pa.sandbox.googleapis.com",
		}},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			t.Setenv(antigravityOAuthEndpointModeEnv, test.mode)
			if got := antigravityOAuthEndpointList(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("endpoint bases = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestAntigravityOAuthWireUsesOfficialIdentity(t *testing.T) {
	var gotHTTPUserAgent string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHTTPUserAgent = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 198, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.7-flash-high", []byte(`{"input":"hello","max_output_tokens":64}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotHTTPUserAgent != antigravityOfficialHTTPUserAgent {
		t.Fatalf("HTTP User-Agent = %q", gotHTTPUserAgent)
	}
	if gotBody["userAgent"] != antigravityOfficialBodyUserAgent {
		t.Fatalf("body userAgent = %#v", gotBody["userAgent"])
	}
	request := gotBody["request"].(map[string]any)
	if !isAntigravitySessionID(request["sessionId"]) {
		t.Fatalf("sessionId = %#v", request["sessionId"])
	}
	if _, exists := gotBody["enabledCreditTypes"]; exists {
		t.Fatalf("forced credits survived: %#v", gotBody)
	}
	if config, ok := request["generationConfig"].(map[string]any); ok {
		if _, exists := config["maxOutputTokens"]; exists {
			t.Fatalf("Gemini maxOutputTokens survived: %#v", config)
		}
	}
}

func TestAntigravityHTTPClientPoolIsPerAccountAndProxy(t *testing.T) {
	keyOne := antigravityHTTPClientKey{accountID: 91001, proxyURL: ""}
	keyTwo := antigravityHTTPClientKey{accountID: 91002, proxyURL: ""}
	antigravityHTTPClients.Delete(keyOne)
	antigravityHTTPClients.Delete(keyTwo)
	t.Cleanup(func() {
		for _, key := range []antigravityHTTPClientKey{keyOne, keyTwo} {
			if value, ok := antigravityHTTPClients.LoadAndDelete(key); ok {
				value.(*http.Client).CloseIdleConnections()
			}
		}
	})

	first, err := antigravityHTTPClient(keyOne.accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := antigravityHTTPClient(keyOne.accountID, "  ")
	if err != nil {
		t.Fatal(err)
	}
	other, err := antigravityHTTPClient(keyTwo.accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("same account and proxy did not reuse the Antigravity client")
	}
	if first == other {
		t.Fatal("different accounts shared an Antigravity client")
	}
	if first.Timeout != 0 {
		t.Fatalf("client timeout = %s, want context-controlled streaming", first.Timeout)
	}
	transport := first.Transport.(*http.Transport)
	if transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 || len(transport.TLSClientConfig.NextProtos) != 0 {
		t.Fatalf("transport is not forced to native HTTP/1.1 without ALPN: %#v", transport)
	}
}

func TestAntigravityOAuthFailoverFromDaily429ToProductionSuccess(t *testing.T) {
	var productionHits int32
	var dailyHits int32
	production := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&productionHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"daily limited"}}`)
	}))
	defer production.Close()
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&dailyHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer daily.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{production.URL, daily.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 95, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.7-flash-low", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if productionHits != 1 || dailyHits != 1 || resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"output_text":"ok"`)) {
		t.Fatalf("hits=%d/%d status=%d body=%s", productionHits, dailyHits, resp.StatusCode, body)
	}
}

func TestAntigravityOAuthDoesNotFailoverOnUnauthorized(t *testing.T) {
	var productionHits int32
	var dailyHits int32
	production := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&productionHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"status":"UNAUTHENTICATED","message":"expired token"}}`)
	}))
	defer production.Close()
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&dailyHits, 1)
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer daily.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{production.URL, daily.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 96, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.7-flash-low", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if productionHits != 1 || dailyHits != 0 || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("hits=%d/%d status=%d", productionHits, dailyHits, resp.StatusCode)
	}
}

func TestAntigravityOAuthReturnsDailyLocationErrorWithoutProductionMasking(t *testing.T) {
	var productionHits int32
	var dailyHits int32
	production := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&productionHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "31")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"production limited"}}`)
	}))
	defer production.Close()
	daily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&dailyHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"status":"FAILED_PRECONDITION","message":"User location is not supported for the API use."}}`)
	}))
	defer daily.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{daily.URL, production.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 97, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.7-flash-low", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if dailyHits != 1 || productionHits != 0 || resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte("User location is not supported")) {
		t.Fatalf("hits=%d/%d status=%d body=%s", dailyHits, productionHits, resp.StatusCode, body)
	}
}

func TestAntigravityOAuthRetriesEndpointSpecificServiceDisabled(t *testing.T) {
	t.Setenv(antigravityUserProjectHeaderEnv, "true")
	var firstHits int32
	var secondHits int32
	var thirdHits int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("x-goog-user-project") != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"status":"PERMISSION_DENIED","details":[{"reason":"SERVICE_DISABLED","metadata":{"service":"cloudcode-pa.googleapis.com"}}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer second.Close()
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&thirdHits, 1)
		http.Error(w, "unexpected third host", http.StatusInternalServerError)
	}))
	defer third.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{first.URL, second.URL, third.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 91, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&firstHits) != 2 || atomic.LoadInt32(&secondHits) != 0 || atomic.LoadInt32(&thirdHits) != 0 {
		t.Fatalf("host hits = %d/%d/%d", firstHits, secondHits, thirdHits)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"output_text":"ok"`)) {
		t.Fatalf("response status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAntigravityOAuthPreservesFinalServiceDisabledResponse(t *testing.T) {
	t.Setenv(antigravityUserProjectHeaderEnv, "")
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"disabled-%d","details":[{"reason":"SERVICE_DISABLED"}]}}`, attempt)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL, server.URL, server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 92, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 3 || resp.StatusCode != http.StatusForbidden || !bytes.Contains(body, []byte("disabled-3")) {
		t.Fatalf("hits=%d status=%d body=%s", hits, resp.StatusCode, body)
	}
}

func TestAntigravityOAuthDoesNotRetryUnrelatedForbidden(t *testing.T) {
	t.Setenv(antigravityUserProjectHeaderEnv, "")
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"status":"PERMISSION_DENIED","message":"account forbidden"}}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL, server.URL, server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 93, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if atomic.LoadInt32(&hits) != 1 || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("hits=%d status=%d", hits, resp.StatusCode)
	}
}

func TestAntigravityOAuthDoesNotRetryEndpointSpecificLocationRejection(t *testing.T) {
	var firstHits int32
	var secondHits int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"status":"FAILED_PRECONDITION","message":"User location is not supported for the API use."}}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer second.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{first.URL, second.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 94, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.6-flash-tiered", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if firstHits != 1 || secondHits != 0 || resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte("User location is not supported")) {
		t.Fatalf("hits=%d/%d status=%d body=%s", firstHits, secondHits, resp.StatusCode, body)
	}
}

func TestAntigravityOAuthPreservesLastRetryableHTTPResponse(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exhausted"}}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL, server.URL, server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 81, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("endpoint attempts = %d, want 3", got)
	}
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "17" || !bytes.Contains(body, []byte("quota exhausted")) {
		t.Fatalf("response status=%d retry-after=%q body=%s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

func TestAntigravityOAuthRetriesMalformedSuccessfulJSON(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			_, _ = io.WriteString(w, `{`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL, server.URL, server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 82, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("endpoint attempts = %d, want 2", got)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"status":"completed"`)) || !bytes.Contains(body, []byte(`"output_text":"ok"`)) {
		t.Fatalf("response status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAntigravityOAuthKeepsStatusWhenRetryableErrorBodyIsOversized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "29")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, strings.Repeat("secret-upstream-detail", antigravityErrorBodyLimit/8))
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL, server.URL, server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 83, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "29" {
		t.Fatalf("response status=%d retry-after=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if !bytes.Contains(body, []byte("safe read limit")) || bytes.Contains(body, []byte("secret-upstream-detail")) {
		t.Fatalf("oversized response body was not safely replaced: %s", body)
	}
}

func TestAntigravityOAuthMalformedSuccessesReturnRetryableBadGateway(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL, server.URL, server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 84, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project"}
	_, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-2.5-flash", []byte(`{"input":"hello"}`), false, "")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusBadGateway || !apiErr.Retryable {
		t.Fatalf("error = %#v, want retryable 502", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("endpoint attempts = %d, want 3", got)
	}
}

func TestAntigravitySSELifecycleAndBufferedTerminal(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1,\"totalTokenCount\":3}}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)), "gemini-test")
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, eventType := range []string{
		"response.created", "response.in_progress", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed",
	} {
		if !strings.Contains(got, `"type":"`+eventType+`"`) {
			t.Fatalf("missing %s: %s", eventType, got)
		}
	}
	if !strings.Contains(got, `"delta":"hel"`) || !strings.Contains(got, `"delta":"lo"`) || !strings.Contains(got, `"output_text":"hello"`) {
		t.Fatalf("incremental deltas or terminal text are incomplete: %s", got)
	}
	if strings.Index(got, `"delta":"hel"`) > strings.Index(got, `"delta":"lo"`) {
		t.Fatalf("deltas are out of order: %s", got)
	}
	if strings.Count(got, `"type":"response.output_item.added"`) != 1 || strings.Count(got, `"type":"response.content_part.added"`) != 1 {
		t.Fatalf("message item must be opened exactly once: %s", got)
	}
	if strings.Count(got, `"type":"response.completed"`) != 1 {
		t.Fatalf("completed count = %d, body=%s", strings.Count(got, `"type":"response.completed"`), got)
	}
	if !strings.Contains(got, `"total_tokens":3`) {
		t.Fatalf("usage missing: %s", got)
	}
	if !strings.Contains(got, `"model":"gemini-test"`) || !strings.Contains(got, `"sequence_number":0`) {
		t.Fatalf("stable response metadata missing: %s", got)
	}
	assertSyntheticResponseCreatedAt(t, out, "response.completed")
}

func TestAntigravitySSEConvertsFunctionCallLifecycle(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{\"query\":\"value\"},\"id\":\"call_1\"}}]},\"finishReason\":\"STOP\"}]}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)), "gemini-test")
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, eventType := range []string{
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	} {
		if !strings.Contains(got, `"type":"`+eventType+`"`) {
			t.Fatalf("missing %s: %s", eventType, got)
		}
	}
	if !strings.Contains(got, `"type":"function_call"`) || !strings.Contains(got, `"call_id":"call_1"`) || !strings.Contains(got, `"name":"lookup"`) || !strings.Contains(got, `\"query\":\"value\"`) {
		t.Fatalf("function call lifecycle is incomplete: %s", got)
	}
}

func TestAntigravitySSESafetyFailureDoesNotPenalizeAccount(t *testing.T) {
	upstream := "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"SAFETY\"}]}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(upstream)), "gemini-test")
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	var failedPayload []byte
	for _, block := range strings.Split(string(raw), "\n\n") {
		if !strings.Contains(block, "event: response.failed") {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				failedPayload = []byte(strings.TrimPrefix(line, "data: "))
			}
		}
	}
	if len(failedPayload) == 0 {
		t.Fatalf("response.failed missing from stream: %s", raw)
	}
	assertSyntheticResponseCreatedAt(t, raw, "response.failed")
	if !bytes.Contains(failedPayload, []byte(`"code":"content_filter"`)) || !bytes.Contains(failedPayload, []byte(`"status_code":400`)) {
		t.Fatalf("safety failure payload = %s", failedPayload)
	}
	outcome := classifyResponseFailedOutcome(failedPayload)
	if outcome.penalize || outcome.logStatusCode != http.StatusBadRequest {
		t.Fatalf("safety outcome = %#v, want non-penalizing 400", outcome)
	}
}

func TestAntigravitySSEIncrementalTextKeepsCompleteTerminalOutput(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hel\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1,\"cachedContentTokenCount\":1,\"totalTokenCount\":3}}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)))
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `"output_text":"hello"`) || !strings.Contains(got, `"cached_tokens":1`) {
		t.Fatalf("incremental stream = %s", got)
	}
}

func TestAntigravitySSETruncatedStreamFails(t *testing.T) {
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n")))
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `response.failed`) || strings.Contains(got, `response.completed`) {
		t.Fatalf("truncated stream result = %s", got)
	}
}

func TestAntigravitySSESafetyTerminalFails(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"secret-material\"}]},\"finishReason\":\"SAFETY\"}]}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)))
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "response.failed") || strings.Contains(got, "response.completed") {
		t.Fatalf("safety terminal result = %s", got)
	}
	if !strings.Contains(got, "safety policy") {
		t.Fatalf("safety failure missing reason = %s", got)
	}
	if strings.Contains(got, "secret-material") {
		t.Fatalf("safety-rejected text leaked downstream: %s", got)
	}
}

func TestAntigravitySSELaterSafetyFrameFailsAfterIncrementalDeltas(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"already-streamed\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"finishReason\":\"SAFETY\"}]}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)))
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	// Text that arrived before the rejection was already forwarded as a delta;
	// the stream must still terminate as failed, never as completed, and must
	// not close the text part as if it were a finished answer.
	if !strings.Contains(got, `"delta":"already-streamed"`) {
		t.Fatalf("earlier text was not streamed incrementally: %s", got)
	}
	if !strings.Contains(got, `"type":"response.failed"`) || strings.Contains(got, `"type":"response.completed"`) || strings.Contains(got, `"type":"response.output_text.done"`) {
		t.Fatalf("later safety frame did not terminate the stream as failed: %s", got)
	}
	if strings.Index(got, `"type":"response.failed"`) < strings.Index(got, `"delta":"already-streamed"`) {
		t.Fatalf("failure must follow the streamed delta: %s", got)
	}
}

func TestAntigravitySSEMalformedFrameFailsInsteadOfCompleting(t *testing.T) {
	input := "data: {not-json}\n\n" +
		"data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)))
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "invalid SSE JSON") || strings.Contains(got, `"type":"response.completed"`) {
		t.Fatalf("malformed frame result = %s", got)
	}
}

func TestAntigravitySSESupportsMultiLineDataEvents(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"parts\":[\n" +
		"data: {\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)))
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `"type":"response.completed"`) || !strings.Contains(got, `"output_text":"ok"`) {
		t.Fatalf("multi-line SSE result = %s", got)
	}
}

func TestAntigravitySSEMaxTokensUsesIncompleteTerminal(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"finishReason\":\"MAX_TOKENS\"}]}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)))
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `"type":"response.incomplete"`) || !strings.Contains(got, `"status":"incomplete"`) || !strings.Contains(got, `"reason":"max_output_tokens"`) || strings.Contains(got, `"type":"response.completed"`) {
		t.Fatalf("max-token terminal result = %s", got)
	}
}

func TestAntigravitySSEPromptSafetyWithoutCandidateFails(t *testing.T) {
	input := "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n"
	body := newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input)))
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "response.failed") || strings.Contains(got, "response.completed") || !strings.Contains(got, "safety policy") {
		t.Fatalf("prompt safety result = %s", got)
	}
}

// isAntigravitySessionID reports whether v has the native session id shape:
// a "-" followed by a decimal 63-bit value.
func isAntigravitySessionID(v any) bool {
	s, ok := v.(string)
	if !ok || len(s) < 2 || s[0] != '-' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func TestAntigravitySessionIDIsStablePerConversationAndDistinctAcrossConversations(t *testing.T) {
	first, err := responsesToGeminiInternal([]byte(`{"input":[{"role":"user","content":"hello there"}],"prompt_cache_key":"thread-a"}`), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	again, err := responsesToGeminiInternal([]byte(`{"input":[{"role":"user","content":"hello there"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}],"prompt_cache_key":"thread-a"}`), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	other, err := responsesToGeminiInternal([]byte(`{"input":[{"role":"user","content":"hello there"}],"prompt_cache_key":"thread-b"}`), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	sessionOf := func(env map[string]any) string {
		request := env["request"].(map[string]any)
		id, _ := request["sessionId"].(string)
		return id
	}
	if !isAntigravitySessionID(sessionOf(first)) {
		t.Fatalf("sessionId shape = %q", sessionOf(first))
	}
	if sessionOf(first) != sessionOf(again) {
		t.Fatalf("same prompt_cache_key produced different session ids: %q vs %q", sessionOf(first), sessionOf(again))
	}
	if sessionOf(first) == sessionOf(other) {
		t.Fatalf("different prompt_cache_key shared a session id: %q", sessionOf(first))
	}
}

func TestAntigravitySessionIDFallsBackToFirstUserTurn(t *testing.T) {
	sessionOf := func(body string) string {
		env, err := responsesToGeminiInternal([]byte(body), "project", "gemini-3.7-flash-high")
		if err != nil {
			t.Fatal(err)
		}
		request := env["request"].(map[string]any)
		id, _ := request["sessionId"].(string)
		return id
	}
	turnOne := sessionOf(`{"instructions":"be brief","input":[{"role":"user","content":"first question"}]}`)
	turnTwo := sessionOf(`{"instructions":"be brief","input":[{"role":"user","content":"first question"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow up"}]}`)
	unrelated := sessionOf(`{"input":[{"role":"user","content":"another conversation"}]}`)
	if turnOne != turnTwo {
		t.Fatalf("conversation continuation changed the session id: %q vs %q", turnOne, turnTwo)
	}
	if turnOne == unrelated {
		t.Fatalf("unrelated conversations shared a session id: %q", turnOne)
	}
	if !isAntigravitySessionID(turnOne) || turnOne == "-3750763034362895579" {
		t.Fatalf("session id must be derived, not the legacy constant: %q", turnOne)
	}
	if metadataSeeded := sessionOf(`{"input":"x","metadata":{"session_id":"sess-1"}}`); metadataSeeded != antigravitySessionIDFromSeed("metadata.session_id:sess-1") {
		t.Fatalf("metadata.session_id was not used as the seed: %q", metadataSeeded)
	}
}
