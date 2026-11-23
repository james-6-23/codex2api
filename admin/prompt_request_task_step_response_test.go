package admin

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/codex2api/database"
)

func TestPromptRequestTaskStepShowsOwnRecordedResponse(t *testing.T) {
	detail := promptTaskDetail{Steps: make([]promptTaskStep, 3)}
	projection := promptTaskProjection{detail: &detail}
	projection.observeResponse(0, &database.PromptRequestResponse{Status: "completed", Text: "我会先检查。"})
	projection.observeResponse(1, &database.PromptRequestResponse{Status: "interrupted", Error: "stream interrupted"})
	if response := detail.Steps[0].Response; response == nil || response.Status != "completed" || response.Text != "我会先检查。" {
		t.Fatal("first model step lost its recorded response")
	}
	if response := detail.Steps[1].Response; response == nil || response.Status != "interrupted" || response.Error != "stream interrupted" {
		t.Fatal("interrupted model step lost its own response")
	}
	if detail.Steps[2].Response != nil || detail.Final != nil {
		t.Fatal("a missing response or a final task answer was inferred")
	}
}

func TestPromptRequestTaskStepBoundsUTF8Response(t *testing.T) {
	text := strings.Repeat("好", promptTaskStepResponseTextLimit)
	reply := &database.PromptRequestResponse{Status: "completed", FinalObserved: true, FinalText: text}
	got := previewPromptTaskStepResponse(reply)
	if !got.Truncated || len(got.Text) > promptTaskStepResponseTextLimit || !utf8.ValidString(got.Text) ||
		!strings.HasSuffix(got.Text, "…") || reply.FinalText != text {
		t.Fatal("step response was not safely bounded independently of its original")
	}
}
