package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestParseCodexCLIVersion(t *testing.T) {
	tests := []struct {
		name      string
		ua        string
		wantMajor int
		wantMinor int
		wantPatch int
		wantOK    bool
	}{
		{
			name:      "valid codex_cli_rs version",
			ua:        "codex_cli_rs/0.117.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
			wantMajor: 0,
			wantMinor: 117,
			wantPatch: 0,
			wantOK:    true,
		},
		{
			name:      "valid codex_cli_rs version with different numbers",
			ua:        "codex_cli_rs/1.2.3",
			wantMajor: 1,
			wantMinor: 2,
			wantPatch: 3,
			wantOK:    true,
		},
		{
			name:   "invalid version - no version",
			ua:     "codex_cli_rs (Mac OS 15.5.0; arm64)",
			wantOK: false,
		},
		{
			name:   "invalid version - different format",
			ua:     "Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
			wantOK: false,
		},
		{
			name:   "empty string",
			ua:     "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, ok := parseCodexCLIVersion(tt.ua)
			if ok != tt.wantOK {
				t.Errorf("parseCodexCLIVersion() ok = %v, want %v", ok, tt.wantOK)
				return
			}
			if !tt.wantOK {
				return
			}
			if version.major != tt.wantMajor {
				t.Errorf("parseCodexCLIVersion() major = %v, want %v", version.major, tt.wantMajor)
			}
			if version.minor != tt.wantMinor {
				t.Errorf("parseCodexCLIVersion() minor = %v, want %v", version.minor, tt.wantMinor)
			}
			if version.patch != tt.wantPatch {
				t.Errorf("parseCodexCLIVersion() patch = %v, want %v", version.patch, tt.wantPatch)
			}
		})
	}
}

func TestCLIVersionCompare(t *testing.T) {
	tests := []struct {
		name string
		v1   cliVersion
		v2   cliVersion
		want int
	}{
		{
			name: "v1 > v2 (major)",
			v1:   cliVersion{major: 1, minor: 0, patch: 0},
			v2:   cliVersion{major: 0, minor: 117, patch: 0},
			want: 1,
		},
		{
			name: "v1 < v2 (major)",
			v1:   cliVersion{major: 0, minor: 117, patch: 0},
			v2:   cliVersion{major: 1, minor: 0, patch: 0},
			want: -1,
		},
		{
			name: "v1 > v2 (minor)",
			v1:   cliVersion{major: 0, minor: 117, patch: 0},
			v2:   cliVersion{major: 0, minor: 116, patch: 0},
			want: 1,
		},
		{
			name: "v1 < v2 (minor)",
			v1:   cliVersion{major: 0, minor: 116, patch: 0},
			v2:   cliVersion{major: 0, minor: 117, patch: 0},
			want: -1,
		},
		{
			name: "v1 > v2 (patch)",
			v1:   cliVersion{major: 0, minor: 117, patch: 1},
			v2:   cliVersion{major: 0, minor: 117, patch: 0},
			want: 1,
		},
		{
			name: "v1 < v2 (patch)",
			v1:   cliVersion{major: 0, minor: 117, patch: 0},
			v2:   cliVersion{major: 0, minor: 117, patch: 1},
			want: -1,
		},
		{
			name: "v1 == v2",
			v1:   cliVersion{major: 0, minor: 117, patch: 0},
			v2:   cliVersion{major: 0, minor: 117, patch: 0},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.v1.Compare(tt.v2)
			if got != tt.want {
				t.Errorf("cliVersion.Compare() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldUpgradeDeviceProfile(t *testing.T) {
	tests := []struct {
		name      string
		candidate deviceProfile
		current   deviceProfile
		want      bool
	}{
		{
			name:      "candidate has no version",
			candidate: deviceProfile{UserAgent: "test", HasVersion: false},
			current:   deviceProfile{UserAgent: "current", HasVersion: true, Version: cliVersion{major: 0, minor: 117, patch: 0}},
			want:      false,
		},
		{
			name:      "candidate has empty UA",
			candidate: deviceProfile{UserAgent: "", HasVersion: true, Version: cliVersion{major: 0, minor: 118, patch: 0}},
			current:   deviceProfile{UserAgent: "current", HasVersion: true, Version: cliVersion{major: 0, minor: 117, patch: 0}},
			want:      false,
		},
		{
			name:      "current has no version - should upgrade",
			candidate: deviceProfile{UserAgent: "codex_cli_rs/0.118.0", HasVersion: true, Version: cliVersion{major: 0, minor: 118, patch: 0}},
			current:   deviceProfile{UserAgent: "", HasVersion: false},
			want:      true,
		},
		{
			name:      "candidate version higher - should upgrade",
			candidate: deviceProfile{UserAgent: "codex_cli_rs/0.118.0", HasVersion: true, Version: cliVersion{major: 0, minor: 118, patch: 0}},
			current:   deviceProfile{UserAgent: "codex_cli_rs/0.117.0", HasVersion: true, Version: cliVersion{major: 0, minor: 117, patch: 0}},
			want:      true,
		},
		{
			name:      "candidate version lower - should not upgrade",
			candidate: deviceProfile{UserAgent: "codex_cli_rs/0.116.0", HasVersion: true, Version: cliVersion{major: 0, minor: 116, patch: 0}},
			current:   deviceProfile{UserAgent: "codex_cli_rs/0.117.0", HasVersion: true, Version: cliVersion{major: 0, minor: 117, patch: 0}},
			want:      false,
		},
		{
			name:      "candidate version equal - should not upgrade",
			candidate: deviceProfile{UserAgent: "codex_cli_rs/0.117.0", HasVersion: true, Version: cliVersion{major: 0, minor: 117, patch: 0}},
			current:   deviceProfile{UserAgent: "codex_cli_rs/0.117.0", HasVersion: true, Version: cliVersion{major: 0, minor: 117, patch: 0}},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldUpgradeDeviceProfile(tt.candidate, tt.current)
			if got != tt.want {
				t.Errorf("shouldUpgradeDeviceProfile() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPinDeviceProfilePlatform(t *testing.T) {
	baseline := deviceProfile{
		OS:   "MacOS",
		Arch: "arm64",
	}
	candidate := deviceProfile{
		OS:   "Linux",
		Arch: "x64",
	}

	result := pinDeviceProfilePlatform(candidate, baseline)

	if result.OS != baseline.OS {
		t.Errorf("pinDeviceProfilePlatform() OS = %v, want %v", result.OS, baseline.OS)
	}
	if result.Arch != baseline.Arch {
		t.Errorf("pinDeviceProfilePlatform() Arch = %v, want %v", result.Arch, baseline.Arch)
	}
}

func TestNormalizeDeviceProfile(t *testing.T) {
	baseline := deviceProfile{
		UserAgent:      "codex_cli_rs/0.117.0",
		PackageVersion: "0.117.0",
		RuntimeVersion: "0.117.0",
		OS:             "MacOS",
		Arch:           "arm64",
		Version:        cliVersion{major: 0, minor: 117, patch: 0},
		HasVersion:     true,
	}

	tests := []struct {
		name     string
		profile  deviceProfile
		baseline deviceProfile
		want     deviceProfile
	}{
		{
			name: "profile with lower version should use baseline",
			profile: deviceProfile{
				UserAgent:      "codex_cli_rs/0.116.0",
				PackageVersion: "0.116.0",
				RuntimeVersion: "0.116.0",
				OS:             "Linux",
				Arch:           "x64",
				Version:        cliVersion{major: 0, minor: 116, patch: 0},
				HasVersion:     true,
			},
			baseline: baseline,
			want:     baseline,
		},
		{
			name: "profile with higher version should keep UA but use baseline OS/Arch",
			profile: deviceProfile{
				UserAgent:      "codex_cli_rs/0.118.0",
				PackageVersion: "0.118.0",
				RuntimeVersion: "0.118.0",
				OS:             "Linux",
				Arch:           "x64",
				Version:        cliVersion{major: 0, minor: 118, patch: 0},
				HasVersion:     true,
			},
			baseline: baseline,
			want: deviceProfile{
				UserAgent:      "codex_cli_rs/0.118.0",
				PackageVersion: "0.118.0",
				RuntimeVersion: "0.118.0",
				OS:             "MacOS",
				Arch:           "arm64",
				Version:        cliVersion{major: 0, minor: 118, patch: 0},
				HasVersion:     true,
			},
		},
		{
			name: "empty profile should use baseline",
			profile: deviceProfile{
				UserAgent:      "",
				PackageVersion: "",
				RuntimeVersion: "",
				OS:             "Linux",
				Arch:           "x64",
				HasVersion:     false,
			},
			baseline: baseline,
			want:     baseline,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeDeviceProfile(tt.profile, tt.baseline)
			if got.UserAgent != tt.want.UserAgent {
				t.Errorf("normalizeDeviceProfile() UserAgent = %v, want %v", got.UserAgent, tt.want.UserAgent)
			}
			if got.OS != tt.want.OS {
				t.Errorf("normalizeDeviceProfile() OS = %v, want %v", got.OS, tt.want.OS)
			}
			if got.Arch != tt.want.Arch {
				t.Errorf("normalizeDeviceProfile() Arch = %v, want %v", got.Arch, tt.want.Arch)
			}
		})
	}
}

func TestExtractDeviceProfile(t *testing.T) {
	cfg := &DeviceProfileConfig{
		UserAgent:      "codex_cli_rs/0.117.0",
		PackageVersion: "0.117.0",
		RuntimeVersion: "0.117.0",
		OS:             "MacOS",
		Arch:           "arm64",
	}

	tests := []struct {
		name    string
		headers http.Header
		wantOK  bool
		wantUA  string
	}{
		{
			name:    "nil headers",
			headers: nil,
			wantOK:  false,
		},
		{
			name:    "valid codex headers",
			headers: http.Header{"User-Agent": []string{"codex_cli_rs/0.118.0 (Ubuntu 24.04; x86_64)"}},
			wantOK:  true,
			wantUA:  "codex_cli_rs/0.118.0 (Ubuntu 24.04; x86_64)",
		},
		{
			name:    "invalid user agent",
			headers: http.Header{"User-Agent": []string{"Mozilla/5.0"}},
			wantOK:  false,
		},
		{
			name:    "empty user agent",
			headers: http.Header{"User-Agent": []string{""}},
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, ok := extractDeviceProfile(tt.headers, cfg)
			if ok != tt.wantOK {
				t.Errorf("extractDeviceProfile() ok = %v, want %v", ok, tt.wantOK)
				return
			}
			if tt.wantOK && profile.UserAgent != tt.wantUA {
				t.Errorf("extractDeviceProfile() UserAgent = %v, want %v", profile.UserAgent, tt.wantUA)
			}
		})
	}
}

func TestDeviceProfileScopeKey(t *testing.T) {
	tests := []struct {
		name   string
		account *auth.Account
		apiKey string
		want   string
	}{
		{
			name:   "nil account, empty api key",
			account: nil,
			apiKey: "",
			want:   "global",
		},
		{
			name:   "nil account, with api key",
			account: nil,
			apiKey: "sk-test123",
			want:   "api_key:sk-test123",
		},
		{
			name:   "with account, empty api key",
			account: &auth.Account{},
			apiKey: "",
			want:   "global",
		},
		{
			name:   "api key takes priority over account",
			account: &auth.Account{}, // account.ID() == 0
			apiKey: "sk-priority",
			want:   "api_key:sk-priority",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deviceProfileScopeKey(tt.account, tt.apiKey)
			if got != tt.want {
				t.Errorf("deviceProfileScopeKey() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeviceProfileCacheKey(t *testing.T) {
	// 测试缓存键生成是确定性的
	key1 := deviceProfileCacheKey(nil, "test-key")
	key2 := deviceProfileCacheKey(nil, "test-key")
	if key1 != key2 {
		t.Errorf("deviceProfileCacheKey() should be deterministic, got %v and %v", key1, key2)
	}

	// 测试不同 key 产生不同结果
	key3 := deviceProfileCacheKey(nil, "different-key")
	if key1 == key3 {
		t.Errorf("deviceProfileCacheKey() should produce different keys for different inputs")
	}
}

func TestIsDeviceProfileStabilizationEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  *DeviceProfileConfig
		want bool
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: false,
		},
		{
			name: "disabled",
			cfg:  &DeviceProfileConfig{StabilizeDeviceProfile: false},
			want: false,
		},
		{
			name: "enabled",
			cfg:  &DeviceProfileConfig{StabilizeDeviceProfile: true},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsDeviceProfileStabilizationEnabled(tt.cfg)
			if got != tt.want {
				t.Errorf("IsDeviceProfileStabilizationEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultDeviceProfile(t *testing.T) {
	// 测试默认配置
	cfg := &DeviceProfileConfig{}
	profile := defaultDeviceProfile(cfg)

	if profile.UserAgent != defaultDeviceProfileUserAgent {
		t.Errorf("defaultDeviceProfile() UserAgent = %v, want %v", profile.UserAgent, defaultDeviceProfileUserAgent)
	}
	if profile.PackageVersion != defaultDeviceProfilePackageVersion {
		t.Errorf("defaultDeviceProfile() PackageVersion = %v, want %v", profile.PackageVersion, defaultDeviceProfilePackageVersion)
	}
	if !profile.HasVersion {
		t.Error("defaultDeviceProfile() should have HasVersion = true")
	}

	// 测试自定义配置
	customCfg := &DeviceProfileConfig{
		UserAgent:      "custom/1.0.0",
		PackageVersion: "1.0.0",
		RuntimeVersion: "1.0.0",
		OS:             "Linux",
		Arch:           "x64",
	}
	customProfile := defaultDeviceProfile(customCfg)

	if customProfile.UserAgent != "custom/1.0.0" {
		t.Errorf("defaultDeviceProfile() with custom cfg UserAgent = %v, want %v", customProfile.UserAgent, "custom/1.0.0")
	}
}

func TestResolveDeviceProfile(t *testing.T) {
	// 清理缓存
	deviceProfileCacheMu.Lock()
	deviceProfileCache = make(map[string]deviceProfileCacheEntry)
	deviceProfileCacheMu.Unlock()

	cfg := &DeviceProfileConfig{
		UserAgent:              "codex_cli_rs/0.117.0",
		PackageVersion:         "0.117.0",
		RuntimeVersion:         "0.117.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: true,
	}

	// 测试 1: 没有 headers 时返回默认值
	t.Run("no headers returns default", func(t *testing.T) {
		profile := ResolveDeviceProfile(nil, "", nil, cfg)
		if profile.UserAgent != cfg.UserAgent {
			t.Errorf("Expected default UA, got %v", profile.UserAgent)
		}
	})

	// 测试 2: 禁用稳定化时返回默认值
	t.Run("disabled stabilization returns default", func(t *testing.T) {
		disabledCfg := &DeviceProfileConfig{StabilizeDeviceProfile: false}
		profile := ResolveDeviceProfile(nil, "", nil, disabledCfg)
		if profile.UserAgent != defaultDeviceProfileUserAgent {
			t.Errorf("Expected default UA when disabled, got %v", profile.UserAgent)
		}
	})

	// 测试 3: 有效的客户端 headers 会被缓存
	t.Run("valid client headers are cached", func(t *testing.T) {
		headers := http.Header{
			"User-Agent": []string{"codex_cli_rs/0.118.0 (Ubuntu 24.04; x86_64)"},
		}
		profile1 := ResolveDeviceProfile(nil, "test-api-key", headers, cfg)

		// 验证版本被正确提取
		if !profile1.HasVersion || profile1.Version.minor != 118 {
			t.Errorf("Expected version 0.118.0, got %+v", profile1.Version)
		}

		// 第二次调用应该返回缓存的值
		profile2 := ResolveDeviceProfile(nil, "test-api-key", headers, cfg)
		if profile2.UserAgent != profile1.UserAgent {
			t.Errorf("Expected cached profile to match")
		}
	})

	// 测试 4: 不同 API key 应该有不同的缓存
	t.Run("different api keys have different cache", func(t *testing.T) {
		headers := http.Header{
			"User-Agent": []string{"codex_cli_rs/0.118.0 (Ubuntu 24.04; x86_64)"},
		}
		profile1 := ResolveDeviceProfile(nil, "key1", headers, cfg)
		profile2 := ResolveDeviceProfile(nil, "key2", headers, cfg)

		// 两者应该有相同的 UA（因为是同一个客户端版本）
		if profile1.UserAgent != profile2.UserAgent {
			t.Errorf("Same client version should produce same UA")
		}
	})
}

func TestApplyDeviceProfileHeaders(t *testing.T) {
	profile := deviceProfile{
		UserAgent:      "test-ua/1.0.0",
		PackageVersion: "1.0.0",
		RuntimeVersion: "1.0.0",
		OS:             "Linux",
		Arch:           "x64",
	}

	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	// 设置一些已存在的头
	req.Header.Set("User-Agent", "old-ua")
	req.Header.Set("X-Stainless-Os", "old-os")
	req.Header.Set("X-Custom-Header", "should-remain")

	ApplyDeviceProfileHeaders(req, profile)

	// 验证头被正确设置
	if req.Header.Get("User-Agent") != profile.UserAgent {
		t.Errorf("User-Agent = %v, want %v", req.Header.Get("User-Agent"), profile.UserAgent)
	}
	if req.Header.Get("X-Stainless-Package-Version") != profile.PackageVersion {
		t.Errorf("X-Stainless-Package-Version = %v, want %v", req.Header.Get("X-Stainless-Package-Version"), profile.PackageVersion)
	}
	if req.Header.Get("X-Stainless-Os") != profile.OS {
		t.Errorf("X-Stainless-Os = %v, want %v", req.Header.Get("X-Stainless-Os"), profile.OS)
	}

	// 验证自定义头保留
	if req.Header.Get("X-Custom-Header") != "should-remain" {
		t.Error("X-Custom-Header should be preserved")
	}
}

func TestApplyDeviceProfileHeadersNilRequest(t *testing.T) {
	// 测试 nil 请求不 panic
	ApplyDeviceProfileHeaders(nil, deviceProfile{})
}

func TestIsCodexCodeClient(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want bool
	}{
		{
			name: "valid codex_cli_rs",
			ua:   "codex_cli_rs/0.117.0 (Mac OS 15.5.0; arm64)",
			want: true,
		},
		{
			name: "invalid ua",
			ua:   "Mozilla/5.0",
			want: false,
		},
		{
			name: "empty ua",
			ua:   "",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCodexCodeClient(tt.ua)
			if got != tt.want {
				t.Errorf("isCodexCodeClient() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPurgeExpiredDeviceProfiles(t *testing.T) {
	// 清理并设置测试数据
	deviceProfileCacheMu.Lock()
	deviceProfileCache = map[string]deviceProfileCacheEntry{
		"expired": {
			profile: deviceProfile{UserAgent: "expired"},
			expire:  time.Now().Add(-time.Hour), // 已过期
		},
		"valid": {
			profile: deviceProfile{UserAgent: "valid"},
			expire:  time.Now().Add(time.Hour), // 有效
		},
	}
	deviceProfileCacheMu.Unlock()

	purgeExpiredDeviceProfiles()

	deviceProfileCacheMu.RLock()
	_, hasExpired := deviceProfileCache["expired"]
	_, hasValid := deviceProfileCache["valid"]
	deviceProfileCacheMu.RUnlock()

	if hasExpired {
		t.Error("expired entry should be purged")
	}
	if !hasValid {
		t.Error("valid entry should be kept")
	}

	// 清理
	deviceProfileCacheMu.Lock()
	deviceProfileCache = make(map[string]deviceProfileCacheEntry)
	deviceProfileCacheMu.Unlock()
}

func TestMapStainlessOS(t *testing.T) {
	// 这个测试依赖于 runtime.GOOS，所以我们只验证它返回非空字符串
	os := mapStainlessOS()
	if os == "" {
		t.Error("mapStainlessOS() should return non-empty string")
	}

	// 验证已知映射
	validOS := map[string]bool{
		"MacOS":        true,
		"Windows":      true,
		"Linux":        true,
		"FreeBSD":      true,
	}

	// 如果不是已知值，应该以 "Other::" 开头
	if !validOS[os] && !strings.HasPrefix(os, "Other::") {
		t.Errorf("mapStainlessOS() returned unexpected value: %v", os)
	}
}

func TestMapStainlessArch(t *testing.T) {
	// 这个测试依赖于 runtime.GOARCH
	arch := mapStainlessArch()
	if arch == "" {
		t.Error("mapStainlessArch() should return non-empty string")
	}

	// 验证已知映射
	validArch := map[string]bool{
		"x64":  true,
		"arm64": true,
		"x86":  true,
	}

	// 如果不是已知值，应该以 "other::" 开头
	if !validArch[arch] && !strings.HasPrefix(arch, "other::") {
		t.Errorf("mapStainlessArch() returned unexpected value: %v", arch)
	}
}

func TestFirstNonEmptyHeader(t *testing.T) {
	tests := []struct {
		name     string
		headers  http.Header
		key      string
		fallback string
		want     string
	}{
		{
			name:     "nil headers",
			headers:  nil,
			key:      "X-Test",
			fallback: "fallback",
			want:     "fallback",
		},
		{
			name:     "empty value",
			headers:  http.Header{"X-Test": []string{""}},
			key:      "X-Test",
			fallback: "fallback",
			want:     "fallback",
		},
		{
			name:     "whitespace value",
			headers:  http.Header{"X-Test": []string{"   "}},
			key:      "X-Test",
			fallback: "fallback",
			want:     "fallback",
		},
		{
			name:     "valid value",
			headers:  http.Header{"X-Test": []string{"value"}},
			key:      "X-Test",
			fallback: "fallback",
			want:     "value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstNonEmptyHeader(tt.headers, tt.key, tt.fallback)
			if got != tt.want {
				t.Errorf("firstNonEmptyHeader() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyLegacyDeviceHeaders(t *testing.T) {
	cfg := &DeviceProfileConfig{
		UserAgent:      "codex_cli_rs/0.117.0",
		PackageVersion: "0.117.0",
		RuntimeVersion: "0.117.0",
		OS:             "MacOS",
		Arch:           "arm64",
	}

	t.Run("nil request", func(t *testing.T) {
		ApplyLegacyDeviceHeaders(nil, nil, cfg)
		// 不应该 panic
	})

	t.Run("sets default headers", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		ApplyLegacyDeviceHeaders(req, nil, cfg)

		if req.Header.Get("User-Agent") == "" {
			t.Error("User-Agent should be set")
		}
		if req.Header.Get("X-Stainless-Package-Version") == "" {
			t.Error("X-Stainless-Package-Version should be set")
		}
		if req.Header.Get("X-Stainless-Os") == "" {
			t.Error("X-Stainless-Os should be set")
		}
	})

	t.Run("preserves existing headers", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		req.Header.Set("X-Stainless-Package-Version", "existing")
		ApplyLegacyDeviceHeaders(req, nil, cfg)

		if req.Header.Get("X-Stainless-Package-Version") != "existing" {
			t.Error("X-Stainless-Package-Version should be preserved")
		}
	})

	t.Run("uses gin headers when request header is empty", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		ginHeaders := http.Header{"X-Stainless-Package-Version": []string{"from-gin"}}
		ApplyLegacyDeviceHeaders(req, ginHeaders, cfg)

		if req.Header.Get("X-Stainless-Package-Version") != "from-gin" {
			t.Errorf("X-Stainless-Package-Version should be 'from-gin', got %v", req.Header.Get("X-Stainless-Package-Version"))
		}
	})

	t.Run("detects codex client ua", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		ginHeaders := http.Header{"User-Agent": []string{"codex_cli_rs/0.118.0"}}
		ApplyLegacyDeviceHeaders(req, ginHeaders, cfg)

		if !strings.HasPrefix(req.Header.Get("User-Agent"), "codex_cli_rs") {
			t.Error("Should use client UA when it's a codex client")
		}
	})
}

// Benchmark 测试

func BenchmarkParseCodexCLIVersion(b *testing.B) {
	ua := "codex_cli_rs/0.117.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464"
	for i := 0; i < b.N; i++ {
		parseCodexCLIVersion(ua)
	}
}

func BenchmarkResolveDeviceProfile(b *testing.B) {
	// 清理缓存
	deviceProfileCacheMu.Lock()
	deviceProfileCache = make(map[string]deviceProfileCacheEntry)
	deviceProfileCacheMu.Unlock()

	cfg := &DeviceProfileConfig{
		UserAgent:              "codex_cli_rs/0.117.0",
		PackageVersion:         "0.117.0",
		RuntimeVersion:         "0.117.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: true,
	}
	headers := http.Header{
		"User-Agent": []string{"codex_cli_rs/0.118.0 (Ubuntu 24.04; x86_64)"},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ResolveDeviceProfile(nil, "test-key", headers, cfg)
	}
}

func BenchmarkApplyDeviceProfileHeaders(b *testing.B) {
	profile := deviceProfile{
		UserAgent:      "codex_cli_rs/0.117.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
		PackageVersion: "0.117.0",
		RuntimeVersion: "0.117.0",
		OS:             "MacOS",
		Arch:           "arm64",
	}

	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		ApplyDeviceProfileHeaders(req, profile)
	}
}
