package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	promptTaskPageSize      = 25
	promptTaskScanLimit     = 2048
	promptTaskStepLimit     = 256
	promptTaskReadBytes     = 192 << 20
	promptTaskToolTextLimit = 64 << 10
)

type promptTaskSummary struct {
	ID                    int64                `json:"id"`
	StartedAt             time.Time            `json:"started_at"`
	RequestIDs            []int64              `json:"request_ids"`
	RequestCount          int                  `json:"request_count"`
	UserPreview           string               `json:"user_preview,omitempty"`
	Status                string               `json:"status"`
	StepsTruncated        bool                 `json:"steps_truncated,omitempty"`
	Kind                  string               `json:"kind,omitempty"`
	UserText              string               `json:"user_text,omitempty"`
	UserTextTruncated     bool                 `json:"user_text_truncated,omitempty"`
	UserRequestID         int64                `json:"user_request_id,omitempty"`
	UserImages            []promptRequestImage `json:"user_images,omitempty"`
	FinalPreview          string               `json:"final_preview,omitempty"`
	FinalPreviewTruncated bool                 `json:"final_preview_truncated,omitempty"`
	FinalStatus           string               `json:"final_status,omitempty"`
	FinalSource           string               `json:"final_source,omitempty"`
	FinalRedacted         bool                 `json:"final_redacted,omitempty"`
	DurationMS            *int64               `json:"duration_ms,omitempty"`
	TimingSource          string               `json:"timing_source,omitempty"`
}

type promptTaskUser struct {
	Text      string               `json:"text"`
	RequestID int64                `json:"request_id"`
	Images    []promptRequestImage `json:"images"`
	Source    string               `json:"source"`
}

type promptTaskStep struct {
	RequestID   int64                                  `json:"request_id"`
	CreatedAt   time.Time                              `json:"created_at"`
	Model       string                                 `json:"model"`
	Endpoint    string                                 `json:"endpoint"`
	Kind        string                                 `json:"kind,omitempty"`
	Action      string                                 `json:"action"`
	Status      int                                    `json:"status"`
	Commentary  string                                 `json:"commentary,omitempty"`
	Response    *promptTaskStepResponse                `json:"response,omitempty"`
	Tools       []promptTaskTool                       `json:"tools"`
	Skills      []promptTaskStepSkill                  `json:"skills,omitempty"`
	SkillUsage  []promptTaskStepSkillUsage             `json:"skill_usage,omitempty"`
	Delegations []database.PromptRequestTaskDelegation `json:"delegations,omitempty"`
}

type promptTaskStepSkill struct {
	Name            string `json:"name"`
	Path            string `json:"path,omitempty"`
	Source          string `json:"source"`
	SourceRequestID int64  `json:"source_request_id"`
}

type promptTaskTool struct {
	CallID         string               `json:"call_id,omitempty"`
	Name           string               `json:"name"`
	Arguments      string               `json:"arguments,omitempty"`
	Result         string               `json:"result,omitempty"`
	FromRequestID  int64                `json:"from_request_id,omitempty"`
	Status         string               `json:"status,omitempty"`
	Images         []promptRequestImage `json:"images,omitempty"`
	workDir        string
	skillUsage     *promptTaskStepSkillUsage
	skillSessionID string
	skillPollID    string
}

type promptTaskFinal struct {
	Text      string `json:"text"`
	Status    string `json:"status"`
	Source    string `json:"source"`
	Redacted  bool   `json:"redacted,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type promptTaskDetail struct {
	promptTaskSummary
	User                 *promptTaskUser                 `json:"user,omitempty"`
	Steps                []promptTaskStep                `json:"steps"`
	Skills               []promptRequestContextSection   `json:"skills"`
	ClientSteps          []database.PromptTaskClientStep `json:"client_steps,omitempty"`
	ClientStepsTruncated bool                            `json:"client_steps_truncated,omitempty"`
	Final                *promptTaskFinal                `json:"final,omitempty"`
	FinishedAt           *time.Time                      `json:"finished_at,omitempty"`
}

type promptTaskGroup struct {
	promptTaskSummary
	anchor, turn string
	userAnchorID int64
	chatTurnRoot int64
	compactions  []int64
	terminal     database.PromptRequestTaskRow
	followLatest bool
}

func groupPromptTaskRows(rows []database.PromptRequestTaskRow) []promptTaskGroup {
	groups := make([]promptTaskGroup, 0)
	for _, row := range rows {
		if len(groups) == 0 || !groups[len(groups)-1].contains(row) {
			groups = append(groups, promptTaskGroup{promptTaskSummary: promptTaskSummary{
				ID: row.ID, StartedAt: row.CreatedAt, RequestIDs: []int64{},
			}})
		}
		task := &groups[len(groups)-1]
		if task.RequestCount == 0 && (row.ChatTurnRoot != 0 || row.ChatReplayKind == database.PromptRequestChatResume) {
			task.chatTurnRoot = row.ChatTurnRoot
			task.Kind = database.PromptRequestChatResume
		}
		if row.ChatReplayKind == database.PromptRequestChatCompaction {
			task.compactions = append(task.compactions, row.ID)
		}
		hasUser := row.ChatTurnRoot == 0 && row.ChatReplayKind == ""
		if task.anchor == "" && row.Anchor != "" {
			task.anchor = row.Anchor
			if hasUser && task.Kind == "" {
				task.userAnchorID = row.ID
			}
		}
		if hasUser && task.Kind == "" && task.UserPreview == "" {
			task.UserPreview = row.Preview
		}
		if task.turn == "" {
			task.turn = row.Turn
		}
		task.RequestIDs = append(task.RequestIDs, row.ID)
		task.RequestCount++
		task.terminal = row
		task.Status = promptTaskGatewayStatus(row.Status, row.Action)
	}
	return groups
}

func (task promptTaskGroup) contains(row database.PromptRequestTaskRow) bool {
	if !row.Indexed || (task.turn != "" && row.Turn != "" && task.turn != row.Turn) {
		return false
	}
	if row.ChatTurnRoot != 0 {
		return row.ChatTurnRoot == task.ID || row.ChatTurnRoot == task.chatTurnRoot
	}
	if task.anchor != "" && row.Anchor != "" {
		return task.anchor == row.Anchor
	}
	return task.turn != "" && task.turn == row.Turn
}

func promptTaskGatewayStatus(status int, action string) string {
	switch {
	case status == 0 && action == "received":
		return "pending"
	case status == 499 || status == 598:
		return "interrupted"
	case status >= 400 || action == "error":
		return "failed"
	default:
		return "unobserved"
	}
}

func (h *Handler) ListPromptRequestTasks(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "Invalid conversation ID")
		return
	}
	before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	followLatest := before <= 0 && c.Query("live") == "1"
	if followLatest {
		id, err = h.db.LatestPromptRequestConversationID(ctx, id)
		if !writePromptTaskError(c, err) {
			return
		}
	}
	rows, err := h.readPromptTaskRows(ctx, id, max(before, 0), true, promptTaskScanLimit+1)
	if !writePromptTaskError(c, err) {
		return
	}
	hasEarlier := len(rows.Rows) > promptTaskScanLimit
	if hasEarlier {
		rows.Rows = rows.Rows[:promptTaskScanLimit]
	}
	slices.Reverse(rows.Rows)
	if !writePromptTaskError(c, h.db.ReconcilePromptRequestTaskDelegations(ctx, rows.Rows)) {
		return
	}
	groups := groupPromptTaskRows(rows.Rows)
	if followLatest && len(groups) > 0 {
		groups[len(groups)-1].followLatest = true
	}
	if hasEarlier && len(groups) > 0 {
		groups[0].StepsTruncated = true
	}
	if len(groups) > promptTaskPageSize {
		hasEarlier = true
		groups = groups[len(groups)-promptTaskPageSize:]
	}
	var next int64
	if hasEarlier && len(groups) > 0 {
		next = groups[0].ID
	}
	summaries, err := h.readPromptTaskOverview(ctx, rows.Group, groups, before)
	if !writePromptTaskError(c, err) {
		return
	}
	tasks := make([]promptTaskSummary, 0, len(summaries))
	events := make([]promptTaskSummary, 0)
	for _, task := range summaries {
		if task.Kind == promptTaskContextCompaction {
			events = append(events, task)
		} else {
			tasks = append(tasks, task)
		}
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{"tasks": tasks, "internal_events": events, "total_requests": rows.Total, "next_before": next})
}

func (h *Handler) GetPromptRequestTask(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "Invalid task ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 22*time.Second)
	defer cancel()
	id, err = h.db.PromptRequestReplayTurnID(ctx, id)
	if !writePromptTaskError(c, err) {
		return
	}
	rows, err := h.readPromptTaskRows(ctx, id, 0, false, promptTaskScanLimit+1)
	if !writePromptTaskError(c, err) {
		return
	}
	if !writePromptTaskError(c, h.db.ReconcilePromptRequestTaskDelegations(ctx, rows.Rows)) {
		return
	}
	groups := groupPromptTaskRows(rows.Rows)
	if len(groups) == 0 {
		writeError(c, http.StatusNotFound, "Request task not found")
		return
	}
	task := groups[0]
	task.StepsTruncated = len(groups) == 1 && len(rows.Rows) > promptTaskScanLimit
	var following *database.PromptRequestTaskRow
	if len(groups) > 1 {
		following = &rows.Rows[task.RequestCount]
	}
	task.followLatest = following == nil && !task.StepsTruncated
	detail, err := h.projectPromptTask(ctx, rows.Group, task, rows.Rows[:task.RequestCount], following)
	if !writePromptTaskError(c, err) {
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, detail)
}

func (h *Handler) readPromptTaskRows(ctx context.Context, id, before int64, newest bool, limit int) (database.PromptRequestTaskRows, error) {
	rows, err := h.db.ReadPromptRequestTaskRows(ctx, id, before, newest, limit)
	if err != nil {
		return rows, err
	}
	for _, row := range rows.Rows {
		if row.Indexed {
			continue
		}
		if err := h.db.IndexPromptRequestConversationTaskAnchors(ctx, id, 64); err != nil {
			return rows, err
		}
		return h.db.ReadPromptRequestTaskRows(ctx, id, before, newest, limit)
	}
	return rows, nil
}

func writePromptTaskError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(c, http.StatusNotFound, "Request task not found")
	case err != nil:
		writeInternalError(c, err)
	default:
		return true
	}
	return false
}

type promptTaskToolPosition struct {
	step, tool int
}

type promptTaskProjection struct {
	detail            *promptTaskDetail
	calls             map[string]promptTaskToolPosition
	reads             promptContextReads
	skills            map[string]bool
	loadedDirectories map[string]promptTaskSkillDirectory
	inheritedSkills   map[[sha256.Size]byte]int
	skillSessions     map[string]*promptTaskSkillSession
	delegationCounts  map[[sha256.Size]byte]int
}

func (h *Handler) projectPromptTask(ctx context.Context, conversation string, task promptTaskGroup,
	rows []database.PromptRequestTaskRow, following *database.PromptRequestTaskRow,
) (promptTaskDetail, error) {
	detail := promptTaskDetail{
		promptTaskSummary: task.promptTaskSummary,
		Steps:             make([]promptTaskStep, 0, min(len(rows), promptTaskStepLimit)),
		Skills:            []promptRequestContextSection{},
	}
	projection := promptTaskProjection{
		detail: &detail, calls: make(map[string]promptTaskToolPosition), skills: make(map[string]bool),
		reads:             promptContextReads{byCallID: make(map[string]promptContextReadCall)},
		loadedDirectories: make(map[string]promptTaskSkillDirectory),
	}
	if task.turn != "" {
		projection.skillSessions = make(map[string]*promptTaskSkillSession)
	}
	detail.StepsTruncated = detail.StepsTruncated || len(rows) > promptTaskStepLimit
	var prior database.PromptRequestBody
	var priorReply *database.PromptRequestResponse
	var terminalReply database.PromptRequestTaskResponse
	var clientInput database.PromptRequestTaskInput
	var processedBytes int
	var checkedSkillBaseline bool
	for index, row := range rows[:min(len(rows), promptTaskStepLimit)] {
		detail.Steps = append(detail.Steps, promptTaskStep{
			RequestID: row.ID, CreatedAt: row.CreatedAt, Model: row.Model, Kind: row.ChatReplayKind,
			Endpoint: row.Endpoint, Action: row.Action, Status: row.Status, Tools: []promptTaskTool{},
		})
		reply, err := h.db.GetPromptRequestResponse(ctx, row.ID)
		if err != nil {
			return detail, err
		}
		if reply != nil {
			detail.Steps[index].Commentary = boundPromptTaskText(reply.CommentaryText)
		}
		if row.ID == task.terminal.ID && reply != nil {
			terminalReply = database.PromptRequestTaskResponse{Status: reply.Status, FinishedAt: reply.FinishedAt,
				FinalObserved: reply.FinalObserved, HasToolCalls: len(reply.ToolCalls) > 0,
				UnconfirmedChatText: reply.UnconfirmedChatReply()}
		}
		if processedBytes >= promptTaskReadBytes {
			detail.StepsTruncated = true
			projection.observeDelegations(index, row.Delegations)
			if reply != nil {
				projection.observeResponse(index, reply)
			}
			continue
		}
		request, err := h.db.GetPromptRequest(ctx, row.ID)
		if err != nil {
			return detail, err
		}
		processedBytes += len(request.Body)
		input := database.DecodePromptRequestTaskUser(request.Body)
		projection.observeDelegations(index, input.Delegations)
		if detail.User == nil && input.Field != "" && detail.Kind != database.PromptRequestChatResume &&
			row.ChatTurnRoot == 0 && row.ChatReplayKind == "" {
			detail.User = promptTaskUserFromRequest(request, input)
			if detail.User.Text != input.Text {
				detail.UserPreview = ""
			}
			clientInput = input
			if isPromptTaskCompaction(request.Endpoint, request.Body, input) {
				detail.Kind = promptTaskContextCompaction
			}
		}
		injections := promptTaskSkillInjections(request.Body)
		if len(injections) > 0 && !checkedSkillBaseline {
			checkedSkillBaseline = true
			anchor := database.PromptRequestBody{PromptRequestRecord: rows[0].PromptRequestRecord, ConversationGroup: conversation}
			ids, err := h.db.PromptRequestNeighborIDs(ctx, anchor, false, 1)
			if err != nil {
				return detail, err
			}
			if len(ids) > 0 {
				baseline, err := h.db.GetPromptRequest(ctx, ids[0])
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return detail, err
				}
				projection.inheritedSkills = make(map[[sha256.Size]byte]int)
				for _, injection := range promptTaskSkillInjections(baseline.Body) {
					projection.inheritedSkills[sha256.Sum256([]byte(injection.section.Text))]++
				}
			}
		}
		projection.observeRequest(index, prior, priorReply, request, input, injections)
		if reply != nil {
			projection.observeResponse(index, reply)
		}
		prior, priorReply = request, reply
	}
	if following != nil && prior.ID != 0 && !detail.StepsTruncated {
		next, err := h.db.GetPromptRequest(ctx, following.ID)
		if err != nil {
			return detail, err
		}
		projection.observeFollowing(prior, next)
	}
	if !detail.StepsTruncated && projection.skillSessions != nil {
		projection.appendPolledSkillUsages()
	}
	for _, evidence := range projection.reads.sections {
		projection.addSkill(evidence)
	}
	if detail.Final == nil && (detail.User != nil || detail.Kind == database.PromptRequestChatResume) &&
		!detail.StepsTruncated && len(detail.Steps) > 0 {
		projection.observeLegacyChatFinal(prior, priorReply)
	}
	if len(rows) > promptTaskStepLimit {
		ids := task.RequestIDs[promptTaskStepLimit:]
		replies, err := h.db.ReadPromptRequestTaskResponses(ctx, ids, []int64{task.terminal.ID})
		if err != nil {
			return detail, err
		}
		for _, id := range ids {
			reply, ok := replies[id]
			if !ok {
				continue
			}
			observation := database.PromptRequestResponse{Status: reply.Status, FinishedAt: reply.FinishedAt,
				FinalText: reply.FinalText, FinalObserved: reply.FinalObserved, Truncated: reply.Truncated}
			if observation.FinalObserved && observation.FinalText != "" && !slices.Contains(task.compactions, id) {
				projection.setGatewayFinal(observation.FinalText, &observation)
			}
			if id == task.terminal.ID {
				terminalReply = reply
			}
		}
	}
	if detail.Kind == "" && detail.Final == nil && terminalReply.UnconfirmedChatText != "" {
		terminalBody := prior.Body
		if prior.ID != task.terminal.ID {
			bodies, err := h.db.ReadPromptRequestTaskBodies(ctx, []int64{task.terminal.ID}, promptTaskOverviewBodyBudget)
			if err != nil {
				return detail, err
			}
			terminalBody = bodies[task.terminal.ID].Body
		}
		if err := h.applyArchivedChatReply(ctx, &detail, task.terminal.PromptRequestRecord, terminalBody, terminalReply); err != nil {
			return detail, err
		}
	}
	detail.setGatewayCompletion(task.terminal.PromptRequestRecord, terminalReply, task.followLatest, clientInput.TurnStartedAt)
	if detail.User != nil && detail.Kind == "" {
		if err := h.applyPromptTaskClient(ctx, conversation, task, clientInput, &detail); err != nil {
			return detail, err
		}
	}
	return detail, nil
}

func (p *promptTaskProjection) observeDelegations(step int, delegations []database.PromptRequestTaskDelegation) {
	if len(delegations) == 0 {
		return
	}
	if p.delegationCounts == nil {
		p.delegationCounts = make(map[[sha256.Size]byte]int)
	}
	occurrences := make(map[[sha256.Size]byte]int, len(delegations))
	for _, delegation := range delegations {
		digest := sha256.Sum256([]byte(delegation.SourceThreadID + "\x00" + delegation.Text))
		occurrences[digest]++
		if occurrences[digest] <= p.delegationCounts[digest] {
			continue
		}
		p.delegationCounts[digest] = occurrences[digest]
		delegation.Text = boundPromptTaskText(delegation.Text)
		p.detail.Steps[step].Delegations = append(p.detail.Steps[step].Delegations, delegation)
	}
}

func promptTaskUserFromRequest(request database.PromptRequestBody, input database.PromptRequestTaskInput) *promptTaskUser {
	user := &promptTaskUser{
		Text:      promptTaskReplayUserText(request.Endpoint, request.Body, input),
		RequestID: request.ID, Images: []promptRequestImage{}, Source: "gateway",
	}
	if input.HasAttachment || input.Field == "prompt" {
		user.Images = append(user.Images, promptTaskUserImages(request, input)...)
	}
	return user
}

func (p *promptTaskProjection) observeResponse(step int, reply *database.PromptRequestResponse) {
	entry := &p.detail.Steps[step]
	entry.Response = previewPromptTaskStepResponse(reply)
	entry.Response.ToolOnly = safeTraceEndpoint(entry.Endpoint) == "chat" &&
		reply.Status == "completed" && entry.Response.Text == "" && len(reply.ToolCalls) > 0
	if entry.Kind == database.PromptRequestChatCompaction {
		return
	}
	p.detail.Status = reply.Status
	if p.detail.Status == "completed" && !reply.FinalObserved {
		p.detail.Status = "unobserved"
	}
	for _, call := range reply.ToolCalls {
		p.addCall(step, call.CallID, call.Name, call.Arguments)
	}
	if reply.FinalObserved && reply.FinalText != "" {
		p.setGatewayFinal(reply.FinalText, reply)
	}
}

func (p *promptTaskProjection) observeLegacyChatFinal(request database.PromptRequestBody, reply *database.PromptRequestResponse) {
	// Archived Chat Completions captured only visible choice text and refusals.
	// Other protocols mix phases in Text, and older Chat captures merged choices.
	step := p.detail.Steps[len(p.detail.Steps)-1]
	if safeTraceEndpoint(step.Endpoint) != "chat" || step.Status < 200 || step.Status >= 300 || len(step.Tools) != 0 ||
		reply == nil || reply.ChatReplyClassified || reply.FinalObserved || reply.Status != "completed" ||
		reply.Error != "" || reply.CommentaryText != "" || reply.Truncated || strings.TrimSpace(reply.Text) == "" {
		return
	}
	if choices := gjson.GetBytes(request.Body, "n"); choices.Exists() && (choices.Type != gjson.Number || choices.Num != 1) {
		return
	}
	p.setGatewayFinal(reply.Text, reply)
}

func (p *promptTaskProjection) setGatewayFinal(text string, reply *database.PromptRequestResponse) {
	p.detail.Final = &promptTaskFinal{Text: text, Status: reply.Status, Source: "gateway", Truncated: reply.Truncated}
	p.detail.FinishedAt = reply.FinishedAt
}

type promptTaskSkillInjection struct {
	index   int
	section promptRequestContextSection
}

func promptTaskSkillInjections(body []byte) []promptTaskSkillInjection {
	var injections []promptTaskSkillInjection
	add := func(index int, source, text string) {
		if !strings.Contains(text, "<skill>") {
			return
		}
		sections := appendPromptContextSections(nil, text, "", promptContextReceived, source)
		// Codex can send an actual Skill as a standalone user-role fragment.
		// Treat quoted or embedded samples in ordinary user prose as text.
		if source == "user_message" && (len(sections) != 1 || strings.TrimSpace(sections[0].Text) != strings.TrimSpace(text)) {
			return
		}
		for _, section := range sections {
			if section.Kind == promptContextSkill {
				injections = append(injections, promptTaskSkillInjection{index: index, section: section})
			}
		}
	}
	for _, field := range promptContextInstructionFields {
		add(-1, field, promptContextText(gjson.GetBytes(body, field), 0))
	}
	_, items := promptRequestToolHistory(body)
	for index, item := range items {
		switch role := item.Get("role").Str; role {
		case "developer", "system", "user":
			if kind := item.Get("type").Str; kind == "" || kind == "message" {
				add(index, role+"_message", promptContextText(item, 0))
			}
		}
	}
	return injections
}

func (p *promptTaskProjection) observeRequest(step int, prior database.PromptRequestBody, priorReply *database.PromptRequestResponse,
	request database.PromptRequestBody, input database.PromptRequestTaskInput, injections []promptTaskSkillInjection,
) {
	attributed := make(map[string]bool)
	if step > 0 && prior.ID != 0 && priorReply == nil {
		visitInferredPromptRequestToolResults(prior.Body, request.Body, func(call, receipt promptRequestToolEvent) {
			p.addCall(step-1, call.id, call.name, promptRequestToolText(call.value))
			attributed[receipt.id] = true
		})
	}

	field, items := promptRequestToolHistory(request.Body)
	source := promptToolResultSource{request: request}
	inherited := maps.Clone(p.inheritedSkills)
	observeInjections := func(through int) {
		for len(injections) > 0 && injections[0].index <= through {
			section := injections[0].section
			injections = injections[1:]
			digest := sha256.Sum256([]byte(section.Text))
			// Consume inherited occurrences in payload order so a repeated
			// activation appended to the history still supplies fresh proof.
			if inherited[digest] > 0 {
				inherited[digest]--
				continue
			}
			section.SourceRequestID = request.ID
			if p.addSkill(section) {
				p.addStepSkill(step, section, "")
			}
		}
	}
	observeInjections(max(input.Index, 0) - 1)
	for i := max(input.Index, 0); i < len(items); i++ {
		item := items[i]
		observeInjections(i)
		if i <= input.Index {
			continue
		}
		if p.detail.Final == nil {
			p.observeHistoricalFinal(item)
		}
		p.observeToolItem(step, item, fmt.Sprintf("$.%s[%d]", field, i), &source, attributed)
	}
}

func (p *promptTaskProjection) observeFollowing(prior, next database.PromptRequestBody) {
	field, after, from, to := promptTaskFollowingItems(prior.Body, next.Body)
	source := promptToolResultSource{request: next}
	findFinal := p.detail.Final == nil
	for i := from; i < to; i++ {
		if findFinal {
			p.observeHistoricalFinal(after[i])
		}
		p.observeToolItem(len(p.detail.Steps)-1, after[i], fmt.Sprintf("$.%s[%d]", field, i), &source, nil)
	}
}

func promptTaskFollowingItems(prior, next []byte) (string, []gjson.Result, int, int) {
	field, before := promptRequestToolHistory(prior)
	nextField, after := promptRequestToolHistory(next)
	input := database.DecodePromptRequestTaskUser(next)
	if field == "" || field != nextField || len(before) == 0 || input.Index <= len(before) || input.Index >= len(after) ||
		(promptfilter.IsCodexCheckpointPrompt(input.Text) && hasPromptTaskCompactionMetadata(next, input)) {
		return "", nil, 0, 0
	}
	for index := range before {
		if !samePromptRequestJSON(before[index], after[index]) {
			return "", nil, 0, 0
		}
	}
	return field, after, len(before), input.Index
}

func (p *promptTaskProjection) observeToolItem(step int, item gjson.Result, path string, source *promptToolResultSource, attributed map[string]bool) {
	visitPromptRequestToolItem(item, path, func(event promptRequestToolEvent) {
		if !event.receipt {
			if !attributed[event.id] {
				p.addCall(step, event.id, event.name, promptRequestToolText(event.value))
			}
			return
		}
		position, exists := p.calls[event.id]
		if !exists {
			p.addCall(step, event.id, event.name, "")
			position = p.calls[event.id]
		}
		tool := &p.detail.Steps[position.step].Tools[position.tool]
		if tool.FromRequestID == 0 {
			p.setToolResult(position.step, tool, source.result(event, tool.Name, ""))
		}
	})
}

func (p *promptTaskProjection) observeHistoricalFinal(item gjson.Result) {
	if final := promptTaskHistoricalFinal(item); final != nil {
		p.detail.Final = final
	}
}

func promptTaskHistoricalFinal(item gjson.Result) *promptTaskFinal {
	if item.Get("role").Str != "assistant" || item.Get("phase").Str != "final_answer" {
		return nil
	}
	if text := promptContextText(item, 0); text != "" {
		return &promptTaskFinal{Text: boundPromptTaskText(text), Status: "completed", Source: "gateway", Truncated: len(text) > promptTaskToolTextLimit}
	}
	return nil
}

func (p *promptTaskProjection) addCall(step int, id, name, arguments string) {
	if _, exists := p.calls[id]; exists && id != "" {
		return
	}
	tools := &p.detail.Steps[step].Tools
	if id != "" {
		p.calls[id] = promptTaskToolPosition{step, len(*tools)}
	}
	command, workDir := promptContextToolCommand(name, arguments)
	tool := promptTaskTool{
		CallID: id, Name: name, Arguments: boundPromptTaskText(arguments), Status: "called",
		workDir: workDir, skillUsage: p.matchSkillUsage(command, workDir, p.detail.Steps[step].RequestID),
	}
	p.beginSkillPoll(&tool, arguments, p.detail.Steps[step].RequestID)
	*tools = append(*tools, tool)
	p.reads.addCall(id, name, arguments, promptContextReceived)
}

func (p *promptTaskProjection) setToolResult(step int, tool *promptTaskTool, result promptRequestToolResult) {
	tool.Status = "returned"
	if result.failed {
		tool.Status = "failed"
	}
	tool.Result = boundPromptTaskText(result.Text)
	tool.FromRequestID, tool.Images = result.FromRequestID, result.Images
	p.observeSkillSession(tool, result, p.detail.Steps[step].RequestID)
	if usage := tool.skillUsage; usage != nil && !result.failed && result.FromRequestID > 0 {
		if evidence := promptTaskSkillUsageReceipt(tool.Result); evidence != "" {
			usage.SourceRequestID, usage.Evidence = result.FromRequestID, evidence
			p.detail.Steps[step].SkillUsage = append(p.detail.Steps[step].SkillUsage, *usage)
		}
	}
	if skill, ok := promptTaskWorkBuddySkill(p.detail.Steps[step].Endpoint, tool, result); ok {
		p.addSkill(skill)
		p.addStepSkill(step, skill, tool.workDir)
	}
	p.reads.addReceipt(tool.CallID, result.Text, promptContextReceived, result.FromRequestID, result.failed)
	for _, index := range p.reads.byCallID[tool.CallID].sections {
		p.addStepSkill(step, p.reads.sections[index], tool.workDir)
	}
}

func (p *promptTaskProjection) addStepSkill(step int, evidence promptRequestContextSection, workDir string) {
	if evidence.Kind != promptContextSkill || evidence.Name == "" || evidence.SourceRequestID <= 0 {
		return
	}
	skill := promptTaskStepSkill{
		Name: evidence.Name, Path: evidence.Path, Source: evidence.Source, SourceRequestID: evidence.SourceRequestID,
	}
	entry := &p.detail.Steps[step]
	if !slices.Contains(entry.Skills, skill) {
		entry.Skills = append(entry.Skills, skill)
	}
	p.rememberLoadedSkill(evidence, workDir)
}

func (p *promptTaskProjection) addSkill(evidence promptRequestContextSection) bool {
	if evidence.Kind != promptContextSkill && evidence.Kind != promptContextAttempt {
		return false
	}
	key := evidence.Kind + "\x00" + evidence.Path + "\x00" + evidence.Name + "\x00" + evidence.Evidence
	if p.skills[key] {
		return false
	}
	p.skills[key] = true
	evidence.Text = boundPromptTaskText(evidence.Text)
	p.detail.Skills = append(p.detail.Skills, evidence)
	return true
}

func (detail *promptTaskDetail) applyClient(snapshot *database.PromptTaskClientSnapshot) {
	detail.ClientSteps, detail.ClientStepsTruncated = snapshot.Steps, snapshot.StepsTruncated
	detail.Status = snapshot.Status
	detail.Final = nil
	if snapshot.FinalText != "" {
		detail.Final = &promptTaskFinal{Text: snapshot.FinalText, Status: snapshot.Status,
			Source: "client", Redacted: snapshot.FinalRedacted, Truncated: snapshot.FinalTruncated}
	}
	if snapshot.FinishedAt != nil {
		detail.setTiming(snapshot.StartedAt, *snapshot.FinishedAt, "client")
	}
}

func (detail *promptTaskDetail) setTiming(start, end time.Time, source string) {
	if end.IsZero() || start.IsZero() || end.Before(start) {
		return
	}
	duration := end.Sub(start).Milliseconds()
	detail.StartedAt, detail.FinishedAt = start, &end
	detail.DurationMS, detail.TimingSource = &duration, source
}

func boundPromptTaskText(text string) string {
	if len(text) <= promptTaskToolTextLimit {
		return text
	}
	bound := promptTaskToolTextLimit - 3
	for bound > 0 && text[bound]&0xc0 == 0x80 {
		bound--
	}
	return strings.Clone(text[:bound]) + "…"
}
