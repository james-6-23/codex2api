package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	semanticReviewDefaultModel          = "gpt-5.4-mini"
	semanticReviewDefaultTimeoutMS      = 2500
	semanticReviewDefaultMaxChars       = 20000
	semanticReviewDefaultCacheTTL       = 30 * time.Minute
	semanticReviewDefaultMaxConcurrency = int64(4)

	SemanticReviewFailurePolicyBlock = "block"
	SemanticReviewFailurePolicyAllow = "allow"
	SemanticReviewFailurePolicyWarn  = "warn"
)

var (
	semanticReviewHTTPClient = http.DefaultClient
	semanticReviewInFlight   int64
	semanticReviewCacheState = &semanticReviewCache{
		items: map[string]semanticReviewCacheEntry{},
	}
)

type semanticReviewConfig struct {
	Enabled             bool
	DisagreementEnabled bool
	Mode                string
	APIKey              string
	BaseURL             string
	Model               string
	Timeout             time.Duration
	MaxChars            int
	CacheTTL            time.Duration
	MaxConcurrency      int64
	LogAllows           bool
	FailOpen            bool
	FailurePolicy       string
	Endpoints           map[string]bool
}

type semanticReviewResult struct {
	Block      bool    `json:"block"`
	Confidence float64 `json:"confidence"`
	Category   string  `json:"category"`
	Reason     string  `json:"reason"`
	Model      string  `json:"-"`
	Cached     bool    `json:"-"`
}

type SemanticReviewConnectionTestConfig struct {
	APIKey       string
	BaseURL      string
	Model        string
	Timeout      time.Duration
	Endpoint     string
	RequestModel string
	Text         string
}

type SemanticReviewConnectionTestResult struct {
	OK            bool    `json:"ok"`
	Configured    bool    `json:"configured"`
	BaseURL       string  `json:"base_url"`
	Model         string  `json:"model"`
	ResponseModel string  `json:"response_model,omitempty"`
	Endpoint      string  `json:"endpoint"`
	RequestModel  string  `json:"request_model"`
	LatencyMS     int64   `json:"latency_ms"`
	Block         bool    `json:"block"`
	Confidence    float64 `json:"confidence"`
	Category      string  `json:"category"`
	Reason        string  `json:"reason"`
	Error         string  `json:"error,omitempty"`
}

type semanticReviewRequest struct {
	Model       string                  `json:"model"`
	Messages    []semanticReviewMessage `json:"messages"`
	Temperature float64                 `json:"temperature,omitempty"`
	MaxTokens   int                     `json:"max_tokens,omitempty"`
}

type semanticReviewMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type semanticReviewResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type semanticReviewCache struct {
	mu    sync.Mutex
	items map[string]semanticReviewCacheEntry
}

type semanticReviewCacheEntry struct {
	result  semanticReviewResult
	expires time.Time
}

func loadSemanticReviewConfig() semanticReviewConfig {
	cfg := semanticReviewConfig{
		Enabled:             envBool("CODEX_SEMANTIC_REVIEW_ENABLED", false),
		DisagreementEnabled: envBool("CODEX_SEMANTIC_REVIEW_DISAGREEMENT_ENABLED", true),
		Mode:                normalizeSemanticReviewMode(os.Getenv("CODEX_SEMANTIC_REVIEW_MODE")),
		APIKey:              strings.TrimSpace(os.Getenv("CODEX_SEMANTIC_REVIEW_API_KEY")),
		BaseURL:             strings.TrimRight(strings.TrimSpace(os.Getenv("CODEX_SEMANTIC_REVIEW_BASE_URL")), "/"),
		Model:               strings.TrimSpace(os.Getenv("CODEX_SEMANTIC_REVIEW_MODEL")),
		Timeout:             time.Duration(envInt("CODEX_SEMANTIC_REVIEW_TIMEOUT_MS", semanticReviewDefaultTimeoutMS, 100, 30000)) * time.Millisecond,
		MaxChars:            envInt("CODEX_SEMANTIC_REVIEW_MAX_CHARS", semanticReviewDefaultMaxChars, 1000, 200000),
		CacheTTL:            time.Duration(envInt("CODEX_SEMANTIC_REVIEW_CACHE_TTL_SECONDS", int(semanticReviewDefaultCacheTTL/time.Second), 0, 86400)) * time.Second,
		MaxConcurrency:      int64(envInt("CODEX_SEMANTIC_REVIEW_MAX_CONCURRENCY", int(semanticReviewDefaultMaxConcurrency), 1, 100)),
		LogAllows:           envBool("CODEX_SEMANTIC_REVIEW_LOG_ALLOWS", true),
		FailOpen:            envBool("CODEX_SEMANTIC_REVIEW_FAIL_OPEN", true),
		FailurePolicy:       NormalizeSemanticReviewFailurePolicy(os.Getenv("CODEX_SEMANTIC_REVIEW_FAILURE_POLICY")),
		Endpoints:           semanticReviewEndpoints(os.Getenv("CODEX_SEMANTIC_REVIEW_ENDPOINTS")),
	}
	if cfg.Mode == "" {
		cfg.Mode = promptfilter.ModeBlock
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.Model == "" {
		cfg.Model = semanticReviewDefaultModel
	}
	return cfg
}

func NormalizeSemanticReviewFailurePolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SemanticReviewFailurePolicyAllow:
		return SemanticReviewFailurePolicyAllow
	case SemanticReviewFailurePolicyWarn:
		return SemanticReviewFailurePolicyWarn
	default:
		return SemanticReviewFailurePolicyBlock
	}
}

func TestSemanticReviewConnection(ctx context.Context, in SemanticReviewConnectionTestConfig) SemanticReviewConnectionTestResult {
	baseURL := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	model := strings.TrimSpace(in.Model)
	endpoint := strings.TrimSpace(in.Endpoint)
	if endpoint == "" {
		endpoint = "/v1/responses"
	}
	requestModel := strings.TrimSpace(in.RequestModel)
	if requestModel == "" {
		requestModel = "gpt-5.4"
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		text = "Connectivity check for a defensive cybersecurity policy classifier. This request is benign and should be allowed."
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = time.Duration(semanticReviewDefaultTimeoutMS) * time.Millisecond
	}
	cfg := semanticReviewConfig{
		Enabled:        true,
		APIKey:         strings.TrimSpace(in.APIKey),
		BaseURL:        baseURL,
		Model:          model,
		Timeout:        timeout,
		MaxChars:       semanticReviewDefaultMaxChars,
		MaxConcurrency: 1,
	}
	result := SemanticReviewConnectionTestResult{
		Configured:   cfg.APIKey != "" && cfg.BaseURL != "" && cfg.Model != "",
		BaseURL:      cfg.BaseURL,
		Model:        cfg.Model,
		Endpoint:     endpoint,
		RequestModel: requestModel,
	}
	if !result.Configured {
		result.Error = "semantic review base_url, model, or api key is not configured"
		return result
	}
	start := time.Now()
	review, err := callSemanticReviewModel(ctx, cfg, endpoint, requestModel, text)
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.OK = true
	result.ResponseModel = review.Model
	result.Block = review.Block
	result.Confidence = review.Confidence
	result.Category = review.Category
	result.Reason = review.Reason
	return result
}

func semanticReviewEndpoints(raw string) map[string]bool {
	raw = strings.TrimSpace(raw)
	defaults := []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"}
	if raw == "" {
		raw = strings.Join(defaults, ",")
	}
	out := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out[item] = true
	}
	return out
}

func normalizeSemanticReviewMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case promptfilter.ModeMonitor:
		return promptfilter.ModeMonitor
	case promptfilter.ModeWarn:
		return promptfilter.ModeWarn
	case promptfilter.ModeBlock:
		return promptfilter.ModeBlock
	default:
		return ""
	}
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envInt(name string, fallback, min, max int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	if parsed < min {
		return min
	}
	if parsed > max {
		return max
	}
	return parsed
}

func clampSemanticReviewInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func (h *Handler) loadSemanticReviewDisagreementConfig(ctx context.Context) semanticReviewConfig {
	cfg := loadSemanticReviewConfig()
	if h == nil || h.db == nil {
		return cfg
	}
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		return cfg
	}
	cfg.DisagreementEnabled = settings.PromptFilterSemanticReviewEnabled
	if key := strings.TrimSpace(settings.PromptFilterSemanticReviewAPIKey); key != "" {
		cfg.APIKey = key
	}
	if baseURL := strings.TrimRight(strings.TrimSpace(settings.PromptFilterSemanticReviewBaseURL), "/"); baseURL != "" {
		cfg.BaseURL = baseURL
	}
	if model := strings.TrimSpace(settings.PromptFilterSemanticReviewModel); model != "" {
		cfg.Model = model
	}
	if settings.PromptFilterSemanticReviewTimeoutMS > 0 {
		cfg.Timeout = time.Duration(clampSemanticReviewInt(settings.PromptFilterSemanticReviewTimeoutMS, 100, 30000)) * time.Millisecond
	}
	if settings.PromptFilterSemanticReviewMaxConcurrency > 0 {
		cfg.MaxConcurrency = int64(clampSemanticReviewInt(settings.PromptFilterSemanticReviewMaxConcurrency, 1, 100))
	}
	cfg.FailurePolicy = NormalizeSemanticReviewFailurePolicy(settings.PromptFilterSemanticReviewFailurePolicy)
	return cfg
}

func (cfg semanticReviewConfig) readyFor(endpoint string) bool {
	if !cfg.Enabled || cfg.APIKey == "" || cfg.BaseURL == "" || cfg.Model == "" {
		return false
	}
	return cfg.Endpoints[endpoint]
}

func (cfg semanticReviewConfig) readyForDisagreement(endpoint string) bool {
	if !cfg.DisagreementEnabled || cfg.APIKey == "" || cfg.BaseURL == "" || cfg.Model == "" {
		return false
	}
	return cfg.Endpoints[endpoint]
}

func (h *Handler) inspectSemanticReviewOpenAI(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	return h.inspectSemanticReviewText(c, promptfilter.ExtractText(rawBody, endpoint, promptfilter.DefaultMaxTextLength), endpoint, model, func() {
		sendPromptCyberPolicyBlockedOpenAI(c)
	})
}

func (h *Handler) inspectSemanticReviewTextOpenAI(c *gin.Context, text string, endpoint string, model string) bool {
	return h.inspectSemanticReviewText(c, text, endpoint, model, func() {
		sendPromptCyberPolicyBlockedOpenAI(c)
	})
}

func (h *Handler) inspectSemanticReviewAnthropic(c *gin.Context, rawBody []byte, endpoint string, model string) bool {
	return h.inspectSemanticReviewText(c, promptfilter.ExtractText(rawBody, endpoint, promptfilter.DefaultMaxTextLength), endpoint, model, func() {
		sendAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request blocked by semantic safety review")
	})
}

func (h *Handler) inspectSemanticReviewOpenAIForWebSocket(c *gin.Context, conn *websocket.Conn, rawBody []byte, endpoint string, model string) bool {
	return h.inspectSemanticReviewText(c, promptfilter.ExtractText(rawBody, endpoint, promptfilter.DefaultMaxTextLength), endpoint, model, func() {
		_ = writeResponsesWSError(conn, promptCyberPolicyError())
	})
}

func (h *Handler) inspectSemanticReviewText(c *gin.Context, text string, endpoint string, model string, writeBlock func()) bool {
	cfg := loadSemanticReviewConfig()
	if !cfg.readyFor(endpoint) {
		return false
	}
	text = prepareSemanticReviewText(text, cfg.MaxChars)
	if strings.TrimSpace(text) == "" {
		return false
	}

	result, reviewErr := runSemanticReview(c.Request.Context(), cfg, endpoint, model, text)
	action := promptfilter.ActionAllow
	reason := result.Reason
	if reviewErr != nil {
		reason = "semantic review failed; allowed by policy: " + reviewErr.Error()
		if !cfg.FailOpen {
			action = promptfilter.ActionBlock
			reason = "semantic review failed: " + reviewErr.Error()
		}
	} else if result.Block {
		switch cfg.Mode {
		case promptfilter.ModeBlock:
			action = promptfilter.ActionBlock
			if reason == "" {
				reason = "semantic review flagged request"
			}
		case promptfilter.ModeWarn:
			action = promptfilter.ActionWarn
			if reason == "" {
				reason = "semantic review flagged request"
			}
		default:
			action = promptfilter.ActionAllow
			if reason == "" {
				reason = "semantic review flagged request; monitor mode allowed"
			}
		}
	}

	if action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", reason)
	}

	if cfg.LogAllows || action != promptfilter.ActionAllow || reviewErr != nil || result.Block {
		h.logSemanticReviewVerdict(c, endpoint, model, text, cfg, result, action, reason, reviewErr)
	}

	if action != promptfilter.ActionBlock {
		return false
	}
	if writeBlock != nil {
		writeBlock()
	}
	return true
}

func (h *Handler) inspectSemanticReviewDisagreementText(c *gin.Context, text string, endpoint string, model string, writeBlock func()) bool {
	cfg := h.loadSemanticReviewDisagreementConfig(c.Request.Context())
	text = prepareSemanticReviewText(text, cfg.MaxChars)
	action := promptfilter.ActionAllow
	reason := ""
	result := semanticReviewResult{Model: cfg.Model}
	var reviewErr error

	if !cfg.readyForDisagreement(endpoint) {
		reviewErr = fmt.Errorf("semantic review is not configured for high-risk prompt review disagreement")
		action = promptfilter.ActionBlock
		reason = "high-risk prompt review disagreement could not be reviewed: " + reviewErr.Error()
	} else if strings.TrimSpace(text) == "" {
		reviewErr = fmt.Errorf("semantic review text empty for high-risk prompt review disagreement")
		action = promptfilter.ActionBlock
		reason = "high-risk prompt review disagreement could not be reviewed: " + reviewErr.Error()
	} else {
		result, reviewErr = runSemanticReview(c.Request.Context(), cfg, endpoint, model, text)
		if reviewErr != nil {
			action, reason = semanticReviewFailureAction(cfg.FailurePolicy, reviewErr)
		} else if result.Block {
			action = promptfilter.ActionBlock
			reason = result.Reason
			if reason == "" {
				reason = "semantic review flagged high-risk prompt review disagreement"
			}
		} else {
			reason = result.Reason
			if reason == "" {
				reason = "semantic review cleared high-risk prompt review disagreement"
			}
		}
	}

	h.logSemanticReviewVerdictWithSource(c, "semantic_review_disagreement", endpoint, model, text, cfg, result, action, reason, reviewErr)
	if action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", reason)
	}
	if action != promptfilter.ActionBlock {
		return false
	}
	if writeBlock != nil {
		writeBlock()
	}
	return true
}

func semanticReviewFailureAction(policy string, reviewErr error) (string, string) {
	errText := ""
	if reviewErr != nil {
		errText = reviewErr.Error()
	}
	switch NormalizeSemanticReviewFailurePolicy(policy) {
	case SemanticReviewFailurePolicyAllow:
		return promptfilter.ActionAllow, "high-risk prompt review disagreement semantic review failed; allowed by failure policy: " + errText
	case SemanticReviewFailurePolicyWarn:
		return promptfilter.ActionWarn, "high-risk prompt review disagreement semantic review failed; warning by failure policy: " + errText
	default:
		return promptfilter.ActionBlock, "high-risk prompt review disagreement semantic review failed: " + errText
	}
}

func runSemanticReview(ctx context.Context, cfg semanticReviewConfig, endpoint string, model string, text string) (semanticReviewResult, error) {
	cacheKey := semanticReviewCacheKey(cfg, endpoint, model, text)
	if cached, ok := semanticReviewCacheState.get(cacheKey); ok {
		cached.Cached = true
		return cached, nil
	}

	current := atomic.AddInt64(&semanticReviewInFlight, 1)
	if current > cfg.MaxConcurrency {
		atomic.AddInt64(&semanticReviewInFlight, -1)
		return semanticReviewResult{Model: cfg.Model}, fmt.Errorf("semantic review concurrency limit reached")
	}
	defer atomic.AddInt64(&semanticReviewInFlight, -1)

	result, err := callSemanticReviewModel(ctx, cfg, endpoint, model, text)
	if err != nil {
		return semanticReviewResult{Model: cfg.Model}, err
	}
	result.Model = cfg.Model
	if cfg.CacheTTL > 0 {
		semanticReviewCacheState.set(cacheKey, result, cfg.CacheTTL)
	}
	return result, nil
}

func callSemanticReviewModel(ctx context.Context, cfg semanticReviewConfig, endpoint string, model string, text string) (semanticReviewResult, error) {
	target, err := semanticReviewChatCompletionsEndpoint(cfg.BaseURL)
	if err != nil {
		return semanticReviewResult{}, err
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	payload, err := json.Marshal(semanticReviewRequest{
		Model: cfg.Model,
		Messages: []semanticReviewMessage{
			{Role: "system", Content: semanticReviewSystemPrompt()},
			{Role: "user", Content: semanticReviewUserPrompt(endpoint, model, text)},
		},
		Temperature: 0,
		MaxTokens:   120,
	})
	if err != nil {
		return semanticReviewResult{}, err
	}
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return semanticReviewResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := semanticReviewHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return semanticReviewResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return semanticReviewResult{}, fmt.Errorf("semantic review request failed with status %d", resp.StatusCode)
	}

	var decoded semanticReviewResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return semanticReviewResult{}, err
	}
	if len(decoded.Choices) == 0 {
		return semanticReviewResult{}, fmt.Errorf("semantic review response missing choices")
	}
	result, err := parseSemanticReviewContent(decoded.Choices[0].Message.Content)
	if err != nil {
		return semanticReviewResult{}, err
	}
	if strings.TrimSpace(decoded.Model) != "" {
		result.Model = strings.TrimSpace(decoded.Model)
	}
	return result, nil
}

func semanticReviewChatCompletionsEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("semantic review base_url must start with http:// or https://")
	}
	if strings.HasSuffix(parsed.Path, "/chat/completions") {
		return parsed.String(), nil
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		parsed.Path = path + "/chat/completions"
	} else {
		parsed.Path = path + "/v1/chat/completions"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func parseSemanticReviewContent(content string) (semanticReviewResult, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return semanticReviewResult{}, fmt.Errorf("semantic review response content empty")
	}
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		content = content[start : end+1]
	}
	var result semanticReviewResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return semanticReviewResult{}, err
	}
	result.Category = strings.TrimSpace(result.Category)
	result.Reason = strings.TrimSpace(result.Reason)
	if result.Confidence < 0 {
		result.Confidence = 0
	}
	if result.Confidence > 1 {
		result.Confidence = 1
	}
	return result, nil
}

func semanticReviewSystemPrompt() string {
	return `You are a strict pre-request classifier for OpenAI cyber safety policy.
Return only compact JSON with this schema:
{"block":true|false,"confidence":0.0-1.0,"category":"short_category","reason":"short_reason"}

Block requests that ask for offensive cyber capability, including malware, credential theft, phishing, persistence, stealth/evasion, data exfiltration, destructive actions, unauthorized access, exploit chaining, botnets, ransomware, or instructions to abuse real systems.
Allow benign defensive security, secure coding, vulnerability explanation, detection, incident response, log analysis, policy discussion, CTF/toy examples without real-world abuse, and requests that refuse or remove harmful content.`
}

func semanticReviewUserPrompt(endpoint string, model string, text string) string {
	return fmt.Sprintf("Endpoint: %s\nModel: %s\nRequest text:\n%s", endpoint, model, text)
}

func prepareSemanticReviewText(text string, maxChars int) string {
	text = promptfilter.RedactSensitive(strings.TrimSpace(text))
	if maxChars <= 0 {
		maxChars = semanticReviewDefaultMaxChars
	}
	if utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	runes := []rune(text)
	head := maxChars * 3 / 4
	tail := maxChars - head
	if head < 1 {
		head = maxChars
		tail = 0
	}
	return string(runes[:head]) + "\n...[semantic review truncated middle]...\n" + string(runes[len(runes)-tail:])
}

func semanticReviewCacheKey(cfg semanticReviewConfig, endpoint string, model string, text string) string {
	sum := sha256.Sum256([]byte(cfg.Model + "\n" + endpoint + "\n" + model + "\n" + text))
	return hex.EncodeToString(sum[:])
}

func (c *semanticReviewCache) get(key string) (semanticReviewResult, bool) {
	if c == nil || key == "" {
		return semanticReviewResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return semanticReviewResult{}, false
	}
	if time.Now().After(entry.expires) {
		delete(c.items, key)
		return semanticReviewResult{}, false
	}
	return entry.result, true
}

func (c *semanticReviewCache) set(key string, result semanticReviewResult, ttl time.Duration) {
	if c == nil || key == "" || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for existing, entry := range c.items {
		if now.After(entry.expires) {
			delete(c.items, existing)
		}
	}
	if len(c.items) > 4096 {
		for existing := range c.items {
			delete(c.items, existing)
			if len(c.items) <= 3072 {
				break
			}
		}
	}
	c.items[key] = semanticReviewCacheEntry{result: result, expires: now.Add(ttl)}
}

func (h *Handler) logSemanticReviewVerdict(c *gin.Context, endpoint string, model string, text string, cfg semanticReviewConfig, result semanticReviewResult, action string, reason string, reviewErr error) {
	h.logSemanticReviewVerdictWithSource(c, "semantic_review", endpoint, model, text, cfg, result, action, reason, reviewErr)
}

func (h *Handler) logSemanticReviewVerdictWithSource(c *gin.Context, source string, endpoint string, model string, text string, cfg semanticReviewConfig, result semanticReviewResult, action string, reason string, reviewErr error) {
	if h == nil || h.db == nil {
		return
	}
	score := int(result.Confidence * 100)
	if result.Block && score == 0 {
		score = 100
	}
	reviewError := ""
	if reviewErr != nil {
		reviewError = reviewErr.Error()
	}
	matched := []promptfilter.Match{}
	if result.Block || result.Category != "" {
		matched = append(matched, promptfilter.Match{
			Name:     "semantic_review",
			Weight:   score,
			Category: result.Category,
			Strict:   result.Block,
		})
	}
	verdict := promptfilter.Verdict{
		Enabled:       true,
		Mode:          cfg.Mode,
		Action:        action,
		Score:         score,
		Threshold:     100,
		Matched:       matched,
		Reason:        reason,
		TextPreview:   text,
		FullText:      text,
		Reviewed:      reviewErr == nil,
		ReviewFlagged: result.Block,
		ReviewError:   reviewError,
		ReviewModel:   result.Model,
	}
	if verdict.ReviewModel == "" {
		verdict.ReviewModel = cfg.Model
	}
	input := &database.PromptFilterLogInput{
		Source:          source,
		Endpoint:        endpoint,
		Model:           model,
		Action:          verdict.Action,
		Mode:            verdict.Mode,
		Score:           verdict.Score,
		Threshold:       verdict.Threshold,
		MatchedPatterns: promptfilter.MatchesJSON(verdict.Matched),
		TextPreview:     promptfilter.RedactedPreview(verdict.TextPreview, 500),
		ClientIP:        c.ClientIP(),
		ReviewModel:     verdict.ReviewModel,
		ReviewFlagged:   verdict.ReviewFlagged,
		ReviewError:     verdict.ReviewError,
	}
	if verdict.Action == promptfilter.ActionBlock {
		input.FullText = promptfilter.RedactedPreview(verdict.FullText, promptFilterFullTextMaxRunes)
	}
	populatePromptFilterAPIKeyMeta(c, input)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.db.InsertPromptFilterLog(ctx, input)
}
