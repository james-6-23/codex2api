package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responsesWSReplayInput builds a portable snapshot without changing the
// incremental request sent over a healthy upstream WebSocket. Missing ancestry
// must not turn a partial input into a supposedly complete cached conversation.
func responsesWSReplayInput(body []byte, owner string) (string, *api.APIError) {
	current := gjson.GetBytes(body, "input")
	var items []json.RawMessage
	previousID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previousID != "" {
		lookup := getResponseCacheForReplay(owner, previousID)
		if lookup.Kind != responseCacheLookupHit {
			prepared := responsesBodyPreparation{PreviousResponseID: previousID, CacheLookup: lookup, RequiresLocalContext: true}
			status, reason, _ := responseCachePreparationFailure(prepared)
			return "", responsesWSContextUnavailable(status, reason)
		}
		items = append(items, lookup.Items...)
	}
	var input []json.RawMessage
	current.ForEach(func(_, item gjson.Result) bool {
		if raw, ok := replayableCachedInputItem(item); ok {
			input = append(input, raw)
		}
		return true
	})
	items = mergeResponsesWSContext(items, input)
	for _, item := range items {
		if gjson.GetBytes(item, "encrypted_content").String() != "" {
			return "", responsesWSContextUnavailable(http.StatusConflict, "nonportable_encrypted_context")
		}
	}
	respCache.mu.RLock()
	config := respCache.config
	respCache.mu.RUnlock()
	if len(items) > config.maxItems || responseContextLogicalBytes(items) > config.reconstructMaxBytes {
		return "", responsesWSContextUnavailable(http.StatusConflict, "reconstruction_too_large")
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return "", responsesWSContextUnavailable(http.StatusConflict, "invalid_context")
	}
	return string(raw), nil
}

func responsesWSContextUnavailable(status int, reason string) *api.APIError {
	code, kind, message := api.ErrCodeResponseContextUnavailable, api.ErrorTypeInvalidRequest, "Previous response context is unavailable"
	if status == http.StatusServiceUnavailable {
		code, kind, message = api.ErrCodeServiceUnavailable, api.ErrorTypeServer, "Previous response context backend is temporarily unavailable"
	}
	return api.NewAPIErrorWithDetails(code, message, kind, api.ErrorDetail{Field: "previous_response_id", Message: reason})
}

func responsesWSContextItemKey(raw json.RawMessage) string {
	item := gjson.ParseBytes(raw)
	callType, call, output := responseContextPairType(item.Get("type").String())
	if id := item.Get("call_id").String(); id != "" && (call || output) {
		if output {
			callType += ":output"
		}
		return callType + ":" + id
	}
	return ""
}

func mergeResponsesWSContext(previous, current []json.RawMessage) []json.RawMessage {
	// Only protocol call IDs establish duplication. Identical message text can
	// be a new user turn and must never be collapsed by content equality.
	merged := append([]json.RawMessage(nil), previous...)
	callIndexes := make(map[string]int)
	for i, raw := range merged {
		if key := responsesWSContextItemKey(raw); key != "" {
			callIndexes[key] = i
		}
	}
	for _, raw := range current {
		if key := responsesWSContextItemKey(raw); key != "" {
			if index, ok := callIndexes[key]; ok {
				merged[index] = raw
				continue
			}
			callIndexes[key] = len(merged)
		}
		merged = append(merged, raw)
	}
	return merged
}

// cacheResponsesWSCompletedResponse also retains message-only responses and
// tool declarations. Opaque reasoning is deliberately excluded by the existing
// replay sanitizer: this snapshot may be replayed on a different account.
// Call only after the attempt and downstream output have committed successfully.
func cacheResponsesWSCompletedResponse(owner, input string, completed []byte, outputItems []json.RawMessage) {
	responseID := gjson.GetBytes(completed, "response.id").String()
	if responseID == "" || input == "" {
		return
	}
	var items []json.RawMessage
	if json.Unmarshal([]byte(sanitizeResponseCacheInput([]byte(input))), &items) != nil {
		return
	}
	terminal := gjson.GetBytes(completed, "response.output")
	if len(terminal.Array()) > len(outputItems) {
		outputItems = nil
		terminal.ForEach(func(_, item gjson.Result) bool {
			outputItems = append(outputItems, json.RawMessage(item.Raw))
			return true
		})
	}
	var output []json.RawMessage
	for _, raw := range outputItems {
		item := gjson.ParseBytes(raw)
		if item.Get("type").String() == "message" {
			if replayable, ok := stripResponseItemID(raw); ok {
				output = append(output, replayable)
			}
		} else if replayable, ok := replayableCachedOutputItem(item); ok {
			output = append(output, replayable)
		}
	}
	items = mergeResponsesWSContext(items, output)
	if len(items) == 0 {
		return
	}
	respCache.mu.Lock()
	config := respCache.config
	if len(items) > config.maxItems || responseContextLogicalBytes(items) > config.reconstructMaxBytes {
		// A tail-trimmed snapshot silently loses its root and tool declarations.
		// Refuse admission instead; the native upstream continuation can still run.
		respCache.setMarkerLocked(responseCacheStoreKey(owner, responseID), responseCacheLookupKnownOversize, time.Now().Add(config.ttl))
		respCache.mu.Unlock()
		return
	}
	respCache.mu.Unlock()
	setResponseCache(owner, responseID, items)
}

func degradeResponsesWSContinuationBody(body []byte, owner string) ([]byte, *api.APIError) {
	if gjson.GetBytes(body, "previous_response_id").String() == "" {
		return body, nil
	}
	input, apiErr := responsesWSReplayInput(body, owner)
	if apiErr != nil {
		return nil, apiErr
	}
	expanded, err := sjson.SetRawBytes(body, "input", []byte(input))
	if err != nil {
		return nil, responsesWSContextUnavailable(http.StatusConflict, "invalid_context")
	}
	expanded, err = sjson.DeleteBytes(expanded, "previous_response_id")
	if err != nil {
		return nil, responsesWSContextUnavailable(http.StatusConflict, "invalid_context")
	}
	return expanded, nil
}
