package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRequestTasksGroupCarryForwardAndJoinObservations(t *testing.T) {
	handler, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC)
	meta := `{"thread_id":"fixture-thread","turn_id":"my-turn"}`
	add := func(key int64, offset time.Duration, input string) int64 {
		t.Helper()
		body := []byte(fmt.Sprintf(`{"client_metadata":%s,"input":[%s]}`, meta, input))
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{
				CreatedAt: start.Add(offset), Endpoint: "/v1/responses", Transport: "http", Model: "gpt-fixture",
			},
			ContentType: "application/json", Body: body, ConversationGroup: database.PromptRequestConversationGroup(body, key),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishPromptRequest(ctx, id, "allow", http.StatusOK); err != nil {
			t.Fatal(err)
		}
		return id
	}

	const prompt = `{"role":"user","id":"submitted","content":[{"type":"input_text","text":"用户任务\n保留换行"},{"type":"input_image","file_id":"new-picture"}]}`
	const initial = `{"role":"user","content":"<environment_context>injected</environment_context>"},` + prompt
	const output = `{"type":"function_call_output","call_id":"call-a","output":"fixture result"}`
	first := add(8, time.Second, initial)
	second := add(8, 2*time.Second, output)
	third := add(8, 3*time.Second, initial+`,`+output)
	next := add(8, 4*time.Second, initial+`,`+output+`,`+prompt)
	other := add(9, 5*time.Second, initial)
	save := func(id int64, reply database.PromptRequestResponse) {
		t.Helper()
		if err := db.SavePromptRequestResponse(ctx, id, reply); err != nil {
			t.Fatal(err)
		}
	}
	save(first, database.PromptRequestResponse{
		Status: "completed", Text: "commentary is not final", CommentaryText: "checking",
		ToolCalls: []database.PromptRequestResponseCall{{CallID: "call-a", Name: "exec_command", Arguments: `{"cmd":"fixture"}`}},
	})
	finished := start.Add(3900 * time.Millisecond)
	save(third, database.PromptRequestResponse{
		Status: "completed", Text: "reply phases", FinalText: "最终答复\n保留换行", FinalObserved: true, FinishedAt: &finished,
	})
	router := gin.New()
	handler.RegisterRoutes(router)
	get := func(path string, result any) {
		t.Helper()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/admin/prompt-filter"+path, nil)
		request.Header.Set("X-Admin-Key", "admin-secret")
		router.ServeHTTP(response, request)
		if err := json.Unmarshal(response.Body.Bytes(), result); response.Code != http.StatusOK || err != nil {
			t.Fatalf("request %s: status %d error %v", path, response.Code, err)
		}
	}

	var page struct {
		Tasks []promptTaskSummary
		Total int `json:"total_requests"`
	}
	get(fmt.Sprintf("/request-conversations/%d/tasks", next), &page)
	if page.Total != 4 || len(page.Tasks) != 2 || !reflect.DeepEqual(page.Tasks[0].RequestIDs, []int64{first, second, third}) ||
		!reflect.DeepEqual(page.Tasks[1].RequestIDs, []int64{next}) {
		t.Fatalf("wrong conversation grouping: %+v", page)
	}
	get(fmt.Sprintf("/request-conversations/%d/tasks", other), &page)
	if page.Total != 1 || len(page.Tasks) != 1 || !reflect.DeepEqual(page.Tasks[0].RequestIDs, []int64{other}) {
		t.Fatal("merged a different authenticated scope")
	}
	var task promptTaskDetail
	get(fmt.Sprintf("/request-tasks/%d", first), &task)
	if task.User == nil || task.User.RequestID != first || task.User.Text != "用户任务\n保留换行" || len(task.User.Images) != 1 {
		t.Fatalf("wrong user message: %+v", task.User)
	}
	if len(task.Steps) != 3 || task.Steps[0].Commentary != "checking" || len(task.Steps[0].Tools) != 1 ||
		task.Steps[0].Tools[0].FromRequestID != second || task.Steps[0].Tools[0].Result != "fixture result" ||
		len(task.Steps[1].Tools) != 0 || len(task.Steps[2].Tools) != 0 {
		t.Fatalf("wrong observed tool association: %+v", task.Steps)
	}
	if task.Final == nil || task.Final.Text != "最终答复\n保留换行" || task.Final.Source != "gateway" ||
		task.TimingSource != "gateway_observed" || task.DurationMS == nil || *task.DurationMS != 2900 {
		t.Fatalf("wrong captured final reply or timing: %+v", task)
	}
	if len(task.Skills) != 0 {
		t.Fatal("ordinary call was promoted to a skill")
	}

	matchGroup := database.PromptTaskClientGroups("fixture-thread", 8)
	clientFinish := start.Add(560 * time.Second)
	clientReply := database.PromptTaskClientSnapshot{
		TurnID: "my-turn", StartedAt: start, FinishedAt: &clientFinish, Status: "completed",
		FinalText: "客户端最终答复", Steps: []database.PromptTaskClientStep{{Name: "tool", Kind: "tool", Status: "completed"}},
	}
	digest := database.PromptTaskClientUserDigest(task.User.Text)
	ok, err := db.StorePromptTaskClientSnapshot(ctx, matchGroup, digest, clientReply)
	if err != nil || ok {
		t.Fatalf("ambiguous client report must remain unassociated: %t %v", ok, err)
	}
	task = promptTaskDetail{}
	get(fmt.Sprintf("/request-tasks/%d", first), &task)
	if task.Final == nil || task.Final.Source != "gateway" {
		t.Fatal("one client completion was allowed to describe two different user sends")
	}
	meta = `{"thread_id":"fixture-thread","turn_id":"client-turn"}`
	clientTask := add(8, 7*time.Second, prompt)
	clientReply.TurnID = "client-turn"
	if ok, err := db.StorePromptTaskClientSnapshot(ctx, matchGroup, digest, clientReply); err != nil || !ok {
		t.Fatalf("unique client fixture could not be stored: %t %v", ok, err)
	}
	task = promptTaskDetail{}
	get(fmt.Sprintf("/request-tasks/%d", clientTask), &task)
	if task.Final == nil || task.Final.Source != "client" || task.Final.Text != "客户端最终答复" ||
		task.TimingSource != "client" || task.DurationMS == nil || *task.DurationMS != 560000 || len(task.ClientSteps) != 1 {
		t.Fatalf("client evidence did not win: %+v", task)
	}
}

func TestPromptRequestTasksDoNotPromoteUnmarkedReplies(t *testing.T) {
	ctx := context.Background()
	handler, db := newTestAdminHandlerFixture(t)
	body := []byte(`{"input":[{"role":"user","content":"fixture"}]}`)
	id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
		PromptRequestRecord: database.PromptRequestRecord{CreatedAt: time.Now().UTC()},
		Body:                body,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	if err := db.SavePromptRequestResponse(ctx, id, database.PromptRequestResponse{Status: "completed", Text: "unmarked", FinishedAt: &now}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ReadPromptRequestTaskRows(ctx, id, 0, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	task, err := handler.projectPromptTask(ctx, rows.Group, groupPromptTaskRows(rows.Rows)[0], rows.Rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.Final != nil || task.DurationMS != nil || task.Status != "unobserved" {
		t.Fatal("unmarked reply was promoted to a final result")
	}

	router := gin.New()
	handler.RegisterRoutes(router)
	notAdmin := httptest.NewRecorder()
	router.ServeHTTP(notAdmin, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/prompt-filter/request-tasks/%d", id), nil))
	if notAdmin.Code == http.StatusOK || strings.Contains(notAdmin.Body.String(), "fixture") {
		t.Fatal("task replay detail did not require administration")
	}
}

func TestPromptRequestTasksMarkSkillsOnTheirOriginatingSteps(t *testing.T) {
	handler, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()
	start := time.Now().UTC()
	add := func(input string, offset time.Duration) int64 {
		t.Helper()
		body := []byte(`{"client_metadata":{"thread_id":"step-skills","turn_id":"this-turn"},"input":[` + input + `]}`)
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{CreatedAt: start.Add(offset), Endpoint: "/v1/responses", Transport: "http"},
			ContentType:         "application/json", Body: body, ConversationGroup: database.PromptRequestConversationGroup(body, 8),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	saveCalls := func(id int64, calls ...database.PromptRequestResponseCall) {
		t.Helper()
		if err := db.SavePromptRequestResponse(ctx, id, database.PromptRequestResponse{Status: "completed", ToolCalls: calls}); err != nil {
			t.Fatal(err)
		}
	}

	const earlierSkill = `{"role":"developer","content":"<skill>\n<name>earlier-skill</name>\n<path>/skills/earlier-skill/SKILL.md</path>\nOld instructions\n</skill>"}`
	const user = earlierSkill + `,{"role":"user","content":"Run the job"}`
	const inject = `{"role":"developer","content":"<skill>\n<name>injected-skill</name>\n<path>/skills/injected-skill/SKILL.md</path>\nCurrent instructions\n</skill>"}`
	const readReceipt = `{"type":"function_call_output","call_id":"read-first","output":{"exit_code":0,"output":"---\nname: read-skill\n---\nConfirmed instructions"}}`
	const failedReceipt = `{"type":"function_call_output","call_id":"read-failed","output":{"exit_code":1,"output":"File not found"}}`
	const repeatReceipt = `{"type":"function_call_output","call_id":"read-again","output":{"exit_code":0,"output":"---\nname: read-skill\n---\nConfirmed instructions"}}`
	const earlyHelperReceipt = `{"type":"function_call_output","call_id":"early-helper","output":{"exit_code":0,"output":"Done"}}`
	const helperReceipt = `{"type":"function_call_output","call_id":"helper","output":{"exit_code":0,"output":"Done"}}`
	const mentionedHelperReceipt = `{"type":"function_call_output","call_id":"mentioned-helper","output":{"exit_code":0,"output":"Printed path"}}`
	const failingHelperReceipt = `{"type":"function_call_output","call_id":"failing-helper","output":{"exit_code":1,"output":"Failed"}}`
	add(earlierSkill+`,{"role":"user","content":"Earlier job"}`, 0)
	first := add(user, time.Second)
	secondHistory := strings.Join([]string{user, readReceipt, failedReceipt, inject, earlyHelperReceipt}, ",")
	second := add(secondHistory, 2*time.Second)
	third := add(strings.Join([]string{secondHistory, repeatReceipt, helperReceipt, mentionedHelperReceipt, failingHelperReceipt}, ","), 3*time.Second)
	saveCalls(first,
		database.PromptRequestResponseCall{CallID: "read-first", Name: "exec_command", Arguments: `{"cmd":"cat /skills/read-skill/SKILL.md"}`},
		database.PromptRequestResponseCall{CallID: "read-failed", Name: "exec_command", Arguments: `{"cmd":"cat /skills/failed-skill/SKILL.md"}`},
		database.PromptRequestResponseCall{CallID: "early-helper", Name: "exec_command", Arguments: `{"cmd":"bash /skills/read-skill/scripts/check.sh"}`},
	)
	saveCalls(second,
		database.PromptRequestResponseCall{CallID: "read-again", Name: "exec_command", Arguments: `{"cmd":"cat /skills/read-skill/SKILL.md"}`},
		database.PromptRequestResponseCall{CallID: "helper", Name: "exec_command", Arguments: `{"cmd":"bash /skills/read-skill/scripts/check.sh --private-arg"}`},
		database.PromptRequestResponseCall{CallID: "mentioned-helper", Name: "exec_command", Arguments: `{"cmd":"echo /skills/read-skill/scripts/check.sh"}`},
		database.PromptRequestResponseCall{CallID: "failing-helper", Name: "exec_command", Arguments: `{"cmd":"bash /skills/read-skill/scripts/check.sh"}`},
	)

	router := gin.New()
	handler.RegisterRoutes(router)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/prompt-filter/request-tasks/%d", first), nil)
	request.Header.Set("X-Admin-Key", "admin-secret")
	router.ServeHTTP(response, request)
	var task promptTaskDetail
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil || response.Code != http.StatusOK {
		t.Fatalf("get task: HTTP %d, %v", response.Code, err)
	}
	if len(task.Steps) != 3 {
		t.Fatalf("task has %d steps", len(task.Steps))
	}
	firstSkill := promptTaskStepSkill{Name: "read-skill", Path: "/skills/read-skill/SKILL.md", Source: "tool_result", SourceRequestID: second}
	reloadedSkill := firstSkill
	reloadedSkill.SourceRequestID = third
	injectedSkill := promptTaskStepSkill{Name: "injected-skill", Path: "/skills/injected-skill/SKILL.md", Source: "developer_message", SourceRequestID: second}
	if !reflect.DeepEqual(task.Steps[0].Skills, []promptTaskStepSkill{firstSkill}) ||
		!reflect.DeepEqual(task.Steps[1].Skills, []promptTaskStepSkill{injectedSkill, reloadedSkill}) ||
		len(task.Steps[2].Skills) != 0 {
		t.Fatalf("unexpected Skill locations: first=%+v second=%+v third=%+v", task.Steps[0].Skills, task.Steps[1].Skills, task.Steps[2].Skills)
	}
	if len(task.Skills) != 3 {
		t.Fatalf("task-level Skill evidence has %d sections", len(task.Skills))
	}
	wantUsage := []promptTaskStepSkillUsage{{
		Name: "read-skill", Path: "/skills/read-skill/scripts/check.sh", Source: "skill_helper_execution", SourceRequestID: third,
		Command: "bash /skills/read-skill/scripts/check.sh", Evidence: "Process exited with code 0",
	}}
	if len(task.Steps[0].SkillUsage) != 0 || !reflect.DeepEqual(task.Steps[1].SkillUsage, wantUsage) || len(task.Steps[2].SkillUsage) != 0 {
		t.Fatal("only a subsequent successful helper runs with specific evidence should be marked as actual use")
	}
}

func TestPromptRequestTasksExtractTrustedSkillInjectionsWithoutRepeatingHistory(t *testing.T) {
	handler, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()
	start := time.Now().UTC()
	tag := func(name string) string {
		return "<skill>\n<name>" + name + "</name>\n<path>/skills/" + name + "/SKILL.md</path>\nInstructions for this Skill.\n</skill>"
	}
	message := func(role, content string) map[string]string {
		return map[string]string{"role": role, "content": content}
	}
	catalog := message("developer", "<skills_instructions>\n### Available skills\n- catalog-only: An available Skill (/skills/catalog-only/SKILL.md)\n</skills_instructions>")
	actualUser := message("user", "Can you review the report? Here is a sample Skill format:\n"+tag("example-only"))
	add := func(offset time.Duration, key int64, turn, instructions string, messages ...map[string]string) int64 {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"client_metadata": map[string]string{"thread_id": "trusted-skill-thread", "turn_id": turn},
			"instructions":    instructions,
			"input":           messages,
		})
		if err != nil {
			t.Fatal(err)
		}
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{CreatedAt: start.Add(offset), Endpoint: "/v1/responses", Transport: "http"},
			ContentType:         "application/json", Body: body, ConversationGroup: database.PromptRequestConversationGroup(body, key),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	firstMessages := []map[string]string{catalog, message("developer", tag("pre-user")), message("user", tag("native-fragment")), actualUser}
	first := add(time.Second, 8, "first-turn", tag("root-skill"), firstMessages...)
	add(2*time.Second, 8, "first-turn", tag("root-skill"), firstMessages...)
	nextMessages := append(slices.Clone(firstMessages), message("developer", tag("later-load")), message("user", "Review the second report."))
	next := add(3*time.Second, 8, "next-turn", tag("root-skill"), nextMessages...)
	repeatMessages := append(slices.Clone(nextMessages), message("user", "Run the review again."), message("user", tag("native-fragment")))
	repeated := add(4*time.Second, 8, "repeat-turn", tag("root-skill"), repeatMessages...)
	independent := add(5*time.Second, 9, "first-turn", tag("root-skill"), actualUser)
	single := add(6*time.Second, 0, "first-turn", tag("root-skill"), actualUser)

	router := gin.New()
	handler.RegisterRoutes(router)
	check := func(id int64, steps int, user string, known map[string]string) {
		t.Helper()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/prompt-filter/request-tasks/%d", id), nil)
		request.Header.Set("X-Admin-Key", "admin-secret")
		router.ServeHTTP(response, request)
		var task promptTaskDetail
		if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil || response.Code != http.StatusOK {
			t.Fatalf("get task: HTTP %d, %v", response.Code, err)
		}
		if task.User == nil || task.User.Text != user || len(task.Steps) != steps || len(task.Skills) != len(known) ||
			len(task.Steps[0].Skills) != len(known) {
			t.Fatalf("wrong injected Skill grouping or task boundary: steps=%d skills=%+v", len(task.Steps), task.Skills)
		}
		for i, entry := range task.Skills {
			marker := task.Steps[0].Skills[i]
			if known[entry.Name] != entry.Source || entry.Kind != promptContextSkill || entry.Evidence != "injected_body" ||
				entry.SourceRequestID != id || marker.SourceRequestID != id || marker.Name != entry.Name {
				t.Fatalf("wrong structured proof: %+v marker=%+v", entry, marker)
			}
		}
		for _, step := range task.Steps[1:] {
			if len(step.Skills) != 0 {
				t.Fatal("unchanged historical context was marked as loaded again")
			}
		}
	}
	check(first, 2, actualUser["content"], map[string]string{
		"root-skill": "instructions", "pre-user": "developer_message", "native-fragment": "user_message",
	})
	check(next, 1, "Review the second report.", map[string]string{"later-load": "developer_message"})
	check(repeated, 1, "Run the review again.", map[string]string{"native-fragment": "user_message"})
	check(independent, 1, actualUser["content"], map[string]string{"root-skill": "instructions"})
	check(single, 1, actualUser["content"], map[string]string{"root-skill": "instructions"})
}

func TestPromptRequestTasksSkillUseRequiresLiteralExecutionInTheProvenDirectory(t *testing.T) {
	projection := promptTaskProjection{loadedDirectories: make(map[string]promptTaskSkillDirectory)}
	projection.rememberLoadedSkill(promptRequestContextSection{
		Name: "build-check", Path: ".agents/skills/build-check/SKILL.md", Source: "tool_result", SourceRequestID: 2,
	}, "C:/project-a")
	projection.rememberLoadedSkill(promptRequestContextSection{
		Name: "build-check", Path: "/project-a/.agents/skills/build-check/SKILL.md", Source: "tool_result", SourceRequestID: 2,
	}, "")
	const script = ".agents/skills/build-check/scripts/check.ps1"
	const absoluteScript = "/project-a/.agents/skills/build-check/scripts/check.ps1"
	const pythonScript = "/project-a/.agents/skills/build-check/scripts/check.py"
	const shellScript = "/project-a/.agents/skills/build-check/scripts/check.sh"
	const nodeScript = "/project-a/.agents/skills/build-check/scripts/check.mjs"
	for _, tc := range []struct {
		name, command, workDir, path string
		requestID                    int64
	}{
		{name: "explicit invocation", command: "& '" + script + "' --private-arg", workDir: "C:/project-a", path: "C:/project-a/" + script, requestID: 2},
		{name: "before the read was confirmed", command: "& '" + script + "'", workDir: "C:/project-a", requestID: 1},
		{name: "same relative path elsewhere", command: "& '" + script + "'", workDir: "C:/project-b", requestID: 2},
		{name: "unknown relative workdir", command: "& '" + script + "'", requestID: 2},
		{name: "Windows path relative to current drive", command: `& '\project-a\.agents\skills\build-check\scripts\check.ps1'`, workDir: "C:/project-a", requestID: 2},
		{name: "quoting alone", command: "'" + absoluteScript + "'", requestID: 2},
		{name: "reading rather than running", command: "Get-Content " + absoluteScript, requestID: 2},
		{name: "compound command", command: "& '" + absoluteScript + "'; echo done", requestID: 2},
		{name: "parent path navigation", command: "& './.agents/skills/build-check/../build-check/scripts/check.ps1'", workDir: "C:/project-a", requestID: 2},
		{name: "same spelling on a different platform", command: "& '" + absoluteScript + "'", workDir: "C:/project-a", requestID: 2},
		{name: "drive path in Unix workdir", command: "& 'C:/project-a/" + script + "'", workDir: "/project-a", requestID: 2},
		{name: "drive-relative workdir", command: "& '" + script + "'", workDir: `\project-a`, requestID: 2},
		{name: "absolute Unix invocation", command: "pwsh -File " + absoluteScript, path: absoluteScript, requestID: 3},
		{name: "literal PowerShell file with common switches", command: "PowerShell.exe -NoProfile -NonInteractive -ExecutionPolicy RemoteSigned -File " + absoluteScript, path: absoluteScript, requestID: 3},
		{name: "versioned Python with flags and separator", command: "/usr/local/bin/python3.12 -u -B -- '" + pythonScript + "'", path: pythonScript, requestID: 3},
		{name: "Windows Python launcher with script flags", command: "py.exe -3 -u .agents/skills/build-check/scripts/check.py", workDir: "C:/project-a", path: "C:/project-a/.agents/skills/build-check/scripts/check.py", requestID: 3},
		{name: "shell with literal script flags", command: "bash --noprofile -eu " + shellScript + " --private-arg", path: shellScript, requestID: 3},
		{name: "node with literal script flags", command: "node --enable-source-maps " + nodeScript, path: nodeScript, requestID: 3},
		{name: "inline Python code", command: "python3 -c '" + pythonScript + "'", requestID: 3},
		{name: "Python module invocation", command: "python3 -m " + pythonScript, requestID: 3},
		{name: "inline shell command", command: "bash -c '" + shellScript + "'", requestID: 3},
		{name: "inline PowerShell command", command: "pwsh -NoProfile -Command '" + absoluteScript + "'", requestID: 3},
		{name: "quoted text with a literal inner quote", command: "python3 '\"" + pythonScript + "\"'", requestID: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := projection.matchSkillUsage(tc.command, tc.workDir, tc.requestID)
			if tc.path == "" {
				if got != nil {
					t.Fatal("ambiguous or unproven path was marked as Skill use")
				}
			} else if got == nil || got.Path != tc.path || got.Name != "build-check" || strings.Contains(got.Command, "--private-arg") {
				t.Fatal("exact invocation was not identified, or unrelated arguments leaked into the evidence")
			}
		})
	}
}

func TestPromptRequestTasksSkillUseFollowsOnlyOwnedEmptyPolls(t *testing.T) {
	const start = `{"cmd":"bash --noprofile -e /skills/build-check/scripts/check.sh"}`
	const startedReceipt = "Chunk ID: abc\nWall time: 0.001 seconds\nProcess running with session ID 47321\nOutput:\nstarted"
	const successReceipt = "Chunk ID: def\nWall time: 0.002 seconds\nProcess exited with code 0\nOutput:\nfinished"
	const poll = `{"session_id":47321,"chars":""}`
	for _, tc := range []struct {
		name, input, result                     string
		tracked, reused, concurrent, wantsProof bool
	}{
		{name: "matching completion", input: poll, result: successReceipt, tracked: true, wantsProof: true},
		{name: "unverified turn", input: poll, result: successReceipt},
		{name: "different session", input: `{"session_id":47322,"chars":""}`, result: successReceipt, tracked: true},
		{name: "nonempty terminal write", input: `{"session_id":47321,"chars":"continue\n"}`, result: successReceipt, tracked: true},
		{name: "terminal failure", input: poll, result: strings.Replace(successReceipt, "code 0", "code 1", 1), tracked: true},
		{name: "session still running", input: poll, result: startedReceipt, tracked: true},
		{name: "parallel polls for one session", input: poll, result: successReceipt, tracked: true, concurrent: true},
		{name: "session identifier reused", input: poll, result: successReceipt, tracked: true, reused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projection := promptTaskProjection{
				detail:            &promptTaskDetail{Steps: []promptTaskStep{{RequestID: 2}, {RequestID: 3}, {RequestID: 4}}},
				calls:             make(map[string]promptTaskToolPosition),
				loadedDirectories: make(map[string]promptTaskSkillDirectory),
			}
			if tc.tracked {
				projection.skillSessions = make(map[string]*promptTaskSkillSession)
			}
			projection.rememberLoadedSkill(promptRequestContextSection{
				Name: "build-check", Path: "/skills/build-check/SKILL.md", Source: "tool_result", SourceRequestID: 1,
			}, "")
			projection.addCall(0, "start", "exec_command", start)
			startResult := promptRequestToolResult{Text: startedReceipt, FromRequestID: 3}
			projection.setToolResult(0, &projection.detail.Steps[0].Tools[0], startResult)
			projection.addCall(1, "poll", "write_stdin", tc.input)
			if tc.concurrent {
				projection.addCall(1, "second-poll", "write_stdin", poll)
			}
			projection.setToolResult(1, &projection.detail.Steps[1].Tools[0], promptRequestToolResult{
				Text: tc.result, FromRequestID: 4,
			})
			if tc.reused {
				projection.addCall(2, "unrelated", "exec_command", `{"cmd":"sleep 1"}`)
				startResult.FromRequestID = 5
				projection.setToolResult(2, &projection.detail.Steps[2].Tools[0], startResult)
			}
			projection.appendPolledSkillUsages()
			uses := projection.detail.Steps[0].SkillUsage
			if tc.wantsProof {
				if len(uses) != 1 || uses[0].SourceRequestID != 4 || uses[0].Name != "build-check" {
					t.Fatalf("completion receipt did not identify the original Skill invocation: %+v", uses)
				}
			} else if len(uses) != 0 {
				t.Fatalf("ambiguous session linked to Skill usage: %+v", uses)
			}
			if len(projection.detail.Steps[1].SkillUsage) != 0 {
				t.Fatal("the poll should point to the original command rather than become another invocation")
			}
		})
	}
}

func TestPromptRequestTasksShowsOnlyTerminalArchivedChatReply(t *testing.T) {
	ctx := context.Background()
	handler, db := newTestAdminHandlerFixture(t)
	start := time.Now().UTC().Truncate(time.Second)
	finished := start.Add(2400 * time.Millisecond)
	const answer = "可以做这些事：\n\n- **写作与翻译**"
	const endpoint = "/v1/chat/completions"
	const messages = `"messages":[{"role":"user","content":"能用什么 Skills"}]`
	recorded := database.PromptRequestResponse{Status: "completed", Text: answer, FinishedAt: &finished}
	add := func(t *testing.T, path, fields, group string, at time.Time, status int, reply *database.PromptRequestResponse) int64 {
		t.Helper()
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{CreatedAt: at, Endpoint: path, Transport: "http"},
			Body:                []byte("{" + fields + "}"), ConversationGroup: group,
		})
		if err != nil {
			t.Fatal(err)
		}
		if status != 0 {
			if err := db.FinishPromptRequest(ctx, id, "allow", status); err != nil {
				t.Fatal(err)
			}
		}
		if reply != nil {
			if err := db.SavePromptRequestResponse(ctx, id, *reply); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	project := func(t *testing.T, id int64) promptTaskDetail {
		t.Helper()
		rows, err := db.ReadPromptRequestTaskRows(ctx, id, 0, false, 2)
		if err != nil {
			t.Fatal(err)
		}
		groups := groupPromptTaskRows(rows.Rows)
		if len(groups) != 1 {
			t.Fatalf("expected one task, got %d", len(groups))
		}
		task, err := handler.projectPromptTask(ctx, rows.Group, groups[0], rows.Rows, nil)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	for _, tc := range []struct {
		name, endpoint, fields string
		status                 int
		modify                 func(*database.PromptRequestResponse)
		wantFinal              bool
		terminalStatus         string
	}{
		{name: "archived completed text", endpoint: endpoint, fields: messages, status: 200, wantFinal: true},
		{name: "chat alias and single choice", endpoint: "/chat/completions", fields: messages + `,"n":1`, status: 200, wantFinal: true},
		{name: "combined choices", endpoint: endpoint, fields: messages + `,"n":2`, status: 200},
		{name: "unmarked responses", endpoint: "/v1/responses", fields: messages, status: 200},
		{name: "failed http status", endpoint: endpoint, fields: messages, status: 503, terminalStatus: "failed"},
		{name: "interrupted stream", endpoint: endpoint, fields: messages, status: 200, terminalStatus: "interrupted", modify: func(r *database.PromptRequestResponse) { r.Status = "interrupted" }},
		{name: "unknown truncated record", endpoint: endpoint, fields: messages, status: 200, modify: func(r *database.PromptRequestResponse) { r.Truncated = true }},
		{name: "classified nonfinal", endpoint: endpoint, fields: messages, status: 200, modify: func(r *database.PromptRequestResponse) { r.ChatReplyClassified = true }},
		{name: "has tool call", endpoint: endpoint, fields: messages, status: 200, modify: func(r *database.PromptRequestResponse) {
			r.ToolCalls = []database.PromptRequestResponseCall{{CallID: "lookup-1", Name: "lookup"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := recorded
			if tc.modify != nil {
				tc.modify(&reply)
			}
			task := project(t, add(t, tc.endpoint, tc.fields, "", start, tc.status, &reply))
			if !tc.wantFinal {
				if task.Final != nil {
					t.Fatal("incomplete reply was promoted to a final answer")
				}
				if tc.terminalStatus != "" {
					if task.Status != tc.terminalStatus || task.DurationMS == nil || *task.DurationMS != 2400 || task.TimingSource != "gateway_observed" {
						t.Fatalf("recorded terminal status or elapsed time was lost: %+v", task)
					}
				} else if task.DurationMS != nil {
					t.Fatal("an unobserved task acquired an end time")
				}
				return
			}
			if task.Final == nil || task.Final.Text != answer || task.Final.Source != "gateway" || task.Status != "completed" ||
				task.TimingSource != "gateway_observed" || task.DurationMS == nil || *task.DurationMS != 2400 {
				t.Fatalf("completed archived chat did not appear in the task result: %+v", task)
			}
		})
	}

	t.Run("later request has no final response", func(t *testing.T) {
		fields := messages + `,"client_metadata":{"thread_id":"chat-thread","turn_id":"chat-turn"}`
		group := database.PromptRequestConversationGroup([]byte("{"+fields+"}"), 8)
		id := add(t, endpoint, fields, group, start, 200, &recorded)
		add(t, endpoint, fields, group, start.Add(3*time.Second), 0, nil)
		task := project(t, id)
		if task.RequestCount != 2 || task.Final != nil || task.DurationMS != nil {
			t.Fatal("an earlier archived reply supplied a final answer while a later request was unresolved")
		}
	})
}
