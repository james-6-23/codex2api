package auth

// claude.ai sessionKey(cookie)一键换 token。
//
// 用户从已登录的 claude.ai 浏览器里复制 sessionKey cookie,服务端代替浏览器完成
// OAuth 授权三步,换出长效 refresh token(或 Setup Token)后 sessionKey 即可丢弃:
//  1. GET  /api/organizations           —— 用 cookie 列出账号所属组织,挑含 chat 能力的;
//  2. POST /v1/oauth/{org}/authorize     —— 带 cookie + 新 PKCE 直接拿到授权码(JSON
//     返回 redirect_uri,从中解析 code/state),不经过浏览器跳转;
//  3. POST platform.claude.com/v1/oauth/token —— 复用授权码交换。
//
// 前两步打的是 claude.ai 网页端(Cloudflare 前置),因此优先使用 uTLS 浏览器指纹
// 客户端、携带浏览器头、禁止自动跟随重定向(3xx 视为被拦截而非成功)。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// claudeWebSessionScope 是 cookie 登录(完整 OAuth)申请的 scope。网页 authorize
	// 端点只接受 user:profile / user:inference 这类基础 scope。
	claudeWebSessionScope = "user:profile user:inference"
	// claudeWebBrowserUA 与 uTLS Chrome 指纹配对的浏览器 UA。
	claudeWebBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// claudeWebSessionKeyMaxBytes 防御性上限:sessionKey 是 sk-ant-sid01-… 的 ASCII 串。
	claudeWebSessionKeyMaxBytes = 4096
)

// claudeWebBaseURL 是 claude.ai 网页端根地址,测试可替换为 httptest 服务器。
var claudeWebBaseURL = "https://claude.ai"

// claudeWebOrganization 映射 /api/organizations 单个条目里我们关心的字段。
type claudeWebOrganization struct {
	UUID         string   `json:"uuid"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

// NormalizeClaudeSessionKey 清理用户粘贴的 sessionKey:去空白、剥离 "sessionKey="
// 前缀与包裹引号/分号。
func NormalizeClaudeSessionKey(raw string) string {
	v := strings.TrimSpace(raw)
	v = strings.Trim(v, "\"'`;, ")
	if idx := strings.Index(strings.ToLower(v), "sessionkey="); idx >= 0 {
		v = v[idx+len("sessionkey="):]
	}
	if semi := strings.Index(v, ";"); semi >= 0 {
		v = v[:semi]
	}
	return strings.TrimSpace(v)
}

// pickClaudeWebOrganization 选「含 chat 能力且能力最多」的组织。没有可用组织时报错
// (通常意味着该账号没有 Claude 订阅或 sessionKey 属于纯 API 控制台账号)。
func pickClaudeWebOrganization(orgs []claudeWebOrganization) (claudeWebOrganization, error) {
	var best *claudeWebOrganization
	for i := range orgs {
		org := &orgs[i]
		hasChat := false
		for _, c := range org.Capabilities {
			if strings.EqualFold(strings.TrimSpace(c), "chat") {
				hasChat = true
				break
			}
		}
		if !hasChat || strings.TrimSpace(org.UUID) == "" {
			continue
		}
		if best == nil || len(org.Capabilities) > len(best.Capabilities) {
			best = org
		}
	}
	if best == nil {
		return claudeWebOrganization{}, fmt.Errorf("未找到具备 chat 能力的组织(账号可能没有 Claude 订阅)")
	}
	return *best, nil
}

// webClient 返回用于 claude.ai 网页端的客户端:基于 primary(uTLS)但不跟随重定向。
func (o *ClaudeAuth) webClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

func applyClaudeWebHeaders(req *http.Request, sessionKey string) {
	req.Header.Set("Cookie", "sessionKey="+sessionKey)
	req.Header.Set("User-Agent", claudeWebBrowserUA)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Referer", "https://claude.ai/new")
}

// doClaudeWeb 先用 uTLS 客户端请求 claude.ai,传输错误时改用标准客户端重试一次。
func (o *ClaudeAuth) doClaudeWeb(ctx context.Context, method, endpoint string, body []byte, sessionKey string) (*http.Response, error) {
	build := func() (*http.Request, error) {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return nil, err
		}
		applyClaudeWebHeaders(req, sessionKey)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}
	req, err := build()
	if err != nil {
		return nil, err
	}
	resp, err := o.webClient(o.primary).Do(req)
	if err == nil {
		return resp, nil
	}
	retry, buildErr := build()
	if buildErr != nil {
		return nil, err
	}
	return o.webClient(o.fallback).Do(retry)
}

// checkClaudeWebStatus 把 claude.ai 网页端的状态码翻译成可读错误。
func checkClaudeWebStatus(resp *http.Response, step string) ([]byte, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%s 被拒绝 (status %d):sessionKey 无效或已过期,请重新从浏览器复制", step, resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fmt.Errorf("%s 被重定向 (status %d):疑似被 Cloudflare 拦截,请更换干净代理重试", step, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("%s 失败 (status %d): %s", step, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// fetchClaudeWebOrganization 是 cookie 登录第 1 步。
func (o *ClaudeAuth) fetchClaudeWebOrganization(ctx context.Context, sessionKey string) (claudeWebOrganization, error) {
	resp, err := o.doClaudeWeb(ctx, http.MethodGet, claudeWebBaseURL+"/api/organizations", nil, sessionKey)
	if err != nil {
		return claudeWebOrganization{}, fmt.Errorf("读取组织列表失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := checkClaudeWebStatus(resp, "读取组织列表")
	if err != nil {
		return claudeWebOrganization{}, err
	}
	var orgs []claudeWebOrganization
	if err := json.Unmarshal(body, &orgs); err != nil {
		return claudeWebOrganization{}, fmt.Errorf("组织列表响应格式无效: %w", err)
	}
	return pickClaudeWebOrganization(orgs)
}

// claudeWebAuthorizeRequest 是网页 authorize 端点请求体(字段顺序固定)。
type claudeWebAuthorizeRequest struct {
	ResponseType        string `json:"response_type"`
	ClientID            string `json:"client_id"`
	OrganizationUUID    string `json:"organization_uuid"`
	RedirectURI         string `json:"redirect_uri"`
	Scope               string `json:"scope"`
	State               string `json:"state"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
}

// authorizeClaudeWithSessionKey 是 cookie 登录第 2 步:返回 (code, verifier, state)。
func (o *ClaudeAuth) authorizeClaudeWithSessionKey(ctx context.Context, sessionKey, orgUUID, scope string) (code, verifier, state string, err error) {
	pkce, err := GenerateClaudePKCE()
	if err != nil {
		return "", "", "", err
	}
	state, err = generateClaudeOAuthState()
	if err != nil {
		return "", "", "", err
	}
	payload, err := json.Marshal(claudeWebAuthorizeRequest{
		ResponseType:        "code",
		ClientID:            ClaudeOAuthClientID,
		OrganizationUUID:    orgUUID,
		RedirectURI:         ClaudeOAuthRedirectURI,
		Scope:               scope,
		State:               state,
		CodeChallenge:       pkce.CodeChallenge,
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		return "", "", "", err
	}
	endpoint := claudeWebBaseURL + "/v1/oauth/" + url.PathEscape(orgUUID) + "/authorize"
	resp, err := o.doClaudeWeb(ctx, http.MethodPost, endpoint, payload, sessionKey)
	if err != nil {
		return "", "", "", fmt.Errorf("cookie 授权请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := checkClaudeWebStatus(resp, "cookie 授权")
	if err != nil {
		return "", "", "", err
	}
	var data struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", "", fmt.Errorf("授权响应解析失败: %w", err)
	}
	if strings.TrimSpace(data.RedirectURI) == "" {
		return "", "", "", fmt.Errorf("授权响应中未找到 redirect_uri")
	}
	code, returnedState := parseClaudeCodeAndState(data.RedirectURI)
	if code == "" {
		return "", "", "", fmt.Errorf("redirect_uri 中未找到授权码")
	}
	if returnedState != "" {
		state = returnedState
	}
	return code, pkce.CodeVerifier, state, nil
}

// ExchangeSessionKey 用 claude.ai sessionKey 一键换取 Claude 凭据。setupToken 为 true
// 时申请长效 Setup Token(仅 user:inference),否则申请可刷新的 OAuth 凭据。
func (o *ClaudeAuth) ExchangeSessionKey(ctx context.Context, sessionKey string, setupToken bool) (*ClaudeTokenData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionKey = NormalizeClaudeSessionKey(sessionKey)
	if sessionKey == "" {
		return nil, fmt.Errorf("sessionKey 不能为空")
	}
	if len(sessionKey) > claudeWebSessionKeyMaxBytes {
		return nil, fmt.Errorf("sessionKey 过长")
	}
	org, err := o.fetchClaudeWebOrganization(ctx, sessionKey)
	if err != nil {
		return nil, err
	}
	scope := claudeWebSessionScope
	if setupToken {
		scope = ClaudeOAuthScopeInference
	}
	code, verifier, state, err := o.authorizeClaudeWithSessionKey(ctx, sessionKey, org.UUID, scope)
	if err != nil {
		return nil, err
	}
	td, err := o.exchangeAuthorizationCode(ctx, code, verifier, state, ClaudeExchangeOptions{RedirectURI: ClaudeOAuthRedirectURI, SetupToken: setupToken})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(td.OrganizationUUID) == "" {
		td.OrganizationUUID = org.UUID
	}
	if strings.TrimSpace(td.OrganizationName) == "" {
		td.OrganizationName = org.Name
	}
	return td, nil
}
