package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

func TestPromptRequestContextUsesTheDeltaPrefixAndSeparatesCatalog(t *testing.T) {
	var catalog strings.Builder
	catalog.WriteString("<skills_instructions>\n## Skills\n### Available skills\n")
	for index := 1; index <= 17; index++ {
		fmt.Fprintf(&catalog, "- skill-%d: Help with a task (including a subtask). (file: r1/skill-%d/SKILL.md)\n", index, index)
	}
	catalog.WriteString("</skills_instructions>")
	history := []any{
		map[string]any{"type": "additional_tools", "role": "developer"},
		map[string]any{"type": "message", "role": "developer", "content": "First line.\n\nSecond paragraph.\n" + catalog.String() + "\nFollow the project."},
		map[string]any{"role": "developer", "content": "Another instruction."},
		map[string]any{"role": "developer", "content": "Third message."},
		map[string]any{"role": "developer", "content": "Fourth message."},
		map[string]any{"role": "user", "content": "Please mention ./notes/SKILL.md."},
	}
	makeBody := func(items []any) []byte {
		t.Helper()
		body, err := json.Marshal(map[string]any{"input": items})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	previous := makeBody(history)
	request := database.PromptRequestBody{
		Body: makeBody(append(history, map[string]any{"role": "user", "content": "New instruction."})),
	}
	view := presentPromptRequest(request, -1)
	changes := comparePromptRequestHistory(25, previous, request.Body, view)
	if changes == nil || changes.RepeatedItems != len(history) {
		t.Fatal("fixture needs an inherited prefix")
	}
	got := projectPromptRequestContext(request, previous, changes).Sections
	if len(got) != 6 {
		t.Fatalf("expected five developer sections and one catalog, got %d", len(got))
	}
	counts := map[string]int{}
	for _, section := range got {
		counts[section.Kind]++
		if section.State != promptContextInherited || section.Source != "developer_message" {
			t.Fatalf("wrong request context provenance: %s, %s", section.State, section.Source)
		}
		if section.Kind == promptContextCatalog {
			if len(section.Skills) != 17 || section.Skills[0].Path != "r1/skill-1/SKILL.md" || section.Text != catalog.String() {
				t.Fatal("catalog entries or raw catalog were lost")
			}
		}
	}
	if counts[promptContextDeveloper] != 5 || counts[promptContextCatalog] != 1 || got[0].Text != "First line.\n\nSecond paragraph.\n" {
		t.Fatal("developer instruction sections were not preserved")
	}
}

func TestPromptRequestContextSeparatesSystemAgentsAndInjectedSkill(t *testing.T) {
	previous := []byte(`{"instructions":"Original system","input":[{"role":"user","content":"Prior question"}]}`)
	request := database.PromptRequestBody{Body: []byte(`{
	  "instructions":"New system\nSecond line",
	  "input":[{"role":"user","content":"Prior question"},
	    {"role":"system","content":"Separate system message"},
	    {"role":"developer","content":"# AGENTS.md instructions for /workspace\n\n<INSTRUCTIONS>\nFixture instructions.\n</INSTRUCTIONS>"},
	    {"role":"user","content":"<skill>\n<name>fixture-skill</name>\n<path>/workspace/skills/fixture/SKILL.md</path>\n<content>Injected body.</content>\n</skill>"}]
	}`)}
	view := presentPromptRequest(request, -1)
	changes := comparePromptRequestHistory(11, previous, request.Body, view)
	got := projectPromptRequestContext(request, previous, changes).Sections
	if len(got) != 4 {
		t.Fatalf("unexpected context section count %d", len(got))
	}
	for _, section := range got {
		if section.State != promptContextNew {
			t.Fatal("new context must be new")
		}
	}
	if got[0].Kind != promptContextSystem || got[0].Source != "instructions" || got[0].Text != "New system\nSecond line" ||
		got[1].Kind != promptContextSystem || got[1].Source != "system_message" ||
		got[2].Kind != promptContextAgents || got[2].Path != "/workspace/AGENTS.md" ||
		got[3].Kind != promptContextSkill || got[3].Name != "fixture-skill" || got[3].Evidence != "injected_body" {
		t.Fatal("incorrect source or kind for request instructions")
	}
	fallback := projectPromptRequestContext(request, nil, nil).Sections
	for _, section := range fallback {
		if section.State != promptContextReceived {
			t.Fatal("unmatched request must not claim a baseline")
		}
	}
}

func TestPromptRequestContextTracksReadEvidenceAcrossTurns(t *testing.T) {
	insert, detail, db := promptRequestResultTestFixture(t)
	const question = `{"role":"user","content":"question"}`
	origin := insert("skill", 10, question)
	if err := db.SavePromptRequestResponse(context.Background(), origin, database.PromptRequestResponse{
		Status: "completed",
		ToolCalls: []database.PromptRequestResponseCall{
			{CallID: "read-one", Name: "exec_command", Arguments: `{"cmd":"cat /skills/confirmed/SKILL.md"}`},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if section := detail(origin).PromptContext.Sections[0]; section.Kind != promptContextAttempt || section.Evidence != promptContextReadAttempt {
		t.Fatal("a read call alone was falsely treated as loaded")
	}
	const receipts = `,
          {"type":"function_call","call_id":"read-one","name":"exec_command","arguments":"{\"cmd\":\"cat /skills/confirmed/SKILL.md\"}"},
          {"type":"function_call_output","call_id":"read-one","output":[{"type":"input_text","text":"{\"exit_code\":0,\"output\":\"---\\nname: named-skill\\n---\\nSkill instructions.\"}"}]},
          {"type":"function_call","call_id":"read-two","name":"exec_command","arguments":"{\"cmd\":\"cat /skills/failed/SKILL.md\"}"},
          {"type":"function_call_output","call_id":"read-two","output":"{\"exit_code\":1,\"output\":\"read failed\"}"},
          {"type":"function_call","call_id":"look","name":"exec_command","arguments":"{\"cmd\":\"rg SKILL.md\"}"},
          {"type":"function_call_output","call_id":"look","output":"{\"exit_code\":0,\"output\":\"Searching for skill files.\"}"}`
	receiver := insert("skill", 10, question+receipts)
	inherited := insert("skill", 10, question+receipts+`,{"role":"user","content":"next"}`)
	following := detail(origin).PromptContext.Sections
	if len(following) != 1 || following[0].Kind != promptContextSkill ||
		following[0].Evidence != promptContextReadSuccess || following[0].SourceRequestID != receiver ||
		following[0].Name != "named-skill" || !strings.HasSuffix(following[0].Text, "Skill instructions.") {
		t.Fatal("origin omitted a confirmed skill read or lost its receipt attribution")
	}
	for id, state := range map[int64]string{receiver: promptContextNew, inherited: promptContextInherited} {
		sections := detail(id).PromptContext.Sections
		if len(sections) != 2 || sections[0].State != state || sections[0].Kind != promptContextSkill ||
			sections[1].Kind != promptContextAttempt || sections[1].Evidence != promptContextReadFailed || sections[1].Text != "" {
			t.Fatalf("received or inherited evidence was incorrectly classified for %d", id)
		}
	}
}

func TestPromptRequestContextRecognizesOnlyDirectReadWithSuccessfulBody(t *testing.T) {
	const wrapped = `text(await tools.exec_command({cmd:"cat -- '/work/skills/test-one/SKILL.md'", "workdir":"/work", yield_time_ms:1000}))`
	if path := promptContextReadPath("exec", wrapped); path != "/work/skills/test-one/SKILL.md" {
		t.Fatalf("wrapped direct read path = %q", path)
	}
	if path := promptContextReadPath("exec_command", `{"cmd":"Get-Content -Raw -LiteralPath 'C:\\Users\\fixture\\skills\\demo\\SKILL.md'"}`); path == "" {
		t.Fatal("direct Windows read was not recognized")
	}
	for _, call := range []string{
		`{"cmd":"cat /skills/test/SKILL.md; echo text"}`,
		`{"cmd":"cat '/skills/*/SKILL.md'"}`,
		`{"cmd":"echo /skills/test/SKILL.md"}`,
	} {
		if path := promptContextReadPath("exec_command", call); path != "" {
			t.Fatal("indirect command was incorrectly identified as a direct read")
		}
	}
	for _, output := range []string{"arbitrary text", `{"exit_code":0,"output":""}`, `{"exit_code":1,"output":"No access"}`} {
		if _, evidence := promptContextReceiptBody(output, 0); evidence == promptContextReadSuccess {
			t.Fatal("read without a proven successful output was classified as loaded")
		}
	}
}

func TestPromptRequestContextRequiresInstructionsInLiteralRead(t *testing.T) {
	const header = "---\nname: literal-skill\n---\n"
	const partial = "Warning: truncated output (original token count: 75)\nTotal output lines: 10\n\n"
	for _, tc := range []struct {
		name, command, output, wantKind, wantEvidence string
	}{
		{"head", `head -n 80 -- '/skills/literal-skill/SKILL.md'`, header + "Compile the requested target.", promptContextSkill, promptContextReadSuccess},
		{"sed", `sed -n '1,80p' /skills/literal-skill/SKILL.md`, header + "Use the fixture.", promptContextSkill, promptContextReadSuccess},
		{"powershell", `Get-Content -TotalCount 80 -LiteralPath 'C:\skills\literal-skill\SKILL.md' -Encoding 'UTF8' -ErrorAction Stop`,
			header + "Follow the project instructions.", promptContextSkill, promptContextReadSuccess},
		{"plain body", `cat /skills/literal-skill/SKILL.md`, "Run the test.", promptContextSkill, promptContextReadSuccess},
		{"incomplete metadata", `head -n2 /skills/literal-skill/SKILL.md`, "---\nname: literal-skill", promptContextAttempt, promptContextNoBody},
		{"metadata without delimiters", `sed -n '2p' /skills/literal-skill/SKILL.md`, "name: literal-skill", promptContextAttempt, promptContextNoBody},
		{"complete metadata only", `sed -n "1,3p" /skills/literal-skill/SKILL.md`, header, promptContextAttempt, promptContextNoBody},
		{"heading only", `sed -n 1,4p /skills/literal-skill/SKILL.md`, header + "# Literal skill", promptContextAttempt, promptContextNoBody},
		{"truncation metadata only", `cat /skills/literal-skill/SKILL.md`, partial, promptContextAttempt, promptContextNoBody},
		{"truncated instructions", `cat /skills/literal-skill/SKILL.md`, partial + header + "…800 chars truncated…", promptContextAttempt, promptContextTruncatedBody},
		{"retained instructions", `cat /skills/literal-skill/SKILL.md`, partial + header + "Run the test.\n…800 chars truncated…", promptContextSkill, promptContextReadSuccess},
		{"powershell error", `Get-Content -Raw -LiteralPath 'C:\skills\literal-skill\SKILL.md'`,
			"Get-Content : Cannot find path", promptContextAttempt, promptContextNoBody},
		{"compound shell command", `sed -n '1,80p' /skills/literal-skill/SKILL.md; echo anything`, header + "Run the test.", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := promptContextReads{byCallID: make(map[string]promptContextReadCall)}
			call, _ := json.Marshal(map[string]string{"cmd": tc.command})
			receipt, _ := json.Marshal(map[string]any{"exit_code": 0, "output": tc.output})
			reads.addCall("read", "exec_command", string(call), promptContextNew)
			reads.addReceipt("read", string(receipt), promptContextNew, 42, false)
			if tc.wantKind == "" {
				if len(reads.sections) != 0 {
					t.Fatalf("non-literal call was treated as a read: %+v", reads.sections)
				}
				return
			}
			if len(reads.sections) != 1 || reads.sections[0].Kind != tc.wantKind ||
				reads.sections[0].Evidence != tc.wantEvidence || reads.sections[0].SourceRequestID != 42 {
				t.Fatalf("wrong receipt evidence: %+v", reads.sections)
			}
		})
	}
}

func TestPromptRequestContextStructuredFileReceiptRequiresMatchingPathAndSuccess(t *testing.T) {
	const path = "/skills/future-mcp/SKILL.md"
	const body = "---\nname: future-mcp\n---\nCheck the fixture."
	for _, tc := range []struct {
		name, path string
		status     map[string]any
		want       string
	}{
		{"matching successful file", path, map[string]any{"isError": false}, promptContextSkill},
		{"different file", "/skills/other/SKILL.md", map[string]any{"isError": false}, promptContextAttempt},
		{"missing status", path, map[string]any{}, promptContextAttempt},
		{"failed file", path, map[string]any{"isError": true}, promptContextAttempt},
		{"contradicting status", path, map[string]any{"isError": false, "exit_code": 1}, promptContextAttempt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := promptContextReads{byCallID: make(map[string]promptContextReadCall)}
			call, _ := json.Marshal(map[string]string{"path": path})
			tc.status["structuredContent"] = map[string]string{"path": tc.path, "text": body}
			receipt, _ := json.Marshal(tc.status)
			reads.addCall("read", "mcp__filesystem__read_text_file", string(call), promptContextNew)
			reads.addReceipt("read", string(receipt), promptContextNew, 43, false)
			if len(reads.sections) != 1 || reads.sections[0].Kind != tc.want || reads.sections[0].SourceRequestID != 43 {
				t.Fatalf("wrong structured file evidence: %+v", reads.sections)
			}
		})
	}
}

func TestPromptRequestContextAttributesMatchingHeadersInPowerShellBatch(t *testing.T) {
	reads := promptContextReads{byCallID: make(map[string]promptContextReadCall)}
	const command = `{"cmd":"Get-Content -Encoding utf8 D:/fixture/skills/first/SKILL.md; Get-Content -Encoding utf8 D:/fixture/skills/second/SKILL.md -TotalCount 57; Write-Output \"log; Get-Content -Encoding utf8 D:/fixture/skills/quoted/SKILL.md\"; if ($false) { Write-Output log; Get-Content D:/fixture/skills/nested/SKILL.md; }; Get-Content -Encoding utf8 D:/fixture/notes.md -TotalCount 85","max_output_tokens":14000}`
	const receipt = "Chunk ID: private-fixture\nWall time: 0.3 seconds\nProcess exited with code 0\nOriginal token count: 81\nOutput:\n" +
		"Unrelated file line with no blank separator next\n---\nname: first\ndescription: Matching document\n---\n# First document\nPrivate unrelated text\n" +
		"---\nname: quoted\n---\n# Unmatched document\n"
	reads.addCall("batch", "exec_command", command, promptContextNew)
	reads.addReceipt("batch", receipt, promptContextNew, 51, false)
	if len(reads.sections) != 2 {
		t.Fatalf("expected two literal candidates, got %+v", reads.sections)
	}
	first, second := reads.sections[0], reads.sections[1]
	if first.Kind != promptContextSkill || first.Name != "first" || first.SourceRequestID != 51 ||
		first.Evidence != promptContextReadSuccess || first.Text != "---\nname: first\ndescription: Matching document\n---" {
		t.Fatalf("missing attribution for the matching document: %+v", first)
	}
	if second.Kind != promptContextAttempt || second.Name != "second" || second.Evidence != promptContextNoBody || second.Text != "" {
		t.Fatalf("a referenced skill without a matching body was marked as loaded: %+v", second)
	}

	reads.addCall("truncated", "exec_command", command, promptContextNew)
	truncated := strings.Replace(receipt, "Output:\n", "Output:\nWarning: truncated output (original token count: 99)\nTotal output lines: 263\n\n…38 chars truncated…\n", 1)
	reads.addReceipt("truncated", truncated, promptContextNew, 53, false)
	if reads.sections[2].Kind != promptContextSkill || reads.sections[3].Kind != promptContextAttempt ||
		reads.sections[3].Evidence != promptContextTruncatedBody {
		t.Fatal("a truncated batch should explain only the absent Skill body")
	}

	reads.addCall("failed", "exec_command", command, promptContextNew)
	reads.addReceipt("failed", receipt, promptContextNew, 52, true)
	for _, section := range reads.sections[4:] {
		if section.Kind != promptContextAttempt || section.Evidence != promptContextReadFailed {
			t.Fatalf("a failed batch confirmed a skill: %+v", section)
		}
	}
}

func TestPromptRequestContextMatchesStatefulReceiptWithinItsConversation(t *testing.T) {
	insert, detail, db := promptRequestResultTestFixture(t)
	origin := insert("detached", 17, `{"role":"user","content":"question"}`)
	const receipt = `{"type":"function_call_output","call_id":"read-one","output":"{\"exit_code\":0,\"output\":\"Skill instructions.\"}"}`
	isolated := insert("detached", 18, receipt)
	receiver := insert("detached", 17, receipt)
	if err := db.SavePromptRequestResponse(context.Background(), origin, database.PromptRequestResponse{
		Status: "completed",
		ToolCalls: []database.PromptRequestResponseCall{
			{CallID: "read-one", Name: "exec_command", Arguments: `{"cmd":"cat /skills/known/SKILL.md"}`},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := detail(isolated).PromptContext.Sections; len(got) != 0 {
		t.Fatal("receipt was matched across distinct API keys")
	}
	if got := detail(receiver).PromptContext.Sections; len(got) != 1 || got[0].Kind != promptContextSkill ||
		got[0].State != promptContextReceived || got[0].SourceRequestID != receiver {
		t.Fatal("detached receipt did not use its observed call from the same conversation")
	}
}
