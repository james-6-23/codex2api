package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const (
	probeShortCircuitDefaultWindowSeconds = 300
	probeShortCircuitDefaultMaxCacheBytes = 256 * 1024
	probeShortCircuitDefaultMaxEntries    = 256
	probeShortCircuitEndpoint             = "local_probe_short_circuit"
	probeObservedEndpoint                 = "local_probe_observed"
	probeShortCircuitMessage              = "Probe acknowledged. Too many repeated probes; returning a local success response."
)

var probeShortCircuitState = &probeShortCircuitTracker{
	lastSeen: map[string]time.Time{},
	replies:  map[string]probeCachedResponse{},
}

type probeShortCircuitConfig struct {
	Enabled       bool
	LogOnly       bool
	Window        time.Duration
	MaxCacheBytes int
	MaxEntries    int
}

type probeShortCircuitTracker struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	replies  map[string]probeCachedResponse
}

type probeShortCircuitDecision struct {
	Probe     bool
	Repeated  bool
	Key       string
	Signature string
	BodyHash  string
	Text      string
	Cached    *probeCachedResponse
}

type probeCachedResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
	CreatedAt   time.Time
}

type probeResponseCapture struct {
	key          string
	writer       *probeCaptureWriter
	maxCacheBytes int
	maxEntries    int
}

type probeCaptureWriter struct {
	gin.ResponseWriter
	buf   bytes.Buffer
	limit int
	full  bool
}

func loadProbeShortCircuitConfig() probeShortCircuitConfig {
	return probeShortCircuitConfig{
		Enabled:       envBool("CODEX_PROBE_SHORT_CIRCUIT_ENABLED", false),
		LogOnly:       envBool("CODEX_PROBE_SHORT_CIRCUIT_LOG_ONLY", false),
		Window:        time.Duration(envInt("CODEX_PROBE_SHORT_CIRCUIT_WINDOW_SECONDS", probeShortCircuitDefaultWindowSeconds, 30, 3600)) * time.Second,
		MaxCacheBytes: envInt("CODEX_PROBE_SHORT_CIRCUIT_MAX_CACHE_BYTES", probeShortCircuitDefaultMaxCacheBytes, 1024, 2*1024*1024),
		MaxEntries:    envInt("CODEX_PROBE_SHORT_CIRCUIT_MAX_ENTRIES", probeShortCircuitDefaultMaxEntries, 16, 10000),
	}
}

func (h *Handler) prepareProbeShortCircuit(c *gin.Context, rawBody []byte, endpoint string, model string, stream bool, responseKind string) (*probeResponseCapture, bool) {
	cfg := loadProbeShortCircuitConfig()
	if !cfg.Enabled {
		return nil, false
	}
	decision := classifyProbeShortCircuit(c, rawBody, endpoint, model, stream, responseKind, cfg.Window, cfg.MaxEntries)
	if cfg.LogOnly {
		if decision.Probe {
			h.logLocalProbeObserved(c, endpoint, model, stream, responseKind, decision)
		}
		return nil, false
	}
	if !decision.Probe || !decision.Repeated {
		if !decision.Probe {
			return nil, false
		}
		writer := &probeCaptureWriter{ResponseWriter: c.Writer, limit: cfg.MaxCacheBytes}
		c.Writer = writer
		return &probeResponseCapture{key: decision.Key, writer: writer, maxCacheBytes: cfg.MaxCacheBytes, maxEntries: cfg.MaxEntries}, false
	}
	h.logLocalProbeShortCircuit(c, endpoint, model, stream, responseKind, decision)
	if decision.Cached != nil {
		writeCachedProbeResponse(c, *decision.Cached)
		return nil, true
	}
	if responseKind == "chat" {
		writeLocalProbeChatCompletion(c, model, stream)
		return nil, true
	}
	writeLocalProbeResponses(c, model, stream)
	return nil, true
}

func classifyProbeShortCircuit(c *gin.Context, rawBody []byte, endpoint string, model string, stream bool, responseKind string, window time.Duration, maxEntries int) probeShortCircuitDecision {
	text := requestProbeCandidateText(rawBody, endpoint)
	signature, ok := probeSignature(text)
	if !ok {
		return probeShortCircuitDecision{Probe: false, Text: text}
	}
	bodyHash := probeRequestBodyHash(rawBody)
	key := probeCacheKey(c, endpoint, model, stream, responseKind, signature, bodyHash)
	repeated, cached := probeShortCircuitState.markAndGet(key, window, maxEntries)
	return probeShortCircuitDecision{Probe: true, Repeated: repeated, Key: key, Signature: signature, BodyHash: bodyHash, Text: text, Cached: cached}
}

func probeRequestBodyHash(rawBody []byte) string {
	sum := sha256.Sum256(rawBody)
	return hex.EncodeToString(sum[:])
}

func probeCacheKey(c *gin.Context, endpoint string, model string, stream bool, responseKind string, signature string, bodyHash string) string {
	source := probeSourceKey(c)
	return strings.Join([]string{
		source,
		strings.TrimSpace(endpoint),
		strings.TrimSpace(model),
		fmt.Sprintf("stream:%t", stream),
		strings.TrimSpace(responseKind),
		signature,
		bodyHash,
	}, "|")
}

func (t *probeShortCircuitTracker) markAndGet(key string, window time.Duration, maxEntries int) (bool, *probeCachedResponse) {
	if t == nil || key == "" {
		return false, nil
	}
	if window <= 0 {
		window = time.Duration(probeShortCircuitDefaultWindowSeconds) * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = probeShortCircuitDefaultMaxEntries
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.lastSeen[key]
	repeated := ok && now.Sub(last) < window
	t.lastSeen[key] = now
	var cached *probeCachedResponse
	if repeated {
		if reply, ok := t.replies[key]; ok && len(reply.Body) > 0 && now.Sub(reply.CreatedAt) <= window {
			copyBody := append([]byte(nil), reply.Body...)
			cached = &probeCachedResponse{StatusCode: reply.StatusCode, ContentType: reply.ContentType, Body: copyBody, CreatedAt: reply.CreatedAt}
		}
	}
	for existing, seenAt := range t.lastSeen {
		if now.Sub(seenAt) > 2*window {
			delete(t.lastSeen, existing)
			delete(t.replies, existing)
		}
	}
	t.enforceLimitLocked(maxEntries)
	return repeated, cached
}

func (t *probeShortCircuitTracker) storeReply(key string, reply probeCachedResponse, maxEntries int) {
	if t == nil || key == "" || len(reply.Body) == 0 {
		return
	}
	if maxEntries <= 0 {
		maxEntries = probeShortCircuitDefaultMaxEntries
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.replies == nil {
		t.replies = map[string]probeCachedResponse{}
	}
	reply.Body = append([]byte(nil), reply.Body...)
	if reply.CreatedAt.IsZero() {
		reply.CreatedAt = time.Now()
	}
	t.replies[key] = reply
	t.enforceLimitLocked(maxEntries)
}

func (t *probeShortCircuitTracker) enforceLimitLocked(maxEntries int) {
	if t == nil || maxEntries <= 0 {
		return
	}
	for len(t.lastSeen) > maxEntries {
		var oldestKey string
		var oldestTime time.Time
		for key, seenAt := range t.lastSeen {
			if oldestKey == "" || seenAt.Before(oldestTime) {
				oldestKey = key
				oldestTime = seenAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(t.lastSeen, oldestKey)
		delete(t.replies, oldestKey)
	}
	for key := range t.replies {
		if _, ok := t.lastSeen[key]; !ok {
			delete(t.replies, key)
		}
	}
}

func (c *probeResponseCapture) finish() {
	if c == nil || c.writer == nil || c.key == "" {
		return
	}
	if c.writer.full || c.writer.buf.Len() == 0 {
		return
	}
	status := c.writer.Status()
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		return
	}
	contentType := strings.TrimSpace(c.writer.Header().Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	probeShortCircuitState.storeReply(c.key, probeCachedResponse{
		StatusCode:  status,
		ContentType: contentType,
		Body:        c.writer.buf.Bytes(),
		CreatedAt:   time.Now(),
	}, c.maxEntries)
}

func (w *probeCaptureWriter) Write(data []byte) (int, error) {
	w.capture(data)
	return w.ResponseWriter.Write(data)
}

func (w *probeCaptureWriter) WriteString(data string) (int, error) {
	w.capture([]byte(data))
	return w.ResponseWriter.WriteString(data)
}

func (w *probeCaptureWriter) capture(data []byte) {
	if w == nil || w.full || len(data) == 0 {
		return
	}
	if w.limit <= 0 {
		w.limit = probeShortCircuitDefaultMaxCacheBytes
	}
	if w.buf.Len()+len(data) > w.limit {
		w.full = true
		w.buf.Reset()
		return
	}
	_, _ = w.buf.Write(data)
}

func probeSourceKey(c *gin.Context) string {
	if id := requestAPIKeyID(c); id > 0 {
		return fmt.Sprintf("key:%d", id)
	}
	if c != nil {
		if ip := strings.TrimSpace(c.ClientIP()); ip != "" {
			return "ip:" + ip
		}
	}
	return "unknown"
}

func requestProbeCandidateText(rawBody []byte, endpoint string) string {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err == nil {
		switch endpoint {
		case "/v1/chat/completions":
			if text := lastChatUserText(body["messages"]); text != "" {
				return text
			}
		default:
			if text := lastResponsesUserText(body["input"]); text != "" {
				return text
			}
		}
	}
	return promptfilter.ExtractText(rawBody, endpoint, 4096)
}

func lastChatUserText(messages any) string {
	list, ok := messages.([]any)
	if !ok {
		return ""
	}
	for i := len(list) - 1; i >= 0; i-- {
		msg, ok := list[i].(map[string]any)
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(msg["role"])), "user") {
			continue
		}
		if text := contentText(msg["content"]); text != "" {
			return text
		}
	}
	return ""
}

func lastResponsesUserText(input any) string {
	if text, ok := input.(string); ok {
		return strings.TrimSpace(text)
	}
	list, ok := input.([]any)
	if !ok {
		return ""
	}
	for i := len(list) - 1; i >= 0; i-- {
		item, ok := list[i].(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(fmt.Sprint(item["role"]))
		if role != "" && !strings.EqualFold(role, "user") {
			continue
		}
		if text := contentText(item["content"]); text != "" {
			return text
		}
		if text := contentText(item["text"]); text != "" {
			return text
		}
	}
	return ""
}

func contentText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		var parts []string
		for _, part := range typed {
			switch item := part.(type) {
			case string:
				if text := strings.TrimSpace(item); text != "" {
					parts = append(parts, text)
				}
			case map[string]any:
				for _, key := range []string{"text", "input_text", "output_text"} {
					if text := contentText(item[key]); text != "" {
						parts = append(parts, text)
						break
					}
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case map[string]any:
		for _, key := range []string{"text", "input_text", "output_text"} {
			if text := contentText(typed[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

/*
func probeSignature(text string) (string, bool) {
	normalized := normalizeProbeText(text)
	if normalized == "" || len([]rune(normalized)) > 160 {
		return "", false
	}
	exact := map[string]string{
		"hi":                              "hello",
		"hello":                           "hello",
		"ping":                            "ping",
		"pong":                            "ping",
		"test":                            "test",
		"ok":                              "ok",
		"hello world":                     "hello_world",
		"count to seven":                  "count_to_seven",
		"count to 7":                      "count_to_seven",
		"whats the opposite of dark":      "opposite_dark",
		"what is the opposite of dark":    "opposite_dark",
		"2 乘 2 等于几":                       "two_times_two",
		"2乘2等于几":                         "two_times_two",
		"2*2等于几":                         "two_times_two",
		"2 x 2 equals what":               "two_times_two",
		"what is 2+2":                     "two_plus_two",
		"what is two plus two":            "two_plus_two",
		"call the probe_ping function with ok=true to acknowledge readiness you must use the tool": "probe_ping",
	}
	if signature, ok := exact[normalized]; ok {
		return signature, true
	}
	if strings.Contains(normalized, "probe_ping") && strings.Contains(normalized, "ok=true") {
		return "probe_ping", true
	}
	if len([]rune(normalized)) <= 80 && strings.Contains(normalized, "acknowledge readiness") && strings.Contains(normalized, "probe") {
		return "probe_readiness", true
	}
	return "", false
}

func normalizeProbeText(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	text = strings.ReplaceAll(text, "’", "'")
	text = strings.ReplaceAll(text, "？", "?")
	text = strings.ReplaceAll(text, "＝", "=")
	text = strings.ReplaceAll(text, "，", ",")
	text = strings.TrimFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'`.,!?;:。！？；：", r)
	})
	var b strings.Builder
	space := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteRune(' ')
				space = true
			}
			continue
		}
		if r == '\'' {
			continue
		}
		b.WriteRune(r)
		space = false
	}
	return strings.TrimSpace(b.String())
}

*/

func probeSignature(text string) (string, bool) {
	normalized := normalizeProbeText(text)
	if normalized == "" || len([]rune(normalized)) > 160 {
		return "", false
	}
	exact := map[string]string{
		"hi":                                                                                   "hello",
		"hello":                                                                                "hello",
		"ping":                                                                                 "ping",
		"pong":                                                                                 "ping",
		"test":                                                                                 "test",
		"ok":                                                                                   "ok",
		"hello world":                                                                          "hello_world",
		"count to seven":                                                                       "count_to_seven",
		"count to 7":                                                                           "count_to_seven",
		"whats the opposite of dark":                                                           "opposite_dark",
		"what is the opposite of dark":                                                         "opposite_dark",
		"2 \u4e58 2 \u7b49\u4e8e\u51e0":                                                       "two_times_two",
		"2\u4e582\u7b49\u4e8e\u51e0":                                                          "two_times_two",
		"2*2\u7b49\u4e8e\u51e0":                                                               "two_times_two",
		"2\u00d72\u7b49\u4e8e\u51e0":                                                          "two_times_two",
		"2 x 2 equals what":                                                                    "two_times_two",
		"what is 2+2":                                                                          "two_plus_two",
		"what is two plus two":                                                                 "two_plus_two",
		"call the probe_ping function with ok=true to acknowledge readiness you must use the tool": "probe_ping",
	}
	if signature, ok := exact[normalized]; ok {
		return signature, true
	}
	if strings.Contains(normalized, "probe_ping") && strings.Contains(normalized, "ok=true") {
		return "probe_ping", true
	}
	if len([]rune(normalized)) <= 80 && strings.Contains(normalized, "acknowledge readiness") && strings.Contains(normalized, "probe") {
		return "probe_readiness", true
	}
	return "", false
}

func normalizeProbeText(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	text = strings.ReplaceAll(text, "\u2019", "'")
	text = strings.ReplaceAll(text, "\uff1f", "?")
	text = strings.ReplaceAll(text, "\uff1d", "=")
	text = strings.ReplaceAll(text, "\uff0c", ",")
	text = strings.TrimFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'`.,!?;:\u3002\uff01\uff1f\uff1b\uff1a\uff0c", r)
	})
	var b strings.Builder
	space := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteRune(' ')
				space = true
			}
			continue
		}
		if r == '\'' {
			continue
		}
		b.WriteRune(r)
		space = false
	}
	return strings.TrimSpace(b.String())
}

func localProbeID(prefix string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", prefix, time.Now().UnixNano())))
	return prefix + "_" + hex.EncodeToString(sum[:])[:24]
}

func writeLocalProbeResponses(c *gin.Context, model string, stream bool) {
	now := time.Now().Unix()
	id := localProbeID("resp_probe")
	msgID := localProbeID("msg_probe")
	resp := api.ResponsesAPIResponse{
		ID:        id,
		Object:    "response",
		CreatedAt: now,
		Status:    "completed",
		Model:     model,
		Output: []api.OutputItem{{
			Type:   "message",
			ID:     msgID,
			Status: "completed",
			Role:   "assistant",
			Content: []api.ContentPart{{
				Type: "output_text",
				Text: probeShortCircuitMessage,
			}},
		}},
		Usage: &api.UsageInfo{},
	}
	if !stream {
		c.JSON(http.StatusOK, resp)
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	writeSSEJSON(c, gin.H{"type": "response.created", "response": resp})
	writeSSEJSON(c, gin.H{"type": "response.output_text.delta", "delta": probeShortCircuitMessage, "item_id": msgID, "output_index": 0, "content_index": 0})
	writeSSEJSON(c, gin.H{"type": "response.completed", "response": resp})
}

func writeCachedProbeResponse(c *gin.Context, cached probeCachedResponse) {
	status := cached.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	contentType := strings.TrimSpace(cached.ContentType)
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(status, contentType, cached.Body)
}

func writeLocalProbeChatCompletion(c *gin.Context, model string, stream bool) {
	now := time.Now().Unix()
	id := localProbeID("chatcmpl_probe")
	if !stream {
		c.JSON(http.StatusOK, api.ChatCompletionResponse{
			ID:      id,
			Object:  "chat.completion",
			Created: now,
			Model:   model,
			Choices: []api.ChatCompletionChoice{{
				Index:        0,
				Message:      &api.Message{Role: "assistant", Content: probeShortCircuitMessage},
				FinishReason: "stop",
			}},
			Usage: &api.UsageInfo{},
		})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	writeSSEJSON(c, api.StreamChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: now,
		Model:   model,
		Choices: []api.ChatCompletionChoice{{Index: 0, Delta: &api.Message{Role: "assistant"}}},
	})
	writeSSEJSON(c, api.StreamChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: now,
		Model:   model,
		Choices: []api.ChatCompletionChoice{{Index: 0, Delta: &api.Message{Content: probeShortCircuitMessage}}},
	})
	writeSSEJSON(c, api.StreamChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: now,
		Model:   model,
		Choices: []api.ChatCompletionChoice{{Index: 0, Delta: &api.Message{}, FinishReason: "stop"}},
		Usage:   &api.UsageInfo{},
	})
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeSSEJSON(c *gin.Context, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = c.Writer.WriteString("data: ")
	_, _ = c.Writer.Write(data)
	_, _ = c.Writer.WriteString("\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *Handler) logLocalProbeShortCircuit(c *gin.Context, endpoint string, model string, stream bool, responseKind string, decision probeShortCircuitDecision) {
	input := &database.UsageLogInput{
		AccountID:          0,
		Endpoint:           endpoint,
		Model:              model,
		EffectiveModel:     model,
		StatusCode:         http.StatusOK,
		DurationMs:         0,
		InboundEndpoint:    endpoint,
		UpstreamEndpoint:   probeShortCircuitEndpoint,
		Stream:             stream,
		UpstreamErrorKind:  "local_probe_short_circuit",
		ErrorMessage:       "repeated probe short-circuited: " + decision.Signature,
		PromptTokens:       0,
		CompletionTokens:   0,
		TotalTokens:        0,
		InputTokens:        0,
		OutputTokens:       0,
		ReasoningTokens:    0,
		FirstTokenMs:       0,
		ReasoningEffort:    "",
		BillingServiceTier: "",
	}
	if responseKind == "responses_compact" {
		input.Compact = true
	}
	h.logUsageForRequest(c, input)
}

func (h *Handler) logLocalProbeObserved(c *gin.Context, endpoint string, model string, stream bool, responseKind string, decision probeShortCircuitDecision) {
	if h == nil || h.db == nil {
		return
	}
	streamValue := "false"
	if stream {
		streamValue = "true"
	}
	input := &database.PromptFilterLogInput{
		Source:          probeObservedEndpoint,
		Endpoint:        endpoint,
		Model:           model,
		Action:          promptfilter.ActionAllow,
		Mode:            "log_only",
		Score:           0,
		Threshold:       0,
		MatchedPatterns: promptfilter.MatchesJSON([]promptfilter.Match{{Name: decision.Signature, Weight: 0, Category: "probe"}}),
		TextPreview: fmt.Sprintf(
			"probe observed: signature=%s repeated=%t body_hash=%s response_kind=%s stream=%s",
			decision.Signature,
			decision.Repeated,
			shortProbeHash(decision.BodyHash),
			responseKind,
			streamValue,
		),
		ClientIP: c.ClientIP(),
	}
	populatePromptFilterAPIKeyMeta(c, input)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.db.InsertPromptFilterLog(ctx, input)
}

func shortProbeHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
