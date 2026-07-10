package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// CodexModelsManifestHandler 向 Codex 客户端透传模型清单。
//
// 用一个可调度的 ChatGPT OAuth 账号的凭据实时转发，响应体与 ETag 原样透传。
// 上游失败时 fast-fail 返回错误、不伪造列表——Codex 客户端自身会回落本地缓存。
func (h *Handler) CodexModelsManifestHandler(c *gin.Context) {
	apiKeyID := requestAPIKeyID(c)
	// 清单端点只存在于 ChatGPT 后端,relay API-key 账号无从代答。
	account := h.store.NextExcludingWithFilter(apiKeyID, nil, func(a *auth.Account) bool {
		return !a.IsOpenAIResponsesAPI()
	})
	if account == nil {
		api.SendError(c, api.ErrServiceUnavailable)
		return
	}
	defer h.store.Release(account)

	manifest, err := FetchCodexModelsManifest(
		c.Request.Context(),
		account,
		h.store.ResolveProxyForAccount(account),
		c.Query("client_version"),
		c.GetHeader("If-None-Match"),
	)
	if err != nil {
		api.SendErrorWithStatus(c,
			api.NewAPIError(api.ErrCodeUpstreamError, fmt.Sprintf("codex models manifest: %v", err), api.ErrorTypeUpstream),
			http.StatusBadGateway)
		return
	}

	if manifest.ETag != "" {
		c.Header("ETag", manifest.ETag)
	}
	if manifest.NotModified {
		c.Status(http.StatusNotModified)
		return
	}
	// 顺手把清单里注册表不认识的新模型学习进注册表（只增不改不删），
	// 让选单里出现的新模型立即通过请求侧模型校验，无需等手动同步。
	h.learnManifestModelsAsync(manifest.Body)
	c.Data(http.StatusOK, "application/json", manifest.Body)
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
	if h == nil || h.db == nil || len(manifestBody) == 0 {
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
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	}()
}

// CodexModelsManifestURL 是 ChatGPT 后端的 Codex 模型清单端点。
// Codex CLI / Codex App 从 provider 的 GET {base_url}/models?client_version=...
// （自定义 provider 模式）或 GET /backend-api/codex/models（chatgpt_base_url 模式）
// 刷新模型选单，期望 manifest 格式（{"models":[{slug,...}]}）而非 OpenAI 兼容列表；
// 解析失败时客户端会静默回落本地缓存，模型选单从此冻结、新模型永远不出现。
const CodexModelsManifestURL = "https://chatgpt.com/backend-api/codex/models"

// codexModelsManifestURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var codexModelsManifestURLForTest = ""

// manifest 响应体上限。清单是结构化 JSON，正常远小于该值，仅作读取护栏。
const codexModelsManifestBodyLimit int64 = 8 << 20

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
	req.Header.Set("User-Agent", defaultCodexCLIUserAgent)
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
	client := &http.Client{Transport: newCodexTransport(proxyURL)}

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

	body, err := io.ReadAll(io.LimitReader(resp.Body, codexModelsManifestBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read codex models response: %w", err)
	}
	return &CodexModelsManifest{Body: body, ETag: resp.Header.Get("ETag")}, nil
}
