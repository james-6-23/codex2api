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
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responsesWSReplayInput builds a portable snapshot without changing the
// incremental request sent over a healthy upstream WebSocket. Missing ancestry
// must not turn a partial input into a supposedly complete cached conversation.
func responsesWSReplayInput(body []byte, owner string) (string, *api.APIError) {
	if strings.HasPrefix(owner, nativeWSCachePrefix) {
		return responsesWSReplayInputWithLookup(body, owner, nativeWSContextLookup)
	}
	return responsesWSReplayInputWithLookup(body, owner, getResponseCacheForReplay)
}

func responsesWSReplayInputWithLookup(body []byte, owner string, lookupFn func(string, string) responseCacheLookupResult) (string, *api.APIError) {
	return responsesWSReplayInputChecked(body, responsesWSReplayOptions{owner: owner, lookup: lookupFn})
}

type responsesWSReplayOptions struct {
	owner       string
	lookup      func(string, string) responseCacheLookupResult
	allowOpaque bool
}

func responsesWSReplayInputChecked(body []byte, options responsesWSReplayOptions) (string, *api.APIError) {
	owner := options.owner
	var items []json.RawMessage
	previousID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previousID != "" {
		lookup := options.lookup(owner, previousID)
		if lookup.Kind != responseCacheLookupHit {
			prepared := responsesBodyPreparation{PreviousResponseID: previousID, CacheLookup: lookup, RequiresLocalContext: true}
			status, reason, _ := responseCachePreparationFailure(prepared)
			return "", responsesWSContextUnavailable(status, reason)
		}
		items = append(items, lookup.Items...)
	}
	input, itemErr := responsesWSReplayItems(gjson.GetBytes(body, "input"), options)
	if itemErr != nil {
		return "", itemErr
	}
	items, itemErr = validateResponsesWSReplayItems(mergeResponsesWSContext(items, input), options)
	if itemErr != nil {
		return "", itemErr
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

func responsesWSReplayItems(current gjson.Result, options responsesWSReplayOptions) ([]json.RawMessage, *api.APIError) {
	var input []json.RawMessage
	var itemErr *api.APIError
	current.ForEach(func(_, item gjson.Result) bool {
		if strings.HasPrefix(options.owner, nativeWSCachePrefix) {
			raw, err := nativeWSReplayItemChecked(item, options.allowOpaque)
			if err != nil {
				itemErr = err
				return false
			}
			input = append(input, raw)
		} else if raw, ok := replayableCachedInputItem(item); ok {
			input = append(input, raw)
		}
		return true
	})
	return input, itemErr
}

func validateResponsesWSReplayItems(items []json.RawMessage, options responsesWSReplayOptions) ([]json.RawMessage, *api.APIError) {
	for index, item := range items {
		if strings.HasPrefix(options.owner, nativeWSCachePrefix) {
			raw, err := nativeWSReplayItemChecked(gjson.ParseBytes(item), options.allowOpaque)
			if err != nil {
				return nil, err
			}
			items[index] = raw
		} else if gjson.GetBytes(item, "encrypted_content").String() != "" {
			return nil, responsesWSContextUnavailable(http.StatusConflict, "nonportable_encrypted_context")
		}
	}
	return items, nil
}

func responsesWSContextUnavailable(status int, reason string) *api.APIError {
	code, kind, message := api.ErrCodeResponseContextUnavailable, api.ErrorTypeInvalidRequest, responseContextUnavailableMessage
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
	if strings.HasPrefix(owner, nativeWSCachePrefix) {
		cacheNativeWSContext(nativeWSCompletedContext{owner: owner, input: input, completed: completed, outputs: outputItems})
		return
	}
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
	if source != nil {
		input = source.Input()
		if source.previous != nil {
			recordResponseCacheLookup(owner, *source.previous)
		}
		apiErr = source.err
	} else {
		input, apiErr = responsesWSReplayInput(body, owner)
	}
	if apiErr != nil {
		if strings.HasPrefix(owner, nativeWSCachePrefix) {
			return nil, false, nativeResponsesWSContextError(apiErr)
		}
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

// markResponsesWSContinuationCapable 为允许持久存储的会话授予 on_demand 写入资格。
// store:false 的原生 WS 续链单独使用完整快照，并复用共享后端。
func markResponsesWSContinuationCapable(owner string, rawBody []byte) {
	if store := gjson.GetBytes(rawBody, "store"); store.Exists() && store.Type == gjson.False {
		return
	}
	markResponseCacheChainOwnerIfOnDemand(owner)
}

// 入场时固定不可变祖先；原生 WS 缺少 L1 时读取共享快照以保留加密上下文的账号。
// 旧续链后端查询、历史合并与序列化仍按需执行，不提前记为客户端回放命中。
type responsesWSReplaySource struct {
	body               []byte
	owner              string
	precomputed        bool
	once               sync.Once
	input              string
	previous           *responseCacheLookupResult
	err                *api.APIError
	attemptProvenance  *nativeWSReplayProvenance
	recoveryProvenance *nativeWSReplayProvenance
}

func newResponsesWSReplaySource(body []byte, owner string) *responsesWSReplaySource {
	source := &responsesWSReplaySource{body: body, owner: owner}
	if previousID := gjson.GetBytes(body, "previous_response_id").String(); previousID != "" {
		respCache.mu.RLock()
		if entry := respCache.store[responseCacheStoreKey(owner, previousID)]; entry != nil && time.Now().Before(entry.expiresAt) {
			source.previous = &responseCacheLookupResult{Kind: responseCacheLookupHit, Source: responseCacheSourceLocal, Items: append([]json.RawMessage(nil), entry.items...), nativeProvenance: entry.nativeProvenance}
		}
		respCache.mu.RUnlock()
		if source.previous == nil && strings.HasPrefix(owner, nativeWSCachePrefix) {
			lookup := nativeWSContextLookup(owner, previousID)
			source.previous = &lookup
		}
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
		return s.nativeValidatedInput()
	}
	s.once.Do(func() {
		if strings.HasPrefix(s.owner, nativeWSCachePrefix) && s.previous == nil {
			if id := gjson.GetBytes(s.body, "previous_response_id").String(); id != "" {
				lookup := nativeWSContextLookup(s.owner, id)
				s.previous = &lookup
			}
		}
		options := responsesWSReplayOptions{owner: s.owner, allowOpaque: s.nativeReplayProvenance().matches(s.attemptProvenance)}
		options.lookup = func(owner, id string) responseCacheLookupResult {
			if s.previous != nil {
				return *s.previous
			}
			lookup := nativeWSLocalLookup(owner, id)
			if !strings.HasPrefix(owner, nativeWSCachePrefix) {
				lookup = lookupResponseCacheResultWithOwnership(owner, id, true)
			} else {
				lookup = nativeWSContextLookup(owner, id)
			}
			s.previous = &lookup
			return lookup
		}
		s.input, s.err = responsesWSReplayInputChecked(s.body, options)
	})
	return s.nativeValidatedInput()
}
