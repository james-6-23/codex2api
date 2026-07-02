package admin

import "testing"

// issue #199: Docker 端口映射后宿主机访问的源 IP 是网桥地址(私网)而非 loopback，
// 未配置 BOOTSTRAP_ALLOWED_CIDR 时必须默认放行私网来源，否则本地 Docker 部署
// 无法完成页面初始化。
func TestBootstrapAllowClientIPDefaultsAllowPrivate(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "")

	allowed := []string{
		"127.0.0.1", "::1", // loopback
		"172.17.0.1", "172.18.0.1", // docker 网桥
		"10.0.0.5", "192.168.1.100", // 常见私网
		"fd00::1",      // ULA IPv6 私网
		"169.254.10.1", // 链路本地
	}
	for _, ip := range allowed {
		if !bootstrapAllowClientIP(ip) {
			t.Errorf("bootstrapAllowClientIP(%q) = false, want true (default private allow)", ip)
		}
	}

	denied := []string{"203.0.113.7", "8.8.8.8", "2001:4860:4860::8888", "", "not-an-ip"}
	for _, ip := range denied {
		if bootstrapAllowClientIP(ip) {
			t.Errorf("bootstrapAllowClientIP(%q) = true, want false (public/invalid must be denied)", ip)
		}
	}
}

func TestBootstrapAllowClientIPExplicitCIDROverridesDefault(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "203.0.113.0/24")

	if !bootstrapAllowClientIP("203.0.113.7") {
		t.Error("IP inside configured CIDR should be allowed")
	}
	// 显式配置后私网默认放行不再生效，只认配置的网段 + loopback
	if bootstrapAllowClientIP("172.17.0.1") {
		t.Error("private IP outside configured CIDR should be denied when CIDR is set")
	}
	if !bootstrapAllowClientIP("127.0.0.1") {
		t.Error("loopback must always be allowed")
	}
}

func TestBootstrapAllowClientIPNoneDisablesDefault(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "none")

	if bootstrapAllowClientIP("172.17.0.1") {
		t.Error("BOOTSTRAP_ALLOWED_CIDR=none should deny private IPs")
	}
	if !bootstrapAllowClientIP("127.0.0.1") {
		t.Error("loopback must always be allowed even with none")
	}
}
