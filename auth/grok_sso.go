package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Grok Web SSO → Build（OAuth）自动转换。
//
// 输入是 grok.com 网页登录的 sso cookie；转换用该 cookie 自动跑完 xAI 的
// RFC 8628 Device Code 授权（device → verify → approve 全部程序化完成，无需人工点授权页），
// 产出与 device flow 相同形态的 OAuth 凭据（access_token + refresh_token），
// 直接进 Grok 账号池，可自动刷新。
//
// 与人工 device flow（grok_device.go）的区别：那条路要用户在浏览器里输 user_code；
// 这条路用 sso cookie 让服务端替用户完成 verify/approve，适合批量导入已登录的 Web 账号。

const (
	grokSSOAccountsURL = "https://accounts.x.ai/"
	grokSSODeviceURL   = GrokDefaultOIDCIssuer + "/oauth2/device/code"
	grokSSOVerifyURL   = GrokDefaultOIDCIssuer + "/oauth2/device/verify"
	grokSSOApproveURL  = GrokDefaultOIDCIssuer + "/oauth2/device/approve"
	grokSSOMaxAuthBody = 2 << 20
	// 浏览器 UA：SSO cookie 是网页态凭据，OAuth 端点对非浏览器 UA 更易触发风控。
	grokSSOUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// GrokSSOSeed 是从导入文件解析出的一条待转换 SSO 凭据。
type GrokSSOSeed struct {
	Name  string
	Email string
	Token string
}

// GrokSSOBuildResult 是 SSO → Build 转换成功的结果，形态与 device flow 一致。
type GrokSSOBuildResult struct {
	Token   *GrokTokenData
	Subject string
	Email   string
	TeamID  string
}

// SanitizeGrokSSOToken 清洗 sso token：剥离 "sso=" 前缀、"; " 之后的其余 cookie、
// 以及回车/换行/空字节。返回纯 token 值。
func SanitizeGrokSSOToken(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "sso=") {
		value = strings.TrimSpace(value[len("sso="):])
	}
	if token, _, found := strings.Cut(value, ";"); found {
		value = token
	}
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(value))
}

// grokSSOImportDocument 是 JSON 导入格式：{"provider":"web","accounts":[...]}。
type grokSSOImportDocument struct {
	Accounts []grokSSOImportEntry `json:"accounts"`
}

type grokSSOImportEntry struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	SSOToken string `json:"sso_token"`
	Token    string `json:"token"`
}

const grokSSOMaxTokenBytes = 16 << 10

// ParseGrokSSOTokens 解析导入数据，支持两种格式：
//   - JSON：{"accounts":[{"name","email","sso_token"/"token"}]}
//   - 纯文本：每行一个 sso token（可带 "sso=" 前缀）
//
// 去重（按 token）后返回。
func ParseGrokSSOTokens(data []byte) ([]GrokSSOSeed, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("导入内容为空")
	}

	seen := make(map[string]struct{})
	var seeds []GrokSSOSeed
	appendSeed := func(name, email, rawToken string) error {
		token := SanitizeGrokSSOToken(rawToken)
		if token == "" {
			return nil
		}
		if len(token) > grokSSOMaxTokenBytes {
			return fmt.Errorf("单个 sso token 超过 16 KiB")
		}
		if _, dup := seen[token]; dup {
			return nil
		}
		seen[token] = struct{}{}
		seeds = append(seeds, GrokSSOSeed{
			Name:  strings.TrimSpace(name),
			Email: strings.TrimSpace(email),
			Token: token,
		})
		return nil
	}

	if strings.HasPrefix(trimmed, "{") {
		var doc grokSSOImportDocument
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("解析 SSO 导入 JSON 失败: %w", err)
		}
		for _, entry := range doc.Accounts {
			tok := entry.SSOToken
			if strings.TrimSpace(tok) == "" {
				tok = entry.Token
			}
			if err := appendSeed(entry.Name, entry.Email, tok); err != nil {
				return nil, err
			}
		}
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			if err := appendSeed("", "", line); err != nil {
				return nil, err
			}
		}
	}

	if len(seeds) == 0 {
		return nil, fmt.Errorf("未解析到有效的 sso token")
	}
	return seeds, nil
}

// grokSSOFlow 承载一次 SSO → Build 转换的 HTTP 会话（携带 sso cookie，手动跟随跳转）。
type grokSSOFlow struct {
	client  *http.Client
	cookies map[string]string
}

// ConvertGrokSSOToBuild 用 sso token 自动完成 xAI Device Code 授权并返回 OAuth 凭据。
func ConvertGrokSSOToBuild(ctx context.Context, ssoToken, proxyURL string) (*GrokSSOBuildResult, error) {
	token := SanitizeGrokSSOToken(ssoToken)
	if token == "" {
		return nil, fmt.Errorf("sso token 为空")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := ConfigureTransportProxy(transport, proxyURL, nil); err != nil {
		return nil, fmt.Errorf("代理配置失败: %w", err)
	}
	flow := &grokSSOFlow{
		// 手动跟随跳转：需要逐跳捕获 Set-Cookie 并校验目标域，禁用默认自动跳转。
		client: &http.Client{
			Transport:     transport,
			Timeout:       90 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cookies: map[string]string{"sso": token, "sso-rw": token},
	}
	return flow.convert(ctx)
}

func (f *grokSSOFlow) convert(ctx context.Context) (*GrokSSOBuildResult, error) {
	// 1) 用 sso cookie 访问 accounts 页做一次弱校验：只把明确的登录跳转/401 判为失效，
	//    其余状态（含 Cloudflare 质询等）不拦截，交由后续 device flow 兜底判定。
	status, finalURL, _, err := f.do(ctx, http.MethodGet, grokSSOAccountsURL, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || strings.Contains(finalURL, "sign-in") || strings.Contains(finalURL, "sign-up") {
		return nil, fmt.Errorf("sso 凭据无效或已过期")
	}

	// 2) 启动 device flow
	form := url.Values{"client_id": {GrokDefaultOAuthClientID}, "scope": {GrokDefaultOAuthScope}}
	status, _, body, err := f.do(ctx, http.MethodPost, grokSSODeviceURL, form)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("启动 device flow 失败 (status=%d)", status)
	}
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &device); err != nil {
		return nil, fmt.Errorf("解析 device flow 响应失败: %w", err)
	}
	if device.DeviceCode == "" || device.UserCode == "" || !safeGrokXAIURL(device.VerificationURIComplete) {
		return nil, fmt.Errorf("device flow 响应字段不完整")
	}
	if device.Interval <= 0 {
		device.Interval = 5
	}
	if device.ExpiresIn <= 0 {
		device.ExpiresIn = 1800
	}

	// 3) 打开验证页 → 提交 user_code（自动 verify）→ 自动 approve
	if status, _, _, err = f.do(ctx, http.MethodGet, device.VerificationURIComplete, nil); err != nil {
		return nil, err
	}
	if status < 200 || status >= 400 {
		return nil, fmt.Errorf("打开验证页失败 (status=%d)", status)
	}
	status, finalURL, _, err = f.do(ctx, http.MethodPost, grokSSOVerifyURL, url.Values{"user_code": {device.UserCode}})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 400 || !strings.Contains(finalURL, "consent") {
		return nil, fmt.Errorf("自动验证 device flow 失败（sso 可能无效）")
	}
	status, finalURL, _, err = f.do(ctx, http.MethodPost, grokSSOApproveURL, url.Values{
		"user_code": {device.UserCode}, "action": {"allow"}, "principal_type": {"User"}, "principal_id": {""},
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 400 || !strings.Contains(finalURL, "done") {
		return nil, fmt.Errorf("自动批准 device flow 失败")
	}

	// 4) 轮询兑换 token（已批准，通常首轮即返回）
	token, err := f.pollToken(ctx, device.DeviceCode, time.Duration(device.Interval)*time.Second, time.Duration(device.ExpiresIn)*time.Second)
	if err != nil {
		return nil, err
	}
	idToken := token.IDToken
	if idToken == "" {
		idToken = token.AccessToken
	}
	claims := grokJWTClaims(idToken)
	subject := grokClaimString(claims, "sub")
	if subject == "" {
		subject = GrokSubjectFromAccessToken(token.AccessToken)
	}
	return &GrokSSOBuildResult{
		Token:   token,
		Subject: subject,
		Email:   grokClaimString(claims, "email"),
		TeamID:  grokClaimString(claims, "team_id"),
	}, nil
}

func (f *grokSSOFlow) pollToken(ctx context.Context, deviceCode string, interval, expiresIn time.Duration) (*GrokTokenData, error) {
	if interval < time.Second {
		interval = time.Second
	}
	// 已批准，token 端点通常首次请求即返回；先立即试一次，pending 再退避轮询，
	// 省掉一整个 interval 的空等。总时长上限取 device 过期与 60s 的较小值。
	deadline := time.Now().Add(min(expiresIn, 60*time.Second))
	for {
		status, _, body, err := f.do(ctx, http.MethodPost, GrokDefaultTokenURL, url.Values{
			"grant_type":  {GrokDeviceCodeGrantType},
			"client_id":   {GrokDefaultOAuthClientID},
			"device_code": {deviceCode},
		})
		if err != nil {
			return nil, err
		}
		var payload struct {
			AccessToken      string `json:"access_token"`
			RefreshToken     string `json:"refresh_token"`
			IDToken          string `json:"id_token"`
			ExpiresIn        int    `json:"expires_in"`
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("解析 token 响应失败: %w", err)
		}
		if status >= 200 && status < 300 && payload.AccessToken != "" {
			expiresIn := payload.ExpiresIn
			if expiresIn <= 0 {
				expiresIn = 21600
			}
			expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
			if claims := grokJWTClaims(payload.AccessToken); claims != nil {
				if exp, ok := claims["exp"].(float64); ok && exp > 0 {
					expiresAt = time.Unix(int64(exp), 0)
				}
			}
			return &GrokTokenData{
				AccessToken:  payload.AccessToken,
				RefreshToken: payload.RefreshToken,
				IDToken:      payload.IDToken,
				ExpiresAt:    expiresAt,
			}, nil
		}
		switch payload.Error {
		case "authorization_pending":
			// 继续退避轮询
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied", "expired_token":
			return nil, fmt.Errorf("device 授权被拒绝或已过期")
		default:
			if status >= 400 {
				desc := strings.TrimSpace(payload.ErrorDescription)
				if desc == "" {
					desc = strconv.Itoa(status)
				}
				return nil, fmt.Errorf("兑换 token 失败: %s", desc)
			}
		}

		// 剩余时间不足一个 interval 就不再等待，直接判超时。
		if time.Now().Add(interval).After(deadline) {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("device flow 轮询超时")
}

// do 发起一次请求并手动跟随最多 8 跳；逐跳捕获 Set-Cookie、校验目标仍在 *.x.ai。
// 返回最终状态码、最终 URL、响应体。
func (f *grokSSOFlow) do(ctx context.Context, method, endpoint string, form url.Values) (int, string, []byte, error) {
	if !safeGrokXAIURL(endpoint) {
		return 0, "", nil, fmt.Errorf("xAI OAuth URL 不安全")
	}
	currentURL := endpoint
	currentMethod := method
	currentForm := form
	for redirects := 0; redirects <= 8; redirects++ {
		var reqBody io.Reader
		if currentForm != nil {
			reqBody = strings.NewReader(currentForm.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, currentMethod, currentURL, reqBody)
		if err != nil {
			return 0, "", nil, err
		}
		req.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("User-Agent", grokSSOUserAgent)
		req.Header.Set("Cookie", f.cookieHeader())
		if currentForm != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := f.client.Do(req)
		if err != nil {
			return 0, "", nil, err
		}
		f.captureCookies(resp)
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, grokSSOMaxAuthBody+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return resp.StatusCode, currentURL, nil, readErr
		}
		if len(data) > grokSSOMaxAuthBody {
			return resp.StatusCode, currentURL, nil, fmt.Errorf("xAI OAuth 响应超过 2 MiB")
		}
		if resp.StatusCode < 300 || resp.StatusCode > 399 {
			return resp.StatusCode, currentURL, data, nil
		}
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location == "" {
			return resp.StatusCode, currentURL, data, fmt.Errorf("xAI OAuth 跳转缺少 Location")
		}
		base, _ := url.Parse(currentURL)
		next, err := url.Parse(location)
		if err != nil {
			return resp.StatusCode, currentURL, data, err
		}
		currentURL = base.ResolveReference(next).String()
		if !safeGrokXAIURL(currentURL) {
			return resp.StatusCode, currentURL, data, fmt.Errorf("xAI OAuth 跳转到非受信域名")
		}
		// 303、以及非 GET/HEAD 的 301/302 跳转后按浏览器行为改为 GET。
		if resp.StatusCode == http.StatusSeeOther ||
			((resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) &&
				currentMethod != http.MethodGet && currentMethod != http.MethodHead) {
			currentMethod = http.MethodGet
			currentForm = nil
		}
	}
	return 0, currentURL, nil, fmt.Errorf("xAI OAuth 跳转次数过多")
}

func (f *grokSSOFlow) captureCookies(resp *http.Response) {
	for _, cookie := range resp.Cookies() {
		name := strings.TrimSpace(cookie.Name)
		value := strings.TrimSpace(cookie.Value)
		if name == "" || len(name) > 128 || len(value) > 16384 || strings.ContainsAny(name+value, "\r\n\x00") {
			continue
		}
		if cookie.MaxAge < 0 {
			delete(f.cookies, name)
			continue
		}
		f.cookies[name] = value
	}
}

func (f *grokSSOFlow) cookieHeader() string {
	keys := make([]string, 0, len(f.cookies))
	for key := range f.cookies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+f.cookies[key])
	}
	return strings.Join(parts, "; ")
}

// safeGrokXAIURL 限定 OAuth 流程只跟随到 https 的 x.ai / *.x.ai。
func safeGrokXAIURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}
