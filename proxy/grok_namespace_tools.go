package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Grok（xAI）上游的 /responses 反序列化器只接受 function/web_search/x_search/… 等工具变体，
// 不认识 Codex CLI 发来的 type:"namespace" 分组工具（会 422 "unknown variant `namespace`"）。
// 这里把 namespace 工具在投递前扁平化成子 function（名字加命名空间前缀），并记录别名映射；
// 上游按扁平名回调工具时，再在响应流里反解回 {name, namespace}，让 Codex 认得。
// 逻辑对齐参考实现：请求侧展平 + 响应侧恢复的完整往返。

type grokNsIdentity struct {
	Namespace  string
	Name       string
	Custom     bool
	ToolSearch bool
}

type grokAliasRegister func(namespace, name string, custom, toolSearch bool) string

func newGrokAliasRegister() (grokAliasRegister, map[string]grokNsIdentity) {
	aliases := make(map[string]grokNsIdentity)
	registered := make(map[string]grokNsIdentity)
	register := func(namespace, name string, custom, toolSearch bool) string {
		alias := grokNamespaceAliasName(namespace, name)
		// Grok 上游保留的内置名不能作为 function 声明名出站(会 400
		// "The function name … is reserved"),统一挪到带后缀的别名上。
		if _, reserved := grokReservedUpstreamFunctionNames[alias]; reserved {
			alias += grokReservedAliasSuffix
		}
		if existing, ok := registered[alias]; ok && (existing.Namespace != namespace || existing.Name != name || existing.Custom != custom || existing.ToolSearch != toolSearch) {
			alias = grokDisambiguatedAlias(alias, namespace, name)
		}
		identity := grokNsIdentity{Namespace: namespace, Name: name, Custom: custom, ToolSearch: toolSearch}
		registered[alias] = identity
		if custom || toolSearch || namespace != "" || alias != name {
			aliases[alias] = identity
		}
		if toolSearch {
			// 模型可能凭习惯直接回调 "tool_search",补一条反解映射,
			// 保证这种回调仍能恢复成 tool_search_call。
			if _, taken := aliases["tool_search"]; !taken {
				aliases["tool_search"] = identity
			}
		}
		return alias
	}
	return register, aliases
}

// grokToolSearchProxyName 是 tool_search 桥接 function 的出站名。不能叫
// "tool_search":Grok 上游把该名保留给内置 tool_search 工具,同名声明会被
// 400 拒绝;响应侧靠别名映射把它反解回 tool_search_call,客户端无感知。
const grokToolSearchProxyName = "codex_tool_search"
const grokToolCallHardLimitBytes = 128 * 1024

// grokReservedUpstreamFunctionNames 是 Grok 上游保留、拒绝同名 function 声明的名字。
var grokReservedUpstreamFunctionNames = map[string]struct{}{"tool_search": {}}

const grokReservedAliasSuffix = "_fn"

func grokFunctionToolForToolSearch(name string) map[string]any {
	return map[string]any{
		"type": "function", "name": name,
		"description": "Search and load Codex tools, plugins, connectors, and MCP namespaces for the current task.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
			},
			"required": []string{"query"},
		},
	}
}

func grokJSONString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return "{}"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func grokToolSearchArguments(value any) any {
	text := strings.TrimSpace(grokJSONString(value))
	var decoded any
	if text != "" && json.Unmarshal([]byte(text), &decoded) == nil {
		return decoded
	}
	return text
}

func grokFunctionToolForCustom(tool map[string]any, name string) map[string]any {
	converted := map[string]any{
		"type": "function",
		"name": name,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string"},
			},
			"required":             []string{"input"},
			"additionalProperties": false,
		},
	}
	if description, ok := tool["description"].(string); ok {
		converted["description"] = description
	}
	return converted
}

// grokPermissiveObjectSchema 是无法保真转换时的降级形态:宽松 object,仅保留
// 原 schema 的 description。宁可让模型自由发挥、由客户端做最终参数校验,也不能
// 让整段对话因 schema 形态被上游 400 掐死。
func grokPermissiveObjectSchema(root map[string]any) map[string]any {
	out := map[string]any{"type": "object", "additionalProperties": true}
	if description, ok := root["description"].(string); ok && strings.TrimSpace(description) != "" {
		out["description"] = description
	}
	return out
}

// grokObjectishSchemaBranch 判断联合分支是否可并入 object 根:显式 object、
// type 数组含 "object",或未声明 type 但带 properties。
func grokObjectishSchemaBranch(branch map[string]any) bool {
	if kind, ok := branch["type"].(string); ok {
		return kind == "object"
	}
	if typeList, ok := branch["type"].([]any); ok {
		for _, item := range typeList {
			if kind, ok := item.(string); ok && kind == "object" {
				return true
			}
		}
		return false
	}
	if _, ok := branch["type"]; ok {
		return false
	}
	_, hasProperties := branch["properties"]
	return hasProperties
}

// grokSchemaTypeListHasNonObject 报告 schema 的 type 数组里是否还有 object 之外
// 的选项。折叠成单一 object 后这些选项不可表达,与丢弃非 object 联合分支适用
// 同一条 required 放宽规则。
func grokSchemaTypeListHasNonObject(schema map[string]any) bool {
	typeList, ok := schema["type"].([]any)
	if !ok {
		return false
	}
	for _, item := range typeList {
		if kind, ok := item.(string); ok && kind != "object" {
			return true
		}
	}
	return false
}

// grokLookupLocalJSONPointer 在 schema 文档内解析 "#/..." 形式的本地 JSON
// Pointer(含 ~0/~1 反转义),仅支持 map 路径——$defs/definitions 场景足够。
func grokLookupLocalJSONPointer(document map[string]any, pointer string) (map[string]any, bool) {
	if !strings.HasPrefix(pointer, "#/") {
		return nil, false
	}
	current := any(document)
	for _, rawToken := range strings.Split(strings.TrimPrefix(pointer, "#/"), "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")
		node, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = node[token]; !ok {
			return nil, false
		}
	}
	target, ok := current.(map[string]any)
	return target, ok
}

// grokDerefSchemaBranch 展开联合分支上的本地 $ref 链(深度上限防循环引用):
// Pydantic/Zod 常生成 anyOf:[{"$ref":"#/$defs/X"},{"type":"null"}] 形态,直接按
// "非 object"丢弃会丢掉整个对象定义。$ref 之外的 sibling 键覆盖在解析结果上
// (2020-12 允许 $ref 携带 sibling)。无 $ref 原样返回;解析失败返回 nil,由调用
// 方按不可合并分支处理(行为与修复前一致,不会更糟)。
func grokDerefSchemaBranch(document, branch map[string]any) map[string]any {
	current := branch
	for depth := 0; depth < 8; depth++ {
		rawRef, exists := current["$ref"]
		if !exists {
			return current
		}
		refText, ok := rawRef.(string)
		if !ok {
			return nil
		}
		target, ok := grokLookupLocalJSONPointer(document, refText)
		if !ok {
			return nil
		}
		next := make(map[string]any, len(target)+len(current))
		for key, value := range target {
			next[key] = value
		}
		for key, value := range current {
			if key != "$ref" {
				next[key] = value
			}
		}
		current = next
	}
	return nil
}

// mergeGrokUnionRootSchema 把 anyOf/oneOf/allOf 根合并成单一 object:
// properties 取各 object 分支的并集,同名属性 schema 不一致时按联合语义包成
// 嵌套 anyOf(联合)或 allOf(交),不丢弃任何分支定义;required 按语义收敛——
// 联合(任一分支成立)取交集,allOf(全部成立)取并集;非 object 分支丢弃。
// 根上的非联合 sibling 键($defs/definitions/additionalProperties 等)原样保留:
// 分支属性里的 "$ref":"#/$defs/X" 依赖根级定义容器,丢掉会产生悬空引用,上游拿到
// 不可解析的 schema 照样 400(PR #580 评审)。没有任何 object 分支时整体降级为
// 宽松 object。
func mergeGrokUnionRootSchema(root map[string]any, branches []any, requireAll bool) map[string]any {
	merged := make(map[string]any, len(root)+1)
	for key, value := range root {
		switch key {
		case "anyOf", "oneOf", "allOf", "type", "properties", "required":
			// 联合关键字被消解;type/properties/required 由下方合并逻辑重建。
			continue
		}
		merged[key] = value
	}
	merged["type"] = "object"
	// 同名属性收集所有互异的分支定义(根定义在前),合并时再决定单值还是嵌套联合。
	propertyVariants := map[string][]any{}
	appendPropertyVariant := func(key string, value any) {
		for _, existing := range propertyVariants[key] {
			if reflect.DeepEqual(existing, value) {
				return
			}
		}
		propertyVariants[key] = append(propertyVariants[key], value)
	}
	// 根自身可能就是 object schema(type:"object" 且额外挂了 anyOf/oneOf):它的
	// properties/required 在任何分支下都成立,作为合并基底且 required 恒保留。
	rootRequired := map[string]bool{}
	if rootProps, ok := root["properties"].(map[string]any); ok {
		for key, value := range rootProps {
			appendPropertyVariant(key, value)
		}
	}
	if list, ok := root["required"].([]any); ok {
		for _, item := range list {
			if key, ok := item.(string); ok {
				rootRequired[key] = true
			}
		}
	}
	var required map[string]bool
	objectBranches := 0
	droppedNonObject := false
	for _, rawBranch := range branches {
		branch, _ := rawBranch.(map[string]any)
		if branch != nil {
			branch = grokDerefSchemaBranch(root, branch)
		}
		if branch == nil || !grokObjectishSchemaBranch(branch) {
			droppedNonObject = true
			continue
		}
		// 分支 type 数组还带 null 等非 object 选项时,这些选项在合并结果里不可
		// 表达,视同丢弃过非 object 分支,触发同一条 required 放宽规则。
		if grokSchemaTypeListHasNonObject(branch) {
			droppedNonObject = true
		}
		objectBranches++
		if _, has := merged["description"]; !has {
			if description, ok := branch["description"].(string); ok && strings.TrimSpace(description) != "" {
				merged["description"] = description
			}
		}
		if props, ok := branch["properties"].(map[string]any); ok {
			for key, value := range props {
				appendPropertyVariant(key, value)
			}
		}
		branchRequired := map[string]bool{}
		if list, ok := branch["required"].([]any); ok {
			for _, item := range list {
				if key, ok := item.(string); ok {
					branchRequired[key] = true
				}
			}
		}
		if required == nil {
			required = branchRequired
		} else if requireAll {
			for key := range branchRequired {
				required[key] = true
			}
		} else {
			for key := range required {
				if !branchRequired[key] {
					delete(required, key)
				}
			}
		}
	}
	if objectBranches == 0 && len(propertyVariants) == 0 {
		return grokPermissiveObjectSchema(root)
	}
	if len(propertyVariants) > 0 {
		// 冲突属性按联合类型包成嵌套联合(嵌套联合 Grok 接受,只有根联合被拒):
		// anyOf/oneOf 用 anyOf 保住每个判别分支(如 const:"create"/const:"update"),
		// 不能先到先得地排除后续分支;allOf 用 allOf 保留全部约束。根定义在
		// anyOf 场景下随之从"恒成立"放宽为"任一成立",符合本文件宁可放宽、
		// 不禁止原本合法调用的降级方针。
		unionKeyword := "anyOf"
		if requireAll {
			unionKeyword = "allOf"
		}
		properties := make(map[string]any, len(propertyVariants))
		for key, variants := range propertyVariants {
			if len(variants) == 1 {
				properties[key] = variants[0]
				continue
			}
			properties[key] = map[string]any{unionKeyword: variants}
		}
		merged["properties"] = properties
	}
	finalRequired := map[string]bool{}
	for key := range rootRequired {
		finalRequired[key] = true
	}
	// 联合里丢弃过非 object 分支(常见是 null,表示"可空调用")时,分支侧的
	// required 不再并入:null 分支已不可表达,再强加必填会让模型无法发出
	// 原本合法的空参调用。allOf(全部成立)不受此影响。
	if requireAll || !droppedNonObject {
		for key := range required {
			finalRequired[key] = true
		}
	}
	if len(finalRequired) > 0 {
		keys := make([]string, 0, len(finalRequired))
		for key := range finalRequired {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		values := make([]any, len(keys))
		for index, key := range keys {
			values[index] = key
		}
		merged["required"] = values
	}
	return merged
}

// normalizeGrokToolParameterSchema 把函数工具参数 schema 归一成 Grok 上游接受
// 的形态:根节点必须是单一 object(上游对联合/非 object 根返回 400
// "tool parameter root must be an object type")。返回 (归一后 schema, 是否改写)。
// 合规的 object 根保持原样,嵌套结构一律不动,避免扰动上游缓存前缀。
func normalizeGrokToolParameterSchema(schema map[string]any) (map[string]any, bool) {
	if schema == nil {
		return schema, false
	}
	// 联合关键字优先于 type 判断:Grok 只要在根上看到 anyOf/oneOf 就按联合根
	// 拒绝,即使同时声明了 type:"object"(2026-08-26 线上实测)。
	for _, key := range []string{"anyOf", "oneOf"} {
		if branches, ok := schema[key].([]any); ok && len(branches) > 0 {
			return mergeGrokUnionRootSchema(schema, branches, false), true
		}
	}
	if branches, ok := schema["allOf"].([]any); ok && len(branches) > 0 {
		return mergeGrokUnionRootSchema(schema, branches, true), true
	}
	if typeList, ok := schema["type"].([]any); ok {
		hasObject := false
		for _, item := range typeList {
			if kind, ok := item.(string); ok && kind == "object" {
				hasObject = true
				break
			}
		}
		if !hasObject {
			return grokPermissiveObjectSchema(schema), true
		}
		out := make(map[string]any, len(schema))
		for key, value := range schema {
			out[key] = value
		}
		out["type"] = "object"
		// type:["object","null"] 与 anyOf:[object,{type:"null"}] 语义等价,折叠后
		// 适用同一条规则:非 object 选项(常见是 null,表示可空参调用)不可表达时
		// 放弃 required 强制,否则模型无法发出原本合法的空参调用。
		if grokSchemaTypeListHasNonObject(schema) {
			delete(out, "required")
		}
		return out, true
	}
	if kind, ok := schema["type"].(string); ok {
		if kind == "object" {
			return schema, false
		}
		return grokPermissiveObjectSchema(schema), true
	}
	if _, ok := schema["properties"]; ok {
		out := make(map[string]any, len(schema)+1)
		for key, value := range schema {
			out[key] = value
		}
		out["type"] = "object"
		return out, true
	}
	if len(schema) == 0 {
		return map[string]any{"type": "object"}, true
	}
	return grokPermissiveObjectSchema(schema), true
}

func normalizeGrokFunctionTool(tool map[string]any, name string) map[string]any {
	converted := make(map[string]any, len(tool))
	for key, value := range tool {
		converted[key] = value
	}
	converted["type"] = "function"
	converted["name"] = name
	if _, ok := converted["parameters"]; !ok {
		if schema, exists := converted["inputSchema"]; exists {
			converted["parameters"] = schema
		} else if schema, exists := converted["input_schema"]; exists {
			converted["parameters"] = schema
		}
	}
	for _, key := range []string{
		"inputSchema", "input_schema", "deferLoading", "defer_loading",
		"allowedCallers", "allowed_callers", "outputSchema", "output_schema",
		"metadata", "x_provider", "cache_control", "cacheControl",
	} {
		delete(converted, key)
	}
	if params, ok := converted["parameters"].(map[string]any); ok {
		if normalized, changed := normalizeGrokToolParameterSchema(params); changed {
			converted["parameters"] = normalized
		}
	} else if flag, ok := converted["parameters"].(bool); ok {
		// JSON Schema 布尔形态:true=任意输入合法,false=任意输入非法。原样透传
		// 都会命中上游 "root must be an object type"。true 降级为宽松 object;
		// false 本就没有合法调用,给出空封闭 object(最接近的可表达形态),
		// 参数校验仍由客户端兜底。
		if flag {
			converted["parameters"] = grokPermissiveObjectSchema(nil)
		} else {
			converted["parameters"] = map[string]any{"type": "object", "additionalProperties": false}
		}
	}
	return converted
}

func marshalGrokCustomToolArguments(input string) string {
	encoded, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return `{"input":""}`
	}
	return string(encoded)
}

func unwrapGrokCustomToolArguments(arguments string) string {
	var decoded struct {
		Input string `json:"input"`
	}
	if json.Unmarshal([]byte(arguments), &decoded) == nil {
		return decoded.Input
	}
	return arguments
}

// grokNamespaceAliasName 生成扁平化后的函数名：namespace + name（命名空间已以 "__" 结尾时不再补分隔符）。
func grokNamespaceAliasName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	sep := "__"
	if strings.HasSuffix(namespace, sep) {
		sep = ""
	}
	return namespace + sep + name
}

func grokNsStringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// grokDisambiguatedAlias 处理极少见的别名撞名：不同 (namespace, name) 展平出同一个
// 扁平名时追加短哈希区分。
func grokDisambiguatedAlias(alias, namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	return alias + "__" + hex.EncodeToString(sum[:])[:8]
}

// grokWebSearchTypes 是 Codex 会声明、但 Grok 上游归一为 "web_search" 的搜索工具变体。
var grokWebSearchTypes = map[string]struct{}{
	"web_search":                    {},
	"web_search_preview":            {},
	"web_search_preview_2025_03_11": {},
	"web_search_2025_08_26":         {},
}

// grokNativeHistoryFields 是 Grok 上游各历史项类型接受的字段白名单（据官方 grok-pager 0.2.106
// 实际请求 + 参考实现的原生 InputItem 契约）。重建时只保留这些字段、丢弃 Codex 扩展字段与 null。
var grokNativeHistoryFields = map[string][]string{
	"message":               {"type", "role", "content"},
	"function_call":         {"type", "call_id", "name", "arguments"},
	"function_call_output":  {"type", "call_id", "output"},
	"reasoning":             {"type", "id", "summary", "content", "encrypted_content"},
	"file_search_call":      {"type", "id", "queries", "status", "results"},
	"web_search_call":       {"type", "action", "id", "status"},
	"image_generation_call": {"type", "id", "result", "status"},
	"code_interpreter_call": {"type", "code", "container_id", "id", "outputs", "status"},
	"shell_call":            {"type", "id", "call_id", "action", "status", "environment"},
	"mcp_list_tools":        {"type", "id", "server_label", "tools", "error"},
	"mcp_approval_request":  {"type", "arguments", "id", "name", "server_label"},
	"mcp_approval_response": {"type", "approval_request_id", "approve", "id", "reason"},
	"mcp_call":              {"type", "arguments", "id", "name", "server_label", "approval_request_id", "error", "output", "status"},
}

// rebuildGrokHistoryItem 把单个 input[] 历史项重建为 Grok 原生字段集：只保留白名单字段、丢弃 null，
// function_call 携带 namespace 的改写成扁平名，外来 compaction 项换成边界消息。
// 返回 (重建后的项, 是否发生改写)；未知类型原样返回不改。
func rebuildGrokHistoryItem(item map[string]any, register grokAliasRegister) (map[string]any, bool) {
	itemType := grokNsStringField(item, "type")
	if itemType == "" && grokNsStringField(item, "role") != "" {
		itemType = "message" // Codex 可能省略带 role 消息的 type
	}
	if itemType == "compaction" {
		// 外来 compaction 密文 Grok 解不了，直接换成边界消息。
		return grokBoundaryMessage(), true
	}
	if itemType == "custom_tool_call" {
		name := strings.TrimSpace(grokNsStringField(item, "name"))
		namespace := strings.TrimSpace(grokNsStringField(item, "namespace"))
		alias := register(namespace, name, true, false)
		return map[string]any{
			"type":      "function_call",
			"call_id":   grokNsStringField(item, "call_id"),
			"name":      alias,
			"arguments": marshalGrokCustomToolArguments(grokNsStringField(item, "input")),
		}, true
	}
	if itemType == "custom_tool_call_output" {
		output := item["output"]
		if output == nil {
			output = ""
		}
		return map[string]any{
			"type":    "function_call_output",
			"call_id": grokNsStringField(item, "call_id"),
			"output":  output,
		}, true
	}
	if itemType == "tool_search_call" {
		alias := register("", grokToolSearchProxyName, false, true)
		return map[string]any{
			"type": "function_call", "call_id": grokNsStringField(item, "call_id"),
			"name": alias, "arguments": grokJSONString(item["arguments"]),
		}, true
	}
	if itemType == "tool_search_output" || itemType == "tool_search_call_output" {
		output := item["output"]
		if output == nil {
			output = item["tools"]
		}
		return map[string]any{
			"type": "function_call_output", "call_id": grokNsStringField(item, "call_id"),
			"output": grokJSONString(output),
		}, true
	}
	fields, known := grokNativeHistoryFields[itemType]
	if !known {
		return item, false
	}
	// function_call 的 namespace 引用改写成扁平名，匹配已展平的工具声明。
	renamed := false
	if itemType == "function_call" {
		if ns := strings.TrimSpace(grokNsStringField(item, "namespace")); ns != "" {
			item["name"] = register(ns, strings.TrimSpace(grokNsStringField(item, "name")), false, false)
		} else if name := strings.TrimSpace(grokNsStringField(item, "name")); name != "" {
			if _, reserved := grokReservedUpstreamFunctionNames[name]; reserved {
				// 声明侧把保留名挪到 _fn 别名(见 newGrokAliasRegister),历史
				// 调用必须跟着改名:否则声明是 tool_search_fn、历史却引用
				// tool_search,上游按保留名/未声明名拒绝,或与内置 tool_search
				// 混淆。桥接工具的历史走 tool_search_call 分支,不会进到这里。
				if alias := register("", name, false, false); alias != name {
					item["name"] = alias
					renamed = true
				}
			}
		}
	}
	allowed := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		allowed[f] = struct{}{}
	}
	changed := false
	for k, v := range item {
		if _, ok := allowed[k]; !ok || v == nil {
			changed = true
			break
		}
	}
	if !changed {
		return item, renamed
	}
	rebuilt := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := item[f]; ok && v != nil {
			rebuilt[f] = v
		}
	}
	rebuilt["type"] = itemType
	return rebuilt, true
}

// normalizeGrokUpstreamTools 把请求体里 Grok 上游不接受的工具形态归一化：
//   - namespace 工具展平成顶层 function（名字加命名空间前缀），并记录别名供响应侧反解；
//   - web_search 工具剥离 Codex 的控制字段（external_web_access / indexed_web_access /
//     search_content_types / user_location 等，上游会以 "Argument not supported" 400 拒绝），
//     仅保留 {type:"web_search"} 与 filters.allowed_domains；external_web_access:false 时
//     直接移除该工具（上游无法表达"仅索引、禁外网"，保守降级）。
//
// 同步改写 tool_choice 与 input[] 历史引用。无相关内容时原样返回、映射 nil（零开销快速路径）。
func normalizeGrokUpstreamTools(body []byte) ([]byte, map[string]grokNsIdentity) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	register, aliases := newGrokAliasRegister()
	changed := false
	webSearchDropped := false

	// 1) tools[]：展平 namespace，归一 web_search。
	if rawTools, ok := payload["tools"].([]any); ok {
		newTools := make([]any, 0, len(rawTools))
		emittedFunctions := make(map[string]bool)
		appendTool := func(tool any) {
			if object, ok := tool.(map[string]any); ok && grokNsStringField(object, "type") == "function" {
				name := strings.TrimSpace(grokNsStringField(object, "name"))
				if name != "" && emittedFunctions[name] {
					changed = true
					return
				}
				if name != "" {
					emittedFunctions[name] = true
				}
			}
			newTools = append(newTools, tool)
		}
		for _, rt := range rawTools {
			tool, ok := rt.(map[string]any)
			if !ok {
				appendTool(rt)
				continue
			}
			kind := grokNsStringField(tool, "type")
			if kind == "tool_search" {
				alias := register("", grokToolSearchProxyName, false, true)
				appendTool(grokFunctionToolForToolSearch(alias))
				changed = true
				continue
			}
			if kind == "function" {
				name := strings.TrimSpace(grokNsStringField(tool, "name"))
				registered := register("", name, false, false)
				if registered != name {
					tool["name"] = registered
					changed = true
				}
				normalized := normalizeGrokFunctionTool(tool, registered)
				if !reflect.DeepEqual(tool, normalized) {
					changed = true
				}
				appendTool(normalized)
				continue
			}
			if kind == "custom" {
				name := strings.TrimSpace(grokNsStringField(tool, "name"))
				alias := register("", name, true, false)
				appendTool(grokFunctionToolForCustom(tool, alias))
				changed = true
				continue
			}
			if _, isWebSearch := grokWebSearchTypes[kind]; isWebSearch {
				converted, keep, toolChanged := normalizeGrokWebSearchTool(tool)
				if toolChanged {
					changed = true
				}
				if keep {
					appendTool(converted)
				} else {
					webSearchDropped = true
				}
				continue
			}
			if kind != "namespace" {
				appendTool(tool)
				continue
			}
			namespace := strings.TrimSpace(grokNsStringField(tool, "name"))
			children, _ := tool["tools"].([]any)
			for _, rc := range children {
				child, ok := rc.(map[string]any)
				if !ok {
					continue
				}
				childType := grokNsStringField(child, "type")
				if childType != "function" && childType != "custom" {
					continue
				}
				name := strings.TrimSpace(grokNsStringField(child, "name"))
				alias := register(namespace, name, childType == "custom", false)
				if childType == "custom" {
					child = grokFunctionToolForCustom(child, alias)
				} else {
					child = normalizeGrokFunctionTool(child, alias)
				}
				appendTool(child)
			}
			changed = true
		}
		if changed {
			payload["tools"] = newTools
		}
	}

	// web_search 被整体移除时，指向它的 tool_choice 也要撤掉，否则上游校验失败。
	if webSearchDropped {
		if tc, ok := payload["tool_choice"].(map[string]any); ok {
			if _, isWebSearch := grokWebSearchTypes[grokNsStringField(tc, "type")]; isWebSearch {
				delete(payload, "tool_choice")
			}
		}
	}

	// 2) tool_choice：{type:function,name,namespace} → {type:function,name:alias}。
	if tc, ok := payload["tool_choice"].(map[string]any); ok {
		kind := grokNsStringField(tc, "type")
		if kind == "custom" {
			name := strings.TrimSpace(grokNsStringField(tc, "name"))
			tc["type"] = "function"
			tc["name"] = register(strings.TrimSpace(grokNsStringField(tc, "namespace")), name, true, false)
			delete(tc, "namespace")
			changed = true
		} else if kind == "tool_search" {
			tc["type"] = "function"
			tc["name"] = register("", grokToolSearchProxyName, false, true)
			changed = true
		} else if kind == "function" {
			if namespace := strings.TrimSpace(grokNsStringField(tc, "namespace")); namespace != "" {
				name := strings.TrimSpace(grokNsStringField(tc, "name"))
				tc["name"] = register(namespace, name, false, false)
				delete(tc, "namespace")
				changed = true
			}
		}
	}

	// 3) input[] 历史：按 Grok 原生 InputItem 契约逐项重建——只保留每种类型的白名单字段、
	//    丢弃 Codex 扩展字段与 null（否则 Grok 严格 untagged enum 反序列化 422），
	//    function_call 的 namespace 引用改写成扁平名以匹配工具声明，外来 compaction 项换成边界消息。
	if input, ok := payload["input"].([]any); ok {
		for i, ri := range input {
			item, ok := ri.(map[string]any)
			if !ok {
				continue
			}
			rebuilt, itemChanged := rebuildGrokHistoryItem(item, register)
			if itemChanged {
				input[i] = rebuilt
				changed = true
			}
		}
	}

	if !changed {
		return body, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}
	return out, aliases
}

// normalizeGrokWebSearchTool 把 Codex 的 web_search 工具降级为 Grok 上游接受的最小形态：
// {type:"web_search"}（+ filters.allowed_domains）。external_web_access:false 时 keep=false
// （移除工具）。返回 (归一后工具, 是否保留, 是否发生改写)。
func normalizeGrokWebSearchTool(tool map[string]any) (map[string]any, bool, bool) {
	if v, ok := tool["external_web_access"].(bool); ok && !v {
		return nil, false, true
	}
	converted := map[string]any{"type": "web_search"}
	if domains := grokWebSearchAllowedDomains(tool); len(domains) > 0 {
		converted["filters"] = map[string]any{"allowed_domains": domains}
	}
	// 仅当原工具就是裸 {type:"web_search"} 时才是 no-op；带控制字段或 preview 变体都算改写。
	changed := grokNsStringField(tool, "type") != "web_search" || len(tool) > 1
	return converted, true, changed
}

// grokWebSearchAllowedDomains 从 web_search 工具里取 allowed_domains（filters 内优先）。
func grokWebSearchAllowedDomains(tool map[string]any) []any {
	if filters, ok := tool["filters"].(map[string]any); ok {
		if d, ok := filters["allowed_domains"].([]any); ok && len(d) > 0 {
			return d
		}
	}
	if d, ok := tool["allowed_domains"].([]any); ok && len(d) > 0 {
		return d
	}
	return nil
}

// Codex 的历史回放会带上游私钥加密的密文，Grok 无法解码（codex2api 不把 compact 请求
// 路由到 Grok，这些密文对 Grok 恒为"外来"），原样转发会 400
// "Could not decode the compaction blob"。密文有两种载体：
//   - reasoning 项的 encrypted_content（最常见）；
//   - remote_compaction_v2 的 type:"compaction" 压缩项（内含 encrypted_content）。
//
// 处理：删掉 reasoning 等历史项的 encrypted_content（密文非回放前置条件，保留明文
// summary/content 即可续接）；去密文后无可回放内容的 reasoning 项、以及整条 compaction 项，
// 替换成一条纯文本边界消息。优雅降级，对齐参考实现对外来密文的处理。
const grokCompactionBoundaryText = "A previously compacted context could not be carried over to this model. Continue from the retained conversation messages above."

func grokBoundaryMessage() map[string]any {
	return map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{"type": "input_text", "text": grokCompactionBoundaryText},
		},
	}
}

// grokReasoningReplayable 判断去掉密文后的 reasoning 项是否还有可回放的明文内容。
func grokReasoningReplayable(item map[string]any) bool {
	if s, ok := item["summary"].([]any); ok && len(s) > 0 {
		return true
	}
	if c, ok := item["content"].([]any); ok && len(c) > 0 {
		return true
	}
	return false
}

func stripGrokUndecodableBlobs(body []byte) []byte {
	if !bytes.Contains(body, []byte("encrypted_content")) && !bytes.Contains(body, []byte(`"compaction"`)) {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	items, ok := payload["input"].([]any)
	if !ok {
		return body
	}
	changed := false
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if grokNsStringField(item, "type") == "compaction" {
			items[i] = grokBoundaryMessage()
			changed = true
			continue
		}
		if _, has := item["encrypted_content"]; has {
			delete(item, "encrypted_content")
			changed = true
			if grokNsStringField(item, "type") == "reasoning" {
				// 去掉密文后，Grok 的 reasoning 变体不接受 null 字段（如 content:null），
				// 否则 422 "did not match any variant of untagged enum ModelInput"。
				for k, v := range item {
					if v == nil {
						delete(item, k)
					}
				}
				if !grokReasoningReplayable(item) {
					items[i] = grokBoundaryMessage()
				}
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

var grokResponsesToolCallIDPrefixes = []string{"fc_", "ctc_", "tsc_"}

// retypeGrokResponsesToolCallItemID keeps a restored Responses item ID aligned
// with its client-visible type. Grok-compatible relays answer the function form
// with fc_* IDs; replaying that ID on custom_tool_call or tool_search_call is
// rejected by strict Responses validators.
func retypeGrokResponsesToolCallItemID(id, itemType string) string {
	want := ""
	switch itemType {
	case "custom_tool_call":
		want = "ctc_"
	case "tool_search_call":
		want = "tsc_"
	case "function_call":
		want = "fc_"
	}
	trimmed := strings.TrimSpace(id)
	if want == "" || trimmed == "" || strings.HasPrefix(trimmed, want) {
		return id
	}
	for _, known := range grokResponsesToolCallIDPrefixes {
		if known != want && strings.HasPrefix(trimmed, known) {
			return want + strings.TrimPrefix(trimmed, known)
		}
	}
	return id
}

func retypeGrokResponsesToolCallItem(item map[string]any, itemType string) {
	id := grokNsStringField(item, "id")
	if retyped := retypeGrokResponsesToolCallItemID(id, itemType); retyped != id {
		item["id"] = retyped
	}
}

// reverseGrokNamespaceValue 递归把响应里任意 type:"function_call" 对象的扁平名反解回
// 原始 {name, namespace}。返回是否发生改写。
func reverseGrokNamespaceValue(value any, aliases map[string]grokNsIdentity) bool {
	changed := false
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if reverseGrokNamespaceValue(item, aliases) {
				changed = true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if reverseGrokNamespaceValue(item, aliases) {
				changed = true
			}
		}
		if grokNsStringField(typed, "type") == "function_call" {
			if identity, ok := aliases[grokNsStringField(typed, "name")]; ok {
				typed["name"] = identity.Name
				if identity.Namespace != "" {
					typed["namespace"] = identity.Namespace
				} else {
					delete(typed, "namespace")
				}
				if identity.ToolSearch {
					typed["type"] = "tool_search_call"
					retypeGrokResponsesToolCallItem(typed, "tool_search_call")
					typed["execution"] = "client"
					typed["arguments"] = grokToolSearchArguments(typed["arguments"])
					delete(typed, "name")
					delete(typed, "namespace")
				} else if identity.Custom {
					typed["type"] = "custom_tool_call"
					retypeGrokResponsesToolCallItem(typed, "custom_tool_call")
					typed["input"] = unwrapGrokCustomToolArguments(grokNsStringField(typed, "arguments"))
					delete(typed, "arguments")
				}
				changed = true
			}
		}
	}
	return changed
}

// reverseGrokNamespaceJSON 反解一个完整 JSON 文档（非流式响应）。
func reverseGrokNamespaceJSON(data []byte, aliases map[string]grokNsIdentity) []byte {
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return data
	}
	if !reverseGrokNamespaceValue(payload, aliases) {
		return data
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return data
	}
	return out
}

// reverseGrokNamespaceSSELine 反解一行 SSE（仅处理携带 function_call 的 data: 行）。
func reverseGrokNamespaceSSELine(line []byte, aliases map[string]grokNsIdentity) []byte {
	if !bytes.Contains(line, []byte(`"function_call"`)) {
		return line
	}
	// 保留行首 "data:" 前缀与行尾换行，仅重写中间的 JSON。
	trimmed := bytes.TrimRight(line, "\r\n")
	suffix := line[len(trimmed):]
	const prefix = "data:"
	idx := bytes.Index(trimmed, []byte(prefix))
	if idx < 0 {
		return line
	}
	head := trimmed[:idx+len(prefix)]
	payload := bytes.TrimLeft(trimmed[idx+len(prefix):], " ")
	gap := trimmed[idx+len(prefix) : len(trimmed)-len(payload)]
	if len(payload) == 0 || payload[0] != '{' {
		return line
	}
	rewritten := reverseGrokNamespaceJSON(payload, aliases)
	if bytes.Equal(rewritten, payload) {
		return line
	}
	out := make([]byte, 0, len(head)+len(gap)+len(rewritten)+len(suffix))
	out = append(out, head...)
	out = append(out, gap...)
	out = append(out, rewritten...)
	out = append(out, suffix...)
	return out
}

// newGrokNamespaceReverser 包装上游响应体：流式按行反解 function_call 名，非流式整体反解。
// 仅在存在别名映射时调用。
func newGrokNamespaceReverser(body io.ReadCloser, streaming bool, aliases map[string]grokNsIdentity) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer body.Close()
		if !streaming {
			data, err := io.ReadAll(body)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_, _ = pw.Write(reverseGrokNamespaceJSON(data, aliases))
			_ = pw.Close()
			return
		}
		reverser := newGrokStreamReverser(aliases)
		reader := bufio.NewReader(body)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				rewritten := reverser.rewriteLine(line)
				if len(rewritten) > 0 {
					if _, werr := pw.Write(rewritten); werr != nil {
						return
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					_ = pw.CloseWithError(err)
					return
				}
				break
			}
		}
		_ = pw.Close()
	}()
	return pr
}

type grokStreamReverser struct {
	aliases         map[string]grokNsIdentity
	customItems     map[string]bool
	toolSearchItems map[string]bool
	clientItemIDs   map[string]string
	inputBytes      map[string]int
	createdAt       int64
	failed          bool
}

func newGrokStreamReverser(aliases map[string]grokNsIdentity) *grokStreamReverser {
	return &grokStreamReverser{
		aliases:         aliases,
		customItems:     make(map[string]bool),
		toolSearchItems: make(map[string]bool),
		clientItemIDs:   make(map[string]string),
		inputBytes:      make(map[string]int),
		createdAt:       time.Now().Unix(),
	}
}

func (r *grokStreamReverser) rememberClientItemID(upstreamID, callID, itemType string) {
	clientID := retypeGrokResponsesToolCallItemID(upstreamID, itemType)
	if clientID == "" {
		return
	}
	if r.clientItemIDs == nil {
		r.clientItemIDs = make(map[string]string)
	}
	if upstreamID != "" {
		r.clientItemIDs[upstreamID] = clientID
	}
	if callID != "" {
		r.clientItemIDs[callID] = clientID
	}
}

func (r *grokStreamReverser) rewriteLine(line []byte) []byte {
	if !bytes.Contains(line, []byte(`"function_call"`)) &&
		!bytes.Contains(line, []byte(`"response.function_call_arguments.`)) &&
		!bytes.Contains(line, []byte(`"response.created"`)) &&
		!bytes.Contains(line, []byte(`"response.completed"`)) &&
		!bytes.Contains(line, []byte(`"response.incomplete"`)) {
		return line
	}
	trimmed := bytes.TrimRight(line, "\r\n")
	suffix := line[len(trimmed):]
	idx := bytes.Index(trimmed, []byte("data:"))
	if idx < 0 {
		return line
	}
	head := trimmed[:idx+len("data:")]
	payload := bytes.TrimLeft(trimmed[idx+len("data:"):], " ")
	gap := trimmed[idx+len("data:") : len(trimmed)-len(payload)]
	if len(payload) == 0 || payload[0] != '{' {
		return line
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return line
	}
	eventType := grokNsStringField(event, "type")
	if r.failed {
		return nil
	}
	switch eventType {
	case "response.created":
		if response, ok := event["response"].(map[string]any); ok {
			if value, ok := response["created_at"].(float64); ok {
				createdAt := int64(value)
				if createdAt > 0 && float64(createdAt) == value {
					r.createdAt = createdAt
				}
			}
		}
		// Capturing response metadata must not change the provider's original frame.
		return line
	case "response.output_item.added", "response.output_item.done":
		if item, ok := event["item"].(map[string]any); ok {
			name := grokNsStringField(item, "name")
			identity, bridged := r.aliases[name]
			if bridged && identity.Custom {
				id := grokNsStringField(item, "id")
				callID := grokNsStringField(item, "call_id")
				if id != "" {
					r.customItems[id] = true
				}
				if callID != "" {
					r.customItems[callID] = true
				}
				r.rememberClientItemID(id, callID, "custom_tool_call")
			}
			if bridged && identity.ToolSearch {
				id := grokNsStringField(item, "id")
				callID := grokNsStringField(item, "call_id")
				if id != "" {
					r.toolSearchItems[id] = true
				}
				if callID != "" {
					r.toolSearchItems[callID] = true
				}
				r.rememberClientItemID(id, callID, "tool_search_call")
			}
			reverseGrokNamespaceValue(item, r.aliases)
		}
	case "response.function_call_arguments.delta":
		itemID := grokNsStringField(event, "item_id")
		if itemID == "" {
			itemID = grokNsStringField(event, "call_id")
		}
		if r.customItems[itemID] || r.toolSearchItems[itemID] {
			r.inputBytes[itemID] += len(grokNsStringField(event, "delta"))
			if r.inputBytes[itemID] > grokToolCallHardLimitBytes {
				return r.failureLine(head, gap, suffix, event, itemID)
			}
			return nil
		}
	case "response.function_call_arguments.done":
		itemID := grokNsStringField(event, "item_id")
		if itemID == "" {
			itemID = grokNsStringField(event, "call_id")
		}
		if r.customItems[itemID] || r.toolSearchItems[itemID] {
			if n := len(grokNsStringField(event, "arguments")); n > r.inputBytes[itemID] {
				r.inputBytes[itemID] = n
			}
			if r.inputBytes[itemID] > grokToolCallHardLimitBytes {
				return r.failureLine(head, gap, suffix, event, itemID)
			}
		}
		if r.toolSearchItems[itemID] {
			return nil
		}
		if r.customItems[itemID] {
			event["type"] = "response.custom_tool_call_input.done"
			if clientID := r.clientItemIDs[itemID]; clientID != "" {
				event["item_id"] = clientID
			}
			event["input"] = unwrapGrokCustomToolArguments(grokNsStringField(event, "arguments"))
			delete(event, "arguments")
		}
	// 截断终态同样带完整 response.output，别名一样要还原。
	case "response.completed", "response.incomplete":
		if response, ok := event["response"].(map[string]any); ok {
			reverseGrokNamespaceValue(response, r.aliases)
		}
	}
	rewritten, err := json.Marshal(event)
	if err != nil {
		return line
	}
	out := make([]byte, 0, len(head)+len(gap)+len(rewritten)+len(suffix))
	out = append(out, head...)
	out = append(out, gap...)
	out = append(out, rewritten...)
	out = append(out, suffix...)
	return out
}

func (r *grokStreamReverser) failureLine(head, gap, suffix []byte, source map[string]any, itemID string) []byte {
	r.failed = true
	failure := map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"object": "response", "created_at": r.createdAt,
			"status": "failed", "status_code": 400, "output": []any{},
			"error": map[string]any{
				"type": "invalid_request_error", "code": "invalid_prompt", "param": "tools", "status_code": 400,
				"message": "tool_input_too_large: codex2api stopped an oversized tool call after " + strconv.Itoa(r.inputBytes[itemID]) + " bytes (limit " + strconv.Itoa(grokToolCallHardLimitBytes) + "). Split the work into smaller batches.",
			},
		},
	}
	if sequence, ok := source["sequence_number"]; ok {
		failure["sequence_number"] = sequence
	}
	encoded, _ := json.Marshal(failure)
	out := append(append(append(append([]byte{}, head...), gap...), encoded...), suffix...)
	return out
}
