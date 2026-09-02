package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GrokUpstreamRoute describes the route chosen for a single selected account.
// Handlers must resolve it inside each retry attempt because another account may
// expose the same model through a different backend.
type GrokUpstreamRoute struct {
	Model        string
	Protocol     GrokProtocol
	BaseURL      string
	Endpoint     string
	ExtraHeaders http.Header
	Native       bool
}

const grokNativeRouteHeader = "X-Codex2api-Grok-Native-Route"

func markGrokNativeRoute(resp *http.Response, route GrokUpstreamRoute, inbound GrokProtocol) {
	if resp == nil || !route.Native || route.Protocol != auth.NormalizeGrokProtocol(string(inbound)) {
		return
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	// This is process-local metadata for the HTTP handlers. It is removed before
	// downstream response headers are copied and never reaches the client.
	resp.Header.Set(grokNativeRouteHeader, "1")
}

func isGrokNativeRouteResponse(resp *http.Response) bool {
	return resp != nil && resp.Header != nil && resp.Header.Get(grokNativeRouteHeader) == "1"
}

func grokProtocolSuffix(protocol GrokProtocol) string {
	switch auth.NormalizeGrokProtocol(string(protocol)) {
	case GrokProtocolChatCompletions:
		return "/v1/chat/completions"
	case GrokProtocolMessages:
		return "/v1/messages"
	default:
		return "/v1/responses"
	}
}

func routeHeaders(raw map[string]string) http.Header {
	if len(raw) == 0 {
		return nil
	}
	result := make(http.Header)
	for key, value := range raw {
		if _, blocked := blockedGrokModelHeaders[strings.ToLower(strings.TrimSpace(key))]; blocked {
			continue
		}
		if key = http.CanonicalHeaderKey(strings.TrimSpace(key)); key != "" && !strings.ContainsAny(value, "\r\n") {
			result.Set(key, value)
		}
	}
	return result
}

// ResolveGrokUpstreamRoute uses the catalog apiBackend. A fresh same-protocol
// probe only marks Native passthrough; it cannot override a different catalog
// backend. Missing catalogs use Responses for the conservative built-in model
// set so existing OAuth/API-key accounts stay usable until sync.
func ResolveGrokUpstreamRoute(account *auth.Account, model string, inbound GrokProtocol, now time.Time) GrokUpstreamRoute {
	baseURL, _ := account.GrokCredentials()
	resolved := GrokUpstreamRoute{
		Model: strings.TrimSpace(model), Protocol: GrokProtocolResponses,
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
	if route, ok := account.GetGrokModelRoute(model, inbound, now); ok {
		resolved.Protocol = route.Protocol
		resolved.BaseURL = route.BaseURL
		resolved.ExtraHeaders = routeHeaders(route.ExtraHeaders)
		resolved.Native = route.Native
	}
	resolved.Endpoint = auth.OpenAIResponsesEndpoint(resolved.BaseURL, grokProtocolSuffix(resolved.Protocol))
	return resolved
}

func GrokVisibleModelIDsForAccount(account *auth.Account) []string {
	if account == nil || !account.IsGrokAPI() {
		return nil
	}
	models := account.GrokCatalogModels()
	if !account.HasGrokModelCatalog() {
		return DefaultGrokModelIDsForAccount(account)
	}
	result := make([]string, 0, len(models))
	for _, model := range models {
		if model.Hidden || (account.GrokAuthKind() == auth.GrokAuthKindAPIKey && model.SupportedInAPI != nil && !*model.SupportedInAPI) {
			continue
		}
		result = append(result, model.ModelID)
	}
	return auth.NormalizeAccountModels(result)
}

func GrokModelRoutable(account *auth.Account, model string, inbound GrokProtocol, now time.Time) bool {
	if account == nil || !account.IsGrokAPI() {
		return false
	}
	if _, ok := account.GetGrokModelRoute(model, inbound, now); ok {
		return true
	}
	// A non-empty catalog is authoritative for model presence. Falling back to
	// built-ins here would resurrect a model explicitly absent (or hidden) in a
	// successfully fetched account catalog.
	if account.HasGrokModelCatalog() {
		return false
	}
	return modelIDInList(model, DefaultGrokModelIDsForAccount(account))
}

func responsesContentPartsToChat(content gjson.Result, role string) any {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	parts := make([]any, 0)
	content.ForEach(func(_, part gjson.Result) bool {
		partType := strings.TrimSpace(part.Get("type").String())
		switch partType {
		case "input_image", "image_url":
			url := part.Get("image_url").String()
			if url == "" {
				url = part.Get("image_url.url").String()
			}
			if url != "" && role == "user" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		default:
			text := part.Get("text").String()
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		}
		return true
	})
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		if part, ok := parts[0].(map[string]any); ok && part["type"] == "text" {
			return part["text"]
		}
	}
	return parts
}

func responsesToolsToChat(tools gjson.Result) []any {
	if !tools.IsArray() {
		return nil
	}
	var result []any
	tools.ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("type").String() != "function" {
			return true
		}
		fn := map[string]any{"name": tool.Get("name").String()}
		if fn["name"] == "" {
			fn["name"] = tool.Get("function.name").String()
		}
		if description := tool.Get("description"); description.Exists() {
			fn["description"] = description.Value()
		}
		if params := tool.Get("parameters"); params.Exists() {
			fn["parameters"] = params.Value()
		} else if params := tool.Get("function.parameters"); params.Exists() {
			fn["parameters"] = params.Value()
		}
		result = append(result, map[string]any{"type": "function", "function": fn})
		return true
	})
	return result
}

func meaningfulGrokJSONValue(value gjson.Result) bool {
	if !value.Exists() || value.Type == gjson.Null {
		return false
	}
	if value.Type == gjson.String {
		return value.String() != ""
	}
	return true
}

func responsesToolChoiceToChat(choice gjson.Result) (any, error) {
	if !choice.Exists() || choice.Type == gjson.Null {
		return nil, nil
	}
	if choice.Type == gjson.String {
		switch choice.String() {
		case "auto", "none", "required":
			return choice.String(), nil
		default:
			return nil, fmt.Errorf("Responses tool_choice %q cannot be represented by Chat Completions", choice.String())
		}
	}
	choiceType := strings.TrimSpace(choice.Get("type").String())
	switch choiceType {
	case "function":
		name := strings.TrimSpace(choice.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(choice.Get("function.name").String())
		}
		if name == "" {
			return nil, fmt.Errorf("Responses function tool_choice requires name")
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}, nil
	case "auto", "none", "required":
		return choiceType, nil
	default:
		return nil, fmt.Errorf("Responses tool_choice type %q cannot be represented by Chat Completions", choiceType)
	}
}

func responsesToolChoiceToMessages(choice gjson.Result, parallel gjson.Result) (any, error) {
	var result map[string]any
	if choice.Exists() && choice.Type != gjson.Null {
		if choice.Type == gjson.String {
			switch choice.String() {
			case "auto":
				result = map[string]any{"type": "auto"}
			case "required":
				result = map[string]any{"type": "any"}
			default:
				return nil, fmt.Errorf("Responses tool_choice %q cannot be represented by Messages", choice.String())
			}
		} else {
			choiceType := strings.TrimSpace(choice.Get("type").String())
			switch choiceType {
			case "function":
				name := strings.TrimSpace(choice.Get("name").String())
				if name == "" {
					name = strings.TrimSpace(choice.Get("function.name").String())
				}
				if name == "" {
					return nil, fmt.Errorf("Responses function tool_choice requires name")
				}
				result = map[string]any{"type": "tool", "name": name}
			case "auto":
				result = map[string]any{"type": "auto"}
			case "required":
				result = map[string]any{"type": "any"}
			default:
				return nil, fmt.Errorf("Responses tool_choice type %q cannot be represented by Messages", choiceType)
			}
		}
	}
	if parallel.Exists() && !parallel.Bool() {
		if result == nil {
			result = map[string]any{"type": "auto"}
		}
		result["disable_parallel_tool_use"] = true
	}
	return result, nil
}

func validateCrossProtocolTools(tools gjson.Result, target string) error {
	if !tools.IsArray() {
		return nil
	}
	var conversionErr error
	tools.ForEach(func(_, tool gjson.Result) bool {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if toolType == "function" {
			return true
		}
		conversionErr = fmt.Errorf("Responses tool type %q cannot be represented by %s", toolType, target)
		return false
	})
	return conversionErr
}

func responsesTextFormatToChat(format gjson.Result) (any, error) {
	if !format.Exists() {
		return nil, nil
	}
	switch strings.TrimSpace(format.Get("type").String()) {
	case "", "text":
		return nil, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		name := strings.TrimSpace(format.Get("name").String())
		schema := format.Get("schema")
		if name == "" || !schema.IsObject() {
			return nil, fmt.Errorf("Responses text.format json_schema requires name and schema")
		}
		jsonSchema := map[string]any{"name": name, "schema": schema.Value()}
		if strict := format.Get("strict"); strict.Exists() {
			jsonSchema["strict"] = strict.Bool()
		}
		return map[string]any{"type": "json_schema", "json_schema": jsonSchema}, nil
	default:
		return nil, fmt.Errorf("Responses text.format type %q cannot be represented by Chat Completions", format.Get("type").String())
	}
}

func responsesTextFormatToMessages(format gjson.Result) (any, error) {
	if !format.Exists() {
		return nil, nil
	}
	typeName := strings.TrimSpace(format.Get("type").String())
	switch typeName {
	case "", "text":
		return nil, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		name := strings.TrimSpace(format.Get("name").String())
		schema := format.Get("schema")
		if name == "" || !schema.IsObject() {
			return nil, fmt.Errorf("Responses text.format json_schema requires name and schema")
		}
		jsonSchema := map[string]any{"name": name, "schema": schema.Value()}
		if strict := format.Get("strict"); strict.Exists() {
			jsonSchema["strict"] = strict.Bool()
		}
		return map[string]any{"type": "json_schema", "json_schema": jsonSchema}, nil
	default:
		return nil, fmt.Errorf("Responses text.format type %q cannot be represented by Messages", typeName)
	}
}

// convertResponsesToChatRequest preserves the ordered Responses input. Unknown
// item types with meaningful content are rejected rather than silently lost.
func convertResponsesToChatRequest(body []byte) ([]byte, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("Responses request must be a JSON object")
	}
	out := map[string]any{"model": root.Get("model").String(), "stream": true, "stream_options": map[string]any{"include_usage": true}}
	var messages []any
	if instructions := root.Get("instructions"); instructions.Exists() && strings.TrimSpace(instructions.String()) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions.String()})
	}
	var pendingReasoning strings.Builder
	knownCalls := make(map[string]struct{})
	input := root.Get("input")
	if input.Type == gjson.String {
		messages = append(messages, map[string]any{"role": "user", "content": input.String()})
	} else if input.IsArray() {
		var conversionErr error
		input.ForEach(func(_, item gjson.Result) bool {
			itemType := strings.TrimSpace(item.Get("type").String())
			if itemType == "" && item.Get("role").Exists() {
				itemType = "message"
			}
			switch itemType {
			case "message":
				role := item.Get("role").String()
				message := map[string]any{"role": role, "content": responsesContentPartsToChat(item.Get("content"), role)}
				if role == "assistant" && pendingReasoning.Len() > 0 {
					message["reasoning_content"] = pendingReasoning.String()
					pendingReasoning.Reset()
				}
				messages = append(messages, message)
			case "reasoning":
				if meaningfulGrokJSONValue(item.Get("encrypted_content")) {
					conversionErr = fmt.Errorf("Responses encrypted reasoning cannot be represented by Chat Completions")
					return false
				}
				item.Get("summary").ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text").String(); text != "" {
						if pendingReasoning.Len() > 0 {
							pendingReasoning.WriteByte('\n')
						}
						pendingReasoning.WriteString(text)
					}
					return true
				})
			case "function_call":
				callID := strings.TrimSpace(item.Get("call_id").String())
				if callID == "" {
					conversionErr = fmt.Errorf("function_call requires call_id")
					return false
				}
				knownCalls[callID] = struct{}{}
				call := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": item.Get("name").String(), "arguments": item.Get("arguments").String()}}
				messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}})
			case "function_call_output":
				callID := item.Get("call_id").String()
				if callID == "" {
					conversionErr = fmt.Errorf("orphan function_call_output without call_id")
					return false
				}
				if _, ok := knownCalls[callID]; !ok {
					conversionErr = fmt.Errorf("orphan function_call_output for unknown call_id %q", callID)
					return false
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": item.Get("output").String()})
			case "":
				return true
			default:
				conversionErr = fmt.Errorf("Responses input item type %q cannot be represented by Chat Completions", itemType)
				return false
			}
			return true
		})
		if conversionErr != nil {
			return nil, conversionErr
		}
	}
	if pendingReasoning.Len() > 0 {
		messages = append(messages, map[string]any{"role": "assistant", "content": nil, "reasoning_content": pendingReasoning.String()})
	}
	out["messages"] = messages
	if err := validateCrossProtocolTools(root.Get("tools"), "Chat Completions"); err != nil {
		return nil, err
	}
	if tools := responsesToolsToChat(root.Get("tools")); len(tools) > 0 {
		out["tools"] = tools
	}
	choice, err := responsesToolChoiceToChat(root.Get("tool_choice"))
	if err != nil {
		return nil, err
	}
	if choice != nil {
		out["tool_choice"] = choice
	}
	if parallel := root.Get("parallel_tool_calls"); parallel.Exists() {
		out["parallel_tool_calls"] = parallel.Bool()
	}
	if effort := root.Get("reasoning.effort").String(); effort != "" {
		out["reasoning_effort"] = effort
	}
	responseFormat, err := responsesTextFormatToChat(root.Get("text.format"))
	if err != nil {
		return nil, err
	}
	if responseFormat != nil {
		out["response_format"] = responseFormat
	}
	for _, field := range []string{"temperature", "top_p", "max_output_tokens", "max_tokens", "max_completion_tokens", "stop", "seed", "presence_penalty", "frequency_penalty"} {
		if value := root.Get(field); value.Exists() {
			name := field
			if field == "max_output_tokens" || field == "max_tokens" {
				name = "max_completion_tokens"
			}
			out[name] = value.Value()
		}
	}
	return json.Marshal(out)
}

func responsesImageToAnthropic(part gjson.Result) (map[string]any, error) {
	url := part.Get("image_url").String()
	if url == "" {
		url = part.Get("image_url.url").String()
	}
	if strings.HasPrefix(url, "data:") {
		rest := strings.TrimPrefix(url, "data:")
		mediaType, data, ok := strings.Cut(rest, ";base64,")
		if ok && strings.TrimSpace(mediaType) != "" && data != "" {
			return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}}, nil
		}
		return nil, fmt.Errorf("Responses data URI image is not valid base64 image syntax")
	}
	if strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("Responses image content requires image_url")
	}
	return nil, fmt.Errorf("Responses remote image URLs cannot be represented by Messages; use a data URI")
}

func responsesContentToAnthropic(content gjson.Result, allowImages bool) ([]any, error) {
	if content.Type == gjson.String {
		return []any{map[string]any{"type": "text", "text": content.String()}}, nil
	}
	if !content.IsArray() {
		if !content.Exists() || content.Type == gjson.Null {
			return nil, nil
		}
		return nil, fmt.Errorf("Responses message content must be a string or array for Messages")
	}
	var blocks []any
	var conversionErr error
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "input_image", "image_url":
			if !allowImages {
				conversionErr = fmt.Errorf("Responses images are only representable in Messages user content")
				return false
			}
			image, err := responsesImageToAnthropic(part)
			if err != nil {
				conversionErr = err
				return false
			}
			blocks = append(blocks, image)
		case "input_text", "output_text", "text", "refusal":
			if text := part.Get("text").String(); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		default:
			conversionErr = fmt.Errorf("Responses content part type %q cannot be represented by Messages", part.Get("type").String())
			return false
		}
		return true
	})
	return blocks, conversionErr
}

func convertResponsesToMessagesRequest(body []byte) ([]byte, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("Responses request must be a JSON object")
	}
	out := map[string]any{"model": root.Get("model").String(), "stream": true}
	maxTokens := root.Get("max_output_tokens").Int()
	if maxTokens <= 0 {
		maxTokens = root.Get("max_tokens").Int()
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	out["max_tokens"] = maxTokens
	var system []any
	if instructions := root.Get("instructions"); instructions.Exists() && strings.TrimSpace(instructions.String()) != "" {
		system = append(system, map[string]any{"type": "text", "text": instructions.String()})
	}
	var messages []any
	knownCalls := make(map[string]struct{})
	input := root.Get("input")
	if input.Type == gjson.String {
		messages = append(messages, map[string]any{"role": "user", "content": input.String()})
	} else if input.IsArray() {
		var conversionErr error
		input.ForEach(func(_, item gjson.Result) bool {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").Exists() {
				itemType = "message"
			}
			switch itemType {
			case "message":
				role := strings.ToLower(item.Get("role").String())
				blocks, err := responsesContentToAnthropic(item.Get("content"), role == "user")
				if err != nil {
					conversionErr = err
					return false
				}
				if role == "system" || role == "developer" {
					system = append(system, blocks...)
				} else if role == "user" || role == "assistant" {
					messages = append(messages, map[string]any{"role": role, "content": blocks})
				}
			case "reasoning":
				var thinking strings.Builder
				item.Get("summary").ForEach(func(_, part gjson.Result) bool { thinking.WriteString(part.Get("text").String()); return true })
				signature := item.Get("encrypted_content").String()
				if thinking.Len() > 0 || signature != "" {
					messages = append(messages, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": thinking.String(), "signature": signature}}})
				}
			case "function_call":
				callID := strings.TrimSpace(item.Get("call_id").String())
				if callID == "" {
					conversionErr = fmt.Errorf("function_call requires call_id")
					return false
				}
				var inputValue any = map[string]any{}
				if raw := item.Get("arguments").String(); raw != "" {
					if err := json.Unmarshal([]byte(raw), &inputValue); err != nil {
						conversionErr = fmt.Errorf("function_call arguments must be valid JSON: %w", err)
						return false
					}
				}
				knownCalls[callID] = struct{}{}
				messages = append(messages, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": callID, "name": item.Get("name").String(), "input": inputValue}}})
			case "function_call_output":
				callID := item.Get("call_id").String()
				if callID == "" {
					conversionErr = fmt.Errorf("orphan function_call_output without call_id")
					return false
				}
				if _, ok := knownCalls[callID]; !ok {
					conversionErr = fmt.Errorf("orphan function_call_output for unknown call_id %q", callID)
					return false
				}
				messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": item.Get("output").String()}}})
			default:
				conversionErr = fmt.Errorf("Responses input item type %q cannot be represented by Messages", itemType)
				return false
			}
			return true
		})
		if conversionErr != nil {
			return nil, conversionErr
		}
	}
	out["messages"] = messages
	if len(system) > 0 {
		out["system"] = system
	}
	if err := validateCrossProtocolTools(root.Get("tools"), "Messages"); err != nil {
		return nil, err
	}
	if tools := root.Get("tools"); tools.IsArray() {
		var converted []any
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "function" {
				converted = append(converted, map[string]any{"name": tool.Get("name").String(), "description": tool.Get("description").String(), "input_schema": tool.Get("parameters").Value()})
			}
			return true
		})
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	toolChoice, err := responsesToolChoiceToMessages(root.Get("tool_choice"), root.Get("parallel_tool_calls"))
	if err != nil {
		return nil, err
	}
	if toolChoice != nil {
		out["tool_choice"] = toolChoice
	}
	outputConfig := map[string]any{}
	if effort := root.Get("reasoning.effort").String(); effort != "" {
		out["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
		outputConfig["effort"] = effort
	}
	format, err := responsesTextFormatToMessages(root.Get("text.format"))
	if err != nil {
		return nil, err
	}
	if format != nil {
		outputConfig["format"] = format
	}
	if len(outputConfig) > 0 {
		out["output_config"] = outputConfig
	}
	if stop := root.Get("stop"); stop.Exists() {
		if stop.Type == gjson.String {
			out["stop_sequences"] = []string{stop.String()}
		} else if stop.IsArray() {
			out["stop_sequences"] = stop.Value()
		} else {
			return nil, fmt.Errorf("Responses stop must be a string or string array for Messages")
		}
	}
	for _, field := range []string{"temperature", "top_p"} {
		if value := root.Get(field); value.Exists() {
			out[field] = value.Value()
		}
	}
	return json.Marshal(out)
}

func eventSSE(data []byte) []byte {
	result := make([]byte, 0, len(data)+8)
	result = append(result, "data: "...)
	result = append(result, data...)
	result = append(result, '\n', '\n')
	return result
}

func responseEvent(eventType string, fields map[string]any) []byte {
	fields["type"] = eventType
	data, _ := json.Marshal(fields)
	return eventSSE(data)
}

type chatToResponsesReader struct {
	source      io.ReadCloser
	reader      *bufio.Reader
	queue       bytes.Buffer
	responseID  string
	model       string
	createdAt   int64
	created     bool
	terminal    bool
	output      []chatResponseOutputRef
	tools       map[int]*chatResponseTool
	reasoningID string
	messageID   string
	text        strings.Builder
	reasoning   strings.Builder
	// pendingStatus 记录已收到 finish_reason 但尚未发出的终态:include_usage 流
	// 会在 finish chunk 之后、[DONE] 之前追加一个 choices 为空的 usage-only chunk,
	// 终态必须等到拿到 usage(或流结束)再发,否则用量统计恒为 0。
	pendingStatus string
	usage         map[string]any
}

type chatResponseOutputKind uint8

const (
	chatResponseOutputReasoning chatResponseOutputKind = iota + 1
	chatResponseOutputMessage
	chatResponseOutputTool
)

type chatResponseOutputRef struct {
	kind      chatResponseOutputKind
	toolIndex int
}

type chatResponseTool struct {
	id              string
	name            string
	arguments       strings.Builder
	outputIndex     int
	itemAdded       bool
	emittedArgsSize int
}

func newChatToResponsesReader(source io.ReadCloser, model string) io.ReadCloser {
	return &chatToResponsesReader{
		source: source, reader: bufio.NewReader(source),
		responseID: "resp_" + uuid.NewString(), model: model, createdAt: time.Now().Unix(),
		tools: make(map[int]*chatResponseTool),
	}
}

func (r *chatToResponsesReader) response(status string) map[string]any {
	return map[string]any{
		"id": r.responseID, "object": "response", "created_at": r.createdAt,
		"status": status, "model": r.model,
	}
}

func (r *chatToResponsesReader) ensureReasoningOutput() {
	if r.reasoningID != "" {
		return
	}
	r.reasoningID = "rs_" + uuid.NewString()
	r.output = append(r.output, chatResponseOutputRef{kind: chatResponseOutputReasoning})
}

func (r *chatToResponsesReader) ensureMessageOutput() {
	if r.messageID != "" {
		return
	}
	r.messageID = "msg_" + uuid.NewString()
	r.output = append(r.output, chatResponseOutputRef{kind: chatResponseOutputMessage})
}

func (r *chatToResponsesReader) ensureToolOutput(index int) *chatResponseTool {
	if tool := r.tools[index]; tool != nil {
		return tool
	}
	tool := &chatResponseTool{outputIndex: len(r.output)}
	r.tools[index] = tool
	r.output = append(r.output, chatResponseOutputRef{kind: chatResponseOutputTool, toolIndex: index})
	return tool
}

func (r *chatToResponsesReader) enqueueToolProgress(tool *chatResponseTool) {
	if tool == nil || tool.id == "" || tool.name == "" {
		return
	}
	if !tool.itemAdded {
		r.queue.Write(responseEvent("response.output_item.added", map[string]any{"output_index": tool.outputIndex, "item": map[string]any{"id": tool.id, "type": "function_call", "call_id": tool.id, "name": tool.name, "arguments": "", "status": "in_progress"}}))
		tool.itemAdded = true
	}
	arguments := tool.arguments.String()
	if tool.emittedArgsSize >= len(arguments) {
		return
	}
	delta := arguments[tool.emittedArgsSize:]
	tool.emittedArgsSize = len(arguments)
	r.queue.Write(responseEvent("response.function_call_arguments.delta", map[string]any{"output_index": tool.outputIndex, "item_id": tool.id, "call_id": tool.id, "delta": delta}))
}

func chatResponseToolItem(tool *chatResponseTool, status string) map[string]any {
	return map[string]any{
		"id": tool.id, "type": "function_call", "call_id": tool.id,
		"name": tool.name, "arguments": tool.arguments.String(), "status": status,
	}
}

func (r *chatToResponsesReader) enqueueToolCompletion(tool *chatResponseTool) map[string]any {
	if tool.id == "" {
		tool.id = "call_" + uuid.NewString()
	}
	if tool.name == "" {
		return chatResponseToolItem(tool, "incomplete")
	}
	r.enqueueToolProgress(tool)
	item := chatResponseToolItem(tool, "completed")
	r.queue.Write(responseEvent("response.function_call_arguments.done", map[string]any{
		"output_index": tool.outputIndex, "item_id": tool.id, "call_id": tool.id,
		"arguments": tool.arguments.String(),
	}))
	r.queue.Write(responseEvent("response.output_item.done", map[string]any{"output_index": tool.outputIndex, "item": item}))
	return item
}

func (r *chatToResponsesReader) enqueueCreated() {
	if r.created {
		return
	}
	r.created = true
	r.queue.Write(responseEvent("response.created", map[string]any{"response": r.response("in_progress")}))
}

func chatFinishReasonStatus(reason string) string {
	switch strings.ToLower(reason) {
	case "length":
		return "incomplete"
	case "content_filter":
		return "failed"
	default:
		return "completed"
	}
}

func (r *chatToResponsesReader) translate(data []byte) {
	r.enqueueCreated()
	chunk := gjson.ParseBytes(data)
	if model := chunk.Get("model").String(); model != "" {
		r.model = model
	}
	if chunk.Get("type").String() == "error" || chunk.Get("error").Exists() {
		code := chunk.Get("error.code").String()
		if code == "" {
			code = chunk.Get("error.type").String()
		}
		if code == "" {
			code = ErrorCodeUpstreamError
		}
		message := chunk.Get("error.message").String()
		if message == "" {
			message = "upstream Chat Completions stream failed"
		}
		response := r.response("failed")
		response["error"] = map[string]any{"code": code, "message": message}
		r.queue.Write(responseEvent("response.failed", map[string]any{"response": response}))
		r.terminal = true
		return
	}
	chunkUsage := chatUsagePayload(chunk)
	if chunkUsage != nil {
		r.usage = chunkUsage
	}
	choices := chunk.Get("choices")
	if !choices.IsArray() || len(choices.Array()) == 0 {
		// include_usage 的独立 usage chunk(choices: [])在 finish chunk 之后到达;
		// 拿到 usage 后补发之前挂起的终态。
		if r.pendingStatus != "" && r.usage != nil {
			r.emitChatTerminal(r.pendingStatus)
		}
		return
	}
	choice := choices.Get("0")
	if reasoning := choice.Get("delta.reasoning_content").String(); reasoning != "" {
		r.ensureReasoningOutput()
		r.reasoning.WriteString(reasoning)
		r.queue.Write(responseEvent("response.reasoning_summary_text.delta", map[string]any{"delta": reasoning, "response_id": r.responseID}))
	}
	if text := choice.Get("delta.content").String(); text != "" {
		r.ensureMessageOutput()
		r.text.WriteString(text)
		r.queue.Write(responseEvent("response.output_text.delta", map[string]any{"delta": text, "response_id": r.responseID}))
	}
	choice.Get("delta.tool_calls").ForEach(func(_, tool gjson.Result) bool {
		index := int(tool.Get("index").Int())
		state := r.ensureToolOutput(index)
		id := tool.Get("id").String()
		name := tool.Get("function.name").String()
		// Chat tool metadata normally arrives only on the first delta. Later
		// argument-only/name-only deltas must not erase or rewrite the stable
		// upstream call identity.
		if state.id == "" && id != "" {
			state.id = id
		}
		if state.name == "" && name != "" {
			state.name = name
		}
		if delta := tool.Get("function.arguments").String(); delta != "" {
			state.arguments.WriteString(delta)
		}
		r.enqueueToolProgress(state)
		return true
	})
	finish := choice.Get("finish_reason").String()
	if finish == "" {
		return
	}
	status := chatFinishReasonStatus(finish)
	// finish chunk 自带 usage(或属失败终态)时立即发出;否则挂起,等 usage-only
	// chunk([DONE]/EOF 时兜底)再发,避免把 include_usage 的用量丢成 0。
	if status == "failed" || chunkUsage != nil {
		r.emitChatTerminal(status)
		return
	}
	r.pendingStatus = status
}

// chatUsagePayload 把 Chat chunk 的 usage 对象转成 Responses usage 形状;
// usage 缺失或为 null 时返回 nil。
func chatUsagePayload(chunk gjson.Result) map[string]any {
	usage := chunk.Get("usage")
	if !usage.Exists() || !usage.IsObject() {
		return nil
	}
	payload := map[string]any{
		"input_tokens":  usage.Get("prompt_tokens").Int(),
		"output_tokens": usage.Get("completion_tokens").Int(),
		"total_tokens":  usage.Get("total_tokens").Int(),
	}
	if cached := usage.Get("prompt_tokens_details.cached_tokens"); cached.Exists() {
		payload["input_tokens_details"] = map[string]any{"cached_tokens": cached.Int()}
	}
	if reasoning := usage.Get("completion_tokens_details.reasoning_tokens"); reasoning.Exists() {
		payload["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning.Int()}
	}
	return payload
}

func (r *chatToResponsesReader) emitChatTerminal(status string) {
	usage := r.usage
	if usage == nil {
		usage = map[string]any{"input_tokens": int64(0), "output_tokens": int64(0), "total_tokens": int64(0)}
	}
	output := make([]any, 0, len(r.output))
	for _, item := range r.output {
		switch item.kind {
		case chatResponseOutputReasoning:
			if r.reasoning.Len() > 0 {
				output = append(output, map[string]any{"id": r.reasoningID, "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": r.reasoning.String()}}, "status": "completed"})
			}
		case chatResponseOutputMessage:
			if r.text.Len() > 0 {
				output = append(output, map[string]any{"id": r.messageID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": r.text.String(), "annotations": []any{}}}})
			}
		case chatResponseOutputTool:
			tool := r.tools[item.toolIndex]
			if tool == nil {
				continue
			}
			if status == "completed" {
				output = append(output, r.enqueueToolCompletion(tool))
				continue
			}
			if tool.id == "" {
				tool.id = "call_" + uuid.NewString()
			}
			output = append(output, chatResponseToolItem(tool, "incomplete"))
		}
	}
	response := r.response(status)
	response["output"] = output
	response["usage"] = usage
	if status == "completed" || status == "incomplete" {
		if status == "incomplete" {
			response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}
		r.queue.Write(responseEvent("response.completed", map[string]any{"response": response}))
	} else {
		response["error"] = map[string]any{"code": "content_filter", "message": "upstream content filter stopped the response"}
		r.queue.Write(responseEvent("response.failed", map[string]any{"response": response}))
	}
	r.terminal = true
	r.pendingStatus = ""
}

func readSSEDataLine(reader *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(line, []byte("data:")) {
				part := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, part...)
			}
			if len(line) == 0 && len(data) > 0 {
				return data, nil
			}
		}
		if err != nil {
			if len(data) > 0 {
				return data, nil
			}
			return nil, err
		}
	}
}

func (r *chatToResponsesReader) Read(p []byte) (int, error) {
	for r.queue.Len() == 0 {
		data, err := readSSEDataLine(r.reader)
		if err != nil {
			if err == io.EOF && !r.terminal {
				// 已收到 finish_reason 只是没等到 usage chunk:按挂起状态正常收尾,
				// 不能把它当成断流。
				if r.pendingStatus != "" {
					r.emitChatTerminal(r.pendingStatus)
					break
				}
				r.enqueueCreated()
				response := r.response("failed")
				response["error"] = map[string]any{"code": ErrorCodeUpstreamStreamBreak, "message": "upstream stream interrupted before completion"}
				r.queue.Write(responseEvent("response.failed", map[string]any{"response": response}))
				r.terminal = true
				break
			}
			return 0, err
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			if r.pendingStatus != "" {
				r.emitChatTerminal(r.pendingStatus)
				continue
			}
			if r.terminal {
				return 0, io.EOF
			}
			continue
		}
		r.translate(data)
	}
	return r.queue.Read(p)
}

func (r *chatToResponsesReader) Close() error { return r.source.Close() }

type messagesToResponsesReader struct {
	source     io.ReadCloser
	reader     *bufio.Reader
	queue      bytes.Buffer
	responseID string
	model      string
	createdAt  int64
	created    bool
	terminal   bool
	blocks     map[int]*messagesResponseBlock
	output     []*messagesResponseBlock
	message    *messagesResponseBlock
	usage      map[string]int64
	stopReason string
	stopSeen   bool
}

type messagesResponseBlock struct {
	blockType          string
	id                 string
	name               string
	text               strings.Builder
	reasoning          strings.Builder
	signature          strings.Builder
	arguments          strings.Builder
	outputIndex        int
	contentIndex       int
	owner              *messagesResponseBlock
	textParts          []*messagesResponseBlock
	itemAdded          bool
	itemDone           bool
	partAdded          bool
	partDone           bool
	summaryPartAdded   bool
	summaryPartDone    bool
	blockStopped       bool
	argumentsFromStart bool
	argumentsDeltaSeen bool
	emittedArguments   int
}

func newMessagesToResponsesReader(source io.ReadCloser, model string) io.ReadCloser {
	return &messagesToResponsesReader{
		source: source, reader: bufio.NewReader(source),
		responseID: "resp_" + uuid.NewString(), model: model, createdAt: time.Now().Unix(),
		blocks: make(map[int]*messagesResponseBlock), usage: make(map[string]int64),
	}
}

func (r *messagesToResponsesReader) response(status string) map[string]any {
	return map[string]any{
		"id": r.responseID, "object": "response", "created_at": r.createdAt,
		"status": status, "model": r.model,
	}
}

func (r *messagesToResponsesReader) ensureBlock(index int, blockType string) *messagesResponseBlock {
	if block := r.blocks[index]; block != nil {
		if block.blockType == "" {
			block.blockType = blockType
		}
		return block
	}
	block := &messagesResponseBlock{blockType: blockType}
	r.blocks[index] = block
	switch blockType {
	case "text":
		owner := r.message
		if owner != nil && len(owner.textParts) > 0 && !owner.textParts[len(owner.textParts)-1].blockStopped {
			// A second start without a stop is malformed. Do not manufacture done
			// events for the abandoned item: keep it in the terminal output, but
			// start a fresh message lifecycle for the new block.
			r.message = nil
			owner = nil
		}
		if owner == nil || owner.itemDone {
			owner = &messagesResponseBlock{
				blockType:   "message",
				id:          "msg_" + uuid.NewString(),
				outputIndex: len(r.output),
			}
			r.output = append(r.output, owner)
			r.message = owner
		}
		block.owner = owner
		block.outputIndex = owner.outputIndex
		block.contentIndex = len(owner.textParts)
		owner.textParts = append(owner.textParts, block)
	case "thinking", "redacted_thinking":
		r.enqueueMessageDone(false)
		block.id = "rs_" + uuid.NewString()
		block.outputIndex = len(r.output)
		r.output = append(r.output, block)
	case "tool_use":
		r.enqueueMessageDone(false)
		block.outputIndex = len(r.output)
		r.output = append(r.output, block)
	}
	return block
}

func messagesResponseOutputItem(block *messagesResponseBlock, status string) map[string]any {
	if block == nil {
		return nil
	}
	switch block.blockType {
	case "message":
		content := make([]any, 0, len(block.textParts))
		for _, part := range block.textParts {
			content = append(content, map[string]any{
				"type": "output_text", "text": part.text.String(), "annotations": []any{},
			})
		}
		return map[string]any{
			"id": block.id, "type": "message", "role": "assistant",
			"status": status, "content": content,
		}
	case "thinking", "redacted_thinking":
		summary := []any{}
		if block.reasoning.Len() > 0 {
			summary = append(summary, map[string]any{"type": "summary_text", "text": block.reasoning.String()})
		}
		item := map[string]any{
			"id": block.id, "type": "reasoning", "summary": summary, "status": status,
		}
		if block.signature.Len() > 0 {
			item["encrypted_content"] = block.signature.String()
		}
		return item
	case "tool_use":
		return messagesResponseToolItem(block, status)
	default:
		return nil
	}
}

func (r *messagesToResponsesReader) enqueueBlockAdded(block *messagesResponseBlock) {
	if block == nil {
		return
	}
	if block.blockType == "text" {
		owner := block.owner
		if owner == nil {
			return
		}
		if !owner.itemAdded {
			r.queue.Write(responseEvent("response.output_item.added", map[string]any{
				"output_index": owner.outputIndex,
				"item": map[string]any{
					"id": owner.id, "type": "message", "role": "assistant",
					"status": "in_progress", "content": []any{},
				},
			}))
			owner.itemAdded = true
		}
		if !block.partAdded {
			r.queue.Write(responseEvent("response.content_part.added", map[string]any{
				"item_id": owner.id, "output_index": owner.outputIndex, "content_index": block.contentIndex,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}))
			block.partAdded = true
		}
		return
	}
	if block.itemAdded {
		return
	}
	if block.id == "" {
		if block.blockType == "tool_use" {
			block.id = "call_" + uuid.NewString()
		} else {
			block.id = "rs_" + uuid.NewString()
		}
	}
	item := messagesResponseOutputItem(block, "in_progress")
	if block.blockType == "tool_use" {
		item["arguments"] = ""
	}
	r.queue.Write(responseEvent("response.output_item.added", map[string]any{
		"output_index": block.outputIndex, "item": item,
	}))
	block.itemAdded = true
}

func (r *messagesToResponsesReader) enqueueTextDelta(block *messagesResponseBlock, delta string) {
	if block == nil || delta == "" {
		return
	}
	r.enqueueBlockAdded(block)
	block.text.WriteString(delta)
	owner := block.owner
	r.queue.Write(responseEvent("response.output_text.delta", map[string]any{
		"item_id": owner.id, "output_index": owner.outputIndex, "content_index": block.contentIndex,
		"delta": delta, "response_id": r.responseID,
	}))
}

func (r *messagesToResponsesReader) enqueueTextPartDone(block *messagesResponseBlock) {
	if block == nil || block.partDone {
		return
	}
	r.enqueueBlockAdded(block)
	owner := block.owner
	text := block.text.String()
	r.queue.Write(responseEvent("response.output_text.done", map[string]any{
		"item_id": owner.id, "output_index": owner.outputIndex, "content_index": block.contentIndex,
		"text": text,
	}))
	r.queue.Write(responseEvent("response.content_part.done", map[string]any{
		"item_id": owner.id, "output_index": owner.outputIndex, "content_index": block.contentIndex,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	}))
	block.partDone = true
}

func (r *messagesToResponsesReader) enqueueMessageCompletion(message *messagesResponseBlock, force bool) {
	if message == nil || message.itemDone {
		return
	}
	if !force {
		for _, part := range message.textParts {
			if !part.blockStopped {
				return
			}
		}
	}
	for _, part := range message.textParts {
		r.enqueueTextPartDone(part)
	}
	if !message.itemAdded {
		return
	}
	r.queue.Write(responseEvent("response.output_item.done", map[string]any{
		"output_index": message.outputIndex, "item": messagesResponseOutputItem(message, "completed"),
	}))
	message.itemDone = true
}

func (r *messagesToResponsesReader) enqueueMessageDone(force bool) {
	message := r.message
	r.message = nil
	r.enqueueMessageCompletion(message, force)
}

func messagesResponseToolItem(block *messagesResponseBlock, status string) map[string]any {
	arguments := block.arguments.String()
	if arguments == "" {
		arguments = "{}"
	}
	return map[string]any{
		"id": block.id, "type": "function_call", "call_id": block.id,
		"name": block.name, "arguments": arguments, "status": status,
	}
}

func (r *messagesToResponsesReader) enqueueToolAdded(block *messagesResponseBlock) {
	if block == nil {
		return
	}
	r.enqueueBlockAdded(block)
	arguments := block.arguments.String()
	if (block.argumentsFromStart && !block.argumentsDeltaSeen) || block.emittedArguments >= len(arguments) {
		return
	}
	delta := arguments[block.emittedArguments:]
	block.emittedArguments = len(arguments)
	r.queue.Write(responseEvent("response.function_call_arguments.delta", map[string]any{
		"output_index": block.outputIndex, "item_id": block.id, "call_id": block.id, "delta": delta,
	}))
}

func (r *messagesToResponsesReader) enqueueToolCompletion(block *messagesResponseBlock) map[string]any {
	if block == nil {
		return nil
	}
	if block.itemDone {
		return messagesResponseToolItem(block, "completed")
	}
	r.enqueueToolAdded(block)
	if block.argumentsFromStart && !block.argumentsDeltaSeen {
		// A start-only input has no later delta to carry it. Emit it immediately
		// before done so stream accumulators and the terminal object agree.
		block.argumentsFromStart = false
		r.enqueueToolAdded(block)
	}
	arguments := messagesResponseToolItem(block, "completed")["arguments"].(string)
	if !json.Valid([]byte(arguments)) {
		// A content_block_stop can still follow a max_tokens cut in the middle
		// of tool JSON. Never announce a partial call as completed.
		return messagesResponseToolItem(block, "incomplete")
	}
	item := messagesResponseToolItem(block, "completed")
	r.queue.Write(responseEvent("response.function_call_arguments.done", map[string]any{
		"output_index": block.outputIndex, "item_id": block.id, "call_id": block.id,
		"arguments": item["arguments"],
	}))
	r.queue.Write(responseEvent("response.output_item.done", map[string]any{"output_index": block.outputIndex, "item": item}))
	block.itemDone = true
	return item
}

func (r *messagesToResponsesReader) enqueueReasoningCompletion(block *messagesResponseBlock) map[string]any {
	if block == nil {
		return nil
	}
	if block.itemDone {
		return messagesResponseOutputItem(block, "completed")
	}
	r.enqueueBlockAdded(block)
	if block.reasoning.Len() > 0 {
		r.enqueueReasoningSummaryPart(block)
		text := block.reasoning.String()
		r.queue.Write(responseEvent("response.reasoning_summary_text.done", map[string]any{
			"item_id": block.id, "output_index": block.outputIndex, "summary_index": 0,
			"text": text,
		}))
		if !block.summaryPartDone {
			r.queue.Write(responseEvent("response.reasoning_summary_part.done", map[string]any{
				"item_id": block.id, "output_index": block.outputIndex, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": text},
			}))
			block.summaryPartDone = true
		}
	}
	if block.signature.Len() > 0 {
		r.queue.Write(responseEvent("response.reasoning.encrypted_content.done", map[string]any{
			"item_id": block.id, "output_index": block.outputIndex,
			"encrypted_content": block.signature.String(),
		}))
	}
	item := messagesResponseOutputItem(block, "completed")
	r.queue.Write(responseEvent("response.output_item.done", map[string]any{
		"output_index": block.outputIndex, "item": item,
	}))
	block.itemDone = true
	return item
}

func (r *messagesToResponsesReader) enqueueReasoningSummaryPart(block *messagesResponseBlock) {
	if block == nil || block.summaryPartAdded {
		return
	}
	r.enqueueBlockAdded(block)
	r.queue.Write(responseEvent("response.reasoning_summary_part.added", map[string]any{
		"item_id": block.id, "output_index": block.outputIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	}))
	block.summaryPartAdded = true
}

func (r *messagesToResponsesReader) enqueueReasoningDelta(block *messagesResponseBlock, delta string) {
	if block == nil || delta == "" {
		return
	}
	r.enqueueReasoningSummaryPart(block)
	block.reasoning.WriteString(delta)
	r.queue.Write(responseEvent("response.reasoning_summary_text.delta", map[string]any{
		"item_id": block.id, "output_index": block.outputIndex, "summary_index": 0,
		"delta": delta, "response_id": r.responseID,
	}))
}

func (r *messagesToResponsesReader) translate(data []byte) {
	event := gjson.ParseBytes(data)
	typeName := event.Get("type").String()
	switch typeName {
	case "message_start":
		r.created = true
		if id := event.Get("message.id").String(); id != "" {
			r.responseID = id
		}
		if model := event.Get("message.model").String(); model != "" {
			r.model = model
		}
		r.usage["input_tokens"] = event.Get("message.usage.input_tokens").Int()
		r.usage["cached_tokens"] = event.Get("message.usage.cache_read_input_tokens").Int()
		r.queue.Write(responseEvent("response.created", map[string]any{"response": r.response("in_progress")}))
	case "content_block_start":
		index := int(event.Get("index").Int())
		blockType := event.Get("content_block.type").String()
		block := r.ensureBlock(index, blockType)
		switch blockType {
		case "text":
			r.enqueueBlockAdded(block)
			r.enqueueTextDelta(block, event.Get("content_block.text").String())
		case "thinking", "redacted_thinking":
			r.enqueueBlockAdded(block)
			r.enqueueReasoningDelta(block, event.Get("content_block.thinking").String())
			signature := event.Get("content_block.signature").String()
			if signature == "" {
				signature = event.Get("content_block.data").String()
			}
			if signature != "" {
				block.signature.WriteString(signature)
				r.queue.Write(responseEvent("response.reasoning.encrypted_content.delta", map[string]any{
					"item_id": block.id, "output_index": block.outputIndex,
					"delta": signature, "response_id": r.responseID,
				}))
			}
		case "tool_use":
			if block.id == "" {
				block.id = event.Get("content_block.id").String()
			}
			if block.name == "" {
				block.name = event.Get("content_block.name").String()
			}
			input := event.Get("content_block.input")
			if input.Exists() && input.Raw != "null" && block.arguments.Len() == 0 {
				raw := strings.TrimSpace(input.Raw)
				if raw != "" {
					block.arguments.WriteString(raw)
					block.argumentsFromStart = true
				}
			}
			r.enqueueToolAdded(block)
		}
	case "content_block_delta":
		index := int(event.Get("index").Int())
		deltaType := event.Get("delta.type").String()
		blockType := ""
		switch deltaType {
		case "text_delta":
			blockType = "text"
		case "thinking_delta", "signature_delta":
			blockType = "thinking"
		case "input_json_delta":
			blockType = "tool_use"
		}
		block := r.ensureBlock(index, blockType)
		switch deltaType {
		case "text_delta":
			r.enqueueTextDelta(block, event.Get("delta.text").String())
		case "thinking_delta":
			delta := event.Get("delta.thinking").String()
			r.enqueueReasoningDelta(block, delta)
		case "signature_delta":
			signature := event.Get("delta.signature").String()
			if signature == "" {
				return
			}
			r.enqueueBlockAdded(block)
			block.signature.WriteString(signature)
			r.queue.Write(responseEvent("response.reasoning.encrypted_content.delta", map[string]any{
				"item_id": block.id, "output_index": block.outputIndex,
				"delta": signature, "response_id": r.responseID,
			}))
		case "input_json_delta":
			partialJSON := event.Get("delta.partial_json").String()
			if partialJSON == "" {
				return
			}
			if !block.argumentsDeltaSeen {
				block.argumentsDeltaSeen = true
			}
			if block.argumentsFromStart {
				// In streaming Messages the start input is provisional (usually {}).
				// Once input_json_delta appears, that delta sequence is authoritative.
				block.arguments.Reset()
				block.emittedArguments = 0
				block.argumentsFromStart = false
			}
			block.arguments.WriteString(partialJSON)
			r.enqueueToolAdded(block)
		}
	case "content_block_stop":
		index := int(event.Get("index").Int())
		block := r.blocks[index]
		if block == nil || block.blockStopped {
			return
		}
		block.blockStopped = true
		switch block.blockType {
		case "text":
			r.enqueueTextPartDone(block)
		case "thinking", "redacted_thinking":
			r.enqueueReasoningCompletion(block)
		case "tool_use":
			r.enqueueToolCompletion(block)
		}
	case "message_delta":
		r.usage["output_tokens"] = event.Get("usage.output_tokens").Int()
		if value := event.Get("usage.input_tokens"); value.Exists() {
			r.usage["input_tokens"] = value.Int()
		}
		stop := event.Get("delta.stop_reason").String()
		if stop == "" {
			return
		}
		r.stopReason = stop
		r.stopSeen = true
	case "message_stop":
		// Messages requires message_delta.stop_reason followed by message_stop.
		// Do not manufacture a successful Responses terminal from only one half.
		if !r.stopSeen {
			return
		}
		stop := r.stopReason
		status := "completed"
		if stop == "max_tokens" || stop == "model_context_window_exceeded" {
			status = "incomplete"
		}
		if status == "completed" {
			for _, block := range r.output {
				if block == nil || block.itemDone {
					continue
				}
				switch block.blockType {
				case "message":
					r.enqueueMessageCompletion(block, true)
				case "thinking", "redacted_thinking":
					r.enqueueReasoningCompletion(block)
				case "tool_use":
					r.enqueueToolCompletion(block)
				}
			}
		} else {
			for _, block := range r.output {
				if block != nil && block.blockType == "message" {
					r.enqueueMessageCompletion(block, false)
				}
			}
		}
		usage := map[string]any{"input_tokens": r.usage["input_tokens"], "output_tokens": r.usage["output_tokens"], "total_tokens": r.usage["input_tokens"] + r.usage["output_tokens"], "input_tokens_details": map[string]any{"cached_tokens": r.usage["cached_tokens"]}}
		output := make([]any, 0, len(r.output))
		for _, block := range r.output {
			if block == nil {
				continue
			}
			itemStatus := "incomplete"
			if block.itemDone {
				itemStatus = "completed"
			}
			if item := messagesResponseOutputItem(block, itemStatus); item != nil {
				output = append(output, item)
			}
		}
		response := r.response(status)
		response["output"] = output
		response["usage"] = usage
		if status == "incomplete" {
			response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}
		terminalType := "response.completed"
		if status == "incomplete" {
			terminalType = "response.incomplete"
		}
		r.queue.Write(responseEvent(terminalType, map[string]any{"response": response}))
		r.terminal = true
	case "error":
		response := r.response("failed")
		response["error"] = map[string]any{"code": event.Get("error.type").String(), "message": event.Get("error.message").String()}
		r.queue.Write(responseEvent("response.failed", map[string]any{"response": response}))
		r.terminal = true
	}
}

func (r *messagesToResponsesReader) Read(p []byte) (int, error) {
	for r.queue.Len() == 0 {
		data, err := readSSEDataLine(r.reader)
		if err != nil {
			if err == io.EOF && !r.terminal {
				response := r.response("failed")
				response["error"] = map[string]any{"code": ErrorCodeUpstreamStreamBreak, "message": "upstream stream interrupted before completion"}
				r.queue.Write(responseEvent("response.failed", map[string]any{"response": response}))
				r.terminal = true
				break
			}
			return 0, err
		}
		r.translate(data)
	}
	return r.queue.Read(p)
}

func (r *messagesToResponsesReader) Close() error { return r.source.Close() }

func adaptGrokProtocolResponse(resp *http.Response, protocol GrokProtocol, model string, upstreamStreaming bool) {
	if resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || protocol == GrokProtocolResponses {
		return
	}
	// A native stream=false response is already in the requested wire shape.
	// Only converted Chat/Messages routes force SSE and need projection back to
	// Responses for the existing downstream translators.
	if !upstreamStreaming {
		return
	}
	switch protocol {
	case GrokProtocolChatCompletions:
		resp.Body = newChatToResponsesReader(resp.Body, model)
	case GrokProtocolMessages:
		resp.Body = newMessagesToResponsesReader(resp.Body, model)
	}
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
}

// ExecuteGrokNativeProtocolProbe bypasses catalog/backend routing on purpose and
// exercises one exact protocol endpoint. It is only for controlled capability
// probing; production requests must use ExecuteGrokProtocolRequest.
// The returned successful body remains in its native wire shape so the caller
// can validate that protocol's actual terminal event/usage without conversion.
func ExecuteGrokNativeProtocolProbe(ctx context.Context, account *auth.Account, protocol GrokProtocol, model string, body []byte, proxyOverride string) (*http.Response, error) {
	return ExecuteGrokNativeProtocolProbeAtOriginWithHeaders(ctx, account, protocol, model, body, "", proxyOverride, nil)
}

// ExecuteGrokNativeProtocolProbeAtOrigin probes the catalog item's concrete
// origin. Empty origin is a safe compatibility fallback to the account base.
// Callers must pass origins obtained from the persisted, sanitized catalog.
func ExecuteGrokNativeProtocolProbeAtOrigin(ctx context.Context, account *auth.Account, protocol GrokProtocol, model string, body []byte, origin, proxyOverride string) (*http.Response, error) {
	return ExecuteGrokNativeProtocolProbeAtOriginWithHeaders(ctx, account, protocol, model, body, origin, proxyOverride, nil)
}

// ExecuteGrokNativeProtocolProbeAtOriginWithHeaders is the catalog-aware probe
// variant. extraHeaders must come from the persisted, sanitized model catalog;
// it is filtered again here and credential/security headers are re-applied last
// so a stale or manually-edited row cannot override authentication identity.
func ExecuteGrokNativeProtocolProbeAtOriginWithHeaders(ctx context.Context, account *auth.Account, protocol GrokProtocol, model string, body []byte, origin, proxyOverride string, extraHeaders map[string]string) (*http.Response, error) {
	protocol = auth.NormalizeGrokProtocol(string(protocol))
	if protocol == "" {
		return nil, fmt.Errorf("unsupported Grok protocol")
	}
	if account == nil {
		return nil, ErrNoAvailableAccount()
	}
	baseURL, bearer := account.GrokCredentials()
	if candidate := strings.TrimRight(strings.TrimSpace(origin), "/"); candidate != "" {
		baseURL = candidate
	}
	if baseURL == "" || bearer == "" {
		return nil, ErrNoAvailableAccount()
	}
	if len(body) == 0 {
		body = MinimalGrokProbeBody(protocol, model)
	}
	endpoint := auth.OpenAIResponsesEndpoint(baseURL, grokProtocolSuffix(protocol))
	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInternalError("创建 Grok 探针请求失败", err)
	}
	applyGrokRequestHeaders(req, account, bearer, nil, nil)
	for name, values := range routeHeaders(extraHeaders) {
		for _, value := range values {
			req.Header.Set(name, value)
		}
	}
	applyGrokRequestHeaders(req, account, bearer, nil, nil)
	req.Header.Set("Accept-Encoding", "gzip, br, deflate")
	req.Header.Set("x-grok-turn-idx", "1")
	if model != "" {
		req.Header.Set("x-grok-model-override", model)
	}
	if protocol == GrokProtocolMessages {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		return nil, ErrUpstream(0, "请求 Grok 原生协议探针失败", err)
	}
	decodeGrokResponseEncoding(resp)
	recordGrokUpstreamObservationsAtOrigin(account, resp.Header, baseURL)
	return resp, nil
}

func canonicalGrokResponsesBody(inbound GrokProtocol, inboundBody, responsesBody []byte) ([]byte, error) {
	if len(responsesBody) == 0 {
		responsesBody = inboundBody
	}
	var (
		body []byte
		err  error
	)
	switch auth.NormalizeGrokProtocol(string(inbound)) {
	case GrokProtocolChatCompletions:
		if len(inboundBody) > 0 {
			body, err = TranslateChatToResponsesForGrok(inboundBody)
		}
	case GrokProtocolMessages:
		if len(inboundBody) > 0 {
			body, _, err = TranslateAnthropicToResponsesForGrok(inboundBody, "", nil)
		}
	default:
		body = responsesBody
	}
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		body = responsesBody
	}
	// The handler-level translation has already applied global/account model
	// mapping for this concrete retry. Reuse only that resolved model while
	// rebuilding the rest of the canonical request from the original wire body.
	if model := strings.TrimSpace(gjson.GetBytes(responsesBody, "model").String()); model != "" {
		body, err = rewriteGrokProtocolModel(body, model)
	}
	return body, err
}

func convertCanonicalGrokResponsesBody(protocol GrokProtocol, canonical []byte) ([]byte, error) {
	switch protocol {
	case GrokProtocolChatCompletions:
		return convertResponsesToChatRequest(canonical)
	case GrokProtocolMessages:
		return convertResponsesToMessagesRequest(canonical)
	default:
		return canonical, nil
	}
}

func grokProtocolBodyStream(body []byte) bool {
	stream := gjson.GetBytes(body, "stream")
	return stream.Exists() && stream.Bool()
}

func rewriteGrokProtocolModel(body []byte, model string) ([]byte, error) {
	model = strings.TrimSpace(model)
	if model == "" || strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "model").String()), model) {
		return body, nil
	}
	return sjson.SetBytes(body, "model", model)
}

func prepareRoutedGrokProtocolRequest(route GrokUpstreamRoute, inbound GrokProtocol, inboundBody, responsesBody []byte) (grokPreflightResult, error) {
	inbound = auth.NormalizeGrokProtocol(string(inbound))
	// A same-protocol route receives the original downstream object even when
	// it was selected from catalog apiBackend rather than a fresh capability
	// probe. This preserves unknown standard fields and provider extensions.
	// Only the already-resolved account mapping may change model; auth/session
	// headers and the protocol-specific Grok preflight remain owned by
	// ExecuteGrokProtocolRequest.
	if route.Protocol == inbound && inbound != "" && len(inboundBody) > 0 {
		body, err := rewriteGrokProtocolModel(inboundBody, route.Model)
		if err != nil {
			return grokPreflightResult{}, err
		}
		if route.Native {
			// 仅 native 路由按线格式直通返回(forwardGrokNativeResponse),
			// 可以完整保留 stream=false。
			//
			// 但直通的是"传输形态",不是"请求内容":Codex 专有工具形态
			// (custom / namespace / tool_search / additional_tools)与被丢弃的
			// 顶层字段仍必须经 preflight 降级,否则原样发给 Grok 会 400。
			// 统一跑 preflight 还让同一会话在 native 与非 native 之间切换时
			// 请求体形态保持一致,不打断上游的静态前缀缓存。
			// 实测 preflight 成本 0.06~1.9ms(6KB~544KB 请求体),相对 Grok
			// 秒级首字可忽略。
			if route.Protocol == GrokProtocolResponses {
				return prepareGrokUpstreamBody(body), nil
			}
			return grokPreflightResult{Body: body, TurnIndex: 1, Model: gjson.GetBytes(body, "model").String()}, nil
		}
		// 非 native 的同协议路由不会直通:响应必须经 adaptGrokProtocolResponse
		// 投影成规范 Responses SSE 再交给下游翻译器,而该投影只处理 SSE。
		// 客户端 stream=false 时必须强制上游流式,否则非流式 JSON 进入 SSE
		// 消费管线,请求确定性失败且账号被误判惩罚。非流式聚合由 handler 完成。
		if !grokProtocolBodyStream(body) {
			forced, forceErr := sjson.SetBytes(body, "stream", true)
			if forceErr != nil {
				return grokPreflightResult{}, forceErr
			}
			if route.Protocol == GrokProtocolChatCompletions {
				// Chat 的 usage 只随 include_usage 的独立 chunk 下发。
				if withUsage, usageErr := sjson.SetBytes(forced, "stream_options.include_usage", true); usageErr == nil {
					forced = withUsage
				}
			}
			body = forced
		}
		if route.Protocol == GrokProtocolResponses {
			return prepareGrokUpstreamBody(body), nil
		}
		return grokPreflightResult{Body: clampGrokReasoningEffort(body), TurnIndex: 1, Model: gjson.GetBytes(body, "model").String()}, nil
	}

	canonical, err := canonicalGrokResponsesBody(inbound, inboundBody, responsesBody)
	if err != nil {
		return grokPreflightResult{}, err
	}
	// Codex-only Responses tools and history must be lowered while the request
	// still has canonical Responses semantics. Converting first rejects those
	// variants and loses the request-local aliases needed to restore tool calls.
	preflight := prepareGrokUpstreamBody(canonical)
	converted, err := convertCanonicalGrokResponsesBody(route.Protocol, preflight.Body)
	if err != nil {
		return grokPreflightResult{}, err
	}
	if route.Protocol != GrokProtocolResponses {
		converted = clampGrokReasoningEffort(converted)
	}
	preflight.Body = converted
	return preflight, nil
}

func prepareRoutedGrokProtocolBody(route GrokUpstreamRoute, inbound GrokProtocol, inboundBody, responsesBody []byte) ([]byte, error) {
	preflight, err := prepareRoutedGrokProtocolRequest(route, inbound, inboundBody, responsesBody)
	return preflight.Body, err
}

func validGrokClientVersion(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func fetchGrokMinimumClientVersion(ctx context.Context, account *auth.Account, proxyURL string) string {
	result, err := FetchGrokControlPlaneFact(ctx, account, proxyURL, GrokControlPlaneSettings, "")
	if err != nil || result.StatusCode < 200 || result.StatusCode >= 300 {
		return ""
	}
	observeGrokRuntimeSettingsFact(account, result)
	for _, path := range []string{"min_client_version", "minClientVersion"} {
		if version := strings.TrimSpace(gjson.GetBytes(result.Body, path).String()); validGrokClientVersion(version) {
			return version
		}
	}
	return ""
}

// ExecuteGrokProtocolRequest is the account-safe facade used by handlers and
// admin probes. inboundBody may be nil when the caller only has canonical
// Responses. The returned body always speaks Responses SSE on successful
// Chat/Messages routes, preserving the existing downstream projection boundary.
func ExecuteGrokProtocolRequest(ctx context.Context, account *auth.Account, inbound GrokProtocol, inboundBody, responsesBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil {
		return nil, ErrNoAvailableAccount()
	}
	model := strings.TrimSpace(gjson.GetBytes(responsesBody, "model").String())
	if model == "" {
		model = strings.TrimSpace(gjson.GetBytes(inboundBody, "model").String())
	}
	route := ResolveGrokUpstreamRoute(account, model, inbound, time.Now())
	preflight, err := prepareRoutedGrokProtocolRequest(route, inbound, inboundBody, responsesBody)
	if err != nil {
		return nil, ErrBadRequest("Grok protocol conversion failed: " + err.Error())
	}
	baseURL, bearer := account.GrokCredentials()
	_ = baseURL
	if route.Endpoint == "" || bearer == "" {
		return nil, ErrNoAvailableAccount()
	}
	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	logGrokPrefixFingerprint(preflight.Body, preflight.TurnIndex, preflight.Model)
	send := func(payload []byte, clientVersion string) (*http.Response, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, route.Endpoint, bytes.NewReader(payload))
		if reqErr != nil {
			return nil, ErrInternalError("创建请求失败", reqErr)
		}
		conversationBody := inboundBody
		if len(conversationBody) == 0 {
			conversationBody = responsesBody
		}
		applyGrokRequestHeaders(req, account, bearer, headers, conversationBody)
		if clientVersion != "" {
			req.Header.Set("x-grok-client-version", clientVersion)
		}
		for name, values := range route.ExtraHeaders {
			for _, value := range values {
				req.Header.Set(name, value)
			}
		}
		// Credential/security headers always win over model metadata.
		applyGrokRequestHeaders(req, account, bearer, headers, conversationBody)
		if clientVersion != "" {
			req.Header.Set("x-grok-client-version", clientVersion)
		}
		req.Header.Set("x-grok-turn-idx", strconv.Itoa(preflight.TurnIndex))
		req.Header.Set("Accept-Encoding", "gzip, br, deflate")
		if preflight.Model != "" {
			req.Header.Set("x-grok-model-override", preflight.Model)
		}
		if route.Protocol == GrokProtocolMessages && req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, doErr := getPooledClient(account, proxyURL).Do(req)
		if doErr != nil {
			if shouldRecyclePooledClient(doErr) {
				recyclePooledClient(account, proxyURL)
			}
			return nil, ErrUpstream(0, "请求 Grok 上游失败", doErr)
		}
		decodeGrokResponseEncoding(resp)
		return resp, nil
	}
	resp, err := send(preflight.Body, "")
	if err != nil {
		return nil, err
	}
	// 426 is a control-plane compatibility signal, not an account failure. The
	// request has produced no visible output yet, so refresh settings and retry
	// this same account exactly once with a validated minimum client version.
	if resp.StatusCode == http.StatusUpgradeRequired {
		if minVersion := fetchGrokMinimumClientVersion(ctx, account, proxyURL); minVersion != "" {
			_ = resp.Body.Close()
			resp, err = send(preflight.Body, minVersion)
			if err != nil {
				return nil, err
			}
		}
	}
	if route.Protocol == GrokProtocolResponses && resp.StatusCode == http.StatusBadRequest && grokBodyHasBlobs(preflight.Body) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if grokIsBlobDecodeFailure(errBody) {
			resp, err = send(stripGrokUndecodableBlobs(preflight.Body), "")
			if err != nil {
				return nil, err
			}
		} else {
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
		}
	}
	recordGrokUpstreamObservationsAtOrigin(account, resp.Header, route.BaseURL)
	observeGrokNativeProtocolFailure(account, route, inbound, resp)
	markGrokNativeRoute(resp, route, inbound)
	// Same-protocol native routes retain the provider wire representation. The
	// concrete handler streams/copies it directly; converted routes continue to
	// project through canonical Responses SSE for the existing translators.
	if !isGrokNativeRouteResponse(resp) {
		adaptGrokProtocolResponse(resp, route.Protocol, model, grokProtocolBodyStream(preflight.Body))
	}
	if len(preflight.Aliases) > 0 && resp.Body != nil {
		streaming := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream")
		resp.Body = newGrokNamespaceReverser(resp.Body, streaming, preflight.Aliases)
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
	}
	return resp, nil
}

// ExecuteRelayStyleProtocolRequest is the protocol-aware dispatcher used by
// the three HTTP handlers. Relay accounts still receive canonical Responses;
// Grok accounts select a backend after the concrete retry account is known.
func ExecuteRelayStyleProtocolRequest(ctx context.Context, account *auth.Account, inbound GrokProtocol, inboundBody, responsesBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if account != nil && account.IsGrokAPI() {
		return ExecuteGrokProtocolRequest(ctx, account, inbound, inboundBody, responsesBody, proxyOverride, headers)
	}
	if len(responsesBody) == 0 {
		responsesBody = inboundBody
	}
	return ExecuteOpenAIResponsesRequest(ctx, account, responsesBody, proxyOverride, headers)
}

func relayUpstreamEndpointForProtocol(account *auth.Account, inbound GrokProtocol, model string) string {
	if account != nil && account.IsGrokAPI() {
		return ResolveGrokUpstreamRoute(account, model, inbound, time.Now()).Endpoint
	}
	return relayUpstreamEndpointForAccount(account)
}

// MinimalGrokProbeBody returns the low-cost native wire body expected by
// ExecuteGrokProtocolRequest callers that explicitly probe a protocol.
func MinimalGrokProbeBody(protocol GrokProtocol, model string) []byte {
	switch auth.NormalizeGrokProtocol(string(protocol)) {
	case GrokProtocolChatCompletions:
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_completion_tokens": 1, "stream": true, "stream_options": map[string]any{"include_usage": true}})
		return body
	case GrokProtocolMessages:
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 1, "stream": true})
		return body
	default:
		body, _ := json.Marshal(map[string]any{"model": model, "input": "hi", "max_output_tokens": 1, "reasoning": map[string]any{"effort": "low"}, "store": false, "stream": true})
		return body
	}
}
