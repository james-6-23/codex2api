package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

const (
	promptResultNeighbors = 8
	promptResultReadLimit = 64 << 20
)

type promptRequestResult struct {
	Response    *database.PromptRequestResponse `json:"response,omitempty"`
	ToolResults []promptRequestToolResult       `json:"tool_results"`
}

type promptRequestToolResult struct {
	CallID        string               `json:"call_id"`
	Name          string               `json:"name,omitempty"`
	Text          string               `json:"text"`
	FromRequestID int64                `json:"from_request_id"`
	Arguments     string               `json:"arguments,omitempty"`
	Images        []promptRequestImage `json:"images,omitempty"`
	failed        bool
}

type promptRequestToolEvent struct {
	id, name string
	value    gjson.Result
	path     string
	receipt  bool
	failed   bool
}

func (h *Handler) requestResult(ctx context.Context, request database.PromptRequestBody) *promptRequestResult {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	response, err := h.db.GetPromptRequestResponse(ctx, request.ID)
	if err != nil {
		log.Printf("[prompt-requests] response lookup for %d failed: %T", request.ID, err)
		return nil
	}
	result := &promptRequestResult{Response: response, ToolResults: []promptRequestToolResult{}}
	limit := 1
	pending := make(map[string]string)
	if response != nil {
		limit = promptResultNeighbors
		for _, call := range response.ToolCalls {
			if call.CallID != "" {
				pending[call.CallID] = call.Name
			}
		}
		if len(pending) == 0 {
			return result
		}
		visitPromptRequestTools(request.Body, func(event promptRequestToolEvent) {
			if event.receipt {
				delete(pending, event.id)
			}
		})
		if len(pending) == 0 {
			return result
		}
	}
	ids, err := h.db.PromptRequestNeighborIDs(ctx, request, true, limit)
	if err != nil {
		log.Printf("[prompt-requests] subsequent requests lookup for %d failed: %T", request.ID, err)
	}
	var readBytes int
	for _, id := range ids {
		next, err := h.db.GetPromptRequest(ctx, id)
		if err != nil {
			break
		}
		readBytes += len(next.Body)
		if readBytes > promptResultReadLimit || ctx.Err() != nil {
			break
		}
		if response == nil {
			result.ToolResults = inferPromptRequestToolResults(request.Body, next)
			break
		}
		source := promptToolResultSource{request: next}
		visitPromptRequestTools(next.Body, func(event promptRequestToolEvent) {
			name, matches := pending[event.id]
			if !event.receipt || !matches {
				return
			}
			result.ToolResults = append(result.ToolResults, source.result(event, name, ""))
			delete(pending, event.id)
		})
		if len(pending) == 0 {
			break
		}
	}
	if result.Response == nil && len(result.ToolResults) == 0 {
		return nil
	}
	return result
}

func (h *Handler) markPromptRequestResultEntries(ctx context.Context, current database.PromptRequestBody, previous []byte, changes *promptRequestChanges) {
	if changes == nil {
		return
	}
	needed := make(map[string]bool)
	for _, entry := range changes.Entries {
		if entry.CallID != "" {
			needed[entry.CallID] = true
		}
	}
	if len(needed) == 0 {
		return
	}
	ids, err := h.db.PromptRequestNeighborIDs(ctx, current, false, promptResultNeighbors)
	if err != nil {
		log.Printf("[prompt-requests] preceding requests lookup for %d failed: %T", current.ID, err)
		return
	}
	assigned := make(map[string]int64)
	for position, id := range ids {
		response, err := h.db.GetPromptRequestResponse(ctx, id)
		if err != nil {
			break
		}
		if response != nil {
			for _, call := range response.ToolCalls {
				if needed[call.CallID] {
					assigned[call.CallID] = id
					delete(needed, call.CallID)
				}
			}
		} else if position == 0 && id == changes.PreviousID && len(current.Body) <= promptResultReadLimit {
			visitInferredPromptRequestToolResults(previous, current.Body, func(_, receipt promptRequestToolEvent) {
				if needed[receipt.id] {
					assigned[receipt.id] = id
					delete(needed, receipt.id)
				}
			})
		}
		if len(needed) == 0 {
			break
		}
	}
	for index := range changes.Entries {
		entry := &changes.Entries[index]
		entry.ResultRequestID = assigned[entry.CallID]
	}
}

// Archived requests contain no upstream response. Only infer receipts from a
// direct continuation, using a newly appended call and its matching output.
func inferPromptRequestToolResults(previous []byte, next database.PromptRequestBody) []promptRequestToolResult {
	var results []promptRequestToolResult
	source := promptToolResultSource{request: next}
	visitInferredPromptRequestToolResults(previous, next.Body, func(call, receipt promptRequestToolEvent) {
		results = append(results, source.result(receipt, call.name, promptRequestToolText(call.value)))
	})
	return results
}

func visitInferredPromptRequestToolResults(previous, next []byte, visit func(call, receipt promptRequestToolEvent)) {
	field, before := promptRequestToolHistory(previous)
	nextField, after := promptRequestToolHistory(next)
	if field == "" || field != nextField || len(before) == 0 || len(after) <= len(before) {
		return
	}
	for index, item := range before {
		if !samePromptRequestJSON(item, after[index]) {
			return
		}
	}
	seen := make(map[string]bool)
	visitPromptRequestToolItems(field, before, 0, func(event promptRequestToolEvent) {
		seen[event.id] = true
	})
	calls := make(map[string]promptRequestToolEvent)
	visitPromptRequestToolItems(field, after, len(before), func(event promptRequestToolEvent) {
		if seen[event.id] {
			return
		}
		if !event.receipt {
			calls[event.id] = event
			return
		}
		if call, ok := calls[event.id]; ok {
			visit(call, event)
			seen[event.id] = true
		}
	})
}

func promptRequestToolHistory(body []byte) (string, []gjson.Result) {
	for _, field := range promptRequestHistoryFields {
		if value := gjson.GetBytes(body, field); value.IsArray() {
			return field, value.Array()
		}
	}
	return "", nil
}

func visitPromptRequestTools(body []byte, visit func(promptRequestToolEvent)) {
	field, items := promptRequestToolHistory(body)
	visitPromptRequestToolItems(field, items, 0, visit)
}

func visitPromptRequestToolItems(field string, items []gjson.Result, start int, visit func(promptRequestToolEvent)) {
	for i := start; i < len(items); i++ {
		visitPromptRequestToolItem(items[i], fmt.Sprintf("$.%s[%d]", field, i), visit)
	}
}

func visitPromptRequestToolItem(item gjson.Result, path string, visit func(promptRequestToolEvent)) {
	if !item.IsObject() {
		return
	}
	read := func(node gjson.Result, path string, receipt bool, ids []string, fields ...string) {
		event := promptRequestToolEvent{
			name: node.Get("name").Str, receipt: receipt,
		}
		if receipt {
			event.failed = node.Get("is_error").Bool() || node.Get("isError").Bool()
			switch strings.ToLower(node.Get("status").Str) {
			case "failed", "error", "incomplete", "cancelled", "canceled", "interrupted", "timed_out", "timeout":
				event.failed = true
			}
		}
		for _, field := range ids {
			if id := node.Get(field).Str; id != "" {
				event.id = id
				break
			}
		}
		if event.id == "" {
			return
		}
		for _, field := range fields {
			if value := node.Get(field); value.Exists() {
				event.value, event.path = value, path+"."+field
				break
			}
		}
		visit(event)
	}
	typ, role := item.Get("type").Str, item.Get("role").Str
	switch {
	case typ == "tool_result", typ == "functionResponse", strings.HasSuffix(typ, "_output"), role == "tool", role == "function":
		read(item, path, true, []string{"call_id", "tool_call_id", "tool_use_id"}, "output", "content", "text", "response")
	case typ == "tool_use", typ == "functionCall", strings.HasSuffix(typ, "_call"):
		read(item, path, false, []string{"call_id", "id"}, "arguments", "input", "action", "query", "args")
	case item.Get("functionResponse").IsObject():
		read(item.Get("functionResponse"), path+".functionResponse", true, []string{"id"}, "response")
	case item.Get("functionCall").IsObject():
		read(item.Get("functionCall"), path+".functionCall", false, []string{"id"}, "args")
	default:
		for _, field := range []string{"content", "parts"} {
			for i, child := range item.Get(field).Array() {
				visitPromptRequestToolItem(child, fmt.Sprintf("%s.%s[%d]", path, field, i), visit)
			}
		}
		for i, call := range item.Get("tool_calls").Array() {
			if id := call.Get("id").Str; id != "" {
				visit(promptRequestToolEvent{id: id, name: call.Get("function.name").Str, value: call.Get("function.arguments"), path: fmt.Sprintf("%s.tool_calls[%d].function.arguments", path, i)})
			}
		}
	}
}

type promptToolResultSource struct {
	request database.PromptRequestBody
	images  map[string]promptRequestImage
}

func (source *promptToolResultSource) result(event promptRequestToolEvent, name, arguments string) promptRequestToolResult {
	if name == "" {
		name = event.name
	}
	result := promptRequestToolResult{CallID: event.id, Name: name, Arguments: arguments, FromRequestID: source.request.ID, failed: event.failed}
	if !event.value.Exists() {
		return result
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(event.value.Raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return result
	}
	view := &promptRequestPresentation{}
	view.walkJSON(&value, event.path, false, -1)
	if len(view.Images) > 0 {
		if source.images == nil {
			source.images = make(map[string]promptRequestImage)
			for _, image := range presentPromptRequest(source.request, -2).Images {
				source.images[image.Label] = image
			}
		}
		// The isolated output has local indices. Remap the placeholders and
		// gallery to the indices assigned in the original request.
		for _, image := range view.Images {
			if original, ok := source.images[image.Label]; ok {
				result.Images = append(result.Images, original)
			}
		}
		sort.Slice(result.Images, func(i, j int) bool { return result.Images[i].Index < result.Images[j].Index })
	}
	text, isText := value.(string)
	if !isText {
		encoded, _ := json.Marshal(value)
		text = string(encoded)
	}
	if len(result.Images) > 0 {
		markers := make([]string, 0, 2*len(view.Images))
		for _, image := range view.Images {
			if original, ok := source.images[image.Label]; ok {
				markers = append(markers, fmt.Sprintf("[Image %d]", image.Index+1), fmt.Sprintf("[Image %d]", original.Index+1))
			}
		}
		text = strings.NewReplacer(markers...).Replace(text)
	}
	result.Text = text
	return result
}

func promptRequestToolText(value gjson.Result) string {
	if value.Type == gjson.String {
		return value.Str
	}
	return value.Raw
}
