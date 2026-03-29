package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
)

const (
	defaultDeviceProfileUserAgent      = "codex_cli_rs/0.117.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464"
	defaultDeviceProfilePackageVersion = "0.117.0"
	defaultDeviceProfileRuntimeVersion = "0.117.0"
	defaultDeviceProfileOS             = "MacOS"
	defaultDeviceProfileArch           = "arm64"
	deviceProfileTTL                   = 7 * 24 * time.Hour
	deviceProfileCleanupPeriod         = time.Hour
)

var (
	codexCLIVersionPattern = regexp.MustCompile(`^codex_cli_rs/(\d+)\.(\d+)\.(\d+)`)

	deviceProfileCache            = make(map[string]deviceProfileCacheEntry)
	deviceProfileCacheMu          sync.RWMutex
	deviceProfileCacheCleanupOnce sync.Once
)

// DeviceProfileConfig 设备指纹配置
type DeviceProfileConfig struct {
	UserAgent              string
	PackageVersion         string
	RuntimeVersion         string
	OS                     string
	Arch                   string
	StabilizeDeviceProfile bool
}

// CLIVersion 表示 Codex CLI 版本
type cliVersion struct {
	major int
	minor int
	patch int
}

func (v cliVersion) Compare(other cliVersion) int {
	switch {
	case v.major != other.major:
		if v.major > other.major {
			return 1
		}
		return -1
	case v.minor != other.minor:
		if v.minor > other.minor {
			return 1
		}
		return -1
	case v.patch != other.patch:
		if v.patch > other.patch {
			return 1
		}
		return -1
	default:
		return 0
	}
}

// DeviceProfile 设备指纹配置
type deviceProfile struct {
	UserAgent      string
	PackageVersion string
	RuntimeVersion string
	OS             string
	Arch           string
	Version        cliVersion
	HasVersion     bool
}

type deviceProfileCacheEntry struct {
	profile deviceProfile
	expire  time.Time
}

// IsDeviceProfileStabilizationEnabled 检查设备指纹稳定化是否启用
func IsDeviceProfileStabilizationEnabled(cfg *DeviceProfileConfig) bool {
	if cfg == nil {
		return false
	}
	return cfg.StabilizeDeviceProfile
}

func defaultDeviceProfile(cfg *DeviceProfileConfig) deviceProfile {
	// 如果 cfg 为 nil，使用默认空配置兜底
	if cfg == nil {
		cfg = &DeviceProfileConfig{}
	}

	hdrDefault := func(cfgVal, fallback string) string {
		if strings.TrimSpace(cfgVal) != "" {
			return strings.TrimSpace(cfgVal)
		}
		return fallback
	}

	profile := deviceProfile{
		UserAgent:      hdrDefault(cfg.UserAgent, defaultDeviceProfileUserAgent),
		PackageVersion: hdrDefault(cfg.PackageVersion, defaultDeviceProfilePackageVersion),
		RuntimeVersion: hdrDefault(cfg.RuntimeVersion, defaultDeviceProfileRuntimeVersion),
		OS:             hdrDefault(cfg.OS, defaultDeviceProfileOS),
		Arch:           hdrDefault(cfg.Arch, defaultDeviceProfileArch),
	}
	if version, ok := parseCodexCLIVersion(profile.UserAgent); ok {
		profile.Version = version
		profile.HasVersion = true
	}
	return profile
}

// mapStainlessOS maps runtime.GOOS to Stainless SDK OS names.
func mapStainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	default:
		return "Other::" + runtime.GOOS
	}
}

// mapStainlessArch maps runtime.GOARCH to Stainless SDK architecture names.
func mapStainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	default:
		return "other::" + runtime.GOARCH
	}
}

func parseCodexCLIVersion(userAgent string) (cliVersion, bool) {
	matches := codexCLIVersionPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(matches) != 4 {
		return cliVersion{}, false
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return cliVersion{}, false
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return cliVersion{}, false
	}
	patch, err := strconv.Atoi(matches[3])
	if err != nil {
		return cliVersion{}, false
	}
	return cliVersion{major: major, minor: minor, patch: patch}, true
}

func shouldUpgradeDeviceProfile(candidate, current deviceProfile) bool {
	if candidate.UserAgent == "" || !candidate.HasVersion {
		return false
	}
	if current.UserAgent == "" || !current.HasVersion {
		return true
	}
	return candidate.Version.Compare(current.Version) > 0
}

func pinDeviceProfilePlatform(profile, baseline deviceProfile) deviceProfile {
	profile.OS = baseline.OS
	profile.Arch = baseline.Arch
	return profile
}

// normalizeDeviceProfile keeps stabilized profiles pinned to the current
// baseline platform and enforces the baseline software fingerprint as a floor.
func normalizeDeviceProfile(profile, baseline deviceProfile) deviceProfile {
	profile = pinDeviceProfilePlatform(profile, baseline)
	if profile.UserAgent == "" || !profile.HasVersion || shouldUpgradeDeviceProfile(baseline, profile) {
		profile.UserAgent = baseline.UserAgent
		profile.PackageVersion = baseline.PackageVersion
		profile.RuntimeVersion = baseline.RuntimeVersion
		profile.Version = baseline.Version
		profile.HasVersion = baseline.HasVersion
	}
	return profile
}

func extractDeviceProfile(headers http.Header, cfg *DeviceProfileConfig) (deviceProfile, bool) {
	if headers == nil {
		return deviceProfile{}, false
	}

	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	version, ok := parseCodexCLIVersion(userAgent)
	if !ok {
		return deviceProfile{}, false
	}

	baseline := defaultDeviceProfile(cfg)
	profile := deviceProfile{
		UserAgent:      userAgent,
		PackageVersion: firstNonEmptyHeader(headers, "X-Stainless-Package-Version", baseline.PackageVersion),
		RuntimeVersion: firstNonEmptyHeader(headers, "X-Stainless-Runtime-Version", baseline.RuntimeVersion),
		OS:             firstNonEmptyHeader(headers, "X-Stainless-Os", baseline.OS),
		Arch:           firstNonEmptyHeader(headers, "X-Stainless-Arch", baseline.Arch),
		Version:        version,
		HasVersion:     true,
	}
	return profile, true
}

func firstNonEmptyHeader(headers http.Header, name, fallback string) string {
	if headers == nil {
		return fallback
	}
	if value := strings.TrimSpace(headers.Get(name)); value != "" {
		return value
	}
	return fallback
}

// deviceProfileScopeKey 生成设备指纹作用域键
// 优先使用 apiKey（细粒度控制），其次是 auth ID（账号级），最后是 global
func deviceProfileScopeKey(account *auth.Account, apiKey string) string {
	switch {
	case strings.TrimSpace(apiKey) != "":
		return "api_key:" + strings.TrimSpace(apiKey)
	case account != nil && account.ID() != 0:
		return "auth:" + strconv.FormatInt(account.ID(), 10)
	default:
		return "global"
	}
}

func deviceProfileCacheKey(account *auth.Account, apiKey string) string {
	sum := sha256.Sum256([]byte(deviceProfileScopeKey(account, apiKey)))
	return hex.EncodeToString(sum[:])
}

func startDeviceProfileCacheCleanup() {
	go func() {
		ticker := time.NewTicker(deviceProfileCleanupPeriod)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredDeviceProfiles()
		}
	}()
}

func purgeExpiredDeviceProfiles() {
	now := time.Now()
	deviceProfileCacheMu.Lock()
	for key, entry := range deviceProfileCache {
		if !entry.expire.After(now) {
			delete(deviceProfileCache, key)
		}
	}
	deviceProfileCacheMu.Unlock()
}

// ResolveDeviceProfile 解析设备指纹配置
// 这是主要的入口函数，根据配置和请求头返回稳定的设备指纹
func ResolveDeviceProfile(account *auth.Account, apiKey string, headers http.Header, cfg *DeviceProfileConfig) deviceProfile {
	if !IsDeviceProfileStabilizationEnabled(cfg) {
		return defaultDeviceProfile(cfg)
	}

	deviceProfileCacheCleanupOnce.Do(startDeviceProfileCacheCleanup)

	cacheKey := deviceProfileCacheKey(account, apiKey)
	now := time.Now()
	baseline := defaultDeviceProfile(cfg)
	candidate, hasCandidate := extractDeviceProfile(headers, cfg)
	if hasCandidate {
		candidate = pinDeviceProfilePlatform(candidate, baseline)
	}
	if hasCandidate && !shouldUpgradeDeviceProfile(candidate, baseline) {
		hasCandidate = false
	}

	deviceProfileCacheMu.RLock()
	entry, hasCached := deviceProfileCache[cacheKey]
	cachedValid := hasCached && entry.expire.After(now) && entry.profile.UserAgent != ""
	deviceProfileCacheMu.RUnlock()

	if hasCandidate {
		deviceProfileCacheMu.Lock()
		entry, hasCached = deviceProfileCache[cacheKey]
		cachedValid = hasCached && entry.expire.After(now) && entry.profile.UserAgent != ""
		if cachedValid {
			entry.profile = normalizeDeviceProfile(entry.profile, baseline)
		}
		if cachedValid && !shouldUpgradeDeviceProfile(candidate, entry.profile) {
			entry.expire = now.Add(deviceProfileTTL)
			deviceProfileCache[cacheKey] = entry
			deviceProfileCacheMu.Unlock()
			return entry.profile
		}

		deviceProfileCache[cacheKey] = deviceProfileCacheEntry{
			profile: candidate,
			expire:  now.Add(deviceProfileTTL),
		}
		deviceProfileCacheMu.Unlock()
		return candidate
	}

	if cachedValid {
		deviceProfileCacheMu.Lock()
		entry = deviceProfileCache[cacheKey]
		if entry.expire.After(now) && entry.profile.UserAgent != "" {
			entry.profile = normalizeDeviceProfile(entry.profile, baseline)
			entry.expire = now.Add(deviceProfileTTL)
			deviceProfileCache[cacheKey] = entry
			deviceProfileCacheMu.Unlock()
			return entry.profile
		}
		deviceProfileCacheMu.Unlock()
	}

	return baseline
}

// ApplyDeviceProfileHeaders 将设备指纹应用到 HTTP 请求头
func ApplyDeviceProfileHeaders(r *http.Request, profile deviceProfile) {
	if r == nil {
		return
	}
	for _, headerName := range []string{
		"User-Agent",
		"X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Os",
		"X-Stainless-Arch",
	} {
		r.Header.Del(headerName)
	}
	r.Header.Set("User-Agent", profile.UserAgent)
	r.Header.Set("X-Stainless-Package-Version", profile.PackageVersion)
	r.Header.Set("X-Stainless-Runtime-Version", profile.RuntimeVersion)
	r.Header.Set("X-Stainless-Os", profile.OS)
	r.Header.Set("X-Stainless-Arch", profile.Arch)
}

// ApplyLegacyDeviceHeaders 应用传统设备头（向后兼容）
func ApplyLegacyDeviceHeaders(r *http.Request, ginHeaders http.Header, cfg *DeviceProfileConfig) {
	if r == nil {
		return
	}
	profile := defaultDeviceProfile(cfg)
	miscEnsure := func(name, fallback string) {
		if strings.TrimSpace(r.Header.Get(name)) != "" {
			return
		}
		if strings.TrimSpace(ginHeaders.Get(name)) != "" {
			r.Header.Set(name, strings.TrimSpace(ginHeaders.Get(name)))
			return
		}
		r.Header.Set(name, fallback)
	}

	miscEnsure("X-Stainless-Runtime-Version", profile.RuntimeVersion)
	miscEnsure("X-Stainless-Package-Version", profile.PackageVersion)
	miscEnsure("X-Stainless-Os", mapStainlessOS())
	miscEnsure("X-Stainless-Arch", mapStainlessArch())

	// Legacy mode preserves per-auth custom header overrides.
	if strings.TrimSpace(r.Header.Get("User-Agent")) != "" {
		return
	}

	clientUA := ""
	if ginHeaders != nil {
		clientUA = strings.TrimSpace(ginHeaders.Get("User-Agent"))
	}
	if isCodexCodeClient(clientUA) {
		r.Header.Set("User-Agent", clientUA)
		return
	}
	r.Header.Set("User-Agent", profile.UserAgent)
}

// isCodexCodeClient 检查 User-Agent 是否是 Codex CLI 客户端
func isCodexCodeClient(userAgent string) bool {
	return codexCLIVersionPattern.MatchString(userAgent)
}
