package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"unicode"

	"github.com/codex2api/auth"
)

// ==================== 动态 User-Agent 生成 ====================
//
// 真实 Codex CLI/TUI 的 UA 格式（源码: codex-rs/login/src/auth/default_client.rs）：
//   {originator}/{version} ({OS} {OS_version}; {arch}) {terminal} ({originator}; {version})
//
// 示例：
//   codex-tui/0.153.3 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.153.3)
//   codex-tui/0.144.0-alpha.10 (Mac OS 13.7.8; arm64) xterm-256color (codex-tui; 0.144.0-alpha.10)
//   Codex Desktop/0.153.3 (Windows 10.0.26100; x86_64) unknown (Codex Desktop; 26.901.41123)
//
// originator 可以含空格（ChatGPT 桌面端就是 "Codex Desktop"），且真实客户端的
// UA 前缀与 Originator 头恒为同一标识。

// ClientProfile 表示一个模拟客户端的完整身份
type ClientProfile struct {
	UserAgent string // 完整的 User-Agent 字符串
	Version   string // codex CLI 版本（需与 UA 中的版本一致）
}

const (
	latestCodexClientName          = "codex-tui"
	latestCodexCLIVersion          = "0.160.0"
	latestCodexCLIUserAgentPrefix  = "codex-tui/" + latestCodexCLIVersion
	defaultCodexUserAgentOSName    = "Mac OS"
	defaultCodexUserAgentOSVersion = "15.5.0"
	defaultCodexUserAgentArch      = "arm64"
	defaultCodexUserAgentTerminal  = "xterm-256color"
	defaultCodexCLIUserAgent       = latestCodexCLIUserAgentPrefix + " (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; " + latestCodexCLIVersion + ")"
)

type CodexUserAgentConfig struct {
	RawUserAgent  string `json:"raw_user_agent,omitempty"`
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	OSName        string `json:"os_name,omitempty"`
	OSVersion     string `json:"os_version,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Terminal      string `json:"terminal,omitempty"`
	// ClientKind 是客户端形态预设(见 codex_ua_catalog.go);空则按 client_name 推断。
	ClientKind string `json:"client_kind,omitempty"`
	// AppName / AppVersion 是末尾标记 "(app名; app版本)";留空按形态自动推导,
	// 桌面端与 VS Code 插件的构建号会按 CLI 版本从目录配对。
	AppName    string `json:"app_name,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
	// Mode 为 "pool" 时忽略单画像字段,按 PoolMix 配比为每个账号确定性抽取目录画像。
	Mode    string         `json:"mode,omitempty"`
	PoolMix map[string]int `json:"pool_mix,omitempty"`
}

var codexOfficialClientUserAgentPrefixes = []string{
	"codex-tui/",
	"codex_cli_rs/",
	"codex_vscode/",
	"codex_app/",
	"codex_chatgpt_desktop/",
	"codex_atlas/",
	"codex_exec/",
	"codex_sdk_ts/",
	"codex ",
	"opencode/",
}

var codexStrictOfficialClientUserAgentPrefixes = []string{
	"codex-tui/",
	"codex_cli_rs/",
	"codex_vscode/",
	"codex_app/",
	"codex_chatgpt_desktop/",
	"codex_atlas/",
	"codex_exec/",
	"codex_sdk_ts/",
}

const codexStrictSpacedUserAgentPrefix = "codex "

// Third-party CLIs that ChatGPT-backend Codex accepts as first-party originators.
// opencode advertises itself via Originator: "opencode" and reaching upstream with
// that identity is required for features like reasoning_effort=xhigh to take effect.
var codexOfficialClientOriginatorPrefixes = []string{
	"codex_",
	"codex-tui",
	"codex ",
	"opencode",
}

var codexStrictOfficialClientOriginators = []string{
	"codex-tui",
	"codex_cli_rs",
	"codex_vscode",
	"codex_app",
	"codex_chatgpt_desktop",
	"codex_atlas",
	"codex_exec",
	"codex_sdk_ts",
}

func IsCodexOfficialClientByHeaders(userAgent, originator string) bool {
	return matchCodexClientHeaderPrefixes(userAgent, codexOfficialClientUserAgentPrefixes) ||
		matchCodexClientHeaderPrefixes(originator, codexOfficialClientOriginatorPrefixes)
}

func IsCodexStrictOfficialClientByHeaders(userAgent, originator string) bool {
	return matchCodexClientHeaderPrefixStrict(userAgent, codexStrictOfficialClientUserAgentPrefixes) ||
		matchCodexSpacedUserAgentStrict(userAgent) ||
		matchCodexClientHeaderExact(originator, codexStrictOfficialClientOriginators)
}

func LatestCodexCLIVersionForHeaders() string {
	return effectiveLatestCodexCLIVersion()
}

// effectiveLatestCodexCLIVersion 返回当前生效的"最新 Codex CLI 版本"：
// 取内置常量与远端同步版本(CodexSyncedCLIVersion)中的较大者，绝不低于内置常量，
// 因此过期或非法的远端值不会导致降级。
func effectiveLatestCodexCLIVersion() string {
	synced := normalizeCodexClientVersionText(CurrentRuntimeSettings().CodexSyncedCLIVersion)
	if synced == "" {
		return latestCodexCLIVersion
	}
	if cmp, ok := compareCodexClientVersions(synced, latestCodexCLIVersion); ok && cmp > 0 {
		return synced
	}
	return latestCodexCLIVersion
}

func MinimalCodexCLIUserAgentForHeaders() string {
	return replaceCodexUserAgentVersion(defaultCodexCLIUserAgent, effectiveLatestCodexCLIVersion())
}

// ApplyCodexModelDiscoveryHeaders applies the identity headers used by Codex
// when it discovers models. Some Responses gateways apply their official
// client policy to GET /v1/models before any request body exists, so the
// installation ID cannot rely on client_metadata from /v1/responses.
//
// account 可为空；身份解析和账号覆盖规则与对话请求保持一致。
func ApplyCodexModelDiscoveryHeaders(headers http.Header, account *auth.Account, baseURL, apiKey string) error {
	if headers == nil {
		return nil
	}
	identity, err := ResolveCodexMaintenanceIdentity(account, nil)
	if err != nil {
		return err
	}
	identity.Apply(headers)
	if identity.Version != "" {
		headers.Set("Version", identity.Version)
		headers.Set("x-codex-app-version", identity.Version)
	}
	headers.Set(codexInstallationIDHeader, codexRelayInstallationID(baseURL, apiKey))
	return nil
}

// codexRelayInstallationID 为中转对话和模型发现提供同源的设备标识。
// 真实客户端的 installation id 是 UUIDv4，与 deriveStableCodexUUID 一致。
func codexRelayInstallationID(baseURL, apiKey string) string {
	baseURL = strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	return deriveStableCodexUUID("codex2api:openai-responses-installation:v1:" + baseURL + "|" + strings.TrimSpace(apiKey))
}

func DefaultCodexUserAgentConfigJSON() string {
	return "{}"
}

func NormalizeCodexUserAgentConfigJSON(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultCodexUserAgentConfigJSON(), nil
	}
	var cfg CodexUserAgentConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", errors.New("codex_user_agent_config must be a JSON object")
	}
	cfg = normalizeCodexUserAgentConfig(cfg)
	if err := validateCodexUserAgentConfig(cfg); err != nil {
		return "", err
	}
	if isEmptyCodexUserAgentConfig(cfg) {
		return DefaultCodexUserAgentConfigJSON(), nil
	}
	buf, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func normalizeCodexUserAgentConfig(cfg CodexUserAgentConfig) CodexUserAgentConfig {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == CodexUserAgentModeSingle {
		mode = ""
	}
	var poolMix map[string]int
	for k, v := range cfg.PoolMix {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if poolMix == nil {
			poolMix = map[string]int{}
		}
		poolMix[k] = v
	}
	return CodexUserAgentConfig{
		RawUserAgent:  strings.TrimSpace(cfg.RawUserAgent),
		ClientName:    normalizeCodexUserAgentClientName(cfg.ClientName),
		ClientVersion: normalizeCodexClientVersionText(cfg.ClientVersion),
		OSName:        strings.TrimSpace(cfg.OSName),
		OSVersion:     strings.TrimSpace(cfg.OSVersion),
		Arch:          strings.TrimSpace(cfg.Arch),
		Terminal:      strings.TrimSpace(cfg.Terminal),
		ClientKind:    strings.ToLower(strings.TrimSpace(cfg.ClientKind)),
		AppName:       normalizeCodexUserAgentClientName(cfg.AppName),
		AppVersion:    strings.TrimSpace(cfg.AppVersion),
		Mode:          mode,
		PoolMix:       poolMix,
	}
}

func validateCodexUserAgentConfig(cfg CodexUserAgentConfig) error {
	if cfg.RawUserAgent != "" {
		if !validHTTPHeaderValue(cfg.RawUserAgent) {
			return errors.New("codex raw User-Agent contains invalid HTTP header characters")
		}
	}
	if cfg.ClientVersion != "" && !validCodexClientVersionString(cfg.ClientVersion) {
		return errors.New("codex User-Agent client_version must be a semantic version like 0.153.3 or 0.144.0-alpha.10")
	}
	if cfg.ClientName != "" && !validCodexUserAgentClientName(cfg.ClientName) {
		return errors.New("codex User-Agent client_name contains invalid characters")
	}
	if cfg.AppName != "" && !validCodexUserAgentClientName(cfg.AppName) {
		return errors.New("codex User-Agent app_name contains invalid characters")
	}
	if _, ok := normalizeCodexClientKind(cfg.ClientKind); !ok {
		return errors.New("codex User-Agent client_kind must be one of codex-tui, codex-desktop, codex-vscode, codex-exec, custom")
	}
	if cfg.Mode != "" && cfg.Mode != CodexUserAgentModePool {
		return errors.New("codex User-Agent mode must be single or pool")
	}
	for kind, weight := range cfg.PoolMix {
		if k, ok := normalizeCodexClientKind(kind); !ok || k == "" || k == CodexClientKindCustom {
			return fmt.Errorf("codex User-Agent pool_mix has unknown client kind %q", kind)
		}
		if weight < 0 {
			return fmt.Errorf("codex User-Agent pool_mix weight for %s must not be negative", kind)
		}
	}
	if cfg.Mode == CodexUserAgentModePool && len(codexPoolMix(cfg)) == 0 {
		return errors.New("codex User-Agent pool_mix needs at least one positive weight")
	}
	tokenFields := map[string]string{
		"client_version": cfg.ClientVersion,
		"app_version":    cfg.AppVersion,
		"arch":           cfg.Arch,
		"terminal":       cfg.Terminal,
	}
	for name, value := range tokenFields {
		if value == "" {
			continue
		}
		if !validCodexUserAgentToken(value) {
			return fmt.Errorf("codex User-Agent %s contains invalid characters", name)
		}
	}
	for name, value := range map[string]string{"os_name": cfg.OSName, "os_version": cfg.OSVersion} {
		if value == "" {
			continue
		}
		if !validCodexUserAgentPlatformPart(value) {
			return fmt.Errorf("codex User-Agent %s contains invalid characters", name)
		}
	}
	return nil
}

func validHTTPHeaderValue(value string) bool {
	for _, r := range value {
		if r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validCodexUserAgentToken(value string) bool {
	if !validHTTPHeaderValue(value) {
		return false
	}
	return !strings.ContainsAny(value, " \t();")
}

func validCodexUserAgentPlatformPart(value string) bool {
	if !validHTTPHeaderValue(value) {
		return false
	}
	return !strings.ContainsAny(value, "();")
}

// validCodexUserAgentClientName 校验 UA 前缀里的客户端名。与 arch/terminal 不同，
// 它允许空格：ChatGPT 桌面端的 originator 就是 "Codex Desktop"（issue #653）。
// 仍然拒绝会破坏 "{name}/{version} (...)" 形状的括号、分号与控制字符。
func validCodexUserAgentClientName(value string) bool {
	if !validHTTPHeaderValue(value) {
		return false
	}
	return !strings.ContainsAny(value, "\t();")
}

// normalizeCodexUserAgentClientName 去掉首尾空白并把内部连续空白折叠成单个空格，
// 避免拼出 "Codex  Desktop/..." 这种真实客户端不会产生的双空格。
func normalizeCodexUserAgentClientName(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// CodexOriginatorForGeneratedUserAgent 返回与网关自行生成的 User-Agent 相匹配的
// Originator 头。真实 Codex 客户端的 UA 前缀与 Originator 恒为同一标识（源码
// codex-rs default_client.rs），模拟 "Codex Desktop" 之类客户端时两者必须一致，
// 否则上游看到的是 UA 说桌面端、Originator 说 codex-tui 的自相矛盾指纹。
// 前缀不是已知官方标识（例如自定义路由名）时回退到默认 Originator。
func CodexOriginatorForGeneratedUserAgent(userAgent string) string {
	name := codexUserAgentClientName(userAgent)
	if name != "" && IsCodexOfficialClientByHeaders("", name) {
		return name
	}
	return Originator
}

// codexUserAgentClientName 取 UA 首个 "/" 之前的客户端名；没有 "/" 视为无法识别。
func codexUserAgentClientName(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	idx := strings.IndexByte(userAgent, '/')
	if idx <= 0 {
		return ""
	}
	return strings.TrimSpace(userAgent[:idx])
}

func isEmptyCodexUserAgentConfig(cfg CodexUserAgentConfig) bool {
	return cfg.RawUserAgent == "" &&
		cfg.ClientName == "" &&
		cfg.ClientVersion == "" &&
		cfg.OSName == "" &&
		cfg.OSVersion == "" &&
		cfg.Arch == "" &&
		cfg.Terminal == "" &&
		cfg.ClientKind == "" &&
		cfg.AppName == "" &&
		cfg.AppVersion == "" &&
		cfg.Mode == "" &&
		len(cfg.PoolMix) == 0
}

func codexUserAgentConfigFromJSON(raw string) CodexUserAgentConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return CodexUserAgentConfig{}
	}
	var cfg CodexUserAgentConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return CodexUserAgentConfig{}
	}
	cfg = normalizeCodexUserAgentConfig(cfg)
	if err := validateCodexUserAgentConfig(cfg); err != nil {
		return CodexUserAgentConfig{}
	}
	return cfg
}

func validCodexClientVersionString(value string) bool {
	_, _, ok := parseCodexClientVersionDetails(latestCodexClientName + "/" + normalizeCodexClientVersionText(value))
	return ok
}

// codexUserAgentFromConfigChecked 按全局 UA 配置生成出站身份。accountID 只在号池模式下参与
// 画像抽取(同一账号恒得同一画像)。
func codexUserAgentFromConfigChecked(raw string, accountID int64, versionFloor string) (string, string, bool, error) {
	cfg := codexUserAgentConfigFromJSON(raw)
	if isEmptyCodexUserAgentConfig(cfg) {
		return "", "", false, nil
	}
	if cfg.RawUserAgent != "" {
		return cfg.RawUserAgent, codexVersionFromUserAgent(cfg.RawUserAgent, cfg.ClientVersion), true, nil
	}
	if cfg.Mode == CodexUserAgentModePool {
		return codexPoolPersona(cfg, accountID, versionFloor)
	}
	return buildCodexStructuredUserAgent(cfg, versionFloor)
}

// buildCodexStructuredUserAgent 按形态预设填补留空字段并拼出 UA:客户端名、末尾标记名、
// 平台与终端取形态默认值,CLI 版本与构建号由 resolveCodexCurrentVersions 选择完整配对。
// 未指定形态且客户端名认不出时按 custom 处理,末尾标记复用客户端名与 CLI 版本。
func buildCodexStructuredUserAgent(cfg CodexUserAgentConfig, versionFloor string) (userAgent, version string, ok bool, err error) {
	kind := effectiveCodexClientKind(cfg)
	spec, hasSpec := codexUAKindSpecFor(kind)
	clientName := strings.TrimSpace(cfg.ClientName)
	platform := codexUAPlatform{OSName: defaultCodexUserAgentOSName, OSVersion: defaultCodexUserAgentOSVersion, Arch: defaultCodexUserAgentArch}
	terminal := defaultCodexUserAgentTerminal
	appName := ""
	if hasSpec {
		clientName = firstNonEmptyString(clientName, spec.ClientName)
		platform = spec.DefaultPlatform
		terminal = spec.DefaultTerminal
		if spec.AppFollowsCLI {
			appName = clientName
		} else if len(spec.AppNames) > 0 {
			appName = spec.AppNames[0].Value
		}
	} else {
		clientName = firstNonEmptyString(clientName, latestCodexClientName)
		appName = clientName
	}
	osName := firstNonEmptyString(cfg.OSName, platform.OSName)
	cliVersion, appVersion, err := resolveCodexCurrentVersions(spec, codexVersionSelection{
		OSName: osName, Arch: firstNonEmptyString(cfg.Arch, platform.Arch), CLIOverride: cfg.ClientVersion, AppOverride: cfg.AppVersion,
		VersionFloor: versionFloor,
	})
	if err != nil {
		return "", "", false, err
	}
	appName = firstNonEmptyString(cfg.AppName, appName)
	osVersion := firstNonEmptyString(cfg.OSVersion, platform.OSVersion)
	arch := firstNonEmptyString(cfg.Arch, platform.Arch)
	terminal = firstNonEmptyString(cfg.Terminal, terminal)
	return formatCodexUserAgentWithApp(clientName, cliVersion, osName, osVersion, arch, terminal, appName, appVersion), cliVersion, true, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func formatCodexUserAgent(clientName, clientVersion, osName, osVersion, arch, terminal string) string {
	clientName = firstNonEmptyString(clientName, latestCodexClientName)
	clientVersion = firstNonEmptyString(clientVersion, latestCodexCLIVersion)
	osName = firstNonEmptyString(osName, defaultCodexUserAgentOSName)
	osVersion = firstNonEmptyString(osVersion, defaultCodexUserAgentOSVersion)
	arch = firstNonEmptyString(arch, defaultCodexUserAgentArch)
	terminal = firstNonEmptyString(terminal, defaultCodexUserAgentTerminal)
	return formatCodexUserAgentWithApp(clientName, clientVersion, osName, osVersion, arch, terminal, clientName, clientVersion)
}

// formatCodexUserAgentWithApp 拼出完整形状,末尾标记可与前缀不同(桌面端 / VS Code 插件)。
func formatCodexUserAgentWithApp(clientName, clientVersion, osName, osVersion, arch, terminal, appName, appVersion string) string {
	appName = firstNonEmptyString(appName, clientName)
	appVersion = firstNonEmptyString(appVersion, clientVersion)
	platform := strings.TrimSpace(osName + " " + osVersion)
	return fmt.Sprintf("%s/%s (%s; %s) %s (%s; %s)", clientName, clientVersion, platform, arch, terminal, appName, appVersion)
}

func normalizeCodexClientVersionText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if value[0] == 'v' || value[0] == 'V' {
		return value[1:]
	}
	return value
}

func compareCodexClientVersions(current, floor string) (int, bool) {
	currentVersion, currentPrerelease, ok := codexClientVersionParts(current)
	if !ok {
		return 0, false
	}
	floorVersion, floorPrerelease, ok := codexClientVersionParts(floor)
	if !ok {
		return 0, false
	}
	if cmp := currentVersion.Compare(floorVersion); cmp != 0 {
		return cmp, true
	}
	return compareSemverPrerelease(currentPrerelease, floorPrerelease), true
}

func codexClientVersionParts(value string) (cliVersion, string, bool) {
	version, rawVersion, ok := parseCodexClientVersionDetails(latestCodexClientName + "/" + normalizeCodexClientVersionText(value))
	if !ok {
		return cliVersion{}, "", false
	}
	if idx := strings.IndexByte(rawVersion, '-'); idx >= 0 {
		return version, rawVersion[idx+1:], true
	}
	return version, "", true
}

func compareSemverPrerelease(current, floor string) int {
	if current == "" && floor == "" {
		return 0
	}
	if current == "" {
		return 1
	}
	if floor == "" {
		return -1
	}

	currentParts := strings.Split(current, ".")
	floorParts := strings.Split(floor, ".")
	for i := 0; i < len(currentParts) && i < len(floorParts); i++ {
		currentPart := currentParts[i]
		floorPart := floorParts[i]
		currentNumeric := isNumericSemverIdentifier(currentPart)
		floorNumeric := isNumericSemverIdentifier(floorPart)
		switch {
		case currentNumeric && floorNumeric:
			if cmp := compareNumericSemverIdentifier(currentPart, floorPart); cmp != 0 {
				return cmp
			}
		case currentNumeric:
			return -1
		case floorNumeric:
			return 1
		case currentPart != floorPart:
			if currentPart > floorPart {
				return 1
			}
			return -1
		}
	}
	switch {
	case len(currentParts) > len(floorParts):
		return 1
	case len(currentParts) < len(floorParts):
		return -1
	default:
		return 0
	}
}

func isNumericSemverIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func compareNumericSemverIdentifier(current, floor string) int {
	current = strings.TrimLeft(current, "0")
	floor = strings.TrimLeft(floor, "0")
	if current == "" {
		current = "0"
	}
	if floor == "" {
		floor = "0"
	}
	switch {
	case len(current) > len(floor):
		return 1
	case len(current) < len(floor):
		return -1
	case current > floor:
		return 1
	case current < floor:
		return -1
	default:
		return 0
	}
}

func replaceCodexUserAgentVersion(userAgent, version string) string {
	_, rawVersion, ok := parseCodexClientVersionDetails(userAgent)
	if !ok || rawVersion == "" || rawVersion == version {
		return userAgent
	}
	return strings.ReplaceAll(userAgent, rawVersion, version)
}

func codexProfileUserAgent(osName, osVersion, arch, terminal string) string {
	return formatCodexUserAgent(latestCodexClientName, latestCodexCLIVersion, osName, osVersion, arch, terminal)
}

func matchCodexClientHeaderPrefixes(value string, prefixes []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(value, prefix) || strings.Contains(value, prefix) {
			return true
		}
	}
	return false
}

func matchCodexClientHeaderPrefixStrict(value string, prefixes []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func matchCodexSpacedUserAgentStrict(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(value, codexStrictSpacedUserAgentPrefix) {
		return false
	}
	remainder := strings.TrimSpace(strings.TrimPrefix(value, codexStrictSpacedUserAgentPrefix))
	return remainder != ""
}

func matchCodexClientHeaderExact(value string, allowed []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, candidate := range allowed {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate != "" && value == candidate {
			return true
		}
	}
	return false
}

// 预定义的真实客户端画像池
// 按开发者常见环境分布：macOS（主力） > Linux > Windows
var clientProfiles = []ClientProfile{
	// ---- macOS arm64（最常见：Apple Silicon 开发者） ----
	{codexProfileUserAgent("Mac OS", "15.5.0", "arm64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.4.1", "arm64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.3.0", "arm64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.5.0", "arm64", "Apple_Terminal/464"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.2.0", "arm64", "iTerm.app/3.5.10"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.5.0", "arm64", "vscode/1.100.0"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.4.0", "arm64", "tmux/3.5a"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "14.7.4", "arm64", "xterm-256color"), latestCodexCLIVersion},
	// ---- macOS x86_64（少量 Intel Mac） ----
	{codexProfileUserAgent("Mac OS", "15.4.0", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "14.7.0", "x86_64", "iTerm.app/3.5.8"), latestCodexCLIVersion},
	// ---- Linux（服务器和开发工作站） ----
	{codexProfileUserAgent("Linux", "Unknown", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Ubuntu", "24.04", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Ubuntu", "24.10", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Arch Linux", "Rolling", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Fedora Linux", "41", "x86_64", "vscode/1.100.0"), latestCodexCLIVersion},
	// ---- Windows ----
	{codexProfileUserAgent("Windows", "10.0.26120", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Windows", "10.0.22631", "x86_64", "WindowsTerminal"), latestCodexCLIVersion},
	// ---- 备用终端画像（保持最新 Codex CLI 版本） ----
	{codexProfileUserAgent("Mac OS", "15.5.0", "arm64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.3.0", "arm64", "Ghostty/1.1.0"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.4.0", "arm64", "vscode/1.98.0"), latestCodexCLIVersion},
	{codexProfileUserAgent("Ubuntu", "24.04", "x86_64", "xterm-256color"), latestCodexCLIVersion},
}

// ProfileForAccount 根据账号 ID 确定性地选择一个 ClientProfile
// 同一个账号永远返回相同的 profile，不同账号大概率返回不同的 profile
func ProfileForAccount(accountID int64) ClientProfile {
	if len(clientProfiles) == 0 {
		return ClientProfile{
			UserAgent: defaultCodexCLIUserAgent,
			Version:   latestCodexCLIVersion,
		}
	}

	// 用 FNV hash 将 accountID 映射到 profile 池，确保分布均匀
	h := fnv.New32a()
	fmt.Fprintf(h, "codex2api:ua-profile:%d", accountID)
	idx := int(h.Sum32()) % len(clientProfiles)
	if idx < 0 {
		idx = -idx
	}

	return clientProfiles[idx]
}
