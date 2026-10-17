// Package intelligencecheck contains the deterministic, transport-independent
// quality grader and action policy. It never sends requests or mutates accounts.
package intelligencecheck

import (
	"fmt"
	"golang.org/x/text/unicode/norm"
	"regexp"
	"strings"
)

type Question struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Prompt         string `json:"prompt"`
	ExpectedAnswer string `json:"expected_answer"`
	MatchMode      string `json:"match_mode"`
	MaxDurationMS  int64  `json:"max_duration_ms"`
	Enabled        bool   `json:"enabled"`
}
type Settings struct {
	Revision           int64      `json:"revision"`
	Enabled            bool       `json:"enabled"`
	GPTModel           string     `json:"gpt_model"`
	ClaudeModel        string     `json:"claude_model"`
	ReasoningEffort    string     `json:"reasoning_effort"`
	JudgeMode          string     `json:"judge_mode"`
	IntervalSeconds    int        `json:"interval_seconds"`
	RetrySeconds       int        `json:"retry_seconds"`
	TimeoutSeconds     int        `json:"timeout_seconds"`
	Concurrency        int        `json:"concurrency"`
	Retention          int        `json:"retention"`
	NewAccountsEnabled bool       `json:"new_accounts_enabled"`
	Action             string     `json:"action"`
	DegradeThreshold   int        `json:"degrade_threshold"`
	RecoverThreshold   int        `json:"recover_threshold"`
	TargetGroupID      int64      `json:"target_group_id"`
	Questions          []Question `json:"questions"`
}

// CandyPrompt is the built-in reasoning check. Keep the no-tools/no-network
// constraint: the question is public, so a browsing model could otherwise look
// up the published answer instead of reasoning through it.
const CandyPrompt = "不允许调用工具和联网。在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状，分别为圆形和五角星形，不同的形状靠手感可以分辨，现已知苹果味圆形有7颗、苹果味五角星形有7颗、桃子味圆形有9颗、桃子味五角星形有6颗、西瓜味圆形有8颗、西瓜味五角星形有4颗，参赛者需要在活动前决定摸出的糖果数目，那么最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖，同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求，最终只回复数字结果。"

func DefaultSettings() Settings {
	return Settings{Revision: 1, GPTModel: "gpt-6-astra", ClaudeModel: "claude-sonnet-4-6", ReasoningEffort: "high", JudgeMode: "answer", IntervalSeconds: 300, RetrySeconds: 60, TimeoutSeconds: 180, Concurrency: 4, Retention: 200, NewAccountsEnabled: true, Action: "none", DegradeThreshold: 2, RecoverThreshold: 1, Questions: []Question{{ID: "candy", Name: "糖果配对", Prompt: CandyPrompt, ExpectedAnswer: "21", MatchMode: "exact", MaxDurationMS: 20000, Enabled: true}}}
}
func ValidateSettings(s Settings) error {
	if strings.TrimSpace(s.GPTModel) == "" || strings.TrimSpace(s.ClaudeModel) == "" {
		return fmt.Errorf("both channel models are required")
	}
	if len(s.GPTModel) > 255 || len(s.ClaudeModel) > 255 {
		return fmt.Errorf("model too long")
	}
	switch s.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("invalid reasoning effort")
	}
	switch s.JudgeMode {
	case "answer", "latency", "both":
	default:
		return fmt.Errorf("invalid judge mode")
	}
	switch s.Action {
	case "none", "pause":
	case "switch_group":
		if s.TargetGroupID <= 0 {
			return fmt.Errorf("target group is required")
		}
	default:
		return fmt.Errorf("invalid action")
	}
	if s.IntervalSeconds < 1 || s.IntervalSeconds > 86400 || s.RetrySeconds < 1 || s.RetrySeconds > 86400 || s.TimeoutSeconds < 1 || s.TimeoutSeconds > 3600 || s.Concurrency < 1 || s.Concurrency > 100 || s.Retention < 1 || s.Retention > 10000 || s.DegradeThreshold < 1 || s.DegradeThreshold > 1000 || s.RecoverThreshold < 1 || s.RecoverThreshold > 1000 {
		return fmt.Errorf("numeric setting outside permitted range")
	}
	if len(s.Questions) > 100 {
		return fmt.Errorf("too many questions")
	}
	seen := map[string]bool{}
	enabled := 0
	for _, q := range s.Questions {
		if q.ID == "" || len(q.ID) > 100 || seen[q.ID] {
			return fmt.Errorf("question ID must be unique and nonempty")
		}
		seen[q.ID] = true
		if strings.TrimSpace(q.Name) == "" || strings.TrimSpace(q.Prompt) == "" || len(q.Name) > 255 || len(q.Prompt) > 100000 || len(q.ExpectedAnswer) > 100000 || q.MaxDurationMS <= 0 {
			return fmt.Errorf("invalid question %s", q.ID)
		}
		switch q.MatchMode {
		case "exact", "contains":
			if normalize(q.ExpectedAnswer) == "" {
				return fmt.Errorf("empty expected answer")
			}
		case "regex":
			if _, err := regexp.Compile(q.ExpectedAnswer); err != nil {
				return fmt.Errorf("question %s: %w", q.ID, err)
			}
		default:
			return fmt.Errorf("invalid match mode")
		}
		if q.Enabled {
			enabled++
		}
	}
	if s.Enabled && enabled == 0 {
		return fmt.Errorf("automatic checks need an enabled question")
	}
	return nil
}

type Observation struct {
	Output           string            `json:"output"`
	DurationMS       int64             `json:"duration_ms"`
	Complete         bool              `json:"complete"`
	Error            string            `json:"error"`
	Interrupted      bool              `json:"interrupted"`
	InputTokens      int64             `json:"input_tokens"`
	OutputTokens     int64             `json:"output_tokens"`
	ReasoningTokens  int64             `json:"reasoning_tokens"`
	ResponseModel    string            `json:"response_model"`
	EffectiveContext map[string]string `json:"effective_context,omitempty"`
}
type Result struct {
	Status        string `json:"status"`
	AnswerPassed  bool   `json:"answer_passed"`
	LatencyPassed bool   `json:"latency_passed"`
	Error         string `json:"error"`
}

func normalize(s string) string {
	s = strings.TrimSpace(norm.NFKC.String(s))
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") {
		first := strings.IndexByte(s, '\n')
		if first >= 3 {
			inner := s[first+1 : len(s)-3]
			if !strings.Contains(inner, "```") {
				s = strings.TrimSpace(inner)
			}
		}
	}
	return s
}
func Grade(q Question, mode string, o Observation) Result {
	r := Result{Status: "error", Error: o.Error}
	if o.Interrupted {
		r.Status = "interrupted"
		return r
	}
	if o.Error != "" || !o.Complete {
		if r.Error == "" {
			r.Error = "response did not reach a successful terminal event"
		}
		return r
	}
	out, expected := normalize(o.Output), normalize(q.ExpectedAnswer)
	switch q.MatchMode {
	case "exact":
		r.AnswerPassed = out == expected
	case "contains":
		r.AnswerPassed = strings.Contains(out, expected)
	case "regex":
		rx, err := regexp.Compile(q.ExpectedAnswer)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.AnswerPassed = rx.MatchString(out)
	default:
		r.Error = "invalid match mode"
		return r
	}
	r.LatencyPassed = o.DurationMS >= 0 && o.DurationMS < q.MaxDurationMS
	pass := false
	switch mode {
	case "answer":
		pass = r.AnswerPassed
	case "latency":
		pass = r.LatencyPassed
	case "both":
		pass = r.AnswerPassed && r.LatencyPassed
	default:
		r.Error = "invalid judge mode"
		return r
	}
	r.Status = "degraded"
	if pass {
		r.Status = "normal"
	}
	return r
}

type State struct {
	OriginalGroupChannels map[int64]string `json:"original_group_channels,omitempty"`
	NextQuestionID        string           `json:"next_question_id,omitempty"`
	LastQuestionID        string           `json:"last_question_id,omitempty"`
	DegradeStreak         int              `json:"failed_streak"`
	RecoverStreak         int              `json:"passed_streak"`
	OwnedAction           string           `json:"action"`
	OriginalGroupIDs      []int64          `json:"original_group_ids,omitempty"`
}

// Transition treats errors and interruptions as neutral observations. Ownership
// blocks stacking, and only the owning policy may release its pause.
func Transition(c Settings, s State, status string) (State, string) {
	switch status {
	case "normal":
		s.RecoverStreak++
		s.DegradeStreak = 0
	case "degraded":
		s.DegradeStreak++
		s.RecoverStreak = 0
	default:
		return s, ""
	}
	if s.OwnedAction != "" {
		if s.RecoverStreak >= c.RecoverThreshold && status == "normal" {
			if s.OwnedAction == "switch_group" {
				s.OwnedAction = ""
				return s, "restore_groups"
			}
			if s.OwnedAction == "pause" && c.Action == "pause" {
				s.OwnedAction = ""
				return s, "resume"
			}
		}
		return s, ""
	}
	if status == "degraded" && s.DegradeStreak >= c.DegradeThreshold && c.Action != "none" {
		s.OwnedAction = c.Action
		return s, c.Action
	}
	return s, ""
}
