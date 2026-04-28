package refresh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RefreshResult 刷新结果
type RefreshResult struct {
	AccountID      uint64    `json:"account_id"`
	Email          string    `json:"email"`
	OK             bool      `json:"ok"`
	Source         string    `json:"source"` // rt / st / failed
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	Error          string    `json:"error,omitempty"`
	RTRotated      bool      `json:"rt_rotated,omitempty"`
	ATVerified     bool      `json:"at_verified"`
	WebUnauthorized bool     `json:"web_unauthorized,omitempty"`
}

// Refresher Token 刷新器
type Refresher struct {
	client   *http.Client
	clientID string
}

// NewRefresher 创建刷新器
func NewRefresher(clientID string) *Refresher {
	return &Refresher{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		clientID: clientID,
	}
}

// RefreshByRT 使用 RefreshToken 刷新 AccessToken
// POST https://auth.openai.com/oauth/token
func (r *Refresher) RefreshByRT(ctx context.Context, refreshToken string) (newAT, newRT string, expAt time.Time, err error) {
	body := map[string]string{
		"client_id":     r.clientID,
		"grant_type":    "refresh_token",
		"redirect_uri":  "com.openai.chat://auth0.openai.com/ios/com.openai.chat/callback",
		"refresh_token": refreshToken,
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://auth.openai.com/oauth/token", bytes.NewReader(buf))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ChatGPT/1.2025.122 (iOS 18.2; iPhone15,2; build 15096)")

	resp, err := r.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		err = fmt.Errorf("rt refresh http=%d body=%s", resp.StatusCode, truncate(string(data), 200))
		return
	}

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err = json.Unmarshal(data, &out); err != nil {
		return
	}
	if out.AccessToken == "" {
		err = errors.New("rt refresh: missing access_token in response")
		return
	}

	newAT = out.AccessToken
	newRT = out.RefreshToken
	if out.ExpiresIn > 0 {
		expAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	} else {
		expAt = parseJWTExp(newAT)
	}
	return
}

// RefreshByST 使用 SessionToken 刷新 AccessToken
// GET https://chatgpt.com/api/auth/session
func (r *Refresher) RefreshByST(ctx context.Context, sessionToken string) (newAT string, expAt time.Time, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://chatgpt.com/api/auth/session", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

	// 添加 Session Token Cookie
	req.AddCookie(&http.Cookie{Name: "__Secure-next-auth.session-token", Value: sessionToken})

	resp, err := r.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		err = fmt.Errorf("st refresh http=%d body=%s", resp.StatusCode, truncate(string(data), 200))
		return
	}

	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "{}" {
		err = errors.New("ST 已过期或无效，响应为空")
		return
	}

	var out struct {
		AccessToken string `json:"accessToken"`
		Expires     string `json:"expires"`
	}
	if err = json.Unmarshal([]byte(raw), &out); err != nil {
		return
	}
	if out.AccessToken == "" {
		err = errors.New("响应缺少 accessToken 字段")
		return
	}

	newAT = out.AccessToken
	if out.Expires != "" {
		if t, e := time.Parse(time.RFC3339, out.Expires); e == nil {
			expAt = t
		}
	}
	if expAt.IsZero() {
		expAt = parseJWTExp(newAT)
	}
	return
}

// VerifyATOnWeb 验证 AccessToken 是否被 chatgpt.com web 后端接受
// GET /backend-api/me
func (r *Refresher) VerifyATOnWeb(ctx context.Context, accessToken string) (int, error) {
	vctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(vctx, "GET",
		"https://chatgpt.com/backend-api/me", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// 读掉 body，释放连接
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// RefreshAuto 自动刷新：优先 RT，失败则回退 ST
func (r *Refresher) RefreshAuto(ctx context.Context, accountID uint64, email, refreshToken, sessionToken string) (*RefreshResult, error) {
	res := &RefreshResult{AccountID: accountID, Email: email}

	var rtRejectedByWeb bool

	// 尝试 RT
	if refreshToken != "" {
		newAT, newRT, expAt, err := r.RefreshByRT(ctx, refreshToken)
		if err == nil && newAT != "" {
			// RT → AT HTTP 200，现在校验 AT 能否被 chatgpt.com web 后端接受
			verifyStatus, verifyErr := r.VerifyATOnWeb(ctx, newAT)
			switch {
			case verifyErr == nil && verifyStatus == 200:
				res.OK = true
				res.Source = "rt"
				res.ExpiresAt = expAt
				res.ATVerified = true
				res.RTRotated = newRT != ""
				return res, nil
			case verifyStatus == 401:
				// AT 作用域不是 web：不使用，走 ST 回退
				rtRejectedByWeb = true
				res.Error = "RT 换出的 AT 被 chatgpt.com 拒绝（iOS 作用域）"
			default:
				// 其他错误：不使用，走 ST 回退
				if verifyErr != nil {
					res.Error = "RT 换出的 AT 校验失败：" + verifyErr.Error()
				} else {
					res.Error = fmt.Sprintf("RT 换出的 AT 校验失败（HTTP %d）", verifyStatus)
				}
			}
		} else {
			// RT → AT HTTP 本身失败，回退 ST
			res.Error = friendlyRefreshErr(err)
		}
	}

	// 尝试 ST（ST → AT 本来就是 web 作用域，不需要再校验）
	if sessionToken != "" {
		newAT, expAt, err := r.RefreshByST(ctx, sessionToken)
		if err == nil && newAT != "" {
			res.OK = true
			res.Source = "st"
			res.ExpiresAt = expAt
			res.ATVerified = true
			return res, nil
		}
		if res.Error == "" {
			res.Error = friendlyRefreshErr(err)
		} else {
			res.Error += " / ST:" + friendlyRefreshErr(err)
		}
	}

	// 都不行：区分两种失败语义
	if rtRejectedByWeb {
		res.WebUnauthorized = true
		if sessionToken == "" {
			res.Error = "RT 换出的 AT 被 chatgpt.com 拒绝（iOS 作用域不兼容 web），请为该账号补充 Session Token"
		}
	}

	if res.Error == "" {
		res.Error = "账号既无可用 RT 也无可用 ST"
	}
	res.Source = "failed"
	return res, nil
}

// parseJWTExp 解 JWT payload 里的 exp（秒级）
func parseJWTExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Now().Add(24 * time.Hour)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 尝试 StdEncoding（容错）
		raw, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Now().Add(24 * time.Hour)
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Now().Add(24 * time.Hour)
	}
	return time.Unix(claims.Exp, 0)
}

func friendlyRefreshErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "http=401"), strings.Contains(low, "invalid_grant"):
		return "RT 已失效（401）"
	case strings.Contains(low, "http=403"):
		return "上游拒绝访问（403）"
	case strings.Contains(low, "http=429"):
		return "触发速率限制（429）"
	case strings.Contains(low, "timeout"), strings.Contains(low, "deadline exceeded"):
		return "刷新请求超时"
	case strings.Contains(low, "no such host"):
		return "DNS 解析失败"
	case strings.Contains(low, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(low, "connection reset"):
		return "连接被重置"
	case strings.Contains(low, "unexpected eof"), strings.HasSuffix(low, ": eof"):
		return "连接被对端关闭"
	case strings.Contains(low, "tls"), strings.Contains(low, "x509"):
		return "TLS 握手失败"
	case strings.Contains(low, "missing access_token"), strings.Contains(s, "ST 已过期"):
		return s
	default:
		return "刷新失败：" + s
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
