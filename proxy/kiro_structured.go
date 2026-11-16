package proxy

import (
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"
)

// 结构化输出模拟（output_config.format = json_schema）：Kiro 没有原生 JSON Schema 模式，
// 在系统提示里要求模型只输出符合 schema 的 JSON，再从回复里取最后一个合规值替换正文。
// 移植自 kiro.rs src/anthropic/structured_{output,rewrite}.rs。

const (
	kiroStructuredMarker     = "[Structured output requirement]"
	kiroStructuredMaxDepth   = 64
	kiroStructuredMaxSteps   = 100_000
	kiroStructuredMaxFailure = 2000
)

// kiroInjectStructuredInstruction 返回请求的 JSON Schema，并把要求追加进 system。
func kiroInjectStructuredInstruction(req *kiroInboundRequest) map[string]any {
	if req.OutputConfig == nil || len(req.OutputConfig.Format) == 0 {
		return nil
	}
	var format struct {
		Type   string         `json:"type"`
		Schema map[string]any `json:"schema"`
	}
	if json.Unmarshal(req.OutputConfig.Format, &format) != nil || format.Type != "json_schema" || format.Schema == nil {
		return nil
	}
	pretty, _ := json.MarshalIndent(format.Schema, "", "  ")
	instruction := kiroStructuredMarker + "\nRespond with ONLY a single JSON value that validates against the JSON Schema below. Do not include any prose, explanation, markdown, or code fences, and nothing before or after the JSON value.\n\nJSON Schema:\n" + string(pretty)
	blocks := []map[string]any{}
	for _, text := range req.systemTexts() {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	req.System, _ = json.Marshal(append(blocks, map[string]any{"type": "text", "text": instruction}))
	return format.Schema
}

type kiroSchemaValidator struct {
	root      any
	remaining int
	// strict 额外检查数值范围、长度、数量等约束（kiro.rs 忽略这些关键字），只用于在候选里优先挑完全合规的。
	strict bool
}

func (v *kiroSchemaValidator) conforms(schema, value any) bool {
	return v.at(schema, value, 0) && v.remaining > 0
}

func (v *kiroSchemaValidator) at(schema, value any, depth int) bool {
	if depth > kiroStructuredMaxDepth || v.remaining <= 0 {
		return false
	}
	v.remaining--
	switch typed := schema.(type) {
	case bool:
		return typed
	case map[string]any:
		return v.object(typed, value, depth)
	}
	return true
}

func (v *kiroSchemaValidator) object(obj map[string]any, value any, depth int) bool {
	sub := func(schema, item any) bool { return v.at(schema, item, depth+1) }
	if ref, ok := obj["$ref"].(string); ok {
		if pointer, ok := strings.CutPrefix(ref, "#"); ok {
			if target, found := kiroJSONPointer(v.root, pointer); found && !sub(target, value) {
				return false
			}
		}
	}
	switch kind := obj["type"].(type) {
	case string:
		if !kiroTypeMatches(kind, value) {
			return false
		}
	case []any:
		matched := false
		for _, name := range kind {
			if text, ok := name.(string); ok && kiroTypeMatches(text, value) {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	if options, ok := obj["enum"].([]any); ok {
		matched := false
		for _, option := range options {
			matched = matched || kiroJSONEqual(option, value)
		}
		if !matched {
			return false
		}
	}
	if constant, ok := obj["const"]; ok && !kiroJSONEqual(constant, value) {
		return false
	}
	if fields, ok := value.(map[string]any); ok {
		required, _ := obj["required"].([]any)
		for _, key := range required {
			if name, ok := key.(string); ok {
				if _, present := fields[name]; !present {
					return false
				}
			}
		}
		props, _ := obj["properties"].(map[string]any)
		for key, propSchema := range props {
			if item, present := fields[key]; present && !sub(propSchema, item) {
				return false
			}
		}
		if _, hasPattern := obj["patternProperties"]; !hasPattern {
			switch extra := obj["additionalProperties"].(type) {
			case bool:
				if !extra {
					for key := range fields {
						if _, declared := props[key]; !declared {
							return false
						}
					}
				}
			case map[string]any:
				for key, item := range fields {
					if _, declared := props[key]; !declared && !sub(extra, item) {
						return false
					}
				}
			}
		}
	}
	if items, ok := value.([]any); ok {
		switch itemSchema := obj["items"].(type) {
		case map[string]any, bool:
			for _, item := range items {
				if !sub(itemSchema, item) {
					return false
				}
			}
		}
	}
	if list, ok := obj["allOf"].([]any); ok {
		for _, schema := range list {
			if !sub(schema, value) {
				return false
			}
		}
	}
	if list, ok := obj["anyOf"].([]any); ok {
		matched := false
		for _, schema := range list {
			if sub(schema, value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if list, ok := obj["oneOf"].([]any); ok {
		count := 0
		for _, schema := range list {
			if sub(schema, value) {
				count++
			}
		}
		if count != 1 {
			return false
		}
	}
	if v.strict && !kiroSchemaConstraints(obj, value) {
		return false
	}
	return true
}

// kiroSchemaConstraints 检查 minimum/maxLength/minItems 这类约束；pattern 不查，Go 正则与 ECMA 方言不一致。
func kiroSchemaConstraints(obj map[string]any, value any) bool {
	switch typed := value.(type) {
	case json.Number:
		n, err := typed.Float64()
		if err != nil {
			return true
		}
		// draft-04 的 exclusiveMinimum/exclusiveMaximum 是修饰 minimum/maximum 的布尔值。
		exclusiveMin, _ := obj["exclusiveMinimum"].(bool)
		exclusiveMax, _ := obj["exclusiveMaximum"].(bool)
		if limit, ok := kiroSchemaNumber(obj, "minimum"); ok && (n < limit || exclusiveMin && n == limit) {
			return false
		}
		if limit, ok := kiroSchemaNumber(obj, "maximum"); ok && (n > limit || exclusiveMax && n == limit) {
			return false
		}
		if limit, ok := kiroSchemaNumber(obj, "exclusiveMinimum"); ok && n <= limit {
			return false
		}
		if limit, ok := kiroSchemaNumber(obj, "exclusiveMaximum"); ok && n >= limit {
			return false
		}
		if divisor, ok := kiroSchemaNumber(obj, "multipleOf"); ok && divisor > 0 {
			quotient := n / divisor
			if math.Abs(quotient-math.Round(quotient)) > 1e-9 {
				return false
			}
		}
	case string:
		return kiroSchemaWithin(obj, "minLength", "maxLength", utf8.RuneCountInString(typed))
	case []any:
		if unique, _ := obj["uniqueItems"].(bool); unique {
			seen := make(map[string]struct{}, len(typed))
			for _, item := range typed {
				key := kiroMarshalString(item)
				if number, ok := item.(json.Number); ok {
					if f, err := number.Float64(); err == nil {
						key = kiroMarshalString(f)
					}
				}
				if _, dup := seen[key]; dup {
					return false
				}
				seen[key] = struct{}{}
			}
		}
		return kiroSchemaWithin(obj, "minItems", "maxItems", len(typed))
	case map[string]any:
		return kiroSchemaWithin(obj, "minProperties", "maxProperties", len(typed))
	}
	return true
}

func kiroSchemaWithin(obj map[string]any, minKey, maxKey string, count int) bool {
	if limit, ok := kiroSchemaNumber(obj, minKey); ok && float64(count) < limit {
		return false
	}
	if limit, ok := kiroSchemaNumber(obj, maxKey); ok && float64(count) > limit {
		return false
	}
	return true
}

func kiroSchemaNumber(obj map[string]any, key string) (float64, bool) {
	number, ok := obj[key].(json.Number)
	if !ok {
		return 0, false
	}
	value, err := number.Float64()
	return value, err == nil
}

func kiroJSONPointer(root any, pointer string) (any, bool) {
	if pointer == "" {
		return root, true
	}
	current := root
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[part]; !ok {
			return nil, false
		}
	}
	return current, true
}

func kiroTypeMatches(name string, value any) bool {
	switch name {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		if _, err := number.Int64(); err == nil {
			return true
		}
		f, err := number.Float64()
		return err == nil && !math.IsInf(f, 0) && f == math.Trunc(f)
	}
	return false
}

func kiroJSONEqual(a, b any) bool {
	left, lok := a.(json.Number)
	right, rok := b.(json.Number)
	if lok || rok {
		if !lok {
			left = json.Number(strings.TrimSpace(kiroMarshalString(a)))
		}
		if !rok {
			right = json.Number(strings.TrimSpace(kiroMarshalString(b)))
		}
		lf, lerr := left.Float64()
		rf, rerr := right.Float64()
		return lerr == nil && rerr == nil && lf == rf
	}
	return kiroMarshalString(a) == kiroMarshalString(b)
}

func kiroMarshalString(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func kiroDecodeJSONNumber(text string) (any, int, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, int(decoder.InputOffset()), err
}

// kiroExtractConformingJSON 先试整段（可去掉代码围栏），否则逐个 {/[ 起点尝试解析，取最后一个合规值；
// 有满足全部约束的候选时优先取它，没有就按 kiro.rs 的规则取最后一个基本合规的。
func kiroExtractConformingJSON(text string, schema map[string]any) (string, bool) {
	schemaNumbers, _, _ := kiroDecodeJSONNumber(kiroMarshalString(schema))
	validator := &kiroSchemaValidator{root: schemaNumbers, remaining: kiroStructuredMaxSteps}
	strict := &kiroSchemaValidator{root: schemaNumbers, remaining: kiroStructuredMaxSteps, strict: true}
	whole := kiroStripFence(strings.TrimSpace(text))
	if value, offset, err := kiroDecodeJSONNumber(whole); err == nil && strings.TrimSpace(whole[offset:]) == "" && validator.conforms(schemaNumbers, value) {
		return kiroMarshalString(value), true
	}
	var last, lastStrict any
	found, foundStrict, failed := false, false, 0
	for pos := 0; pos < len(text) && validator.remaining > 0; {
		rel := strings.IndexAny(text[pos:], "{[")
		if rel < 0 {
			break
		}
		start := pos + rel
		value, offset, err := kiroDecodeJSONNumber(text[start:])
		if err != nil {
			if failed++; failed >= kiroStructuredMaxFailure {
				break
			}
			pos = start + 1
			continue
		}
		if validator.conforms(schemaNumbers, value) {
			last, found = value, true
			if strict.conforms(schemaNumbers, value) {
				lastStrict, foundStrict = value, true
			}
		}
		pos = start + max(offset, 1)
	}
	if foundStrict {
		return kiroMarshalString(lastStrict), true
	}
	if !found {
		return "", false
	}
	return kiroMarshalString(last), true
}

func kiroStripFence(text string) string {
	rest, ok := strings.CutPrefix(text, "```")
	if !ok {
		return text
	}
	rest, ok = strings.CutSuffix(rest, "```")
	if !ok {
		return text
	}
	if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
		return strings.TrimSpace(rest[newline+1:])
	}
	return strings.TrimSpace(rest)
}

// kiroRewriteStructuredEvents 把所有文本块合并替换成一个只含合规 JSON 的文本块，其余块重新编号；
// 有错误事件或找不到合规 JSON 时原样返回。
func kiroRewriteStructuredEvents(events []kiroSSEEvent, schema map[string]any) []kiroSSEEvent {
	textBlocks := map[int]bool{}
	for _, event := range events {
		if event.Event == "error" {
			return events
		}
		if event.Event == "content_block_start" {
			if block, _ := event.Data["content_block"].(map[string]any); block["type"] == "text" {
				textBlocks[event.Data["index"].(int)] = true
			}
		}
	}
	var text strings.Builder
	for _, event := range events {
		if event.Event != "content_block_delta" || !textBlocks[event.Data["index"].(int)] {
			continue
		}
		if delta, _ := event.Data["delta"].(map[string]any); delta["type"] == "text_delta" {
			text.WriteString(delta["text"].(string))
		}
	}
	jsonText, ok := kiroExtractConformingJSON(text.String(), schema)
	if !ok {
		return events
	}
	remap, next, replaced := map[int]int{}, 0, false
	out := make([]kiroSSEEvent, 0, len(events))
	for _, event := range events {
		old, indexed := event.Data["index"].(int)
		if !indexed {
			out = append(out, event)
			continue
		}
		if textBlocks[old] {
			if event.Event == "content_block_start" && !replaced {
				replaced = true
				out = append(out,
					kiroSSEEvent{"content_block_start", map[string]any{"type": "content_block_start", "index": next, "content_block": map[string]any{"type": "text", "text": ""}}},
					kiroSSEEvent{"content_block_delta", map[string]any{"type": "content_block_delta", "index": next, "delta": map[string]any{"type": "text_delta", "text": jsonText}}},
					kiroBlockStopEvent(next))
				next++
			}
			continue
		}
		index, seen := remap[old]
		if !seen {
			index = next
			remap[old] = index
			next++
		}
		copied := map[string]any{}
		for key, value := range event.Data {
			copied[key] = value
		}
		copied["index"] = index
		out = append(out, kiroSSEEvent{event.Event, copied})
	}
	return out
}
