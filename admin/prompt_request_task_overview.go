package admin

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/tidwall/gjson"
)

const (
	promptTaskContextCompaction  = "context_compaction"
	promptTaskOverviewBodyBudget = 64 << 20
	promptTaskUserTextLimit      = 64 << 10
	promptTaskFinalPreviewLimit  = 8 << 10
	promptTaskToolContinuation   = 2 * time.Minute
	promptTaskPendingWindow      = 30 * time.Minute
)

func (h *Handler) readPromptTaskOverview(ctx context.Context, conversation string, groups []promptTaskGroup, before int64) ([]promptTaskSummary, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	var ids []int64
	terminalIDs := make([]int64, 0, len(groups))
	for _, task := range groups {
		ids = append(ids, task.RequestIDs...)
		terminalIDs = append(terminalIDs, task.terminal.ID)
	}
	replies, err := h.db.ReadPromptRequestTaskResponses(ctx, ids, terminalIDs)
	if err != nil {
		return nil, err
	}

	details := make([]promptTaskDetail, len(groups))
	bodyIDs := make([]int64, 0, 2*len(groups))
	var bodySeen = make(map[int64]bool)
	appendBody := func(id int64) {
		if id > 0 && !bodySeen[id] {
			bodyIDs = append(bodyIDs, id)
			bodySeen[id] = true
		}
	}
	// Check compaction candidates even if a later long conversation consumes
	// the body budget. The preview is only a hint; metadata must also agree.
	for _, task := range groups {
		if strings.HasPrefix(task.UserPreview, "You are performing a CONTEXT CHECKPOINT COMPACTION.") {
			appendBody(promptTaskUserAnchor(task))
		}
	}
	for i, task := range groups {
		appendBody(promptTaskUserAnchor(task))
		details[i].promptTaskSummary = task.promptTaskSummary
		for _, id := range task.RequestIDs {
			if reply := replies[id]; reply.FinalObserved && reply.FinalText != "" && !slices.Contains(task.compactions, id) {
				details[i].Final = &promptTaskFinal{Text: reply.FinalText, Status: reply.Status, Source: "gateway", Truncated: reply.Truncated}
				details[i].FinishedAt = reply.FinishedAt
			}
		}
	}
	for i, task := range groups {
		if details[i].Final == nil && !task.StepsTruncated {
			appendBody(task.terminal.ID)
			appendBody(promptTaskNextAnchor(groups, i, before, conversation))
		}
	}
	bodies, err := h.db.ReadPromptRequestTaskBodies(ctx, bodyIDs, promptTaskOverviewBodyBudget)
	if err != nil {
		return nil, err
	}

	result := make([]promptTaskSummary, 0, len(groups))
	for i, task := range groups {
		detail := &details[i]
		anchor := bodies[promptTaskUserAnchor(task)]
		input := database.DecodePromptRequestTaskUser(anchor.Body)
		terminal := replies[task.terminal.ID]
		switch {
		case isPromptTaskCompaction(task.terminal.Endpoint, anchor.Body, input):
			detail.Kind = promptTaskContextCompaction
			detail.UserPreview = ""
		case detail.Kind == database.PromptRequestChatResume:
			detail.UserPreview = ""
		case input.Field != "":
			detail.UserRequestID = anchor.ID
			text := promptTaskReplayUserText(task.terminal.Endpoint, anchor.Body, input)
			detail.UserText, detail.UserTextTruncated = limitPromptTaskPreview(text, promptTaskUserTextLimit)
			if text != input.Text {
				detail.UserPreview = ""
			}
			if input.HasAttachment || input.Field == "prompt" {
				detail.UserImages = promptTaskUserImages(anchor, input)
			}
		case task.UserPreview != "":
			detail.UserText, detail.UserTextTruncated = task.UserPreview, true
		}

		if detail.Kind == "" && detail.Final == nil && input.Field != "" && !task.StepsTruncated {
			last := bodies[task.terminal.ID]
			next := bodies[promptTaskNextAnchor(groups, i, before, conversation)]
			detail.Final = promptTaskOverviewHistoricalFinal(anchor, last, next)
			if detail.Final == nil && terminal.LegacyChatFinalText != "" && isPromptTaskSingleChatReply(task.terminal.PromptRequestRecord, last.Body) &&
				!promptTaskHasCurrentToolItems(last.Body) {
				detail.Final = &promptTaskFinal{Text: terminal.LegacyChatFinalText, Status: "completed", Source: "gateway"}
				detail.FinishedAt = terminal.FinishedAt
			}
		}
		if detail.Kind == "" && detail.Final == nil {
			if err := h.applyArchivedChatReply(ctx, detail, task.terminal.PromptRequestRecord, bodies[task.terminal.ID].Body, terminal); err != nil {
				return nil, err
			}
		}
		detail.setGatewayCompletion(task.terminal.PromptRequestRecord, terminal, task.followLatest, input.TurnStartedAt)
		if detail.Kind == "" && input.Field != "" {
			if err := h.applyPromptTaskClient(ctx, conversation, task, input, detail); err != nil {
				return nil, err
			}
		}
		if detail.Final != nil {
			detail.FinalPreview, detail.FinalPreviewTruncated = limitPromptTaskPreview(detail.Final.Text, promptTaskFinalPreviewLimit)
			detail.FinalPreviewTruncated = detail.FinalPreviewTruncated || detail.Final.Truncated
			detail.FinalSource, detail.FinalStatus, detail.FinalRedacted = detail.Final.Source, detail.Final.Status, detail.Final.Redacted
		}
		result = append(result, detail.promptTaskSummary)
	}
	return result, nil
}

func promptTaskUserAnchor(task promptTaskGroup) int64 {
	if task.Kind == database.PromptRequestChatResume {
		return 0
	}
	if task.userAnchorID > 0 {
		return task.userAnchorID
	}
	return task.ID
}

func promptTaskNextAnchor(groups []promptTaskGroup, index int, before int64, conversation string) int64 {
	if index+1 < len(groups) {
		return groups[index+1].ID
	}
	// The pagination query checked the boundary exists in this conversation.
	if conversation != "" && conversation != "single" {
		return before
	}
	return 0
}

func isPromptTaskCompaction(endpoint string, body []byte, input database.PromptRequestTaskInput) bool {
	if (safeTraceEndpoint(endpoint) != "responses" && safeTraceEndpoint(endpoint) != "responses_compact") ||
		input.Field != "input" || input.Index < 0 || !promptfilter.IsCodexCheckpointPrompt(input.Text) {
		return false
	}
	return hasPromptTaskCompactionMetadata(body, input)
}

func hasPromptTaskCompactionMetadata(body []byte, input database.PromptRequestTaskInput) bool {
	outer := gjson.GetBytes(body, "client_metadata")
	metadata := outer.Get("x-codex-turn-metadata")
	if metadata.Type == gjson.String && len(metadata.Str) <= 16<<10 && gjson.Valid(metadata.Str) {
		metadata = gjson.Parse(metadata.Str)
	}
	if !metadata.IsObject() || input.TurnID == "" || metadata.Get("turn_id").Str != input.TurnID || metadata.Get("request_kind").Str != "compaction" {
		return false
	}
	thread := outer.Get("thread_id").Str
	if thread == "" {
		thread = outer.Get("session_id").Str
	}
	if thread == "" || (metadata.Get("thread_id").Str != thread && metadata.Get("session_id").Str != thread) {
		return false
	}
	compaction := metadata.Get("compaction")
	if !compaction.IsObject() || compaction.Get("implementation").Str != "responses_compaction_v2" ||
		compaction.Get("phase").Str != "pre_turn" || compaction.Get("reason").Str == "" || compaction.Get("strategy").Str == "" || compaction.Get("trigger").Str == "" {
		return false
	}
	field, items := promptRequestToolHistory(body)
	if field != "input" || input.Index != len(items)-1 {
		return false
	}
	item := items[input.Index]
	parts := item.Get("content").Array()
	return item.Get("type").Str == "message" && len(parts) == 1 && parts[0].Get("type").Str == "input_text"
}

func promptTaskOverviewHistoricalFinal(anchor, last, next database.PromptRequestBody) *promptTaskFinal {
	for _, request := range []database.PromptRequestBody{anchor, last} {
		input := database.DecodePromptRequestTaskUser(request.Body)
		if input.Index < 0 || len(request.Body) == 0 {
			continue
		}
		field, items := promptRequestToolHistory(request.Body)
		if field != input.Field || input.Index >= len(items) {
			continue
		}
		for _, item := range items[input.Index+1:] {
			if final := promptTaskHistoricalFinal(item); final != nil {
				return final
			}
		}
	}
	if len(last.Body) == 0 || len(next.Body) == 0 {
		return nil
	}
	_, items, from, to := promptTaskFollowingItems(last.Body, next.Body)
	for _, item := range items[from:to] {
		if final := promptTaskHistoricalFinal(item); final != nil {
			return final
		}
	}
	return nil
}

func promptTaskHasCurrentToolItems(body []byte) bool {
	input := database.DecodePromptRequestTaskUser(body)
	field, items := promptRequestToolHistory(body)
	if field != input.Field || input.Index < 0 || input.Index >= len(items) {
		return false
	}
	for _, item := range items[input.Index+1:] {
		found := false
		visitPromptRequestToolItem(item, "", func(promptRequestToolEvent) { found = true })
		if found {
			return true
		}
	}
	return false
}

func (h *Handler) applyArchivedChatReply(ctx context.Context, detail *promptTaskDetail, terminal database.PromptRequestRecord,
	body []byte, reply database.PromptRequestTaskResponse,
) error {
	if reply.UnconfirmedChatText == "" || !isPromptTaskSingleChatReply(terminal, body) {
		return nil
	}
	confirmed, err := h.db.PromptRequestChatReplyEchoed(ctx, terminal.ID, body, reply.UnconfirmedChatText)
	if confirmed {
		detail.Final = &promptTaskFinal{Text: reply.UnconfirmedChatText, Status: "completed", Source: "gateway"}
		detail.FinishedAt = reply.FinishedAt
	}
	return err
}

func isPromptTaskSingleChatReply(row database.PromptRequestRecord, body []byte) bool {
	if safeTraceEndpoint(row.Endpoint) != "chat" || row.Status < 200 || row.Status >= 300 || len(body) == 0 {
		return false
	}
	choices := gjson.GetBytes(body, "n")
	return !choices.Exists() || (choices.Type == gjson.Number && choices.Num == 1)
}

func (h *Handler) applyPromptTaskClient(ctx context.Context, group string, task promptTaskGroup, input database.PromptRequestTaskInput, detail *promptTaskDetail) error {
	if input.TurnID == "" {
		return nil
	}
	unique, err := h.db.PromptRequestTaskTurnIsUnambiguous(ctx, group, task.turn)
	if err != nil || !unique {
		return err
	}
	snapshot, err := h.db.MatchPromptTaskClient(ctx, group, input.TurnID, input.Text, task.StartedAt)
	if snapshot != nil {
		detail.applyClient(snapshot)
	}
	return err
}

func (detail *promptTaskDetail) setGatewayCompletion(row database.PromptRequestRecord, reply database.PromptRequestTaskResponse, followLatest bool, clientStart *time.Time) {
	detail.Status = promptTaskGatewayStatus(row.Status, row.Action)
	if detail.Status != "interrupted" && detail.Status != "failed" && reply.Status != "" {
		detail.Status = reply.Status
		if reply.Status == "completed" && !reply.FinalObserved {
			detail.Status = "unobserved"
		}
	}
	if detail.Status == "pending" && !recentPromptTaskActivity(row.CreatedAt, promptTaskPendingWindow) {
		detail.Status = "unobserved"
	}
	if followLatest && detail.Status == "unobserved" && detail.Final == nil &&
		reply.Status == "completed" && reply.HasToolCalls && reply.FinishedAt != nil &&
		recentPromptTaskActivity(*reply.FinishedAt, promptTaskToolContinuation) {
		// The next gateway call can arrive after the client runs the tool.
		// Keep that gap active without leaving older tool-only turns pending.
		detail.Status = "pending"
	}
	switch detail.Status {
	case "interrupted", "failed":
		detail.FinishedAt = reply.FinishedAt
	case "pending":
		detail.FinishedAt = nil
	default:
		if detail.Final != nil {
			detail.Status = detail.Final.Status
		}
	}
	if detail.FinishedAt == nil || (detail.Final == nil && detail.Status != "interrupted" && detail.Status != "failed") {
		return
	}
	start := detail.StartedAt
	if clientStart != nil && !clientStart.After(start) && start.Sub(*clientStart) <= 24*time.Hour {
		start = *clientStart
	}
	detail.setTiming(start, *detail.FinishedAt, "gateway_observed")
}

func recentPromptTaskActivity(at time.Time, window time.Duration) bool {
	age := time.Since(at)
	return age >= 0 && age <= window
}

func limitPromptTaskPreview(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	limit := maxBytes - len("…")
	for limit > 0 && text[limit]&0xc0 == 0x80 {
		limit--
	}
	return strings.Clone(text[:limit]) + "…", true
}

func promptTaskUserImages(request database.PromptRequestBody, input database.PromptRequestTaskInput) []promptRequestImage {
	var images []promptRequestImage
	// The image index cannot match, so the presentation enumerates metadata
	// without retaining any image bytes or formatting the full request body.
	for _, image := range presentPromptRequest(request, math.MaxInt).Images {
		field, position := promptRequestImageLocation(image.Label)
		if (field == input.Field && position == input.Index) ||
			(input.Field == "prompt" && (field == "image" || field == "image_url" || field == "images" || field == "input_images")) {
			images = append(images, image)
		}
	}
	return images
}
