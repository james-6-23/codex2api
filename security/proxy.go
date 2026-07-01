package security

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// allowedProxySchemes are the proxy URL schemes the dialers support.
var allowedProxySchemes = map[string]struct{}{
	"http":    {},
	"https":   {},
	"socks5":  {},
	"socks5h": {},
}

func proxySchemeAllowed(scheme string) bool {
	_, ok := allowedProxySchemes[strings.ToLower(strings.TrimSpace(scheme))]
	return ok
}

// ParseProxyURL parses a proxy URL leniently and validates it.
//
// It first tries the standard url.Parse, which handles clean URLs and IPv6
// hosts. If that fails — commonly because the password contains reserved
// characters like '#', '/' or '?' that users paste verbatim (issue #293) — it
// percent-encodes the userinfo and retries, so proxy credentials with special
// characters work end-to-end. The dialers keep using u.User.Username()/Password(),
// which return the decoded values.
func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("proxy url is empty")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || !proxySchemeAllowed(u.Scheme) {
		normalized, nerr := normalizeProxyURLEncodingUserinfo(raw)
		if nerr != nil {
			return nil, nerr
		}
		u, err = url.Parse(normalized)
		if err != nil {
			return nil, fmt.Errorf("parse proxy url: %w", err)
		}
	}

	if !proxySchemeAllowed(u.Scheme) {
		return nil, fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy url missing host")
	}
	if port := u.Port(); port != "" {
		if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("proxy url has invalid port %q", port)
		}
	}
	return u, nil
}

// normalizeProxyURLEncodingUserinfo rebuilds a proxy URL with its userinfo
// percent-encoded so url.Parse accepts passwords containing reserved characters.
func normalizeProxyURLEncodingUserinfo(raw string) (string, error) {
	sep := strings.Index(raw, "://")
	if sep <= 0 {
		return "", fmt.Errorf("proxy url must include a scheme, e.g. socks5://host:port")
	}
	scheme := strings.ToLower(raw[:sep])
	if !proxySchemeAllowed(scheme) {
		return "", fmt.Errorf("unsupported proxy scheme: %s", scheme)
	}

	rest := raw[sep+3:]
	if rest == "" {
		return "", fmt.Errorf("proxy url missing host")
	}

	// userinfo 与 hostport 以最后一个 '@' 分隔（密码可能含 '@'）。
	userinfo := ""
	hostport := rest
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo = rest[:at]
		hostport = rest[at+1:]
	}
	// 代理 URL 不带 path/query/fragment，去掉尾部多余部分。
	if cut := strings.IndexAny(hostport, "/?#"); cut >= 0 {
		hostport = hostport[:cut]
	}
	if hostport == "" {
		return "", fmt.Errorf("proxy url missing host")
	}

	encoded := ""
	if userinfo != "" {
		var ui *url.Userinfo
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			ui = url.UserPassword(userinfo[:colon], userinfo[colon+1:])
		} else {
			ui = url.User(userinfo)
		}
		encoded = ui.String() + "@"
	}
	return scheme + "://" + encoded + hostport, nil
}
