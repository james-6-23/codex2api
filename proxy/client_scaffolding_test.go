package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const clientScaffoldingSample = "<system-reminder>\n" +
	"# Environment\n" +
	"You have been invoked in the following environment:\n" +
	" - Primary working directory: C:\\Users\\frank\n" +
	" - Is a git repository: false\n" +
	" - Platform: win32\n" +
	" - Shell: bash\n" +
	"</system-reminder>"

// claudeCodeScaffoldedTurn builds one turn of a Claude Code conversation in the
// shape New-API forwards to codex2api: a Claude Code system prompt plus
// <system-reminder> scaffolding in front of the user's real message.
//
// gitRepo is deliberately a parameter: the environment block carries volatile
// session state (git status, working directory) that changes as the session
// moves, which is exactly what must not reach a cache key.
func claudeCodeScaffoldedTurn(gitRepo, question, history string) []byte {
	return []byte(`{
		"model":"gemini-3.7-flash-high",
		"instructions":"You are Claude Code, Anthropic's official CLI tool for Claude.",
		"input":[
			{"role":"user","content":[
				{"type":"input_text","text":"<system-reminder>\n# Environment\nYou have been invoked in the following environment:\n - Platform: win32\n - Is a git repository: ` + gitRepo + `\n</system-reminder>"},
				{"type":"input_text","text":"<system-reminder>\nYou are powered by the model gemini-3.8-flash-high.\n</system-reminder>"},
				{"type":"input_text","text":"` + question + `"}
			]}` + history + `
		]
	}`)
}

func TestNormalizeClientScaffoldingStripsHarnessScaffolding(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "environment reminder",
			in:   clientScaffoldingSample,
			want: "",
		},
		{
			name: "powered by reminder",
			in:   "<system-reminder>\nYou are powered by the model gemini-3.8-flash-high.\n</system-reminder>",
			want: "",
		},
		{
			name: "agent types reminder",
			in:   "<system-reminder>\nAvailable agent types for the Agent tool:\n- Explore: Read-only search agent\n</system-reminder>",
			want: "",
		},
		{
			name: "skills reminder",
			in:   "<system-reminder>\nThe following skills are available for use with the Skill tool:\n- init: Initialize a new CLAUDE.md file\n</system-reminder>",
			want: "",
		},
		{
			name: "attribution reminder",
			in:   "<system-reminder>\nAttribution for git commits and pull requests you create from here on:\n- End git commit messages with:\nCo-Authored-By: Claude Code <noreply@anthropic.com>\n</system-reminder>",
			want: "",
		},
		{
			name: "standalone attribution line",
			in:   "fix the parser\n\nCo-Authored-By: Claude Code <noreply@anthropic.com>\n",
			want: "fix the parser",
		},
		{
			name: "standalone generated-with line",
			in:   "fix the parser\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n",
			want: "fix the parser",
		},
		{
			name: "standalone powered by line",
			in:   "You are powered by the model gemini-3.8-flash-high.",
			want: "",
		},
		{
			name: "reminders followed by the real question",
			in:   clientScaffoldingSample + "\n\n仔细研究下 token 策略",
			want: "仔细研究下 token 策略",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeClientScaffolding(tc.in); got != tc.want {
				t.Fatalf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A question that merely quotes a reminder tag must survive: block removal is
// gated on a scaffolding marker, not on the tag alone.
func TestNormalizeClientScaffoldingKeepsReminderWithoutMarkers(t *testing.T) {
	in := "<system-reminder>remember to run the tests before pushing</system-reminder>"
	if got := normalizeClientScaffolding(in); got != in {
		t.Fatalf("marker-free reminder was modified: %q", got)
	}
}

func TestNormalizeClientScaffoldingIsDeterministicAndIdempotent(t *testing.T) {
	in := clientScaffoldingSample + "\n\n" + "<system-reminder>\nYou are powered by the model gemini-3.8-flash-high.\n</system-reminder>" + "\n\nkeep me"
	first := normalizeClientScaffolding(in)
	if second := normalizeClientScaffolding(in); second != first {
		t.Fatalf("not deterministic: %q vs %q", first, second)
	}
	if twice := normalizeClientScaffolding(first); twice != first {
		t.Fatalf("not idempotent: %q -> %q", first, twice)
	}
	if first != "keep me" {
		t.Fatalf("unexpected result: %q", first)
	}
}

func TestNormalizeClientScaffoldingRespectsEnvToggles(t *testing.T) {
	t.Run("master switch off leaves text untouched", func(t *testing.T) {
		t.Setenv(clientScaffoldingStripEnv, "off")
		if got := normalizeClientScaffolding(clientScaffoldingSample); got != clientScaffoldingSample {
			t.Fatalf("disabled neutralizer modified text: %q", got)
		}
	})
	t.Run("reminder switch off keeps blocks but drops attribution lines", func(t *testing.T) {
		t.Setenv(clientScaffoldingStripRemindersEnv, "false")
		in := clientScaffoldingSample + "\n\nCo-Authored-By: Claude Code <noreply@anthropic.com>\n"
		got := normalizeClientScaffolding(in)
		if !strings.Contains(got, "<system-reminder>") {
			t.Fatalf("reminder block should survive when only the reminder switch is off: %q", got)
		}
		if strings.Contains(got, "Co-Authored-By") {
			t.Fatalf("attribution line should still be dropped: %q", got)
		}
	})
}

// deriveContentSessionSeed normalizes content.Raw, which embeds base64 image
// bytes verbatim, so an image turn pushes megabytes of payload through this text
// pass. Four regexes across that payload cost hundreds of milliseconds per
// request — enough to make the image pipeline test miss its own 45s deadline
// under -race — so the pass is bounded.
func TestNormalizeClientScaffoldingSkipsOversizedPayloads(t *testing.T) {
	oversized := `[{"type":"input_text","text":"` + clientScaffoldingSample + `"},{"type":"input_image","image_url":"data:image/png;base64,` +
		strings.Repeat("A", clientScaffoldingMaxNormalizeBytes) + `"}]`
	if len(oversized) <= clientScaffoldingMaxNormalizeBytes {
		t.Fatalf("fixture is %d bytes, needs to exceed the %d cap", len(oversized), clientScaffoldingMaxNormalizeBytes)
	}
	if got := normalizeClientScaffolding(oversized); got != oversized {
		t.Fatal("text past the cap must be returned unchanged")
	}

	// Under the cap the pass still runs, so the bound is a size limit and not a
	// silent off switch.
	if got := normalizeClientScaffolding("keep me\n\n" + clientScaffoldingSample); got != "keep me" {
		t.Fatalf("scaffolding under the cap must still be stripped, got %q", got)
	}
}

func TestResponsesToGeminiInternalStripsClientScaffolding(t *testing.T) {
	body := `{
		"instructions":"You are Claude Code, Anthropic's official CLI tool for Claude.",
		"input":[{"role":"user","content":[
			{"type":"input_text","text":"<system-reminder>\n# Environment\nYou have been invoked in the following environment:\n - Platform: win32\n</system-reminder>"},
			{"type":"input_text","text":"<system-reminder>\nAvailable agent types for the Agent tool:\n- Explore: Read-only search agent\n</system-reminder>"},
			{"type":"input_text","text":"<system-reminder>\nYou are powered by the model gemini-3.8-flash-high.\n</system-reminder>"},
			{"type":"input_text","text":"仔细研究下 token 策略"}
		]}]
	}`
	got, err := responsesToGeminiInternal([]byte(body), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	request := got["request"].(map[string]any)

	contents, ok := request["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents = %#v", contents)
	}
	turn := contents[0].(map[string]any)
	if turn["role"] != "user" {
		t.Fatalf("turn role = %#v", turn["role"])
	}
	parts := turn["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("scaffolding parts survived: %#v", parts)
	}
	if text := parts[0].(map[string]any)["text"]; text != "仔细研究下 token 策略" {
		t.Fatalf("user text = %#v", text)
	}

	// The system prompt keeps its real instructions but its client-identity
	// preamble is rewritten to the official Antigravity identity.
	system := request["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(system, officialAntigravityIdentity) {
		t.Fatalf("systemInstruction = %q", system)
	}
}

// Structured payloads are data, not prose: they must round-trip byte for byte.
func TestResponsesToGeminiInternalKeepsStructuredPayloadsIntact(t *testing.T) {
	body := `{
		"input":[
			{"type":"function_call","name":"lookup","call_id":"c1","arguments":"{\"query\":\"<system-reminder># Environment</system-reminder>\"}"},
			{"type":"function_call_output","call_id":"c1","output":"<system-reminder>\n# Environment\n - Platform: win32\n</system-reminder>"},
			{"role":"user","content":"hi"}
		]
	}`
	got, err := responsesToGeminiInternal([]byte(body), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	contents := got["request"].(map[string]any)["contents"].([]any)

	callArgs, _ := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)["args"].(map[string]any)
	if query, _ := callArgs["query"].(string); !strings.Contains(query, "<system-reminder>") {
		t.Fatalf("function call arguments were rewritten: %#v", callArgs)
	}

	response, _ := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)["response"].(map[string]any)
	if result, _ := response["result"].(string); strings.Contains(result, "<system-reminder>") {
		t.Fatalf("function response scaffolding survived: %#v", response)
	}
}

// Stripping the volatile reminder blocks is what keeps the session id, and
// therefore the upstream prompt cache prefix, stable across turns.
func TestResponsesToGeminiInternalSessionSeedSurvivesVolatileReminders(t *testing.T) {
	body := func(platform string) []byte {
		return []byte(`{"input":[{"role":"user","content":[
			{"type":"input_text","text":"<system-reminder>\n# Environment\n - Platform: ` + platform + `\n - Is a git repository: false\n</system-reminder>"},
			{"type":"input_text","text":"what changed today?"}
		]}]}`)
	}
	first, err := responsesToGeminiInternal(body("win32"), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	second, err := responsesToGeminiInternal(body("darwin"), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := first["request"].(map[string]any)
	secondRequest := second["request"].(map[string]any)
	if firstRequest["sessionId"] != secondRequest["sessionId"] {
		t.Fatalf("session id drifted with volatile scaffolding: %v vs %v", firstRequest["sessionId"], secondRequest["sessionId"])
	}
	firstPrefix, _ := json.Marshal(firstRequest["contents"])
	secondPrefix, _ := json.Marshal(secondRequest["contents"])
	if string(firstPrefix) != string(secondPrefix) {
		t.Fatalf("cached prefix drifted: %s vs %s", firstPrefix, secondPrefix)
	}
}

func TestGeminiNativeToAntigravityEnvelopeStripsClientScaffolding(t *testing.T) {
	payload, _, _, err := geminiNativeToAntigravityEnvelope([]byte(`{
		"system_instruction":{"parts":[{"text":"You are powered by the model gemini-3.8-flash-high."}]},
		"contents":[{"role":"user","parts":[
			{"text":"<system-reminder>\n# Environment\n - Platform: win32\n</system-reminder>"},
			{"text":"hello"}
		]}]
	}`), "google-project", "models/gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	request := envelope["request"].(map[string]any)
	if _, ok := request["systemInstruction"]; ok {
		t.Fatalf("pure-scaffolding systemInstruction should be dropped: %#v", request["systemInstruction"])
	}
	parts := request["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("native contents shape must be preserved: %#v", parts)
	}
	if text := parts[0].(map[string]any)["text"]; text != "" {
		t.Fatalf("scaffolding part should be emptied in place: %#v", text)
	}
	if text := parts[1].(map[string]any)["text"]; text != "hello" {
		t.Fatalf("real content was damaged: %#v", text)
	}
}

// The strip has to hold on the real ingress, not only inside the converter:
// /v1/chat/completions and /v1/messages both funnel into the Antigravity
// envelope, and the scaffolding must be gone by the time it leaves the process.
func TestAntigravityIngressStripsClientScaffolding(t *testing.T) {
	const upstreamBody = `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`

	cases := []struct {
		name     string
		path     string
		dispatch func(*Handler, *gin.Context)
		body     string
		question string
	}{
		{
			name:     "chat completions",
			path:     "/v1/chat/completions",
			dispatch: (*Handler).ChatCompletions,
			body: `{"model":"` + antigravityTransportTestModel + `","messages":[
				{"role":"user","content":"<system-reminder>\n# Environment\nYou have been invoked in the following environment:\n - Platform: win32\n</system-reminder>\n\nCo-Authored-By: Claude Code <noreply@anthropic.com>\n\n仔细研究下 token 策略"}
			]}`,
			question: "仔细研究下 token 策略",
		},
		{
			name:     "anthropic messages",
			path:     "/v1/messages",
			dispatch: (*Handler).Messages,
			body: `{"model":"` + antigravityTransportTestModel + `","max_tokens":256,"messages":[
				{"role":"user","content":"<system-reminder>\nAvailable agent types for the Agent tool:\n- Explore: Read-only search agent\n</system-reminder>\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nhello there"}
			]}`,
			question: "hello there",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, upstream := newAntigravityTransportTestHandler(t, upstreamBody)
			recorder := invokeAntigravityTransport(t, handler, tc.path, tc.body, tc.dispatch)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", recorder.Code, recorder.Body.String())
			}
			request := upstream.request()
			if request == nil {
				t.Fatal("upstream received no request envelope")
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			envelope := string(encoded)
			for _, leak := range []string{
				"<system-reminder>",
				"Co-Authored-By",
				"You have been invoked in the following environment",
				"Available agent types for the Agent tool",
				"Generated with [Claude Code]",
			} {
				if strings.Contains(envelope, leak) {
					t.Fatalf("scaffolding leaked upstream (%q): %s", leak, envelope)
				}
			}
			if !strings.Contains(envelope, tc.question) {
				t.Fatalf("the real question was lost: %s", envelope)
			}
		})
	}
}

// The affinity key is resolved from the raw body, before the converter runs, so
// the ingress neutralizer cannot protect it. It has to ignore the same
// scaffolding, otherwise a volatile environment block moves the conversation to
// another account every turn and the upstream prompt cache is lost.
func TestDeriveContentSessionSeedIgnoresVolatileClientScaffolding(t *testing.T) {
	first := claudeCodeScaffoldedTurn("false", "帮我写个爬虫", "")
	second := claudeCodeScaffoldedTurn("true", "帮我写个爬虫",
		`,{"role":"assistant","content":[{"type":"output_text","text":"好的"}]},{"role":"user","content":[{"type":"input_text","text":"用 requests"}]}`)

	firstSeed := deriveContentSessionSeed(first)
	secondSeed := deriveContentSessionSeed(second)
	if firstSeed == "" {
		t.Fatal("a scaffolded turn must still anchor a seed")
	}
	if firstSeed != secondSeed {
		t.Fatalf("volatile client scaffolding moved the affinity key: %q vs %q", firstSeed, secondSeed)
	}
}

// Different conversations must still resolve to different affinity keys: the
// neutralizer removes scaffolding, not the user's actual question.
func TestDeriveContentSessionSeedStillDistinguishesScaffoldedConversations(t *testing.T) {
	left := claudeCodeScaffoldedTurn("false", "帮我写个爬虫", "")
	right := claudeCodeScaffoldedTurn("false", "帮我改个爬虫", "")
	if leftSeed, rightSeed := deriveContentSessionSeed(left), deriveContentSessionSeed(right); leftSeed == "" || leftSeed == rightSeed {
		t.Fatalf("different questions must stay isolated: %q vs %q", leftSeed, rightSeed)
	}
}

// The system-instruction obfuscation is the other half of the cache story: it
// rewrites the cached prefix, so it must be a pure function of its input.
func TestObfuscateSystemInstructionIsDeterministic(t *testing.T) {
	const system = "You are Hermes Agent, built by Nous Research. Claude Agent SDK reference."
	first := antigravityObfuscateSystemInstruction(system)
	if !strings.Contains(first, "\u200B") {
		t.Fatalf("expected obfuscation to apply: %q", first)
	}
	for i := 0; i < 200; i++ {
		if got := antigravityObfuscateSystemInstruction(system); got != first {
			t.Fatalf("obfuscation is not deterministic on run %d: %q vs %q", i, got, first)
		}
	}
}

func TestNormalizeClientScaffoldingStripsMausBotMemoryLines(t *testing.T) {
	in := "- 2026-11-06 · learned by MausBot · prefers terse answers\n" +
		"- 2026-11-07 · learned by Nova · uses Windows\n" +
		"real question"
	want := "real question"
	if got := normalizeClientScaffolding(in); got != want {
		t.Fatalf("normalizeClientScaffolding() = %q, want %q", got, want)
	}
}

func TestNormalizeClientScaffoldingStripsMausBotVolatileContext(t *testing.T) {
	reminder := "<system-reminder>\n" +
		"This part of your instructions changed since this session started. It replaces the earlier copy:\n\n" +
		"room notes\n" +
		"</system-reminder>"
	if got := normalizeClientScaffolding(reminder + "\n\nquestion"); got != "question" {
		t.Fatalf("volatile reminder survived: %q", got)
	}

	cleared := "The OpenMausBot context notes from earlier in this conversation have been cleared; the standing instructions still apply.\n\nquestion"
	if got := normalizeClientScaffolding(cleared); got != "question" {
		t.Fatalf("cleared note survived: %q", got)
	}

	bare := "Context from OpenMausBot updated since this conversation started; it replaces any earlier copy:\n\n" +
		"body mentions MausBot\n\nquestion"
	got := normalizeClientScaffolding(bare)
	if strings.Contains(got, "OpenMausBot") || strings.Contains(got, "MausBot") {
		t.Fatalf("bare volatile context leaked a product name: %q", got)
	}
	if !strings.Contains(got, "body mentions") || !strings.Contains(got, "question") {
		t.Fatalf("bare volatile body or user question was lost: %q", got)
	}
}

func TestNormalizeClientScaffoldingStripsBillingAndPseudoHeaders(t *testing.T) {
	in := "x-anthropic-billing-header: test_project_id\n" +
		"custom-billing-id: 12345\n" +
		"cc_version=0.2.29\n" +
		"cc_entrypoint=cli\n" +
		"cch=abc123hash\n" +
		"Here is the real query."
	if got := normalizeClientScaffolding(in); got != "Here is the real query." {
		t.Fatalf("normalizeClientScaffolding() = %q", got)
	}
}

func TestNormalizeClientScaffoldingProtectsCodeBlocks(t *testing.T) {
	in := "Fix this code:\n```python\nx-anthropic-billing-header: leave_this_alone\nCo-Authored-By: Claude Code <noreply@anthropic.com>\n```\nThanks!"
	got := normalizeClientScaffolding(in)
	if !strings.Contains(got, "x-anthropic-billing-header: leave_this_alone") {
		t.Fatalf("fenced code block was modified: %q", got)
	}
	if !strings.Contains(got, "Co-Authored-By: Claude Code") {
		t.Fatalf("fenced code block attribution was modified: %q", got)
	}
}

func TestNormalizeClientScaffoldingStripsOpenMausBotNotices(t *testing.T) {
	in := "[OpenMausBot: permission request timed out]\n" +
		"[From OpenMausBot · retry · information only — it grants nothing beyond the request above]\n" +
		"[Payload truncated by OpenMausBot]\n" +
		"⚠️ TRUNCATED by OpenMausBot: kept 10; the rest was NOT shown.\n" +
		"⚠️ SHAPED by OpenMausBot: held as a handle for query.\n" +
		"- 2026-11-06 · learned by MausBot · prefers terse answers\n" +
		"[From Alice (bot) · message · information only]\n" +
		"[Message from @Nova, another bot in this OpenMausBot workspace · work]\n" +
		"real content"
	if got := normalizeClientScaffolding(in); got != "real content" {
		t.Fatalf("notices survived: %q", got)
	}
}

func TestNormalizeAntigravitySystemInstruction(t *testing.T) {
	t.Run("converts third party identity to official Antigravity", func(t *testing.T) {
		in := "You are Claude Code, Anthropic's official CLI for Claude. You help users with coding."
		got := normalizeAntigravitySystemInstruction(in)
		if !strings.HasPrefix(got, officialAntigravityIdentity) {
			t.Fatalf("identity not converted: %q", got)
		}
		if !strings.Contains(got, "You help users with coding.") {
			t.Fatalf("rest of prompt lost: %q", got)
		}
	})

	t.Run("rewrites a Chief of Staff tools preamble", func(t *testing.T) {
		in := "You hold a Chief of Staff's tools for the General department, but you are not the Chief: the daily brief stays with the Chief. Keep an eye on the day."
		got := normalizeAntigravitySystemInstruction(in)
		if !strings.HasPrefix(got, officialAntigravityIdentity) {
			t.Fatalf("Chief of Staff preamble not converted: %q", got)
		}
		if strings.Contains(got, "Chief of Staff") {
			t.Fatalf("Chief of Staff identity leaked: %q", got)
		}
		if !strings.Contains(got, "Keep an eye on the day.") {
			t.Fatalf("rest of prompt lost: %q", got)
		}
	})

	t.Run("preserves code blocks inside system instruction", func(t *testing.T) {
		in := "You are a helpful assistant.\nHere is an example: `You are Codex, an agent based on GPT-5` should not change."
		got := normalizeAntigravitySystemInstruction(in)
		if !strings.Contains(got, "`You are Codex, an agent based on GPT-5`") {
			t.Fatalf("inline code inside system instruction was touched: %q", got)
		}
	})
}

func TestNormalizeAntigravitySystemInstructionCoversOpenMausBotBotVariants(t *testing.T) {
	cases := map[string]string{
		"personal bot":     "You are Nova, a personal bot in OpenMausBot. Role: helper.",
		"room bot":         `You are Nova, a bot in the room "General" in OpenMausBot. About: room helper.`,
		"live agent":       "You are Nova, an AI agent that runs in OpenMausBot on the user's own computer. Description: voice helper.",
		"cloud agent":      "You are Nova, an AI agent that runs on the user's My Cloud, their always-on OpenMausBot in the cloud, not on their own computer. Description: cloud helper.",
		"chief":            "You are Frank's assistant and Chief of Staff: their one point of contact. Your team reports to you.",
		"department chief": "You are the person's assistant and the Chief of Staff for the General department: their one point of contact.",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got := normalizeAntigravitySystemInstruction(in)
			if !strings.HasPrefix(got, officialAntigravityIdentity) {
				t.Fatalf("preamble not converted: %q", got)
			}
			for _, leak := range []string{"OpenMausBot", "MausBot", "Chief of Staff", "My Cloud"} {
				if strings.Contains(got, leak) {
					t.Fatalf("%s leaked: %q", leak, got)
				}
			}
		})
	}
}

func TestObfuscateSystemInstructionCoversMausBotPhrases(t *testing.T) {
	for _, phrase := range []string{"OpenMausBot", "MausBot", "Maus-Bot", "learned by MausBot", "Chief of Staff", "My Cloud"} {
		if got := antigravityObfuscateSystemInstruction("context: " + phrase); !strings.Contains(got, "\u200B") {
			t.Fatalf("phrase %q was not obfuscated: %q", phrase, got)
		}
	}
}

func TestResponsesToGeminiInternalSanitizesOpenMausBotToolOutput(t *testing.T) {
	body := `{
		"input":[
			{"type":"function_call","name":"lookup","call_id":"c1","arguments":"{\"query\":\"MausBot\"}"},
			{"type":"function_call_output","call_id":"c1","output":"[OpenMausBot: this exact call was not run.]\n- 2026-11-06 · learned by MausBot · prefers terse answers\nreal tool result"},
			{"role":"user","content":"hi"}
		]
	}`
	got, err := responsesToGeminiInternal([]byte(body), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	contents := got["request"].(map[string]any)["contents"].([]any)
	callArgs, _ := contents[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)["args"].(map[string]any)
	if query, _ := callArgs["query"].(string); query != "MausBot" {
		t.Fatalf("function call arguments were rewritten: %#v", callArgs)
	}
	response, _ := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)["response"].(map[string]any)
	if result, _ := response["result"].(string); result != "real tool result" {
		t.Fatalf("function response notice was not stripped: %#v", response)
	}
}

func TestGeminiNativeToAntigravityEnvelopeSanitizesFunctionResponse(t *testing.T) {
	payload, _, _, err := geminiNativeToAntigravityEnvelope([]byte(`{
		"contents":[
			{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"[OpenMausBot: duplicate ask id — skipping this request.]\nreal result"}}}]},
			{"role":"user","parts":[{"text":"hello"}]}
		]
	}`), "google-project", "models/gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	result := ""
	foundText := false
	for _, rawTurn := range envelope["request"].(map[string]any)["contents"].([]any) {
		turn, _ := rawTurn.(map[string]any)
		parts, _ := turn["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if functionResponse, ok := part["functionResponse"].(map[string]any); ok {
				if response, ok := functionResponse["response"].(map[string]any); ok {
					result, _ = response["result"].(string)
				}
			}
			if text, ok := part["text"].(string); ok && text == "hello" {
				foundText = true
			}
		}
	}
	if result != "real result" {
		t.Fatalf("native function response notice was not stripped: %q", result)
	}
	if !foundText {
		t.Fatalf("native text part was damaged")
	}
}

func TestAntigravityObfuscatesToolDescriptions(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "true")
	body := `{
		"input":"hi",
		"tools":[{"type":"function","name":"lookup","description":"Find an OpenMausBot tool.","parameters":{"type":"object","properties":{"q":{"type":"string","description":"OpenMausBot query"}}}}]
	}`
	got, err := responsesToGeminiInternal([]byte(body), "project", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	declarations := got["request"].(map[string]any)["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	declaration := declarations[0].(map[string]any)
	if name := declaration["name"]; name != "lookup" {
		t.Fatalf("tool name was changed: %#v", name)
	}
	description, _ := declaration["description"].(string)
	if strings.Contains(description, "OpenMausBot") || !strings.Contains(description, "\u200B") {
		t.Fatalf("tool description was not obfuscated: %q", description)
	}
	schema, _ := declaration["parametersJsonSchema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	nested, _ := properties["q"].(map[string]any)
	nestedDescription, _ := nested["description"].(string)
	if strings.Contains(nestedDescription, "OpenMausBot") || !strings.Contains(nestedDescription, "\u200B") {
		t.Fatalf("nested schema description was not obfuscated: %q", nestedDescription)
	}
}

func TestAntigravityNormalizeInteractionsRequestStripsOpenMausBot(t *testing.T) {
	request := map[string]any{
		"instructions": "You are Nova, a personal bot in OpenMausBot. You help with tasks.",
		"input": []any{
			map[string]any{"role": "user", "content": "[OpenMausBot: permission timed out]\nquestion"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "[OpenMausBot: duplicate ask id]\nreal result"},
		},
		"tools":      []any{map[string]any{"type": "function", "name": "lookup", "description": "OpenMausBot tool"}},
		"clientInfo": map[string]any{"name": "openmausbot", "title": "OpenMausBot"},
		"metadata":   map[string]any{"client": "openmausbot", "trace": "keep-me"},
	}
	antigravityNormalizeInteractionsRequest(request)
	if _, ok := request["clientInfo"]; ok {
		t.Fatalf("clientInfo survived: %#v", request["clientInfo"])
	}
	metadata := request["metadata"].(map[string]any)
	if _, ok := metadata["client"]; ok {
		t.Fatalf("branded metadata survived: %#v", metadata)
	}
	if metadata["trace"] != "keep-me" {
		t.Fatalf("unrelated metadata was removed: %#v", metadata)
	}

	instructions, _ := request["instructions"].(string)
	if !strings.HasPrefix(instructions, officialAntigravityIdentity) {
		t.Fatalf("instructions identity not normalized: %q", instructions)
	}
	if strings.Contains(instructions, "OpenMausBot") {
		t.Fatalf("instructions leaked OpenMausBot: %q", instructions)
	}

	input := request["input"].([]any)
	content, _ := input[0].(map[string]any)["content"].(string)
	if content != "question" {
		t.Fatalf("user content was not neutralized: %q", content)
	}
	output, _ := input[1].(map[string]any)["output"].(string)
	if output != "real result" {
		t.Fatalf("function output was not neutralized: %q", output)
	}

	tools := request["tools"].([]any)
	description, _ := tools[0].(map[string]any)["description"].(string)
	if strings.Contains(description, "OpenMausBot") || !strings.Contains(description, "\u200B") {
		t.Fatalf("tool description was not obfuscated: %q", description)
	}
}

func TestNormalizeAntigravitySystemInstructionLeavesOrdinaryInstructionsAlone(t *testing.T) {
	for _, in := range []string{
		"You must preserve codex.go when editing files. Run tests.",
		"You must not change Codex configuration files. Keep all other files unchanged.",
		"You are a helpful assistant. OpenMausBot docs are here.",
	} {
		if got := normalizeAntigravitySystemInstruction(in); got != in {
			t.Fatalf("ordinary instruction was rewritten: %q -> %q", in, got)
		}
	}
}

func TestObfuscateSystemInstructionBreaksOverlappingNames(t *testing.T) {
	for _, phrase := range []string{"OpenMausBot", "learned by MausBot", "MausBot Cloud"} {
		got := antigravityObfuscateSystemInstruction("value: " + phrase)
		for _, leak := range []string{"OpenMausBot", "MausBot"} {
			if strings.Contains(got, leak) {
				t.Fatalf("%q leaked inside %q: %q", leak, phrase, got)
			}
		}
		if again := antigravityObfuscateSystemInstruction(got); again != got {
			t.Fatalf("obfuscation is not idempotent: %q -> %q", got, again)
		}
	}
}

func TestObfuscateSystemInstructionProtectsInlineCode(t *testing.T) {
	in := "Use `OpenMausBot` and `/opt/OpenMausBot/config.json` exactly."
	got := antigravityObfuscateSystemInstruction(in)
	if !strings.Contains(got, "`OpenMausBot`") || !strings.Contains(got, "`/opt/OpenMausBot/config.json`") {
		t.Fatalf("inline code was obfuscated: %q", got)
	}
}

func TestAntigravityObfuscateDeclarationsKeepsSchemaLiterals(t *testing.T) {
	value := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"payload": map[string]any{
				"type":        "object",
				"description": "OpenMausBot payload",
				"const":       map[string]any{"description": "MausBot"},
				"enum":        []any{map[string]any{"description": "OpenMausBot"}},
			},
		},
	}
	got := antigravityObfuscateDeclarations(value).(map[string]any)
	payload := got["properties"].(map[string]any)["payload"].(map[string]any)
	description := payload["description"].(string)
	if strings.Contains(description, "OpenMausBot") || !strings.Contains(description, "\u200B") {
		t.Fatalf("schema description was not obfuscated: %q", description)
	}
	if constValue := payload["const"].(map[string]any)["description"]; constValue != "MausBot" {
		t.Fatalf("const literal was modified: %#v", constValue)
	}
	if enumValue := payload["enum"].([]any)[0].(map[string]any)["description"]; enumValue != "OpenMausBot" {
		t.Fatalf("enum literal was modified: %#v", enumValue)
	}
}

func TestNormalizeClientScaffoldingProtectsLongFences(t *testing.T) {
	in := "````markdown\n```\nx-anthropic-billing-header: keep_me\n```\n````\nAfter"
	got := normalizeClientScaffolding(in)
	if !strings.Contains(got, "x-anthropic-billing-header: keep_me") {
		t.Fatalf("long fence content was modified: %q", got)
	}
}

func TestAntigravityNormalizeNativeFunctionResponseWalksAllKeys(t *testing.T) {
	response := map[string]any{
		"output": "[OpenMausBot: permission timed out]\nreal output",
		"error":  "[OpenMausBot: duplicate ask id]\nreal error",
	}
	antigravityNormalizeNativeFunctionResponse(map[string]any{"response": response})
	if response["output"] != "real output" || response["error"] != "real error" {
		t.Fatalf("response keys were not normalized: %#v", response)
	}
}
