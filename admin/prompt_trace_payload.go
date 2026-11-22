package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/codex2api/database"
)

type otlpTracePayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource    `json:"resource"`
	ScopeSpans []otlpScopeSpan `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpScopeSpan struct {
	Spans []otlpSpan `json:"spans"`
}

type otlpSpan struct {
	TraceID      string          `json:"traceId"`
	SpanID       string          `json:"spanId"`
	ParentSpanID string          `json:"parentSpanId,omitempty"`
	Name         string          `json:"name"`
	Kind         int             `json:"kind"`
	Start        string          `json:"startTimeUnixNano"`
	End          string          `json:"endTimeUnixNano"`
	Attributes   []otlpAttribute `json:"attributes"`
	Status       *otlpSpanStatus `json:"status,omitempty"`
}

type otlpSpanStatus struct {
	Code int `json:"code"`
}

type otlpAttribute struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func otlpAttr(key, value string) otlpAttribute {
	return otlpAttribute{Key: key, Value: map[string]any{"stringValue": value}}
}

func otlpJSON(key string, value any) otlpAttribute {
	encoded, _ := json.Marshal(value)
	return otlpAttr(key, string(encoded))
}

func otlpTags(tags []string) otlpAttribute {
	values := make([]map[string]string, 0, len(tags))
	for _, tag := range tags {
		values = append(values, map[string]string{"stringValue": tag})
	}
	return otlpAttribute{Key: "langfuse.trace.tags", Value: map[string]any{"arrayValue": map[string]any{"values": values}}}
}

func traceHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		// JSON strings keep arbitrary local metadata separate from domain labels.
		encoded, _ := json.Marshal(part)
		_, _ = h.Write(encoded)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func safeTraceEndpoint(endpoint string) string {
	switch endpoint {
	case "/v1/responses", "/responses", "/backend-api/codex/responses", "/codex/responses":
		return "responses"
	case "/v1/responses/compact", "/responses/compact":
		return "responses_compact"
	case "/v1/chat/completions", "/chat/completions":
		return "chat"
	case "/v1/messages", "/messages":
		return "messages"
	case "/v1/messages/count_tokens", "/messages/count_tokens", "/v1/responses/input_tokens", "/responses/input_tokens":
		return "count_tokens"
	case "/v1/images/generations", "/images/generations", "/v1/images/edits", "/images/edits":
		return "images"
	case "/v1/videos/generations", "/videos/generations", "/v1/videos/edits", "/videos/edits", "/v1/videos/extensions", "/videos/extensions":
		return "videos"
	case "/v1/alpha/search", "/alpha/search", "/backend-api/codex/alpha/search":
		return "search"
	case "/v1/live", "/live":
		return "live"
	default:
		return "other"
	}
}

func safeTraceTool(name string) string {
	switch name {
	case "exec", "exec_command", "shell", "local_shell", "write_stdin", "bash":
		return "shell"
	case "apply_patch":
		return "patch"
	case "web", "web_search", "search", "retrieve":
		return "search"
	case "spawn_agent", "send_input", "wait", "collaboration.spawn_agent":
		return "agent"
	case "view_image", "imagegen", "image_gen":
		return "image"
	default:
		return "other"
	}
}

func safeTraceSkill(name string) string {
	switch name {
	case "imagegen", "openai-docs", "skill-creator", "skill-installer", "pdf", "spreadsheets", "presentations",
		"documents", "repo-review", "skill-developer", "github":
		return name
	default:
		return ""
	}
}

func traceFilterMetadata(prefix string, fields map[string]any) []otlpAttribute {
	attrs := make([]otlpAttribute, 0, len(fields))
	for key, value := range fields {
		attrs = append(attrs, otlpJSON(prefix+key, value))
	}
	return attrs
}

func promptTraceContextCounts(view promptRequestPresentation, tags []string) (map[string]int, []string) {
	counts := make(map[string]int)
	loaded := make(map[string]bool)
	for _, section := range view.PromptContext.Sections {
		switch section.Kind {
		case "system_prompt", "developer_prompt", "agents_md", "available_skills", "loaded_skill", "attempted_skill":
		default:
			continue
		}
		switch section.State {
		case "new", "inherited", "received":
		default:
			continue
		}
		key := string(section.Kind) + "_" + string(section.State)
		counts[key]++
		switch section.Kind {
		case "available_skills":
			counts["available_skill_entries"] += len(section.Skills)
		case "loaded_skill":
			if name := safeTraceSkill(section.Name); name != "" && !loaded[name] {
				loaded[name] = true
				tags = append(tags, "skill-loaded:"+name)
			}
		}
	}
	if counts["loaded_skill_new"]+counts["loaded_skill_inherited"]+counts["loaded_skill_received"] > 0 {
		tags = append(tags, "skill-loaded")
	}
	return counts, tags
}

// buildPromptTrace is the only outbound projection. It accepts local analysis
// and copies only explicit, typed counters and enums into the OTLP payload.
func buildPromptTrace(destination, sourceURL string, request database.PromptRequestBody, view promptRequestPresentation,
	job *database.PromptRequestTraceJob, analysisSkipped bool,
) (string, otlpTracePayload) {
	id := strconv.FormatInt(request.ID, 10)
	traceID := traceHash("codex2api/request/v1", destination, id, request.CreatedAt.UTC().Format(time.RFC3339Nano))[:32]
	traceName := "Codex2API #" + id
	start, end := request.CreatedAt, job.FinishedAt
	archived := end.IsZero()
	if end.Before(start) {
		end = start
	}
	tags := []string{"codex2api"}
	sourcePath := "/admin/prompt-filter/requests?request=" + id
	metadata := map[string]any{
		"request_id":  request.ID,
		"source_path": sourcePath,
		"endpoint":    safeTraceEndpoint(request.Endpoint),
		"archived":    archived,
	}
	if sourceURL != "" {
		metadata["source_url"] = sourceURL + sourcePath
	}
	switch request.Transport {
	case "http", "websocket":
		metadata["transport"] = request.Transport
	}
	if model := database.SafePromptTraceModel(request.Model); model != "" {
		metadata["requested_model"] = model
	}
	if !archived {
		metadata["duration_ms"] = end.Sub(start).Milliseconds()
	}
	if analysisSkipped {
		metadata["analysis_skipped"] = "request_size"
	} else {
		metadata["image_count"] = len(view.Images)
	}
	if request.Status >= 100 && request.Status <= 599 {
		metadata["http_status"] = request.Status
	}
	switch request.Action {
	case "allow", "block", "warn", "error", "received":
		metadata["gateway_action"] = request.Action
	}
	tags = addPromptTraceChanges(view.Changes, metadata, tags)
	contextCounts, tags := promptTraceContextCounts(view, tags)
	for key, count := range contextCounts {
		metadata[key] = count
	}

	var toolCalls []database.PromptRequestResponseCall
	output := map[string]any{"response_status": "not_recorded"}
	statusCode := 0
	if view.Result != nil {
		output["attributed_tool_results"] = len(view.Result.ToolResults)
		if response := view.Result.Response; response != nil {
			toolCalls = response.ToolCalls
			output["response_bytes"] = len(response.Text)
			output["tool_calls"] = len(toolCalls)
			output["truncated"] = response.Truncated
			switch response.Status {
			case "completed":
				output["response_status"], statusCode = response.Status, 1
			case "interrupted", "failed":
				output["response_status"], statusCode = response.Status, 2
			}
		}
	}
	if statusCode == 0 && request.Status >= 400 {
		statusCode = 2
	}
	metadata["response_status"] = output["response_status"]
	if output["response_status"] == "interrupted" {
		tags = append(tags, "stream-interrupted")
	}
	if !archived {
		metadata["observed_model_attempts"] = job.Metrics.TotalAttempts
	}
	if job.Metrics.TotalAttempts > len(job.Metrics.Attempts) {
		metadata["model_attempts_omitted"] = job.Metrics.TotalAttempts - len(job.Metrics.Attempts)
	}
	common := []otlpAttribute{otlpAttr("langfuse.trace.name", traceName), otlpTags(tags)}
	if group := request.ConversationGroup; group != "" && group != "single" {
		common = append(common, otlpAttr("session.id", traceHash("codex2api/session/v1", destination, group)[:32]))
	}
	common = append(common, traceFilterMetadata("langfuse.trace.metadata.", metadata)...)
	rootID := traceHash("gateway", traceID)[:16]
	builder := promptTraceBuilder{traceID: traceID, rootID: rootID, start: start, end: end, shared: common}
	root := builder.span("gateway request", rootID, "", "span", nil, start, end, statusCode)
	root.Attributes = append(root.Attributes, otlpJSON("langfuse.observation.input", metadata), otlpJSON("langfuse.observation.output", output))
	spans := []otlpSpan{root}
	for index, attempt := range job.Metrics.Attempts {
		spans = append(spans, builder.model(index, attempt))
	}
	spans = append(spans, builder.tools(request, view.Changes, toolCalls)...)
	return traceID, otlpTracePayload{ResourceSpans: []otlpResourceSpans{{
		Resource:   otlpResource{Attributes: []otlpAttribute{otlpAttr("service.name", "codex2api")}},
		ScopeSpans: []otlpScopeSpan{{Spans: spans}},
	}}}
}

func addPromptTraceChanges(changes *promptRequestChanges, metadata map[string]any, tags []string) []string {
	if changes == nil {
		metadata["has_history_baseline"] = false
		return tags
	}
	metadata["has_history_baseline"] = true
	metadata["previous_request_id"] = changes.PreviousID
	metadata["historical_items"] = changes.RepeatedItems
	metadata["new_items"] = changes.NewItems
	metadata["historical_images"] = changes.HistoricalImages
	metadata["new_images"] = len(changes.ImageIndices)
	newUserText := 0
	for _, change := range changes.Entries {
		if change.Kind == "text" && change.Label == "user" {
			newUserText++
		}
	}
	metadata["new_user_text_parts"] = newUserText
	if changes.HistoricalImages > 0 {
		tags = append(tags, "repeated-images")
	}
	return tags
}

type promptTraceBuilder struct {
	traceID string
	rootID  string
	start   time.Time
	end     time.Time
	shared  []otlpAttribute
}

func (b promptTraceBuilder) span(name, id, parent, observationType string, attrs []otlpAttribute,
	start, end time.Time, status int,
) otlpSpan {
	all := make([]otlpAttribute, 0, len(b.shared)+len(attrs)+1)
	all = append(all, b.shared...)
	all = append(all, otlpAttr("langfuse.observation.type", observationType))
	result := otlpSpan{
		TraceID: b.traceID, SpanID: id, ParentSpanID: parent, Name: name, Kind: 1,
		Start: strconv.FormatInt(start.UnixNano(), 10), End: strconv.FormatInt(end.UnixNano(), 10),
		Attributes: append(all, attrs...),
	}
	if status != 0 {
		result.Status = &otlpSpanStatus{Code: status}
	}
	return result
}

func (b promptTraceBuilder) model(index int, attempt database.PromptRequestTraceAttempt) otlpSpan {
	id := traceHash("model", b.traceID, strconv.Itoa(index))[:16]
	end := attempt.FinishedAt
	if end.Before(b.start) || end.After(b.end) {
		end = b.end
	}
	start := end.Add(-time.Duration(max(0, attempt.DurationMs)) * time.Millisecond)
	if start.Before(b.start) || start.After(end) {
		start = b.start
	}
	metadata := map[string]any{"retry": attempt.Retry, "stream": attempt.Stream}
	status := 0
	if attempt.StatusCode >= 100 && attempt.StatusCode <= 599 {
		metadata["http_status"] = attempt.StatusCode
		switch {
		case attempt.StatusCode >= 400:
			status = 2
		case attempt.StatusCode >= 200 && attempt.StatusCode < 300:
			status = 1
		}
	}
	if attempt.DurationMs > 0 {
		metadata["duration_ms"] = attempt.DurationMs
	}
	attrs := traceFilterMetadata("langfuse.observation.metadata.", metadata)
	if model := database.SafePromptTraceModel(attempt.Model); model != "" {
		attrs = append(attrs, otlpAttr("langfuse.observation.model.name", model))
	}
	if attempt.FirstTokenMs > 0 && attempt.FirstTokenMs <= attempt.DurationMs {
		first := start.Add(time.Duration(attempt.FirstTokenMs) * time.Millisecond)
		if !first.After(end) {
			attrs = append(attrs, otlpAttr("langfuse.observation.completion_start_time", first.UTC().Format(time.RFC3339Nano)))
		}
	}
	if usage := promptTraceTokenUsage(attempt); len(usage) > 0 {
		attrs = append(attrs, otlpJSON("langfuse.observation.usage_details", usage))
	}
	return b.span(fmt.Sprintf("model attempt %d", index+1), id, b.rootID, "generation", attrs, start, end, status)
}

// Gateway usage normalizes inclusive input/output totals. Split cache writes,
// cache reads and reasoning into disjoint buckets for Langfuse.
func promptTraceTokenUsage(attempt database.PromptRequestTraceAttempt) map[string]int {
	const maxTokens = 100_000_000
	input := min(max(attempt.InputTokens, 0), maxTokens)
	output := min(max(attempt.OutputTokens, 0), maxTokens)
	usage := make(map[string]int)
	if input+output == 0 {
		if attempt.TotalTokens > 0 && attempt.TotalTokens <= maxTokens {
			usage["total"] = attempt.TotalTokens
		}
		return usage
	}
	cache := min(max(attempt.CachedTokens, 0), input)
	write5m := min(max(attempt.CacheWrite5m, 0), input-cache)
	write1h := min(max(attempt.CacheWrite1h, 0), input-cache-write5m)
	reasoning := min(max(attempt.ReasoningTokens, 0), output)
	usage["input"] = input - cache - write5m - write1h
	usage["output"] = output - reasoning
	usage["total"] = input + output
	for key, n := range map[string]int{
		"input_cached_tokens": cache, "input_cache_write_5m": write5m, "input_cache_write_1h": write1h,
		"output_reasoning_tokens": reasoning,
	} {
		if n > 0 {
			usage[key] = n
		}
	}
	return usage
}

func (b promptTraceBuilder) tools(request database.PromptRequestBody, changes *promptRequestChanges,
	calls []database.PromptRequestResponseCall,
) []otlpSpan {
	const maxToolObservations = 24
	spans := make([]otlpSpan, 0, min(len(calls), maxToolObservations))
	for index, call := range calls[:min(len(calls), maxToolObservations)] {
		id := traceHash("sent-tool", b.traceID, strconv.Itoa(index))[:16]
		attrs := []otlpAttribute{otlpAttr("langfuse.observation.metadata.tool_category", safeTraceTool(call.Name))}
		spans = append(spans, b.span("model tool call", id, b.rootID, "tool", attrs, b.end, b.end, 0))
	}
	if changes != nil {
		for index, item := range changes.Entries {
			if len(spans) >= maxToolObservations {
				break
			}
			var label string
			switch item.Kind {
			case "tool_call":
				label = "received tool call context"
			case "tool_result":
				label = "received tool result"
			default:
				continue
			}
			metadata := map[string]any{"received_request_id": request.ID, "tool_category": safeTraceTool(item.Label)}
			if item.ResultRequestID > 0 {
				metadata["origin_request_id"] = item.ResultRequestID
			}
			id := traceHash("received-tool", b.traceID, strconv.Itoa(index))[:16]
			attrs := traceFilterMetadata("langfuse.observation.metadata.", metadata)
			spans = append(spans, b.span(label, id, b.rootID, "tool", attrs, b.start, b.start, 0))
		}
	}
	return spans
}
