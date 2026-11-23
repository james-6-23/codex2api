package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

type promptRequestChangeEntry struct {
	Kind            string `json:"kind"`
	Label           string `json:"label,omitempty"`
	Text            string `json:"text"`
	CallID          string `json:"call_id,omitempty"`
	ResultRequestID int64  `json:"result_request_id,omitempty"`
}

type promptRequestChanges struct {
	PreviousID       int64                      `json:"previous_id"`
	RepeatedItems    int                        `json:"repeated_items"`
	NewItems         int                        `json:"new_items"`
	HistoricalImages int                        `json:"historical_images"`
	Entries          []promptRequestChangeEntry `json:"entries"`
	ImageIndices     []int                      `json:"image_indices"`
	historyField     string
}

func (h *Handler) requestChanges(ctx context.Context, current database.PromptRequestBody, view promptRequestPresentation) (*promptRequestChanges, []byte) {
	if view.bodyFields == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	id, previous, err := h.db.PreviousPromptRequest(ctx, current)
	if err != nil {
		log.Printf("[prompt-requests] previous request lookup for %d failed: %T", current.ID, err)
		return nil, nil
	}
	if id == 0 || !gjson.ValidBytes(previous) {
		return nil, nil
	}
	changes := comparePromptRequestHistory(id, previous, current.Body, view)
	h.markPromptRequestResultEntries(ctx, current, previous, changes)
	return changes, previous
}

var promptRequestHistoryFields = []string{"input", "messages", "contents"}
var promptRequestContextFields = []string{
	"instructions", "system", "systemInstruction", "system_instruction", "prompt", "style",
	"tools", "tool_choice", "image", "image_url", "images", "input_images", "mask",
}

func comparePromptRequestHistory(previousID int64, previous, current []byte, view promptRequestPresentation) *promptRequestChanges {
	var historyField string
	var repeated int
	var items []any
	for _, field := range promptRequestHistoryFields {
		currentItems, ok := view.bodyFields[field].([]any)
		before, after := gjson.GetBytes(previous, field), gjson.GetBytes(current, field)
		if !ok || !before.IsArray() || !after.IsArray() {
			continue
		}
		old, new := before.Array(), after.Array()
		for index := 0; index < min(len(old), len(new)); index++ {
			if !samePromptRequestJSON(old[index], new[index]) {
				break
			}
			repeated++
		}
		if repeated > 0 && repeated <= len(currentItems) {
			historyField, items = field, currentItems
		}
		break
	}
	// A compacted/edited prefix or a lone request cannot establish repeated
	// history. Keep the complete request visible in those cases.
	if historyField == "" {
		return nil
	}

	result := &promptRequestChanges{
		PreviousID: previousID, RepeatedItems: repeated, NewItems: len(items) - repeated,
		Entries: []promptRequestChangeEntry{}, ImageIndices: []int{},
		historyField: historyField,
	}
	changedFields := make(map[string]bool)
	for _, field := range promptRequestContextFields {
		value, exists := view.bodyFields[field]
		if !exists || samePromptRequestJSON(gjson.GetBytes(previous, field), gjson.GetBytes(current, field)) {
			continue
		}
		changedFields[field] = true
		switch field {
		case "image", "image_url", "images", "input_images", "mask":
			// These appear in the image gallery instead of repeating the JSON.
		default:
			result.addChange("context", field, value)
		}
	}
	for _, item := range items[repeated:] {
		result.addItem(item, "")
	}
	for _, image := range view.Images {
		field, index := promptRequestImageLocation(image.Label)
		if changedFields[field] || (field == historyField && index >= repeated) {
			result.ImageIndices = append(result.ImageIndices, image.Index)
		} else if field == historyField && index >= 0 && index < repeated {
			result.HistoricalImages++
		}
	}
	return result
}

func samePromptRequestJSON(a, b gjson.Result) bool {
	if a.Raw == b.Raw {
		return true
	}
	if !a.Exists() || !b.Exists() || a.Type != b.Type {
		return false
	}
	decode := func(raw string) (any, error) {
		var value any
		decoder := json.NewDecoder(bytes.NewBufferString(raw))
		decoder.UseNumber()
		err := decoder.Decode(&value)
		return value, err
	}
	av, err := decode(a.Raw)
	if err != nil {
		return false
	}
	bv, err := decode(b.Raw)
	return err == nil && reflect.DeepEqual(av, bv)
}

func promptRequestImageLocation(label string) (string, int) {
	label = strings.TrimPrefix(label, "$.")
	end := strings.IndexAny(label, ".[")
	if end < 0 {
		return label, -1
	}
	field := label[:end]
	if label[end] == '[' {
		index, _, found := strings.Cut(label[end+1:], "]")
		if value, err := strconv.Atoi(index); found && err == nil {
			return field, value
		}
	}
	return field, -1
}

func (result *promptRequestChanges) addItem(value any, role string) {
	item, ok := value.(map[string]any)
	if !ok {
		result.addChange("text", role, value)
		return
	}
	if v, _ := item["role"].(string); v != "" {
		role = v
	}
	kind, _ := item["type"].(string)
	switch {
	case kind == "reasoning", kind == "compaction", kind == "context_compaction", kind == "encrypted_content":
		// Opaque continuations count toward the delta but their payload is
		// only relevant in the complete request.
		return
	case kind == "tool_result", kind == "functionResponse", strings.HasSuffix(kind, "_output"), role == "tool", role == "function":
		label, _ := item["name"].(string)
		id, _ := firstPromptChangeValue(item, "call_id", "tool_call_id", "tool_use_id").(string)
		result.addToolChange("tool_result", label, id, firstPromptChangeValue(item, "output", "content", "text", "response"))
	case kind == "tool_use", kind == "functionCall", strings.HasSuffix(kind, "_call"):
		label, _ := item["name"].(string)
		id, _ := firstPromptChangeValue(item, "call_id", "id").(string)
		result.addToolChange("tool_call", label, id, firstPromptChangeValue(item, "arguments", "input", "action", "query", "args"))
	case item["functionResponse"] != nil, item["functionCall"] != nil:
		for _, field := range []struct{ key, kind string }{{"functionCall", "tool_call"}, {"functionResponse", "tool_result"}} {
			if node, ok := item[field.key].(map[string]any); ok {
				id, _ := node["id"].(string)
				label, _ := node["name"].(string)
				result.addToolChange(field.kind, label, id, firstPromptChangeValue(node, "args", "response"))
			}
		}
	default:
		if calls, ok := item["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				function, _ := call["function"].(map[string]any)
				id, _ := call["id"].(string)
				label, _ := function["name"].(string)
				result.addToolChange("tool_call", label, id, function["arguments"])
			}
		}
		content := firstPromptChangeValue(item, "content", "parts", "text")
		if blocks, ok := content.([]any); ok {
			for _, block := range blocks {
				result.addItem(block, role)
			}
			return
		}
		result.addChange("text", role, content)
	}
}

func (result *promptRequestChanges) addToolChange(kind, label, id string, value any) {
	before := len(result.Entries)
	result.addChange(kind, label, value)
	if len(result.Entries) > before {
		result.Entries[before].CallID = id
	} else if label != "" || id != "" {
		result.Entries = append(result.Entries, promptRequestChangeEntry{Kind: kind, Label: label, CallID: id})
	}
}

func firstPromptChangeValue(item map[string]any, fields ...string) any {
	for _, field := range fields {
		if value := item[field]; value != nil {
			return value
		}
	}
	return nil
}

func (result *promptRequestChanges) addChange(kind, label string, value any) {
	if value == nil {
		return
	}
	text, isText := value.(string)
	if !isText {
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return
		}
		text = string(encoded)
	}
	if strings.TrimSpace(text) != "" {
		result.Entries = append(result.Entries, promptRequestChangeEntry{Kind: kind, Label: label, Text: text})
	}
}
