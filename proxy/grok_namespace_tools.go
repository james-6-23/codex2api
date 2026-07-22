package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

// Grok（xAI）上游的 /responses 反序列化器只接受 function/web_search/x_search/… 等工具变体，
// 不认识 Codex CLI 发来的 type:"namespace" 分组工具（会 422 "unknown variant `namespace`"）。
// 这里把 namespace 工具在投递前扁平化成子 function（名字加命名空间前缀），并记录别名映射；
// 上游按扁平名回调工具时，再在响应流里反解回 {name, namespace}，让 Codex 认得。
// 逻辑对齐参考实现：请求侧展平 + 响应侧恢复的完整往返。

type grokNsIdentity struct {
	Namespace string
	Name      string
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
func rebuildGrokHistoryItem(item map[string]any, register func(namespace, name string) string) (map[string]any, bool) {
	itemType := grokNsStringField(item, "type")
	if itemType == "" && grokNsStringField(item, "role") != "" {
		itemType = "message" // Codex 可能省略带 role 消息的 type
	}
	if itemType == "compaction" {
		// 外来 compaction 密文 Grok 解不了，直接换成边界消息。
		return grokBoundaryMessage(), true
	}
	fields, known := grokNativeHistoryFields[itemType]
	if !known {
		return item, false
	}
	// function_call 的 namespace 引用改写成扁平名，匹配已展平的工具声明。
	if itemType == "function_call" {
		if ns := strings.TrimSpace(grokNsStringField(item, "namespace")); ns != "" {
			item["name"] = register(ns, strings.TrimSpace(grokNsStringField(item, "name")))
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
		return item, false
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
	// 触发条件：tools 里有 namespace/web_search，或 input[] 有需要按原生字段重建的历史项
	// （function_call/reasoning 等携带 Codex 扩展字段会让 Grok 严格反序列化 422）。
	needsWork := bytes.Contains(body, []byte(`"namespace"`)) ||
		bytes.Contains(body, []byte(`"web_search`)) ||
		bytes.Contains(body, []byte(`"function_call`)) ||
		bytes.Contains(body, []byte(`"reasoning"`)) ||
		bytes.Contains(body, []byte(`_call"`))
	if !needsWork {
		return body, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	aliases := make(map[string]grokNsIdentity)
	changed := false
	webSearchDropped := false

	register := func(namespace, name string) string {
		alias := grokNamespaceAliasName(namespace, name)
		if existing, ok := aliases[alias]; ok && (existing.Namespace != namespace || existing.Name != name) {
			// 极少见：不同 (ns,name) 撞出同一别名，追加短哈希消歧。
			sum := sha256.Sum256([]byte(namespace + "\x00" + name))
			alias = alias + "__" + hex.EncodeToString(sum[:])[:8]
		}
		aliases[alias] = grokNsIdentity{Namespace: namespace, Name: name}
		return alias
	}

	// 1) tools[]：展平 namespace，归一 web_search。
	if rawTools, ok := payload["tools"].([]any); ok {
		newTools := make([]any, 0, len(rawTools))
		for _, rt := range rawTools {
			tool, ok := rt.(map[string]any)
			if !ok {
				newTools = append(newTools, rt)
				continue
			}
			kind := grokNsStringField(tool, "type")
			if _, isWebSearch := grokWebSearchTypes[kind]; isWebSearch {
				converted, keep, toolChanged := normalizeGrokWebSearchTool(tool)
				if toolChanged {
					changed = true
				}
				if keep {
					newTools = append(newTools, converted)
				} else {
					webSearchDropped = true
				}
				continue
			}
			if kind != "namespace" {
				newTools = append(newTools, tool)
				continue
			}
			namespace := strings.TrimSpace(grokNsStringField(tool, "name"))
			children, _ := tool["tools"].([]any)
			for _, rc := range children {
				child, ok := rc.(map[string]any)
				if !ok || grokNsStringField(child, "type") != "function" {
					continue
				}
				name := strings.TrimSpace(grokNsStringField(child, "name"))
				child["name"] = register(namespace, name)
				delete(child, "defer_loading")
				newTools = append(newTools, child)
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
		if grokNsStringField(tc, "type") == "function" {
			if namespace := strings.TrimSpace(grokNsStringField(tc, "namespace")); namespace != "" {
				name := strings.TrimSpace(grokNsStringField(tc, "name"))
				tc["name"] = register(namespace, name)
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
		reader := bufio.NewReader(body)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				if _, werr := pw.Write(reverseGrokNamespaceSSELine(line, aliases)); werr != nil {
					return
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
