package promptfilter

import (
	"strings"

	"github.com/tidwall/gjson"
)

const approvalReviewerInstruction = "You are judging one planned coding-agent action.\nAssess the exact action's intrinsic risk and whether the transcript authorizes its target and side effects."

// closedApprovalReassessmentSurface keeps the application-prompt downgrade
// limited to a non-executable auto-review envelope. Related Guardian requests
// are authorized before prompt filtering; this check protects direct callers
// that can request the public auto-review alias themselves.
func closedApprovalReassessmentSurface(body []byte, requestedModel string) (string, bool) {
	if !strings.EqualFold(strings.TrimSpace(requestedModel), "codex-auto-review") || !gjson.ValidBytes(body) {
		return "", false
	}
	root := gjson.ParseBytes(body)
	for _, path := range []string{"tools", "tool_choice", "previous_response_id", "messages", "prompt", "conversation", "context_management"} {
		if jsonValuePresent(root.Get(path)) {
			return "", false
		}
	}
	if instruction := root.Get("instructions"); jsonValuePresent(instruction) &&
		strings.TrimSpace(strings.ReplaceAll(instruction.String(), "\r\n", "\n")) != approvalReviewerInstruction {
		return "", false
	}
	input := root.Get("input")
	if !input.IsArray() {
		return "", false
	}
	items := input.Array()
	if len(items) != 1 || !items[0].IsObject() ||
		!strings.EqualFold(strings.TrimSpace(items[0].Get("role").String()), "user") ||
		!jsonKeysAllowed(items[0], "role", "type", "content") {
		return "", false
	}
	if kind := strings.TrimSpace(items[0].Get("type").String()); kind != "" && !strings.EqualFold(kind, "message") {
		return "", false
	}
	content := items[0].Get("content")
	if !content.IsArray() || len(content.Array()) == 0 {
		return "", false
	}
	var text strings.Builder
	for _, part := range content.Array() {
		if !part.IsObject() || !strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "input_text") ||
			!jsonKeysAllowed(part, "type", "text") || part.Get("text").Type != gjson.String {
			return "", false
		}
		text.WriteString(part.Get("text").String())
	}
	return text.String(), strings.TrimSpace(text.String()) != ""
}

func collapseApprovalCurrentUser(envelope *RequestEnvelope, text string) {
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
	if inserted {
		envelope.Segments = segments
		envelope.currentUserExactText = text
		envelope.CurrentUserTruncated = false
		envelope.currentUserPrecheck = nil
		envelope.precheckIncomplete = false
	}
}

func jsonValuePresent(value gjson.Result) bool {
	if !value.Exists() || value.Type == gjson.Null {
		return false
	}
	return !((value.Type == gjson.String && strings.TrimSpace(value.String()) == "") ||
		(value.IsArray() && len(value.Array()) == 0) || (value.IsObject() && len(value.Map()) == 0))
}

func jsonKeysAllowed(value gjson.Result, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range value.Map() {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return value.IsObject()
}
