package promptfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	DefaultReviewBaseURL        = "https://api.openai.com"
	DefaultReviewModel          = "omni-moderation-latest"
	DefaultReviewTimeoutSeconds = 10
	HighRiskReviewFailureScore  = 200
)

type ReviewClient struct {
	HTTPClient *http.Client
}

var DefaultReviewClient = ReviewClient{}

type reviewRequest struct {
	Model string `json:"model,omitempty"`
	Input string `json:"input"`
}

type reviewResponse struct {
	Model   string         `json:"model"`
	Results []reviewResult `json:"results"`
}

type reviewResult struct {
	Flagged bool `json:"flagged"`
}

func NormalizeReviewConfig(cfg ReviewConfig) ReviewConfig {
	defaults := DefaultReviewConfig()
	// 规范化多 key：按行/逗号/分号/空白切分，去空去重，再以换行拼回，
	// 便于存储与轮询（issue #289）。单 key 配置行为不变。
	cfg.APIKey = strings.Join(parseReviewAPIKeys(cfg.APIKey), "\n")
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaults.BaseURL
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.Model == "" {
		cfg.Model = defaults.Model
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if cfg.TimeoutSeconds > 60 {
		cfg.TimeoutSeconds = 60
	}
	return cfg
}

// APIKeyList 解析配置的审查 API key 列表。可用换行/逗号/分号/空白分隔多个 key，
// 以便把 Moderations 的 TPM 额度分摊到多个 OpenAI 账号上（issue #289）。
// 去除空白项与重复项并保持顺序。
func (cfg ReviewConfig) APIKeyList() []string {
	return parseReviewAPIKeys(cfg.APIKey)
}

func parseReviewAPIKeys(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';'
	})
	seen := make(map[string]struct{}, len(fields))
	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		key := strings.TrimSpace(f)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

func (cfg ReviewConfig) Ready() bool {
	cfg = NormalizeReviewConfig(cfg)
	return cfg.Enabled && len(cfg.APIKeyList()) > 0 && cfg.BaseURL != ""
}

func ValidateReviewConfig(cfg ReviewConfig) error {
	cfg = NormalizeReviewConfig(cfg)
	if cfg.Enabled && len(cfg.APIKeyList()) == 0 {
		return fmt.Errorf("at least one review api key is required when prompt filter review is enabled")
	}
	if cfg.BaseURL == "" {
		return nil
	}
	_, err := reviewEndpoint(cfg.BaseURL)
	return err
}

// reviewKeyCursor 为多 key 轮询提供全局起点游标，让并发请求均匀分摊 TPM 额度。
var reviewKeyCursor atomic.Uint64

func (c ReviewClient) ReviewText(ctx context.Context, text string, cfg ReviewConfig) (bool, string, error) {
	cfg = NormalizeReviewConfig(cfg)
	if !cfg.Ready() {
		return false, cfg.Model, nil
	}
	if strings.TrimSpace(text) == "" {
		return false, cfg.Model, nil
	}
	endpoint, err := reviewEndpoint(cfg.BaseURL)
	if err != nil {
		return false, cfg.Model, err
	}
	payload, err := json.Marshal(reviewRequest{
		Model: cfg.Model,
		Input: text,
	})
	if err != nil {
		return false, cfg.Model, err
	}

	keys := cfg.APIKeyList()
	// 轮询起点 + 遇到限流/失效 key（429/401/403/5xx/网络错误）自动切换下一个 key。
	start := reviewKeyCursor.Add(1) - 1
	var lastErr error
	for i := 0; i < len(keys); i++ {
		key := keys[(start+uint64(i))%uint64(len(keys))]
		flagged, model, retriable, reqErr := c.reviewOnce(ctx, endpoint, key, payload, cfg)
		if reqErr == nil {
			return flagged, model, nil
		}
		lastErr = reqErr
		if !retriable {
			return false, cfg.Model, reqErr
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("review request failed")
	}
	return false, cfg.Model, lastErr
}

// reviewOnce 用单个 key 发起一次 Moderations 请求。retriable 表示该错误是否
// 值得切换到下一个 key 重试（限流/失效 key/服务端错误/网络错误）。
func (c ReviewClient) reviewOnce(ctx context.Context, endpoint, apiKey string, payload []byte, cfg ReviewConfig) (flagged bool, model string, retriable bool, err error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, cfg.Model, false, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// 网络错误：换下一个 key 再试。
		return false, cfg.Model, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, cfg.Model, reviewStatusRetriable(resp.StatusCode), fmt.Errorf("review request failed with status %d", resp.StatusCode)
	}

	var decoded reviewResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return false, cfg.Model, false, err
	}
	if len(decoded.Results) == 0 {
		return false, cfg.Model, false, fmt.Errorf("review response missing results")
	}
	for _, result := range decoded.Results {
		if result.Flagged {
			flagged = true
			break
		}
	}
	model = strings.TrimSpace(decoded.Model)
	if model == "" {
		model = cfg.Model
	}
	return flagged, model, false, nil
}

// reviewStatusRetriable 判断某个 HTTP 状态码是否应切换到下一个 key 重试：
// 429（TPM/RPM 限流，本 issue 主因）、401/403（key 失效）、5xx（服务端错误）。
func reviewStatusRetriable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return status >= 500
}

func ApplyReviewResult(verdict Verdict, flagged bool, model string, reviewErr error, cfg ReviewConfig) Verdict {
	cfg = NormalizeReviewConfig(cfg)
	verdict.Reviewed = true
	verdict.ReviewFlagged = flagged
	verdict.ReviewModel = strings.TrimSpace(model)
	if verdict.ReviewModel == "" {
		verdict.ReviewModel = cfg.Model
	}
	if reviewErr != nil {
		verdict.ReviewError = reviewErr.Error()
		if cfg.FailClosed {
			verdict.Action = ActionBlock
			verdict.Reason = "prompt review failed: " + reviewErr.Error()
		} else if reviewFailureShouldFailClosed(verdict) {
			verdict.Action = ActionBlock
			verdict.Reason = "high-risk prompt review failed: " + reviewErr.Error()
		} else {
			verdict.Action = ActionAllow
			verdict.Reason = "prompt review failed; allowed by policy: " + reviewErr.Error()
		}
		return verdict
	}
	if !flagged {
		verdict.Action = ActionAllow
		verdict.Reason = "prompt review cleared local filter match"
		return verdict
	}
	verdict.Reason = "prompt review confirmed local filter match"
	return verdict
}

func reviewFailureShouldFailClosed(verdict Verdict) bool {
	return IsHighRiskReviewVerdict(verdict)
}

func IsHighRiskReviewVerdict(verdict Verdict) bool {
	if verdict.Score >= HighRiskReviewFailureScore || verdict.RawScore >= HighRiskReviewFailureScore || verdict.StrictHit {
		return true
	}
	for _, match := range verdict.Matched {
		if match.Strict {
			return true
		}
	}
	return false
}

func reviewEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultReviewBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("review base_url must start with http:// or https://")
	}
	if strings.HasSuffix(parsed.Path, "/moderations") {
		return parsed.String(), nil
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		parsed.Path = path + "/moderations"
	} else {
		parsed.Path = path + "/v1/moderations"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
