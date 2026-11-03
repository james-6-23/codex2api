package proxy

import (
	"errors"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

type CodexClientIdentityInput struct {
	Account      *auth.Account
	APIKey       string
	DeviceConfig *DeviceProfileConfig
	Headers      http.Header
}

type CodexOutboundClientIdentity struct {
	UserAgent string
	Version   string
	Generated bool
}

// ResolveCodexOutboundClientIdentity 将版本不可用错误传到出站前的调用者。
func ResolveCodexOutboundClientIdentity(input CodexClientIdentityInput) (CodexOutboundClientIdentity, error) {
	if IsDeviceProfileStabilizationEnabled(input.DeviceConfig) {
		return codexDeviceClientIdentity(input), nil
	}
	userAgent := strings.TrimSpace(input.Headers.Get("User-Agent"))
	originator := strings.TrimSpace(input.Headers.Get("Originator"))
	settings := CurrentRuntimeSettings()
	if shouldGenerateCodexClientHeaders(settings, userAgent, originator) {
		ua, version, err := generatedCodexClientHeadersChecked(input.Account, settings)
		return CodexOutboundClientIdentity{UserAgent: ua, Version: version, Generated: true}, err
	}
	if IsCodexOfficialClientByHeaders(userAgent, originator) && userAgent != "" {
		version := firstNonEmptyHeader(input.Headers, "Version", codexVersionFromUserAgent(userAgent, latestCodexCLIVersion))
		return CodexOutboundClientIdentity{UserAgent: userAgent, Version: version}, nil
	}
	accountID := int64(0)
	if input.Account != nil {
		accountID = input.Account.ID()
	}
	floor := codexIdentityVersionFloor(settings)
	ua, version, configured, err := codexUserAgentFromConfigChecked(settings.CodexUserAgentConfig, accountID, floor)
	if err != nil || configured {
		return CodexOutboundClientIdentity{UserAgent: ua, Version: version, Generated: true}, err
	}
	version = effectiveLatestCodexCLIVersion()
	if !codexVersionAtLeast(version, floor) {
		return CodexOutboundClientIdentity{}, codexClientVersionUnavailable("codex-cli", "", floor)
	}
	return CodexOutboundClientIdentity{UserAgent: replaceCodexUserAgentVersion(defaultCodexCLIUserAgent, version), Version: version}, nil
}

func codexIdentityVersionFloor(settings RuntimeSettings) string {
	if settings.ClientCompatMode == ClientCompatModeAuto {
		return settings.CodexMinCLIVersion
	}
	return ""
}

func codexDeviceClientIdentity(input CodexClientIdentityInput) CodexOutboundClientIdentity {
	profile := ResolveDeviceProfile(input.Account, input.APIKey, input.Headers, input.DeviceConfig)
	return CodexOutboundClientIdentity{UserAgent: firstNonEmptyString(profile.UserAgent, defaultCodexCLIUserAgent), Version: codexVersionFromProfile(profile, input.DeviceConfig.PackageVersion)}
}

func generatedCodexClientHeadersChecked(account *auth.Account, settings RuntimeSettings) (string, string, error) {
	floor := codexIdentityVersionFloor(settings)
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	ua, version, configured, err := codexUserAgentFromConfigChecked(settings.CodexUserAgentConfig, accountID, floor)
	if err != nil || configured {
		return ua, version, err
	}
	profile := ProfileForAccount(accountID)
	version = effectiveLatestCodexCLIVersion()
	if !codexVersionAtLeast(version, floor) {
		return "", "", codexClientVersionUnavailable("codex-cli", "", floor)
	}
	ua = firstNonEmptyString(profile.UserAgent, defaultCodexCLIUserAgent)
	return replaceCodexUserAgentVersion(ua, version), version, nil
}

// CodexMaintenanceIdentity 是网关自发的 Codex 维护请求（wham 用量/重置券、每日用量、
// token 明细、订阅同步、模型清单、中转模型发现）携带的客户端身份。
type CodexMaintenanceIdentity struct {
	UserAgent  string
	Originator string
	Version    string
}

// Apply 写入维护请求的身份头。Version 只由已经发送它的调用方自行写入：
// 维护请求不凭空补发真实客户端不会带的头。
func (id CodexMaintenanceIdentity) Apply(headers http.Header) {
	if headers == nil {
		return
	}
	headers.Set("User-Agent", id.UserAgent)
	headers.Set("Originator", id.Originator)
}

// ResolveCodexMaintenanceIdentity 与对话请求共用身份解析和账号覆盖规则。
func ResolveCodexMaintenanceIdentity(account *auth.Account, downstream http.Header) (CodexMaintenanceIdentity, error) {
	identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account, Headers: downstream})
	if err == nil && strings.TrimSpace(identity.UserAgent) == "" {
		err = errEmptyCodexMaintenanceUserAgent
	}
	if err != nil {
		return CodexMaintenanceIdentity{}, err
	}

	resolved := CodexMaintenanceIdentity{UserAgent: identity.UserAgent, Version: identity.Version}
	if identity.Generated {
		resolved.Originator = CodexOriginatorForGeneratedUserAgent(identity.UserAgent)
	} else if originator := strings.TrimSpace(downstream.Get("Originator")); originator != "" && IsCodexOfficialClientByHeaders("", originator) {
		resolved.Originator = originator
	} else {
		resolved.Originator = Originator
	}
	if resolved.Version == "" {
		resolved.Version = codexVersionFromUserAgent(resolved.UserAgent, effectiveLatestCodexCLIVersion())
	}
	// 账号自定义头在对话出站里最后应用（applyAccountCustomHeaders），钉死的身份头
	// 同样作用于维护请求，否则对话与探针又是两套身份。
	for name, value := range account.GetCustomHeaders() {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch http.CanonicalHeaderKey(strings.TrimSpace(name)) {
		case "User-Agent":
			resolved.UserAgent = value
		case "Originator":
			resolved.Originator = value
		case "Version":
			resolved.Version = value
		}
	}
	return resolved, nil
}

var errEmptyCodexMaintenanceUserAgent = errors.New("resolved empty User-Agent")

// OAuth 默认客户端只需要 UA 和 Originator，不复用对话 attestation。
func CodexOAuthIdentityHeaders(account *auth.Account) (http.Header, error) {
	identity, err := ResolveCodexMaintenanceIdentity(account, nil)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	identity.Apply(headers)
	return headers, nil
}

func applyCodexMaintenanceHeaders(req *http.Request, account *auth.Account) error {
	identity, err := ResolveCodexMaintenanceIdentity(account, nil)
	if err != nil {
		return err
	}
	identity.Apply(req.Header)
	return nil
}
