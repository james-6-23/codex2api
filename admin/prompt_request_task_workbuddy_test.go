package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

func TestPromptTaskWorkBuddyUserReplay(t *testing.T) {
	const question = "请核对状态\n并说明下一步。"
	const framed = `<system-reminder data-role="user-context">
<craft_mode>Agent mode</craft_mode>
</system-reminder>
<system-reminder data-role="additional-data">
<current_time>Now</current_time>
</system-reminder>
<user_query>` + question + `</user_query>`

	for _, tc := range []struct {
		name, endpoint, brand, text, want string
	}{
		{"workbuddy query", "/v1/chat/completions", "WorkBuddy", framed, question},
		{"codex chat", "/v1/chat/completions", "Codex", framed, framed},
		{"native responses", "/v1/responses", "WorkBuddy", framed, framed},
		{"literal tags", "/v1/chat/completions", "WorkBuddy", "Explain these tags:\n" + framed, "Explain these tags:\n" + framed},
		{"trailing user text", "/v1/chat/completions", "WorkBuddy", framed + "\nAdditional text", framed + "\nAdditional text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"messages": []map[string]any{
				{"role": "developer", "content": tc.brand},
				{"role": "user", "content": []map[string]string{{"type": "text", "text": tc.text}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			input := database.DecodePromptRequestTaskUser(body)
			request := database.PromptRequestBody{
				PromptRequestRecord: database.PromptRequestRecord{ID: 1, Endpoint: tc.endpoint}, Body: body,
			}
			if got := promptTaskUserFromRequest(request, input).Text; got != tc.want || input.Text != tc.text {
				t.Fatalf("display text=%q, decoded source=%q", got, input.Text)
			}
		})
	}
}

func TestPromptTaskWorkBuddyReplayKeepsOriginalTextPartBoundaries(t *testing.T) {
	wrap := func(query string) string {
		return `<system-reminder data-role="user-context"><craft_mode>Agent mode</craft_mode></system-reminder>` +
			`<system-reminder data-role="additional-data"><current_time>Now</current_time></system-reminder>` +
			"<user_query>" + query + "</user_query>"
	}
	textPart := func(text string) map[string]any {
		return map[string]any{"type": "text", "text": text}
	}
	const question = "换一套配色"
	const followup = "请继续执行任务"
	const plain = "保留这一行 <user_query> 原样。"
	const malformed = `<system-reminder data-role="unrecognized">Literal example</system-reminder><user_query>Keep this</user_query>`
	for _, tc := range []struct {
		name, brand, endpoint string
		content               any
		want                  string
		hasAttachment         bool
	}{
		{"several wrapped parts", "WorkBuddy", "/v1/chat/completions",
			[]any{textPart(wrap(question)), textPart(wrap(followup)), textPart(wrap(followup)),
				textPart(wrap(followup)), textPart(wrap(followup))},
			strings.Join([]string{question, followup, followup, followup, followup}, "\n"), false},
		{"wrapped text with unwrapped text and an image", "WorkBuddy", "/chat/completions",
			[]any{textPart(plain), map[string]any{"type": "image_url", "image_url": map[string]string{"url": "https://example.invalid/image.png"}},
				textPart(wrap(question)), textPart(malformed)}, strings.Join([]string{plain, question, malformed}, "\n"), true},
		{"plain string envelope", "WorkBuddy", "/v1/chat/completions", wrap(question), question, false},
		{"native client", "Codex", "/v1/chat/completions",
			[]any{textPart(wrap(question)), textPart(wrap(followup))}, wrap(question) + "\n" + wrap(followup), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"messages": []map[string]any{
				{"role": "developer", "content": tc.brand},
				{"role": "user", "content": wrap("Earlier request")},
				{"role": "assistant", "content": "Earlier answer"},
				{"role": "user", "content": tc.content},
			}})
			if err != nil {
				t.Fatal(err)
			}
			input := database.DecodePromptRequestTaskUser(body)
			got := promptTaskReplayUserText(tc.endpoint, body, input)
			if got != tc.want || input.HasAttachment != tc.hasAttachment {
				t.Fatalf("display=%q attachment=%t", got, input.HasAttachment)
			}
		})
	}
}

func TestPromptTaskWorkBuddySkillReceipt(t *testing.T) {
	const body = "# Sample Skill\n\nReview the active task."
	const directory = `C:\Users\test\.workbuddy\plugins\cache\skills\sample-skill`
	const header = "Base directory for this skill: "
	base := promptTaskTool{CallID: "call-1", Name: "Skill", Arguments: `{"skill":"sample-skill"}`}

	for _, tc := range []struct {
		name, endpoint, receipt string
		failed, loaded          bool
	}{
		{"workbuddy receipt", "/v1/chat/completions", header + directory + "\r\n" + body, false, true},
		{"native codex", "/v1/responses", header + directory + "\n" + body, false, false},
		{"failed receipt", "/v1/chat/completions", header + directory + "\n" + body, true, false},
		{"wrong skill", "/v1/chat/completions", header + directory + "-other\n" + body, false, false},
		{"outside workbuddy", "/v1/chat/completions", header + "/tmp/sample-skill\n" + body, false, false},
		{"only header", "/v1/chat/completions", header + directory + "\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projection := promptTaskProjection{
				detail:            &promptTaskDetail{Steps: []promptTaskStep{{RequestID: 12, Endpoint: tc.endpoint}}},
				skills:            make(map[string]bool),
				loadedDirectories: make(map[string]promptTaskSkillDirectory),
			}
			tool := base
			result := promptRequestToolResult{CallID: tool.CallID, Text: tc.receipt, FromRequestID: 13, failed: tc.failed}
			projection.setToolResult(0, &tool, result)
			skills := projection.detail.Skills
			stepSkills := projection.detail.Steps[0].Skills
			if tc.loaded {
				if len(skills) != 1 || len(stepSkills) != 1 || skills[0].Text != body ||
					skills[0].Name != "sample-skill" || skills[0].Path != directory+`\SKILL.md` ||
					skills[0].SourceRequestID != 13 || stepSkills[0].SourceRequestID != 13 {
					t.Fatalf("wrong attributed skills: %+v, %+v", skills, stepSkills)
				}
			} else if len(skills) != 0 || len(stepSkills) != 0 {
				t.Fatalf("unexpected attributed skills: %+v, %+v", skills, stepSkills)
			}
			if tool.Result != tc.receipt {
				t.Fatal("raw tool result was changed")
			}
		})
	}
}
