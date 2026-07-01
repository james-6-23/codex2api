package security

import "testing"

func TestParseProxyURLCleanFormats(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantHost string
		wantUser string
		wantPass string
	}{
		{"socks5 with auth", "socks5://user:pass@1.2.3.4:1080", "1.2.3.4:1080", "user", "pass"},
		{"socks5 no auth", "socks5://1.2.3.4:1080", "1.2.3.4:1080", "", ""},
		{"socks5h with auth", "socks5h://user:pass@host.example:1080", "host.example:1080", "user", "pass"},
		{"http with auth", "http://user:pass@1.2.3.4:8080", "1.2.3.4:8080", "user", "pass"},
		{"password with @", "socks5://user:pa@ss@1.2.3.4:1080", "1.2.3.4:1080", "user", "pa@ss"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := ParseProxyURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseProxyURL(%q) error: %v", tc.raw, err)
			}
			if u.Host != tc.wantHost {
				t.Fatalf("host = %q, want %q", u.Host, tc.wantHost)
			}
			gotUser := u.User.Username()
			gotPass, _ := u.User.Password()
			if gotUser != tc.wantUser || gotPass != tc.wantPass {
				t.Fatalf("user/pass = %q/%q, want %q/%q", gotUser, gotPass, tc.wantUser, tc.wantPass)
			}
		})
	}
}

// issue #293: 代理密码含 # / ? 等保留字符，new URL / url.Parse 会直接报错。
// ParseProxyURL 应通过 userinfo 百分号编码正确解出主机与凭据。
func TestParseProxyURLSpecialCharPassword(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantPass string
	}{
		{"hash in password", "socks5://user:pa#ss@1.2.3.4:1080", "pa#ss"},
		{"slash in password", "socks5://user:pa/ss@1.2.3.4:1080", "pa/ss"},
		{"question in password", "socks5://user:pa?ss@1.2.3.4:1080", "pa?ss"},
		{"percent in password", "socks5://user:pa%ss@1.2.3.4:1080", "pa%ss"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := ParseProxyURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseProxyURL(%q) error: %v", tc.raw, err)
			}
			if u.Host != "1.2.3.4:1080" {
				t.Fatalf("host = %q, want 1.2.3.4:1080", u.Host)
			}
			if got, _ := u.User.Password(); got != tc.wantPass {
				t.Fatalf("password = %q, want %q", got, tc.wantPass)
			}
			if u.User.Username() != "user" {
				t.Fatalf("username = %q, want user", u.User.Username())
			}
		})
	}
}

func TestParseProxyURLRejectsInvalid(t *testing.T) {
	cases := []string{
		"",
		"1.2.3.4:1080",                    // 缺 scheme
		"ftp://1.2.3.4:1080",              // 不支持的 scheme
		"socks4://1.2.3.4:1080",           // 不支持的 scheme
		"socks5://user:pass@1.2.3.4:99999", // 端口越界
		"socks5://",                        // 缺主机
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseProxyURL(raw); err == nil {
				t.Fatalf("ParseProxyURL(%q) expected error, got nil", raw)
			}
		})
	}
}
