package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Grok 媒体(生图/生视频)直投官方 REST 端点:
//   POST /images/generations|edits           (同步,响应即产物)
//   POST /videos/generations|edits|extensions (异步,返回 request_id)
//   GET  /videos/{id}                         (客户端轮询状态)
//   GET  /videos/{id}/content                 (产物下载,经网关代理)
//
// 上游有两套 profile:OAuth 账号先走 CLI 网关(cli-chat-proxy),媒体端点不可用
// (403/404/413 等)时回落 xAI 公开 API;API Key 账号直接走 xAI 公开 API。
// 两套 profile 的差异:视频 1.5 模型名带不带 -preview 后缀、图片引用字段名
// (image_url vs url)。
//
// 视频任务是账号亲和的:创建成功后把 request_id → 账号/profile 绑定写入运行态
// 缓存(Redis 部署跨实例、跨重启有效),后续状态/下载必须命中同一账号,否则上游
// 查不到任务。产物按会话选择的模式透传:图片原样透传上游 JSON(data[].url /
// b64_json),视频 done 状态里的上游签名 URL 重写为本网关的 /content 代理地址。

const (
	grokMediaProfileCLI = "cli"
	grokMediaProfileXAI = "xai"

	// maxGrokMediaAttempts caps ordinary finite media retries. A deliberately
	// selected continuous retry bypasses it until the client disconnects.
	maxGrokMediaAttempts = 3

	// grokMaxImageEditInputs Grok 图片编辑上游最多接受的源图数。
	grokMaxImageEditInputs = 3

	grokImagineAliasModel        = "grok-imagine"
	grokImagineImageQualityModel = "grok-imagine-image-quality"
	grokImagineVideoModel        = "grok-imagine-video"
	grokImagineVideo15Model      = "grok-imagine-video-1.5"
	grokImagineVideo15Preview    = "grok-imagine-video-1.5-preview"
	grokDefaultVideoModel        = grokImagineVideo15Model

	grokVideoBindingNamespace = "grok_video_bind"
	// grokVideoBindingTTL 与上游任务的可查询窗口对齐(过期后上游返回 expired)。
	grokVideoBindingTTL = 24 * time.Hour

	grokMediaErrorBodyLimit  = 1 << 20
	grokVideoStatusBodyLimit = 4 << 20
	// grokImagesResponseLimit 生图响应体上限:b64_json 形态下多张 2k 图可达数十 MB。
	grokImagesResponseLimit = 128 << 20
)

// ==================== 模型分类与准入 ====================

func isGrokImageModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == grokImagineAliasModel || strings.HasPrefix(model, "grok-imagine-image")
}

func isGrokVideoModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "grok-imagine-video")
}

func isGrokMediaModel(model string) bool {
	return isGrokImageModel(model) || isGrokVideoModel(model)
}

// isMediaOnlyModel 判断模型是否只能走媒体端点(images/videos),不接受文本请求。
func isMediaOnlyModel(model string) bool {
	return isImageOnlyModel(model) || isGrokMediaModel(model)
}

// normalizeGrokMediaModel 把裸别名 grok-imagine 归一到质量档生图模型。
func normalizeGrokMediaModel(model string) string {
	if strings.EqualFold(strings.TrimSpace(model), grokImagineAliasModel) {
		return grokImagineImageQualityModel
	}
	return strings.TrimSpace(model)
}

// grokMediaModelsForAccount 返回账号可服务的媒体模型集。媒体是与文本目录独立的
// 能力轴:账号导入/同步会把文本目录写进 models 白名单,而媒体模型永远不在上游
// /models 目录里,若把"白名单没有媒体条目"当成关闭,所有存量账号都会被误关。
// 因此白名单只有显式出现 grok-imagine 条目时才对媒体收窄;纯文本白名单不影响
// 媒体能力,取默认集并并入账号目录里出现的媒体条目(默认集即候选全集)。
func grokMediaModelsForAccount(account *auth.Account) []string {
	if account == nil || !account.IsGrokAPI() {
		return nil
	}
	declared := make([]string, 0)
	for _, model := range account.GrokModels() {
		if isGrokMediaModel(model) {
			declared = append(declared, normalizeGrokMediaModel(model))
		}
	}
	if len(declared) > 0 {
		return auth.NormalizeAccountModels(declared)
	}
	result := append([]string{}, auth.GrokImageDefaultModelIDs()...)
	result = append(result, auth.GrokVideoDefaultModelIDs()...)
	for _, item := range account.GrokCatalogModels() {
		if !item.Hidden && isGrokMediaModel(item.ModelID) {
			result = append(result, item.ModelID)
		}
	}
	return auth.NormalizeAccountModels(result)
}

func grokMediaAccountSupportsModel(account *auth.Account, model string) bool {
	if account == nil || !account.IsGrokAPI() || strings.TrimSpace(model) == "" {
		return false
	}
	if !account.GrokDispatchHardAllowed(time.Now()) {
		return false
	}
	return modelIDInList(model, grokMediaModelsForAccount(account))
}

// grokMediaAccountFilter 媒体请求的账号过滤器:仅 Grok 账号,账号级模型映射先行,
// 模型级冷却生效。
func grokMediaAccountFilter(model string) auth.AccountFilter {
	model = strings.TrimSpace(model)
	return func(account *auth.Account) bool {
		if account == nil || !account.IsGrokAPI() {
			return false
		}
		routedModel := model
		if mapped, ok := resolveAccountModelMapping(account, model); ok && mapped != "" {
			routedModel = normalizeGrokMediaModel(mapped)
		}
		if routedModel != "" && account.IsModelRateLimited(routedModel) {
			return false
		}
		return grokMediaAccountSupportsModel(account, routedModel)
	}
}

// grokMediaChannelBlocked 判断当前 Key 的上游渠道限定是否排除 Grok 媒体能力。
func grokMediaChannelBlocked(c *gin.Context) bool {
	return requestUpstreamChannel(c) == database.UpstreamChannelCodex
}

func sendGrokMediaChannelBlockedError(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{"error": gin.H{
		"message": "grok-imagine models are not available for this API key (upstream channel is limited to codex)",
		"type":    "invalid_request_error",
	}})
}

// grokMediaPreferredAccountFilter 在媒体过滤器上再收一层:优先付费凭据。
// free 计划的 OAuth 号打媒体端点必 403,先挑付费号能避免把重试额度和 6h 模型
// 冷却烧在注定失败的账号上;API Key 账号按量计费视同付费。
func grokMediaPreferredAccountFilter(model string) auth.AccountFilter {
	base := grokMediaAccountFilter(model)
	return func(account *auth.Account) bool {
		if !base(account) {
			return false
		}
		if account.GrokAuthKind() == auth.GrokAuthKindAPIKey {
			return true
		}
		plan := strings.ToLower(strings.TrimSpace(account.GetPlanType()))
		return plan != "" && plan != "free"
	}
}

// nextGrokMediaAccount 两层选号:先付费凭据,挑不到再放开到全部候选
// (与生图路径的 plus 优先层级同构)。两层都过 scope 预算闸门。
func (h *Handler) nextGrokMediaAccount(c *gin.Context, apiKeyID int64, exclude map[int64]bool, model string, identity requestSessionIdentity) (*auth.Account, string) {
	preferred := applyAffinityGroupRouting(c, identity, h.withModelCooldownFilter(model, grokMediaPreferredAccountFilter(model)))
	if account, stickyProxyURL := h.nextAccountForSessionWithFilter("", apiKeyID, exclude, h.applyScopeBudgetFilter(c, preferred)); account != nil {
		return account, stickyProxyURL
	}
	fallback := applyAffinityGroupRouting(c, identity, h.withModelCooldownFilter(model, grokMediaAccountFilter(model)))
	return h.nextAccountForSessionWithFilter("", apiKeyID, exclude, h.applyScopeBudgetFilter(c, fallback))
}

// ==================== 上游 profile 与请求投递 ====================

type grokMediaProfile struct {
	Kind    string
	BaseURL string
}

// grokMediaProfilesForAccount 按凭据形态返回有序的上游候选:OAuth 先 CLI 网关
// 再回落 xAI 公开 API(OAuth Bearer 在两边都有效);API Key 只有 xAI 公开 API。
func grokMediaProfilesForAccount(account *auth.Account) []grokMediaProfile {
	baseURL, bearer := account.GrokCredentials()
	if baseURL == "" || bearer == "" {
		return nil
	}
	if account.GrokAuthKind() == auth.GrokAuthKindAPIKey {
		return []grokMediaProfile{{Kind: grokMediaProfileXAI, BaseURL: baseURL}}
	}
	profiles := []grokMediaProfile{{Kind: grokMediaProfileCLI, BaseURL: baseURL}}
	if !strings.EqualFold(strings.TrimRight(baseURL, "/"), strings.TrimRight(auth.GrokDefaultAPIBaseURL, "/")) {
		profiles = append(profiles, grokMediaProfile{Kind: grokMediaProfileXAI, BaseURL: auth.GrokDefaultAPIBaseURL})
	}
	return profiles
}

// grokMediaProfileForKind 取指定 kind 的 profile;绑定的 profile 已不可用(如账号
// 凭据形态变化)时回退第一个可用 profile。
func grokMediaProfileForKind(account *auth.Account, kind string) (grokMediaProfile, bool) {
	profiles := grokMediaProfilesForAccount(account)
	for _, profile := range profiles {
		if profile.Kind == kind {
			return profile, true
		}
	}
	if len(profiles) > 0 {
		return profiles[0], true
	}
	return grokMediaProfile{}, false
}

// mapGrokMediaModelForProfile 按上游 profile 归一模型名:grok-imagine-video-1.5
// 在 xAI 公开 API 上以 -preview 后缀发布,CLI 网关则用不带后缀的名字。
func mapGrokMediaModelForProfile(model, kind string) string {
	switch kind {
	case grokMediaProfileXAI:
		if strings.EqualFold(model, grokImagineVideo15Model) {
			return grokImagineVideo15Preview
		}
	default:
		if strings.EqualFold(model, grokImagineVideo15Preview) {
			return grokImagineVideo15Model
		}
	}
	return model
}

// grokMediaShouldFallbackProfile 判断状态码是否值得换下一个上游 profile 重试
// (CLI 网关不认媒体端点或 body 体积超限时回落 xAI 公开 API)。
func grokMediaShouldFallbackProfile(statusCode int) bool {
	switch statusCode {
	case http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge:
		return true
	}
	return false
}

// doGrokMediaRequest 向指定 profile 的媒体端点发一次请求。extra 里的头最后应用
// (用于 Range 透传等逐请求覆盖)。
func doGrokMediaRequest(ctx context.Context, account *auth.Account, profile grokMediaProfile, proxyURL, method, suffix string, body []byte, downstreamHeaders http.Header, model string, extra http.Header) (*http.Response, error) {
	endpoint := auth.OpenAIResponsesEndpoint(profile.BaseURL, suffix)
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, ErrInternalError("创建请求失败", err)
	}
	_, bearer := account.GrokCredentials()
	applyGrokRequestHeaders(req, account, bearer, downstreamHeaders, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, br, deflate")
	if len(body) == 0 {
		req.Header.Del("Content-Type")
	}
	if model != "" {
		req.Header.Set("x-grok-model-override", model)
	}
	// CLI 身份头只发给 CLI 网关。xAI 公开 API 识别到 x-grok-client-* 会按 CLI
	// 客户端的 Zero Data Retention 政策处理(实测报 "Zero Data Retention teams
	// must provide output.upload_url"),媒体请求直接 400。
	if profile.Kind == grokMediaProfileXAI {
		stripGrokCLIIdentityHeaders(req.Header)
	}
	for key, values := range extra {
		if len(values) > 0 {
			req.Header.Set(key, values[0])
		}
	}
	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 Grok 上游失败", err)
	}
	decodeGrokResponseEncoding(resp)
	return resp, nil
}

// stripGrokCLIIdentityHeaders 移除 Grok CLI 专属的身份/会话头,保留 Authorization、
// Content-Type、Accept 等通用头。
func stripGrokCLIIdentityHeaders(header http.Header) {
	for _, key := range []string{
		"x-grok-client-version", "x-grok-client-identifier", "x-grok-client-mode",
		"x-xai-token-auth", "x-authenticateresponse", "x-compaction-at",
		"x-grok-agent-id", "x-grok-session-id", "x-grok-conv-id", "x-grok-req-id",
		"x-grok-turn-idx", "x-grok-model-override", "x-userid", "x-grok-user-id",
		"x-grok-doom-loop-check", "x-compactions-remaining",
	} {
		header.Del(key)
	}
}

type grokMediaSendResult struct {
	Resp    *http.Response
	Profile grokMediaProfile
	Model   string
}

// grokMediaInvalidSuccessSelected handles failures hidden inside HTTP 200:
// truncated bodies, error envelopes, or missing media identifiers. Catch-all
// retries every such upstream failure; selective mode can match transport,
// exact error-code, or context categories from the original body/error.
func grokMediaInvalidSuccessSelected(policy database.ContinuousRetryPolicy, body []byte, readErr error) bool {
	if isExplicitUpstreamCyberPolicy(body) || isExplicitUpstreamCyberPolicyError(readErr) {
		return false
	}
	if readErr != nil {
		return continuousRetryLimitForRequestError(readErr, 0, policy) == -1
	}
	return policy.CatchesAllUpstreamFailures() || continuousRetryHTTPSelected(policy, http.StatusOK, body)
}

// sendGrokMediaWithProfiles 依次尝试账号的上游 profile;每个 profile 的请求体和
// 模型名由 buildBody 按 profile 差异构造。
func sendGrokMediaWithProfiles(ctx context.Context, account *auth.Account, proxyURL, method, suffix string, buildBody func(profile grokMediaProfile) ([]byte, string), downstreamHeaders http.Header) (grokMediaSendResult, error) {
	profiles := grokMediaProfilesForAccount(account)
	if len(profiles) == 0 {
		return grokMediaSendResult{}, ErrNoAvailableAccount()
	}
	for idx, profile := range profiles {
		body, model := buildBody(profile)
		resp, err := executeHTTPWithContinuousRetryKeepalive(ctx, func() (*http.Response, error) {
			return doGrokMediaRequest(ctx, account, profile, proxyURL, method, suffix, body, downstreamHeaders, model, nil)
		})
		if err != nil {
			return grokMediaSendResult{}, err
		}
		if idx < len(profiles)-1 && grokMediaShouldFallbackProfile(resp.StatusCode) {
			_, _ = readAllWithContinuousRetryKeepalive(ctx, io.LimitReader(resp.Body, grokMediaErrorBodyLimit))
			_ = resp.Body.Close()
			continue
		}
		return grokMediaSendResult{Resp: resp, Profile: profile, Model: model}, nil
	}
	return grokMediaSendResult{}, ErrNoAvailableAccount()
}

// applyGrokMediaCooldown 媒体路径的上游错误 → 冷却映射。与文本路径不同,媒体
// 能力的失败尽量只做模型级隔离,不牵连同账号的文本调度;401 例外(凭据问题
// 两边一致),交给通用映射走 token 强刷。
func applyGrokMediaCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	if store == nil || account == nil || strings.TrimSpace(model) == "" {
		return codex429Decision{}
	}
	markModel := func(reason string, d time.Duration) codex429Decision {
		cooldown := store.MarkModelCooldownUntil(account, model, reason, time.Now().Add(d))
		return codex429Decision{Scope: rateLimitScopeModel, Reason: reason, Model: model, ResetAt: cooldown.ResetAt, Cooldown: time.Until(cooldown.ResetAt)}
	}
	if IsGrokFreeQuotaExhaustedError(body) ||
		(IsGrokSpendingLimitError(body) && (statusCode == http.StatusPaymentRequired || statusCode == http.StatusTooManyRequests)) {
		return markModel("usage_limited", 24*time.Hour)
	}
	switch statusCode {
	case http.StatusTooManyRequests:
		cooldown := time.Minute
		if resp != nil {
			if retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After")); retryAfter > 0 {
				cooldown = retryAfter
			}
		}
		if cooldown > 15*time.Minute {
			cooldown = 15 * time.Minute
		}
		return markModel("rate_limited", cooldown)
	case http.StatusUnauthorized:
		return applyGrokCooldown(store, account, statusCode, body, resp, model)
	case http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
		// 凭据被接受但媒体能力不可用(免费号/无 billing/端点未开放):模型级长隔离。
		return markModel("unsupported", 6*time.Hour)
	}
	return codex429Decision{}
}

// ==================== 生图 ====================

type grokImagesParams struct {
	N           int64
	AspectRatio string
	Resolution  string
	Quality     string
}

func grokImagesParamsFromJSON(body []byte) grokImagesParams {
	params := grokImagesParams{
		AspectRatio: strings.TrimSpace(gjson.GetBytes(body, "aspect_ratio").String()),
		Resolution:  strings.TrimSpace(gjson.GetBytes(body, "resolution").String()),
		Quality:     strings.TrimSpace(gjson.GetBytes(body, "quality").String()),
	}
	if v := gjson.GetBytes(body, "n"); v.Exists() && v.Int() > 0 {
		params.N = v.Int()
	}
	// OpenAI 客户端只有 size 语义:未显式给 resolution 时,2048 边长以上映射到 2k 档。
	if params.Resolution == "" {
		if size := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "size").String())); size != "" {
			if parts := strings.SplitN(size, "x", 2); len(parts) == 2 {
				if parseIntField(parts[0], 0) >= 2048 || parseIntField(parts[1], 0) >= 2048 {
					params.Resolution = "2k"
				}
			}
		}
	}
	return params
}

func grokImagesParamsFromForm(c *gin.Context) grokImagesParams {
	params := grokImagesParams{
		AspectRatio: strings.TrimSpace(c.PostForm("aspect_ratio")),
		Resolution:  strings.TrimSpace(c.PostForm("resolution")),
		Quality:     strings.TrimSpace(c.PostForm("quality")),
	}
	if n := parseIntField(c.PostForm("n"), 0); n > 0 {
		params.N = n
	}
	return params
}

// buildGrokImagesBody 构造发往 Grok 媒体上游的生图请求体。上游不接受 size 字段
// (size 语义已在参数提取时折算进 resolution 档位),源图统一放 images 数组。
func buildGrokImagesBody(model, prompt, responseFormat string, params grokImagesParams, images []string) []byte {
	body := []byte(`{}`)
	body, _ = sjson.SetBytes(body, "model", model)
	body, _ = sjson.SetBytes(body, "prompt", prompt)
	if params.N > 0 {
		body, _ = sjson.SetBytes(body, "n", params.N)
	}
	if responseFormat != "" {
		body, _ = sjson.SetBytes(body, "response_format", responseFormat)
	}
	if params.AspectRatio != "" {
		body, _ = sjson.SetBytes(body, "aspect_ratio", params.AspectRatio)
	}
	if params.Resolution != "" {
		body, _ = sjson.SetBytes(body, "resolution", strings.ToLower(params.Resolution))
	}
	if params.Quality != "" {
		body, _ = sjson.SetBytes(body, "quality", params.Quality)
	}
	idx := 0
	for _, image := range images {
		if image = strings.TrimSpace(image); image == "" {
			continue
		}
		prefix := fmt.Sprintf("images.%d", idx)
		body, _ = sjson.SetBytes(body, prefix+".type", "image_url")
		body, _ = sjson.SetBytes(body, prefix+".url", image)
		idx++
	}
	return body
}

// grokImagesUsageLogInfo 从 OpenAI Images 形状的响应(data[].url|b64_json)提取
// 用量维度:计数必有;b64 形态可解出尺寸/字节数,url 形态只有计数与格式。
func grokImagesUsageLogInfo(responseJSON []byte) imageUsageLogInfo {
	var info imageUsageLogInfo
	data := gjson.GetBytes(responseJSON, "data")
	if !data.IsArray() {
		return info
	}
	for _, item := range data.Array() {
		b64 := strings.TrimSpace(item.Get("b64_json").String())
		if b64 == "" && strings.TrimSpace(item.Get("url").String()) == "" {
			continue
		}
		entry := imageUsageLogInfo{Count: 1, Format: imageFormatFromContentType(item.Get("mime_type").String())}
		if b64 != "" {
			if stats, ok := imageStatsFromBase64(b64); ok {
				entry.Bytes = stats.ByteSize
				entry.Width = stats.Width
				entry.Height = stats.Height
				entry.Size = imageActualSize(stats.Width, stats.Height)
			}
		}
		info = mergeImageUsageLogInfo(info, entry)
	}
	return info
}

// forwardGrokImagesRequest 把 /v1/images/* 请求直投 Grok 媒体上游,成功响应按
// OpenAI Images 形状原样透传(data[].url / b64_json 由上游按 response_format 决定)。
func (h *Handler) forwardGrokImagesRequest(c *gin.Context, inboundEndpoint, imageModel, logModel, logEffectiveModel, prompt, responseFormat string, params grokImagesParams, images []string, stream bool) {
	if stream {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: stream is not supported for grok-imagine models", "type": "invalid_request_error"}})
		return
	}
	if grokMediaChannelBlocked(c) {
		sendGrokMediaChannelBlockedError(c)
		return
	}
	if len(images) > grokMaxImageEditInputs {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": fmt.Sprintf("Invalid request: too many input images for grok-imagine models (%d, max %d)", len(images), grokMaxImageEditInputs), "type": "invalid_request_error"}})
		return
	}
	imageModel = normalizeGrokMediaModel(imageModel)
	if strings.TrimSpace(logModel) == "" {
		logModel = imageModel
	}

	apiKeyID := requestAPIKeyID(c)
	identity := resolveRequestSessionIdentity(c.Request.Header, nil)
	defer h.ReleaseAPIKeyScopeConcurrency(c)
	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolOpenAI)
	defer stopRetryDeadline()
	stopRetryKeepalive := installContinuousRetryHTTPInformationalKeepalive(c)
	defer stopRetryKeepalive()
	maxRetries := h.getMaxRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	continuousRetryActive := false

	for attempt := 0; ; attempt++ {
		if attempt >= maxGrokMediaAttempts && !continuousRetryActive {
			break
		}
		if err := c.Request.Context().Err(); err != nil {
			return
		}
		selectAccount := func(exclude map[int64]bool) (*auth.Account, string) {
			return h.nextGrokMediaAccount(c, apiKeyID, exclude, imageModel, identity)
		}
		var account *auth.Account
		var stickyProxyURL string
		if continuousRetryActive {
			account, stickyProxyURL = nextContinuousRetryAccount(c.Request.Context(), retryExclusions, selectAccount, h.store.Release)
		} else {
			account, stickyProxyURL = nextBoundedRetryAccountWithContext(c.Request.Context(), h.store.Release, retryExclusions, selectAccount)
		}
		if account != nil && c.Request.Context().Err() != nil {
			h.store.Release(account)
			if continuousRetryDeadlineExceeded(c.Request.Context()) {
				continuousRetryCommitExpired(c, continuousRetryProtocolOpenAI)
			}
			return
		}
		if account == nil {
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolOpenAI) || c.Request.Context().Err() != nil {
				return
			}
			if lastStatusCode > 0 && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
			return
		}
		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		routedModel := imageModel
		if mapped, ok := resolveAccountModelMapping(account, imageModel); ok && mapped != "" {
			routedModel = normalizeGrokMediaModel(mapped)
		}

		result, reqErr := sendGrokMediaWithProfiles(c.Request.Context(), account, proxyURL, http.MethodPost, inboundEndpoint, func(profile grokMediaProfile) ([]byte, string) {
			model := mapGrokMediaModelForProfile(routedModel, profile.Kind)
			return buildGrokImagesBody(model, prompt, responseFormat, params, images), model
		}, c.Request.Header.Clone())
		durationMs := int(time.Since(start).Milliseconds())
		if reqErr != nil {
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if !retryable {
				ErrorToGinResponse(c, reqErr)
				return
			}
			continuousSelected := continuousRetryLimitForRequestError(reqErr, 0, continuousRetryPolicy) == -1
			retryLimit := continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)
			shouldRetry := retryAllowedByEndpointCap(attempt, maxGrokMediaAttempts, continuousSelected) && shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy)
			if shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
				continuousRetryActive = continuousRetryActive || continuousSelected
				if retryLimit == -1 && !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, retryLimit) {
					return
				}
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}
		resp := result.Resp
		recordGrokUpstreamObservations(account, resp.Header)

		if resp.StatusCode != http.StatusOK {
			errBody, _ := readAllWithContinuousRetryKeepalive(c.Request.Context(), io.LimitReader(resp.Body, grokMediaErrorBodyLimit))
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			resp.Body.Close()
			if continuousRetryCommitExpired(c, continuousRetryProtocolOpenAI) {
				h.store.Release(account)
				return
			}
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			logUpstreamError(inboundEndpoint, resp.StatusCode, logModel, account.ID(), errBody)
			decision := applyGrokMediaCooldown(h.store, account, resp.StatusCode, errBody, resp, result.Model)
			effectiveRateLimitRetries := h.effectiveMaxRateLimitRetries(account, h.getMaxRateLimitRetries())
			continuousSelected := continuousRetryHTTPSelected(continuousRetryPolicy, resp.StatusCode, errBody)
			shouldRetry := retryAllowedByEndpointCap(attempt, maxGrokMediaAttempts, continuousSelected) && shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousRetryPolicy)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID: account.ID(), Endpoint: inboundEndpoint, Model: logModel, EffectiveModel: logEffectiveModel,
				StatusCode: resp.StatusCode, DurationMs: durationMs,
				InboundEndpoint: inboundEndpoint, UpstreamEndpoint: inboundEndpoint, Stream: false,
				IsRetryAttempt: shouldRetry, AttemptIndex: attempt + 1,
				UpstreamErrorKind: upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:      usageLogErrorMessage(resp.StatusCode, errBody),
			})
			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, effectiveRateLimitRetries, continuousRetryPolicy)
				continuousRetryActive = continuousRetryActive || continuousSelected
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousRetryPolicy)
				if retryLimit == -1 && !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		out, readErr := readAllWithContinuousRetryKeepalive(c.Request.Context(), io.LimitReader(resp.Body, grokImagesResponseLimit))
		resp.Body.Close()
		imageCount := int(gjson.GetBytes(out, "data.#").Int())
		if readErr != nil || !gjson.ValidBytes(out) || imageCount <= 0 {
			// 上游 200 但没有任何产物:按可疑失败换号重试。
			h.store.Release(account)
			if c.Request.Context().Err() != nil {
				return
			}
			continuousSelected := grokMediaInvalidSuccessSelected(continuousRetryPolicy, out, readErr)
			willRetry := retryAllowedByEndpointCap(attempt, maxGrokMediaAttempts, continuousSelected)
			if continuousSelected {
				retryExclusions.MarkTransient(account.ID())
				continuousRetryActive = true
			} else {
				retryExclusions.MarkHard(account.ID())
			}
			errMsg := "upstream returned no image data"
			if readErr != nil {
				errMsg = readErr.Error()
			}
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID: account.ID(), Endpoint: inboundEndpoint, Model: logModel, EffectiveModel: logEffectiveModel,
				StatusCode: http.StatusBadGateway, DurationMs: int(time.Since(start).Milliseconds()),
				InboundEndpoint: inboundEndpoint, UpstreamEndpoint: inboundEndpoint, Stream: false,
				IsRetryAttempt: willRetry, AttemptIndex: attempt + 1,
				UpstreamErrorKind: "empty_response", ErrorMessage: errMsg,
			})
			lastStatusCode = http.StatusBadGateway
			lastBody = []byte(errMsg)
			if willRetry {
				rememberContinuousRetryFailure(c.Request.Context(), continuousRetryFailure{
					status:      lastStatusCode,
					body:        lastBody,
					contentType: "text/plain",
				})
				if continuousSelected && !h.waitBeforeRetryWithBudget(c.Request.Context(), attempt+1, -1) {
					return
				}
				continue
			}
			h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
			return
		}

		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", logModel)

		logInput := &database.UsageLogInput{
			AccountID: account.ID(), Endpoint: inboundEndpoint, Model: logModel, EffectiveModel: logEffectiveModel,
			StatusCode: http.StatusOK, DurationMs: int(time.Since(start).Milliseconds()),
			InboundEndpoint: inboundEndpoint, UpstreamEndpoint: inboundEndpoint, Stream: false,
		}
		logInput.CompletionTokens = imageCount
		logInput.OutputTokens = imageCount
		logInput.TotalTokens = imageCount
		applyImageUsageLogInfo(logInput, grokImagesUsageLogInfo(out))
		if !claimContinuousRetrySuccess(c, continuousRetryProtocolOpenAI) {
			h.store.Release(account)
			return
		}
		h.logUsageForRequest(c, logInput)
		h.store.ClearModelCooldown(account, routedModel)
		h.store.ReportRequestSuccess(account, time.Duration(logInput.DurationMs)*time.Millisecond)
		h.store.Release(account)
		c.Data(http.StatusOK, "application/json", out)
		return
	}
	if lastStatusCode > 0 && len(lastBody) > 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
		return
	}
	c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
}

// ==================== 生视频:任务创建 ====================

// VideosGenerations 处理 POST /v1/videos/generations(文生视频/图生视频)。
func (h *Handler) VideosGenerations(c *gin.Context) { h.grokVideoCreate(c, "generations") }

// VideosEdits 处理 POST /v1/videos/edits(视频编辑)。
func (h *Handler) VideosEdits(c *gin.Context) { h.grokVideoCreate(c, "edits") }

// VideosExtensions 处理 POST /v1/videos/extensions(视频延展)。
func (h *Handler) VideosExtensions(c *gin.Context) { h.grokVideoCreate(c, "extensions") }

// buildGrokVideoBody 从下游请求体白名单拷贝视频字段,并按 profile 归一图片引用
// 的字段名(CLI 网关用 image_url,xAI 公开 API 用 url)。output/storage_options
// 等未支持字段一律丢弃。
func buildGrokVideoBody(rawBody []byte, model, profileKind string) []byte {
	body := []byte(`{}`)
	body, _ = sjson.SetBytes(body, "model", model)
	if prompt := strings.TrimSpace(gjson.GetBytes(rawBody, "prompt").String()); prompt != "" {
		body, _ = sjson.SetBytes(body, "prompt", prompt)
	}
	if v := gjson.GetBytes(rawBody, "duration"); v.Exists() && v.Int() > 0 {
		body, _ = sjson.SetBytes(body, "duration", v.Int())
	}
	for _, field := range []string{"aspect_ratio", "resolution"} {
		if value := strings.TrimSpace(gjson.GetBytes(rawBody, field).String()); value != "" {
			body, _ = sjson.SetBytes(body, field, strings.ToLower(value))
		}
	}
	if ref, ok := grokMediaImageRef(gjson.GetBytes(rawBody, "image"), profileKind); ok {
		body, _ = sjson.SetRawBytes(body, "image", ref)
	}
	if refs := gjson.GetBytes(rawBody, "reference_images"); refs.IsArray() {
		idx := 0
		for _, item := range refs.Array() {
			if ref, ok := grokMediaImageRef(item, profileKind); ok {
				body, _ = sjson.SetRawBytes(body, fmt.Sprintf("reference_images.%d", idx), ref)
				idx++
			}
		}
	}
	if audios := gjson.GetBytes(rawBody, "reference_audios"); audios.IsArray() && len(audios.Array()) > 0 {
		body, _ = sjson.SetRawBytes(body, "reference_audios", []byte(audios.Raw))
	}
	if video := gjson.GetBytes(rawBody, "video"); video.Exists() {
		if ref, ok := grokMediaVideoRef(video); ok {
			body, _ = sjson.SetRawBytes(body, "video", ref)
		}
	}
	return body
}

// grokMediaImageRef 接受字符串或 {url|image_url} 对象,输出 profile 对应字段名的对象。
func grokMediaImageRef(value gjson.Result, profileKind string) ([]byte, bool) {
	target := ""
	switch {
	case value.Type == gjson.String:
		target = strings.TrimSpace(value.String())
	case value.IsObject():
		target = strings.TrimSpace(value.Get("url").String())
		if target == "" {
			target = strings.TrimSpace(value.Get("image_url").String())
		}
	}
	if target == "" {
		return nil, false
	}
	key := "url"
	if profileKind == grokMediaProfileCLI {
		key = "image_url"
	}
	ref, _ := sjson.SetBytes([]byte(`{}`), key, target)
	return ref, true
}

func grokMediaVideoRef(value gjson.Result) ([]byte, bool) {
	target := ""
	switch {
	case value.Type == gjson.String:
		target = strings.TrimSpace(value.String())
	case value.IsObject():
		target = strings.TrimSpace(value.Get("url").String())
	}
	if target == "" {
		return nil, false
	}
	ref, _ := sjson.SetBytes([]byte(`{}`), "url", target)
	return ref, true
}

func (h *Handler) grokVideoCreate(c *gin.Context, operation string) {
	inboundEndpoint := "/v1/videos/" + operation
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	h.capturePromptRequestIngress(c, rawBody)
	if !json.Valid(rawBody) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: body must be valid JSON", "type": "invalid_request_error"}})
		return
	}
	if gjson.GetBytes(rawBody, "stream").Bool() {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: stream is not supported for grok-imagine-video models", "type": "invalid_request_error"}})
		return
	}

	model := normalizeGrokMediaModel(gjson.GetBytes(rawBody, "model").String())
	if model == "" {
		// 上游实测:edits/extensions 只有 grok-imagine-video 支持,1.5 系列会
		// 400 "not supported for this model",默认模型按操作分开。
		if operation == "generations" {
			model = grokDefaultVideoModel
		} else {
			model = grokImagineVideoModel
		}
	}
	requestModel := model
	if mapped, ok := h.resolveConfiguredRequestModel(model, h.supportedModelIDs(c.Request.Context())); ok {
		model = normalizeGrokMediaModel(mapped)
	}
	logEffectiveModel := usageEffectiveModelForMapping(requestModel, model, !strings.EqualFold(requestModel, model))
	if !isGrokVideoModel(model) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": fmt.Sprintf("Invalid request: %s requires a grok-imagine-video model, got %q", inboundEndpoint, model), "type": "invalid_request_error"}})
		return
	}
	if grokMediaChannelBlocked(c) {
		sendGrokMediaChannelBlockedError(c)
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawBody, "prompt").String())
	if operation == "generations" && prompt == "" && !gjson.GetBytes(rawBody, "image").Exists() && !gjson.GetBytes(rawBody, "reference_images").Exists() {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: prompt is required", "type": "invalid_request_error"}})
		return
	}
	if operation != "generations" && !gjson.GetBytes(rawBody, "video").Exists() {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: video is required for " + inboundEndpoint, "type": "invalid_request_error"}})
		return
	}
	if prompt != "" && h.inspectPromptFilterTextOpenAI(c, prompt, inboundEndpoint, model) {
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	apiKeyID := requestAPIKeyID(c)
	identity := resolveRequestSessionIdentity(c.Request.Header, nil)
	defer h.ReleaseAPIKeyScopeConcurrency(c)
	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolOpenAI)
	defer stopRetryDeadline()
	stopRetryKeepalive := installContinuousRetryHTTPInformationalKeepalive(c)
	defer stopRetryKeepalive()
	maxRetries := h.getMaxRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	continuousRetryActive := false

	for attempt := 0; ; attempt++ {
		if attempt >= maxGrokMediaAttempts && !continuousRetryActive {
			break
		}
		if err := c.Request.Context().Err(); err != nil {
			return
		}
		selectAccount := func(exclude map[int64]bool) (*auth.Account, string) {
			return h.nextGrokMediaAccount(c, apiKeyID, exclude, model, identity)
		}
		var account *auth.Account
		var stickyProxyURL string
		if continuousRetryActive {
			account, stickyProxyURL = nextContinuousRetryAccount(c.Request.Context(), retryExclusions, selectAccount, h.store.Release)
		} else {
			account, stickyProxyURL = nextBoundedRetryAccountWithContext(c.Request.Context(), h.store.Release, retryExclusions, selectAccount)
		}
		if account != nil && c.Request.Context().Err() != nil {
			h.store.Release(account)
			if continuousRetryDeadlineExceeded(c.Request.Context()) {
				continuousRetryCommitExpired(c, continuousRetryProtocolOpenAI)
			}
			return
		}
		if account == nil {
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolOpenAI) || c.Request.Context().Err() != nil {
				return
			}
			if lastStatusCode > 0 && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
			return
		}
		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		routedModel := model
		if mapped, ok := resolveAccountModelMapping(account, model); ok && mapped != "" {
			routedModel = normalizeGrokMediaModel(mapped)
		}

		result, reqErr := sendGrokMediaWithProfiles(c.Request.Context(), account, proxyURL, http.MethodPost, inboundEndpoint, func(profile grokMediaProfile) ([]byte, string) {
			m := mapGrokMediaModelForProfile(routedModel, profile.Kind)
			return buildGrokVideoBody(rawBody, m, profile.Kind), m
		}, c.Request.Header.Clone())
		durationMs := int(time.Since(start).Milliseconds())
		if reqErr != nil {
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if !retryable {
				ErrorToGinResponse(c, reqErr)
				return
			}
			continuousSelected := continuousRetryLimitForRequestError(reqErr, 0, continuousRetryPolicy) == -1
			retryLimit := continuousRetryLimitForRequestError(reqErr, maxRetries, continuousRetryPolicy)
			shouldRetry := retryAllowedByEndpointCap(attempt, maxGrokMediaAttempts, continuousSelected) && shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy)
			if shouldRetry {
				rememberContinuousRetryRequestFailure(c.Request.Context(), reqErr)
				retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
				continuousRetryActive = continuousRetryActive || continuousSelected
				if retryLimit == -1 && !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, retryLimit) {
					return
				}
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}
		resp := result.Resp
		recordGrokUpstreamObservations(account, resp.Header)

		if resp.StatusCode != http.StatusOK {
			errBody, _ := readAllWithContinuousRetryKeepalive(c.Request.Context(), io.LimitReader(resp.Body, grokMediaErrorBodyLimit))
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			resp.Body.Close()
			if continuousRetryCommitExpired(c, continuousRetryProtocolOpenAI) {
				h.store.Release(account)
				return
			}
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			logUpstreamError(inboundEndpoint, resp.StatusCode, model, account.ID(), errBody)
			decision := applyGrokMediaCooldown(h.store, account, resp.StatusCode, errBody, resp, result.Model)
			effectiveRateLimitRetries := h.effectiveMaxRateLimitRetries(account, h.getMaxRateLimitRetries())
			continuousSelected := continuousRetryHTTPSelected(continuousRetryPolicy, resp.StatusCode, errBody)
			shouldRetry := retryAllowedByEndpointCap(attempt, maxGrokMediaAttempts, continuousSelected) && shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousRetryPolicy)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID: account.ID(), Endpoint: inboundEndpoint, Model: requestModel, EffectiveModel: logEffectiveModel,
				StatusCode: resp.StatusCode, DurationMs: durationMs,
				InboundEndpoint: inboundEndpoint, UpstreamEndpoint: inboundEndpoint, Stream: false,
				IsRetryAttempt: shouldRetry, AttemptIndex: attempt + 1,
				UpstreamErrorKind: upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:      usageLogErrorMessage(resp.StatusCode, errBody),
			})
			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, effectiveRateLimitRetries, continuousRetryPolicy)
				continuousRetryActive = continuousRetryActive || continuousSelected
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, effectiveRateLimitRetries, continuousRetryPolicy)
				if retryLimit == -1 && !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		out, readErr := readAllWithContinuousRetryKeepalive(c.Request.Context(), io.LimitReader(resp.Body, grokVideoStatusBodyLimit))
		resp.Body.Close()
		requestID := strings.TrimSpace(gjson.GetBytes(out, "request_id").String())
		if requestID == "" {
			requestID = strings.TrimSpace(gjson.GetBytes(out, "id").String())
		}
		if !validGrokVideoRequestID(requestID) {
			h.store.Release(account)
			if c.Request.Context().Err() != nil {
				return
			}
			continuousSelected := grokMediaInvalidSuccessSelected(continuousRetryPolicy, out, readErr)
			willRetry := retryAllowedByEndpointCap(attempt, maxGrokMediaAttempts, continuousSelected)
			if continuousSelected {
				retryExclusions.MarkTransient(account.ID())
				continuousRetryActive = true
			} else {
				retryExclusions.MarkHard(account.ID())
			}
			errMsg := "upstream video create returned no request_id"
			if readErr != nil {
				errMsg = readErr.Error()
			}
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID: account.ID(), Endpoint: inboundEndpoint, Model: requestModel, EffectiveModel: logEffectiveModel,
				StatusCode: http.StatusBadGateway, DurationMs: durationMs,
				InboundEndpoint: inboundEndpoint, UpstreamEndpoint: inboundEndpoint, Stream: false,
				IsRetryAttempt: willRetry, AttemptIndex: attempt + 1,
				UpstreamErrorKind: "empty_response", ErrorMessage: errMsg,
			})
			lastStatusCode = http.StatusBadGateway
			lastBody = []byte(errMsg)
			if willRetry {
				rememberContinuousRetryFailure(c.Request.Context(), continuousRetryFailure{
					status:      lastStatusCode,
					body:        lastBody,
					contentType: "text/plain",
				})
				if continuousSelected && !h.waitBeforeRetryWithBudget(c.Request.Context(), attempt+1, -1) {
					return
				}
				continue
			}
			h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
			return
		}

		if !claimContinuousRetrySuccess(c, continuousRetryProtocolOpenAI) {
			h.store.Release(account)
			return
		}
		h.storeGrokVideoBinding(c.Request.Context(), requestID, grokVideoBinding{
			AccountID: account.ID(),
			APIKeyID:  apiKeyID,
			Profile:   result.Profile.Kind,
			Model:     result.Model,
			CreatedAt: time.Now().Unix(),
		})

		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", requestModel)
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID: account.ID(), Endpoint: inboundEndpoint, Model: requestModel, EffectiveModel: logEffectiveModel,
			StatusCode: http.StatusOK, DurationMs: durationMs,
			InboundEndpoint: inboundEndpoint, UpstreamEndpoint: inboundEndpoint, Stream: false,
		})
		h.store.ClearModelCooldown(account, routedModel)
		h.store.ReportRequestSuccess(account, time.Duration(durationMs)*time.Millisecond)
		h.store.Release(account)
		c.Data(http.StatusOK, "application/json", out)
		return
	}
	if lastStatusCode > 0 && len(lastBody) > 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
		return
	}
	c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
}

// ==================== 生视频:任务绑定 ====================

type grokVideoBinding struct {
	AccountID int64  `json:"account_id"`
	APIKeyID  int64  `json:"api_key_id"`
	Profile   string `json:"profile"`
	Model     string `json:"model"`
	CreatedAt int64  `json:"created_at"`
}

func validGrokVideoRequestID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 200 {
		return false
	}
	for _, r := range id {
		if r <= 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func (h *Handler) storeGrokVideoBinding(ctx context.Context, requestID string, binding grokVideoBinding) {
	if h == nil || h.cache == nil {
		return
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return
	}
	if err := h.cache.SetRuntime(ctx, grokVideoBindingNamespace, requestID, payload, grokVideoBindingTTL); err != nil {
		log.Printf("保存 Grok 视频任务绑定失败 (request_id=%s): %v", requestID, err)
	}
}

func (h *Handler) loadGrokVideoBinding(ctx context.Context, requestID string) (grokVideoBinding, bool) {
	var binding grokVideoBinding
	if h == nil || h.cache == nil {
		return binding, false
	}
	raw, ok, err := h.cache.GetRuntime(ctx, grokVideoBindingNamespace, requestID)
	if err != nil || !ok {
		return binding, false
	}
	if json.Unmarshal(raw, &binding) != nil {
		return binding, false
	}
	return binding, binding.AccountID > 0
}

// resolveGrokVideoBinding 校验 request_id 的归属(必须是创建它的 API Key)并取回
// 绑定账号;失败统一回 404,不泄露任务是否存在。
func (h *Handler) resolveGrokVideoBinding(c *gin.Context, requestID string) (grokVideoBinding, *auth.Account, bool) {
	binding, ok := h.loadGrokVideoBinding(c.Request.Context(), requestID)
	if !ok || binding.APIKeyID != requestAPIKeyID(c) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "Video request not found", "type": "invalid_request_error"}})
		return binding, nil, false
	}
	account := h.store.FindByID(binding.AccountID)
	if account == nil || !account.IsGrokAPI() {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "Video request not found", "type": "invalid_request_error"}})
		return binding, nil, false
	}
	return binding, account, true
}

// ==================== 生视频:状态查询与产物代理 ====================

// VideosStatus 代理 GET /v1/videos/:request_id 状态查询。请求命中创建时绑定的
// 账号;done 状态里的上游资产 URL 重写为本网关的 /content 代理地址(上游签名
// URL 会过期,统一走网关下载)。
func (h *Handler) VideosStatus(c *gin.Context) {
	requestID := strings.TrimSpace(c.Param("request_id"))
	if !validGrokVideoRequestID(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: request_id is invalid", "type": "invalid_request_error"}})
		return
	}
	binding, account, ok := h.resolveGrokVideoBinding(c, requestID)
	if !ok {
		return
	}
	profile, ok := grokMediaProfileForKind(account, binding.Profile)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
		return
	}
	proxyURL := h.resolveProxyForAttempt(account, "")
	resp, err := doGrokMediaRequest(c.Request.Context(), account, profile, proxyURL, http.MethodGet, "/v1/videos/"+url.PathEscape(requestID), nil, c.Request.Header.Clone(), binding.Model, nil)
	if err != nil {
		ErrorToGinResponse(c, err)
		return
	}
	defer resp.Body.Close()
	recordGrokUpstreamObservations(account, resp.Header)
	out, _ := io.ReadAll(io.LimitReader(resp.Body, grokVideoStatusBodyLimit))
	// 任务进行中上游以 202 携带 {"status":"pending","progress":N} 返回,
	// 与 200 一样是合法状态体;统一以 200 透传,轮询客户端只看 body.status。
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		h.sendFinalUpstreamError(c, resp.StatusCode, out)
		return
	}
	out = rewriteGrokVideoContentURL(c, out, requestID)
	c.Data(http.StatusOK, "application/json", out)
}

// rewriteGrokVideoContentURL 把状态响应里的 video.url 换成本网关的 content 代理地址。
func rewriteGrokVideoContentURL(c *gin.Context, body []byte, requestID string) []byte {
	if !gjson.GetBytes(body, "video.url").Exists() {
		return body
	}
	proxyURL := grokVideoContentProxyURL(c, requestID)
	if proxyURL == "" {
		return body
	}
	if updated, err := sjson.SetBytes(body, "video.url", proxyURL); err == nil {
		return updated
	}
	return body
}

func grokVideoContentProxyURL(c *gin.Context, requestID string) string {
	if c == nil || c.Request == nil {
		return ""
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")); forwarded != "" {
		if idx := strings.IndexByte(forwarded, ','); idx >= 0 {
			forwarded = forwarded[:idx]
		}
		if forwarded = strings.TrimSpace(forwarded); forwarded != "" {
			scheme = forwarded
		}
	}
	host := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(c.Request.Host)
	}
	if host == "" {
		return ""
	}
	prefix := "/videos/"
	if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
		prefix = "/v1/videos/"
	}
	return scheme + "://" + host + prefix + url.PathEscape(requestID) + "/content"
}

// VideosContent 代理 GET /v1/videos/:request_id/content 产物下载:先查状态拿上游
// 签名 URL(白名单 host、禁跳转)匿名拉取,拿不到再回退带凭据的上游 content 端点。
// 两条路径都透传 Range。
func (h *Handler) VideosContent(c *gin.Context) {
	requestID := strings.TrimSpace(c.Param("request_id"))
	if !validGrokVideoRequestID(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: request_id is invalid", "type": "invalid_request_error"}})
		return
	}
	binding, account, ok := h.resolveGrokVideoBinding(c, requestID)
	if !ok {
		return
	}
	profile, ok := grokMediaProfileForKind(account, binding.Profile)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
		return
	}
	proxyURL := h.resolveProxyForAttempt(account, "")
	suffix := "/v1/videos/" + url.PathEscape(requestID)

	signedURL := ""
	if resp, err := doGrokMediaRequest(c.Request.Context(), account, profile, proxyURL, http.MethodGet, suffix, nil, c.Request.Header.Clone(), binding.Model, nil); err == nil {
		statusBody, _ := io.ReadAll(io.LimitReader(resp.Body, grokVideoStatusBodyLimit))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			if candidate := strings.TrimSpace(gjson.GetBytes(statusBody, "video.url").String()); isTrustedGrokVideoAssetURL(candidate) {
				signedURL = candidate
			}
		}
	}
	if signedURL != "" && h.streamGrokVideoAsset(c, account, proxyURL, signedURL) {
		return
	}

	rangeHeader := http.Header{}
	if value := strings.TrimSpace(c.GetHeader("Range")); value != "" {
		rangeHeader.Set("Range", value)
	}
	resp, err := doGrokMediaRequest(c.Request.Context(), account, profile, proxyURL, http.MethodGet, suffix+"/content", nil, c.Request.Header.Clone(), binding.Model, rangeHeader)
	if err != nil {
		ErrorToGinResponse(c, err)
		return
	}
	defer resp.Body.Close()
	streamGrokMediaHTTPResponse(c, resp)
}

// isTrustedGrokVideoAssetURL 校验上游签名资产 URL:仅 https、无 userinfo、默认端口,
// host 限官方资产域及其子域。
func isTrustedGrokVideoAssetURL(raw string) bool {
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, allowed := range []string{"vidgen.x.ai", "assets.grok.com", "cdn.x.ai", "videos.x.ai", "imgen.x.ai"} {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

// streamGrokVideoAsset 匿名拉取上游签名资产 URL 并流式转发;返回 false 表示未写出
// 任何响应,调用方可回退带凭据的下载路径。
func (h *Handler) streamGrokVideoAsset(c *gin.Context, account *auth.Account, proxyURL, assetURL string) bool {
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, assetURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "video/*,*/*;q=0.8")
	req.Header.Set("User-Agent", grokUserAgent())
	if rangeHeader := strings.TrimSpace(c.GetHeader("Range")); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	// 浅拷贝共享连接池但独立 CheckRedirect:签名 URL 域外跳转一律拒绝(防 SSRF)。
	client := *getPooledClient(account, proxyURL)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return false
	}
	streamGrokMediaHTTPResponse(c, resp)
	return true
}

func streamGrokMediaHTTPResponse(c *gin.Context, resp *http.Response) {
	for _, headerKey := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Content-Disposition"} {
		if value := resp.Header.Get(headerKey); value != "" {
			c.Header(headerKey, value)
		}
	}
	if c.Writer.Header().Get("Content-Type") == "" {
		c.Header("Content-Type", "video/mp4")
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}
