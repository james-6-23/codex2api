package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Grok 投递前置处理的单次遍历实现。
//
// 逐步改写（工具归一化 → 字段剥离 → 轮次统计）语义上等价，但会对整个请求体做
// map[string]any 往返，再叠加数次 sjson 全量重分配；实抓的官方 CLI 会话第 5 轮
// 请求体已达 ~800KB，那条路径单请求要分配近 8MB、两万余次。
//
// 这里改为只扫一遍：顶层按键重写，干净的历史项原样拷贝 raw 字节，只有确实需要
// 按原生契约重建的项才解码成小对象、复用 rebuildGrokHistoryItem 处理。
// 等价性由 grok_preflight_diff_test.go 对着逐步改写实现做差分断言守护。

// grokPreflightResult 是一次前置处理的全部产物：归一化后的请求体、namespace
// 别名映射（响应侧反解用）、会话轮次序号与模型名。
type grokPreflightResult struct {
	Body      []byte
	Aliases   map[string]grokNsIdentity
	TurnIndex int
	Model     string
}

// grokDroppedTopLevelFields 是 Codex 管道注入、Grok 上游不接受的顶层字段。
var grokDroppedTopLevelFields = map[string]struct{}{
	"client_metadata":   {},
	"prompt_cache_key":  {},
	"service_tier":      {},
	"safety_identifier": {},
}

// prepareGrokUpstreamBody 在一次遍历里完成 Grok 投递前的全部改写：namespace 工具
// 展平、web_search 降级、历史项按原生契约重建、Codex 专属顶层字段剥离、思考强度
// 钳制、无工具时撤掉 tool_choice，并顺带算出轮次序号与模型名。
// 请求体非法 JSON 或顶层不是对象时原样返回，交由上游报错。
func prepareGrokUpstreamBody(body []byte) grokPreflightResult {
	if lifted, changed := liftGrokAdditionalTools(body); changed {
		return prepareGrokUpstreamBody(lifted)
	}
	if guarded, changed := addGrokGiantToolInstructions(body); changed {
		return prepareGrokUpstreamBody(guarded)
	}
	result := grokPreflightResult{Body: body, TurnIndex: 1}
	if !gjson.ValidBytes(body) {
		return result
	}
	root := gjson.GetBytes(body, "@this")
	if !root.IsObject() {
		return result
	}

	if model := root.Get("model"); model.Exists() {
		result.Model = model.String()
	}

	register, aliases := newGrokAliasRegister()

	// tools 先处理：input 与 tool_choice 的 namespace 引用要改写成同一份别名，
	// 处理顺序必须与逐步改写实现一致，否则别名撞名时的消歧结果会不同。
	toolsRaw, toolsChanged, toolsEmpty, webSearchDropped := grokNormalizeToolsRaw(root.Get("tools"), register)
	choiceRaw, choiceChanged, choiceDropped := grokNormalizeToolChoiceRaw(root.Get("tool_choice"), toolsEmpty, webSearchDropped, register)

	var out bytes.Buffer
	out.Grow(len(body) + 1024)
	out.WriteByte('{')
	first := true
	changed := false
	turnIndex := 0

	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if _, dropped := grokDroppedTopLevelFields[name]; dropped {
			changed = true
			return true
		}
		switch name {
		case "model":
			// 与 gjson 取值语义一致：重复键以首次出现为准。
			if result.Model == "" {
				result.Model = value.String()
			}
		case "tools":
			if toolsChanged {
				grokWriteObjectKey(&out, &first, key)
				out.Write(toolsRaw)
				changed = true
				return true
			}
		case "tool_choice":
			if choiceDropped {
				changed = true
				return true
			}
			if choiceChanged {
				grokWriteObjectKey(&out, &first, key)
				out.Write(choiceRaw)
				changed = true
				return true
			}
		case "input":
			if value.IsArray() {
				// 先乐观地把重建结果直接写进同一个缓冲区，整段没改动再回退成原样拷贝，
				// 省掉"先建数组缓冲、再整体搬进外层"的一次全量复制。
				grokWriteObjectKey(&out, &first, key)
				mark := out.Len()
				inputChanged, turns := grokWriteRebuiltInput(&out, value, register)
				turnIndex = turns
				if inputChanged {
					changed = true
				} else {
					out.Truncate(mark)
					out.WriteString(value.Raw)
				}
				return true
			}
		case "reasoning":
			if patched, ok := grokClampReasoningObjectRaw(value, result.Model); ok {
				grokWriteObjectKey(&out, &first, key)
				out.Write(patched)
				changed = true
				return true
			}
		case "reasoning_effort":
			if mapped, ok := mapGrokReasoningEffort(value.String(), result.Model); ok {
				if encoded, err := json.Marshal(mapped); err == nil {
					grokWriteObjectKey(&out, &first, key)
					out.Write(encoded)
					changed = true
					return true
				}
			}
		}
		grokWriteObjectKey(&out, &first, key)
		out.WriteString(value.Raw)
		return true
	})
	out.WriteByte('}')

	if turnIndex < 1 {
		turnIndex = 1
	}
	result.TurnIndex = turnIndex
	if len(aliases) > 0 {
		result.Aliases = aliases
	}
	// 无改写时返回原始字节，避免为等价内容白付一次重建。
	if changed {
		result.Body = out.Bytes()
	}
	return result
}

const grokGiantToolInstructionMarker = "[codex2api giant-tool-call guard]"

func addGrokGiantToolInstructions(body []byte) ([]byte, bool) {
	if !bytes.Contains(bytes.ToLower(body), []byte(`"apply_patch"`)) || bytes.Contains(body, []byte(grokGiantToolInstructionMarker)) {
		return body, false
	}
	var request map[string]any
	if decodeGrokJSONPreservingNumbers(body, &request) != nil || !grokBodyHasNamedTool(request, "apply_patch") {
		return body, false
	}
	// The 96 KiB guidance leaves deliberate headroom below the 128 KiB hard stop
	// for wrapper JSON and escaping overhead.
	guidance := grokGiantToolInstructionMarker + "\nWhen using apply_patch or another code-writing tool, modify at most 2 files per call and keep each tool input below 96 KiB. Split larger work into verified batches."
	existing := strings.TrimSpace(grokNsStringField(request, "instructions"))
	if existing != "" {
		guidance = existing + "\n\n" + guidance
	}
	request["instructions"] = guidance
	out, err := json.Marshal(request)
	return out, err == nil
}

func grokBodyHasNamedTool(request map[string]any, want string) bool {
	var scan func([]any) bool
	scan = func(tools []any) bool {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(grokNsStringField(tool, "name")), want) {
				return true
			}
			if nested, ok := tool["tools"].([]any); ok && scan(nested) {
				return true
			}
		}
		return false
	}
	tools, _ := request["tools"].([]any)
	return scan(tools)
}

func liftGrokAdditionalTools(body []byte) ([]byte, bool) {
	if !bytes.Contains(body, []byte(`"additional_tools"`)) && !bytes.Contains(body, []byte(`"tool_search_output"`)) && !bytes.Contains(body, []byte(`"tool_search_call_output"`)) {
		return body, false
	}
	var request map[string]any
	if decodeGrokJSONPreservingNumbers(body, &request) != nil {
		return body, false
	}
	input, ok := request["input"].([]any)
	if !ok {
		return body, false
	}
	tools, _ := request["tools"].([]any)
	filtered := make([]any, 0, len(input))
	changed := false
	for _, raw := range input {
		item, itemOK := raw.(map[string]any)
		if !itemOK {
			filtered = append(filtered, raw)
			continue
		}
		itemType := strings.TrimSpace(grokNsStringField(item, "type"))
		additional, toolsOK := item["tools"].([]any)
		if !toolsOK || (itemType != "additional_tools" && itemType != "tool_search_output" && itemType != "tool_search_call_output") {
			filtered = append(filtered, raw)
			continue
		}
		tools = append(tools, additional...)
		changed = true
		if itemType != "additional_tools" {
			output := item["output"]
			if output == nil {
				output = additional
			}
			item["output"] = output
			delete(item, "tools")
			filtered = append(filtered, raw)
		}
	}
	if !changed {
		return body, false
	}
	request["input"] = filtered
	request["tools"] = tools
	out, err := json.Marshal(request)
	if err != nil {
		return body, false
	}
	return out, true
}

func decodeGrokJSONPreservingNumbers(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	return decoder.Decode(target)
}

// grokWriteObjectKey 写出 JSON 对象里的 `"key":`，必要时补上分隔逗号。
func grokWriteObjectKey(out *bytes.Buffer, first *bool, key gjson.Result) {
	if !*first {
		out.WriteByte(',')
	}
	*first = false
	if raw := key.Raw; len(raw) > 0 && raw[0] == '"' {
		out.WriteString(raw)
	} else if encoded, err := json.Marshal(key.String()); err == nil {
		out.Write(encoded)
	} else {
		out.WriteString(`""`)
	}
	out.WriteByte(':')
}

// grokNormalizeToolsRaw 归一化 tools[]：namespace 工具展平成子 function、web_search
// 降级为上游接受的最小形态，其余工具原样拷贝。
// 返回 (新数组 JSON, 是否改写, 归一后是否等同于"没有工具", 是否移除过 web_search)。
// "没有工具"含 tools 缺失与归一后空数组两种——两者都让 tool_choice 失去意义。
func grokNormalizeToolsRaw(tools gjson.Result, register grokAliasRegister) (raw []byte, changed, empty, webSearchDropped bool) {
	if !tools.Exists() {
		return nil, false, true, false
	}
	// 非数组属于畸形请求，保持原样交给上游报错（也不据此撤 tool_choice）。
	if !tools.IsArray() {
		return nil, false, false, false
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	kept := 0
	emittedFunctions := make(map[string]bool)

	writeRaw := func(value string) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.WriteString(value)
		kept++
	}
	writeObject := func(value map[string]any) bool {
		if grokNsStringField(value, "type") == "function" {
			name := strings.TrimSpace(grokNsStringField(value, "name"))
			if name != "" && emittedFunctions[name] {
				changed = true
				return true
			}
			if name != "" {
				emittedFunctions[name] = true
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return false
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(encoded)
		kept++
		return true
	}

	tools.ForEach(func(_, tool gjson.Result) bool {
		if !tool.IsObject() {
			writeRaw(tool.Raw)
			return true
		}
		kind := tool.Get("type").String()
		if kind == "tool_search" {
			writeObject(grokFunctionToolForToolSearch(register("", grokToolSearchProxyName, false, true)))
			changed = true
			return true
		}
		if kind == "function" {
			name := strings.TrimSpace(tool.Get("name").String())
			registered := register("", name, false, false)
			decoded, ok := grokDecodeObject(tool.Raw)
			if !ok {
				writeRaw(tool.Raw)
				return true
			}
			normalized := normalizeGrokFunctionTool(decoded, registered)
			writeObject(normalized)
			if registered != name || !reflect.DeepEqual(decoded, normalized) {
				changed = true
			}
			return true
		}
		if kind == "custom" {
			decoded, ok := grokDecodeObject(tool.Raw)
			if !ok {
				writeRaw(tool.Raw)
				return true
			}
			name := strings.TrimSpace(grokNsStringField(decoded, "name"))
			writeObject(grokFunctionToolForCustom(decoded, register("", name, true, false)))
			changed = true
			return true
		}
		if _, isWebSearch := grokWebSearchTypes[kind]; isWebSearch {
			decoded, ok := grokDecodeObject(tool.Raw)
			if !ok {
				writeRaw(tool.Raw)
				return true
			}
			converted, keep, toolChanged := normalizeGrokWebSearchTool(decoded)
			if toolChanged {
				changed = true
			}
			if !keep {
				webSearchDropped = true
				return true
			}
			if !writeObject(converted) {
				writeRaw(tool.Raw)
			}
			return true
		}
		if kind != "namespace" {
			writeRaw(tool.Raw)
			return true
		}
		namespace := strings.TrimSpace(tool.Get("name").String())
		tool.Get("tools").ForEach(func(_, child gjson.Result) bool {
			childType := child.Get("type").String()
			if !child.IsObject() || (childType != "function" && childType != "custom") {
				return true
			}
			decoded, ok := grokDecodeObject(child.Raw)
			if !ok {
				return true
			}
			alias := register(namespace, strings.TrimSpace(grokNsStringField(decoded, "name")), childType == "custom", false)
			if childType == "custom" {
				writeObject(grokFunctionToolForCustom(decoded, alias))
			} else {
				writeObject(normalizeGrokFunctionTool(decoded, alias))
			}
			return true
		})
		changed = true
		return true
	})
	buf.WriteByte(']')
	return buf.Bytes(), changed, kept == 0, webSearchDropped
}

// grokNormalizeToolChoiceRaw 处理 tool_choice：web_search 被整体移除时撤掉指向它的
// 选择，namespace 引用改写成扁平名，归一后没有工具声明时整个撤掉（上游对"设了
// tool_choice 但 tools 为空"硬校验 400）。
// 判定顺序与逐步改写实现一致：先 web_search 撤销、再 namespace 改写、最后空工具撤销，
// 使 namespace 别名的注册时机不受影响。
func grokNormalizeToolChoiceRaw(choice gjson.Result, toolsEmpty, webSearchDropped bool, register grokAliasRegister) (raw []byte, changed, dropped bool) {
	if !choice.Exists() {
		return nil, false, false
	}
	if choice.IsObject() {
		kind := choice.Get("type").String()
		if webSearchDropped {
			if _, isWebSearch := grokWebSearchTypes[kind]; isWebSearch {
				return nil, false, true
			}
		}
		if kind == "custom" {
			if decoded, ok := grokDecodeObject(choice.Raw); ok {
				decoded["type"] = "function"
				decoded["name"] = register(strings.TrimSpace(choice.Get("namespace").String()), strings.TrimSpace(choice.Get("name").String()), true, false)
				delete(decoded, "namespace")
				if encoded, err := json.Marshal(decoded); err == nil {
					raw, changed = encoded, true
				}
			}
		} else if kind == "tool_search" {
			if encoded, err := json.Marshal(map[string]any{"type": "function", "name": register("", grokToolSearchProxyName, false, true)}); err == nil {
				raw, changed = encoded, true
			}
		} else if kind == "function" {
			if namespace := strings.TrimSpace(choice.Get("namespace").String()); namespace != "" {
				if decoded, ok := grokDecodeObject(choice.Raw); ok {
					decoded["name"] = register(namespace, strings.TrimSpace(grokNsStringField(decoded, "name")), false, false)
					delete(decoded, "namespace")
					if encoded, err := json.Marshal(decoded); err == nil {
						raw, changed = encoded, true
					}
				}
			}
		}
	}
	if toolsEmpty {
		return nil, false, true
	}
	return raw, changed, false
}

// grokWriteRebuiltInput 逐项处理 input[] 并直接写入 out：干净项原样拷贝 raw 字节，
// 只有需要重建的项才走重写。顺带数出 user 消息数作为轮次序号，
// 省掉一趟独立的全量遍历。
func grokWriteRebuiltInput(out *bytes.Buffer, input gjson.Result, register grokAliasRegister) (changed bool, turns int) {
	out.WriteByte('[')
	first := true
	input.ForEach(func(_, item gjson.Result) bool {
		if !first {
			out.WriteByte(',')
		}
		first = false
		itemChanged, isUserMessage := grokWriteHistoryItem(out, item, register)
		if itemChanged {
			changed = true
		}
		if isUserMessage {
			turns++
		}
		return true
	})
	out.WriteByte(']')
	return changed, turns
}

// grokWriteHistoryItem 把单个历史项写入 out，返回 (是否改写, 是否计入轮次)。
// 无需改写时原样拷贝 raw 字节。
func grokWriteHistoryItem(out *bytes.Buffer, item gjson.Result, register grokAliasRegister) (bool, bool) {
	verbatim := func() (bool, bool) {
		out.WriteString(item.Raw)
		return false, grokRawItemIsUserMessage(item)
	}
	if !item.IsObject() {
		return verbatim()
	}
	itemType := grokResolveHistoryItemType(item)
	// 外来压缩密文 Grok 解不了，整项换成纯文本边界消息（developer 角色，不计轮次）。
	if itemType == "compaction" {
		encoded, err := json.Marshal(grokBoundaryMessage())
		if err != nil {
			return verbatim()
		}
		out.Write(encoded)
		return true, false
	}
	if itemType == "custom_tool_call" || itemType == "custom_tool_call_output" || itemType == "tool_search_call" || itemType == "tool_search_output" || itemType == "tool_search_call_output" {
		decoded, ok := grokDecodeObject(item.Raw)
		if !ok {
			return verbatim()
		}
		rebuilt, changed := rebuildGrokHistoryItem(decoded, register)
		if !changed {
			return verbatim()
		}
		encoded, err := json.Marshal(rebuilt)
		if err != nil {
			return verbatim()
		}
		out.Write(encoded)
		return true, false
	}
	fields, known := grokNativeHistoryFields[itemType]
	if !known {
		return verbatim() // 未知类型原样透传
	}

	// function_call 的 namespace 引用要改写成扁平名以匹配已展平的工具声明。
	// namespace 本身不在原生字段白名单里，出现即意味着必须重建。
	aliasName := ""
	if itemType == "function_call" {
		if namespace := strings.TrimSpace(grokRawStringField(item, "namespace")); namespace != "" {
			aliasName = register(namespace, strings.TrimSpace(grokRawStringField(item, "name")), false, false)
		} else if name := strings.TrimSpace(grokRawStringField(item, "name")); name != "" {
			if _, reserved := grokReservedUpstreamFunctionNames[name]; reserved {
				// 声明侧把保留名挪到 _fn 别名,历史调用必须同步改名,否则声明
				// 与历史引用分叉被上游拒绝。桥接工具的历史是 tool_search_call
				// 类型,走不到这个分支。
				if alias := register("", name, false, false); alias != name {
					aliasName = alias
				}
			}
		}
	}
	if aliasName == "" && grokHistoryItemFieldsAreNative(item, fields) {
		return verbatim()
	}

	if grokWriteRewrittenHistoryItem(out, item, itemType, fields, aliasName) {
		// 重建后的 type 恒为归一后的 itemType；role 只有原值存在且非 null 才会被保留。
		return true, itemType == "message" && grokRawRoleIsUser(item)
	}
	// raw 快路覆盖不到（字段白名单超出内联容量）时回退到 map 版重建。
	// register 幂等，前面已注册过别名不受影响。
	decoded, ok := grokDecodeObject(item.Raw)
	if !ok {
		return verbatim()
	}
	rebuilt, changed := rebuildGrokHistoryItem(decoded, register)
	if !changed {
		return verbatim()
	}
	encoded, err := json.Marshal(rebuilt)
	if err != nil {
		return verbatim()
	}
	out.Write(encoded)
	return true, grokDecodedItemIsUserMessage(rebuilt)
}

// grokResolveHistoryItemType 解析历史项类型。与 map 版一致：只认字符串形态的 type，
// 缺失时按带字符串 role 的项推断为 message（Codex 可能省略 type）。
func grokResolveHistoryItemType(item gjson.Result) string {
	itemType := grokRawStringField(item, "type")
	if itemType == "" && grokRawStringField(item, "role") != "" {
		itemType = "message"
	}
	return itemType
}

// grokHistoryItemFieldsAreNative 判断历史项的字段集是否已符合原生契约：
// 全部键都在白名单内且没有 null 值，此时无需重建、可原样透传。
func grokHistoryItemFieldsAreNative(item gjson.Result, fields []string) bool {
	native := true
	item.ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.Null || !slices.Contains(fields, key.String()) {
			native = false
			return false
		}
		return true
	})
	return native
}

// grokMaxNativeFields 是原生字段白名单的内联容量（当前最长的 mcp_call 为 9 个字段）。
// 超出时 raw 快路让位给 map 版重建，保证白名单扩充后仍然正确。
const grokMaxNativeFields = 16

// grokWriteRewrittenHistoryItem 按原生字段白名单重建历史项并写入 out：只拷贝白名单内的
// 原始键值对，不解码嵌套内容——大段 output 文本与 encrypted_content 密文直接原样搬运，
// 省掉"解码成 Go 值再重新转义"的往返。
// type 恒写成归一后的类型；function_call 的 name 在有别名时替换为扁平名。
// 返回 false 表示这条项走不了 raw 快路，此时未向 out 写入任何字节，由调用方回退。
func grokWriteRewrittenHistoryItem(out *bytes.Buffer, item gjson.Result, itemType string, fields []string, aliasName string) bool {
	if len(fields) > grokMaxNativeFields {
		return false
	}
	encodedAlias := ""
	if aliasName != "" {
		encoded, err := json.Marshal(aliasName)
		if err != nil {
			return false
		}
		encodedAlias = string(encoded)
	}

	// 一趟扫完整项，按白名单位置收集原始值。重复键取最后一次出现、null 视作缺失，
	// 与 map 解码的语义一致。
	var raws [grokMaxNativeFields]string
	var present [grokMaxNativeFields]bool
	item.ForEach(func(key, value gjson.Result) bool {
		index := slices.Index(fields, key.String())
		if index < 0 {
			return true
		}
		if value.Type == gjson.Null {
			present[index] = false
			return true
		}
		raws[index] = value.Raw
		present[index] = true
		return true
	})

	out.WriteByte('{')
	first := true
	typeWritten := false
	for index, field := range fields {
		switch {
		case field == "type":
			// 原生契约要求 type 就是归一后的类型，原值（可能缺失或非字符串）一律覆盖。
			grokWriteQuotedField(out, &first, "type", itemType)
			typeWritten = true
		case !present[index]:
			continue
		case field == "name" && encodedAlias != "":
			grokWriteRawField(out, &first, field, encodedAlias)
		default:
			grokWriteRawField(out, &first, field, raws[index])
		}
	}
	if !typeWritten {
		grokWriteQuotedField(out, &first, "type", itemType)
	}
	out.WriteByte('}')
	return true
}

// grokWriteRawField 写出 `"name":raw`，必要时补分隔逗号。
// name 取自原生字段白名单，均为纯 ASCII 标识符，无需转义。
func grokWriteRawField(out *bytes.Buffer, first *bool, name, raw string) {
	if !*first {
		out.WriteByte(',')
	}
	*first = false
	out.WriteByte('"')
	out.WriteString(name)
	out.WriteString(`":`)
	out.WriteString(raw)
}

// grokWriteQuotedField 写出 `"name":"value"`。value 取自原生字段白名单的键名，
// 同样是纯 ASCII 标识符，无需转义。
func grokWriteQuotedField(out *bytes.Buffer, first *bool, name, value string) {
	grokWriteRawField(out, first, name, "")
	out.WriteByte('"')
	out.WriteString(value)
	out.WriteByte('"')
}

// grokRawStringField 取字符串形态的字段值，非字符串一律视作缺失
// （与 map 版的类型断言语义一致）。
func grokRawStringField(item gjson.Result, key string) string {
	if value := item.Get(key); value.Type == gjson.String {
		return value.String()
	}
	return ""
}

// grokRawItemIsUserMessage 判定原样透传的历史项是否计入轮次，
// 与 grokTurnIndex 对同一项的判定一致。
func grokRawItemIsUserMessage(item gjson.Result) bool {
	return item.Get("type").String() == "message" && grokRawRoleIsUser(item)
}

func grokRawRoleIsUser(item gjson.Result) bool {
	role := item.Get("role")
	return role.Exists() && role.Type != gjson.Null && strings.EqualFold(role.String(), "user")
}

// grokDecodedItemIsUserMessage 是 map 回退路径上的同一判定。
func grokDecodedItemIsUserMessage(item map[string]any) bool {
	return grokNsStringField(item, "type") == "message" &&
		strings.EqualFold(grokNsStringField(item, "role"), "user")
}

// grokClampReasoningObjectRaw 钳制 reasoning.effort，返回 (新对象 JSON, 是否改写)。
func grokClampReasoningObjectRaw(reasoning gjson.Result, model string) ([]byte, bool) {
	if !reasoning.IsObject() {
		return nil, false
	}
	effort := reasoning.Get("effort")
	if !effort.Exists() {
		return nil, false
	}
	mapped, ok := mapGrokReasoningEffort(effort.String(), model)
	if !ok {
		return nil, false
	}
	patched, err := sjson.SetBytes([]byte(reasoning.Raw), "effort", mapped)
	if err != nil {
		return nil, false
	}
	return patched, true
}

// grokDecodeObject 把单个 JSON 对象解码成 map。历史项与工具都是小对象，
// 这里的解码开销与整体请求体无关。
func grokDecodeObject(raw string) (map[string]any, bool) {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}
