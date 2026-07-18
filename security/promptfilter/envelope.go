package promptfilter

import (
	"strings"

	"github.com/tidwall/gjson"
)

type Protocol string

const (
	ProtocolUnknown   Protocol = "unknown"
	ProtocolResponses Protocol = "responses"
	ProtocolChat      Protocol = "chat_completions"
	ProtocolMessages  Protocol = "messages"
	ProtocolImages    Protocol = "images"
)

type Transport string

const (
	TransportHTTP      Transport = "http"
	TransportWebSocket Transport = "websocket"
)

type ModelFamily string

const (
	ModelFamilyOpenAI    ModelFamily = "openai"
	ModelFamilyAnthropic ModelFamily = "anthropic"
	ModelFamilyXAI       ModelFamily = "xai"
	ModelFamilyUnknown   ModelFamily = "unknown"
)

type SegmentOrigin string

type SegmentTrust string

const (
	OriginCurrentUser       SegmentOrigin = "current_user"
	OriginHistory           SegmentOrigin = "history"
	OriginSystem            SegmentOrigin = "system"
	OriginDeveloper         SegmentOrigin = "developer"
	OriginInstructions      SegmentOrigin = "instructions"
	OriginToolOutput        SegmentOrigin = "tool_output"
	OriginToolArguments     SegmentOrigin = "tool_arguments"
	OriginAttachmentRefs    SegmentOrigin = "attachment_refs"
	OriginSessionContext    SegmentOrigin = "session_context"
	OriginAttachmentContent SegmentOrigin = "attachment_content"
)

const (
	SegmentTrustClientSupplied SegmentTrust = "client_supplied"
	SegmentTrustGatewaySigned  SegmentTrust = "gateway_signed"
	SegmentTrustServerInjected SegmentTrust = "server_injected"
)

// Segment keeps request text and its provenance separate. Sequence reflects
// wire order within the request; Role retains the protocol role when present.
type Segment struct {
	Origin   SegmentOrigin `json:"origin"`
	Role     string        `json:"role,omitempty"`
	Text     string        `json:"text"`
	Sequence int           `json:"sequence"`
	Linked   bool          `json:"linked,omitempty"`
	Trust    SegmentTrust  `json:"trust"`
}

type RequestEnvelope struct {
	Endpoint       string      `json:"endpoint"`
	Protocol       Protocol    `json:"protocol"`
	Transport      Transport   `json:"transport"`
	RequestedModel string      `json:"requested_model,omitempty"`
	EffectiveModel string      `json:"effective_model,omitempty"`
	ModelFamily    ModelFamily `json:"model_family"`
	Segments       []Segment   `json:"segments"`
}

func BuildEnvelope(body []byte, endpoint string, requestedModel string, transport Transport, maxLen int) RequestEnvelope {
	return BuildEnvelopeWithModels(body, endpoint, requestedModel, "", transport, maxLen)
}

func BuildEnvelopeWithModels(body []byte, endpoint string, requestedModel string, effectiveModel string, transport Transport, maxLen int) RequestEnvelope {
	protocol := ProtocolForEndpoint(endpoint)
	if transport != TransportWebSocket {
		transport = TransportHTTP
	}
	if requestedModel = strings.TrimSpace(requestedModel); requestedModel == "" && gjson.ValidBytes(body) {
		requestedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	envelope := RequestEnvelope{
		Endpoint:       strings.TrimSpace(endpoint),
		Protocol:       protocol,
		Transport:      transport,
		RequestedModel: requestedModel,
		EffectiveModel: strings.TrimSpace(effectiveModel),
		ModelFamily:    ResolveModelFamily(requestedModel, effectiveModel),
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return envelope
	}

	builder := envelopeBuilder{envelope: &envelope, maxLen: maxLen}
	switch protocol {
	case ProtocolResponses:
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "instructions"))
		builder.appendMessages(gjson.GetBytes(body, "input"))
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "prompt"))
		if !envelopeHasOrigin(envelope, OriginCurrentUser) {
			builder.appendMessages(gjson.GetBytes(body, "messages"))
		}
	case ProtocolChat:
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "instructions"))
		builder.appendMessages(gjson.GetBytes(body, "messages"))
	case ProtocolMessages:
		builder.appendResult(OriginSystem, "system", gjson.GetBytes(body, "system"))
		builder.appendMessages(gjson.GetBytes(body, "messages"))
	case ProtocolImages:
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "prompt"))
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "style"))
		builder.appendAttachmentReferences(gjson.ParseBytes(body), "user")
	default:
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "instructions"))
		builder.appendMessages(gjson.GetBytes(body, "messages"))
		builder.appendMessages(gjson.GetBytes(body, "input"))
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "prompt"))
	}
	return envelope
}

func ProtocolForEndpoint(endpoint string) Protocol {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "response", "responses", "responses_compact", "/v1/responses", "/v1/responses/compact":
		return ProtocolResponses
	case "chat", "chat_completions", "/v1/chat/completions":
		return ProtocolChat
	case "messages", "anthropic", "/v1/messages":
		return ProtocolMessages
	case "image", "images", "images_generations", "images_edits", "/v1/images/generations", "/v1/images/edits":
		return ProtocolImages
	default:
		return ProtocolUnknown
	}
}

func ResolveModelFamily(requestedModel string, effectiveModel string) ModelFamily {
	model := strings.ToLower(strings.TrimSpace(effectiveModel))
	if model == "" {
		model = strings.ToLower(strings.TrimSpace(requestedModel))
	}
	switch {
	case strings.HasPrefix(model, "claude") || strings.Contains(model, "anthropic"):
		return ModelFamilyAnthropic
	case strings.HasPrefix(model, "grok") || strings.Contains(model, "xai") || strings.Contains(model, "x-ai"):
		return ModelFamilyXAI
	case strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "chatgpt-") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") || strings.Contains(model, "codex"):
		return ModelFamilyOpenAI
	default:
		return ModelFamilyUnknown
	}
}

func (e RequestEnvelope) SegmentsForOrigin(origin SegmentOrigin) []Segment {
	out := make([]Segment, 0)
	for _, segment := range e.Segments {
		if segment.Origin == origin {
			out = append(out, segment)
		}
	}
	return out
}

func envelopeHasOrigin(envelope RequestEnvelope, origin SegmentOrigin) bool {
	for _, segment := range envelope.Segments {
		if segment.Origin == origin {
			return true
		}
	}
	return false
}

type envelopeBuilder struct {
	envelope *RequestEnvelope
	maxLen   int
	sequence int
}

func (b *envelopeBuilder) append(origin SegmentOrigin, role string, text string) {
	if b == nil || b.envelope == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	text = limitScanText(text, b.maxLen)
	b.envelope.Segments = append(b.envelope.Segments, Segment{
		Origin: origin, Role: strings.ToLower(strings.TrimSpace(role)), Text: text, Sequence: b.sequence, Trust: SegmentTrustClientSupplied,
	})
	b.sequence++
}

func (b *envelopeBuilder) appendResult(origin SegmentOrigin, role string, result gjson.Result) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	switch {
	case result.IsArray():
		for _, item := range result.Array() {
			b.appendResult(origin, role, item)
		}
	case result.IsObject():
		blockType := strings.ToLower(strings.TrimSpace(result.Get("type").String()))
		switch blockType {
		case "tool_result", "function_call_output", "computer_call_output", "mcp_call_output":
			b.appendResult(OriginToolOutput, "tool", firstExistingResult(result, "output", "content", "text"))
		case "tool_use", "function_call", "computer_call", "mcp_call":
			b.appendToolArguments(result, role)
		default:
			if text := result.Get("text"); text.Type == gjson.String {
				b.append(origin, role, text.String())
			}
			if content := result.Get("content"); content.Exists() {
				b.appendResult(origin, role, content)
			}
		}
		b.appendAttachmentReferences(result, role)
	case result.Type == gjson.String:
		b.append(origin, role, result.String())
	}
}

func (b *envelopeBuilder) appendMessages(result gjson.Result) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	if !result.IsArray() {
		if result.IsObject() {
			if role := strings.ToLower(strings.TrimSpace(result.Get("role").String())); role != "" {
				b.appendMessage(result, role, true)
				return
			}
			if itemType := strings.ToLower(strings.TrimSpace(result.Get("type").String())); itemType != "" {
				b.appendTypedInputItem(result, true)
				return
			}
		}
		b.appendResult(OriginCurrentUser, "user", result)
		return
	}

	items := result.Array()
	currentItems := selectCurrentInputItems(items)
	segmentStarts := make([]int, len(items))
	segmentEnds := make([]int, len(items))
	for index, item := range items {
		segmentStarts[index] = len(b.envelope.Segments)
		if role := inputItemRole(item); role != "" {
			b.appendMessage(item, role, currentItems[index])
		} else {
			b.appendTypedInputItem(item, currentItems[index])
		}
		segmentEnds[index] = len(b.envelope.Segments)
	}

	firstCurrent := -1
	currentText := make([]string, 0, 2)
	for index, selected := range currentItems {
		if !selected {
			continue
		}
		if firstCurrent < 0 {
			firstCurrent = index
		}
		for _, segment := range b.envelope.Segments[segmentStarts[index]:segmentEnds[index]] {
			if segment.Origin == OriginCurrentUser {
				currentText = append(currentText, segment.Text)
			}
		}
	}
	if firstCurrent < 0 || !isContinuationOnly(strings.Join(currentText, "\n")) {
		return
	}
	previousUser := previousUserCandidate(items, firstCurrent)
	if previousUser < 0 {
		return
	}
	for index := segmentStarts[previousUser]; index < segmentEnds[previousUser]; index++ {
		if b.envelope.Segments[index].Origin == OriginHistory {
			b.envelope.Segments[index].Linked = true
		}
	}
}

type currentInputKind uint8

const (
	currentInputNone currentInputKind = iota
	currentInputExplicitMessage
	currentInputTextBlock
	currentInputImplicitMessage
	currentInputScalar
)

func inputItemRole(item gjson.Result) string {
	if !item.IsObject() {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(item.Get("role").String()))
}

func currentInputKindForItem(item gjson.Result) currentInputKind {
	if !item.IsObject() {
		if item.Type == gjson.String {
			return currentInputScalar
		}
		return currentInputNone
	}
	if role := inputItemRole(item); role != "" {
		if role == "user" {
			return currentInputExplicitMessage
		}
		return currentInputNone
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "input_text":
		return currentInputTextBlock
	case "", "message":
		return currentInputImplicitMessage
	default:
		return currentInputNone
	}
}

func selectCurrentInputItems(items []gjson.Result) []bool {
	selected := make([]bool, len(items))
	lastIndex := -1
	lastKind := currentInputNone
	for index, item := range items {
		if kind := currentInputKindForItem(item); kind != currentInputNone {
			lastIndex = index
			lastKind = kind
		}
	}
	if lastIndex < 0 {
		return selected
	}
	// A request that ends with assistant/tool interaction data is a tool
	// continuation, not a new direct user turn. Do not resurrect an older user
	// message as current evidence merely because it is the last user candidate
	// somewhere in the replayed conversation. Session metadata and agent replay
	// items intentionally do not close the direct prompt because Codex can append
	// those transport records after a newly entered user message.
	for index := lastIndex + 1; index < len(items); index++ {
		if inputItemClosesDirectPrompt(items[index]) {
			return selected
		}
	}
	selected[lastIndex] = true
	// Adjacent input_text items are content blocks of one direct Responses
	// prompt. Message objects and scalar forms are complete messages, so only
	// their final candidate is current.
	if lastKind == currentInputTextBlock {
		for index := lastIndex - 1; index >= 0; index-- {
			kind := currentInputKindForItem(items[index])
			if kind == currentInputTextBlock {
				selected[index] = true
				continue
			}
			if typedInputItemIsAttachment(items[index]) {
				continue
			}
			break
		}
	}
	return selected
}

func inputItemClosesDirectPrompt(item gjson.Result) bool {
	if !item.IsObject() {
		return false
	}
	switch inputItemRole(item) {
	case "assistant", "tool", "function":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "output_text", "refusal",
		"function_call_output", "tool_call_output", "local_shell_call_output", "shell_call_output",
		"apply_patch_call_output", "tool_search_output", "tool_search_call_output", "custom_tool_call_output",
		"mcp_tool_call_output", "computer_call_output", "mcp_call_output", "tool_result",
		"function_call", "tool_call", "local_shell_call", "shell_call", "apply_patch_call",
		"tool_search_call", "custom_tool_call", "mcp_tool_call", "mcp_call", "mcp_list_tools",
		"mcp_approval_request", "mcp_approval_response", "additional_tools", "code_interpreter_call",
		"computer_call", "file_search_call", "image_generation_call", "web_search_call", "tool_use":
		return true
	default:
		return false
	}
}

func previousUserCandidate(items []gjson.Result, before int) int {
	for index := before - 1; index >= 0; index-- {
		if currentInputKindForItem(items[index]) != currentInputNone {
			return index
		}
	}
	return -1
}

func typedInputItemIsAttachment(item gjson.Result) bool {
	if !item.IsObject() || inputItemRole(item) != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "input_file", "input_image", "image_url", "file", "image", "attachment", "computer_screenshot":
		return true
	default:
		return false
	}
}

func (b *envelopeBuilder) appendMessage(message gjson.Result, role string, currentUser bool) {
	origin := OriginHistory
	switch role {
	case "user":
		if currentUser {
			origin = OriginCurrentUser
		}
	case "system":
		origin = OriginSystem
	case "developer":
		origin = OriginDeveloper
	case "tool", "function":
		origin = OriginToolOutput
	}
	if role == "tool" || role == "function" {
		b.appendResult(OriginToolOutput, role, firstExistingResult(message, "content", "output", "text"))
	} else {
		b.appendResult(origin, role, firstExistingResult(message, "content", "text"))
	}
	b.appendToolArguments(message, role)
	b.appendAttachmentReferences(message, role)
}

func (b *envelopeBuilder) appendTypedInputItem(item gjson.Result, currentUser bool) {
	if !item.Exists() || item.Type == gjson.Null {
		return
	}
	if !item.IsObject() {
		origin := OriginHistory
		if currentUser {
			origin = OriginCurrentUser
		}
		b.appendResult(origin, "user", item)
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	switch itemType {
	case "function_call_output", "tool_call_output", "local_shell_call_output", "shell_call_output",
		"apply_patch_call_output", "tool_search_output", "tool_search_call_output", "custom_tool_call_output",
		"mcp_tool_call_output", "computer_call_output", "mcp_call_output", "tool_result":
		b.appendResult(OriginToolOutput, "tool", firstExistingResult(item, "output", "content", "text"))
	case "function_call", "tool_call", "local_shell_call", "shell_call", "apply_patch_call",
		"tool_search_call", "custom_tool_call", "mcp_tool_call", "mcp_call", "mcp_list_tools",
		"mcp_approval_request", "mcp_approval_response", "additional_tools", "code_interpreter_call",
		"computer_call", "file_search_call", "image_generation_call", "web_search_call", "tool_use":
		b.appendToolArguments(item, "assistant")
	case "agent_message":
		// These are assistant/agent replay items from an existing Codex session.
		// Treating their content as the current user prompt makes fixed transport
		// labels such as "Payload:" look like user intent and creates false hits.
		b.appendAgentMessage(item)
	case "output_text", "refusal":
		b.appendResult(OriginHistory, "assistant", firstExistingResult(item, "content", "text", "output"))
	case "reasoning":
		b.appendResult(OriginSessionContext, "assistant", firstExistingResult(item, "summary", "content", "text"))
	case "summary_text", "compaction", "context_compaction":
		b.appendResult(OriginSessionContext, "context", firstExistingResult(item, "summary", "content", "text"))
	case "compaction_trigger", "item_reference":
		// Control/reference items carry no end-user prompt text.
		b.appendAttachmentReferences(item, "context")
	case "input_text", "message":
		origin := OriginHistory
		if currentUser {
			origin = OriginCurrentUser
		}
		b.appendResult(origin, "user", firstExistingResult(item, "content", "text"))
	case "input_file", "input_image", "image_url", "file", "image", "attachment", "computer_screenshot":
		b.appendAttachmentReferences(item, "user")
	default:
		// An untyped object is the legacy direct-input shape. Unknown typed
		// objects are session context so future replay item types cannot silently
		// become current-user evidence or qualify for strikes.
		if itemType == "" {
			origin := OriginHistory
			if currentUser {
				origin = OriginCurrentUser
			}
			b.appendResult(origin, "user", item)
			return
		}
		b.appendResult(OriginSessionContext, "context", item)
	}
}

func (b *envelopeBuilder) appendAgentMessage(item gjson.Result) {
	start := len(b.envelope.Segments)
	b.appendResult(OriginHistory, "assistant", firstExistingResult(item, "content", "text", "output"))
	write := start
	for index := start; index < len(b.envelope.Segments); index++ {
		segment := b.envelope.Segments[index]
		segment.Text = stripAgentMessageEnvelope(segment.Text)
		if segment.Text == "" {
			continue
		}
		b.envelope.Segments[write] = segment
		write++
	}
	b.envelope.Segments = b.envelope.Segments[:write]
	b.appendAttachmentReferences(item, "assistant")
}

func stripAgentMessageEnvelope(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	hasCollaborationHeader := false
	payloadIndex := -1
	for index, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(lower, "message type:"), strings.HasPrefix(lower, "task name:"), strings.HasPrefix(lower, "sender:"), strings.HasPrefix(lower, "recipient:"):
			hasCollaborationHeader = true
		case strings.HasPrefix(lower, "payload:"):
			payloadIndex = index
		}
		if payloadIndex >= 0 {
			break
		}
	}
	if !hasCollaborationHeader || payloadIndex < 0 {
		return text
	}
	firstLine := strings.TrimSpace(lines[payloadIndex])
	firstPayload := strings.TrimSpace(firstLine[len("Payload:"):])
	parts := make([]string, 0, len(lines)-payloadIndex)
	if firstPayload != "" {
		parts = append(parts, firstPayload)
	}
	for _, line := range lines[payloadIndex+1:] {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func (b *envelopeBuilder) appendToolArguments(result gjson.Result, role string) {
	if !result.Exists() || !result.IsObject() {
		return
	}
	appendArgument := func(argument gjson.Result) {
		if !argument.Exists() || argument.Type == gjson.Null {
			return
		}
		if argument.Type == gjson.String {
			b.append(OriginToolArguments, role, argument.String())
			return
		}
		b.append(OriginToolArguments, role, argument.Raw)
	}
	appendArgument(result.Get("arguments"))
	appendArgument(result.Get("function.arguments"))
	appendArgument(result.Get("input"))
	appendArgument(result.Get("action"))
	appendArgument(result.Get("query"))
	if calls := result.Get("tool_calls"); calls.IsArray() {
		for _, call := range calls.Array() {
			appendArgument(call.Get("function.arguments"))
			appendArgument(call.Get("arguments"))
		}
	}
}

func (b *envelopeBuilder) appendAttachmentReferences(result gjson.Result, role string) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	if result.IsArray() {
		for _, item := range result.Array() {
			b.appendAttachmentReferences(item, role)
		}
		return
	}
	if !result.IsObject() {
		return
	}
	result.ForEach(func(key, value gjson.Result) bool {
		switch strings.ToLower(strings.TrimSpace(key.String())) {
		case "file_id", "image_url", "url", "attachment_id", "file":
			if value.Type == gjson.String {
				b.append(OriginAttachmentRefs, role, value.String())
			} else if value.IsObject() {
				b.appendAttachmentReferences(value, role)
			}
		case "attachments", "source":
			b.appendAttachmentReferences(value, role)
		}
		return true
	})
}

func firstExistingResult(result gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		value := result.Get(path)
		if value.Exists() && value.Type != gjson.Null {
			return value
		}
	}
	return gjson.Result{}
}
