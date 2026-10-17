package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex's Responses backend rejects hosted search. Messages requests execute
// search through the same official standalone endpoint used by Codex clients.
type anthropicCodexWebSearch struct {
	name, model string
	domains     []string
	remaining   int
}

const anthropicCodexMaxSearchUses = 8

func prepareAnthropicCodexWebSearch(body, messagesBody []byte) ([]byte, *anthropicCodexWebSearch, error) {
	var bridge *anthropicCodexWebSearch
	tools := gjson.GetBytes(body, "tools").Array()
	for i, tool := range tools {
		if tool.Get("type").String() != "web_search" {
			continue
		}
		if bridge != nil {
			return nil, nil, fmt.Errorf("only one hosted web_search declaration is supported")
		}
		if len(tool.Get("user_location").Map()) > 0 || len(tool.Get("filters.blocked_domains").Array()) > 0 {
			return nil, nil, fmt.Errorf("Codex standalone web search does not support user_location or blocked_domains")
		}
		bridge = &anthropicCodexWebSearch{name: "codex2api_web_search", model: gjson.GetBytes(body, "model").String(), remaining: anthropicCodexMaxSearchUses}
		for _, other := range tools {
			if other.Get("name").String() == bridge.name {
				bridge.name += "_" + uuid.NewString()[:8]
			}
		}
		for _, domain := range tool.Get("filters.allowed_domains").Array() {
			bridge.domains = append(bridge.domains, domain.String())
		}
		for _, original := range gjson.GetBytes(messagesBody, "tools").Array() {
			if original.Get("name").String() == "web_search" {
				if limit := original.Get("max_uses"); limit.Exists() {
					if limit.Type != gjson.Number || limit.Float() != float64(limit.Int()) || limit.Int() < 0 {
						return nil, nil, fmt.Errorf("web_search max_uses must be a non-negative integer")
					}
					bridge.remaining = min(bridge.remaining, int(limit.Int()))
				}
			}
		}
		replacement := map[string]any{
			"type": "function", "name": bridge.name, "strict": false,
			"description": "Search the web using the official Codex search service. Use the returned sources to answer with links.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
		}
		body, _ = sjson.SetBytes(body, fmt.Sprintf("tools.%d", i), replacement)
	}
	if bridge == nil {
		return body, nil, nil
	}
	for i, tool := range gjson.GetBytes(body, "tool_choice.tools").Array() {
		if tool.Get("type").String() == "web_search" {
			body, _ = sjson.SetBytes(body, fmt.Sprintf("tool_choice.tools.%d", i), map[string]any{"type": "function", "name": bridge.name})
		}
	}
	return body, bridge, nil
}

type anthropicSearchStreamBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *anthropicSearchStreamBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

func executeAnthropicCodexRequest(ctx context.Context, account *auth.Account, body, messagesBody []byte, sessionID, proxyURL, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, websocket bool) (*http.Response, error) {
	body, bridge, err := prepareAnthropicCodexWebSearch(body, messagesBody)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	if bridge == nil {
		return ExecuteRequest(ctx, account, body, sessionID, proxyURL, apiKey, deviceCfg, headers, websocket)
	}
	// The outer Messages reader owns downstream keepalive writes.
	ctx, cancel := context.WithCancel(context.WithValue(ctx, continuousRetryKeepaliveContextKey{}, struct{}{}))
	open := func(next []byte) (*http.Response, error) {
		return ExecuteRequest(ctx, account, next, sessionID, proxyURL, apiKey, deviceCfg, headers, websocket)
	}
	resp, err := open(body)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body = &anthropicSearchStreamBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	search := func(raw []byte) (*CodexAlphaSearchResponse, error) {
		return ForwardCodexAlphaSearch(ctx, account, proxyURL, raw, headers, deviceCfg, apiKey)
	}
	reader, writer := io.Pipe()
	wrapped := *resp
	wrapped.Body = &anthropicSearchStreamBody{ReadCloser: reader, cancel: cancel}
	go func() {
		defer cancel()
		err := bridge.run(ctx, resp, body, open, search, writer)
		_ = writer.CloseWithError(err)
	}()
	return &wrapped, nil
}

func (b *anthropicCodexWebSearch) run(ctx context.Context, resp *http.Response, base []byte, open func([]byte) (*http.Response, error), search func([]byte) (*CodexAlphaSearchResponse, error), writer io.Writer) error {
	output := make([]json.RawMessage, 0)
	var replay []json.RawMessage
	var inputTokens, outputTokens, cachedTokens, reasoningTokens int64
	first := true
	emit := func(data []byte) bool {
		_, err := fmt.Fprintf(writer, "data: %s\n\n", data)
		return err == nil
	}
	for round := 0; round <= anthropicCodexMaxSearchUses; round++ {
		var terminal []byte
		hiddenIDs := map[string]bool{}
		hiddenIndices := map[int64]bool{}
		hiddenItems := map[string]json.RawMessage{}
		var hiddenOrder []string
		visibleItems := map[string]bool{}
		hiddenCurrent := false
		currentHiddenID := ""
		err := readSSEStreamWithContinuousRetryKeepalive(ctx, resp.Body, func(event string, data []byte) bool {
			parsed := gjson.ParseBytes(data)
			kind := normalizedUpstreamSSEEventType(event, data)
			if isResponsesSuccessTerminalEvent(kind) || kind == "response.failed" {
				terminal = append([]byte(nil), data...)
				return false
			}
			if kind == "response.created" || kind == "response.in_progress" {
				if !first {
					return true
				}
			}
			if kind == "response.output_item.added" || kind == "response.output_item.done" {
				item := parsed.Get("item")
				hiddenCurrent = (item.Get("type").String() == "function_call" && item.Get("name").String() == b.name) || hiddenIDs[item.Get("id").String()]
				if hiddenCurrent {
					if id := item.Get("id").String(); id != "" {
						currentHiddenID = id
						hiddenIDs[id] = true
						if previous := hiddenItems[id]; previous == nil {
							hiddenItems[id] = json.RawMessage(item.Raw)
							hiddenOrder = append(hiddenOrder, id)
						} else {
							for _, key := range []string{"arguments", "call_id", "name"} {
								if value := item.Get(key); value.Exists() && value.String() != "" {
									previous, _ = sjson.SetRawBytes(previous, key, []byte(value.Raw))
								}
							}
							hiddenItems[id] = previous
						}
					}
					if index := parsed.Get("output_index"); index.Exists() {
						hiddenIndices[index.Int()] = true
					}
					return true
				}
				if kind == "response.output_item.added" {
					visibleItems[item.Get("id").String()+"|"+item.Get("call_id").String()+"|"+item.Get("type").String()] = true
				}
			}
			if strings.HasPrefix(kind, "response.function_call_arguments.") {
				hidden := hiddenCurrent
				if id := parsed.Get("item_id").String(); id != "" {
					hidden = hiddenIDs[id]
				} else if index := parsed.Get("output_index"); index.Exists() {
					hidden = hiddenIndices[index.Int()]
				}
				if hidden {
					if kind == "response.function_call_arguments.done" {
						id := parsed.Get("item_id").String()
						if id == "" {
							id = currentHiddenID
						}
						if hiddenItems[id] != nil && parsed.Get("arguments").Type == gjson.String {
							hiddenItems[id], _ = sjson.SetBytes(hiddenItems[id], "arguments", parsed.Get("arguments").String())
						}
					}
					return true
				}
			}
			return emit(data)
		})
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(terminal) == 0 {
			return io.ErrUnexpectedEOF
		}
		parsed := gjson.ParseBytes(terminal)
		inputTokens += parsed.Get("response.usage.input_tokens").Int()
		outputTokens += parsed.Get("response.usage.output_tokens").Int()
		cachedTokens += parsed.Get("response.usage.input_tokens_details.cached_tokens").Int()
		reasoningTokens += parsed.Get("response.usage.output_tokens_details.reasoning_tokens").Int()
		var calls []gjson.Result
		seen := map[string]bool{}
		external := false
		for _, item := range parsed.Get("response.output").Array() {
			id := item.Get("id").String()
			if item.Get("type").String() == "function_call" && (item.Get("name").String() == b.name || hiddenIDs[id]) {
				if item.Get("arguments").String() == "" && hiddenItems[id] != nil {
					item = gjson.ParseBytes(hiddenItems[id])
				}
				calls = append(calls, item)
				seen[id] = true
			} else {
				key := item.Get("id").String() + "|" + item.Get("call_id").String() + "|" + item.Get("type").String()
				if !visibleItems[key] && !emitAnthropicSearchCompletedItem(item, emit) {
					return io.ErrClosedPipe
				}
				output = append(output, json.RawMessage(item.Raw))
				external = external || item.Get("type").String() == "function_call" || item.Get("type").String() == "custom_tool_call"
			}
			replay = append(replay, json.RawMessage(item.Raw))
		}
		for _, id := range hiddenOrder {
			if !seen[id] {
				item := gjson.ParseBytes(hiddenItems[id])
				calls = append(calls, item)
				replay = append(replay, json.RawMessage(item.Raw))
			}
		}
		if parsed.Get("type").String() == "response.completed" {
			for _, call := range calls {
				item, result, err := b.executeSearch(ctx, call, search, emit)
				if err != nil {
					return err
				}
				output = append(output, item)
				replay = append(replay, result)
			}
		}
		if len(calls) == 0 || external || parsed.Get("type").String() != "response.completed" {
			terminal, _ = sjson.SetBytes(terminal, "response.output", output)
			terminal, _ = sjson.SetBytes(terminal, "response.usage", map[string]any{
				"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens,
				"input_tokens_details": map[string]any{"cached_tokens": cachedTokens}, "output_tokens_details": map[string]any{"reasoning_tokens": reasoningTokens},
			})
			if !emit(terminal) {
				return io.ErrClosedPipe
			}
			return nil
		}
		if b.remaining <= 0 {
			tools := make([]json.RawMessage, 0)
			for _, tool := range gjson.GetBytes(base, "tools").Array() {
				if tool.Get("name").String() != b.name {
					tools = append(tools, json.RawMessage(tool.Raw))
				}
			}
			base, _ = sjson.SetBytes(base, "tools", tools)
		}
		base, _ = sjson.SetBytes(base, "tool_choice", "auto")
		next, err := buildContinuationBody(base, replay)
		if err != nil {
			return err
		}
		resp, err = open(next)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, codexAlphaSearchBodyLimit))
			_ = resp.Body.Close()
			if readErr != nil {
				return readErr
			}
			return ErrUpstream(resp.StatusCode, usageLogErrorMessage(resp.StatusCode, raw), nil)
		}
		first = false
	}
	_ = resp.Body.Close()
	return ErrUpstreamStreamBreak("Codex web search exceeded the maximum number of continuation rounds")
}

func (b *anthropicCodexWebSearch) executeSearch(ctx context.Context, call gjson.Result, search func([]byte) (*CodexAlphaSearchResponse, error), emit func([]byte) bool) (json.RawMessage, json.RawMessage, error) {
	if call.Get("call_id").String() == "" {
		return nil, nil, ErrUpstreamStreamBreak("Codex search function call is missing call_id")
	}
	arguments := call.Get("arguments").String()
	queryValue := gjson.Get(arguments, "query")
	query := strings.TrimSpace(queryValue.String())
	if !gjson.Valid(arguments) || queryValue.Type != gjson.String || len(arguments) > 64<<10 {
		query = ""
	}
	id := call.Get("id").String()
	if id == "" {
		id = call.Get("call_id").String()
	}
	if id == "" {
		return nil, nil, ErrUpstreamStreamBreak("Codex search function call is missing call_id")
	}
	item := map[string]any{"id": id, "type": "web_search_call", "status": "in_progress", "action": map[string]any{"type": "search", "queries": []string{query}}}
	if !emit(mustMarshalSearchEvent("response.output_item.added", item)) {
		return nil, nil, io.ErrClosedPipe
	}
	errorCode := ""
	result := map[string]any{}
	switch {
	case b.remaining <= 0:
		errorCode = "max_uses_exceeded"
	case query == "":
		errorCode = "invalid_tool_input"
	default:
		b.remaining--
		command := map[string]any{"q": query}
		if len(b.domains) > 0 {
			command["domains"] = b.domains
		}
		raw, _ := json.Marshal(map[string]any{"id": "srch_" + uuid.NewString(), "model": b.model, "commands": map[string]any{"search_query": []any{command}, "response_length": "short"}})
		resp, err := search(raw)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err != nil {
			errorCode = "unavailable"
		} else if resp.StatusCode != http.StatusOK {
			errorCode = "unavailable"
			if resp.StatusCode == http.StatusTooManyRequests {
				errorCode = "too_many_requests"
			} else if resp.StatusCode == http.StatusBadRequest {
				errorCode = "invalid_tool_input"
			}
		} else if err := json.Unmarshal(resp.Body, &result); err != nil || (gjson.GetBytes(resp.Body, "output").Type != gjson.String && !gjson.GetBytes(resp.Body, "output").IsArray()) || (gjson.GetBytes(resp.Body, "results").Exists() && !gjson.GetBytes(resp.Body, "results").IsArray()) {
			errorCode = "unavailable"
		} else {
			delete(result, "encrypted_output")
			item["results"] = result["results"]
		}
	}
	item["status"] = "completed"
	if errorCode != "" {
		item["status"] = "failed"
		item["error"] = map[string]any{"code": errorCode}
		result = map[string]any{"error": errorCode}
		b.remaining = 0
	}
	if !emit(mustMarshalSearchEvent("response.output_item.done", item)) {
		return nil, nil, io.ErrClosedPipe
	}
	itemJSON, _ := json.Marshal(item)
	resultJSON, _ := json.Marshal(result)
	toolOutput, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": call.Get("call_id").String(), "output": string(resultJSON)})
	return itemJSON, toolOutput, nil
}

// Some upstreams only include an output item in the terminal event.
func emitAnthropicSearchCompletedItem(item gjson.Result, emit func([]byte) bool) bool {
	if !emit(mustMarshalSearchEvent("response.output_item.added", json.RawMessage(item.Raw))) {
		return false
	}
	if item.Get("type").String() == "message" {
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() == "output_text" {
				raw, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": part.Get("text").String()})
				if !emit(raw) {
					return false
				}
			}
		}
	}
	if item.Get("type").String() == "reasoning" {
		for _, part := range item.Get("summary").Array() {
			raw, _ := json.Marshal(map[string]any{"type": "response.reasoning_summary_part.added", "part": json.RawMessage(part.Raw)})
			if !emit(raw) {
				return false
			}
		}
	}
	return emit(mustMarshalSearchEvent("response.output_item.done", json.RawMessage(item.Raw)))
}

func mustMarshalSearchEvent(kind string, item any) []byte {
	raw, _ := json.Marshal(map[string]any{"type": kind, "item": item})
	return raw
}
