package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeServiceTierField(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.4","serviceTier":"fast"}`)

	got := normalizeServiceTierField(raw)

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "fast" {
		t.Fatalf("service_tier mismatch: got %q want %q", tier, "fast")
	}
	if gjson.GetBytes(got, "serviceTier").Exists() {
		t.Fatal("serviceTier should be removed after normalization")
	}
}

func TestResolveServiceTier(t *testing.T) {
	if got := resolveServiceTier("fast", "default"); got != "fast" {
		t.Fatalf("expected actual tier to win, got %q", got)
	}
	if got := resolveServiceTier("", "fast"); got != "fast" {
		t.Fatalf("expected requested tier fallback, got %q", got)
	}
	if got := resolveServiceTier("default", "fast"); got != "fast" {
		t.Fatalf("expected requested fast to win for logging, got %q", got)
	}
}

func TestSanitizeServiceTierForUpstream_FastToPriority(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"service_tier":"fast"
	}`)

	got := sanitizeServiceTierForUpstream(raw)

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("fast should be mapped to priority for upstream, got %q", tier)
	}
}

func TestTranslateRequest_PreservesSupportedServiceTier(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hello"}],
		"serviceTier":"priority",
		"reasoning_effort":"high"
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("service_tier mismatch: got %q want %q", tier, "priority")
	}
	if gjson.GetBytes(got, "serviceTier").Exists() {
		t.Fatal("serviceTier should not be present after translation")
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "high" {
		t.Fatalf("reasoning.effort mismatch: got %q want %q", effort, "high")
	}
}

func TestTranslateRequest_NormalizesReasoningEffortAliases(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hello"}],
		"reasoning_effort":"MAX"
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning.effort mismatch: got %q want %q", effort, "xhigh")
	}
}

func TestTranslateRequest_FillsMissingArrayItemsInToolSchema(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"test"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"godot-mcp_node_signal",
					"parameters":{
						"type":"object",
						"properties":{
							"args":{"type":"array"}
						}
					}
				}
			}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	items := gjson.GetBytes(got, "tools.0.parameters.properties.args.items")
	if !items.Exists() || items.Type != gjson.JSON {
		t.Fatalf("expected array schema items object to be injected, got %s", items.Raw)
	}
}

func TestPrepareResponsesBody_FillsMissingArrayItemsInToolSchema(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"tools":[
			{
				"type":"function",
				"name":"godot-mcp_node_signal",
				"parameters":{
					"type":"object",
					"properties":{
						"args":{"type":"array"}
					}
				}
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	items := gjson.GetBytes(got, "tools.0.parameters.properties.args.items")
	if !items.Exists() || items.Type != gjson.JSON {
		t.Fatalf("expected array schema items object to be injected, got %s", items.Raw)
	}
}

func TestPrepareResponsesBody_DefaultsIncludeForResponses(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test"
	}`)

	got, _ := PrepareResponsesBody(raw)

	include := gjson.GetBytes(got, "include")
	if !include.Exists() || len(include.Array()) != 1 || include.Array()[0].String() != "reasoning.encrypted_content" {
		t.Fatalf("expected default include for responses, got %s", include.Raw)
	}
	if stream := gjson.GetBytes(got, "stream"); !stream.Exists() || !stream.Bool() {
		t.Fatalf("expected stream to be forced for responses, got %s", stream.Raw)
	}
	if store := gjson.GetBytes(got, "store"); !store.Exists() || store.Bool() {
		t.Fatalf("expected store=false for responses, got %s", store.Raw)
	}
	if gotTool := gjson.GetBytes(got, "tools.0.type").String(); gotTool != "image_generation" {
		t.Fatalf("expected default image_generation tool, got %s", string(got))
	}
	if model := gjson.GetBytes(got, "tools.0.model").String(); model != defaultImagesToolModel {
		t.Fatalf("expected default image model %q, got %q", defaultImagesToolModel, model)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages1KSize {
		t.Fatalf("expected default image size %q, got %q", defaultImages1KSize, size)
	}
	if format := gjson.GetBytes(got, "tools.0.output_format").String(); format != "png" {
		t.Fatalf("expected default image output_format png, got %q", format)
	}
	if instructions := gjson.GetBytes(got, "instructions").String(); !strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		t.Fatalf("expected bridge instructions, got %q", instructions)
	}
}

func TestPrepareResponsesBody_NormalizesNestedReasoningEffortAliases(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"reasoning":{"effort":"MAX"}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning.effort mismatch: got %q want %q; body=%s", effort, "xhigh", got)
	}
}

func TestPrepareResponsesBody_DropsBlankReasoningEffort(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"reasoning_effort":"   "
	}`)

	got, _ := PrepareResponsesBody(raw)

	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Fatalf("reasoning.effort should be omitted for blank effort; body=%s", got)
	}
}

func TestPrepareResponsesBody_DropsBlankNestedReasoningEffort(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"reasoning":{"effort":"   ","summary":"auto"}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Fatalf("reasoning.effort should be omitted for blank nested effort; body=%s", got)
	}
	if summary := gjson.GetBytes(got, "reasoning.summary").String(); summary != "auto" {
		t.Fatalf("reasoning.summary mismatch: got %q want auto; body=%s", summary, got)
	}
}

func TestPrepareResponsesBody_ImageOnlyModelBuildsImageToolRequest(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2",
		"prompt":"draw a cat",
		"size":"1024x1024",
		"quality":"high",
		"output_format":"webp",
		"partial_images":2
	}`)

	got, _ := PrepareResponsesBody(raw)

	if model := gjson.GetBytes(got, "model").String(); model != defaultImagesMainModel {
		t.Fatalf("model = %q, want %q; body=%s", model, defaultImagesMainModel, got)
	}
	if text := gjson.GetBytes(got, "input.0.content").String(); text != "draw a cat" {
		t.Fatalf("input text = %q, want draw a cat; body=%s", text, got)
	}
	if gjson.GetBytes(got, "prompt").Exists() {
		t.Fatalf("prompt should be removed, got %s", got)
	}
	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != "gpt-image-2" {
		t.Fatalf("tool model = %q, want gpt-image-2; body=%s", toolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != "1024x1024" {
		t.Fatalf("tool size = %q, want 1024x1024; body=%s", size, got)
	}
	if format := gjson.GetBytes(got, "tools.0.output_format").String(); format != "webp" {
		t.Fatalf("tool output_format = %q, want webp; body=%s", format, got)
	}
	if partial := gjson.GetBytes(got, "tools.0.partial_images").Int(); partial != 2 {
		t.Fatalf("partial_images = %d, want 2; body=%s", partial, got)
	}
	if choice := gjson.GetBytes(got, "tool_choice.type").String(); choice != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; body=%s", choice, got)
	}
}

func TestPrepareResponsesBody_ImageAliasSetsDefaultSizeAndRealToolModel(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2-4k",
		"prompt":"draw a city wallpaper"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if model := gjson.GetBytes(got, "model").String(); model != defaultImagesMainModel {
		t.Fatalf("model = %q, want %q; body=%s", model, defaultImagesMainModel, got)
	}
	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages4KSize {
		t.Fatalf("tool size = %q, want %q; body=%s", size, defaultImages4KSize, got)
	}
}

func TestPrepareResponsesBody_ExplicitSizeOverridesImageAliasDefault(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2-4k",
		"prompt":"draw a square logo",
		"size":"1536x1536"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != "1536x1536" {
		t.Fatalf("tool size = %q, want explicit size; body=%s", size, got)
	}
}

func TestPrepareResponsesBody_ToolImageAliasInfersPortraitSize(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4-mini",
		"input":"draw a poster",
		"tools":[{"type":"image_generation","model":"gpt-image-2-2k"}],
		"tool_choice":{"type":"image_generation"}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages2KPortraitSize {
		t.Fatalf("tool size = %q, want %q; body=%s", size, defaultImages2KPortraitSize, got)
	}
}

func TestPrepareResponsesBody_ImageAliasInfersPortraitFromStructuredInput(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2-4k",
		"input":[
			{
				"role":"user",
				"content":[
					{"type":"input_text","text":"mobile wallpaper portrait neon skyline"}
				]
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if model := gjson.GetBytes(got, "model").String(); model != defaultImagesMainModel {
		t.Fatalf("model = %q, want %q; body=%s", model, defaultImagesMainModel, got)
	}
	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages4KPortraitSize {
		t.Fatalf("tool size = %q, want %q; body=%s", size, defaultImages4KPortraitSize, got)
	}
}

func TestPrepareResponsesBody_ImageAliasInfersSquareFromToolPrompt(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4-mini",
		"input":"square app icon logo",
		"tools":[{"type":"image_generation","model":"gpt-image-2-4k"}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages4KSquareSize {
		t.Fatalf("tool size = %q, want %q; body=%s", size, defaultImages4KSquareSize, got)
	}
}

func TestPrepareResponsesBody_InvalidImageSizeSurvivesForValidation(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2-4k",
		"prompt":"draw a giant image",
		"size":1024
	}`)

	got, _ := PrepareResponsesBody(raw)

	if size := gjson.GetBytes(got, "tools.0.size"); size.Type != gjson.Number {
		t.Fatalf("expected invalid numeric size to survive validation, got %s; body=%s", size.Raw, got)
	}
	if err := validateResponsesImageGenerationSizes(got); err == nil {
		t.Fatalf("expected image size validation error; body=%s", got)
	}
}

func TestPrepareResponsesBody_PromptCompatAndTopLevelImageOptions(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4-mini",
		"prompt":"draw a skyline sticker",
		"size":"1536x1024",
		"quality":"high",
		"background":"transparent",
		"output_format":"webp"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if text := gjson.GetBytes(got, "input.0.content").String(); text != "draw a skyline sticker" {
		t.Fatalf("input text = %q, want prompt text; body=%s", text, got)
	}
	for _, field := range []string{"prompt", "size", "quality", "background", "output_format"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("top-level %s should be removed, got %s", field, got)
		}
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != "1536x1024" {
		t.Fatalf("tool size = %q, want 1536x1024; body=%s", size, got)
	}
	if model := gjson.GetBytes(got, "tools.0.model").String(); model != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", model, defaultImagesToolModel, got)
	}
	if quality := gjson.GetBytes(got, "tools.0.quality").String(); quality != "high" {
		t.Fatalf("tool quality = %q, want high; body=%s", quality, got)
	}
	if background := gjson.GetBytes(got, "tools.0.background").String(); background != "transparent" {
		t.Fatalf("tool background = %q, want transparent; body=%s", background, got)
	}
	if format := gjson.GetBytes(got, "tools.0.output_format").String(); format != "webp" {
		t.Fatalf("tool output_format = %q, want webp; body=%s", format, got)
	}
}

func TestPrepareResponsesBody_InjectsImageToolWithinToolLimit(t *testing.T) {
	tools := make([]any, maxTools)
	for i := range tools {
		tools[i] = map[string]any{
			"type":        "function",
			"name":        fmt.Sprintf("tool_%d", i),
			"description": "test tool",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		}
	}
	raw, err := json.Marshal(map[string]any{
		"model": "gpt-5.4-mini",
		"input": "test",
		"tools": tools,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	got, _ := PrepareResponsesBody(raw)

	outTools := gjson.GetBytes(got, "tools").Array()
	if len(outTools) != maxTools {
		t.Fatalf("tools count = %d, want %d; body=%s", len(outTools), maxTools, got)
	}
	last := outTools[len(outTools)-1]
	if last.Get("type").String() != "image_generation" {
		t.Fatalf("last tool type = %q, want image_generation; body=%s", last.Get("type").String(), got)
	}
	if last.Get("model").String() != defaultImagesToolModel {
		t.Fatalf("image tool model = %q, want %q; body=%s", last.Get("model").String(), defaultImagesToolModel, got)
	}
}

func TestPrepareResponsesBody_PreservesExistingImageToolAndNormalizesAliases(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4-mini",
		"input":"draw a cat",
		"style":"cinematic",
		"tools":[
			{"type":"image_generation","model":"gpt-image-1.5","format":"webp","compression":45,"style":"cinematic"}
		],
		"instructions":"base"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if count := len(gjson.GetBytes(got, "tools").Array()); count != 1 {
		t.Fatalf("tools count = %d, want 1; body=%s", count, got)
	}
	if model := gjson.GetBytes(got, "tools.0.model").String(); model != "gpt-image-1.5" {
		t.Fatalf("tool model = %q, want gpt-image-1.5; body=%s", model, got)
	}
	if format := gjson.GetBytes(got, "tools.0.output_format").String(); format != "webp" {
		t.Fatalf("output_format = %q, want webp; body=%s", format, got)
	}
	if compression := gjson.GetBytes(got, "tools.0.output_compression").Int(); compression != 45 {
		t.Fatalf("output_compression = %d, want 45; body=%s", compression, got)
	}
	if gjson.GetBytes(got, "tools.0.format").Exists() || gjson.GetBytes(got, "tools.0.compression").Exists() {
		t.Fatalf("legacy aliases should be removed, got %s", got)
	}
	if gjson.GetBytes(got, "style").Exists() || gjson.GetBytes(got, "tools.0.style").Exists() {
		t.Fatalf("unsupported style parameter should be removed, got %s", got)
	}
	instructions := gjson.GetBytes(got, "instructions").String()
	if !strings.HasPrefix(instructions, "base\n\n") {
		t.Fatalf("expected bridge to append after existing instructions, got %q", instructions)
	}
	if strings.Count(instructions, codexImageGenerationBridgeMarker) != 1 {
		t.Fatalf("expected bridge marker once, got %q", instructions)
	}

	gotAgain, _ := PrepareResponsesBody(got)
	if instructionsAgain := gjson.GetBytes(gotAgain, "instructions").String(); strings.Count(instructionsAgain, codexImageGenerationBridgeMarker) != 1 {
		t.Fatalf("expected bridge marker once after second pass, got %q", instructionsAgain)
	}
}

func TestPrepareCompactResponsesBody_RemovesUnsupportedInjectedFields(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test"
	}`)

	got, _ := PrepareCompactResponsesBody(raw)

	for _, field := range []string{"include", "store", "stream"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("expected %s to be removed for compact body", field)
		}
	}
	input := gjson.GetBytes(got, "input")
	if !input.Exists() || !input.IsArray() || len(input.Array()) != 1 {
		t.Fatalf("expected compact input to remain normalized, got %s", input.Raw)
	}
	if input.Array()[0].Get("content").String() != "test" {
		t.Fatalf("expected compact input content to be preserved, got %s", input.Raw)
	}
}

func TestPrepareCompactResponsesBody_RemovesClientSuppliedInclude(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"include":["reasoning.encrypted_content"]
	}`)

	got, _ := PrepareCompactResponsesBody(raw)

	if gjson.GetBytes(got, "include").Exists() {
		t.Fatalf("expected client-supplied include to be removed for compact body, got %s", string(got))
	}
}

func TestPrepareResponsesBody_ConvertsPlaintextCompactionToDeveloperMessage(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction","text":"previous context was compacted"}
		]
	}`)

	got, expandedInputRaw := PrepareResponsesBody(raw)

	input := gjson.GetBytes(got, "input")
	if gotType := input.Get("1.type").String(); gotType == "compaction" {
		t.Fatalf("plaintext compaction item should not be sent upstream, got %s", input.Raw)
	}
	if gotRole := input.Get("1.role").String(); gotRole != "developer" {
		t.Fatalf("converted compaction role = %q, want developer; input=%s", gotRole, input.Raw)
	}
	if gotText := input.Get("1.content.0.text").String(); !strings.Contains(gotText, "previous context was compacted") {
		t.Fatalf("converted compaction text = %q, want summary; input=%s", gotText, input.Raw)
	}

	expanded := gjson.Parse(expandedInputRaw)
	if gotType := expanded.Get("1.type").String(); gotType == "compaction" {
		t.Fatalf("expanded input cache should not retain plaintext compaction, got %s", expanded.Raw)
	}
	if gotRole := expanded.Get("1.role").String(); gotRole != "developer" {
		t.Fatalf("expanded compaction role = %q, want developer; input=%s", gotRole, expanded.Raw)
	}
}

func TestPrepareCompactResponsesBody_ConvertsPlaintextCompactionToDeveloperMessage(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction","summary":"previous context was compacted"}
		]
	}`)

	got, _ := PrepareCompactResponsesBody(raw)

	input := gjson.GetBytes(got, "input")
	if gotType := input.Get("1.type").String(); gotType == "compaction" {
		t.Fatalf("plaintext compaction item should not be sent to compact upstream, got %s", input.Raw)
	}
	if gotRole := input.Get("1.role").String(); gotRole != "developer" {
		t.Fatalf("converted compaction role = %q, want developer; input=%s", gotRole, input.Raw)
	}
	if gotText := input.Get("1.content.0.text").String(); !strings.Contains(gotText, "previous context was compacted") {
		t.Fatalf("converted compaction text = %q, want summary; input=%s", gotText, input.Raw)
	}
}

// ==================== Function Calling 测试 ====================

func TestConvertMessagesToInput_ToolRole(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"tool","tool_call_id":"call_abc","content":"{\"temp\":72}"}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	if !input.IsArray() {
		t.Fatal("input should be an array")
	}

	item := input.Array()[0]
	if item.Get("type").String() != "function_call_output" {
		t.Fatalf("expected type function_call_output, got %q", item.Get("type").String())
	}
	if item.Get("call_id").String() != "call_abc" {
		t.Fatalf("expected call_id call_abc, got %q", item.Get("call_id").String())
	}
	if item.Get("output").String() != `{"temp":72}` {
		t.Fatalf("expected output to match, got %q", item.Get("output").String())
	}
}

func TestConvertMessagesToInput_AssistantWithToolCalls(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_123","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
			]}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	items := input.Array()
	if len(items) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(items))
	}

	fc := items[0]
	if fc.Get("type").String() != "function_call" {
		t.Fatalf("expected type function_call, got %q", fc.Get("type").String())
	}
	if fc.Get("call_id").String() != "call_123" {
		t.Fatalf("expected call_id call_123, got %q", fc.Get("call_id").String())
	}
	if fc.Get("name").String() != "get_weather" {
		t.Fatalf("expected name get_weather, got %q", fc.Get("name").String())
	}
	if fc.Get("arguments").String() != `{"city":"NYC"}` {
		t.Fatalf("expected arguments to match, got %q", fc.Get("arguments").String())
	}
}

func TestConvertMessagesToInput_FullMultiTurn(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"user","content":"What is the weather in NYC?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_001","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"NYC\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_001","content":"{\"temperature\":72}"},
			{"role":"user","content":"Thanks!"}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	items := input.Array()
	if len(items) != 4 {
		t.Fatalf("expected 4 input items, got %d", len(items))
	}

	// 用户消息
	if items[0].Get("type").String() != "message" || items[0].Get("role").String() != "user" {
		t.Fatal("first item should be user message")
	}
	// function_call
	if items[1].Get("type").String() != "function_call" {
		t.Fatalf("second item should be function_call, got %q", items[1].Get("type").String())
	}
	// function_call_output
	if items[2].Get("type").String() != "function_call_output" {
		t.Fatalf("third item should be function_call_output, got %q", items[2].Get("type").String())
	}
	// 用户消息
	if items[3].Get("type").String() != "message" || items[3].Get("role").String() != "user" {
		t.Fatal("fourth item should be user message")
	}
}

func TestStreamTranslator_FunctionCall(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 1. output_item.added: function_call
	addedEvent := []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","id":"fc_001","call_id":"call_abc","name":"get_weather","arguments":"","status":"in_progress"}
	}`)
	chunk, done := st.Translate(addedEvent)
	if done {
		t.Fatal("should not be done after output_item.added")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for function_call added")
	}
	// 验证首块包含 tool_calls
	tc := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0")
	if tc.Get("id").String() != "call_abc" {
		t.Fatalf("expected call_id call_abc, got %q", tc.Get("id").String())
	}
	if tc.Get("function.name").String() != "get_weather" {
		t.Fatalf("expected function name get_weather, got %q", tc.Get("function.name").String())
	}
	if tc.Get("index").Int() != 0 {
		t.Fatalf("expected index 0, got %d", tc.Get("index").Int())
	}

	// 2. function_call_arguments.delta
	deltaEvent := []byte(`{
		"type":"response.function_call_arguments.delta",
		"item_id":"fc_001",
		"output_index":0,
		"delta":"{\"city\":"
	}`)
	chunk, done = st.Translate(deltaEvent)
	if done {
		t.Fatal("should not be done after arguments delta")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for arguments delta")
	}
	argsDelta := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments").String()
	if argsDelta != `{"city":` {
		t.Fatalf("expected arguments delta, got %q", argsDelta)
	}

	// 3. function_call_arguments.done
	doneEvent := []byte(`{
		"type":"response.function_call_arguments.done",
		"item_id":"fc_001",
		"output_index":0,
		"arguments":"{\"city\":\"NYC\"}"
	}`)
	chunk, done = st.Translate(doneEvent)
	if done || chunk != nil {
		t.Fatal("function_call_arguments.done should be ignored")
	}

	// 4. response.completed
	completedEvent := []byte(`{
		"type":"response.completed",
		"response":{
			"usage":{"input_tokens":10,"output_tokens":5},
			"output":[{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"NYC\"}"}]
		}
	}`)
	chunk, done = st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done after response.completed")
	}
	if chunk == nil {
		t.Fatal("should emit final chunk")
	}
	finishReason := gjson.GetBytes(chunk, "choices.0.finish_reason").String()
	if finishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", finishReason)
	}

	if !st.HasToolCalls {
		t.Fatal("HasToolCalls should be true")
	}
}

func TestStreamTranslator_TextOnly(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 文本 delta
	textEvent := []byte(`{"type":"response.output_text.delta","delta":"Hello"}`)
	chunk, done := st.Translate(textEvent)
	if done {
		t.Fatal("should not be done")
	}
	if chunk == nil {
		t.Fatal("should emit chunk")
	}
	if gjson.GetBytes(chunk, "choices.0.delta.content").String() != "Hello" {
		t.Fatal("content mismatch")
	}

	// completed
	completedEvent := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3}}}`)
	chunk, done = st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done")
	}
	finishReason := gjson.GetBytes(chunk, "choices.0.finish_reason").String()
	if finishReason != "stop" {
		t.Fatalf("expected finish_reason stop for text-only, got %q", finishReason)
	}

	if st.HasToolCalls {
		t.Fatal("HasToolCalls should be false for text-only")
	}
}

func TestStreamTranslator_CachedTokenDetails(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	completedEvent := []byte(`{
		"type":"response.completed",
		"response":{
			"usage":{
				"input_tokens":12,
				"output_tokens":4,
				"input_tokens_details":{"cached_tokens":7}
			}
		}
	}`)

	chunk, done := st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done")
	}
	if got := gjson.GetBytes(chunk, "usage.cached_tokens").Int(); got != 7 {
		t.Fatalf("usage.cached_tokens = %d, want 7; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(chunk, "usage.prompt_tokens_details.cached_tokens").Int(); got != 7 {
		t.Fatalf("usage.prompt_tokens_details.cached_tokens = %d, want 7; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(chunk, "usage.input_tokens_details.cached_tokens").Int(); got != 7 {
		t.Fatalf("usage.input_tokens_details.cached_tokens = %d, want 7; chunk=%s", got, chunk)
	}
}

func TestStreamTranslator_MultipleFunctionCalls(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 第一个 function call
	event1 := []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","id":"fc_001","call_id":"call_1","name":"func_a","arguments":""}
	}`)
	chunk, _ := st.Translate(event1)
	if gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.index").Int() != 0 {
		t.Fatal("first tool call should have index 0")
	}

	// 第二个 function call
	event2 := []byte(`{
		"type":"response.output_item.added",
		"output_index":1,
		"item":{"type":"function_call","id":"fc_002","call_id":"call_2","name":"func_b","arguments":""}
	}`)
	chunk, _ = st.Translate(event2)
	if gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.index").Int() != 1 {
		t.Fatal("second tool call should have index 1")
	}

	if st.nextIdx != 2 {
		t.Fatalf("expected nextIdx 2, got %d", st.nextIdx)
	}
}

func TestExtractToolCallsFromOutput(t *testing.T) {
	event := []byte(`{
		"type":"response.completed",
		"response":{
			"output":[
				{"type":"message","content":[{"type":"output_text","text":"hi"}]},
				{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"NYC\"}"},
				{"type":"function_call","call_id":"call_2","name":"get_time","arguments":"{}"}
			],
			"usage":{"input_tokens":10,"output_tokens":5}
		}
	}`)

	tcs := ExtractToolCallsFromOutput(event)
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
	if tcs[0].ID != "call_1" || tcs[0].Name != "get_weather" {
		t.Fatalf("first tool call mismatch: %+v", tcs[0])
	}
	if tcs[1].ID != "call_2" || tcs[1].Name != "get_time" {
		t.Fatalf("second tool call mismatch: %+v", tcs[1])
	}
}

func TestNormalizeResponsesCompactionItemsConvertsToDeveloperMessage(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello from earlier"},
			{"type":"compaction","summary":"用户问候并讨论了 X 主题"},
			{"type":"message","role":"user","content":"继续上面的话题"}
		]
	}`)

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !normalizeResponsesCompactionItems(body) {
		t.Fatal("expected modification, got false")
	}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d: %s", len(items), got)
	}

	if items[0].Get("type").String() != "message" || items[0].Get("role").String() != "user" {
		t.Fatalf("item 0 should be untouched user message: %s", items[0].Raw)
	}
	if items[2].Get("type").String() != "message" || items[2].Get("role").String() != "user" {
		t.Fatalf("item 2 should be untouched user message: %s", items[2].Raw)
	}

	converted := items[1]
	if converted.Get("type").String() != "message" {
		t.Fatalf("compaction item should become message, got type=%q", converted.Get("type").String())
	}
	if converted.Get("role").String() != "developer" {
		t.Fatalf("compaction item should use developer role, got %q", converted.Get("role").String())
	}
	contentParts := converted.Get("content").Array()
	if len(contentParts) != 1 {
		t.Fatalf("expected 1 content part, got %d: %s", len(contentParts), converted.Raw)
	}
	if contentParts[0].Get("type").String() != "input_text" {
		t.Fatalf("content part type should be input_text, got %q", contentParts[0].Get("type").String())
	}
	text := contentParts[0].Get("text").String()
	if !strings.HasPrefix(text, "[Conversation summary from earlier turns]") {
		t.Fatalf("text should carry summary prefix, got %q", text)
	}
	if !strings.Contains(text, "用户问候并讨论了 X 主题") {
		t.Fatalf("text should contain original summary, got %q", text)
	}
}

func TestNormalizeResponsesCompactionItemsDropsEmptySummary(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"keep me"},
			{"type":"compaction","summary":""},
			{"type":"compaction"},
			{"type":"compaction","summary":"   "},
			{"type":"message","role":"user","content":"keep me too"}
		]
	}`)

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !normalizeResponsesCompactionItems(body) {
		t.Fatal("expected modification, got false")
	}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 2 {
		t.Fatalf("empty/missing-summary compaction items should be dropped, got %d items: %s", len(items), got)
	}
	for i, item := range items {
		if item.Get("type").String() != "message" || item.Get("role").String() != "user" {
			t.Fatalf("remaining item %d should be original user message, got %s", i, item.Raw)
		}
	}
}

func TestPrepareResponsesBodyHandlesMultipleCompactionItems(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"compaction","summary":"first summary"},
			{"type":"message","role":"user","content":"middle"},
			{"type":"compaction","summary":"second summary"},
			{"type":"message","role":"user","content":"latest"}
		]
	}`)

	codexBody, _ := PrepareResponsesBody(raw)

	items := gjson.GetBytes(codexBody, "input").Array()
	if len(items) != 4 {
		t.Fatalf("expected 4 items after normalization, got %d: %s", len(items), codexBody)
	}

	expected := []struct {
		role    string
		summary string
	}{
		{"developer", "first summary"},
		{"user", ""},
		{"developer", "second summary"},
		{"user", ""},
	}
	for i, want := range expected {
		item := items[i]
		if item.Get("type").String() != "message" {
			t.Fatalf("item %d should be message, got type=%q", i, item.Get("type").String())
		}
		if item.Get("role").String() != want.role {
			t.Fatalf("item %d role = %q, want %q", i, item.Get("role").String(), want.role)
		}
		if want.summary != "" {
			text := item.Get("content.0.text").String()
			if !strings.HasPrefix(text, "[Conversation summary from earlier turns]") {
				t.Fatalf("item %d missing summary prefix, got %q", i, text)
			}
			if !strings.Contains(text, want.summary) {
				t.Fatalf("item %d should contain %q, got %q", i, want.summary, text)
			}
		}
	}

	if gjson.GetBytes(codexBody, "input.0.type").String() == "compaction" ||
		gjson.GetBytes(codexBody, "input.2.type").String() == "compaction" {
		t.Fatalf("compaction type should not survive in upstream body: %s", codexBody)
	}
}
