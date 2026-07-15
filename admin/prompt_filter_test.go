package admin

import (
	"testing"

	"github.com/codex2api/security/promptfilter"
)

func TestShouldReviewPromptFilterVerdictSkipsEveryTerminalVerdict(t *testing.T) {
	cfg := promptfilter.DefaultConfig()
	cfg.StrictTerminalEnabled = false
	cfg.Review.Enabled = true
	cfg.Review.APIKey = "test-review-key"

	terminal := promptfilter.Verdict{Action: promptfilter.ActionBlock, TerminalStrictHit: true}
	if shouldReviewPromptFilterVerdict(terminal, cfg) {
		t.Fatal("terminal verdict was sent to secondary review while strict terminal enforcement was disabled")
	}

	nonTerminal := promptfilter.Verdict{Action: promptfilter.ActionWarn}
	if !shouldReviewPromptFilterVerdict(nonTerminal, cfg) {
		t.Fatal("eligible non-terminal verdict did not enter secondary review")
	}
}
