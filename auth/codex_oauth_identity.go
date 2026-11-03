package auth

import (
	"context"
	"net/http"
)

// 由启动装配注入，与对话共用解析器，避免 auth 反向依赖 proxy。
var CodexOAuthIdentityHeaders func(*Account) (http.Header, error)

type codexOAuthIdentityKey struct{}

type codexOAuthIdentity struct {
	headers http.Header
	reason  string
}

func withCodexOAuthIdentity(ctx context.Context, account *Account, force bool) (context.Context, error) {
	identity := codexOAuthIdentity{reason: "proactive_expiry"}
	if force {
		identity.reason = "forced"
	}
	if CodexOAuthIdentityHeaders != nil {
		headers, err := CodexOAuthIdentityHeaders(account)
		if err != nil {
			return nil, err
		}
		identity.headers = headers.Clone()
	}
	return context.WithValue(ctx, codexOAuthIdentityKey{}, identity), nil
}

func applyCodexOAuthIdentity(req *http.Request) {
	identity, _ := req.Context().Value(codexOAuthIdentityKey{}).(codexOAuthIdentity)
	// OAuth 刷新只使用默认客户端身份，不携带对话、设备证明或账号认证头。
	for _, name := range []string{"User-Agent", "Originator"} {
		if value := identity.headers.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
}

func codexOAuthRefreshReason(ctx context.Context) string {
	identity, ok := ctx.Value(codexOAuthIdentityKey{}).(codexOAuthIdentity)
	if ok {
		return identity.reason
	}
	return "forced"
}
