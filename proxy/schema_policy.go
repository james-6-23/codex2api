package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type schemaNormalizationPolicy struct {
	preserveStringLengths bool
}

// Length constraints were accepted by these exact models on Responses Lite
// WebSocket in the 2026-09-05 capture. This is not evidence for tool parameters,
// other model variants, HTTP, or other JSON Schema validation keywords.
func codexStructuredOutputSchemaPolicy(model string, websocket, lite bool) schemaNormalizationPolicy {
	if websocket && lite {
		switch strings.ToLower(strings.TrimSpace(model)) {
		case "gpt-6-astra", "gpt-5.6-luna":
			return schemaNormalizationPolicy{preserveStringLengths: true}
		}
	}
	return schemaNormalizationPolicy{}
}

// Apply after payload rules, account capability gates, and the final transport
// decision. In particular, preserved WS constraints cannot leak into HTTP
// fallback or a request whose model/Lite signal was rewritten later.
func normalizeCodexStructuredOutputForTransport(body []byte, websocket, lite bool) []byte {
	if !bytes.Contains(body, []byte(`"minLength"`)) && !bytes.Contains(body, []byte(`"maxLength"`)) && !bytes.Contains(body, []byte(`\u`)) {
		return body
	}
	if codexStructuredOutputSchemaPolicy(gjson.GetBytes(body, "model").String(), websocket, lite).preserveStringLengths {
		return body
	}
	format := gjson.GetBytes(body, "text.format")
	if !format.IsObject() {
		return body
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(format.Raw), &decoded) != nil {
		return body
	}
	modified := false
	if schema, ok := decoded["schema"].(map[string]any); ok {
		modified = stripStructuredStringLengths(schema) || modified
	}
	if nested, ok := decoded["json_schema"].(map[string]any); ok {
		if schema, ok := nested["schema"].(map[string]any); ok {
			modified = stripStructuredStringLengths(schema) || modified
		}
	}
	if !modified {
		return body
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "text.format", raw)
	if err != nil {
		return body
	}
	return updated
}

func stripStructuredStringLengths(schema map[string]any) bool {
	modified := false
	for _, key := range []string{"minLength", "maxLength"} {
		if _, exists := schema[key]; exists {
			delete(schema, key)
			modified = true
		}
	}
	forEachSubSchema(schema, func(child map[string]any) { modified = stripStructuredStringLengths(child) || modified })
	return modified
}
