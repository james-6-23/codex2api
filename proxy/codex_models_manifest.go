package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// CodexModelsManifestHandler 向 Codex 客户端提供模型清单。
//
// 有可调度的 ChatGPT OAuth 账号时，凭据实时转发官方清单（响应体与 ETag 原样透传）。
// Antigravity / 仅中转号池没有 ChatGPT 账号：把与 Cockpit 相同的 /v1/models
// 目录改写成 Codex 期望的 {"models":[{"slug":...}]}，避免客户端 503 后静默
// 冻结在本地缓存（选单不全、模型信息联不通）。
func (h *Handler) CodexModelsManifestHandler(c *gin.Context) {
	row := apiKeyRowFromContext(c)
	if h.preferScopedCodexManifest(c) {
		if !h.serveScopedCodexManifest(c, row) {
			api.SendError(c, api.ErrServiceUnavailable)
		}
		return
	}
	account := h.scopedCodexManifestAccount(row)
	if account == nil {
		if !h.serveScopedCodexManifest(c, row) {
			api.SendError(c, api.ErrServiceUnavailable)
		}
		return
	}
	restrictManifest := codexManifestNeedsFiltering(row, account)
	extraModels := h.extraRelayManifestModels(c.Request.Context(), row)
	ifNoneMatch := c.GetHeader("If-None-Match")
	if restrictManifest || len(extraModels) > 0 {
		// Restricted responses use a gateway ETag derived from the filtered
		// or locally merged representation. It is not an upstream validator, and
		// forwarding it can produce a body-less 304 that cannot be rebuilt safely.
		ifNoneMatch = ""
	}

	manifest, err := FetchCodexModelsManifest(
		c.Request.Context(),
		account,
		h.store.ResolveProxyForAccount(account),
		c.Query("client_version"),
		ifNoneMatch,
	)
	if err != nil {
		if h.serveScopedCodexManifest(c, row) {
			return
		}
		api.SendErrorWithStatus(c,
			api.NewAPIError(api.ErrCodeUpstreamError, fmt.Sprintf("codex models manifest: %v", err), api.ErrorTypeUpstream),
			http.StatusBadGateway)
		return
	}

	if restrictManifest {
		if manifest.NotModified {
			api.SendErrorWithStatus(c,
				api.NewAPIError(api.ErrCodeUpstreamError, "codex models manifest returned an unusable 304", api.ErrorTypeUpstream),
				http.StatusBadGateway)
			return
		}
		body, _, filterErr := filterCodexManifest(manifest.Body, manifest.ETag, func(slug string) bool {
			return codexManifestModelAllowed(row, account, slug)
		})
		if filterErr != nil {
			api.SendErrorWithStatus(c,
				api.NewAPIError(api.ErrCodeUpstreamError, fmt.Sprintf("codex models manifest: %v", filterErr), api.ErrorTypeUpstream),
				http.StatusBadGateway)
			return
		}
		h.learnManifestModelsAsync(manifest.Body)
		h.writeMergedCodexManifest(c, body, "", extraModels)
		return
	}

	if manifest.NotModified {
		if manifest.ETag != "" {
			c.Header("ETag", manifest.ETag)
		}
		c.Status(http.StatusNotModified)
		return
	}
	// 顺手把清单里注册表不认识的新模型学习进注册表（只增不改不删），
	// 让选单里出现的新模型立即通过请求侧模型校验，无需等手动同步。
	h.learnManifestModelsAsync(manifest.Body)
	h.writeMergedCodexManifest(c, manifest.Body, manifest.ETag, extraModels)
}

func (h *Handler) preferScopedCodexManifest(c *gin.Context) bool {
	return requestUpstreamChannel(c) == database.UpstreamChannelAntigravity
}

func (h *Handler) serveScopedCodexManifest(c *gin.Context, row *database.APIKeyRow) bool {
	if h == nil || c == nil || row == nil {
		return false
	}
	models := h.scopedModels(c.Request.Context(), row)
	if len(models) == 0 {
		return false
	}
	body, err := buildScopedCodexManifest(models)
	if err != nil {
		log.Printf("build scoped Codex manifest: %v", err)
		return false
	}
	h.writeCodexManifest(c, body, "")
	return true
}

func (h *Handler) writeMergedCodexManifest(c *gin.Context, body []byte, upstreamETag string, extras []api.Model) {
	merged := body
	if len(extras) > 0 {
		next, err := mergeCodexManifestModels(body, extras)
		if err != nil {
			log.Printf("merge relay models into Codex manifest: %v", err)
		} else {
			merged = next
		}
	}
	etag := upstreamETag
	if etag == "" || string(merged) != string(body) {
		etag = ""
	}
	h.writeCodexManifest(c, merged, etag)
}

func (h *Handler) extraRelayManifestModels(ctx context.Context, row *database.APIKeyRow) []api.Model {
	if h == nil || row == nil {
		return nil
	}
	records := h.scopedModelRecords(ctx, row)
	extras := make([]api.Model, 0, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		if record.backing&(modelBackingRelay|modelBackingGrok|modelBackingAntigravity) == 0 {
			continue
		}
		extras = append(extras, api.Model{ID: record.id, Object: "model", OwnedBy: scopedModelOwner(record)})
	}
	sort.Slice(extras, func(i, j int) bool {
		return strings.ToLower(extras[i].ID) < strings.ToLower(extras[j].ID)
	})
	return extras
}

func (h *Handler) writeCodexManifest(c *gin.Context, body []byte, etag string) {
	if etag == "" {
		etag = scopedCodexManifestETag(body)
	}
	c.Header("ETag", etag)
	if etagHeaderMatches(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json", body)
}

type scopedCodexManifestItem struct {
	Slug                     string   `json:"slug"`
	DisplayName              string   `json:"display_name"`
	Hidden                   bool     `json:"hidden"`
	Availability             string   `json:"availability"`
	SupportedInAPI           bool     `json:"supported_in_api"`
	PreferWebsockets         bool     `json:"prefer_websockets"`
	UseResponsesLite         bool     `json:"use_responses_lite"`
	InputModalities          []string `json:"input_modalities,omitempty"`
	SupportedReasoningLevels []string `json:"supported_reasoning_levels,omitempty"`
}

func buildScopedCodexManifest(models []api.Model) ([]byte, error) {
	items := make([]scopedCodexManifestItem, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		slug := strings.TrimSpace(model.ID)
		if slug == "" {
			continue
		}
		key := strings.ToLower(slug)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		item := scopedCodexManifestItem{
			Slug:             slug,
			DisplayName:      slug,
			Availability:     "available",
			SupportedInAPI:   true,
			PreferWebsockets: false,
			UseResponsesLite: false,
			InputModalities:  []string{"text"},
		}
		if strings.Contains(key, "image") {
			item.InputModalities = []string{"text", "image"}
		}
		// Antigravity's reasoning metadata is provider-specific. Never infer
		// levels from names such as "thinking" or "reason": Claude Opus
		// `*-thinking` is not a Gemini reasoning-control model.
		if _, isAntigravityModel := antigravityPublicModel(slug); isAntigravityModel {
			item.SupportedReasoningLevels = antigravityCodexReasoningLevels(slug)
		} else if !strings.EqualFold(strings.TrimSpace(model.OwnedBy), "google") && (strings.Contains(key, "thinking") || strings.Contains(key, "reason")) {
			item.SupportedReasoningLevels = []string{"low", "medium", "high"}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Slug) < strings.ToLower(items[j].Slug)
	})
	return json.Marshal(struct {
		Models []scopedCodexManifestItem `json:"models"`
	}{Models: items})
}

func mergeCodexManifestModels(body []byte, extras []api.Model) ([]byte, error) {
	if len(extras) == 0 {
		return body, nil
	}
	extraBody, err := buildScopedCodexManifest(extras)
	if err != nil {
		return nil, err
	}
	var extraRoot struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(extraBody, &extraRoot); err != nil {
		return nil, err
	}
	if len(extraRoot.Models) == 0 {
		return body, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	rawModels, ok := root["models"]
	if !ok {
		return nil, fmt.Errorf("unsupported codex manifest schema: models is missing")
	}
	var models []json.RawMessage
	if err := json.Unmarshal(rawModels, &models); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(models)+len(extraRoot.Models))
	for _, raw := range models {
		var item struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if slug := strings.ToLower(strings.TrimSpace(item.Slug)); slug != "" {
			seen[slug] = struct{}{}
		}
	}
	for _, raw := range extraRoot.Models {
		var item struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		slug := strings.ToLower(strings.TrimSpace(item.Slug))
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		models = append(models, raw)
	}
	encoded, err := json.Marshal(models)
	if err != nil {
		return nil, err
	}
	root["models"] = encoded
	return json.Marshal(root)
}

func scopedCodexManifestETag(body []byte) string {
	sum := sha256.Sum256(append([]byte("codex2api-scoped-manifest-v1\x00"), body...))
	return `"codex2api-` + hex.EncodeToString(sum[:]) + `"`
}

// manifestLearnKnown 缓存已确认在注册表里的模型 slug（小写），避免客户端每次
// 刷新选单都对注册表做一次 DB 差集。带 TTL 是为了让管理端手动删除的行能被重新学习。
var manifestLearnKnown = struct {
	sync.Mutex
	slugs   map[string]struct{}
	expires time.Time
}{}

const manifestLearnCacheTTL = 10 * time.Minute

// learnManifestModelsAsync 判断清单里是否有缓存未见过的 slug，有则后台学习。
// 学习失败只记日志，绝不影响清单透传本身。
func (h *Handler) learnManifestModelsAsync(manifestBody []byte) {
	if len(manifestBody) == 0 {
		return
	}
	// lite 能力学习不依赖 DB：清单每次透传都刷新 use_responses_lite 真值，
	// 供发出前剥离"非 lite 模型 + lite 信号"的必死组合。
	RecordResponsesLiteSupportFromManifest(manifestBody)
	if h == nil || h.db == nil {
		return
	}
	slugs := ExtractManifestModelSlugs(manifestBody)
	if len(slugs) == 0 {
		return
	}

	now := time.Now()
	manifestLearnKnown.Lock()
	if manifestLearnKnown.slugs == nil || now.After(manifestLearnKnown.expires) {
		manifestLearnKnown.slugs = make(map[string]struct{})
		manifestLearnKnown.expires = now.Add(manifestLearnCacheTTL)
	}
	fresh := false
	for _, slug := range slugs {
		if _, ok := manifestLearnKnown.slugs[strings.ToLower(slug)]; !ok {
			fresh = true
			break
		}
	}
	manifestLearnKnown.Unlock()
	if !fresh {
		return
	}

	body := append([]byte(nil), manifestBody...)
	h.db.RunBackgroundTask(func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		defer cancel()
		added, err := LearnModelsFromManifest(ctx, h.db, body, time.Now().UTC())
		if err != nil {
			log.Printf("模型清单学习失败（不影响透传）: %v", err)
			return
		}
		// 学习成功（或全部已知）后，本轮清单的 slug 全部记入缓存。
		manifestLearnKnown.Lock()
		for _, slug := range ExtractManifestModelSlugs(body) {
			manifestLearnKnown.slugs[strings.ToLower(slug)] = struct{}{}
		}
		manifestLearnKnown.Unlock()
		if len(added) > 0 {
			log.Printf("已从上游模型清单学习 %d 个新模型进注册表: %s", len(added), strings.Join(added, ", "))
		}
	})
}

// CodexModelsManifestURL 是 ChatGPT 后端的 Codex 模型清单端点。
// Codex CLI / Codex App 从 provider 的 GET {base_url}/models?client_version=...
// （自定义 provider 模式）或 GET /backend-api/codex/models（chatgpt_base_url 模式）
// 刷新模型选单，期望 manifest 格式（{"models":[{slug,...}]}）而非 OpenAI 兼容列表；
// 解析失败时客户端会静默回落本地缓存，模型选单从此冻结、新模型永远不出现。
const CodexModelsManifestURL = "https://chatgpt.com/backend-api/codex/models"

// codexModelsManifestURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var codexModelsManifestURLForTest = ""

// CodexModelsManifest 承载上游清单原文与缓存元数据，供 handler 原样透传给客户端。
type CodexModelsManifest struct {
	Body        []byte
	ETag        string
	NotModified bool
}

// FetchCodexModelsManifest 用账号凭据向 ChatGPT 后端实时拉取 Codex 模型清单。
//
// 响应体原样透传，不在本地解析或维护清单：manifest schema 随 Codex 客户端版本
// 演进，透传使网关无需跟进 schema 变化，且返回的始终是账号真实的模型权限
// （区别于内置模型注册表的"理论列表"）。
func FetchCodexModelsManifest(ctx context.Context, account *auth.Account, proxyURL, clientVersion, ifNoneMatch string) (*CodexModelsManifest, error) {
	endpoint := CodexModelsManifestURL
	if codexModelsManifestURLForTest != "" {
		endpoint = codexModelsManifestURLForTest
	}
	return fetchCodexModelsManifestWithURL(ctx, account, proxyURL, endpoint, clientVersion, ifNoneMatch)
}

func fetchCodexModelsManifestWithURL(ctx context.Context, account *auth.Account, proxyURL, endpoint, clientVersion, ifNoneMatch string) (*CodexModelsManifest, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("account has no access token")
	}

	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		clientVersion = effectiveLatestCodexCLIVersion()
	}
	requestURL := endpoint + "?client_version=" + url.QueryEscape(clientVersion)

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build codex models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	// UA 版本段与 Version 头、client_version query 三者保持同一版本，
	// 避免出站身份自相矛盾（UA 钉内置常量、Version 跟随同步值）。
	req.Header.Set("User-Agent", replaceCodexUserAgentVersion(defaultCodexCLIUserAgent, clientVersion))
	req.Header.Set("Originator", Originator)
	req.Header.Set("Version", clientVersion)
	if ifNoneMatch = strings.TrimSpace(ifNoneMatch); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	// 与 wham 查询一致:自定义头覆盖了工作区 ID 时,清单按覆盖后的空间查询。
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}

	// 复用网关同款 transport（支持 uTLS Chrome 指纹），与 /responses、wham 一致。
	// 池化而非每次新建：Codex 客户端会周期性拉取清单，一次性 uTLS transport
	// 会把连接与 goroutine 持续泄漏到进程结束（issue #446）。
	client := getCodexMaintenanceClient(account, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex models request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return &CodexModelsManifest{ETag: resp.Header.Get("ETag"), NotModified: true}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("codex models upstream status %d: %s", resp.StatusCode, message)
	}

	body, err := ReadModelsListBody(resp.Body, CurrentRuntimeSettings().ModelsListReadMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("read codex models response: %w", err)
	}
	return &CodexModelsManifest{Body: body, ETag: resp.Header.Get("ETag")}, nil
}
