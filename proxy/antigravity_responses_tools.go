package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Gemini only accepts flat functions. Reuse the existing Responses tool codec,
// without applying Grok-specific schema or conversation normalization.
func prepareAntigravityResponsesTools(body []byte) ([]byte, map[string]grokNsIdentity) {
	if !bytes.Contains(body, []byte(`"namespace"`)) &&
		!bytes.Contains(body, []byte(`"custom"`)) &&
		!bytes.Contains(body, []byte(`"custom_tool_call`)) &&
		!bytes.Contains(body, []byte(`"tool_search`)) &&
		!bytes.Contains(body, []byte(`"additional_tools"`)) {
		return body, nil
	}
	body, _ = liftGrokAdditionalTools(body)
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return body, nil
	}
	registerFlat, _ := newGrokAliasRegister()
	aliases := make(map[string]grokNsIdentity)
	registered := make(map[grokNsIdentity]string)
	used := make(map[string]string)
	register := func(namespace, name string, custom, search bool) string {
		identity := grokNsIdentity{Namespace: namespace, Name: name, Custom: custom, ToolSearch: search}
		if wire, ok := registered[identity]; ok {
			return wire
		}
		wire := antigravitySanitizeFunctionName(registerFlat(namespace, name, custom, search))
		key := grokJSONString(identity)
		if used[wire] != "" {
			wire = antigravityDisambiguateFunctionName(wire, key, used)
		}
		registered[identity], used[wire] = wire, key
		if namespace != "" || custom || search || wire != name {
			aliases[wire] = identity
		}
		return wire
	}
	rawTools, _ := request["tools"].([]any)
	tools := make([]any, 0, len(rawTools))
	emitted := make(map[string]bool)
	appendFunction := func(tool map[string]any, namespace string) {
		kind := grokNsStringField(tool, "type")
		if kind != "function" && kind != "custom" {
			return
		}
		function := tool
		if nested, ok := tool["function"].(map[string]any); ok {
			function = nested
		} else if kind == "custom" {
			if nested, ok := tool["custom"].(map[string]any); ok {
				function = make(map[string]any, len(tool)+len(nested))
				for key, value := range tool {
					function[key] = value
				}
				for key, value := range nested {
					function[key] = value
				}
			}
		}
		name := strings.TrimSpace(grokNsStringField(function, "name"))
		if name == "" && kind == "custom" {
			name = strings.TrimSpace(grokNsStringField(tool, "name"))
		}
		if name == "" {
			return
		}
		alias := register(namespace, name, kind == "custom", false)
		if emitted[alias] {
			return
		}
		emitted[alias] = true
		var converted map[string]any
		if kind == "custom" {
			converted = grokFunctionToolForCustom(function, alias)
		} else {
			converted = make(map[string]any, len(function)+1)
			for key, value := range function {
				converted[key] = value
			}
			converted["type"] = "function"
			converted["name"] = alias
			if converted["parameters"] == nil && converted["parametersJsonSchema"] == nil {
				converted["parameters"] = function["input_schema"]
			}
		}
		tools = append(tools, converted)
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch grokNsStringField(tool, "type") {
		case "tool_search":
			alias := register("", grokToolSearchProxyName, false, true)
			if !emitted[alias] {
				tools = append(tools, grokFunctionToolForToolSearch(alias))
				emitted[alias] = true
			}
		case "namespace":
			namespace := strings.TrimSpace(grokNsStringField(tool, "name"))
			children, _ := tool["tools"].([]any)
			for _, rawChild := range children {
				if child, ok := rawChild.(map[string]any); ok {
					appendFunction(child, namespace)
				}
			}
		case "function", "custom":
			appendFunction(tool, "")
		default:
			tools = append(tools, tool)
		}
	}
	if len(rawTools) > 0 {
		request["tools"] = tools
	}
	if input, ok := request["input"].([]any); ok {
		for i, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch grokNsStringField(item, "type") {
			case "function_call":
				item["name"] = register(grokNsStringField(item, "namespace"), grokNsStringField(item, "name"), false, false)
				delete(item, "namespace")
			case "custom_tool_call", "tool_search_call", "tool_search_output", "tool_search_call_output":
				input[i], _ = rebuildGrokHistoryItem(item, register)
				// The Gemini converter already handles custom outputs and must
				// retain their original type when reporting orphaned call IDs.
			}
		}
	}
	if choice, ok := request["tool_choice"].(map[string]any); ok {
		kind := grokNsStringField(choice, "type")
		switch kind {
		case "tool_search":
			request["tool_choice"] = map[string]any{
				"type": "function", "name": register("", grokToolSearchProxyName, false, true),
			}
		case "function", "custom":
			choice["name"] = register(grokNsStringField(choice, "namespace"), grokNsStringField(choice, "name"), kind == "custom", false)
			choice["type"] = "function"
			delete(choice, "namespace")
		}
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return body, nil
	}
	return encoded, aliases
}
