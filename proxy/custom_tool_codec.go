package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

const anthropicCustomToolIDPrefix = "toolu_c2custom_"

// Messages has only JSON tool_use input. The call ID carries an explicit,
// reversible type tag, avoiding guesses based on a tool's name or JSON fields.
func anthropicCustomToolID(callID, namespace string) string {
	raw, _ := json.Marshal([2]string{callID, namespace})
	return anthropicCustomToolIDPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func parseAnthropicCustomToolID(id string) (callID, namespace string, ok bool) {
	encoded, found := strings.CutPrefix(id, anthropicCustomToolIDPrefix)
	if !found || len(encoded) > 8192 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	var parts []string
	if err != nil || json.Unmarshal(raw, &parts) != nil || len(parts) != 2 || parts[0] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func wrappedCustomToolInput(input string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"input": input})
	return raw
}

func customToolInputFragment(input string) string {
	raw, _ := json.Marshal(input)
	return string(raw[1 : len(raw)-1])
}

func validateAnthropicCustomToolHistory(messages []anthropicMessage) error {
	for _, message := range messages {
		var blocks []anthropicContentBlock
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type != "tool_use" {
				continue
			}
			if _, _, custom := parseAnthropicCustomToolID(block.ID); custom && gjson.GetBytes(block.Input, "input").Type != gjson.String {
				return fmt.Errorf("custom tool %q requires a string input field", block.Name)
			}
		}
	}
	return nil
}

// Restore only tools explicitly identified by custom call history and declared
// with the JSON wrapper schema. An ordinary function with an input field keeps
// its own JSON contract when no custom type tag is present.
func restoreAnthropicCustomToolDeclarations(body map[string]any) {
	input, _ := body["input"].([]any)
	names := make(map[string]string)
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok && item["type"] == "custom_tool_call" {
			name, _ := item["name"].(string)
			namespace, _ := item["namespace"].(string)
			names[name] = namespace
		}
	}
	tools, _ := body["tools"].([]any)
	namespaceGroups := make(map[string]map[string]any)
	var rebuilt []any
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			rebuilt = append(rebuilt, raw)
			continue
		}
		name, _ := tool["name"].(string)
		namespace, custom := names[name]
		if !custom {
			rebuilt = append(rebuilt, raw)
			continue
		}
		params, _ := tool["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		field, _ := props["input"].(map[string]any)
		if len(props) != 1 || field["type"] != "string" {
			rebuilt = append(rebuilt, raw)
			continue
		}
		tool["type"], tool["format"] = "custom", map[string]any{"type": "text"}
		delete(tool, "parameters")
		delete(tool, "strict")
		if namespace == "" {
			rebuilt = append(rebuilt, tool)
		} else {
			group := namespaceGroups[namespace]
			if group == nil {
				group = map[string]any{"type": "namespace", "name": namespace, "tools": []any{}}
				namespaceGroups[namespace] = group
				rebuilt = append(rebuilt, group)
			}
			group["tools"] = append(group["tools"].([]any), tool)
		}
		if choice, ok := body["tool_choice"].(map[string]any); ok {
			function, _ := choice["function"].(map[string]any)
			if choice["name"] == name || function["name"] == name {
				choice["type"], choice["name"] = "custom", name
				delete(choice, "function")
				if namespace != "" {
					choice["namespace"] = namespace
				}
			}
		}
	}
	if len(tools) > 0 {
		body["tools"] = rebuilt
	}
}

func (t *anthropicStreamTranslator) customToolInputDelta(delta string) []anthropicStreamEvent {
	if t.currentToolInputFinalized {
		t.toolInputError = fmt.Errorf("custom tool input continued after the done event")
		return nil
	}
	if len(delta) > responseCacheMaxEntry-t.currentToolInputBuffer.Len() {
		t.toolInputError = fmt.Errorf("custom tool input exceeds the size limit")
		return nil
	}
	t.currentToolInputBuffer.WriteString(delta)
	return t.contentBlockDelta(anthropicDelta{Type: "input_json_delta", PartialJSON: customToolInputFragment(delta)})
}

func (t *anthropicStreamTranslator) finishCustomToolInput(final gjson.Result) []anthropicStreamEvent {
	current := t.currentToolInputBuffer.String()
	candidate := current
	if final.Exists() {
		if final.Type != gjson.String {
			t.toolInputError = fmt.Errorf("custom tool input must be text")
			return nil
		}
		candidate = final.String()
	}
	if !strings.HasPrefix(candidate, current) {
		t.toolInputError = fmt.Errorf("custom tool input does not match streamed deltas")
		return nil
	}
	var events []anthropicStreamEvent
	if candidate != current {
		events = append(events, t.customToolInputDelta(candidate[len(current):])...)
	}
	if t.toolInputError != nil || t.currentToolInputFinalized {
		return events
	}
	t.currentToolInputFinalized = true
	return append(events, t.contentBlockDelta(anthropicDelta{Type: "input_json_delta", PartialJSON: `"}`})...)
}

// Custom input is opaque text, even when that text happens to be valid JSON.
type customToolCallData struct {
	Name  string `json:"name,omitempty"`
	Input string `json:"input"`
}

func lowerChatCustomTool(tool map[string]any) map[string]any {
	if tool["type"] != "custom" {
		return tool
	}
	nested, ok := tool["custom"].(map[string]any)
	if !ok {
		return tool
	}
	lowered := make(map[string]any, len(nested)+1)
	for key, value := range nested {
		lowered[key] = value
	}
	lowered["type"] = "custom"
	return lowered
}

func newCustomToolChunk(id, model string, created int64, index int, callID, name, input, namespace string) []byte {
	tool := toolCallDelta{Index: index, ID: callID, Custom: &customToolCallData{Name: name, Input: input}, Namespace: namespace}
	role := ""
	if callID != "" || name != "" {
		tool.Type = "custom"
		role = "assistant"
	}
	chunk := openAIStreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []streamChoice{{Index: 0, Delta: &streamDelta{Role: role, ToolCalls: []toolCallDelta{tool}}}}}
	raw, _ := json.Marshal(chunk)
	return raw
}

func (st *StreamTranslator) customToolInput(index int) string {
	if buffer := st.customToolInputs[index]; buffer != nil {
		return buffer.String()
	}
	return ""
}

func (st *StreamTranslator) appendCustomToolInput(index int, delta string) ([]byte, bool) {
	if st.toolCallFinalized[index] {
		return st.failToolArguments(index, "continued after the done event")
	}
	buffer := st.customToolInputs[index]
	if buffer == nil {
		buffer = &strings.Builder{}
		st.customToolInputs[index] = buffer
	}
	if len(delta) > responseCacheMaxEntry-buffer.Len() {
		return st.failToolArguments(index, "exceed the tool input size limit")
	}
	buffer.WriteString(delta)
	return newCustomToolChunk(st.ChunkID, st.Model, st.Created, index, "", "", delta, ""), false
}

func (st *StreamTranslator) finalizeCustomToolInput(index int, final gjson.Result) ([]byte, bool) {
	current := st.customToolInput(index)
	candidate := current
	if final.Exists() {
		if final.Type != gjson.String {
			return st.failToolArguments(index, "must contain text input")
		}
		candidate = final.String()
	}
	if len(candidate) > responseCacheMaxEntry {
		return st.failToolArguments(index, "exceed the tool input size limit")
	}
	if !strings.HasPrefix(candidate, current) {
		return st.failToolArguments(index, "do not match the streamed deltas")
	}
	if candidate == current {
		st.toolCallFinalized[index] = true
		return nil, false
	}
	suffix := candidate[len(current):]
	chunk, done := st.appendCustomToolInput(index, suffix)
	st.toolCallFinalized[index] = true
	return chunk, done
}
