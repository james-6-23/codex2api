package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Kiro 事件 → Anthropic Messages SSE，移植自 kiro.rs src/anthropic/stream.rs。

const (
	kiroRefusalStopReason       = "refusal"
	kiroRepeatGuardTripCount    = 32
	kiroRepeatGuardErrorMessage = "Upstream response stream was interrupted: the model output degenerated into a repeated-token loop. Retry the request."
	kiroToolStreamRefusal       = "Upstream refusal interrupted an incomplete tool call; discard its streamed arguments."
)

var kiroStrayInvokeTokens = map[string]struct{}{"call": {}, "count": {}, "card": {}}

type kiroSSEEvent struct {
	Event string
	Data  map[string]any
}

func (e kiroSSEEvent) bytes() []byte {
	data, _ := json.Marshal(e.Data)
	return []byte("event: " + e.Event + "\ndata: " + string(data) + "\n\n")
}

type kiroBlockState struct {
	blockType        string
	started, stopped bool
}

// kiroSSEState 管理 content block 生命周期，保证 start/delta/stop 成对且有序。
type kiroSSEState struct {
	messageStarted   bool
	messageDeltaSent bool
	messageEnded     bool
	blocks           map[int]*kiroBlockState
	nextIndex        int
	stopReason       string
	hasToolUse       bool
}

func newKiroSSEState() *kiroSSEState {
	return &kiroSSEState{blocks: map[int]*kiroBlockState{}}
}

func (s *kiroSSEState) sortedIndices() []int {
	indices := make([]int, 0, len(s.blocks))
	for index := range s.blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices
}

func (s *kiroSSEState) isOpen(index int, blockType string) bool {
	block := s.blocks[index]
	return block != nil && block.started && !block.stopped && block.blockType == blockType
}

func (s *kiroSSEState) nextBlockIndex() int {
	index := s.nextIndex
	s.nextIndex++
	return index
}

// setStopReason：拒答一旦确定不再被覆盖。
func (s *kiroSSEState) setStopReason(reason string) {
	if s.stopReason == kiroRefusalStopReason {
		return
	}
	s.stopReason = reason
}

func (s *kiroSSEState) resolvedStopReason() string {
	switch {
	case s.stopReason != "":
		return s.stopReason
	case s.hasToolUse:
		return "tool_use"
	}
	return "end_turn"
}

func kiroBlockStopEvent(index int) kiroSSEEvent {
	return kiroSSEEvent{"content_block_stop", map[string]any{"type": "content_block_stop", "index": index}}
}

func (s *kiroSSEState) blockStart(index int, blockType string, contentBlock map[string]any) []kiroSSEEvent {
	var events []kiroSSEEvent
	if blockType == "tool_use" {
		s.hasToolUse = true
		for _, open := range s.sortedIndices() {
			if block := s.blocks[open]; block.blockType == "text" && block.started && !block.stopped {
				block.stopped = true
				events = append(events, kiroBlockStopEvent(open))
			}
		}
	}
	if block := s.blocks[index]; block != nil {
		if block.started {
			return events
		}
		block.started = true
	} else {
		s.blocks[index] = &kiroBlockState{blockType: blockType, started: true}
	}
	return append(events, kiroSSEEvent{"content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": contentBlock}})
}

func (s *kiroSSEState) blockDelta(index int, delta map[string]any) []kiroSSEEvent {
	block := s.blocks[index]
	if block == nil || !block.started || block.stopped {
		return nil
	}
	return []kiroSSEEvent{{"content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta}}}
}

func (s *kiroSSEState) blockStop(index int) []kiroSSEEvent {
	block := s.blocks[index]
	if block == nil || block.stopped {
		return nil
	}
	block.stopped = true
	return []kiroSSEEvent{kiroBlockStopEvent(index)}
}

func (s *kiroSSEState) finalEvents(usage map[string]any) []kiroSSEEvent {
	var events []kiroSSEEvent
	for _, index := range s.sortedIndices() {
		if block := s.blocks[index]; block.started && !block.stopped {
			block.stopped = true
			events = append(events, kiroBlockStopEvent(index))
		}
	}
	if !s.messageDeltaSent {
		s.messageDeltaSent = true
		events = append(events, kiroSSEEvent{"message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": s.resolvedStopReason(), "stop_sequence": nil},
			"usage": usage,
		}})
	}
	if !s.messageEnded {
		s.messageEnded = true
		events = append(events, kiroSSEEvent{"message_stop", map[string]any{"type": "message_stop"}})
	}
	return events
}

func (s *kiroSSEState) errorEvents(errorType, message string) []kiroSSEEvent {
	var events []kiroSSEEvent
	for _, index := range s.sortedIndices() {
		if block := s.blocks[index]; block.started && !block.stopped && block.blockType != "tool_use" {
			block.stopped = true
			events = append(events, kiroBlockStopEvent(index))
		}
	}
	s.messageEnded = true
	return append(events, kiroSSEEvent{"error", map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}}})
}

type kiroToolBuffer struct {
	name  string
	input strings.Builder
}

type kiroToolJSONError struct {
	toolUseID, name, detail string
	incompleteBytes         int
}

func (e *kiroToolJSONError) message() string {
	if e.detail != "" {
		return fmt.Sprintf("Upstream returned invalid JSON for tool_use %s (%s): %s", e.toolUseID, e.name, e.detail)
	}
	return fmt.Sprintf("Upstream ended before completing tool_use %s (%s) JSON input; received %d bytes. Discard any streamed arguments; the tool call must not be executed.", e.toolUseID, e.name, e.incompleteBytes)
}

// kiroStreamContext 是单个响应的转换状态。
type kiroStreamContext struct {
	state       *kiroSSEState
	model       string
	messageID   string
	inputTokens int

	contextInputTokens *int
	providerUsage      *kiroTokenUsage
	outputTokens       int

	toolBlockIndices map[string]int
	toolInputCounts  map[int][2]int
	toolNameMap      map[string]string
	toolBuffers      map[string]*kiroToolBuffer
	toolJSONError    *kiroToolJSONError
	toolRefused      bool

	thinkingEnabled      bool
	thinkingBuffer       string
	inThinkingBlock      bool
	thinkingExtracted    bool
	thinkingBlockIndex   int
	pendingSignature     string
	textBlockIndex       int
	stripThinkingNewline bool

	credits  float64
	metering *kiroMeteringEvent
	// cache 只在开启本地 cache 模拟时非零，且只在上游没给精确用量时参与拆分。
	cache kiroCacheUsage

	repeatLastLine string
	repeatRun      int
	repeatTripped  bool

	nativeStopReason  string
	contextWindowFull bool
	filter            *kiroToolLeakFilters
}

func newKiroStreamContext(model string, inputTokens int, thinkingEnabled bool, conversion *kiroConversion) *kiroStreamContext {
	toolNames := map[string]string{}
	var filter *kiroToolLeakFilters
	if conversion != nil {
		toolNames = conversion.ToolNameMap
		filter = newKiroToolLeakFilters(conversion)
	}
	return &kiroStreamContext{
		state:              newKiroSSEState(),
		model:              model,
		messageID:          "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		inputTokens:        inputTokens,
		toolBlockIndices:   map[string]int{},
		toolInputCounts:    map[int][2]int{},
		toolNameMap:        toolNames,
		toolBuffers:        map[string]*kiroToolBuffer{},
		thinkingEnabled:    thinkingEnabled,
		thinkingBlockIndex: -1,
		textBlockIndex:     -1,
		filter:             filter,
	}
}

func (c *kiroStreamContext) initialEvents() []kiroSSEEvent {
	var events []kiroSSEEvent
	if !c.state.messageStarted {
		c.state.messageStarted = true
		events = append(events, kiroSSEEvent{"message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": c.messageID, "type": "message", "role": "assistant", "content": []any{}, "model": c.model,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": c.inputTokens, "output_tokens": 1, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		}}})
	}
	if c.thinkingEnabled {
		return events
	}
	c.textBlockIndex = c.state.nextBlockIndex()
	return append(events, c.state.blockStart(c.textBlockIndex, "text", map[string]any{"type": "text", "text": ""})...)
}

// processFrame 处理一帧上游事件。
func (c *kiroStreamContext) processFrame(frame kiroFrame) []kiroSSEEvent {
	switch frame.MessageType {
	case "exception":
		if frame.ExceptionType == "ContentLengthExceededException" {
			c.state.setStopReason("max_tokens")
		}
		return nil
	case "error":
		return nil
	}
	switch frame.EventType {
	case "assistantResponseEvent":
		event, err := decodeKiroPayload[kiroAssistantEvent](frame)
		if err != nil {
			return nil
		}
		return c.processAssistantResponse(event.Content)
	case "toolUseEvent":
		event, err := decodeKiroPayload[kiroToolUseEvent](frame)
		if err != nil {
			return nil
		}
		return c.processToolUse(event)
	case "reasoningContentEvent":
		event, err := decodeKiroPayload[kiroReasoningEvent](frame)
		if err != nil {
			return nil
		}
		return c.processReasoning(event)
	case "metadataEvent":
		event, err := decodeKiroPayload[kiroMetadataEvent](frame)
		if err != nil {
			return nil
		}
		if reason := event.truncationStopReason(); reason != "" && c.state.stopReason == "" {
			c.state.setStopReason(reason)
		}
		if event.TokenUsage != nil {
			usage := event.TokenUsage.sanitized()
			c.providerUsage = &usage
		}
		if event.StopReason != "" {
			c.nativeStopReason = event.StopReason
		}
		if event.contentFiltered() {
			c.state.setStopReason(kiroRefusalStopReason)
		}
	case "contextUsageEvent":
		event, err := decodeKiroPayload[kiroContextUsageEvent](frame)
		if err != nil {
			return nil
		}
		tokens := int(event.ContextUsagePercentage * float64(kiroContextWindow(c.model)) / 100)
		c.contextInputTokens = &tokens
		if event.ContextUsagePercentage >= 100 {
			c.contextWindowFull = true
		}
	case "meteringEvent":
		event, err := decodeKiroPayload[kiroMeteringEvent](frame)
		if err != nil {
			return nil
		}
		c.credits += event.Usage
		c.metering = &event
	}
	return nil
}

func (c *kiroStreamContext) processAssistantResponse(content string) []kiroSSEEvent {
	content = c.filter.filterXML(content)
	if content == "" {
		return nil
	}
	var events []kiroSSEEvent
	if c.isThinkingBlockOpen() && !c.inThinkingBlock {
		events = append(events, c.closeOpenThinkingBlock()...)
	}
	c.outputTokens += kiroEstimateTokens(content)
	if c.thinkingEnabled {
		return append(events, c.processContentWithThinking(content)...)
	}
	return append(events, c.textDeltaEvents(content)...)
}

// processContentWithThinking 从正文里抽出 <thinking>…</thinking>（旧模型靠提示前缀开启思考）。
func (c *kiroStreamContext) processContentWithThinking(content string) []kiroSSEEvent {
	var events []kiroSSEEvent
	c.thinkingBuffer += content
	for {
		switch {
		case !c.inThinkingBlock && !c.thinkingExtracted:
			head := strings.TrimLeft(c.thinkingBuffer, " \t\r\n")
			if head != "" && !strings.HasPrefix(head, "<thinking>") && !strings.HasPrefix("<thinking>", head) {
				c.thinkingExtracted = true
				continue
			}
			start := kiroFindThinkingStartTag(c.thinkingBuffer)
			if start >= 0 && strings.TrimSpace(c.thinkingBuffer[:start]) == "" {
				c.inThinkingBlock = true
				c.stripThinkingNewline = true
				c.thinkingBuffer = c.thinkingBuffer[start+len("<thinking>"):]
				c.thinkingBlockIndex = c.state.nextBlockIndex()
				events = append(events, c.state.blockStart(c.thinkingBlockIndex, "thinking", map[string]any{"type": "thinking", "thinking": ""})...)
				continue
			}
			safe := kiroCharBoundary(c.thinkingBuffer, len(c.thinkingBuffer)-len("<thinking>"))
			if safe > 0 && strings.TrimSpace(c.thinkingBuffer[:safe]) != "" {
				events = append(events, c.textDeltaEvents(c.thinkingBuffer[:safe])...)
				c.thinkingBuffer = c.thinkingBuffer[safe:]
			}
			return events
		case c.inThinkingBlock:
			if c.stripThinkingNewline {
				if strings.HasPrefix(c.thinkingBuffer, "\n") {
					c.thinkingBuffer = c.thinkingBuffer[1:]
					c.stripThinkingNewline = false
				} else if c.thinkingBuffer != "" {
					c.stripThinkingNewline = false
				}
			}
			if end := kiroFindThinkingEndTag(c.thinkingBuffer); end >= 0 {
				if thinking := c.thinkingBuffer[:end]; thinking != "" {
					events = append(events, c.thinkingDelta(thinking)...)
				}
				c.inThinkingBlock = false
				c.thinkingExtracted = true
				events = append(events, c.finishThinkingBlock(kiroThinkingSignaturePlaceholder)...)
				c.thinkingBuffer = c.thinkingBuffer[end+kiroThinkingEndStripLen(c.thinkingBuffer, end):]
				continue
			}
			safe := kiroCharBoundary(c.thinkingBuffer, len(c.thinkingBuffer)-len("</thinking>\n\n"))
			if safe > 0 {
				events = append(events, c.thinkingDelta(c.thinkingBuffer[:safe])...)
				c.thinkingBuffer = c.thinkingBuffer[safe:]
			}
			return events
		default:
			if c.thinkingBuffer != "" {
				remaining := c.thinkingBuffer
				c.thinkingBuffer = ""
				events = append(events, c.textDeltaEvents(remaining)...)
			}
			return events
		}
	}
}

func (c *kiroStreamContext) thinkingDelta(text string) []kiroSSEEvent {
	if c.thinkingBlockIndex < 0 {
		return nil
	}
	return c.state.blockDelta(c.thinkingBlockIndex, map[string]any{"type": "thinking_delta", "thinking": text})
}

// finishThinkingBlock 发空 delta + 签名后关闭当前 thinking 块。
func (c *kiroStreamContext) finishThinkingBlock(signature string) []kiroSSEEvent {
	if c.thinkingBlockIndex < 0 {
		return nil
	}
	events := c.thinkingDelta("")
	events = append(events, c.state.blockDelta(c.thinkingBlockIndex, map[string]any{"type": "signature_delta", "signature": signature})...)
	return append(events, c.state.blockStop(c.thinkingBlockIndex)...)
}

func (c *kiroStreamContext) textDeltaEvents(text string) []kiroSSEEvent {
	if text == "" {
		return nil
	}
	return c.emitSniffed(c.filter.pushText(text))
}

func (c *kiroStreamContext) emitSniffed(outputs []kiroSniffOutput) []kiroSSEEvent {
	var events []kiroSSEEvent
	for _, output := range outputs {
		if output.toolName == "" {
			events = append(events, c.emitTextDeltaRaw(output.text)...)
			continue
		}
		id := "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		events = append(events, c.emitCompletedToolUse(id, c.clientToolName(output.toolName), output.toolInput)...)
	}
	return events
}

// repeatGuardFilter 截断上游退化成的 "call\ncall\n…" 洪泛，达到阈值后整条流报错。
func (c *kiroStreamContext) repeatGuardFilter(text string) string {
	if c.repeatTripped {
		return ""
	}
	var kept strings.Builder
	for _, segment := range strings.SplitAfter(text, "\n") {
		if segment == "" {
			continue
		}
		line := strings.TrimSpace(segment)
		if _, stray := kiroStrayInvokeTokens[line]; stray {
			if line == c.repeatLastLine {
				c.repeatRun++
			} else {
				c.repeatLastLine, c.repeatRun = line, 1
			}
			if c.repeatRun >= kiroRepeatGuardTripCount {
				c.repeatTripped = true
				return kept.String()
			}
		} else if line != "" {
			c.repeatLastLine, c.repeatRun = line, 0
		}
		kept.WriteString(segment)
	}
	return kept.String()
}

func (c *kiroStreamContext) emitTextDeltaRaw(text string) []kiroSSEEvent {
	text = c.repeatGuardFilter(text)
	if text == "" {
		return nil
	}
	var events []kiroSSEEvent
	if c.textBlockIndex >= 0 && !c.state.isOpen(c.textBlockIndex, "text") {
		c.textBlockIndex = -1
	}
	if c.textBlockIndex < 0 {
		c.textBlockIndex = c.state.nextBlockIndex()
		events = append(events, c.state.blockStart(c.textBlockIndex, "text", map[string]any{"type": "text", "text": ""})...)
	}
	return append(events, c.state.blockDelta(c.textBlockIndex, map[string]any{"type": "text_delta", "text": text})...)
}

func (c *kiroStreamContext) isThinkingBlockOpen() bool {
	return c.thinkingBlockIndex >= 0 && c.state.isOpen(c.thinkingBlockIndex, "thinking")
}

func (c *kiroStreamContext) closeOpenTextBlock() []kiroSSEEvent {
	if c.textBlockIndex < 0 {
		return nil
	}
	index := c.textBlockIndex
	c.textBlockIndex = -1
	if !c.state.isOpen(index, "text") {
		return nil
	}
	return c.state.blockStop(index)
}

func (c *kiroStreamContext) ensureThinkingBlock() []kiroSSEEvent {
	if c.isThinkingBlockOpen() {
		return nil
	}
	var events []kiroSSEEvent
	buffered := c.thinkingBuffer
	c.thinkingBuffer = ""
	if strings.TrimSpace(buffered) != "" {
		events = append(events, c.textDeltaEvents(buffered)...)
	}
	events = append(events, c.closeOpenTextBlock()...)
	c.thinkingBlockIndex = c.state.nextBlockIndex()
	c.thinkingExtracted = true
	return append(events, c.state.blockStart(c.thinkingBlockIndex, "thinking", map[string]any{"type": "thinking", "thinking": ""})...)
}

func (c *kiroStreamContext) closeOpenThinkingBlock() []kiroSSEEvent {
	if !c.isThinkingBlockOpen() {
		return nil
	}
	signature := c.pendingSignature
	c.pendingSignature = ""
	if signature == "" {
		signature = kiroThinkingSignaturePlaceholder
	}
	return c.finishThinkingBlock(signature)
}

func (c *kiroStreamContext) processReasoning(event kiroReasoningEvent) []kiroSSEEvent {
	if !c.thinkingEnabled {
		c.outputTokens += kiroEstimateTokens(event.Text)
		return nil
	}
	var events []kiroSSEEvent
	if event.Signature != "" {
		c.pendingSignature = event.Signature
	}
	if event.Text != "" {
		c.outputTokens += kiroEstimateTokens(event.Text)
		events = append(events, c.ensureThinkingBlock()...)
		events = append(events, c.thinkingDelta(event.Text)...)
	}
	if event.RedactedContent != "" {
		c.outputTokens += 8
		events = append(events, c.closeOpenThinkingBlock()...)
		events = append(events, c.closeOpenTextBlock()...)
		index := c.state.nextBlockIndex()
		events = append(events, c.state.blockStart(index, "redacted_thinking", map[string]any{"type": "redacted_thinking", "data": event.RedactedContent})...)
		events = append(events, c.state.blockStop(index)...)
	}
	return events
}

func (c *kiroStreamContext) clientToolName(kiroName string) string {
	if original, ok := c.toolNameMap[kiroName]; ok {
		return original
	}
	return kiroName
}

func (c *kiroStreamContext) startToolUse(id, name string) (int, []kiroSSEEvent) {
	if index, ok := c.toolBlockIndices[id]; ok {
		return index, nil
	}
	index := c.state.nextBlockIndex()
	c.toolBlockIndices[id] = index
	return index, c.state.blockStart(index, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
}

// emitToolInputFragment 按累计字符数估算输出 token，避免同样内容因分片多而重复高估。
func (c *kiroStreamContext) emitToolInputFragment(index int, input string) []kiroSSEEvent {
	if input == "" {
		return nil
	}
	counts := c.toolInputCounts[index]
	previous := 0
	if counts != [2]int{} {
		previous = kiroEstimateCharacterTokens(counts[0], counts[1])
	}
	chinese, other := kiroCountTokenCharacters(input)
	counts[0] += chinese
	counts[1] += other
	c.toolInputCounts[index] = counts
	c.outputTokens += kiroEstimateCharacterTokens(counts[0], counts[1]) - previous
	return c.state.blockDelta(index, map[string]any{"type": "input_json_delta", "partial_json": input})
}

func (c *kiroStreamContext) emitCompletedToolUse(id, name string, input any) []kiroSSEEvent {
	index, events := c.startToolUse(id, name)
	encoded, _ := json.Marshal(input)
	events = append(events, c.emitToolInputFragment(index, string(encoded))...)
	return append(events, c.state.blockStop(index)...)
}

func (c *kiroStreamContext) processToolUse(event kiroToolUseEvent) []kiroSSEEvent {
	var events []kiroSSEEvent
	c.state.hasToolUse = true
	if c.isThinkingBlockOpen() && !c.inThinkingBlock {
		events = append(events, c.closeOpenThinkingBlock()...)
	}
	events = append(events, c.emitSniffed(c.filter.abandon())...)
	events = append(events, c.flushThinkingBufferBeforeTool()...)

	buffer := c.toolBuffers[event.ToolUseID]
	if buffer == nil {
		buffer = &kiroToolBuffer{name: event.Name}
		c.toolBuffers[event.ToolUseID] = buffer
	} else if buffer.name == "" {
		buffer.name = event.Name
	}
	kiroName := buffer.name
	emptyInput := strings.TrimSpace(buffer.input.String()) == "" && strings.TrimSpace(event.Input) == ""
	prefix := ""
	if _, started := c.toolBlockIndices[event.ToolUseID]; !started {
		prefix = buffer.input.String()
	}
	buffer.input.WriteString(event.Input)
	if kiroName == "" {
		return events
	}
	if event.Stop {
		delete(c.toolBuffers, event.ToolUseID)
		if raw := strings.TrimSpace(buffer.input.String()); raw != "" && !json.Valid([]byte(raw)) {
			c.toolJSONError = &kiroToolJSONError{toolUseID: event.ToolUseID, name: kiroName, detail: "invalid JSON"}
			c.state.setStopReason("error")
			return events
		}
	}
	if !emptyInput || event.Stop {
		index, start := c.startToolUse(event.ToolUseID, c.clientToolName(kiroName))
		events = append(events, start...)
		events = append(events, c.emitToolInputFragment(index, prefix)...)
		events = append(events, c.emitToolInputFragment(index, event.Input)...)
		if event.Stop {
			if emptyInput {
				events = append(events, c.emitToolInputFragment(index, "{}")...)
			}
			events = append(events, c.state.blockStop(index)...)
		}
	}
	return events
}

// flushThinkingBufferBeforeTool：工具调用到来时，把仍在缓冲的思考 / 正文先发出去。
func (c *kiroStreamContext) flushThinkingBufferBeforeTool() []kiroSSEEvent {
	var events []kiroSSEEvent
	if c.thinkingEnabled && c.inThinkingBlock {
		if end := kiroFindThinkingEndTagAtBufferEnd(c.thinkingBuffer); end >= 0 {
			if thinking := c.thinkingBuffer[:end]; thinking != "" {
				events = append(events, c.thinkingDelta(thinking)...)
			}
			c.inThinkingBlock = false
			c.thinkingExtracted = true
			events = append(events, c.finishThinkingBlock(kiroThinkingSignaturePlaceholder)...)
			remaining := strings.TrimLeft(c.thinkingBuffer[end+len("</thinking>"):], " \t\r\n")
			c.thinkingBuffer = ""
			events = append(events, c.textDeltaEvents(remaining)...)
		}
	}
	if c.thinkingEnabled && !c.inThinkingBlock && !c.thinkingExtracted && c.thinkingBuffer != "" {
		buffered := c.thinkingBuffer
		c.thinkingBuffer = ""
		events = append(events, c.textDeltaEvents(buffered)...)
	}
	return events
}

// resolvedUsage 返回 (uncached input, cache write, cache read, output)。上游精确用量优先；
// 否则用 contextUsageEvent 推算的总输入减去输出，再按本地 cache 模拟拆分。
func (c *kiroStreamContext) resolvedUsage() (int, int, int, int) {
	if c.providerUsage != nil {
		usage := c.providerUsage.sanitized()
		return usage.UncachedInputTokens, usage.CacheWriteInputTokens, usage.CacheReadInputTokens, usage.OutputTokens
	}
	total := max(c.inputTokens, 0)
	if c.contextInputTokens != nil {
		total = max(max(*c.contextInputTokens, 0)-max(c.outputTokens, 0), 0)
	}
	input, write, read := c.cache.split(total)
	return input, write, read, max(c.outputTokens, 0)
}

func (c *kiroStreamContext) errorEvents(errorType, message string) []kiroSSEEvent {
	return append(c.closeOpenThinkingBlock(), c.state.errorEvents(errorType, message)...)
}

func (c *kiroStreamContext) finalEvents() []kiroSSEEvent {
	var events []kiroSSEEvent
	if leftover := c.filter.finishXML(); leftover != "" {
		if c.thinkingEnabled {
			events = append(events, c.processContentWithThinking(leftover)...)
		} else {
			events = append(events, c.textDeltaEvents(leftover)...)
		}
	}
	if c.isThinkingBlockOpen() && !c.inThinkingBlock {
		events = append(events, c.closeOpenThinkingBlock()...)
	}
	if c.thinkingEnabled && c.thinkingBuffer != "" {
		if c.inThinkingBlock {
			if end := kiroFindThinkingEndTagAtBufferEnd(c.thinkingBuffer); end >= 0 {
				if thinking := c.thinkingBuffer[:end]; thinking != "" {
					events = append(events, c.thinkingDelta(thinking)...)
				}
				events = append(events, c.finishThinkingBlock(kiroThinkingSignaturePlaceholder)...)
				remaining := strings.TrimLeft(c.thinkingBuffer[end+len("</thinking>"):], " \t\r\n")
				c.thinkingBuffer, c.inThinkingBlock, c.thinkingExtracted = "", false, true
				events = append(events, c.textDeltaEvents(remaining)...)
			} else {
				events = append(events, c.thinkingDelta(c.thinkingBuffer)...)
				events = append(events, c.finishThinkingBlock(kiroThinkingSignaturePlaceholder)...)
			}
		} else {
			events = append(events, c.textDeltaEvents(c.thinkingBuffer)...)
		}
		c.thinkingBuffer = ""
	}
	events = append(events, c.emitSniffed(c.filter.finishText())...)
	if c.state.resolvedStopReason() == kiroRefusalStopReason {
		c.toolJSONError = nil
		for _, index := range c.toolBlockIndices {
			if c.state.isOpen(index, "tool_use") {
				c.toolRefused = true
				return append(events, c.errorEvents("permission_error", kiroToolStreamRefusal)...)
			}
		}
	} else if c.toolJSONError == nil {
		events = append(events, c.finishPendingTools()...)
	}
	if c.toolJSONError != nil {
		return append(events, c.errorEvents("upstream_tool_json_error", c.toolJSONError.message())...)
	}
	if c.repeatTripped {
		return append(events, c.errorEvents("api_error", kiroRepeatGuardErrorMessage)...)
	}
	switch strings.ToUpper(c.nativeStopReason) {
	case "MAX_TOKENS":
		c.state.setStopReason("max_tokens")
	case "MODEL_CONTEXT_WINDOW_EXCEEDED":
		c.state.setStopReason("model_context_window_exceeded")
	case "":
		if c.contextWindowFull {
			c.state.setStopReason("model_context_window_exceeded")
		}
	}
	input, cacheWrite, cacheRead, output := c.resolvedUsage()
	usage := map[string]any{"input_tokens": input, "output_tokens": output, "cache_creation_input_tokens": cacheWrite, "cache_read_input_tokens": cacheRead}
	if c.metering != nil {
		usage["credit_usage"] = c.metering.Usage
		usage["credit_unit"] = c.metering.Unit
		usage["credit_unit_plural"] = c.metering.UnitPlural
	}
	return append(events, c.state.finalEvents(usage)...)
}

// finishPendingTools：只收到空输入、没等到 stop 的工具按 {} 补全；有半截 JSON 的整条流报错，
// 避免客户端执行参数被截断的工具调用。
func (c *kiroStreamContext) finishPendingTools() []kiroSSEEvent {
	var events []kiroSSEEvent
	ids := make([]string, 0, len(c.toolBuffers))
	for id := range c.toolBuffers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var truncated *kiroToolJSONError
	for _, id := range ids {
		buffer := c.toolBuffers[id]
		if size := len(buffer.input.String()); strings.TrimSpace(buffer.input.String()) != "" && (truncated == nil || size > truncated.incompleteBytes) {
			truncated = &kiroToolJSONError{toolUseID: id, name: buffer.name, incompleteBytes: size}
		}
	}
	if truncated != nil {
		c.toolJSONError = truncated
		c.state.setStopReason("error")
		return nil
	}
	for _, id := range ids {
		if _, started := c.toolBlockIndices[id]; started {
			index := c.toolBlockIndices[id]
			events = append(events, c.emitToolInputFragment(index, "{}")...)
			events = append(events, c.state.blockStop(index)...)
			continue
		}
		events = append(events, c.emitCompletedToolUse(id, c.clientToolName(c.toolBuffers[id].name), map[string]any{})...)
	}
	c.toolBuffers = map[string]*kiroToolBuffer{}
	return events
}

// ---- thinking 标签定位：引号/反引号包住的标签不算真正的标签 ----

const kiroQuoteChars = "`\"'\\#!@$%^&*()-_=+[]{};:<>,.?/"

func kiroIsQuoteChar(buffer string, pos int) bool {
	return pos >= 0 && pos < len(buffer) && strings.IndexByte(kiroQuoteChars, buffer[pos]) >= 0
}

func kiroFindThinkingStartTag(buffer string) int {
	const tag = "<thinking>"
	for start := 0; ; {
		pos := strings.Index(buffer[start:], tag)
		if pos < 0 {
			return -1
		}
		abs := start + pos
		if !kiroIsQuoteChar(buffer, abs-1) && !kiroIsQuoteChar(buffer, abs+len(tag)) {
			return abs
		}
		start = abs + 1
	}
}

func kiroFindThinkingEndTag(buffer string) int {
	const tag = "</thinking>"
	for start := 0; ; {
		pos := strings.Index(buffer[start:], tag)
		if pos < 0 {
			return -1
		}
		abs := start + pos
		after := abs + len(tag)
		if kiroIsQuoteChar(buffer, abs-1) || kiroIsQuoteChar(buffer, after) {
			start = abs + 1
			continue
		}
		rest := buffer[after:]
		if len(rest) < 2 {
			return -1
		}
		if strings.HasPrefix(rest, "\n\n") || strings.HasPrefix(rest, "\n") && !strings.HasPrefix(rest[1:], "\n") {
			return abs
		}
		start = abs + 1
	}
}

func kiroFindThinkingEndTagAtBufferEnd(buffer string) int {
	const tag = "</thinking>"
	for start := 0; ; {
		pos := strings.Index(buffer[start:], tag)
		if pos < 0 {
			return -1
		}
		abs := start + pos
		after := abs + len(tag)
		if !kiroIsQuoteChar(buffer, abs-1) && !kiroIsQuoteChar(buffer, after) && strings.TrimSpace(buffer[after:]) == "" {
			return abs
		}
		start = abs + 1
	}
}

func kiroThinkingEndStripLen(buffer string, pos int) int {
	after := pos + len("</thinking>")
	newlines := 0
	for newlines < 2 && after+newlines < len(buffer) && buffer[after+newlines] == '\n' {
		newlines++
	}
	return len("</thinking>") + newlines
}

func kiroCharBoundary(value string, target int) int {
	if target >= len(value) {
		return len(value)
	}
	if target <= 0 {
		return 0
	}
	for target > 0 && !utf8.RuneStart(value[target]) {
		target--
	}
	return target
}

// ---- token 估算（上游没给精确用量时的兜底）----

func kiroCountTokenCharacters(text string) (int, int) {
	chinese, other := 0, 0
	for _, r := range text {
		if r >= 0x4E00 && r <= 0x9FFF {
			chinese++
		} else {
			other++
		}
	}
	return chinese, other
}

func kiroEstimateCharacterTokens(chinese, other int) int {
	return max((chinese*2+2)/3+(other+3)/4, 1)
}

func kiroEstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return kiroEstimateCharacterTokens(kiroCountTokenCharacters(text))
}
