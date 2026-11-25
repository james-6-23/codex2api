package admin

import (
	"strings"
	"unicode/utf8"

	"github.com/codex2api/database"
)

const promptTaskStepResponseTextLimit = 8 << 10

type promptTaskStepResponse struct {
	Status    string `json:"status"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	ToolOnly  bool   `json:"tool_only,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func previewPromptTaskStepResponse(reply *database.PromptRequestResponse) *promptTaskStepResponse {
	text := reply.Text
	if text == "" && reply.FinalObserved {
		text = reply.FinalText
	}
	preview, textTruncated := boundPromptTaskStepResponseText(text)
	message, errorTruncated := boundPromptTaskStepResponseText(reply.Error)
	return &promptTaskStepResponse{
		Status: reply.Status, Text: preview, Error: message,
		Truncated: reply.Truncated || textTruncated || errorTruncated,
	}
}

func boundPromptTaskStepResponseText(text string) (string, bool) {
	if len(text) <= promptTaskStepResponseTextLimit {
		return text, false
	}
	limit := promptTaskStepResponseTextLimit - len("…")
	for !utf8.RuneStart(text[limit]) {
		limit--
	}
	return strings.Clone(text[:limit]) + "…", true
}
