package proxy

import (
	"encoding/json"
	"strconv"
	"strings"
)

// 上游偶尔把工具调用泄漏成正文：<tool_use …>…</tool_use> XML 直接剥离；行首的
// <invoke name="…"><parameter …>…</parameter></invoke> 块按声明的 schema 严格校验后
// 还原成真正的工具调用，校验不过的原样当文本输出。移植自 kiro.rs
// src/kiro/model/events/assistant.rs 与 src/anthropic/stream.rs。

const (
	kiroToolUseXMLPrefix   = "<tool_use"
	kiroToolUseXMLClose    = "</tool_use>"
	kiroMaxToolUseHold     = 65_536
	kiroMaxInvokeHold      = 262_144
	kiroInvokeKeyword      = "invoke"
	kiroInvokeCloseKeyword = "invoke>"
)

// kiroMarkdownCode 跟踪当前位置是否处于代码块 / 行内代码 / 缩进代码中。
type kiroMarkdownCode struct {
	fenceChar   byte
	fenceLen    int
	line        strings.Builder
	inlineTicks int
}

func (m *kiroMarkdownCode) advance(text string) {
	for _, r := range text {
		if r == '\n' {
			line := m.line.String()
			m.line.Reset()
			m.finishLine(line)
		} else {
			m.line.WriteRune(r)
		}
	}
}

func kiroFenceMarker(line string) (byte, int, string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || trimmed[0] != '`' && trimmed[0] != '~' {
		return 0, 0, "", false
	}
	c := trimmed[0]
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, "", false
	}
	rest := trimmed[n:]
	if c == '`' && strings.Contains(rest, "`") {
		return 0, 0, "", false
	}
	return c, n, rest, true
}

func kiroScanInlineTicks(open int, line string) int {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && open == 0 {
			i++
			continue
		}
		if c != '`' {
			continue
		}
		n := 1
		for i+1 < len(line) && line[i+1] == '`' {
			i++
			n++
		}
		if open == 0 {
			open = n
		} else if n == open {
			open = 0
		}
	}
	return open
}

func (m *kiroMarkdownCode) finishLine(line string) {
	line = strings.TrimSuffix(line, "\r")
	if m.fenceChar != 0 {
		if c, n, rest, ok := kiroFenceMarker(line); ok && c == m.fenceChar && n >= m.fenceLen && strings.TrimSpace(rest) == "" {
			m.fenceChar, m.fenceLen = 0, 0
		}
		return
	}
	if c, n, _, ok := kiroFenceMarker(line); ok {
		m.fenceChar, m.fenceLen, m.inlineTicks = c, n, 0
		return
	}
	if strings.TrimSpace(line) == "" {
		m.inlineTicks = 0
		return
	}
	m.inlineTicks = kiroScanInlineTicks(m.inlineTicks, line)
}

func (m *kiroMarkdownCode) inCode() bool {
	line := m.line.String()
	return m.fenceChar != 0 || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") || kiroScanInlineTicks(m.inlineTicks, line) > 0
}

func (m *kiroMarkdownCode) atLineStart() bool { return m.line.Len() == 0 }

func (m *kiroMarkdownCode) clone() *kiroMarkdownCode {
	copied := &kiroMarkdownCode{fenceChar: m.fenceChar, fenceLen: m.fenceLen, inlineTicks: m.inlineTicks}
	copied.line.WriteString(m.line.String())
	return copied
}

func kiroLongestPrefixSuffix(s, needle string) int {
	for length := min(len(s), len(needle)-1); length >= 1; length-- {
		if isUTF8Boundary(s, len(s)-length) && strings.HasPrefix(needle, s[len(s)-length:]) {
			return length
		}
	}
	return 0
}

// ---- <tool_use> XML ----

type kiroXMLFilter struct {
	buffer string
	code   kiroMarkdownCode
}

func (f *kiroXMLFilter) drain(atEnd bool) string {
	rest := f.buffer
	f.buffer = ""
	var out strings.Builder
	for {
		start := strings.Index(rest, kiroToolUseXMLPrefix)
		if start < 0 {
			keep := 0
			if !atEnd {
				keep = kiroLongestPrefixSuffix(rest, kiroToolUseXMLPrefix)
			}
			emit := rest[:len(rest)-keep]
			f.code.advance(emit)
			out.WriteString(emit)
			f.buffer = rest[len(rest)-keep:]
			return out.String()
		}
		before := rest[:start]
		f.code.advance(before)
		out.WriteString(before)
		tail := rest[start:]
		action, end := f.classify(tail, atEnd)
		switch action {
		case "strip":
			rest = tail[end:]
		case "keep":
			f.code.advance(kiroToolUseXMLPrefix)
			out.WriteString(kiroToolUseXMLPrefix)
			rest = tail[len(kiroToolUseXMLPrefix):]
		default:
			f.buffer = tail
			return out.String()
		}
	}
}

func (f *kiroXMLFilter) classify(tail string, atEnd bool) (string, int) {
	if f.code.inCode() {
		return "keep", 0
	}
	after := tail[len(kiroToolUseXMLPrefix):]
	if after == "" {
		if atEnd {
			return "keep", 0
		}
		return "wait", 0
	}
	if !strings.ContainsRune(" \t\r\n", rune(after[0])) {
		return "keep", 0
	}
	gt := strings.IndexByte(after, '>')
	if gt < 0 {
		if atEnd || len(tail) > kiroMaxToolUseHold {
			return "keep", 0
		}
		return "wait", 0
	}
	if strings.TrimSpace(after[:gt]) == "" {
		return "keep", 0
	}
	bodyStart := len(kiroToolUseXMLPrefix) + gt + 1
	if closeAt := strings.Index(tail[bodyStart:], kiroToolUseXMLClose); closeAt >= 0 {
		return "strip", bodyStart + closeAt + len(kiroToolUseXMLClose)
	}
	if atEnd || len(tail) > kiroMaxToolUseHold {
		return "keep", 0
	}
	return "wait", 0
}

// ---- <invoke> 嗅探 ----

// kiroOpenTagLT 返回标签名前的 '<' 位置，兼容 <ns:invoke> 这类带前缀的写法。
func kiroOpenTagLT(buffer string, namePos int) int {
	if namePos == 0 {
		return -1
	}
	switch buffer[namePos-1] {
	case '<':
		return namePos - 1
	case ':':
		j := namePos - 1
		for j > 0 && kiroIdentByte(buffer[j-1]) {
			j--
		}
		if j < namePos-1 && j > 0 && buffer[j-1] == '<' {
			return j - 1
		}
	}
	return -1
}

func kiroIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func kiroInvokeNameEnds(buffer string, after int) bool {
	return after >= len(buffer) || strings.IndexByte(" \t\r\n>/", buffer[after]) >= 0
}

func kiroFindInvokeStart(buffer string) int {
	for search := 0; ; {
		rel := strings.Index(buffer[search:], kiroInvokeKeyword)
		if rel < 0 {
			return -1
		}
		namePos := search + rel
		if lt := kiroOpenTagLT(buffer, namePos); lt >= 0 && kiroInvokeNameEnds(buffer, namePos+len(kiroInvokeKeyword)) && !(lt > 0 && kiroIsQuoteChar(buffer, lt-1)) {
			return lt
		}
		search = namePos + len(kiroInvokeKeyword)
	}
}

func kiroFindNextInvokeOpen(buffer string, start int) int {
	gt := strings.IndexByte(buffer[start:], '>')
	if gt < 0 {
		return -1
	}
	afterOpen := start + gt + 1
	region := buffer[afterOpen:]
	for search := 0; ; {
		rel := strings.Index(region[search:], kiroInvokeKeyword)
		if rel < 0 {
			return -1
		}
		namePos := search + rel
		if lt := kiroOpenTagLT(region, namePos); lt >= 0 && kiroInvokeNameEnds(region, namePos+len(kiroInvokeKeyword)) {
			return afterOpen + lt
		}
		search = namePos + len(kiroInvokeKeyword)
	}
}

// kiroFindLastClose 在 [from, boundary) 里找最后一个 </keyword>（含 </ns:keyword>），
// 返回闭合标签的起止位置。
func kiroFindLastClose(buffer string, from, boundary int, keyword string) (int, int) {
	if boundary > len(buffer) {
		boundary = len(buffer)
	}
	if from >= boundary {
		return -1, -1
	}
	region := buffer[from:boundary]
	closeStart, closeEnd := -1, -1
	for search := 0; ; {
		rel := strings.Index(region[search:], keyword)
		if rel < 0 {
			return closeStart, closeEnd
		}
		namePos := search + rel
		if namePos >= 2 && region[namePos-2:namePos] == "</" {
			closeStart, closeEnd = from+namePos-2, from+namePos+len(keyword)
		} else if namePos >= 1 && region[namePos-1] == ':' {
			j := namePos - 1
			for j > 0 && kiroIdentByte(region[j-1]) {
				j--
			}
			if j >= 2 && region[j-2:j] == "</" {
				closeStart, closeEnd = from+j-2, from+namePos+len(keyword)
			}
		}
		search = namePos + len(keyword)
	}
}

func kiroFindInvokeBlockEnd(buffer string, start int) int {
	boundary := kiroFindNextInvokeOpen(buffer, start)
	if boundary < 0 {
		boundary = len(buffer)
	}
	_, end := kiroFindLastClose(buffer, start, boundary, kiroInvokeCloseKeyword)
	return end
}

func kiroExtractNameAttr(tag string) (string, bool) {
	rel := strings.Index(tag, `name="`)
	if rel < 0 {
		return "", false
	}
	start := rel + len(`name="`)
	end := strings.IndexByte(tag[start:], '"')
	if end < 0 {
		return "", false
	}
	return tag[start : start+end], true
}

type kiroInvokeParam struct{ key, value string }

func kiroFindNextParamOpen(body string, from int) int {
	for search := from; ; {
		rel := strings.Index(body[search:], `parameter name="`)
		if rel < 0 {
			return -1
		}
		kwPos := search + rel
		if lt := kiroOpenTagLT(body, kwPos); lt >= 0 {
			return lt
		}
		search = kwPos + len("parameter")
	}
}

func kiroParseInvokeBlock(block string) (string, []kiroInvokeParam, bool) {
	openEnd := strings.IndexByte(block, '>')
	if openEnd < 0 {
		return "", nil, false
	}
	name, ok := kiroExtractNameAttr(block[:openEnd+1])
	if !ok || name == "" {
		return "", nil, false
	}
	body := block[openEnd+1:]
	lastClose := strings.LastIndex(body, "</")
	if lastClose < 0 {
		return "", nil, false
	}
	inner := body[:lastClose]
	var params []kiroInvokeParam
	for cursor := 0; ; {
		rest := inner[cursor:]
		ltRel := strings.IndexByte(rest, '<')
		if ltRel < 0 {
			if strings.TrimSpace(rest) != "" {
				return "", nil, false
			}
			return name, params, true
		}
		if strings.TrimSpace(rest[:ltRel]) != "" {
			return "", nil, false
		}
		lt := cursor + ltRel
		kwRel := strings.Index(inner[lt:], "parameter")
		if kwRel < 0 {
			return "", nil, false
		}
		nameKw := lt + kwRel
		if kiroOpenTagLT(inner, nameKw) != lt {
			return "", nil, false
		}
		gtRel := strings.IndexByte(inner[nameKw:], '>')
		if gtRel < 0 {
			return "", nil, false
		}
		tagGT := nameKw + gtRel
		key, ok := kiroExtractNameAttr(inner[nameKw : tagGT+1])
		if !ok || key == "" {
			return "", nil, false
		}
		for _, existing := range params {
			if existing.key == key {
				return "", nil, false
			}
		}
		valueStart := tagGT + 1
		boundary := kiroFindNextParamOpen(inner, valueStart)
		if boundary < 0 {
			boundary = len(inner)
		}
		closeStart, closeEnd := kiroFindLastClose(inner, valueStart, boundary, "parameter>")
		if closeStart < 0 {
			return "", nil, false
		}
		params = append(params, kiroInvokeParam{key, inner[valueStart:closeStart]})
		cursor = closeEnd
	}
}

func kiroCoerceInvokeScalar(raw, kind string) (any, bool) {
	trimmed := strings.TrimSpace(raw)
	var parsed any
	jsonOK := json.Unmarshal([]byte(trimmed), &parsed) == nil
	switch kind {
	case "integer":
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return n, true
		}
	case "number":
		if _, ok := parsed.(float64); jsonOK && ok {
			return parsed, true
		}
	case "boolean":
		switch trimmed {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	case "null":
		if trimmed == "null" {
			return nil, true
		}
	case "array":
		if _, ok := parsed.([]any); jsonOK && ok {
			return parsed, true
		}
	case "object":
		if _, ok := parsed.(map[string]any); jsonOK && ok {
			return parsed, true
		}
	}
	return nil, false
}

func kiroSchemaTypes(schema map[string]any, types *[]string) bool {
	switch value := schema["type"].(type) {
	case string:
		*types = append(*types, value)
	case []any:
		for _, item := range value {
			if name, ok := item.(string); ok {
				*types = append(*types, name)
			}
		}
	default:
		return false
	}
	return true
}

func kiroCoerceInvokeValue(raw string, schema map[string]any) (any, bool) {
	var types []string
	topTyped := kiroSchemaTypes(schema, &types)
	hasBranches, untyped := false, false
	for _, key := range []string{"anyOf", "oneOf"} {
		branches, _ := schema[key].([]any)
		for _, branch := range branches {
			hasBranches = true
			branchSchema, _ := branch.(map[string]any)
			if !kiroSchemaTypes(branchSchema, &types) {
				untyped = true
			}
		}
	}
	if untyped || !topTyped && !hasBranches {
		types = append(types, "string")
	}
	var value any
	found := false
	for _, kind := range types {
		if kind == "string" {
			continue
		}
		if coerced, ok := kiroCoerceInvokeScalar(raw, kind); ok {
			value, found = coerced, true
			break
		}
	}
	if !found {
		allowString := false
		for _, kind := range types {
			allowString = allowString || kind == "string"
		}
		if !allowString {
			return nil, false
		}
		value = raw
	}
	if allowed, ok := schema["enum"].([]any); ok {
		encoded, _ := json.Marshal(value)
		for _, candidate := range allowed {
			if other, _ := json.Marshal(candidate); string(other) == string(encoded) {
				return value, true
			}
		}
		return nil, false
	}
	return value, true
}

func kiroCoerceInvokeParams(params []kiroInvokeParam, schema map[string]any) (map[string]any, bool) {
	properties, _ := schema["properties"].(map[string]any)
	extraAllowed := len(properties) == 0
	switch value := schema["additionalProperties"].(type) {
	case bool:
		extraAllowed = value
	case map[string]any:
		extraAllowed = true
	}
	out := map[string]any{}
	for _, param := range params {
		if property, ok := properties[param.key].(map[string]any); ok {
			value, ok := kiroCoerceInvokeValue(param.value, property)
			if !ok {
				return nil, false
			}
			out[param.key] = value
		} else if extraAllowed {
			out[param.key] = param.value
		} else {
			return nil, false
		}
	}
	required, _ := schema["required"].([]any)
	for _, name := range required {
		if key, ok := name.(string); ok {
			if _, present := out[key]; !present {
				return nil, false
			}
		}
	}
	return out, true
}

type kiroInvokeCall struct {
	name  string
	input map[string]any
}

func kiroValidateInvokeBlock(block string, schemas map[string]map[string]any) (kiroInvokeCall, bool) {
	name, params, ok := kiroParseInvokeBlock(block)
	if !ok {
		return kiroInvokeCall{}, false
	}
	schema, ok := schemas[name]
	if !ok {
		return kiroInvokeCall{}, false
	}
	input, ok := kiroCoerceInvokeParams(params, schema)
	if !ok || !kiroInvokeInputConforms(schema, input) {
		return kiroInvokeCall{}, false
	}
	return kiroInvokeCall{name, input}, true
}

// kiroInvokeInputConforms 用结构化输出的 schema 校验器校验完整的还原结果（含数组元素、嵌套对象），
// 不合规就保留原文本，不发出参数非法的工具调用。
func kiroInvokeInputConforms(schema map[string]any, input map[string]any) bool {
	encoded, err := json.Marshal(input)
	if err != nil {
		return false
	}
	var value any
	if kiroDecodeJSON(encoded, &value) != nil {
		return false
	}
	validator := &kiroSchemaValidator{root: schema, remaining: kiroStructuredMaxSteps}
	return validator.conforms(schema, value)
}

func kiroCouldBecomeInvokeOpen(tail string) bool {
	rest, ok := strings.CutPrefix(tail, "<")
	if !ok || len(rest) > 64 {
		return false
	}
	isIdent := func(s string) bool {
		for i := 0; i < len(s); i++ {
			if !kiroIdentByte(s[i]) {
				return false
			}
		}
		return true
	}
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		return colon > 0 && isIdent(rest[:colon]) && strings.HasPrefix(kiroInvokeKeyword, rest[colon+1:])
	}
	return isIdent(rest)
}

func kiroOpenInvokeTagRejected(tail string, schemas map[string]map[string]any) bool {
	gt := strings.IndexByte(tail, '>')
	if gt < 0 {
		return false
	}
	name, ok := kiroExtractNameAttr(tail[:gt+1])
	_, known := schemas[name]
	return !ok || !known
}

func kiroStripTrailingStrayTokens(before string) string {
	end := len(before)
	for {
		e := end
		for e > 0 && (before[e-1] == '\n' || before[e-1] == '\r') {
			e--
		}
		lineStart := strings.LastIndexByte(before[:e], '\n') + 1
		if _, stray := kiroStrayInvokeTokens[strings.TrimSpace(before[lineStart:e])]; !stray {
			return before[:end]
		}
		end = lineStart
		if end == 0 {
			return ""
		}
	}
}

func kiroAtLineStartAfter(code *kiroMarkdownCode, before string) bool {
	if before == "" {
		return code.atLineStart()
	}
	return strings.HasSuffix(before, "\n")
}

type kiroInvokeCandidate struct {
	raw   strings.Builder
	calls []kiroInvokeCall
}

type kiroInvokeSniffer struct {
	buffer    string
	code      kiroMarkdownCode
	candidate *kiroInvokeCandidate
	disabled  bool
	schemas   map[string]map[string]any
}

func (s *kiroInvokeSniffer) emitText(text string, out *[]kiroSniffOutput) {
	if text == "" {
		return
	}
	s.code.advance(text)
	if n := len(*out); n > 0 && (*out)[n-1].toolName == "" {
		(*out)[n-1].text += text
		return
	}
	*out = append(*out, kiroSniffOutput{text: text})
}

func (s *kiroInvokeSniffer) emitPrefix(length int, out *[]kiroSniffOutput) {
	text := s.buffer[:length]
	s.buffer = s.buffer[length:]
	s.emitText(text, out)
}

func (s *kiroInvokeSniffer) releaseCandidate(out *[]kiroSniffOutput) {
	if s.candidate != nil {
		raw := s.candidate.raw.String()
		s.candidate = nil
		s.emitText(raw, out)
	}
}

func (s *kiroInvokeSniffer) partialHoldLen() int {
	lt := strings.LastIndexByte(s.buffer, '<')
	if lt < 0 || !kiroCouldBecomeInvokeOpen(s.buffer[lt:]) || !kiroAtLineStartAfter(&s.code, s.buffer[:lt]) {
		return 0
	}
	return len(s.buffer) - lt
}

func (s *kiroInvokeSniffer) drain(atEnd bool, out *[]kiroSniffOutput) {
	if s.disabled || len(s.schemas) == 0 {
		s.releaseCandidate(out)
		rest := s.buffer
		s.buffer = ""
		s.emitText(rest, out)
		return
	}
	for {
		if s.candidate != nil {
			rest := s.buffer
			gap := len(rest) - len(strings.TrimLeft(rest, " \t\r\n"))
			if gap == len(rest) {
				return
			}
			if gap == 0 || strings.HasSuffix(rest[:gap], "\n") {
				tail := rest[gap:]
				if kiroFindInvokeStart(tail) == 0 {
					if end := kiroFindInvokeBlockEnd(tail, 0); end >= 0 {
						if call, ok := kiroValidateInvokeBlock(tail[:end], s.schemas); ok {
							s.candidate.raw.WriteString(s.buffer[:gap+end])
							s.candidate.calls = append(s.candidate.calls, call)
							s.buffer = s.buffer[gap+end:]
							continue
						}
					} else if !atEnd && len(tail) <= kiroMaxInvokeHold && !kiroOpenInvokeTagRejected(tail, s.schemas) {
						return
					}
				} else if !atEnd && kiroCouldBecomeInvokeOpen(tail) {
					return
				}
			}
			s.releaseCandidate(out)
			continue
		}
		start := kiroFindInvokeStart(s.buffer)
		if start < 0 {
			hold := 0
			if !atEnd {
				hold = s.partialHoldLen()
			}
			s.emitPrefix(len(s.buffer)-hold, out)
			return
		}
		before := s.buffer[:start]
		stripped := kiroStripTrailingStrayTokens(before)
		probe := s.code.clone()
		probe.advance(stripped)
		if !kiroAtLineStartAfter(&s.code, before) || !kiroAtLineStartAfter(&s.code, stripped) || probe.inCode() {
			s.emitPrefix(start+1, out)
			continue
		}
		if end := kiroFindInvokeBlockEnd(s.buffer, start); end >= 0 {
			if call, ok := kiroValidateInvokeBlock(s.buffer[start:end], s.schemas); ok {
				s.emitPrefix(len(stripped), out)
				candidate := &kiroInvokeCandidate{calls: []kiroInvokeCall{call}}
				candidate.raw.WriteString(s.buffer[:end-len(stripped)])
				s.buffer = s.buffer[end-len(stripped):]
				s.candidate = candidate
			} else {
				s.emitPrefix(end, out)
			}
			continue
		}
		tail := s.buffer[start:]
		if atEnd || len(tail) > kiroMaxInvokeHold || kiroOpenInvokeTagRejected(tail, s.schemas) {
			s.emitPrefix(start+1, out)
			continue
		}
		s.emitPrefix(len(stripped), out)
		return
	}
}

// kiroToolLeakFilters 组合两个过滤器；nil 接收者表示不过滤。
type kiroToolLeakFilters struct {
	xml     kiroXMLFilter
	sniffer kiroInvokeSniffer
}

// kiroSniffOutput 是嗅探器的输出：toolName 为空时是普通文本，否则是还原出的工具调用（Kiro 侧名）。
type kiroSniffOutput struct {
	text      string
	toolName  string
	toolInput map[string]any
}

func newKiroToolLeakFilters(conversion *kiroConversion) *kiroToolLeakFilters {
	filters := &kiroToolLeakFilters{}
	filters.sniffer.schemas = map[string]map[string]any{}
	context := conversion.Request.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil {
		return filters
	}
	for _, tool := range context.Tools {
		if _, known := conversion.KnownToolNames[tool.ToolSpecification.Name]; !known {
			continue
		}
		if schema, ok := tool.ToolSpecification.InputSchema.JSON.(map[string]any); ok {
			filters.sniffer.schemas[tool.ToolSpecification.Name] = schema
		}
	}
	return filters
}

func (f *kiroToolLeakFilters) filterXML(text string) string {
	if f == nil {
		return text
	}
	f.xml.buffer += text
	return f.xml.drain(false)
}

func (f *kiroToolLeakFilters) finishXML() string {
	if f == nil {
		return ""
	}
	return f.xml.drain(true)
}

func (f *kiroToolLeakFilters) pushText(text string) []kiroSniffOutput {
	if f == nil {
		if text == "" {
			return nil
		}
		return []kiroSniffOutput{{text: text}}
	}
	f.sniffer.buffer += text
	var out []kiroSniffOutput
	f.sniffer.drain(false, &out)
	return out
}

// abandon 在上游发出真正的工具调用后关闭嗅探，缓冲内容原样作为文本输出。
func (f *kiroToolLeakFilters) abandon() []kiroSniffOutput {
	if f == nil {
		return nil
	}
	f.sniffer.disabled = true
	var out []kiroSniffOutput
	f.sniffer.releaseCandidate(&out)
	rest := f.sniffer.buffer
	f.sniffer.buffer = ""
	f.sniffer.emitText(rest, &out)
	return out
}

func (f *kiroToolLeakFilters) finishText() []kiroSniffOutput {
	if f == nil {
		return nil
	}
	var out []kiroSniffOutput
	f.sniffer.drain(true, &out)
	if candidate := f.sniffer.candidate; candidate != nil {
		f.sniffer.candidate = nil
		f.sniffer.buffer = ""
		for _, call := range candidate.calls {
			out = append(out, kiroSniffOutput{toolName: call.name, toolInput: call.input})
		}
	}
	rest := f.sniffer.buffer
	f.sniffer.buffer = ""
	f.sniffer.emitText(rest, &out)
	return out
}
