package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const proxyRiskScoringMaxResponseBytes = 2 << 20

type proxyRiskScoringProfileRequest struct {
	Name               *string `json:"name"`
	Provider           *string `json:"provider"`
	Enabled            *bool   `json:"enabled"`
	Priority           *int    `json:"priority"`
	ScamalyticsHost    *string `json:"scamalytics_host"`
	ScamalyticsUser    *string `json:"scamalytics_user"`
	ScamalyticsKey     *string `json:"scamalytics_key"`
	TimeoutSeconds     *int    `json:"timeout_seconds"`
	Concurrency        *int    `json:"concurrency"`
	RequestDelayMS     *int    `json:"request_delay_ms"`
	CacheTTLSeconds    *int    `json:"cache_ttl_seconds"`
	MaxChecksPerJob    *int    `json:"max_checks_per_job"`
	DailyCheckLimit    *int    `json:"daily_check_limit"`
	CreditReserve      *int64  `json:"credit_reserve"`
	AllowForceRefresh  *bool   `json:"allow_force_refresh"`
	ResolveHostnames   *bool   `json:"resolve_hostnames"`
	AllowPrivateTarget *bool   `json:"allow_private_targets"`
	DocsURL            *string `json:"docs_url"`
	TutorialURL        *string `json:"tutorial_url"`
}

func validateScamalyticsHost(raw string) error {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		return errors.New("Scamalytics Host 不能为空")
	}
	if strings.ContainsAny(host, "/?#@:") || (host != "scamalytics.com" && !strings.HasSuffix(host, ".scamalytics.com")) {
		return errors.New("Scamalytics Host 必须是 scamalytics.com 的域名")
	}
	return nil
}

func validateProxyRiskScoringLink(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return errors.New("文档或教程 URL 必须包含 http(s) scheme 和主机名")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("文档或教程 URL 仅支持 http 或 https")
	}
	if parsed.User != nil {
		return errors.New("文档或教程 URL 不得包含用户信息")
	}
	return nil
}

func mergeProxyRiskScoringProfile(current database.ProxyRiskScoringProfile, req proxyRiskScoringProfileRequest) (database.ProxyRiskScoringProfile, error) {
	if req.Name != nil {
		current.Name = *req.Name
	}
	if req.Provider != nil {
		current.Provider = *req.Provider
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.Priority != nil {
		current.Priority = *req.Priority
	}
	if req.ScamalyticsHost != nil {
		current.ScamalyticsHost = *req.ScamalyticsHost
	}
	if req.ScamalyticsUser != nil {
		current.ScamalyticsUser = *req.ScamalyticsUser
	}
	if req.ScamalyticsKey != nil {
		current.ScamalyticsKey = *req.ScamalyticsKey
	}
	if req.TimeoutSeconds != nil {
		current.TimeoutSeconds = *req.TimeoutSeconds
	}
	if req.Concurrency != nil {
		current.Concurrency = *req.Concurrency
	}
	if req.RequestDelayMS != nil {
		current.RequestDelayMS = *req.RequestDelayMS
	}
	if req.CacheTTLSeconds != nil {
		current.CacheTTLSeconds = *req.CacheTTLSeconds
	}
	if req.MaxChecksPerJob != nil {
		current.MaxChecksPerJob = *req.MaxChecksPerJob
	}
	if req.DailyCheckLimit != nil {
		current.DailyCheckLimit = *req.DailyCheckLimit
	}
	if req.CreditReserve != nil {
		current.CreditReserve = *req.CreditReserve
	}
	if req.AllowForceRefresh != nil {
		current.AllowForceRefresh = *req.AllowForceRefresh
	}
	if req.ResolveHostnames != nil {
		current.ResolveHostnames = *req.ResolveHostnames
	}
	if req.AllowPrivateTarget != nil {
		current.AllowPrivateTarget = *req.AllowPrivateTarget
	}
	if req.DocsURL != nil {
		current.DocsURL = *req.DocsURL
	}
	if req.TutorialURL != nil {
		current.TutorialURL = *req.TutorialURL
	}
	current = database.NormalizeProxyRiskScoringProfile(current)
	if err := validateScamalyticsHost(current.ScamalyticsHost); err != nil {
		return current, err
	}
	if current.Enabled && (strings.TrimSpace(current.ScamalyticsUser) == "" || strings.TrimSpace(current.ScamalyticsKey) == "") {
		return current, errors.New("启用评分档案前必须配置 Scamalytics User 和 API Key")
	}
	if err := validateProxyRiskScoringLink(current.DocsURL); err != nil {
		return current, err
	}
	if err := validateProxyRiskScoringLink(current.TutorialURL); err != nil {
		return current, err
	}
	return current, nil
}

func maskProxyRiskSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "••••"
	}
	return value[:4] + "…" + value[len(value)-4:]
}

func proxyRiskScoringProfileResponse(profile database.ProxyRiskScoringProfile) gin.H {
	return gin.H{
		"id": profile.ID, "name": profile.Name, "provider": profile.Provider, "enabled": profile.Enabled, "priority": profile.Priority,
		"engine":           "builtin_scamalytics_v3",
		"scamalytics_host": profile.ScamalyticsHost, "scamalytics_user": profile.ScamalyticsUser,
		"scamalytics_key_configured": strings.TrimSpace(profile.ScamalyticsKey) != "", "scamalytics_key_masked": maskProxyRiskSecret(profile.ScamalyticsKey),
		"timeout_seconds": profile.TimeoutSeconds, "concurrency": profile.Concurrency, "request_delay_ms": profile.RequestDelayMS,
		"cache_ttl_seconds": profile.CacheTTLSeconds, "max_checks_per_job": profile.MaxChecksPerJob, "daily_check_limit": profile.DailyCheckLimit,
		"credit_reserve": profile.CreditReserve, "allow_force_refresh": profile.AllowForceRefresh, "resolve_hostnames": profile.ResolveHostnames,
		"allow_private_targets": profile.AllowPrivateTarget, "docs_url": profile.DocsURL, "tutorial_url": profile.TutorialURL,
		"daily_used_date": profile.DailyUsedDate, "daily_used_count": profile.DailyUsedCount, "credits_remaining": profile.CreditsRemaining,
		"credits_used": profile.CreditsUsed, "credit_reset_at": profile.CreditResetAt, "last_quota_checked_at": profile.LastQuotaCheckedAt,
		"last_error": profile.LastError, "created_at": profile.CreatedAt, "updated_at": profile.UpdatedAt,
	}
}

func (h *Handler) ListProxyRiskScoringProfiles(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	profiles, err := h.db.ListProxyRiskScoringProfiles(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	items := make([]gin.H, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, proxyRiskScoringProfileResponse(profile))
	}
	c.JSON(http.StatusOK, gin.H{"profiles": items})
}

func (h *Handler) CreateProxyRiskScoringProfile(c *gin.Context) {
	var req proxyRiskScoringProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "评分服务配置格式错误")
		return
	}
	profile := database.ProxyRiskScoringProfile{Enabled: false}
	profile, err := mergeProxyRiskScoringProfile(profile, req)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	id, err := h.db.CreateProxyRiskScoringProfile(ctx, &profile)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	profile.ID = id
	c.JSON(http.StatusCreated, proxyRiskScoringProfileResponse(profile))
}

func (h *Handler) UpdateProxyRiskScoringProfile(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("profile_id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "评分服务档案 ID 无效")
		return
	}
	var req proxyRiskScoringProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "评分服务配置格式错误")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	current, err := h.db.GetProxyRiskScoringProfile(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "评分服务档案不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	updated, err := mergeProxyRiskScoringProfile(*current, req)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	updated.ID = id
	if err := h.db.UpdateProxyRiskScoringProfile(ctx, &updated); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, proxyRiskScoringProfileResponse(updated))
}

func (h *Handler) DeleteProxyRiskScoringProfile(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("profile_id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "评分服务档案 ID 无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.DeleteProxyRiskScoringProfile(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "评分服务档案不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "评分服务档案已删除"})
}

type proxyRiskScoringClient struct {
	profile database.ProxyRiskScoringProfile
	http    *http.Client
}

func newProxyRiskScoringClient(profile database.ProxyRiskScoringProfile) *proxyRiskScoringClient {
	profile = database.NormalizeProxyRiskScoringProfile(profile)
	return &proxyRiskScoringClient{profile: profile, http: &http.Client{Timeout: time.Duration(profile.TimeoutSeconds) * time.Second}}
}

func (client *proxyRiskScoringClient) requestIP(ctx context.Context, ip string) ([]byte, int, error) {
	if client == nil {
		return nil, 0, errors.New("评分客户端未初始化")
	}
	if err := validateScamalyticsHost(client.profile.ScamalyticsHost); err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(client.profile.ScamalyticsUser) == "" || strings.TrimSpace(client.profile.ScamalyticsKey) == "" {
		return nil, 0, errors.New("Scamalytics User 和 API Key 必须配置")
	}
	if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed == nil || parsed.To4() == nil {
		return nil, 0, errors.New("评分请求必须使用 IPv4 地址")
	}
	requestURL := "https://" + strings.TrimSpace(client.profile.ScamalyticsHost) + "/v3/" + url.PathEscape(strings.TrimSpace(client.profile.ScamalyticsUser)) + "/?key=" + url.QueryEscape(strings.TrimSpace(client.profile.ScamalyticsKey)) + "&ip=" + url.QueryEscape(strings.TrimSpace(ip))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Codex2API-Scamalytics/1.0")
	started := time.Now()
	resp, err := client.http.Do(req)
	latency := int(time.Since(started).Milliseconds())
	if err != nil {
		return nil, latency, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, proxyRiskScoringMaxResponseBytes+1))
	if readErr != nil {
		return nil, latency, readErr
	}
	if len(body) > proxyRiskScoringMaxResponseBytes {
		return nil, latency, errors.New("Scamalytics 响应过大")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if len(message) > 256 {
			message = message[:256]
		}
		if message == "" {
			message = fmt.Sprintf("Scamalytics 返回 HTTP %d", resp.StatusCode)
		}
		return nil, latency, errors.New(message)
	}
	return body, latency, nil
}

func (client *proxyRiskScoringClient) testConnection(ctx context.Context) (database.ProxyRiskScoreSnapshot, *proxyRiskCredits, error) {
	result, credits, err := client.checkIP(ctx, "8.8.8.8")
	if err != nil {
		return result, credits, err
	}
	return result, credits, nil
}

type proxyRiskCredits struct {
	Remaining *int64
	Used      *int64
	ResetAt   *time.Time
}

func (client *proxyRiskScoringClient) checkIP(ctx context.Context, ip string) (database.ProxyRiskScoreSnapshot, *proxyRiskCredits, error) {
	started := time.Now()
	body, latency, err := client.requestIP(ctx, ip)
	if latency == 0 {
		latency = int(time.Since(started).Milliseconds())
	}
	if err != nil {
		return database.ProxyRiskScoreSnapshot{Provider: client.profile.Provider, ResolvedIP: ip, Status: database.ProxyRiskScoringStatusError(), LatencyMS: latency, Error: err.Error()}, nil, err
	}
	result, credits, err := parseProxyRiskScoringResponse(body, latency)
	result.Provider = client.profile.Provider
	result.ResolvedIP = ip
	result.RawResponseJSON = redactProxyRiskJSON(body)
	return result, credits, err
}

func parseProxyRiskScoringResponse(body []byte, latencyMS int) (database.ProxyRiskScoreSnapshot, *proxyRiskCredits, error) {
	if !gjson.ValidBytes(body) {
		return database.ProxyRiskScoreSnapshot{Status: database.ProxyRiskScoringStatusError(), LatencyMS: latencyMS, Error: "评分服务返回了无效 JSON"}, nil, errors.New("评分服务返回了无效 JSON")
	}
	root := gjson.ParseBytes(body)
	if message := strings.TrimSpace(root.Get("error").String()); message != "" {
		return database.ProxyRiskScoreSnapshot{Status: database.ProxyRiskScoringStatusError(), LatencyMS: latencyMS, Error: message}, nil, errors.New(message)
	}
	if message := strings.TrimSpace(root.Get("scamalytics.error").String()); message != "" {
		return database.ProxyRiskScoreSnapshot{Status: database.ProxyRiskScoringStatusError(), LatencyMS: latencyMS, Error: message}, nil, errors.New(message)
	}
	result := database.ProxyRiskScoreSnapshot{Status: database.ProxyRiskScoringStatusSuccess(), LatencyMS: latencyMS, BlacklistSource: []string{}}
	scoreResult := root.Get("scamalytics.scamalytics_score")
	if !scoreResult.Exists() {
		scoreResult = root.Get("scamalytics.score")
	}
	if !scoreResult.Exists() {
		scoreResult = root.Get("score")
	}
	if scoreResult.Exists() {
		if value, err := parseProxyRiskScoreValue(scoreResult); err == nil {
			result.Score = &value
		}
	}
	result.RiskLevel = strings.ToLower(strings.TrimSpace(firstJSONString(root, "scamalytics.scamalytics_risk", "scamalytics.risk", "risk")))
	proxyData := root.Get("scamalytics.scamalytics_proxy")
	external := root.Get("external_datasources")
	result.IsVPN = jsonBool(proxyData.Get("is_vpn")) || jsonBool(external.Get("x4bnet.is_vpn"))
	result.IsTOR = jsonBool(proxyData.Get("is_tor")) || strings.EqualFold(firstJSONString(external, "ip2proxy_lite.proxy_type", "ip2proxy.proxy_type"), "TOR")
	result.IsDatacenter = jsonBool(proxyData.Get("is_datacenter")) || jsonBool(external.Get("x4bnet.is_datacenter"))
	result.IsBlacklisted = jsonBool(root.Get("scamalytics.is_blacklisted_external"))
	proxyType := strings.ToUpper(strings.TrimSpace(firstJSONString(external, "ip2proxy_lite.proxy_type", "ip2proxy.proxy_type")))
	result.ProxyType = proxyType
	if jsonBool(external.Get("firehol.is_proxy")) || proxyType == "VPN" || proxyType == "TOR" || proxyType == "PUB" || proxyType == "WEB" {
		result.IsVPN = result.IsVPN || proxyType == "VPN"
	}
	blacklist := []struct {
		path string
		name string
	}{
		{"ip2proxy_lite.ip_blacklisted", "ip2proxy"}, {"ip2proxy.ip_blacklisted", "ip2proxy"}, {"ipsum.ip_blacklisted", "ipsum"},
		{"spamhaus_drop.ip_blacklisted", "spamhaus"}, {"firehol.is_blacklisted_1day", "firehol"}, {"firehol.is_blacklisted_30", "firehol"},
		{"x4bnet.is_blacklisted_spambot", "x4bnet-spambot"},
	}
	seenBlacklist := map[string]struct{}{}
	for _, item := range blacklist {
		if jsonBool(external.Get(item.path)) {
			result.IsBlacklisted = true
			if _, exists := seenBlacklist[item.name]; !exists {
				result.BlacklistSource = append(result.BlacklistSource, item.name)
				seenBlacklist[item.name] = struct{}{}
			}
		}
	}
	result.ISP = boundedProxyScoringText(firstJSONString(root, "scamalytics.scamalytics_isp", "scamalytics.scamalytics_org", "external_datasources.dbip.isp_name", "external_datasources.ip2proxy_lite.isp_name"), 512)
	result.Country = boundedProxyScoringText(firstJSONString(root, "external_datasources.ip2proxy_lite.ip_country_code", "external_datasources.dbip.ip_country_code", "scamalytics.ip_country_code"), 128)
	result.Recommendation = proxyRiskRecommendation(result)
	features := map[string]any{"scamalytics_proxy": proxyData.Value(), "external_datasources": external.Value()}
	if encoded, err := json.Marshal(features); err == nil {
		result.FeaturesJSON = redactProxyRiskJSON(encoded)
	}
	credits := parseProxyRiskCredits(root)
	return result, credits, nil
}

func parseProxyRiskScoreValue(value gjson.Result) (int, error) {
	raw := strings.Trim(strings.TrimSpace(value.Raw), `"`)
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || math.Trunc(parsed) != parsed || parsed < 0 || parsed > 100 {
		return 0, errors.New("评分服务返回了无效分数")
	}
	return int(parsed), nil
}

func firstJSONString(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		value := root.Get(path)
		if value.Exists() && value.Type == gjson.String {
			if text := strings.TrimSpace(value.String()); text != "" {
				return text
			}
		}
	}
	return ""
}

func jsonBool(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	return value.Bool() || value.String() == "1" || strings.EqualFold(value.String(), "true")
}

func boundedProxyScoringText(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

func proxyRiskRecommendation(result database.ProxyRiskScoreSnapshot) string {
	if result.RiskLevel == "high" || result.RiskLevel == "very high" || result.IsBlacklisted || result.IsTOR {
		return "replace"
	}
	if result.RiskLevel == "medium" || result.IsVPN || result.IsDatacenter || (result.Score != nil && *result.Score >= 50) {
		return "watch"
	}
	return "keep"
}

func parseProxyRiskCredits(root gjson.Result) *proxyRiskCredits {
	credits := root.Get("scamalytics.credits")
	if !credits.Exists() {
		credits = root.Get("credits")
	}
	if !credits.Exists() || !credits.IsObject() {
		return nil
	}
	result := &proxyRiskCredits{}
	for key, target := range map[string]**int64{"remaining": &result.Remaining, "used": &result.Used} {
		value := credits.Get(key)
		if value.Exists() {
			if parsed, err := strconv.ParseInt(strings.TrimSpace(value.Raw), 10, 64); err == nil && parsed >= 0 {
				copyValue := parsed
				*target = &copyValue
			}
		}
	}
	for _, key := range []string{"reset_at", "resetAt", "reset_time"} {
		value := strings.TrimSpace(credits.Get(key).String())
		if value == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			result.ResetAt = &parsed
			break
		}
	}
	if result.Remaining == nil && result.Used == nil && result.ResetAt == nil {
		return nil
	}
	return result
}

func redactProxyRiskJSON(body []byte) string {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return ""
	}
	var redact func(any) any
	redact = func(input any) any {
		switch typed := input.(type) {
		case map[string]any:
			out := make(map[string]any, len(typed))
			for key, value := range typed {
				lower := strings.ToLower(key)
				if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || lower == "key" || strings.HasSuffix(lower, "_key") {
					out[key] = "[redacted]"
					continue
				}
				out[key] = redact(value)
			}
			return out
		case []any:
			out := make([]any, len(typed))
			for index, value := range typed {
				out[index] = redact(value)
			}
			return out
		default:
			return input
		}
	}
	encoded, err := json.Marshal(redact(value))
	if err != nil {
		return ""
	}
	return boundedProxyScoringText(string(encoded), proxyRiskScoringMaxResponseBytes)
}

func resolveProxyRiskScoringIP(ctx context.Context, rawURL string, resolveHostnames, allowPrivate bool, lookup func(context.Context, string) ([]net.IP, error)) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	host, err := database.ResolveProxyRiskScoringHost(rawURL)
	if err != nil {
		return "", err
	}
	if literal := net.ParseIP(host); literal != nil {
		if literal.To4() == nil {
			return "", errors.New("评分服务目前只支持 IPv4 代理")
		}
		if !allowPrivate && !database.IsPublicProxyRiskScoringIP(literal) {
			return "", errors.New("代理 IP 属于私网或保留地址，默认不评分")
		}
		return literal.To4().String(), nil
	}
	if !resolveHostnames {
		return "", errors.New("代理 URL 使用域名；请先开启受限 DNS 解析")
	}
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip4", host)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return "", fmt.Errorf("代理域名解析失败: %w", err)
	}
	for _, ip := range ips {
		if ip == nil || ip.To4() == nil {
			continue
		}
		if !allowPrivate && !database.IsPublicProxyRiskScoringIP(ip) {
			continue
		}
		return ip.To4().String(), nil
	}
	return "", errors.New("代理域名没有可评分的公网 IPv4 地址")
}

type proxyRiskScoringJob struct {
	mu        sync.RWMutex
	ID        string    `json:"job_id"`
	ProfileID int64     `json:"profile_id"`
	Status    string    `json:"status"`
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Success   int       `json:"success"`
	Failed    int       `json:"failed"`
	Skipped   int       `json:"skipped"`
	CacheHits int       `json:"cache_hits"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Current 是正在检测的代理标签（host:port），Items 是逐条结果（按 Seq 递增），
	// 供前端轮询增量渲染：检测完一条就能在表格里看到一条。
	Current string                    `json:"current,omitempty"`
	Items   []proxyRiskScoringJobItem `json:"-"`
	cancel  context.CancelFunc
}

// proxyRiskScoringJobItem 是任务里一条代理的检测结果。
type proxyRiskScoringJobItem struct {
	Seq       int                              `json:"seq"`
	ProxyID   int64                            `json:"proxy_id"`
	Label     string                           `json:"label"`
	Status    string                           `json:"status"` // success | error | skipped | cached
	Error     string                           `json:"error,omitempty"`
	Snapshot  *database.ProxyRiskScoreSnapshot `json:"snapshot,omitempty"`
	CheckedAt time.Time                        `json:"checked_at"`
}

type proxyRiskScoringJobSnapshot struct {
	ID        string    `json:"job_id"`
	ProfileID int64     `json:"profile_id"`
	Status    string    `json:"status"`
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Success   int       `json:"success"`
	Failed    int       `json:"failed"`
	Skipped   int       `json:"skipped"`
	CacheHits int       `json:"cache_hits"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Current   string    `json:"current,omitempty"`
	// Items 只包含 Seq > after 的增量；LastSeq 供下次轮询作为 after。
	Items   []proxyRiskScoringJobItem `json:"items"`
	LastSeq int                       `json:"last_seq"`
}

func (job *proxyRiskScoringJob) snapshot() proxyRiskScoringJobSnapshot {
	return job.snapshotAfter(0)
}

func (job *proxyRiskScoringJob) snapshotAfter(after int) proxyRiskScoringJobSnapshot {
	job.mu.RLock()
	defer job.mu.RUnlock()
	items := make([]proxyRiskScoringJobItem, 0)
	for _, item := range job.Items {
		if item.Seq > after {
			items = append(items, item)
		}
	}
	return proxyRiskScoringJobSnapshot{
		ID: job.ID, ProfileID: job.ProfileID, Status: job.Status, Total: job.Total,
		Done: job.Done, Success: job.Success, Failed: job.Failed, Skipped: job.Skipped,
		CacheHits: job.CacheHits, Error: job.Error, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
		Current: job.Current, Items: items, LastSeq: len(job.Items),
	}
}

// appendItem 追加一条逐条结果（调用方须持有 job.mu）。
func (job *proxyRiskScoringJob) appendItem(item proxyRiskScoringJobItem) {
	item.Seq = len(job.Items) + 1
	if item.CheckedAt.IsZero() {
		item.CheckedAt = time.Now().UTC()
	}
	job.Items = append(job.Items, item)
}

// proxyRiskScoringLabel 是进度里展示的代理标签：优先备注，否则 host:port（不带凭据）。
func proxyRiskScoringLabel(proxy *database.ProxyRow) string {
	if proxy == nil {
		return ""
	}
	if label := strings.TrimSpace(proxy.Label); label != "" {
		return label
	}
	if parsed, err := url.Parse(strings.TrimSpace(proxy.URL)); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return fmt.Sprintf("#%d", proxy.ID)
}

func (h *Handler) setProxyRiskScoringJob(job *proxyRiskScoringJob) {
	h.proxyRiskJobsMu.Lock()
	defer h.proxyRiskJobsMu.Unlock()
	h.proxyRiskJobs[job.ID] = job
}

func (h *Handler) getProxyRiskScoringJob(id string) *proxyRiskScoringJob {
	h.proxyRiskJobsMu.RLock()
	defer h.proxyRiskJobsMu.RUnlock()
	return h.proxyRiskJobs[id]
}

func (h *Handler) updateProxyRiskScoringJob(id string, fn func(*proxyRiskScoringJob)) {
	job := h.getProxyRiskScoringJob(id)
	if job == nil {
		return
	}
	job.mu.Lock()
	fn(job)
	job.UpdatedAt = time.Now().UTC()
	job.mu.Unlock()
}

type proxyRiskScoringJobRequest struct {
	ProfileID int64   `json:"profile_id"`
	ProxyIDs  []int64 `json:"proxy_ids"`
	Force     bool    `json:"force"`
}

func (h *Handler) StartProxyRiskScoringJob(c *gin.Context) {
	var req proxyRiskScoringJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "评分任务格式错误")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	profiles, err := h.db.ListProxyRiskScoringProfiles(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	var profile *database.ProxyRiskScoringProfile
	for index := range profiles {
		if req.ProfileID > 0 && profiles[index].ID == req.ProfileID {
			candidate := profiles[index]
			profile = &candidate
			break
		}
		if req.ProfileID == 0 && profile == nil && profiles[index].Enabled && strings.TrimSpace(profiles[index].ScamalyticsHost) != "" {
			candidate := profiles[index]
			profile = &candidate
		}
	}
	if profile == nil {
		writeError(c, http.StatusBadRequest, "没有可用的评分服务档案")
		return
	}
	if !profile.Enabled {
		writeError(c, http.StatusBadRequest, "评分服务档案未启用")
		return
	}
	if err := validateScamalyticsHost(profile.ScamalyticsHost); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(profile.ScamalyticsUser) == "" || strings.TrimSpace(profile.ScamalyticsKey) == "" {
		writeError(c, http.StatusBadRequest, "评分前必须配置 Scamalytics User 和 API Key")
		return
	}
	if req.Force && !profile.AllowForceRefresh {
		writeError(c, http.StatusBadRequest, "当前评分档案未允许强制刷新")
		return
	}
	proxies, err := h.db.ListProxies(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if len(req.ProxyIDs) > 0 {
		selected := make(map[int64]struct{}, len(req.ProxyIDs))
		for _, id := range req.ProxyIDs {
			if id > 0 {
				selected[id] = struct{}{}
			}
		}
		filtered := make([]*database.ProxyRow, 0, len(selected))
		for _, proxy := range proxies {
			if _, exists := selected[proxy.ID]; exists {
				filtered = append(filtered, proxy)
			}
		}
		proxies = filtered
	}
	if len(proxies) == 0 {
		writeError(c, http.StatusBadRequest, "没有可评分的代理")
		return
	}
	jobCtx, jobCancel := context.WithCancel(context.Background())
	job := &proxyRiskScoringJob{ID: uuid.NewString(), ProfileID: profile.ID, Status: "queued", Total: len(proxies), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), cancel: jobCancel}
	h.setProxyRiskScoringJob(job)
	if !h.db.RunBackgroundTask(func(dbCtx context.Context) {
		runCtx, stop := context.WithCancel(dbCtx)
		defer stop()
		go func() {
			select {
			case <-jobCtx.Done():
				stop()
			case <-dbCtx.Done():
			}
		}()
		h.runProxyRiskScoringJob(runCtx, job, *profile, proxies, req.Force)
	}) {
		jobCancel()
		h.updateProxyRiskScoringJob(job.ID, func(current *proxyRiskScoringJob) {
			current.Status = "rejected"
			current.Error = "后台任务正在关闭"
		})
		writeError(c, http.StatusServiceUnavailable, "评分后台队列暂不可用")
		return
	}
	c.JSON(http.StatusAccepted, job.snapshot())
}

func (h *Handler) runProxyRiskScoringJob(ctx context.Context, job *proxyRiskScoringJob, profile database.ProxyRiskScoringProfile, proxies []*database.ProxyRow, force bool) {
	defer func() {
		job.mu.Lock()
		cancel := job.cancel
		job.cancel = nil
		if ctx.Err() != nil {
			job.Status = "cancelled"
		} else {
			job.Status = "completed"
		}
		job.UpdatedAt = time.Now().UTC()
		job.mu.Unlock()
		// Stop the parent watcher goroutine once the job has finished. The
		// cancellation function is intentionally cleared so a completed job
		// cannot be cancelled again from the admin API.
		if cancel != nil {
			cancel()
		}
	}()
	h.updateProxyRiskScoringJob(job.ID, func(current *proxyRiskScoringJob) { current.Status = "running" })
	if profile.MaxChecksPerJob > 0 && len(proxies) > profile.MaxChecksPerJob {
		for _, proxy := range proxies[profile.MaxChecksPerJob:] {
			h.recordProxyRiskScoringSkipped(ctx, job, profile, proxy, "超过单次任务检测上限")
		}
		proxies = proxies[:profile.MaxChecksPerJob]
	}
	latest, _ := h.db.ListLatestProxyRiskScores(ctx, proxyIDsFromRows(proxies))
	client := newProxyRiskScoringClient(profile)
	sem := make(chan struct{}, profile.Concurrency)
	var wg sync.WaitGroup
	for _, proxy := range proxies {
		if ctx.Err() != nil {
			break
		}
		proxy := proxy
		if cached := latest[proxy.ID]; cached != nil && !force && cached.ExpiresAt != nil && cached.ExpiresAt.After(time.Now()) {
			cachedCopy := *cached
			h.updateProxyRiskScoringJob(job.ID, func(current *proxyRiskScoringJob) {
				current.Done++
				current.CacheHits++
				current.appendItem(proxyRiskScoringJobItem{ProxyID: proxy.ID, Label: proxyRiskScoringLabel(proxy), Status: "cached", Snapshot: &cachedCopy})
			})
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			h.scoreOneProxyRisk(ctx, job, profile, client, proxy)
		}()
		if profile.RequestDelayMS > 0 {
			timer := time.NewTimer(time.Duration(profile.RequestDelayMS) * time.Millisecond)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
			}
		}
	}
	wg.Wait()
}

func proxyIDsFromRows(rows []*database.ProxyRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			ids = append(ids, row.ID)
		}
	}
	return ids
}

func (h *Handler) scoreOneProxyRisk(ctx context.Context, job *proxyRiskScoringJob, profile database.ProxyRiskScoringProfile, client *proxyRiskScoringClient, proxy *database.ProxyRow) {
	if proxy == nil {
		return
	}
	ip, err := resolveProxyRiskScoringIP(ctx, proxy.URL, profile.ResolveHostnames, profile.AllowPrivateTarget, nil)
	if err != nil {
		h.recordProxyRiskScoringSkipped(ctx, job, profile, proxy, err.Error())
		return
	}
	if profile.CreditReserve > 0 && profile.CreditsRemaining != nil && *profile.CreditsRemaining <= profile.CreditReserve {
		h.recordProxyRiskScoringSkipped(ctx, job, profile, proxy, "评分服务剩余额度低于保护阈值")
		return
	}
	allowed, _, err := h.db.ReserveProxyRiskScoringCheck(ctx, profile.ID, time.Now())
	if err != nil {
		h.recordProxyRiskScoringSkipped(ctx, job, profile, proxy, "无法登记本地评分次数: "+err.Error())
		return
	}
	if !allowed {
		h.recordProxyRiskScoringSkipped(ctx, job, profile, proxy, "达到每日评分次数上限")
		return
	}
	label := proxyRiskScoringLabel(proxy)
	h.updateProxyRiskScoringJob(job.ID, func(current *proxyRiskScoringJob) { current.Current = label })
	snapshot, credits, checkErr := client.checkIP(ctx, ip)
	snapshot.ProxyID = proxy.ID
	snapshot.ProfileID = profile.ID
	if checkErr != nil {
		snapshot.Status = database.ProxyRiskScoringStatusError()
		snapshot.Error = checkErr.Error()
	}
	if credits != nil {
		_ = h.db.UpdateProxyRiskScoringQuota(context.Background(), profile.ID, credits.Remaining, credits.Used, credits.ResetAt, snapshot.Error)
	}
	if snapshot.ExpiresAt == nil {
		expires := time.Now().UTC().Add(time.Duration(profile.CacheTTLSeconds) * time.Second)
		snapshot.ExpiresAt = &expires
	}
	_ = h.db.InsertProxyRiskScoreSnapshot(context.WithoutCancel(ctx), &snapshot)
	snapshotCopy := snapshot
	h.updateProxyRiskScoringJob(job.ID, func(current *proxyRiskScoringJob) {
		current.Done++
		item := proxyRiskScoringJobItem{ProxyID: proxy.ID, Label: label, Status: "success", Snapshot: &snapshotCopy}
		if checkErr != nil {
			current.Failed++
			item.Status = "error"
			item.Error = checkErr.Error()
		} else {
			current.Success++
		}
		current.appendItem(item)
		if current.Current == label {
			current.Current = ""
		}
	})
}

func (h *Handler) recordProxyRiskScoringSkipped(ctx context.Context, job *proxyRiskScoringJob, profile database.ProxyRiskScoringProfile, proxy *database.ProxyRow, reason string) {
	if proxy != nil {
		snapshot := &database.ProxyRiskScoreSnapshot{ProxyID: proxy.ID, ProfileID: profile.ID, Provider: profile.Provider, Status: database.ProxyRiskScoringStatusSkipped(), Error: reason, CheckedAt: time.Now().UTC()}
		expires := snapshot.CheckedAt.Add(time.Duration(profile.CacheTTLSeconds) * time.Second)
		snapshot.ExpiresAt = &expires
		_ = h.db.InsertProxyRiskScoreSnapshot(context.WithoutCancel(ctx), snapshot)
	}
	h.updateProxyRiskScoringJob(job.ID, func(current *proxyRiskScoringJob) {
		current.Done++
		current.Skipped++
		current.appendItem(proxyRiskScoringJobItem{ProxyID: proxy.ID, Label: proxyRiskScoringLabel(proxy), Status: "skipped", Error: reason})
	})
}

func (h *Handler) GetProxyRiskScoringJob(c *gin.Context) {
	job := h.getProxyRiskScoringJob(strings.TrimSpace(c.Param("job_id")))
	if job == nil {
		writeError(c, http.StatusNotFound, "评分任务不存在或已过期")
		return
	}
	after, _ := strconv.Atoi(strings.TrimSpace(c.Query("after")))
	c.JSON(http.StatusOK, job.snapshotAfter(after))
}

func (h *Handler) CancelProxyRiskScoringJob(c *gin.Context) {
	job := h.getProxyRiskScoringJob(strings.TrimSpace(c.Param("job_id")))
	if job == nil {
		writeError(c, http.StatusNotFound, "评分任务不存在或已过期")
		return
	}
	job.mu.RLock()
	cancel := job.cancel
	status := job.Status
	job.mu.RUnlock()
	if cancel != nil && status != "completed" && status != "cancelled" {
		cancel()
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "已请求取消评分任务"})
}

func (h *Handler) TestProxyRiskScoringProfile(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("profile_id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "评分服务档案 ID 无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	profile, err := h.db.GetProxyRiskScoringProfile(ctx, id)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	client := newProxyRiskScoringClient(*profile)
	result, credits, err := client.testConnection(ctx)
	remaining, used, resetAt := profile.CreditsRemaining, profile.CreditsUsed, profile.CreditResetAt
	if credits != nil {
		remaining, used, resetAt = credits.Remaining, credits.Used, credits.ResetAt
	}
	_ = h.db.UpdateProxyRiskScoringQuota(context.Background(), id, remaining, used, resetAt, errorString(err))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "error": err.Error(), "latency_ms": result.LatencyMS})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "latency_ms": result.LatencyMS, "score": result.Score, "risk_level": result.RiskLevel, "credits_remaining": resultCreditsRemaining(credits), "snapshot": result, "message": "内置 Scamalytics v3 评分引擎连接正常"})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func resultCreditsRemaining(credits *proxyRiskCredits) *int64 {
	if credits == nil {
		return nil
	}
	return credits.Remaining
}

func (h *Handler) GetProxyRiskScore(c *gin.Context) {
	proxyID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || proxyID <= 0 {
		writeError(c, http.StatusBadRequest, "代理 ID 无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	scores, err := h.db.ListLatestProxyRiskScores(ctx, []int64{proxyID})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if score := scores[proxyID]; score != nil {
		c.JSON(http.StatusOK, score)
		return
	}
	c.JSON(http.StatusOK, gin.H{"score": nil, "status": "unscored"})
}

func (h *Handler) ListProxyRiskScoreHistory(c *gin.Context) {
	proxyID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || proxyID <= 0 {
		writeError(c, http.StatusBadRequest, "代理 ID 无效")
		return
	}
	profileID, _ := strconv.ParseInt(strings.TrimSpace(c.Query("profile_id")), 10, 64)
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	if profileID <= 0 {
		writeError(c, http.StatusBadRequest, "profile_id 必须有效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	items, total, err := h.db.ListProxyRiskScoreHistory(ctx, proxyID, profileID, page, pageSize)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}
