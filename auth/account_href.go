package auth

import (
	"fmt"
	"net/url"
	"strings"
)

// AccountHrefCredentialKey 是账号绑定的「跳转地址」（credentials JSON 内），
// 供账号页在浏览器里一键打开账号对应的控制台/用量页面。存放在既有
// credentials JSON 里，旧库可以安全忽略该字段；空值表示未配置，
// 前端回退打开账号的 base_url（api-base）。
const AccountHrefCredentialKey = "account_href"

// NormalizeAccountHref 校验并归一跳转地址：仅接受 http/https 绝对地址，
// 去掉尾部斜杠；空输入合法（= 清除配置）。
func NormalizeAccountHref(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", fmt.Errorf("account_href 必须是完整的 http/https URL")
	}
	return strings.TrimRight(trimmed, "/"), nil
}
