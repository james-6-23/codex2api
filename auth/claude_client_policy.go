package auth

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ClaudeClientPlatform controls which kind of Anthropic client may use an
// OAuth account. Empty values normalize to Any for backwards compatibility.
type ClaudeClientPlatform string

const (
	ClaudeClientPlatformAny     ClaudeClientPlatform = "any"
	ClaudeClientPlatformCLIOnly ClaudeClientPlatform = "claude_code_cli_only"
)

const (
	ClaudeClientPlatformCredentialKey = "claude_client_platform"
	ClaudeVersionPolicyCredentialKey  = "claude_version_policy"
	ClaudeClientVersionCredentialKey  = "claude_client_version"
)

// ClaudeVersionPolicy controls how a recognized Claude Code version is handled.
type ClaudeVersionPolicy string

const (
	ClaudeVersionPolicyPassthrough ClaudeVersionPolicy = "passthrough"
	ClaudeVersionPolicyFixed       ClaudeVersionPolicy = "fixed"
	ClaudeVersionPolicyMinimum     ClaudeVersionPolicy = "minimum"
)

// Deny codes produced by ValidateClaudeClientRequest.
const (
	ClaudeClientDenyPlatformNotAllowed = "client_platform_not_allowed"
	ClaudeClientDenyVersionMissing     = "client_version_missing"
	ClaudeClientDenyVersionTooOld      = "client_version_too_old"
)

// DefaultClaudeClientPolicy is the backwards-compatible any/passthrough policy.
func DefaultClaudeClientPolicy() ClaudeClientPolicy {
	return ClaudeClientPolicy{Platform: ClaudeClientPlatformAny, VersionPolicy: ClaudeVersionPolicyPassthrough}
}

// ClaudeClientPolicy is the effective global/account client policy.
type ClaudeClientPolicy struct {
	Platform      ClaudeClientPlatform `json:"client_platform"`
	VersionPolicy ClaudeVersionPolicy  `json:"version_policy"`
	ClientVersion string               `json:"client_version"`
}

// ClaudeClientDecision contains the result of a request preflight.
type ClaudeClientDecision struct {
	Allowed         bool   `json:"allowed"`
	Code            string `json:"code,omitempty"`
	Message         string `json:"message,omitempty"`
	DetectedVersion string `json:"detected_version,omitempty"`
	RequiredVersion string `json:"required_version,omitempty"`
	RewriteVersion  string `json:"rewrite_version,omitempty"`
	IsCLI           bool   `json:"is_cli"`
}

var claudeClientVersionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bclaude(?:-cli|-code)[/\s:_-]*v?(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)`),
	regexp.MustCompile(`(?i)\bclaude\s+code[/\s:_-]+v?(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)`),
}

type claudeSemVer struct {
	major, minor, patch int
	pre                 string
}

// NormalizeClaudeClientPolicy validates and fills compatibility defaults.
func NormalizeClaudeClientPolicy(policy ClaudeClientPolicy) (ClaudeClientPolicy, error) {
	policy.Platform = ClaudeClientPlatform(strings.ToLower(strings.TrimSpace(string(policy.Platform))))
	if policy.Platform == "" {
		policy.Platform = ClaudeClientPlatformAny
	}
	if policy.Platform != ClaudeClientPlatformAny && policy.Platform != ClaudeClientPlatformCLIOnly {
		return ClaudeClientPolicy{}, fmt.Errorf("client_platform must be any or claude_code_cli_only")
	}
	policy.VersionPolicy = ClaudeVersionPolicy(strings.ToLower(strings.TrimSpace(string(policy.VersionPolicy))))
	if policy.VersionPolicy == "" {
		policy.VersionPolicy = ClaudeVersionPolicyPassthrough
	}
	if policy.VersionPolicy != ClaudeVersionPolicyPassthrough && policy.VersionPolicy != ClaudeVersionPolicyFixed && policy.VersionPolicy != ClaudeVersionPolicyMinimum {
		return ClaudeClientPolicy{}, fmt.Errorf("version_policy must be passthrough, fixed, or minimum")
	}
	policy.ClientVersion = strings.TrimSpace(policy.ClientVersion)
	if policy.ClientVersion != "" {
		parsed, err := parseClaudeSemVer(policy.ClientVersion)
		if err != nil {
			return ClaudeClientPolicy{}, fmt.Errorf("client_version must be major.minor.patch: %w", err)
		}
		policy.ClientVersion = formatClaudeSemVer(parsed)
	}
	if (policy.VersionPolicy == ClaudeVersionPolicyFixed || policy.VersionPolicy == ClaudeVersionPolicyMinimum) && policy.ClientVersion == "" {
		return ClaudeClientPolicy{}, fmt.Errorf("client_version is required for %s policy", policy.VersionPolicy)
	}
	return policy, nil
}

// ParseClaudeClientVersion extracts a Claude Code CLI SemVer from User-Agent.
// It deliberately does not treat generic Claude API/desktop identifiers as CLI.
func ParseClaudeClientVersion(userAgent string) (string, bool) {
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		return "", false
	}
	for _, pattern := range claudeClientVersionPatterns {
		match := pattern.FindStringSubmatch(ua)
		if len(match) != 2 {
			continue
		}
		parsed, err := parseClaudeSemVer(match[1])
		if err == nil {
			return formatClaudeSemVer(parsed), true
		}
	}
	return "", false
}

// CompareClaudeClientVersions compares two Claude Code SemVers.
func CompareClaudeClientVersions(a, b string) (int, error) {
	left, err := parseClaudeSemVer(a)
	if err != nil {
		return 0, err
	}
	right, err := parseClaudeSemVer(b)
	if err != nil {
		return 0, err
	}
	if left.major != right.major {
		return compareInts(left.major, right.major), nil
	}
	if left.minor != right.minor {
		return compareInts(left.minor, right.minor), nil
	}
	if left.patch != right.patch {
		return compareInts(left.patch, right.patch), nil
	}
	if left.pre == right.pre {
		return 0, nil
	}
	if left.pre == "" {
		return 1, nil
	}
	if right.pre == "" {
		return -1, nil
	}
	if left.pre < right.pre {
		return -1, nil
	}
	return 1, nil
}

// ClaudeModelMinimumVersion returns a known Claude Code floor for a model.
func ClaudeModelMinimumVersion(model string) string {
	canonical := strings.ToLower(strings.TrimSpace(model))
	canonical = strings.ReplaceAll(canonical, ".", "-")
	if isClaudeModelVariant(canonical, "claude-fable-5-1") {
		return "2.1.251"
	}
	return ""
}

// ValidateClaudeClientRequest evaluates platform, configured version policy,
// and known model floors without performing any network or account mutation.
func ValidateClaudeClientRequest(policy ClaudeClientPolicy, userAgent, model string) (ClaudeClientDecision, error) {
	normalized, err := NormalizeClaudeClientPolicy(policy)
	if err != nil {
		return ClaudeClientDecision{}, err
	}
	detected, isCLI := ParseClaudeClientVersion(userAgent)
	decision := ClaudeClientDecision{Allowed: true, DetectedVersion: detected, IsCLI: isCLI}
	if normalized.Platform == ClaudeClientPlatformCLIOnly && !isCLI {
		return denyClaudeClient(ClaudeClientDenyPlatformNotAllowed, "Claude account only accepts Claude Code CLI requests", decision), nil
	}
	modelFloor := ClaudeModelMinimumVersion(model)
	required := ""
	if normalized.VersionPolicy == ClaudeVersionPolicyMinimum {
		required = normalized.ClientVersion
	}
	if modelFloor != "" && isCLI {
		if required == "" || mustUseClaudeVersion(modelFloor, required) {
			required = modelFloor
		}
	}
	if normalized.VersionPolicy == ClaudeVersionPolicyFixed && isCLI {
		decision.RewriteVersion = normalized.ClientVersion
	}
	decision.RequiredVersion = required
	if required == "" || !isCLI {
		return decision, nil
	}
	versionToCheck := detected
	if normalized.VersionPolicy == ClaudeVersionPolicyFixed {
		versionToCheck = normalized.ClientVersion
	}
	if versionToCheck == "" {
		return denyClaudeClient(ClaudeClientDenyVersionMissing, "Claude Code CLI version is required", decision), nil
	}
	if cmp, compareErr := CompareClaudeClientVersions(versionToCheck, required); compareErr != nil {
		return ClaudeClientDecision{}, compareErr
	} else if cmp < 0 {
		return denyClaudeClient(ClaudeClientDenyVersionTooOld, "Claude Code CLI version is too old; run 'claude update'", decision), nil
	}
	decision.RequiredVersion = required
	return decision, nil
}

// HTTPStatus maps a deny decision onto the downstream status code: version
// problems are 426 Upgrade Required, everything else is a plain 400.
func (d ClaudeClientDecision) HTTPStatus() int {
	if d.Allowed {
		return 200
	}
	if d.Code == ClaudeClientDenyVersionTooOld || d.Code == ClaudeClientDenyVersionMissing {
		return 426
	}
	return 400
}

// DetailMessage renders the deny message with detected/required versions so
// every caller emits the same wording.
func (d ClaudeClientDecision) DetailMessage() string {
	message := d.Message
	if d.DetectedVersion != "" {
		message += fmt.Sprintf(" (detected %s)", d.DetectedVersion)
	}
	if d.RequiredVersion != "" {
		message += fmt.Sprintf("; required %s", d.RequiredVersion)
	}
	return message
}

func denyClaudeClient(code, message string, decision ClaudeClientDecision) ClaudeClientDecision {
	decision.Allowed = false
	decision.Code = code
	decision.Message = message
	return decision
}

func mustUseClaudeVersion(candidate, current string) bool {
	cmp, err := CompareClaudeClientVersions(candidate, current)
	return err == nil && cmp > 0
}

func isClaudeModelVariant(model, base string) bool {
	return model == base || strings.HasPrefix(model, base+"-")
}

func parseClaudeSemVer(value string) (claudeSemVer, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "v"))
	if value == "" {
		return claudeSemVer{}, fmt.Errorf("version is empty")
	}
	parts := strings.SplitN(value, "+", 2)
	core := parts[0]
	coreParts := strings.SplitN(core, "-", 2)
	numbers := strings.Split(coreParts[0], ".")
	if len(numbers) != 3 {
		return claudeSemVer{}, fmt.Errorf("invalid version %q", value)
	}
	parsed := claudeSemVer{}
	values := []*int{&parsed.major, &parsed.minor, &parsed.patch}
	for i, raw := range numbers {
		if raw == "" || (len(raw) > 1 && raw[0] == '0') {
			return claudeSemVer{}, fmt.Errorf("invalid version %q", value)
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return claudeSemVer{}, fmt.Errorf("invalid version %q", value)
		}
		*values[i] = n
	}
	if len(coreParts) == 2 {
		parsed.pre = coreParts[1]
		if parsed.pre == "" {
			return claudeSemVer{}, fmt.Errorf("invalid version %q", value)
		}
	}
	return parsed, nil
}

func formatClaudeSemVer(version claudeSemVer) string {
	if version.pre == "" {
		return fmt.Sprintf("%d.%d.%d", version.major, version.minor, version.patch)
	}
	return fmt.Sprintf("%d.%d.%d-%s", version.major, version.minor, version.patch, version.pre)
}

func compareInts(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
