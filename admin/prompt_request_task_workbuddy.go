package admin

import (
	"strings"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

const workBuddyReminder = `<system-reminder data-role="`

// WorkBuddy can wrap each part of one Chat message with its own reminders.
// Unwrap the original parts for display after identifying the task.
func promptTaskReplayUserText(endpoint string, body []byte, input database.PromptRequestTaskInput) string {
	if input.Field != "messages" || safeTraceEndpoint(endpoint) != "chat" || !strings.Contains(input.Text, workBuddyReminder) {
		return input.Text
	}
	instructions := gjson.GetBytes(body, "messages.0")
	if (instructions.Get("role").Str != "developer" && instructions.Get("role").Str != "system") ||
		!strings.Contains(strings.ToLower(promptContextText(instructions, 0)), "workbuddy") {
		return input.Text
	}
	parts := input.ChatTextParts(body)
	for i, part := range parts {
		parts[i] = unwrapWorkBuddyQueryPart(part)
	}
	return strings.Join(parts, "\n")
}

func unwrapWorkBuddyQueryPart(part string) string {
	text := strings.TrimSpace(part)
	if !strings.HasPrefix(text, workBuddyReminder) {
		return part
	}
	for strings.HasPrefix(text, workBuddyReminder) {
		role, content, ok := strings.Cut(text[len(workBuddyReminder):], `">`)
		if !ok || (role != "user-context" && role != "additional-data") {
			return part
		}
		_, remaining, ok := strings.Cut(content, "</system-reminder>")
		if !ok {
			return part
		}
		text = strings.TrimSpace(remaining)
	}
	query, ok := strings.CutPrefix(text, "<user_query>")
	if !ok {
		return part
	}
	query, ok = strings.CutSuffix(query, "</user_query>")
	if !ok || strings.TrimSpace(query) == "" {
		return part
	}
	return strings.TrimSpace(query)
}

// A successful receipt from the named WorkBuddy Skill tool contains a base
// directory followed by the Skill body. The caller has already paired the
// receipt with the tool call by ID.
func promptTaskWorkBuddySkill(endpoint string, tool *promptTaskTool, result promptRequestToolResult) (promptRequestContextSection, bool) {
	if safeTraceEndpoint(endpoint) != "chat" || tool.Name != "Skill" || tool.CallID == "" ||
		result.failed || result.FromRequestID <= 0 || !gjson.Valid(tool.Arguments) {
		return promptRequestContextSection{}, false
	}
	name := gjson.Get(tool.Arguments, "skill").Str
	header, body, ok := strings.Cut(result.Text, "\n")
	directory, hasDirectory := strings.CutPrefix(header, "Base directory for this skill: ")
	directory = strings.TrimRight(strings.TrimSpace(directory), "/\\")
	normalized := strings.ReplaceAll(directory, "\\", "/")
	if !ok || !hasDirectory || name == "" || name != promptContextFileName(directory) ||
		!strings.Contains(strings.ToLower(normalized), "/.workbuddy/") {
		return promptRequestContextSection{}, false
	}
	markdown, evidence := promptContextSingleSkillReceipt(body)
	if evidence != promptContextReadSuccess {
		return promptRequestContextSection{}, false
	}
	separator := "/"
	if strings.LastIndex(directory, "\\") > strings.LastIndex(directory, "/") {
		separator = "\\"
	}
	return promptRequestContextSection{
		Kind: promptContextSkill, Name: name, Text: markdown, Path: directory + separator + "SKILL.md",
		Source: "tool_result", State: promptContextReceived, Evidence: evidence, SourceRequestID: result.FromRequestID,
	}, true
}
