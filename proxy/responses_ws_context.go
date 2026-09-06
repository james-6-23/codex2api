package proxy

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responsesWSReplayInput builds a portable snapshot without changing the
// incremental request sent over a healthy upstream WebSocket. Missing ancestry
// must not turn a partial input into a supposedly complete cached conversation.
func responsesWSReplayInput(body []byte, owner string) (string, *api.APIError) {
	return responsesWSReplayInputWithLookup(body, owner, getResponseCacheForReplay)
}

func responsesWSReplayInputWithLookup(body []byte, owner string, lookupFn func(string, string) responseCacheLookupResult) (string, *api.APIError) {
	current := gjson.GetBytes(body, "input")
	var items []json.RawMessage
	previousID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previousID != "" {
		lookup := lookupFn(owner, previousID)
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
	expanded, _, err := degradeResponsesWSContinuationWithSource(body, owner, nil)
	return expanded, err
}

// lost distinguishes an explicitly lossy fallback from a complete replay.
func degradeResponsesWSContinuationWithSource(body []byte, owner string, source *responsesWSReplaySource) ([]byte, bool, *api.APIError) {
	if gjson.GetBytes(body, "previous_response_id").String() == "" {
		return body, false, nil
	}
	var input string
	var apiErr *api.APIError
	if source != nil && source.previous != nil {
		input = source.Input()
		recordResponseCacheLookup(owner, *source.previous)
		apiErr = source.err
	} else {
		input, apiErr = responsesWSReplayInput(body, owner)
	}
	if apiErr != nil {
		if !responsesWSContinuationFailOpen() {
			return nil, false, apiErr
		}
		// 逃生阀：退回旧行为，剥掉 previous_response_id 后按原样转发当轮 input。
		// 上游看到的是丢失历史的会话，只在明确接受该风险时启用。
		log.Printf("Responses WebSocket continuation context unavailable (%s); CODEX_WS_CONTINUATION_FAIL_OPEN=true, forwarding without history", responsesWSContextReason(apiErr))
		stripped, err := sjson.DeleteBytes(body, "previous_response_id")
		if err != nil {
			return nil, false, apiErr
		}
		return stripped, true, nil
	}
	expanded, err := sjson.SetRawBytes(body, "input", []byte(input))
	if err != nil {
		return nil, false, responsesWSContextUnavailable(http.StatusConflict, "invalid_context")
	}
	expanded, err = sjson.DeleteBytes(expanded, "previous_response_id")
	if err != nil {
		return nil, false, responsesWSContextUnavailable(http.StatusConflict, "invalid_context")
	}
	return expanded, false, nil
}

// responsesWSContinuationFailOpen 读取逃生阀：CODEX_WS_CONTINUATION_FAIL_OPEN=true
// 时上下文不可恢复不再关连接，而是按旧行为剥 id 硬发。
func responsesWSContinuationFailOpen() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_WS_CONTINUATION_FAIL_OPEN"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func responsesWSContextReason(apiErr *api.APIError) string {
	if apiErr == nil {
		return ""
	}
	switch details := apiErr.Details.(type) {
	case []api.ErrorDetail:
		for _, detail := range details {
			if detail.Message != "" {
				return detail.Message
			}
		}
	case api.ErrorDetail:
		if details.Message != "" {
			return details.Message
		}
	}
	return apiErr.Message
}

// markResponsesWSContinuationCapable 给可能续链的 WS 会话在根轮就授予 on_demand
// 写入资格。Codex CLI 全量上下文每轮 store:false，永远不会带 previous_response_id
// 回来，不能为它开闸；store 非 false 的会话才有机会续链，若根轮不入缓存，
// 之后每轮都因祖先缺失无法成快照，降级必然失败。
func markResponsesWSContinuationCapable(owner string, rawBody []byte) {
	if store := gjson.GetBytes(rawBody, "store"); store.Exists() && store.Type == gjson.False {
		return
	}
	markResponseCacheChainOwnerIfOnDemand(owner)
}

// responsesWSReplaySource pins a live L1 ancestor at turn admission. Its immutable
// bodies survive expiry/eviction during generation; merging and backend fallback
// remain lazy and do not count as client replay hits or misses.
type responsesWSReplaySource struct {
	body        []byte
	owner       string
	precomputed bool
	once        sync.Once
	input       string
	previous    *responseCacheLookupResult
	err         *api.APIError
}

func newResponsesWSReplaySource(body []byte, owner string) *responsesWSReplaySource {
	source := &responsesWSReplaySource{body: body, owner: owner}
	if previousID := gjson.GetBytes(body, "previous_response_id").String(); previousID != "" {
		respCache.mu.RLock()
		if entry := respCache.store[responseCacheStoreKey(owner, previousID)]; entry != nil && time.Now().Before(entry.expiresAt) {
			source.previous = &responseCacheLookupResult{Kind: responseCacheLookupHit, Source: responseCacheSourceLocal, Items: append([]json.RawMessage(nil), entry.items...)}
		}
		respCache.mu.RUnlock()
	}
	return source
}

func newResponsesWSReplaySourceFromInput(input string) *responsesWSReplaySource {
	return &responsesWSReplaySource{input: input, precomputed: true}
}

// Input 返回可回放快照；祖先缺失、超限或不可移植时返回空串，调用方据此跳过写缓存。
func (s *responsesWSReplaySource) Input() string {
	if s == nil {
		return ""
	}
	if s.precomputed {
		return s.input
	}
	s.once.Do(func() {
		s.input, s.err = responsesWSReplayInputWithLookup(s.body, s.owner, func(owner, id string) responseCacheLookupResult {
			if s.previous != nil {
				return *s.previous
			}
			return lookupResponseCacheResultWithOwnership(owner, id, true)
		})
	})
	return s.input
}

// Record failures only when returning them to a client, not while considering a
// best-effort snapshot after an otherwise successful upstream turn.
func responsesWSContextCloseCode(apiErr *api.APIError) int {
	if apiErr.Code == api.ErrCodeServiceUnavailable {
		return websocket.CloseInternalServerErr
	}
	recordResponseCacheKnownUnavailableError()
	return websocket.ClosePolicyViolation
}
