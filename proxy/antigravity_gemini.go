package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

// ExecuteAntigravityGeminiRequest forwards a native Gemini generateContent request
// through the Cloud Code v1internal Antigravity adapter and unwraps the upstream
// envelope back into Gemini API shape.
func ExecuteAntigravityGeminiRequest(ctx context.Context, account *auth.Account, model string, body []byte, stream bool, proxyURL string) (*http.Response, error) {
	resetUpstreamAttemptTrace(ctx)
	if account == nil {
		return nil, fmt.Errorf("antigravity account is nil")
	}
	if account.AntigravityAuthKind() != auth.AntigravityAuthKindOAuth {
		return nil, fmt.Errorf("native Gemini format requires an Antigravity OAuth account")
	}
	project, bearer := account.AntigravityCredentials()
	if project == "" || bearer == "" {
		return nil, fmt.Errorf("antigravity account %d has no project_id or access token", account.ID())
	}
	payload, wireModel, reverseNameMap, err := geminiNativeToAntigravityEnvelope(body, project, model)
	if err != nil {
		return nil, err
	}
	_ = bearer
	jsonMode := antigravityClaudeJSONModeRequest(payload)
	return executeAntigravityOAuthRequest(ctx, account, payload, antigravityOAuthGenerateCall(stream), proxyURL, wireModel, func(resp *http.Response, stream bool, _ string) (*http.Response, error) {
		if stream {
			sse := newAntigravityNativeGeminiSSEResponseBody(resp.Body, reverseNameMap)
			sse.jsonMode = jsonMode
			resp.Body = sse
			resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
			return resp, nil
		}
		if jsonMode {
			raw, readErr := readBoundedAntigravityBody(resp.Body, antigravityResponseBodyLimit)
			if readErr != nil {
				return nil, fmt.Errorf("read Antigravity Gemini JSON response: %w", readErr)
			}
			raw, _ = antigravityRewriteJSONModeFrame(raw, false)
			resp.Body = io.NopCloser(bytes.NewReader(raw))
		}
		converted, convertErr := newAntigravityNativeGeminiJSONResponseBody(resp.Body, reverseNameMap)
		if convertErr != nil {
			return nil, convertErr
		}
		resp.Body = converted
		return resp, nil
	})
}

// ExecuteAntigravityGeminiCountTokensRequest forwards a native Gemini countTokens
// request through Cloud Code v1internal:countTokens.
func ExecuteAntigravityGeminiCountTokensRequest(ctx context.Context, account *auth.Account, model string, body []byte, proxyURL string) (*http.Response, error) {
	resetUpstreamAttemptTrace(ctx)
	if account == nil {
		return nil, fmt.Errorf("antigravity account is nil")
	}
	if account.AntigravityAuthKind() != auth.AntigravityAuthKindOAuth {
		return nil, fmt.Errorf("native Gemini format requires an Antigravity OAuth account")
	}
	project, bearer := account.AntigravityCredentials()
	if project == "" || bearer == "" {
		return nil, fmt.Errorf("antigravity account %d has no project_id or access token", account.ID())
	}
	payload, wireModel, err := geminiNativeToAntigravityCountTokensEnvelope(body, project, model)
	if err != nil {
		return nil, err
	}
	_ = bearer
	return executeAntigravityOAuthRequest(ctx, account, payload, antigravityOAuthCountTokensCall(), proxyURL, wireModel, func(resp *http.Response, _ bool, _ string) (*http.Response, error) {
		converted, convertErr := newAntigravityNativeGeminiCountTokensResponseBody(resp.Body)
		if convertErr != nil {
			return nil, convertErr
		}
		resp.Body = converted
		return resp, nil
	})
}

func geminiNativeToAntigravityCountTokensEnvelope(rawBody []byte, project, model string) ([]byte, string, error) {
	payload, wireModel, _, err := geminiNativeToAntigravityEnvelope(rawBody, project, model)
	if err != nil {
		return nil, "", err
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, "", fmt.Errorf("decode countTokens envelope: %w", err)
	}
	request, ok := envelope["request"].(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("countTokens request missing")
	}
	delete(request, "safetySettings")
	delete(request, "toolConfig")
	delete(request, "labels")
	delete(request, "sessionId")
	antigravityEnsureGeminiLeadingUserContent(request, wireModel)
	out, err := json.Marshal(map[string]any{"request": request})
	if err != nil {
		return nil, "", err
	}
	return out, wireModel, nil
}

func antigravityEnsureGeminiLeadingUserContent(request map[string]any, wireModel string) {
	if strings.Contains(strings.ToLower(strings.TrimSpace(wireModel)), "claude") {
		return
	}
	contents, ok := request["contents"].([]any)
	if !ok || len(contents) == 0 {
		return
	}
	first, ok := contents[0].(map[string]any)
	if !ok {
		return
	}
	role, _ := first["role"].(string)
	if role != "model" {
		return
	}
	leading := map[string]any{
		"role":  "user",
		"parts": []any{map[string]any{"text": ""}},
	}
	request["contents"] = append([]any{leading}, contents...)
}

func geminiNativeToAntigravityEnvelope(rawBody []byte, project, model string) ([]byte, string, map[string]string, error) {
	model = normalizeGeminiPublicModel(model)
	nameMap := antigravityGeminiFunctionNameMap(rawBody)
	reverseNameMap := antigravityGeminiReverseNameMap(nameMap)
	var request map[string]any
	if err := json.Unmarshal(rawBody, &request); err != nil {
		return nil, "", nil, fmt.Errorf("decode Gemini request: %w", err)
	}
	delete(request, "model")
	if si, ok := request["system_instruction"]; ok {
		request["systemInstruction"] = si
		delete(request, "system_instruction")
	}
	antigravityObfuscateNativeGeminiSystemInstruction(request)
	if err := antigravityFixGeminiToolResponseGrouping(request); err != nil {
		return nil, "", nil, err
	}
	antigravityNormalizeGeminiContentRoles(request)
	antigravitySanitizeNativeGeminiTools(request, nameMap)
	antigravityRewriteNativeGeminiFunctionNames(request, nameMap)
	wireModel := antigravityGeminiResolvedModel(model, nil)
	antigravityApplyNativeGeminiThinkingConfig(request, model, wireModel)
	antigravityNormalizeNativeGeminiResponseSchema(request)
	antigravitySanitizeNativeGeminiThoughtSignatures(request, wireModel)
	// Strip client scaffolding before the session id is derived from the
	// contents, so the id stays stable across turns.
	antigravityNormalizeNativeGeminiContents(request)
	contents, _ := request["contents"].([]any)
	request["sessionId"] = antigravitySessionID(nil, contents)
	envelope := map[string]any{
		"project":     project,
		"requestId":   antigravityRequestID(),
		"request":     request,
		"model":       wireModel,
		"userAgent":   antigravityOfficialBodyUserAgent,
		"requestType": "agent",
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, "", nil, err
	}
	return payload, wireModel, reverseNameMap, nil
}

func antigravityApplyNativeGeminiThinkingConfig(request map[string]any, publicModel, wireModel string) {
	genConfig, ok := request["generationConfig"].(map[string]any)
	if !ok {
		genConfig = map[string]any{}
		request["generationConfig"] = genConfig
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(wireModel)), "gemini-") {
		delete(genConfig, "maxOutputTokens")
	}
	if thinking, ok := genConfig["thinkingConfig"].(map[string]any); ok && len(thinking) > 0 {
		return
	}
	if level, enabled := antigravityGeminiThinkingLevel(publicModel, wireModel, nil); enabled {
		thinkingConfig := map[string]any{"thinkingLevel": level}
		if auth.AntigravityExposeThoughts() {
			thinkingConfig["includeThoughts"] = true
		}
		genConfig["thinkingConfig"] = thinkingConfig
		return
	}
	if budget, enabled := antigravityGeminiThinkingBudget(publicModel, wireModel, nil); enabled {
		genConfig["thinkingConfig"] = map[string]any{
			"includeThoughts": true,
			"thinkingBudget":  budget,
		}
	}
}

// antigravityNormalizeNativeGeminiResponseSchema 把 generationConfig.responseJsonSchema
// (及 snake_case 的 response_json_schema)归一成 responseSchema。Antigravity 后端不认
// 前者,原样透传时结构化输出会静默不生效。responseJsonSchema 是标准 JSON Schema,
// 可能带 $defs/$ref 与 responseSchema(OpenAPI 子集)不认的关键字,搬过去时套用与
// Responses→Antigravity 相同的清洗;客户端已显式给了 responseSchema 则以它为准,
// 只删掉过时键。
func antigravityNormalizeNativeGeminiResponseSchema(request map[string]any) {
	for _, container := range []string{"generationConfig", "generation_config"} {
		genConfig, ok := request[container].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"responseJsonSchema", "response_json_schema"} {
			schema, exists := genConfig[key]
			if !exists {
				continue
			}
			delete(genConfig, key)
			if _, has := genConfig["responseSchema"]; has || schema == nil {
				continue
			}
			genConfig["responseSchema"] = antigravityResponseSchemaFromJSONSchema(schema)
		}
	}
}

func antigravityResponseSchemaFromJSONSchema(schema any) any {
	root, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	definitions := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if defs, defsOK := root[key].(map[string]any); defsOK {
			for name, definition := range defs {
				definitions[name] = definition
			}
		}
	}
	cleaned, cleanedOK := antigravityCleanGeminiSchema(root, definitions, map[string]bool{}, 0).(map[string]any)
	if !cleanedOK || len(cleaned) == 0 {
		return schema
	}
	return cleaned
}

const antigravityGeminiSkipThoughtSignature = "skip_thought_signature_validator"

func antigravitySanitizeNativeGeminiThoughtSignatures(request map[string]any, wireModel string) {
	if !antigravityGeminiNeedsToolSignature(wireModel) {
		return
	}
	contents, ok := request["contents"].([]any)
	if !ok {
		return
	}
	for index, rawContent := range contents {
		content, ok := rawContent.(map[string]any)
		if !ok {
			continue
		}
		role, _ := content["role"].(string)
		parts, ok := content["parts"].([]any)
		if !ok {
			continue
		}
		cleanedParts := make([]any, 0, len(parts))
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				cleanedParts = append(cleanedParts, rawPart)
				continue
			}
			if _, hasResponse := part["functionResponse"]; hasResponse {
				antigravityDeleteGeminiThoughtSignatureFields(part)
				cleanedParts = append(cleanedParts, part)
				continue
			}
			if legacy, ok := part["function_response"].(map[string]any); ok {
				antigravityDeleteGeminiThoughtSignatureFields(part)
				delete(legacy, "thoughtSignature")
				delete(legacy, "thought_signature")
				cleanedParts = append(cleanedParts, part)
				continue
			}
			if _, hasCall := part["functionCall"]; hasCall {
				if !antigravityGeminiPartHasThoughtSignature(part) {
					part["thoughtSignature"] = antigravityGeminiSkipThoughtSignature
					part["thought_signature"] = antigravityGeminiSkipThoughtSignature
				}
				cleanedParts = append(cleanedParts, part)
				continue
			}
			if role != "model" {
				antigravityDeleteGeminiThoughtSignatureFields(part)
			}
			cleanedParts = append(cleanedParts, part)
		}
		content["parts"] = cleanedParts
		contents[index] = content
	}
	request["contents"] = contents
}

func antigravityDeleteGeminiThoughtSignatureFields(part map[string]any) {
	delete(part, "thoughtSignature")
	delete(part, "thought_signature")
	for _, key := range []string{"functionCall", "function_call", "functionResponse", "function_response"} {
		if nested, ok := part[key].(map[string]any); ok {
			delete(nested, "thoughtSignature")
			delete(nested, "thought_signature")
		}
	}
	if extra, ok := part["extra_content"].(map[string]any); ok {
		if google, ok := extra["google"].(map[string]any); ok {
			delete(google, "thought_signature")
		}
	}
}

func antigravityGeminiPartHasThoughtSignature(part map[string]any) bool {
	for _, path := range [][]string{
		{"thoughtSignature"},
		{"thought_signature"},
		{"functionCall", "thoughtSignature"},
		{"functionCall", "thought_signature"},
		{"function_call", "thoughtSignature"},
		{"function_call", "thought_signature"},
	} {
		current := any(part)
		for _, key := range path {
			m, ok := current.(map[string]any)
			if !ok {
				break
			}
			value, exists := m[key]
			if !exists {
				break
			}
			if key == path[len(path)-1] {
				if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
					return true
				}
				return false
			}
			current = value
		}
	}
	return false
}

func normalizeGeminiPublicModel(model string) string {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "models/")
	return model
}

func antigravityObfuscateNativeGeminiSystemInstruction(request map[string]any) {
	systemInstruction, ok := request["systemInstruction"].(map[string]any)
	if !ok {
		return
	}
	parts, ok := systemInstruction["parts"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(parts))
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			kept = append(kept, part)
			continue
		}
		text, ok := partMap["text"].(string)
		if !ok {
			kept = append(kept, part)
			continue
		}
		text = normalizeClientScaffolding(text)
		if text == "" {
			continue
		}
		partMap["text"] = antigravityObfuscateSystemInstruction(text)
		kept = append(kept, partMap)
	}
	if len(kept) == 0 {
		delete(request, "systemInstruction")
		return
	}
	systemInstruction["parts"] = kept
}

// antigravityNormalizeNativeGeminiContents neutralizes client scaffolding in
// native Gemini contents in place. Parts are rewritten rather than dropped so
// the envelope keeps the exact shape the caller sent; an emptied text part
// becomes an empty string, which is a shape this codebase already produces
// elsewhere.
func antigravityNormalizeNativeGeminiContents(request map[string]any) {
	if !clientScaffoldingStripEnabled() {
		return
	}
	contents, ok := request["contents"].([]any)
	if !ok || len(contents) == 0 {
		return
	}
	for index, raw := range contents {
		content, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := content["parts"].([]any)
		if !ok {
			continue
		}
		for partIndex, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			part["text"] = normalizeClientScaffolding(text)
			parts[partIndex] = part
		}
		content["parts"] = parts
		contents[index] = content
	}
	request["contents"] = contents
}

func antigravitySanitizeNativeGeminiTools(request map[string]any, nameMap map[string]string) {
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}
	cleanedTools := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"functionDeclarations", "function_declarations"} {
			declarations, ok := tool[key].([]any)
			if !ok || len(declarations) == 0 {
				continue
			}
			cleaned := make([]any, 0, len(declarations))
			seen := map[string]struct{}{}
			for _, rawDeclaration := range declarations {
				declaration, ok := rawDeclaration.(map[string]any)
				if !ok {
					continue
				}
				name, _ := declaration["name"].(string)
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				mappedName := antigravityMapGeminiFunctionName(nameMap, name)
				if _, exists := seen[mappedName]; exists {
					continue
				}
				seen[mappedName] = struct{}{}
				declaration["name"] = mappedName
				antigravityApplyGeminiDeclarationSchema(declaration)
				cleaned = append(cleaned, declaration)
			}
			if len(cleaned) > 0 {
				tool[key] = cleaned
			} else {
				delete(tool, key)
			}
		}
		if len(tool) == 0 {
			continue
		}
		cleanedTools = append(cleanedTools, tool)
	}
	if len(cleanedTools) == 0 {
		delete(request, "tools")
		return
	}
	request["tools"] = cleanedTools
}

func antigravityNormalizeGeminiContentRoles(request map[string]any) {
	contents, ok := request["contents"].([]any)
	if !ok || len(contents) == 0 {
		return
	}
	previousRole := ""
	for index, rawContent := range contents {
		content, ok := rawContent.(map[string]any)
		if !ok {
			continue
		}
		role, _ := content["role"].(string)
		if role != "user" && role != "model" {
			if antigravityContentHasFunctionResponse(content) {
				role = "user"
			} else if previousRole == "" || previousRole == "model" {
				role = "user"
			} else {
				role = "model"
			}
			content["role"] = role
			contents[index] = content
		}
		previousRole = role
	}
	request["contents"] = contents
}

func antigravityContentHasFunctionResponse(content map[string]any) bool {
	parts, _ := content["parts"].([]any)
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := partMap["functionResponse"]; ok {
			return true
		}
		if _, ok := partMap["function_response"]; ok {
			return true
		}
	}
	return false
}

type antigravityGeminiToolCallGroup struct {
	responsesNeeded int
	callNames       []string
}

func antigravityFixGeminiToolResponseGrouping(request map[string]any) error {
	contents, ok := request["contents"].([]any)
	if !ok || len(contents) == 0 {
		return nil
	}
	needsGrouping := false
	for _, rawContent := range contents {
		content, ok := rawContent.(map[string]any)
		if !ok {
			continue
		}
		parts, _ := content["parts"].([]any)
		for _, part := range parts {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := partMap["functionResponse"]; ok {
				needsGrouping = true
				break
			}
			if _, ok := partMap["function_response"]; ok {
				needsGrouping = true
				break
			}
		}
		if needsGrouping {
			break
		}
	}
	if !needsGrouping {
		return nil
	}

	rebuilt := make([]any, 0, len(contents))
	pendingGroups := make([]*antigravityGeminiToolCallGroup, 0)
	collectedResponses := make([]map[string]any, 0)

	appendFunctionResponses := func(responses []map[string]any, callNames []string) {
		if len(responses) == 0 {
			return
		}
		parts := make([]any, 0, len(responses))
		for index, response := range responses {
			fallbackName := ""
			if index < len(callNames) {
				fallbackName = callNames[index]
			}
			parts = append(parts, antigravityNormalizeFunctionResponsePart(response, fallbackName))
		}
		rebuilt = append(rebuilt, map[string]any{"role": "function", "parts": parts})
	}

	for _, rawContent := range contents {
		content, ok := rawContent.(map[string]any)
		if !ok {
			continue
		}
		role, _ := content["role"].(string)
		responseParts := antigravityCollectFunctionResponsesWithInlineData(content)
		if len(responseParts) > 0 {
			collectedResponses = append(collectedResponses, responseParts...)
			for len(pendingGroups) > 0 && len(collectedResponses) >= pendingGroups[0].responsesNeeded {
				group := pendingGroups[0]
				pendingGroups = pendingGroups[1:]
				groupResponses := collectedResponses[:group.responsesNeeded]
				collectedResponses = collectedResponses[group.responsesNeeded:]
				appendFunctionResponses(groupResponses, group.callNames)
			}
			continue
		}
		if role == "model" {
			callNames := antigravityFunctionCallNames(content)
			if len(callNames) > 0 {
				rebuilt = append(rebuilt, content)
				pendingGroups = append(pendingGroups, &antigravityGeminiToolCallGroup{
					responsesNeeded: len(callNames),
					callNames:       callNames,
				})
				continue
			}
		}
		rebuilt = append(rebuilt, content)
	}
	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.responsesNeeded {
			groupResponses := collectedResponses[:group.responsesNeeded]
			collectedResponses = collectedResponses[group.responsesNeeded:]
			appendFunctionResponses(groupResponses, group.callNames)
		}
	}
	request["contents"] = rebuilt
	return nil
}

func antigravityFunctionCallNames(content map[string]any) []string {
	names := make([]string, 0)
	parts, _ := content["parts"].([]any)
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if call, ok := partMap["functionCall"].(map[string]any); ok {
			if name, _ := call["name"].(string); strings.TrimSpace(name) != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

func antigravityCollectFunctionResponsesWithInlineData(content map[string]any) []map[string]any {
	parts, _ := content["parts"].([]any)
	responses := make([]map[string]any, 0)
	leadingImages := make([]map[string]any, 0)
	current := -1
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := part["functionResponse"]; ok {
			responses = append(responses, part)
			current = len(responses) - 1
			if len(leadingImages) > 0 {
				responses[current] = antigravityAttachInlineDataToFunctionResponse(responses[current], leadingImages)
				leadingImages = nil
			}
			continue
		}
		if _, ok := part["function_response"]; ok {
			responses = append(responses, part)
			current = len(responses) - 1
			if len(leadingImages) > 0 {
				responses[current] = antigravityAttachInlineDataToFunctionResponse(responses[current], leadingImages)
				leadingImages = nil
			}
			continue
		}
		if inline := antigravityNormalizeInlineDataPart(part); inline != nil {
			if current >= 0 {
				responses[current] = antigravityAttachInlineDataToFunctionResponse(responses[current], []map[string]any{inline})
			} else {
				leadingImages = append(leadingImages, inline)
			}
		}
	}
	return responses
}

func antigravityNormalizeInlineDataPart(part map[string]any) map[string]any {
	inline, ok := part["inlineData"].(map[string]any)
	if !ok {
		inline, ok = part["inline_data"].(map[string]any)
	}
	if !ok {
		return nil
	}
	data, _ := inline["data"].(string)
	if strings.TrimSpace(data) == "" {
		return nil
	}
	mimeType, _ := inline["mimeType"].(string)
	if mimeType == "" {
		mimeType, _ = inline["mime_type"].(string)
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return map[string]any{
		"inlineData": map[string]any{
			"mimeType": mimeType,
			"data":     data,
		},
	}
}

func antigravityAttachInlineDataToFunctionResponse(response map[string]any, images []map[string]any) map[string]any {
	if len(images) == 0 {
		return response
	}
	functionResponse, ok := response["functionResponse"].(map[string]any)
	if !ok {
		if legacy, ok := response["function_response"].(map[string]any); ok {
			functionResponse = legacy
		}
	}
	if functionResponse == nil {
		return response
	}
	parts, _ := functionResponse["parts"].([]any)
	if parts == nil {
		parts = make([]any, 0, len(images))
	}
	for _, image := range images {
		parts = append(parts, image)
	}
	functionResponse["parts"] = parts
	if _, ok := response["functionResponse"]; ok {
		response["functionResponse"] = functionResponse
	} else {
		response["function_response"] = functionResponse
	}
	return response
}

func antigravityCollectFunctionResponses(content map[string]any) []map[string]any {
	return antigravityCollectFunctionResponsesWithInlineData(content)
}

func antigravityNormalizeFunctionResponsePart(part map[string]any, fallbackName string) map[string]any {
	functionResponse, ok := part["functionResponse"].(map[string]any)
	if !ok {
		if legacy, ok := part["function_response"].(map[string]any); ok {
			functionResponse = legacy
		}
	}
	if functionResponse == nil {
		name := fallbackName
		if name == "" {
			name = "unknown"
		}
		return map[string]any{
			"functionResponse": map[string]any{
				"name": name,
				"response": map[string]any{
					"result": "",
				},
			},
		}
	}
	if name, _ := functionResponse["name"].(string); strings.TrimSpace(name) == "" && fallbackName != "" {
		functionResponse["name"] = fallbackName
	}
	return map[string]any{"functionResponse": functionResponse}
}

func newAntigravityNativeGeminiCountTokensResponseBody(r io.ReadCloser) (io.ReadCloser, error) {
	body, err := readBoundedAntigravityBody(r, antigravityErrorBodyLimit)
	if err != nil {
		return nil, fmt.Errorf("read Antigravity countTokens response: %w", err)
	}
	return io.NopCloser(bytes.NewReader(unwrapAntigravityCountTokensResponse(body))), nil
}

func unwrapAntigravityCountTokensResponse(body []byte) []byte {
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return body
	}
	if response, ok := envelope["response"].(map[string]any); ok {
		if _, ok := response["totalTokens"]; ok {
			out, err := json.Marshal(response)
			if err == nil {
				return out
			}
		}
	}
	if _, ok := envelope["totalTokens"]; ok {
		return body
	}
	return body
}

func newAntigravityNativeGeminiJSONResponseBody(r io.ReadCloser, reverseNameMap map[string]string) (io.ReadCloser, error) {
	body, err := readBoundedAntigravityBody(r, antigravityResponseBodyLimit)
	if err != nil {
		return nil, fmt.Errorf("read Antigravity Gemini JSON response: %w", err)
	}
	chunk := unwrapAntigravityNativeGeminiChunk(body)
	chunk = ensureGeminiFinishReason(chunk)
	chunk = antigravityRestoreNativeGeminiResponseNames(chunk, reverseNameMap)
	return io.NopCloser(bytes.NewReader(chunk)), nil
}

func unwrapAntigravityNativeGeminiChunk(body []byte) []byte {
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return body
	}
	if response, ok := envelope["response"].(map[string]any); ok {
		out, err := json.Marshal(restoreAntigravityNativeGeminiUsage(response))
		if err != nil {
			return body
		}
		return out
	}
	out, err := json.Marshal(restoreAntigravityNativeGeminiUsage(envelope))
	if err != nil {
		return body
	}
	return out
}

func restoreAntigravityNativeGeminiUsage(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if usage, ok := value["cpaUsageMetadata"].(map[string]any); ok {
		value["usageMetadata"] = usage
		delete(value, "cpaUsageMetadata")
	}
	return value
}

func ensureGeminiFinishReason(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	candidates, ok := payload["candidates"].([]any)
	if !ok {
		return body
	}
	changed := false
	for index, rawCandidate := range candidates {
		candidate, ok := rawCandidate.(map[string]any)
		if !ok {
			continue
		}
		finishReason, _ := candidate["finishReason"].(string)
		if strings.TrimSpace(finishReason) == "" {
			candidate["finishReason"] = "STOP"
			candidates[index] = candidate
			changed = true
		}
	}
	if !changed {
		return body
	}
	payload["candidates"] = candidates
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

type antigravityNativeGeminiSSEBody struct {
	source         io.ReadCloser
	events         *antigravitySSEReader
	queue          bytes.Buffer
	reverseNameMap map[string]string
	terminalErr    error
	sawTerminal    bool
	// jsonMode reads every event through antigravityRewriteJSONModeFrame;
	// jsonModeToolCalled is its cross-event state.
	jsonMode           bool
	jsonModeToolCalled bool
}

func newAntigravityNativeGeminiSSEResponseBody(r io.ReadCloser, reverseNameMap map[string]string) *antigravityNativeGeminiSSEBody {
	return &antigravityNativeGeminiSSEBody{source: r, events: newAntigravitySSEReader(bufio.NewReader(r)), reverseNameMap: reverseNameMap}
}

func (b *antigravityNativeGeminiSSEBody) Close() error {
	if b == nil || b.source == nil {
		return nil
	}
	return b.source.Close()
}

func (b *antigravityNativeGeminiSSEBody) enqueue(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	b.queue.WriteString("data: ")
	b.queue.Write(chunk)
	b.queue.WriteString("\n\n")
}

func (b *antigravityNativeGeminiSSEBody) observe(payload map[string]any) {
	switch strings.ToUpper(geminiFinishReason(payload)) {
	case "", "NONE", "FINISH_REASON_UNSPECIFIED":
	default:
		b.sawTerminal = true
	}
	if feedback, ok := payload["promptFeedback"].(map[string]any); ok {
		reason, _ := feedback["blockReason"].(string)
		switch strings.ToUpper(strings.TrimSpace(reason)) {
		case "", "NONE", "BLOCK_REASON_UNSPECIFIED":
		default:
			b.sawTerminal = true
		}
	}
}

func (b *antigravityNativeGeminiSSEBody) endError(err error) error {
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if b.sawTerminal {
		return io.EOF
	}
	return fmt.Errorf("antigravity Gemini stream ended before a terminal response: %w", io.ErrUnexpectedEOF)
}

// The final event is drained before Read reports the end of the stream,
// including when the stream has no trailing blank line.
func (b *antigravityNativeGeminiSSEBody) readEvent() ([]byte, error) {
	return b.events.next()
}

func antigravityNativeGeminiStreamPayload(data []byte) (map[string]any, error) {
	payload, err := decodeAntigravityStreamEnvelope(data)
	if err != nil {
		return nil, fmt.Errorf("decode antigravity Gemini stream event: %w", err)
	}
	for {
		if upstreamError, ok := payload["error"]; ok {
			return nil, newAntigravityStreamError(upstreamError)
		}
		response, ok := payload["response"].(map[string]any)
		if !ok {
			return payload, nil
		}
		payload = response
	}
}

func (b *antigravityNativeGeminiSSEBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for b.queue.Len() == 0 {
		if b.terminalErr != nil {
			return 0, b.terminalErr
		}
		data, readErr := b.readEvent()
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 {
			if readErr != nil {
				b.terminalErr = b.endError(readErr)
			}
			continue
		}
		if bytes.Equal(trimmed, []byte("[DONE]")) {
			b.terminalErr = b.endError(readErr)
			continue
		}
		if b.jsonMode {
			trimmed, b.jsonModeToolCalled = antigravityRewriteJSONModeFrame(trimmed, b.jsonModeToolCalled)
		}
		payload, err := antigravityNativeGeminiStreamPayload(trimmed)
		if err != nil {
			b.terminalErr = err
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				b.terminalErr = readErr
			}
			continue
		}
		chunk := unwrapAntigravityNativeGeminiChunk(trimmed)
		chunk = antigravityRestoreNativeGeminiResponseNames(chunk, b.reverseNameMap)
		b.observe(payload)
		b.enqueue(chunk)
		if readErr != nil {
			b.terminalErr = b.endError(readErr)
		}
	}
	return b.queue.Read(p)
}

func antigravityOAuthChannelAccountFilter(model string) auth.AccountFilter {
	base := antigravityChannelAccountFilter(model)
	return func(account *auth.Account) bool {
		if account == nil || account.AntigravityAuthKind() != auth.AntigravityAuthKindOAuth {
			return false
		}
		return base(account)
	}
}
