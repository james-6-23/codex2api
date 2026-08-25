package promptfilter

import (
	"strings"

	"github.com/tidwall/gjson"
)

// ClosedProjectTitleCandidate recognizes the constrained Responses request
// emitted by Codex for project naming. The prompt wrapper is public, so it is
// accepted only together with a single user input, no continuation/tool
// surface, and an output schema that can return only title/description text.
// This helper is shared by routing and GuardPipeline classification.
func ClosedProjectTitleCandidate(body []byte, requestedModel string) (string, bool) {
	if !gjson.ValidBytes(body) || !strings.EqualFold(strings.TrimSpace(requestedModel), "gpt-5.6-luna") {
		return "", false
	}
	root := gjson.ParseBytes(body)
	if hasProjectTitleExecutionSurface(root) {
		return "", false
	}
	inputText, ok := closedProjectTitleInputText(root.Get("input"))
	if !ok {
		return "", false
	}
	candidate, ok := closedProjectTitlePromptCandidate(inputText)
	if !ok || !closedProjectTitleSchema(root.Get("text.format.schema")) {
		return "", false
	}
	return candidate, true
}

// Codex desktop releases have emitted project-title input in both legal
// Responses forms: a direct string and one closed user/input_text message.
// Accepting both keeps direct Codex2API and NewAPI relay paths equivalent while
// still rejecting extra messages, content parts, roles, or object fields.
func closedProjectTitleInputText(input gjson.Result) (string, bool) {
	if input.Type == gjson.String {
		text := input.String()
		return text, strings.TrimSpace(text) != ""
	}
	if !input.IsArray() {
		return "", false
	}
	items := input.Array()
	if len(items) != 1 || !items[0].IsObject() {
		return "", false
	}
	message := items[0]
	if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "user") {
		return "", false
	}
	if kind := strings.TrimSpace(message.Get("type").String()); kind != "" && !strings.EqualFold(kind, "message") {
		return "", false
	}
	if !jsonObjectKeysAllowed(message, "role", "type", "content") {
		return "", false
	}
	content := message.Get("content")
	if !content.IsArray() {
		return "", false
	}
	parts := content.Array()
	if len(parts) != 1 || !parts[0].IsObject() ||
		!strings.EqualFold(strings.TrimSpace(parts[0].Get("type").String()), "input_text") ||
		!jsonObjectKeysAllowed(parts[0], "type", "text") || parts[0].Get("text").Type != gjson.String {
		return "", false
	}
	text := parts[0].Get("text").String()
	return text, strings.TrimSpace(text) != ""
}

// ClosedApprovalReassessmentText validates the non-text execution surface of
// a Codex Guardian/auto-review request. The approval parser separately checks
// the complete static prompt and planned-action structure; this layer prevents
// that public prompt from authorizing extra instructions, tools, continuation
// state, history, or parallel user inputs.
func ClosedApprovalReassessmentText(body []byte) (string, bool) {
	if !gjson.ValidBytes(body) {
		return "", false
	}
	root := gjson.ParseBytes(body)
	for _, path := range []string{"tools", "tool_choice", "previous_response_id", "messages", "prompt", "conversation", "context_management"} {
		if closedJSONValuePresent(root.Get(path)) {
			return "", false
		}
	}
	if instruction := root.Get("instructions"); closedJSONValuePresent(instruction) && !closedGuardianInstruction(instruction.String()) {
		return "", false
	}
	input := root.Get("input")
	if !input.IsArray() {
		return "", false
	}
	items := input.Array()
	if len(items) != 1 || !items[0].IsObject() ||
		!strings.EqualFold(strings.TrimSpace(items[0].Get("role").String()), "user") ||
		!jsonObjectKeysAllowed(items[0], "role", "type", "content") {
		return "", false
	}
	if kind := strings.TrimSpace(items[0].Get("type").String()); kind != "" && !strings.EqualFold(kind, "message") {
		return "", false
	}
	content := items[0].Get("content")
	if !content.IsArray() {
		return "", false
	}
	parts := content.Array()
	if len(parts) == 0 {
		return "", false
	}
	var text strings.Builder
	for _, part := range parts {
		if !part.IsObject() || !strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "input_text") ||
			!jsonObjectKeysAllowed(part, "type", "text") || part.Get("text").Type != gjson.String {
			return "", false
		}
		text.WriteString(part.Get("text").String())
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", false
	}
	return text.String(), true
}

func approvalReassessmentRequestedModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "codex-auto-review" || model == "gpt-5.6-luna"
}

func collapseClosedApprovalCurrentUser(envelope *RequestEnvelope, text string) {
	if envelope == nil {
		return
	}
	segments := make([]Segment, 0, len(envelope.Segments))
	inserted := false
	for _, segment := range envelope.Segments {
		if segment.Origin != OriginCurrentUser {
			segments = append(segments, segment)
			continue
		}
		if inserted {
			continue
		}
		segment.Text = text
		segment.Truncated = false
		segments = append(segments, segment)
		inserted = true
	}
	if !inserted {
		return
	}
	envelope.Segments = segments
	envelope.currentUserExactText = text
	envelope.CurrentUserTruncated = false
	envelope.currentUserPrecheck = nil
	envelope.precheckIncomplete = false
}

func closedGuardianInstruction(value string) bool {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
	return value == "You are judging one planned coding-agent action.\nAssess the exact action's intrinsic risk and whether the transcript authorizes its target and side effects."
}

func closedJSONValuePresent(value gjson.Result) bool {
	if !value.Exists() || value.Type == gjson.Null {
		return false
	}
	switch {
	case value.Type == gjson.String && strings.TrimSpace(value.String()) == "":
		return false
	case value.IsArray() && len(value.Array()) == 0:
		return false
	case value.IsObject() && len(value.Map()) == 0:
		return false
	default:
		return true
	}
}

func hasProjectTitleExecutionSurface(root gjson.Result) bool {
	for _, path := range []string{"instructions", "tools", "tool_choice", "previous_response_id", "messages", "prompt", "conversation", "context_management"} {
		value := root.Get(path)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		switch {
		case value.Type == gjson.String && strings.TrimSpace(value.String()) == "":
			continue
		case value.IsArray() && len(value.Array()) == 0:
			continue
		case value.IsObject() && len(value.Map()) == 0:
			continue
		default:
			return true
		}
	}
	return false
}

func closedProjectTitlePromptCandidate(text string) (string, bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.Count(text, projectTitleUserPromptMarker) != 1 {
		return "", false
	}
	index := strings.Index(text, projectTitleUserPromptMarker)
	if index <= 0 {
		return "", false
	}
	prefix := strings.ToLower(strings.TrimSpace(text[:index]))
	if !strings.Contains(prefix, "presented with a user prompt") ||
		!strings.Contains(prefix, "provide a short title") {
		return "", false
	}
	candidate := strings.TrimSpace(text[index+len(projectTitleUserPromptMarker):])
	return candidate, candidate != ""
}

func closedProjectTitleSchema(schema gjson.Result) bool {
	if !schema.IsObject() || !strings.EqualFold(strings.TrimSpace(schema.Get("type").String()), "object") {
		return false
	}
	properties := schema.Get("properties")
	if !properties.IsObject() || len(properties.Map()) != 2 {
		return false
	}
	for _, name := range []string{"title", "description"} {
		field := properties.Get(name)
		if !field.IsObject() || !strings.EqualFold(strings.TrimSpace(field.Get("type").String()), "string") {
			return false
		}
	}
	if required := schema.Get("required"); required.Exists() && !jsonStringSetEquals(required, "title", "description") {
		return false
	}
	if additional := schema.Get("additionalProperties"); additional.Exists() && additional.Type != gjson.Null && additional.Bool() {
		return false
	}
	return true
}

func jsonObjectKeysAllowed(value gjson.Result, allowed ...string) bool {
	if !value.IsObject() {
		return false
	}
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range value.Map() {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}

func jsonStringSetEquals(value gjson.Result, expected ...string) bool {
	if !value.IsArray() {
		return false
	}
	items := value.Array()
	if len(items) != len(expected) {
		return false
	}
	want := make(map[string]struct{}, len(expected))
	for _, item := range expected {
		want[item] = struct{}{}
	}
	for _, item := range items {
		if item.Type != gjson.String {
			return false
		}
		key := strings.TrimSpace(item.String())
		if _, ok := want[key]; !ok {
			return false
		}
		delete(want, key)
	}
	return len(want) == 0
}
