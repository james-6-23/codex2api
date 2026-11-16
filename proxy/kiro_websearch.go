package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
)

// 托管 web_search，移植自 kiro.rs src/anthropic/websearch{,_loop}.rs：只声明 web_search 时
// 直接调用 Kiro MCP 搜索；与其他工具混用时在服务端跑搜索循环，web_search 永远不以原始
// tool_use 离开网关，客户端工具调用原样返回。

const kiroMaxWebSearchRounds = 5

var kiroWebSearchToolSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"The search query"}},"required":["query"]}`)

type kiroSearchResults struct {
	Results []struct {
		Title         string `json:"title"`
		URL           string `json:"url"`
		Snippet       string `json:"snippet"`
		PublishedDate *int64 `json:"publishedDate"`
	} `json:"results"`
}

func kiroIsNativeWebSearchTool(tool kiroInboundTool) bool {
	return tool.Name == "web_search" && strings.HasPrefix(tool.Type, "web_search_")
}

func kiroOnlyWebSearchTool(req *kiroInboundRequest) bool {
	return len(req.Tools) == 1 && kiroIsNativeWebSearchTool(req.Tools[0])
}

func kiroWebSearchAmongTools(req *kiroInboundRequest) bool {
	if len(req.Tools) < 2 {
		return false
	}
	for _, tool := range req.Tools {
		if kiroIsNativeWebSearchTool(tool) {
			return true
		}
	}
	return false
}

func kiroRandomID(length int, charset string) string {
	out := make([]byte, length)
	for i := range out {
		out[i] = charset[rand.IntN(len(charset))]
	}
	return string(out)
}

func kiroServerToolUseID() string {
	return "srvtoolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:32]
}

// kiroCallMCPSearch 调用 Kiro 的 /mcp web_search。返回值三选一：结果（可为空）、
// 映射后的上游错误响应、传输错误。
func kiroCallMCPSearch(ctx context.Context, account *auth.Account, query, proxyOverride string) (*kiroSearchResults, *http.Response, error) {
	requestAuth := account.KiroRequestAuth()
	if requestAuth.BearerToken == "" {
		return nil, kiroSyntheticResponse(http.StatusUnauthorized, []byte(`{"message":"The bearer token included in the request is invalid."}`)), nil
	}
	cred := requestAuth.Credential
	host := kiroRuntimeHost(cred.EffectiveAPIRegion())
	payload, _ := json.Marshal(map[string]any{
		"id":      fmt.Sprintf("web_search_tooluse_%s_%d_%s", kiroRandomID(22, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"), time.Now().UnixMilli(), kiroRandomID(8, "abcdefghijklmnopqrstuvwxyz0123456789")),
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params":  map[string]any{"name": "web_search", "arguments": map[string]any{"query": query}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/mcp", bytes.NewReader(payload))
	if err != nil {
		return nil, nil, ErrInternalError("创建 Kiro MCP 请求失败", err)
	}
	version := auth.KiroIDEVersion()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=3")
	req.Header.Set("Authorization", "Bearer "+requestAuth.BearerToken)
	if cred.Endpoint == auth.KiroEndpointCLI {
		req.Header.Set("x-amz-user-agent", "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.14474 os/"+auth.KiroOSName+" lang/rust/1.92.0 m/F app/AmazonQ-For-CLI")
		req.Header.Set("User-Agent", "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.14474 os/"+auth.KiroOSName+" lang/rust/1.92.0 md/appVersion-"+version+" app/AmazonQ-For-CLI")
	} else {
		req.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.34 KiroIDE-"+version+"-"+cred.MachineID)
		req.Header.Set("User-Agent", "aws-sdk-js/1.0.34 ua/2.1 os/"+auth.KiroOSName+" lang/js md/nodejs#"+auth.KiroNodeVersion+" api/codewhispererstreaming#1.0.34 m/E KiroIDE-"+version+"-"+cred.MachineID)
	}
	if arn := cred.StreamingProfileARN(); arn != "" {
		req.Header.Set("x-amzn-kiro-profile-arn", arn)
	}
	if tokenType := cred.TokenTypeHeader(); tokenType != "" {
		req.Header.Set("tokentype", tokenType)
	}
	proxyURL := requestAuth.ProxyURL
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	proxyURL = requestProxyURL(ctx, proxyURL)
	client := *getRequestClient(ctx, account, proxyURL)
	resp, err := doTracedUpstreamRequest(&client, req, account, proxyURL)
	if err != nil {
		return nil, nil, ErrUpstream(0, "请求 Kiro web_search 失败", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, kiroSyntheticResponse(kiroMapUpstreamStatus(resp.StatusCode, string(raw)), raw), nil
	}
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, nil, ErrUpstream(http.StatusBadGateway, "Kiro web_search 响应无法解析", err)
	}
	// MCP 层的错误（含“无结果”）按空结果处理，让模型据此作答，而不是整轮失败。
	if parsed.Error != nil || parsed.Result == nil || len(parsed.Result.Content) == 0 || parsed.Result.Content[0].Type != "text" {
		return nil, nil, nil
	}
	var results kiroSearchResults
	if json.Unmarshal([]byte(parsed.Result.Content[0].Text), &results) != nil {
		return nil, nil, nil
	}
	return &results, nil, nil
}

func kiroSearchSummary(query string, results *kiroSearchResults) string {
	var summary strings.Builder
	summary.WriteString(fmt.Sprintf("Search results for %q:\n\n", query))
	if results == nil || len(results.Results) == 0 {
		summary.WriteString("No results found.\n")
		return summary.String()
	}
	for i, result := range results.Results {
		summary.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, result.Title))
		if result.Snippet != "" {
			snippet := result.Snippet
			if runes := []rune(snippet); len(runes) > 200 {
				snippet = string(runes[:200]) + "..."
			}
			summary.WriteString("   " + snippet + "\n")
		}
		summary.WriteString("   Source: " + result.URL + "\n\n")
	}
	return summary.String()
}

func kiroSearchResultBlocks(results *kiroSearchResults) []any {
	blocks := []any{}
	if results == nil {
		return blocks
	}
	for _, result := range results.Results {
		var pageAge any
		if result.PublishedDate != nil {
			pageAge = time.UnixMilli(*result.PublishedDate).UTC().Format("January 2, 2006")
		}
		blocks = append(blocks, map[string]any{"type": "web_search_result", "title": result.Title, "url": result.URL, "encrypted_content": result.Snippet, "page_age": pageAge})
	}
	return blocks
}

func kiroSearchPresentation(query string, results *kiroSearchResults) []any {
	id := kiroServerToolUseID()
	return []any{
		map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": query}},
		map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": kiroSearchResultBlocks(results)},
	}
}

// kiroExtractSearchQuery 取最后一条 user 的首段文本（Claude Code 会加固定前缀）。
func kiroExtractSearchQuery(req *kiroInboundRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		text, isString, blocks := kiroContent(req.Messages[i].Content)
		if !isString {
			text = ""
			for _, block := range blocks {
				if kiroString(block, "type") == "text" {
					text = kiroString(block, "text")
					break
				}
			}
		}
		return strings.TrimPrefix(text, "Perform a web search for the query: ")
	}
	return ""
}

// runKiroWebSearch 处理带托管 web_search 的请求，返回一条完整 Anthropic message 的事件序列。
func runKiroWebSearch(ctx context.Context, account *auth.Account, req *kiroInboundRequest, proxyOverride string) ([]kiroSSEEvent, *http.Response, error) {
	inputTokens := kiroEstimateInputTokens(req)
	if kiroOnlyWebSearchTool(req) {
		query := kiroExtractSearchQuery(req)
		if query == "" {
			return nil, nil, ErrBadRequest("无法从消息中提取搜索查询")
		}
		results, failure, err := kiroCallMCPSearch(ctx, account, query, proxyOverride)
		if err != nil || failure != nil {
			return nil, failure, err
		}
		summary := kiroSearchSummary(query, results)
		content := append(kiroSearchPresentation(query, results), map[string]any{"type": "text", "text": summary})
		usage := map[string]any{"input_tokens": inputTokens, "output_tokens": (len(summary) + 3) / 4, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "server_tool_use": map[string]any{"web_search_requests": 1}}
		return kiroEventsFromMessage(req.Model, content, "end_turn", usage), nil, nil
	}

	working := *req
	working.Messages = append([]kiroInboundMessage(nil), req.Messages...)
	working.Tools = make([]kiroInboundTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if kiroIsNativeWebSearchTool(tool) {
			tool = kiroInboundTool{Name: "web_search", Description: "Search the web for current information and return the top results.", InputSchema: kiroWebSearchToolSchema}
		}
		working.Tools = append(working.Tools, tool)
	}
	var presentation []any
	totals := map[string]any{}
	searches := 0
	for round := 0; ; round++ {
		conversion, err := convertKiroRequest(&working, kiroPurposeGenerate)
		if err != nil {
			return nil, nil, ErrBadRequest(err.Error())
		}
		upstream, failure, err := kiroOpenUpstream(ctx, account, working.Model, conversion, proxyOverride)
		if err != nil || failure != nil {
			return nil, failure, err
		}
		stream := newKiroStreamContext(working.Model, kiroEstimateInputTokens(&working), working.Thinking.enabled(), conversion)
		var events []kiroSSEEvent
		kiroDrainUpstream(upstream, stream, func(event kiroSSEEvent) error { events = append(events, event); return nil })
		message, problem := kiroAggregateEvents(events)
		if problem != nil {
			encoded, _ := json.Marshal(map[string]any{"type": "error", "error": problem})
			errorType, _ := problem["type"].(string)
			return nil, kiroSyntheticResponse(kiroFailureStatus(errorType), encoded), nil
		}
		kiroAddUsage(totals, message["usage"])
		var text, searchUses, clientUses, others []any
		for _, raw := range message["content"].([]any) {
			block, _ := raw.(map[string]any)
			switch {
			case block["type"] == "tool_use" && block["name"] == "web_search":
				searchUses = append(searchUses, block)
			case block["type"] == "tool_use":
				clientUses = append(clientUses, block)
			case block["type"] == "text":
				text = append(text, block)
			default:
				others = append(others, block)
			}
		}
		stopReason, _ := message["stop_reason"].(string)
		if len(searchUses) == 0 || round >= kiroMaxWebSearchRounds {
			content := append(append(append(presentation, others...), text...), clientUses...)
			if len(searchUses) > 0 && len(clientUses) == 0 {
				stopReason = "end_turn"
			}
			totals["server_tool_use"] = map[string]any{"web_search_requests": searches}
			return kiroEventsFromMessage(req.Model, content, stopReason, totals), nil, nil
		}
		assistant := append(append([]any{}, text...), searchUses...)
		var results []any
		for _, raw := range searchUses {
			use := raw.(map[string]any)
			input, _ := use["input"].(map[string]any)
			query, _ := input["query"].(string)
			found, failure, err := kiroCallMCPSearch(ctx, account, query, proxyOverride)
			if err != nil || failure != nil {
				return nil, failure, err
			}
			searches++
			presentation = append(presentation, kiroSearchPresentation(query, found)...)
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": use["id"], "content": kiroSearchSummary(query, found)})
		}
		assistantContent, _ := json.Marshal(assistant)
		userContent, _ := json.Marshal(results)
		working.Messages = append(working.Messages, kiroInboundMessage{Role: "assistant", Content: assistantContent}, kiroInboundMessage{Role: "user", Content: userContent})
		if len(clientUses) > 0 {
			// 客户端工具不进循环：本轮搜索照常呈现，客户端工具原样交回。
			content := append(append(append(presentation, others...), text...), clientUses...)
			totals["server_tool_use"] = map[string]any{"web_search_requests": searches}
			return kiroEventsFromMessage(req.Model, content, "tool_use", totals), nil, nil
		}
	}
}

func kiroAddUsage(totals map[string]any, raw any) {
	usage, _ := raw.(map[string]any)
	for _, key := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		totals[key] = kiroUsageInt(totals, key) + kiroUsageInt(usage, key)
	}
	if credits, ok := usage["credit_usage"].(float64); ok {
		previous, _ := totals["credit_usage"].(float64)
		totals["credit_usage"] = previous + credits
		totals["credit_unit"], totals["credit_unit_plural"] = usage["credit_unit"], usage["credit_unit_plural"]
	}
}

// kiroEventsFromMessage 把一条完整 message 还原成标准 Anthropic 事件序列。
func kiroEventsFromMessage(model string, content []any, stopReason string, usage map[string]any) []kiroSSEEvent {
	start := map[string]any{"input_tokens": kiroUsageInt(usage, "input_tokens"), "output_tokens": 1, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
	events := []kiroSSEEvent{{"message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "type": "message", "role": "assistant", "content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil, "usage": start,
	}}}}
	for index, raw := range content {
		block, _ := raw.(map[string]any)
		startBlock := map[string]any{}
		for key, value := range block {
			startBlock[key] = value
		}
		var deltas []map[string]any
		switch block["type"] {
		case "text":
			startBlock["text"] = ""
			deltas = append(deltas, map[string]any{"type": "text_delta", "text": block["text"]})
		case "thinking":
			startBlock["thinking"], startBlock["signature"] = "", nil
			delete(startBlock, "signature")
			deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": block["thinking"]}, map[string]any{"type": "signature_delta", "signature": block["signature"]})
		case "tool_use":
			startBlock["input"] = map[string]any{}
			encoded, _ := json.Marshal(block["input"])
			deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": string(encoded)})
		}
		events = append(events, kiroSSEEvent{"content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": startBlock}})
		for _, delta := range deltas {
			events = append(events, kiroSSEEvent{"content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta}})
		}
		events = append(events, kiroBlockStopEvent(index))
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}
	return append(events,
		kiroSSEEvent{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": usage}},
		kiroSSEEvent{"message_stop", map[string]any{"type": "message_stop"}})
}

// executeKiroWebSearchMessages 是 /v1/messages 的托管 web_search 入口。
func executeKiroWebSearchMessages(ctx context.Context, account *auth.Account, req *kiroInboundRequest, proxyOverride string) (*http.Response, error) {
	events, failure, err := runKiroWebSearch(ctx, account, req, proxyOverride)
	if err != nil || failure != nil {
		return failure, err
	}
	if !req.Stream {
		message, _ := kiroAggregateEvents(events)
		start, _ := events[0].Data["message"].(map[string]any)
		message["id"], message["model"] = start["id"], req.Model
		encoded, _ := json.Marshal(message)
		return kiroSyntheticResponse(http.StatusOK, encoded), nil
	}
	var body bytes.Buffer
	for _, event := range events {
		body.Write(event.bytes())
	}
	return kiroSSEResponse(body.Bytes()), nil
}

// executeKiroWebSearchResponses 是 /v1/responses 的托管 web_search 入口。
func executeKiroWebSearchResponses(ctx context.Context, account *auth.Account, req *kiroInboundRequest, translation *kiroResponsesTranslation, proxyOverride string) (*http.Response, error) {
	events, failure, err := runKiroWebSearch(ctx, account, req, proxyOverride)
	if err != nil || failure != nil {
		return failure, err
	}
	responses := newKiroResponsesStream(translation)
	if translation.stream {
		chunks := responses.initialEvents()
		for _, event := range events {
			chunks = append(chunks, responses.handle(event)...)
		}
		return kiroSSEResponse(bytes.Join(chunks, nil)), nil
	}
	for _, event := range events {
		responses.handle(event)
	}
	return kiroResponsesJSON(responses), nil
}
