package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	openCodeControlRequestTimeout = 20 * time.Second
	openCodeControlMaxBodyBytes   = 2 * 1024 * 1024
)

// OpenCodeEndpointURL 按版本感知方式拼接 OpenCode 端点路径，避免 /v1/v1/... 重复。
func OpenCodeEndpointURL(baseURL, suffix string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultOpenCodeGoBaseURL
	}
	suffix = "/" + strings.TrimLeft(strings.TrimSpace(suffix), "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(suffix, "/v1/") {
		return base + strings.TrimPrefix(suffix, "/v1")
	}
	return base + suffix
}

// OpenCodeGoUsageURL 根据账号基址解析 OpenCode Go 额度端点（/zen/go/v1/usage）。
func OpenCodeGoUsageURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = DefaultOpenCodeGoBaseURL
	}
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/usage"
}

func newOpenCodeControlClient(proxyURL string) (*http.Client, error) {
	client, err := BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return nil, err
	}
	copied := *client
	copied.Timeout = openCodeControlRequestTimeout
	// 禁止自动跟随重定向，防止 API Key 被跨站转发泄露。
	copied.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &copied, nil
}

func applyOpenCodeControlHeaders(req *http.Request, apiKey string, customHeaders map[string]string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", OpenCodeUserAgent())
	for name, value := range customHeaders {
		trimmedName := strings.TrimSpace(name)
		if trimmedName == "" || IsOpenCodeReservedHeader(trimmedName) {
			continue
		}
		if strings.ContainsAny(trimmedName, "\r\n") || strings.ContainsAny(value, "\r\n") {
			continue
		}
		req.Header.Set(trimmedName, value)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
}

// FetchOpenCodeModels 调用上游 {base_url}/models 拉取 OpenCode 账号可用模型目录。
func FetchOpenCodeModels(ctx context.Context, baseURL, apiKey, proxyURL string, customHeaders map[string]string) ([]string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("缺少 OpenCode API Key")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := OpenCodeEndpointURL(baseURL, "/v1/models")
	client, err := newOpenCodeControlClient(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("创建代理客户端失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造模型列表请求失败: %w", err)
	}
	applyOpenCodeControlHeaders(req, apiKey, customHeaders)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 OpenCode 模型列表失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, openCodeControlMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 OpenCode 模型列表失败: %w", err)
	}
	if len(body) > openCodeControlMaxBodyBytes {
		return nil, fmt.Errorf("OpenCode 模型列表响应过大")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenCode 模型列表返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 256)])))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 OpenCode 模型列表失败: %w", err)
	}
	seen := make(map[string]struct{}, len(payload.Data))
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := StripOpenCodeModelPrefix(item.ID)
		if id == "" || IsOpenCodeUnsupportedModel(id) {
			continue
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, id)
	}
	sort.Strings(models)
	return models, nil
}

type openCodeGoUsageAPIWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

type openCodeGoUsageAPIUsage struct {
	Rolling *openCodeGoUsageAPIWindow `json:"rolling"`
	Weekly  *openCodeGoUsageAPIWindow `json:"weekly"`
	Monthly *openCodeGoUsageAPIWindow `json:"monthly"`
}

type openCodeGoUsageAPIResponse struct {
	Usage *openCodeGoUsageAPIUsage `json:"usage"`
}

// ParseOpenCodeGoUsageJSON 宽容解析官方 /zen/go/v1/usage 响应（支持外层 usage 包裹或顶层直出）。
func ParseOpenCodeGoUsageJSON(body []byte, now time.Time) (*OpenCodeUsageSnapshot, error) {
	var wrapped openCodeGoUsageAPIResponse
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("解析 OpenCode Go 用量 JSON 失败: %w", err)
	}
	usage := wrapped.Usage
	if usage == nil {
		var direct openCodeGoUsageAPIUsage
		if err := json.Unmarshal(body, &direct); err != nil {
			return nil, fmt.Errorf("解析 OpenCode Go 用量 JSON 失败: %w", err)
		}
		usage = &direct
	}
	snap := &OpenCodeUsageSnapshot{
		Status:        OpenCodeUsageStatusOK,
		FetchedAt:     now.UTC(),
		LastAttemptAt: now.UTC(),
		HTTPStatus:    http.StatusOK,
	}
	convertWindow := func(raw *openCodeGoUsageAPIWindow) *OpenCodeUsageWindow {
		if raw == nil {
			return nil
		}
		out := &OpenCodeUsageWindow{
			Status:  strings.TrimSpace(raw.Status),
			Percent: clampSchedulingPercent(raw.Percent),
		}
		if resetsAt, err := time.Parse(time.RFC3339, strings.TrimSpace(raw.ResetsAt)); err == nil {
			out.ResetsAt = resetsAt.UTC()
		}
		return out
	}
	snap.Rolling = convertWindow(usage.Rolling)
	snap.Weekly = convertWindow(usage.Weekly)
	snap.Monthly = convertWindow(usage.Monthly)
	return snap, nil
}

// FetchOpenCodeGoUsage 调用 OpenCode Go 官方 /v1/usage 端点拉取 rolling(5h)/weekly/monthly 快照。
func FetchOpenCodeGoUsage(ctx context.Context, baseURL, apiKey, proxyURL string, customHeaders map[string]string) (*OpenCodeUsageSnapshot, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("缺少 OpenCode API Key")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC()
	endpoint := OpenCodeGoUsageURL(baseURL)
	client, err := newOpenCodeControlClient(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("创建代理客户端失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 OpenCode Go 用量请求失败: %w", err)
	}
	applyOpenCodeControlHeaders(req, apiKey, customHeaders)
	resp, err := client.Do(req)
	if err != nil {
		return &OpenCodeUsageSnapshot{
			Status:        OpenCodeUsageStatusFailed,
			LastAttemptAt: now,
			LastError:     err.Error(),
		}, fmt.Errorf("请求 OpenCode Go 用量失败: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 512*1024+1))
	if readErr != nil {
		return &OpenCodeUsageSnapshot{
			Status:        OpenCodeUsageStatusFailed,
			LastAttemptAt: now,
			HTTPStatus:    resp.StatusCode,
			LastError:     "读取用量响应失败",
		}, fmt.Errorf("读取 OpenCode Go 用量响应失败: %w", readErr)
	}
	if len(body) > 512*1024 {
		return &OpenCodeUsageSnapshot{
			Status:        OpenCodeUsageStatusFailed,
			LastAttemptAt: now,
			HTTPStatus:    resp.StatusCode,
			LastError:     "用量响应过大",
		}, fmt.Errorf("OpenCode Go 用量响应过大")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return &OpenCodeUsageSnapshot{
			Status:        OpenCodeUsageStatusUnauthorized,
			LastAttemptAt: now,
			HTTPStatus:    resp.StatusCode,
			LastError:     "unauthorized",
		}, fmt.Errorf("OpenCode Go 用量认证失败 (HTTP 401)")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return &OpenCodeUsageSnapshot{
			Status:        OpenCodeUsageStatusFailed,
			LastAttemptAt: now,
			HTTPStatus:    resp.StatusCode,
			LastError:     "redirect_blocked",
		}, fmt.Errorf("OpenCode Go 用量端点发生重定向 (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &OpenCodeUsageSnapshot{
			Status:        OpenCodeUsageStatusFailed,
			LastAttemptAt: now,
			HTTPStatus:    resp.StatusCode,
			LastError:     fmt.Sprintf("http_%d", resp.StatusCode),
		}, fmt.Errorf("OpenCode Go 用量返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 256)])))
	}
	snap, parseErr := ParseOpenCodeGoUsageJSON(body, now)
	if parseErr != nil {
		return &OpenCodeUsageSnapshot{
			Status:        OpenCodeUsageStatusFailed,
			LastAttemptAt: now,
			HTTPStatus:    resp.StatusCode,
			LastError:     "invalid_json",
		}, parseErr
	}
	return snap, nil
}
