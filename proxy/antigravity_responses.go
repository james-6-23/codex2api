package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
)

const antigravityInteractionsAgent = "antigravity-preview-05-2026"

const (
	antigravityFunctionToolsEnv     = "ANTIGRAVITY_FUNCTION_TOOLS_ENABLED"
	antigravityUserProjectHeaderEnv = "ANTIGRAVITY_X_GOOG_USER_PROJECT_ENABLED"
	antigravitySensitivePhrasesEnv  = "ANTIGRAVITY_SYSTEM_INSTRUCTION_SENSITIVE_PHRASES"
	// Operators can pin a single Cloud Code endpoint for diagnostics. The
	// default follows the official Antigravity client: daily first, then the
	// production endpoint only for retryable daily failures.
	antigravityOAuthEndpointModeEnv = "ANTIGRAVITY_OAUTH_ENDPOINT_MODE"
)

const (
	antigravityResponseBodyLimit = 8 << 20
	antigravityErrorBodyLimit    = 1 << 20
)

var antigravityInteractionsEndpoint = "https://generativelanguage.googleapis.com/v1beta/interactions"

var antigravityOAuthEndpointBases = []string{
	"https://daily-cloudcode-pa.googleapis.com",
	"https://cloudcode-pa.googleapis.com",
}

const (
	antigravityOAuthDailyEndpoint    = "https://daily-cloudcode-pa.googleapis.com"
	antigravityOAuthSandboxEndpoint  = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityOfficialBodyUserAgent = "antigravity"
	antigravityOfficialHTTPUserAgent = "antigravity/hub/2.9.1 windows/amd64"
	antigravityZeroWidthSpace        = "\u200B"
)

var antigravityDefaultSensitivePhrases = []string{
	"Hermes Agent",
	"Nous Research",
	"Claude Agent SDK",
	"You are Codex, an agent based on GPT-5",
}

type antigravityHTTPClientKey struct {
	accountID int64
	proxyURL  string
}

var antigravityHTTPClients sync.Map

// antigravityOAuthEndpointList follows the official daily-to-production
// failover order by default. Sandbox remains explicit-only. Keep the base
// slice injectable for tests and controlled deployments.
func antigravityOAuthEndpointList() []string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(antigravityOAuthEndpointModeEnv)))
	switch mode {
	case "daily":
		return []string{antigravityOAuthDailyEndpoint}
	case "sandbox":
		return []string{antigravityOAuthSandboxEndpoint}
	case "all", "nonprod", "daily+sandbox", "sandbox+daily":
		return []string{
			antigravityOAuthDailyEndpoint,
			"https://cloudcode-pa.googleapis.com",
			antigravityOAuthSandboxEndpoint,
		}
	default:
		return append([]string(nil), antigravityOAuthEndpointBases...)
	}
}

// ExecuteAntigravityResponsesRequest adapts an OpenAI Responses request to the
// Cloud Code v1internal Gemini envelope used by both Antigravity projects.
func ExecuteAntigravityResponsesRequest(ctx context.Context, account *auth.Account, model string, body []byte, stream bool, proxyURL string) (*http.Response, error) {
	if account == nil {
		return nil, fmt.Errorf("antigravity account is nil")
	}
	if account.AntigravityAuthKind() == auth.AntigravityAuthKindAPIKey {
		return executeAntigravityInteractionsRequest(ctx, account, model, body, stream, proxyURL)
	}
	project, bearer := account.AntigravityCredentials()
	if project == "" || bearer == "" {
		return nil, fmt.Errorf("antigravity account %d has no project_id or access token", account.ID())
	}
	gemini, err := responsesToGeminiInternal(body, project, model)
	if err != nil {
		return nil, err
	}
	method := "generateContent"
	query := ""
	if stream {
		method, query = "streamGenerateContent", "?alt=sse"
	}
	client, err := antigravityHTTPClient(account.ID(), proxyURL)
	if err != nil {
		return nil, err
	}
	var last error
	var lastResponse *http.Response
	var lastRetryableResponse *http.Response
	discardLastRetryable := func() {
		if lastRetryableResponse != nil && lastRetryableResponse.Body != nil {
			_ = lastRetryableResponse.Body.Close()
		}
		lastRetryableResponse = nil
	}
	useUserProject := antigravityUserProjectHeaderEnabled()
	payload, _ := json.Marshal(gemini)
	sameEndpointBudget := &antigravitySameEndpointRetryBudget{}
	for headerAttempt := 0; headerAttempt < 2; headerAttempt++ {
		retryWithoutUserProject := false
		for _, base := range antigravityOAuthEndpointList() {
			endpoint := strings.TrimRight(base, "/") + "/v1internal:" + method + query
			var resp *http.Response
			for {
				req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
				if reqErr != nil {
					return nil, reqErr
				}
				req.Header.Set("Authorization", "Bearer "+bearer)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("User-Agent", antigravityOfficialHTTPUserAgent)
				if useUserProject {
					req.Header.Set("x-goog-user-project", project)
				}
				var doErr error
				resp, doErr = client.Do(req)
				if doErr != nil {
					last = doErr
					resp = nil
					break
				}
				if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
					break
				}
				body, readErr := readBoundedAntigravityBody(resp.Body, antigravityErrorBodyLimit)
				if readErr != nil {
					last = fmt.Errorf("antigravity upstream HTTP %d body: %w", resp.StatusCode, readErr)
					body = []byte(`{"error":{"message":"Antigravity upstream error response exceeded the safe read limit","type":"upstream_error"}}`)
					resp.Header.Set("Content-Type", "application/json")
				} else {
					last = fmt.Errorf("antigravity upstream HTTP %d", resp.StatusCode)
				}
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				// A sub-second RATE_LIMIT_EXCEEDED or a shared MODEL_CAPACITY_EXHAUSTED
				// is cheaper to wait out right here than to switch endpoint or account.
				if wait, retry := sameEndpointBudget.retryDelay(resp.StatusCode, body); retry {
					if sleepErr := antigravitySleep(ctx, wait); sleepErr == nil {
						continue
					}
				}
				if lastRetryableResponse != nil {
					_ = lastRetryableResponse.Body.Close()
				}
				lastResponse = resp
				lastRetryableResponse = resp
				resp = nil
				break
			}
			if resp == nil {
				continue
			}
			if resp.StatusCode == http.StatusForbidden {
				body, readErr := readBoundedAntigravityBody(resp.Body, antigravityErrorBodyLimit)
				if readErr != nil {
					body = []byte(`{"error":{"message":"Antigravity upstream error response exceeded the safe read limit","type":"upstream_error"}}`)
					resp.Header.Set("Content-Type", "application/json")
				}
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				// Antigravity Manager retries every 403 once without the quota
				// consumer header. Some managed projects can call Cloud Code with
				// their bearer but are not allowed to use themselves as the billing
				// project, which otherwise looks exactly like SERVICE_DISABLED.
				if useUserProject {
					_ = resp.Body.Close()
					retryWithoutUserProject = true
					break
				}
				if readErr == nil && antigravityEndpointServiceDisabled(body) {
					last = fmt.Errorf("antigravity endpoint %s is disabled for this project", strings.TrimRight(base, "/"))
					lastResponse = resp
					continue
				}
				discardLastRetryable()
				return resp, nil
			}
			if resp.StatusCode == http.StatusNotFound {
				body, _ := readBoundedAntigravityBody(resp.Body, antigravityErrorBodyLimit)
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				last = fmt.Errorf("antigravity upstream HTTP %d", resp.StatusCode)
				lastResponse = resp
				continue
			}
			if resp.StatusCode == http.StatusBadRequest {
				body, readErr := readBoundedAntigravityBody(resp.Body, antigravityErrorBodyLimit)
				if readErr != nil {
					body = []byte(`{"error":{"message":"Antigravity upstream error response exceeded the safe read limit","type":"upstream_error"}}`)
					resp.Header.Set("Content-Type", "application/json")
				}
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				if readErr == nil && antigravityEndpointLocationUnsupported(body) {
					// Location eligibility is authoritative for the official daily
					// route. Return the real 400 instead of hiding it behind a later
					// production 429/5xx response.
					discardLastRetryable()
					return resp, nil
				}
				discardLastRetryable()
				return resp, nil
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				discardLastRetryable()
				return resp, nil
			}
			if stream {
				resp.Body = newAntigravitySSEResponseBody(resp.Body, model)
				resp.Header.Set("Content-Type", "text/event-stream")
			} else {
				converted, convertErr := newAntigravityJSONResponseBody(resp.Body, model)
				if convertErr != nil {
					last = convertErr
					continue
				}
				resp.Body = converted
			}
			discardLastRetryable()
			return resp, nil
		}
		if retryWithoutUserProject {
			useUserProject = false
			continue
		}
		break
	}
	if lastRetryableResponse != nil {
		return lastRetryableResponse, nil
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, &Error{
		Code:       ErrorCodeUpstreamError,
		Message:    "Antigravity upstream returned no valid response",
		Type:       ErrorTypeUpstreamError,
		Retryable:  true,
		HTTPStatus: http.StatusBadGateway,
		Cause:      last,
	}
}

// antigravityUpstreamEndpoint is the usage-log path for a Cloud Code call.
func antigravityUpstreamEndpoint(stream bool) string {
	if stream {
		return "/v1internal:streamGenerateContent"
	}
	return "/v1internal:generateContent"
}

// antigravityHTTPClient keeps one native HTTP/1.1 connection pool per account
// and effective proxy. The official client reuses its daily Cloud Code
// connection; sharing a short-lived generic client loses that routing affinity.
func antigravityHTTPClient(accountID int64, proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	key := antigravityHTTPClientKey{accountID: accountID, proxyURL: proxyURL}
	if cached, ok := antigravityHTTPClients.Load(key); ok {
		return cached.(*http.Client), nil
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 60 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: nil},
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		MaxConnsPerHost:       20,
		IdleConnTimeout:       10 * time.Minute,
		ExpectContinueTimeout: time.Second,
	}
	if proxyURL != "" {
		if err := auth.ConfigureTransportProxy(transport, proxyURL, dialer); err != nil {
			transport.CloseIdleConnections()
			return nil, fmt.Errorf("configure Antigravity proxy: %w", err)
		}
	}

	client := &http.Client{Transport: transport}
	actual, loaded := antigravityHTTPClients.LoadOrStore(key, client)
	if loaded {
		transport.CloseIdleConnections()
		return actual.(*http.Client), nil
	}
	return client, nil
}

func antigravityEndpointServiceDisabled(body []byte) bool {
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	errorValue, _ := envelope["error"].(map[string]any)
	return antigravityHasServiceDisabledReason(errorValue)
}

func antigravityEndpointLocationUnsupported(body []byte) bool {
	message := strings.ToLower(string(body))
	return strings.Contains(message, "user location is not supported") ||
		strings.Contains(message, "location is not supported for the api use")
}

func antigravityUserProjectHeaderEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(antigravityUserProjectHeaderEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func antigravityHasServiceDisabledReason(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if strings.EqualFold(strings.TrimSpace(key), "reason") || strings.EqualFold(strings.TrimSpace(key), "status") {
				if reason, ok := item.(string); ok && strings.EqualFold(strings.TrimSpace(reason), "SERVICE_DISABLED") {
					return true
				}
			}
			if antigravityHasServiceDisabledReason(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if antigravityHasServiceDisabledReason(item) {
				return true
			}
		}
	}
	return false
}

func executeAntigravityInteractionsRequest(ctx context.Context, account *auth.Account, model string, body []byte, stream bool, proxyURL string) (*http.Response, error) {
	apiKey := account.AntigravityAPIKey()
	if apiKey == "" {
		return nil, fmt.Errorf("antigravity API-key account %d has no api_key", account.ID())
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode Responses request: %w", err)
	}
	if _, ok := request["input"]; !ok {
		return nil, fmt.Errorf("antigravity interactions request requires input")
	}
	reasoning, _ := request["reasoning"].(map[string]any)
	wireModel := antigravityGeminiResolvedModel(model, reasoning)
	if variant, ok := antigravityResolvedVariant(model, reasoning); ok {
		reasoning, _ := request["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		// Send the normalized tier matching the chosen backing. Compatibility
		// aliases remain fixed even when the body requests a conflicting effort.
		reasoning["effort"] = variant.level
		request["reasoning"] = reasoning
	}
	request["model"] = wireModel
	request["agent"] = antigravityInteractionsAgent
	request["stream"] = stream
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	client, err := auth.BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityInteractionsEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", map[bool]string{true: "text/event-stream", false: "application/json"}[stream])
	req.Header.Set("x-goog-api-key", apiKey)
	return client.Do(req)
}

func responsesToGeminiInternal(raw []byte, project, model string) (map[string]any, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode Responses request: %w", err)
	}
	textConfig, _ := in["text"].(map[string]any)
	reasoning, _ := in["reasoning"].(map[string]any)
	wireModel := antigravityGeminiResolvedModel(model, reasoning)
	contents := make([]any, 0)
	addParts := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if len(contents) > 0 {
			last, _ := contents[len(contents)-1].(map[string]any)
			if last != nil && last["role"] == role {
				lastParts, _ := last["parts"].([]any)
				last["parts"] = append(lastParts, parts...)
				return
			}
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	add := func(role, text string) {
		if strings.TrimSpace(text) != "" {
			addParts(role, []any{map[string]any{"text": text}})
		}
	}
	callNames := map[string]string{}
	systemParts := make([]string, 0, 2)
	if s, ok := in["instructions"].(string); ok {
		if s = strings.TrimSpace(s); s != "" {
			systemParts = append(systemParts, s)
		}
	}
	switch v := in["input"].(type) {
	case string:
		add("user", v)
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, antigravityOAuthUnsupported("non-message input items")
			}
			itemType := lowerStringField(m, "type")
			switch itemType {
			case "", "message":
				role, _ := m["role"].(string)
				role = strings.ToLower(strings.TrimSpace(role))
				if role == "" {
					role = "user"
				}
				text, textErr := responseItemText(m["content"])
				if textErr != nil {
					return nil, textErr
				}
				switch role {
				case "assistant":
					add("model", text)
				case "system", "developer":
					if strings.TrimSpace(text) != "" {
						systemParts = append(systemParts, text)
					}
				case "user":
					add("user", text)
				default:
					return nil, antigravityOAuthUnsupported("message role " + role)
				}
			case "function_call":
				name, _ := m["name"].(string)
				name = strings.TrimSpace(name)
				callID := firstAntigravityString(m, "call_id", "id")
				if name == "" || callID == "" {
					return nil, antigravityOAuthUnsupported("function_call without name or call_id")
				}
				arguments, argumentErr := antigravityGeminiFunctionArguments(m["arguments"])
				if argumentErr != nil {
					return nil, argumentErr
				}
				functionPart := map[string]any{"functionCall": map[string]any{"name": name, "args": arguments, "id": callID}}
				if antigravityGeminiNeedsToolSignature(wireModel) {
					functionPart["thoughtSignature"] = "skip_thought_signature_validator"
					functionPart["thought_signature"] = "skip_thought_signature_validator"
				}
				callNames[callID] = name
				addParts("model", []any{functionPart})
			case "function_call_output":
				callID := firstAntigravityString(m, "call_id", "id")
				name := callNames[callID]
				if callID == "" || name == "" {
					return nil, antigravityOAuthUnsupported("orphan function_call_output")
				}
				output, outputErr := antigravityFunctionOutputText(m["output"])
				if outputErr != nil {
					return nil, outputErr
				}
				addParts("user", []any{map[string]any{"functionResponse": map[string]any{
					"name": name, "response": map[string]any{"result": output}, "id": callID,
				}}})
			case "reasoning":
				// Codex and the Anthropic bridge echo previous reasoning items
				// back as conversation history. Their payload is an opaque
				// Codex-lineage blob with no Gemini contents equivalent, and the
				// tool-call thought signature is already stubbed separately, so
				// carrying the turn forward without them is correct. Rejecting
				// the request would break every multi-turn tool conversation.
				continue
			case "additional_tools":
				// Codex may include this Responses input item as a client-side
				// capability envelope. The actual callable tools, when supported,
				// are carried in the top-level `tools` field and are converted above.
				// It is not conversational content and has no Gemini contents
				// equivalent, so ignore it instead of rejecting an otherwise valid
				// request.
				continue
			default:
				return nil, antigravityOAuthUnsupported("input item type " + itemType)
			}
		}
	case nil:
		return nil, antigravityOAuthUnsupported("requests without input")
	default:
		return nil, antigravityOAuthUnsupported("non-text input")
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("antigravity request contains no supported text input")
	}
	request := map[string]any{"contents": contents}
	if len(systemParts) > 0 {
		request["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": antigravityObfuscateSystemInstruction(strings.Join(systemParts, "\n\n"))}}}
	}
	request["sessionId"] = antigravitySessionID(in, contents)
	if tools, ok := in["tools"].([]any); ok {
		if declarations := antigravityGeminiFunctionDeclarations(tools); len(declarations) > 0 && antigravityGeminiFunctionToolsEnabled() && antigravityGeminiSupportsFunctionTools(wireModel) {
			request["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
			request["toolConfig"] = map[string]any{
				"functionCallingConfig":            map[string]any{"mode": antigravityGeminiToolMode(in["tool_choice"])},
				"includeServerSideToolInvocations": true,
			}
		} else if len(declarations) > 0 && antigravityGeminiToolChoiceRequiresFunctions(in["tool_choice"]) {
			return nil, antigravityOAuthUnsupported("forced function tools while the experimental function bridge is disabled")
		}
	}
	generationConfig := map[string]any{}
	if structuredConfig, structuredErr := antigravityGeminiStructuredTextConfig(textConfig); structuredErr != nil {
		return nil, structuredErr
	} else {
		for key, value := range structuredConfig {
			generationConfig[key] = value
		}
	}
	isGeminiModel := strings.HasPrefix(strings.ToLower(strings.TrimSpace(wireModel)), "gemini-")
	if n, ok := in["max_output_tokens"].(float64); ok && !isGeminiModel {
		maxOutputTokens := int(n)
		if limit := antigravityGeminiMaxOutputTokens(wireModel); maxOutputTokens > limit {
			maxOutputTokens = limit
		}
		generationConfig["maxOutputTokens"] = maxOutputTokens
	}
	if t, ok := in["temperature"].(float64); ok {
		generationConfig["temperature"] = t
	}
	if thinkingBudget, enabled := antigravityGeminiThinkingBudget(model, wireModel, reasoning); enabled {
		generationConfig["thinkingConfig"] = map[string]any{
			"includeThoughts": true,
			"thinkingBudget":  thinkingBudget,
		}
	}
	if len(generationConfig) > 0 {
		request["generationConfig"] = generationConfig
	}
	return map[string]any{
		"project":     project,
		"requestId":   antigravityRequestID(),
		"request":     request,
		"model":       wireModel,
		"userAgent":   antigravityOfficialBodyUserAgent,
		"requestType": "agent",
	}, nil
}

func antigravitySensitivePhrases() []string {
	phrases := append([]string(nil), antigravityDefaultSensitivePhrases...)
	for _, phrase := range strings.FieldsFunc(os.Getenv(antigravitySensitivePhrasesEnv), func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	}) {
		if phrase = strings.TrimSpace(phrase); utf8.RuneCountInString(phrase) >= 2 && !strings.Contains(phrase, antigravityZeroWidthSpace) {
			phrases = append(phrases, phrase)
		}
	}
	sort.SliceStable(phrases, func(i, j int) bool { return len(phrases[i]) > len(phrases[j]) })
	return phrases
}

func antigravityObfuscateSystemInstruction(text string) string {
	phrases := antigravitySensitivePhrases()
	if text == "" || len(phrases) == 0 {
		return text
	}
	escaped := make([]string, 0, len(phrases))
	for _, phrase := range phrases {
		phrase = strings.TrimSpace(phrase)
		if utf8.RuneCountInString(phrase) >= 2 && !strings.Contains(phrase, antigravityZeroWidthSpace) {
			escaped = append(escaped, regexp.QuoteMeta(phrase))
		}
	}
	if len(escaped) == 0 {
		return text
	}
	matcher, err := regexp.Compile("(?i)" + strings.Join(escaped, "|"))
	if err != nil {
		return text
	}
	return matcher.ReplaceAllStringFunc(text, func(match string) string {
		if strings.Contains(match, antigravityZeroWidthSpace) {
			return match
		}
		_, size := utf8.DecodeRuneInString(match)
		if size <= 0 || size >= len(match) {
			return match
		}
		return match[:size] + antigravityZeroWidthSpace + match[size:]
	})
}

func antigravityStructuredTextRequested(textConfig map[string]any) bool {
	if textConfig == nil {
		return false
	}
	format, exists := textConfig["format"]
	if !exists || format == nil {
		return false
	}
	switch typed := format.(type) {
	case string:
		formatType := strings.ToLower(strings.TrimSpace(typed))
		return formatType != "" && formatType != "text"
	case map[string]any:
		formatType := lowerStringField(typed, "type")
		return formatType != "" && formatType != "text"
	default:
		return true
	}
}

func antigravityGeminiStructuredTextConfig(textConfig map[string]any) (map[string]any, error) {
	if !antigravityStructuredTextRequested(textConfig) {
		return nil, nil
	}
	format, ok := textConfig["format"].(map[string]any)
	if !ok {
		return nil, antigravityOAuthUnsupported("structured text output format")
	}
	formatType := lowerStringField(format, "type")
	switch formatType {
	case "json_object":
		return map[string]any{"responseMimeType": "application/json"}, nil
	case "json_schema":
		schema := format["schema"]
		if schema == nil {
			if nested, nestedOK := format["json_schema"].(map[string]any); nestedOK {
				schema = nested["schema"]
			}
		}
		root, rootOK := schema.(map[string]any)
		if !rootOK {
			return nil, antigravityOAuthUnsupported("json_schema output without a schema")
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
			return nil, antigravityOAuthUnsupported("json_schema output with an unusable schema")
		}
		return map[string]any{
			"responseMimeType": "application/json",
			"responseSchema":   cleaned,
		}, nil
	default:
		return nil, antigravityOAuthUnsupported("structured text output type " + formatType)
	}
}

func antigravityGeminiFunctionArguments(raw any) (any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	if encoded, ok := raw.(string); ok {
		if strings.TrimSpace(encoded) == "" {
			return map[string]any{}, nil
		}
		var arguments any
		if err := json.Unmarshal([]byte(encoded), &arguments); err != nil {
			return nil, antigravityOAuthUnsupported("function_call arguments that are not valid JSON")
		}
		return arguments, nil
	}
	return raw, nil
}

func antigravityFunctionOutputText(raw any) (string, error) {
	if raw == nil {
		return "", nil
	}
	if text, ok := raw.(string); ok {
		return text, nil
	}
	if parts, ok := raw.([]any); ok {
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			partValue, ok := part.(map[string]any)
			if !ok {
				return "", antigravityOAuthUnsupported("non-text function_call_output parts")
			}
			partType := lowerStringField(partValue, "type")
			if partType != "" && partType != "input_text" && partType != "output_text" && partType != "text" {
				return "", antigravityOAuthUnsupported("function_call_output part type " + partType)
			}
			if text, ok := partValue["text"].(string); ok {
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "\n"), nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("encode function_call_output: %w", err)
	}
	return string(encoded), nil
}

func antigravityGeminiNeedsToolSignature(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(name, "gemini") && (strings.Contains(name, "flash") || strings.Contains(name, "pro") || strings.Contains(name, "thinking"))
}

func antigravityGeminiSupportsFunctionTools(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	switch name {
	case "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-2.5-flash-thinking", "gemini-3.1-flash-lite":
		return false
	default:
		return true
	}
}

// antigravityGeminiFunctionToolsEnabled reports whether Responses function
// tools are bridged into Gemini functionDeclarations. Dropping them is not a
// safe degradation: the upstream still receives the agent system instruction
// describing those tools, emits a call it was never allowed to declare, and
// terminates the turn with MALFORMED_FUNCTION_CALL. The bridge is on by
// default; operators can still pin it off for diagnostics.
func antigravityGeminiFunctionToolsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(antigravityFunctionToolsEnv))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func antigravityGeminiToolChoiceRequiresFunctions(choice any) bool {
	switch value := choice.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "required")
	case map[string]any:
		typeName := lowerStringField(value, "type")
		return typeName == "function" || typeName == "required"
	default:
		return false
	}
}

func antigravityGeminiReasoningTier(reasoning map[string]any) string {
	switch lowerStringField(reasoning, "effort") {
	case "none", "minimal", "low":
		return "low"
	case "high", "xhigh", "max", "ultra":
		return "high"
	case "medium", "":
		return "medium"
	default:
		return "medium"
	}
}

func antigravityGeminiResolvedModel(model string, reasoning map[string]any) string {
	if variant, ok := antigravityResolvedVariant(model, reasoning); ok {
		return variant.wireModel
	}
	if wireModel, ok := antigravityPublicModelWireID(model); ok {
		return wireModel
	}
	return strings.TrimSpace(model)
}

func antigravityGeminiThinkingBudget(requestedModel, wireModel string, reasoning map[string]any) (int, bool) {
	var budget int
	if _, ok := antigravityPublicModel(requestedModel); ok {
		variant, resolved := antigravityResolvedVariant(requestedModel, reasoning)
		if !resolved || variant.thinkingBudget <= 0 {
			return 0, false
		}
		budget = variant.thinkingBudget
	} else if logical, ok := antigravityLogicalCompatibilityModel(requestedModel); ok {
		variant, resolved := antigravityResolvedVariant(logical.id, reasoning)
		if !resolved {
			return 0, false
		}
		budget = variant.thinkingBudget
	} else {
		// Raw backing IDs remain accepted for internal/account mappings, but
		// only an explicit reasoning request enables a tiered raw model.
		if len(reasoning) == 0 {
			return 0, false
		}
		effort := antigravityGeminiReasoningTier(reasoning)
		switch strings.ToLower(strings.TrimSpace(wireModel)) {
		case "gemini-3.5-flash-extra-low":
			budget = 1000
		case "gemini-3.5-flash-low":
			budget = 4000
		case "gemini-3-flash-agent":
			budget = 10000
		case "gemini-3.6-flash-low":
			budget = 4096
		case "gemini-3.6-flash-medium":
			budget = 8192
		case "gemini-3.6-flash-high":
			budget = 24576
		case "gemini-3.7-flash-tiered":
			switch effort {
			case "low":
				budget = 4096
			case "high":
				budget = 24576
			default:
				budget = 8192
			}
		case "gemini-3.1-pro-low":
			budget = 1001
		case "gemini-pro-agent":
			budget = 10001
		default:
			return 0, false
		}
	}
	if cap := antigravityGeminiThinkingBudgetCap(wireModel); cap <= 0 {
		return 0, false
	} else if budget > cap {
		budget = cap
	}
	if outputCap := antigravityGeminiMaxOutputTokens(wireModel); budget >= outputCap {
		budget = outputCap - 1
	}
	return budget, budget > 0
}

func antigravityGeminiThinkingBudgetCap(model string) int {
	name := strings.ToLower(strings.TrimSpace(model))
	switch name {
	case "gemini-3.5-flash-extra-low":
		return 1000
	case "gemini-3.5-flash-low":
		return 4000
	case "gemini-3-flash-agent":
		return 10000
	case "gemini-3.6-flash-low":
		return 4096
	case "gemini-3.6-flash-medium":
		return 8192
	case "gemini-3.6-flash-high", "gemini-3.7-flash-tiered":
		return 24576
	case "gemini-3.1-pro-low":
		return 1001
	case "gemini-pro-agent":
		return 10001
	default:
		return 0
	}
}

func antigravityGeminiMaxOutputTokens(model string) int {
	name := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(name, "claude"):
		return 64000
	case strings.Contains(name, "gpt-oss"):
		return 32768
	case strings.Contains(name, "gemini-3.5-flash"), strings.Contains(name, "gemini-3-flash"):
		return 65536
	default:
		return 65535
	}
}

func antigravityGeminiToolMode(choice any) string {
	switch value := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "none":
			return "NONE"
		case "auto":
			return "AUTO"
		case "required":
			return "ANY"
		default:
			return "ANY"
		}
	case map[string]any:
		if lowerStringField(value, "type") == "none" {
			return "NONE"
		}
		return "ANY"
	default:
		return "VALIDATED"
	}
}

func antigravityGeminiFunctionDeclarations(tools []any) []any {
	declarations := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok || lowerStringField(tool, "type") != "function" {
			// Codex commonly sends built-in tools next to function tools. The
			// v1internal bridge cannot faithfully express them, so ignore them.
			continue
		}
		function := tool
		if nested, ok := tool["function"].(map[string]any); ok {
			function = nested
		}
		name, _ := function["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		declaration := map[string]any{"name": name}
		if description, ok := function["description"].(string); ok && strings.TrimSpace(description) != "" {
			declaration["description"] = description
		}
		declaration["parameters"] = antigravityGeminiParameters(function["parameters"])
		declarations = append(declarations, declaration)
	}
	sort.SliceStable(declarations, func(i, j int) bool {
		left, _ := declarations[i].(map[string]any)["name"].(string)
		right, _ := declarations[j].(map[string]any)["name"].(string)
		return left < right
	})
	return declarations
}

func antigravityGeminiParameters(raw any) map[string]any {
	root, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{"type": "OBJECT", "properties": map[string]any{}}
	}
	definitions := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := root[key].(map[string]any); ok {
			for name, definition := range defs {
				definitions[name] = definition
			}
		}
	}
	cleaned, _ := antigravityCleanGeminiSchema(root, definitions, map[string]bool{}, 0).(map[string]any)
	if cleaned == nil {
		cleaned = map[string]any{}
	}
	if _, ok := cleaned["type"]; !ok {
		cleaned["type"] = "OBJECT"
	}
	if _, ok := cleaned["properties"]; !ok {
		cleaned["properties"] = map[string]any{}
	}
	return cleaned
}

func antigravityCleanGeminiSchema(value any, definitions map[string]any, resolving map[string]bool, depth int) any {
	if depth > 32 {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		if ref, _ := typed["$ref"].(string); ref != "" {
			const defsPrefix = "#/$defs/"
			const definitionsPrefix = "#/definitions/"
			name := ""
			switch {
			case strings.HasPrefix(ref, defsPrefix):
				name = strings.TrimPrefix(ref, defsPrefix)
			case strings.HasPrefix(ref, definitionsPrefix):
				name = strings.TrimPrefix(ref, definitionsPrefix)
			}
			if definition, ok := definitions[name]; ok && name != "" && !resolving[name] {
				resolving[name] = true
				if expanded, ok := antigravityCleanGeminiSchema(definition, definitions, resolving, depth+1).(map[string]any); ok {
					for key, item := range expanded {
						out[key] = item
					}
				}
				delete(resolving, name)
			}
		}
		nullable := false
		for key, item := range typed {
			switch key {
			case "$ref", "$defs", "definitions", "$schema", "$id", "$comment",
				"additionalProperties", "unevaluatedProperties", "patternProperties",
				"format", "default", "examples", "example", "deprecated",
				"readOnly", "writeOnly", "contentEncoding", "contentMediaType":
				continue
			case "type":
				if schemaType, isNullable := antigravityGeminiSchemaType(item); schemaType != "" {
					out[key] = schemaType
					nullable = isNullable
				}
			case "const":
				out["enum"] = []any{item}
			default:
				if cleaned := antigravityCleanGeminiSchema(item, definitions, resolving, depth+1); cleaned != nil {
					out[key] = cleaned
				}
			}
		}
		if nullable {
			out["nullable"] = true
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			if cleaned := antigravityCleanGeminiSchema(item, definitions, resolving, depth+1); cleaned != nil {
				out = append(out, cleaned)
			}
		}
		return out
	default:
		return value
	}
}

func antigravityGeminiSchemaType(value any) (string, bool) {
	if single, ok := value.(string); ok {
		if strings.EqualFold(strings.TrimSpace(single), "null") {
			return "", true
		}
		return strings.ToUpper(strings.TrimSpace(single)), false
	}
	values, _ := value.([]any)
	nullable := false
	schemaType := ""
	for _, item := range values {
		single, ok := item.(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(single), "null") {
			nullable = true
			continue
		}
		if schemaType == "" {
			schemaType = strings.ToUpper(strings.TrimSpace(single))
		}
	}
	return schemaType, nullable
}

func antigravityOAuthUnsupported(feature string) error {
	return &Error{
		Code:       "antigravity_feature_unsupported",
		Message:    "Antigravity OAuth adapter does not safely support " + feature,
		Type:       ErrorTypeInvalidRequest,
		HTTPStatus: http.StatusBadRequest,
	}
}

func responseItemText(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	arr, _ := v.([]any)
	var out []string
	for _, x := range arr {
		m, ok := x.(map[string]any)
		if !ok {
			return "", antigravityOAuthUnsupported("non-text content parts")
		}
		partType := lowerStringField(m, "type")
		if partType != "" && partType != "input_text" && partType != "output_text" && partType != "text" {
			return "", antigravityOAuthUnsupported("content part type " + partType)
		}
		if s, ok := m["text"].(string); ok {
			out = append(out, s)
		} else if s, ok := m["content"].(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n"), nil
}

func lowerStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.ToLower(strings.TrimSpace(s))
}

// antigravitySessionID derives the Cloud Code sessionId for one request. The
// native client uses one stable id per conversation, so the gateway derives a
// stable value from the strongest conversation identifier the downstream
// request carries: an explicit prompt_cache_key or session/user metadata,
// otherwise the first user turn's text. Distinct conversations therefore get
// distinct ids while every turn of one conversation keeps the same id. A
// single constant shared by every account and conversation would be a pool
// fingerprint, which is why no fixed fallback is used.
func antigravitySessionID(in map[string]any, contents []any) string {
	if key := antigravitySessionSeed(in, contents); key != "" {
		return antigravitySessionIDFromSeed(key)
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return antigravitySessionIDFromDigest(raw)
}

func antigravitySessionSeed(in map[string]any, contents []any) string {
	if in != nil {
		if key, _ := in["prompt_cache_key"].(string); strings.TrimSpace(key) != "" {
			return "prompt_cache_key:" + strings.TrimSpace(key)
		}
		if metadata, ok := in["metadata"].(map[string]any); ok {
			for _, field := range []string{"session_id", "conversation_id", "user_id"} {
				if value, _ := metadata[field].(string); strings.TrimSpace(value) != "" {
					return "metadata." + field + ":" + strings.TrimSpace(value)
				}
			}
		}
		if user, _ := in["user"].(string); strings.TrimSpace(user) != "" {
			return "user:" + strings.TrimSpace(user)
		}
	}
	for _, content := range contents {
		entry, _ := content.(map[string]any)
		if entry == nil || entry["role"] != "user" {
			continue
		}
		parts, _ := entry["parts"].([]any)
		for _, part := range parts {
			partValue, _ := part.(map[string]any)
			if text, _ := partValue["text"].(string); strings.TrimSpace(text) != "" {
				return "first_user_text:" + text
			}
		}
	}
	return ""
}

func antigravitySessionIDFromSeed(seed string) string {
	sum := sha256.Sum256([]byte("codex2api:antigravity:session\x00" + seed))
	return antigravitySessionIDFromDigest(sum[:8])
}

// antigravitySessionIDFromDigest renders eight digest bytes the way the native
// client renders its session ids: a negative-signed decimal of a 63-bit value.
func antigravitySessionIDFromDigest(digest []byte) string {
	value := int64(binary.BigEndian.Uint64(digest[:8]) & 0x7FFFFFFFFFFFFFFF)
	return "-" + strconv.FormatInt(value, 10)
}

func antigravityRequestID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("agent/%d/%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}

func readBoundedAntigravityBody(r io.ReadCloser, limit int64) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("empty response body")
	}
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	_ = r.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return body, nil
}

func newAntigravityJSONResponseBody(r io.ReadCloser, model string) (io.ReadCloser, error) {
	// Reading the complete upstream body may block until generation finishes.
	// Freeze the synthetic Responses creation time before that work starts.
	createdAt := time.Now().Unix()
	return newAntigravityJSONResponseBodyAt(r, model, createdAt)
}

func newAntigravityJSONResponseBodyAt(r io.ReadCloser, model string, createdAt int64) (io.ReadCloser, error) {
	body, err := readBoundedAntigravityBody(r, antigravityResponseBodyLimit)
	if err != nil {
		return nil, fmt.Errorf("read Antigravity JSON response: %w", err)
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode Antigravity JSON response: %w", err)
	}
	if v, ok := env["response"].(map[string]any); ok {
		env = v
	}
	text := extractGeminiText(env)
	functionCalls := extractGeminiFunctionCalls(env)
	finishReason := geminiFinishReason(env)
	blocked := geminiBlocked(env)
	status := geminiFinishStatus(finishReason)
	unsupportedOutput := geminiHasUnsupportedOutput(env)
	if _, ok := env["error"]; ok || blocked || unsupportedOutput || (lenGeminiCandidates(env) == 0) {
		status = "failed"
	}
	if status == "failed" {
		// Safety/policy rejected candidate text must never be exposed in a failed
		// response. The terminal error is the only safe downstream payload.
		text = ""
		functionCalls = nil
	}
	message := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	output := make([]any, 0, 1+len(functionCalls))
	if text != "" || len(functionCalls) == 0 {
		output = append(output, message)
	}
	for _, functionCall := range functionCalls {
		output = append(output, antigravityFunctionCallItem(functionCall, "completed", functionCall.Arguments))
	}
	response := map[string]any{
		"id": "resp_" + antigravityRequestID(), "object": "response", "created_at": createdAt, "status": status, "model": model,
		"output":      output,
		"output_text": text,
	}
	if status == "failed" {
		message := "antigravity upstream returned no usable candidate"
		if blocked {
			message = "antigravity response blocked by safety policy"
		} else if unsupportedOutput {
			message = "antigravity upstream returned an unsupported non-text output"
		} else if finishReason != "" {
			message = "antigravity response terminated with " + finishReason
		}
		response["error"] = map[string]any{"type": "upstream_error", "message": message}
	} else if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if usage, ok := env["usageMetadata"].(map[string]any); ok {
		response["usage"] = antigravityUsage(usage)
	}
	out, _ := json.Marshal(response)
	return io.NopCloser(bytes.NewReader(out)), nil
}

type antigravityFunctionCall struct {
	ItemID    string
	CallID    string
	Name      string
	Arguments string
}

func extractGeminiFunctionCalls(value map[string]any) []antigravityFunctionCall {
	candidates, _ := value["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	calls := make([]antigravityFunctionCall, 0)
	for _, part := range parts {
		partValue, _ := part.(map[string]any)
		functionCall, _ := partValue["functionCall"].(map[string]any)
		if functionCall == nil {
			functionCall, _ = partValue["function_call"].(map[string]any)
		}
		if functionCall == nil {
			continue
		}
		name, _ := functionCall["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		arguments := "{}"
		if rawArguments, ok := functionCall["args"]; ok {
			switch typed := rawArguments.(type) {
			case string:
				if strings.TrimSpace(typed) != "" {
					arguments = typed
				}
			default:
				if encoded, err := json.Marshal(typed); err == nil {
					arguments = string(encoded)
				}
			}
		}
		callID := firstAntigravityString(functionCall, "id", "callId", "call_id")
		if callID == "" {
			callID = "call_ag_" + antigravityRandomHex(12)
		}
		calls = append(calls, antigravityFunctionCall{
			ItemID:    "fc_ag_" + antigravityRandomHex(12),
			CallID:    callID,
			Name:      name,
			Arguments: arguments,
		})
	}
	return calls
}

func firstAntigravityString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if item, ok := value[key].(string); ok && strings.TrimSpace(item) != "" {
			return strings.TrimSpace(item)
		}
	}
	return ""
}

func antigravityFunctionCallItem(functionCall antigravityFunctionCall, status, arguments string) map[string]any {
	return map[string]any{
		"id":        functionCall.ItemID,
		"type":      "function_call",
		"status":    status,
		"call_id":   functionCall.CallID,
		"name":      functionCall.Name,
		"arguments": arguments,
	}
}

type antigravitySSEBody struct {
	source     io.ReadCloser
	reader     *bufio.Reader
	queue      bytes.Buffer
	text       strings.Builder
	functions  []antigravityFunctionCall
	responseID string
	messageID  string
	model      string
	createdAt  int64
	sequence   int64
	started    bool
	terminal   bool
	// textStarted records that the assistant message item and its text part
	// have been opened downstream, so later fragments go out as deltas.
	textStarted bool
}

func newAntigravitySSEResponseBody(r io.ReadCloser, model ...string) io.ReadCloser {
	modelID := ""
	if len(model) > 0 {
		modelID = strings.TrimSpace(model[0])
	}
	return &antigravitySSEBody{
		source: r, reader: bufio.NewReader(r),
		responseID: "resp_ag_" + antigravityRandomHex(12),
		messageID:  "msg_ag_" + antigravityRandomHex(12),
		model:      modelID, createdAt: time.Now().Unix(),
	}
}
func (b *antigravitySSEBody) Close() error {
	if b == nil || b.source == nil {
		return nil
	}
	return b.source.Close()
}

func antigravityRandomHex(size int) string {
	if size <= 0 {
		size = 8
	}
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func (b *antigravitySSEBody) enqueue(eventType string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["type"] = eventType
	fields["sequence_number"] = b.sequence
	b.sequence++
	b.queue.WriteString("event: " + eventType + "\n")
	b.queue.WriteString("data: " + antigravityJSON(fields) + "\n\n")
}

func (b *antigravitySSEBody) response(status string, output []any) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id": b.responseID, "object": "response", "created_at": b.createdAt,
		"status": status, "model": b.model, "output": output,
	}
}

func (b *antigravitySSEBody) enqueueStart() {
	if b.started {
		return
	}
	b.started = true
	b.enqueue("response.created", map[string]any{"response": b.response("in_progress", nil)})
	b.enqueue("response.in_progress", map[string]any{"response": b.response("in_progress", nil)})
}

func (b *antigravitySSEBody) enqueueFailure(code, message string, statusCode ...int) {
	if b.terminal {
		return
	}
	b.enqueueStart()
	b.terminal = true
	errorValue := map[string]any{"code": code, "message": message}
	if len(statusCode) > 0 && statusCode[0] >= 400 && statusCode[0] <= 599 {
		errorValue["status_code"] = statusCode[0]
	}
	response := b.response("failed", nil)
	response["error"] = errorValue
	b.enqueue("response.failed", map[string]any{"response": response})
}

// openTextItem emits the assistant message item and its text part once, so
// text can then stream as incremental deltas.
func (b *antigravitySSEBody) openTextItem() {
	if b.textStarted {
		return
	}
	b.enqueueStart()
	b.textStarted = true
	b.enqueue("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id": b.messageID, "type": "message", "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})
	b.enqueue("response.content_part.added", map[string]any{
		"item_id": b.messageID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// emitTextDelta forwards one upstream text fragment downstream immediately.
// Cloud Code streams incrementally; holding the text until the finish reason
// made time-to-first-token equal the whole generation time.
func (b *antigravitySSEBody) emitTextDelta(fragment string) {
	if fragment == "" || b.terminal {
		return
	}
	b.openTextItem()
	b.text.WriteString(fragment)
	b.enqueue("response.output_text.delta", map[string]any{
		"item_id": b.messageID, "output_index": 0, "content_index": 0, "delta": fragment,
	})
}

func (b *antigravitySSEBody) enqueueSuccess(status string, usage map[string]any) {
	if b.terminal {
		return
	}
	b.enqueueStart()
	text := b.text.String()
	content := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	message := map[string]any{
		"id": b.messageID, "type": "message", "status": "completed",
		"role": "assistant", "content": []any{content},
	}
	output := make([]any, 0, 1+len(b.functions))
	if b.textStarted || len(b.functions) == 0 {
		// A response with neither text nor tool calls still needs one empty
		// message item so the Responses lifecycle stays well-formed.
		b.openTextItem()
		b.enqueue("response.output_text.done", map[string]any{
			"item_id": b.messageID, "output_index": 0, "content_index": 0, "text": text,
		})
		b.enqueue("response.content_part.done", map[string]any{
			"item_id": b.messageID, "output_index": 0, "content_index": 0, "part": content,
		})
		b.enqueue("response.output_item.done", map[string]any{"output_index": 0, "item": message})
		output = append(output, message)
	}
	b.terminal = true
	functionOutputStart := len(output)
	for index, functionCall := range b.functions {
		outputIndex := functionOutputStart + index
		added := antigravityFunctionCallItem(functionCall, "in_progress", "")
		b.enqueue("response.output_item.added", map[string]any{
			"output_index": outputIndex,
			"item":         added,
		})
		if functionCall.Arguments != "" {
			b.enqueue("response.function_call_arguments.delta", map[string]any{
				"output_index": outputIndex,
				"item_id":      functionCall.ItemID,
				"call_id":      functionCall.CallID,
				"delta":        functionCall.Arguments,
			})
		}
		b.enqueue("response.function_call_arguments.done", map[string]any{
			"output_index": outputIndex,
			"item_id":      functionCall.ItemID,
			"call_id":      functionCall.CallID,
			"arguments":    functionCall.Arguments,
		})
		completed := antigravityFunctionCallItem(functionCall, "completed", functionCall.Arguments)
		b.enqueue("response.output_item.done", map[string]any{"output_index": outputIndex, "item": completed})
		output = append(output, completed)
	}
	response := b.response(status, output)
	response["output_text"] = text
	if usage != nil {
		response["usage"] = usage
	}
	eventType := "response.completed"
	if status == "incomplete" {
		eventType = "response.incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	b.enqueue(eventType, map[string]any{"response": response})
}

func (b *antigravitySSEBody) Read(p []byte) (int, error) {
	for b.queue.Len() == 0 {
		if !b.started {
			b.enqueueStart()
			break
		}
		if b.terminal {
			return 0, io.EOF
		}
		data, err := readSSEDataLine(b.reader)
		if err != nil {
			b.enqueueFailure(ErrorCodeUpstreamStreamBreak, "antigravity stream ended before completion")
			continue
		}
		if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			b.enqueueFailure(ErrorCodeUpstreamStreamBreak, "antigravity stream ended before completion")
			continue
		}
		var env map[string]any
		if err := json.Unmarshal(data, &env); err != nil {
			b.enqueueFailure(ErrorCodeUpstreamError, "antigravity upstream emitted invalid SSE JSON")
			continue
		}
		if v, ok := env["response"].(map[string]any); ok {
			env = v
		}
		if _, ok := env["error"]; ok {
			b.enqueueFailure(ErrorCodeUpstreamError, "antigravity upstream error")
			continue
		}
		finishReason := geminiFinishReason(env)
		finishStatus := geminiFinishStatus(finishReason)
		blocked := geminiBlocked(env)
		if blocked || finishStatus == "failed" {
			message := "antigravity response blocked by safety policy"
			if blocked {
				b.enqueueFailure("content_filter", message, http.StatusBadRequest)
				continue
			}
			if finishReason != "" {
				message = "antigravity response terminated with " + finishReason
			}
			b.enqueueFailure(ErrorCodeUpstreamError, message)
			continue
		}
		if lenGeminiCandidates(env) == 0 {
			continue
		}
		if geminiHasUnsupportedOutput(env) {
			b.enqueueFailure("unsupported_output", "antigravity upstream returned an unsupported non-text output", http.StatusBadRequest)
			continue
		}
		// Text streams through as deltas. A safety or error frame that arrives
		// in the same upstream frame as text is checked above, before any of
		// that frame's text is forwarded; a rejection that arrives in a later
		// frame terminates the stream with response.failed instead.
		fragment := extractGeminiText(env)
		calls := extractGeminiFunctionCalls(env)
		if b.text.Len()+len(fragment)+antigravityFunctionCallsSize(b.functions)+antigravityFunctionCallsSize(calls) > antigravityResponseBodyLimit {
			b.enqueueFailure(ErrorCodeUpstreamError, "antigravity streamed response exceeded the safe size limit")
			continue
		}
		b.emitTextDelta(fragment)
		b.addFunctionCalls(calls)
		if finishReason != "" {
			var usage map[string]any
			if metadata, ok := env["usageMetadata"].(map[string]any); ok {
				usage = antigravityUsage(metadata)
			}
			b.enqueueSuccess(finishStatus, usage)
		}
	}
	return b.queue.Read(p)
}

func (b *antigravitySSEBody) addFunctionCalls(calls []antigravityFunctionCall) {
	for _, functionCall := range calls {
		matched := false
		for index := range b.functions {
			existing := &b.functions[index]
			if existing.CallID == functionCall.CallID || (existing.Name == functionCall.Name && existing.Arguments == functionCall.Arguments) {
				if functionCall.Name != "" {
					existing.Name = functionCall.Name
				}
				if functionCall.Arguments != "" {
					existing.Arguments = functionCall.Arguments
				}
				matched = true
				break
			}
		}
		if !matched {
			b.functions = append(b.functions, functionCall)
		}
	}
}

func antigravityFunctionCallsSize(calls []antigravityFunctionCall) int {
	total := 0
	for _, functionCall := range calls {
		total += len(functionCall.ItemID) + len(functionCall.CallID) + len(functionCall.Name) + len(functionCall.Arguments)
	}
	return total
}

func lenGeminiCandidates(v map[string]any) int {
	c, _ := v["candidates"].([]any)
	return len(c)
}
func geminiFinishReason(v map[string]any) string {
	c, _ := v["candidates"].([]any)
	if len(c) == 0 {
		return ""
	}
	cm, _ := c[0].(map[string]any)
	if s, _ := cm["finishReason"].(string); s != "" {
		return strings.TrimSpace(s)
	}
	if s, _ := cm["finish_reason"].(string); s != "" {
		return strings.TrimSpace(s)
	}
	return ""
}
func geminiBlocked(v map[string]any) bool {
	if pf, ok := v["promptFeedback"].(map[string]any); ok {
		if reason, _ := pf["blockReason"].(string); strings.TrimSpace(reason) != "" && !strings.EqualFold(reason, "NONE") {
			return true
		}
	}
	return strings.EqualFold(geminiFinishReason(v), "SAFETY")
}

func geminiFinishStatus(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "", "STOP":
		return "completed"
	case "MAX_TOKENS", "MAX_OUTPUT_TOKENS":
		return "incomplete"
	default:
		return "failed"
	}
}
func extractGeminiText(v map[string]any) string {
	c, _ := v["candidates"].([]any)
	if len(c) == 0 {
		return ""
	}
	cm, _ := c[0].(map[string]any)
	content, _ := cm["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var out []string
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if thought, _ := pm["thought"].(bool); thought {
			continue
		}
		if s, ok := pm["text"].(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, "")
}

func geminiHasUnsupportedOutput(v map[string]any) bool {
	candidates, _ := v["candidates"].([]any)
	for _, candidate := range candidates {
		cm, _ := candidate.(map[string]any)
		content, _ := cm["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, part := range parts {
			pm, _ := part.(map[string]any)
			hasPayload := false
			unsupported := false
			for key, item := range pm {
				switch key {
				case "text":
					if _, ok := item.(string); !ok {
						unsupported = true
					} else {
						hasPayload = true
					}
				case "functionCall", "function_call":
					if _, ok := item.(map[string]any); !ok {
						unsupported = true
					} else {
						hasPayload = true
					}
				case "thought", "thoughtSignature", "thought_signature":
					// Metadata attached to a supported text/function payload.
				default:
					unsupported = true
				}
			}
			if unsupported || (len(pm) > 0 && !hasPayload) {
				return true
			}
		}
	}
	return false
}

func antigravityUsage(usage map[string]any) map[string]any {
	inputTokens := antigravityUsageTokenCount(usage["promptTokenCount"])
	outputTokens := antigravityUsageTokenCount(usage["candidatesTokenCount"])
	reasoningTokens := antigravityUsageTokenCount(usage["thoughtsTokenCount"])
	outputTokens += reasoningTokens
	totalTokens := antigravityUsageTokenCount(usage["totalTokenCount"])
	if minimum := inputTokens + outputTokens; totalTokens < minimum {
		totalTokens = minimum
	}
	result := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  totalTokens,
	}
	if cached := usage["cachedContentTokenCount"]; cached != nil {
		result["input_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoningTokens > 0 {
		result["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoningTokens}
	}
	return result
}

func antigravityUsageTokenCount(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		count, _ := typed.Int64()
		return count
	default:
		return 0
	}
}
func antigravityJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
