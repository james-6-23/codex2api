package auth

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// UpstreamAntigravity marks Google Antigravity accounts. They participate in
// /v1/responses through the dedicated v1internal adapter and must not inherit
// Codex unauthorized/banned/usage fences.
const UpstreamAntigravity = "antigravity"

const (
	AntigravityAuthKindOAuth  = "oauth"
	AntigravityAuthKindAPIKey = "api_key"

	// AntigravityExperimentalInteractionsEnv gates the API-key Interactions
	// executor. Its request/response wire adapter is still incomplete, so live
	// inference is fail-closed unless an operator explicitly opts in.
	AntigravityExperimentalInteractionsEnv = "ANTIGRAVITY_ENABLE_EXPERIMENTAL_INTERACTIONS"
)

// AntigravityExperimentalInteractionsEnabled reports whether the explicitly
// experimental Google API-key inference path may participate in dispatch.
func AntigravityExperimentalInteractionsEnabled() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(AntigravityExperimentalInteractionsEnv)))
	return err == nil && enabled
}

func (a *Account) isAntigravityAPILocked() bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamAntigravity) {
		return false
	}
	return strings.TrimSpace(a.APIKey) != "" || strings.TrimSpace(a.AccessToken) != "" || strings.TrimSpace(a.RefreshToken) != ""
}

// IsAntigravityAPI reports whether the account belongs to the Antigravity
// Google OAuth or API-key channel.
func (a *Account) IsAntigravityAPI() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isAntigravityAPILocked()
}

// AntigravityAuthKind reports the credential shape used by this account.
func (a *Account) AntigravityAuthKind() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isAntigravityAPILocked() {
		return ""
	}
	if strings.TrimSpace(a.APIKey) != "" {
		return AntigravityAuthKindAPIKey
	}
	return AntigravityAuthKindOAuth
}

// AntigravityDispatchEnabled keeps OAuth inference enabled while failing the
// incomplete API-key Interactions adapter closed by default. Explicit admin
// capability probes bypass this dispatch gate and can still validate a key.
func (a *Account) AntigravityDispatchEnabled() bool {
	kind := a.AntigravityAuthKind()
	return kind == AntigravityAuthKindOAuth ||
		(kind == AntigravityAuthKindAPIKey && AntigravityExperimentalInteractionsEnabled())
}

// AntigravityAPIKey returns the official Generative Language API credential.
func (a *Account) AntigravityAPIKey() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isAntigravityAPILocked() {
		return ""
	}
	return strings.TrimSpace(a.APIKey)
}

// AntigravityDefaultModelIDs is the conservative raw/wire fallback catalog.
// Public model discovery expands these backing IDs through the proxy's native
// 14-model projection; keeping wire IDs here preserves request admission and
// capability checks when an account has not completed its first sync yet.
func AntigravityDefaultModelIDs() []string {
	return []string{
		"gemini-3.5-flash-extra-low",
		"gemini-3.5-flash-low",
		"gemini-3-flash-agent",
		"gemini-3.6-flash-low",
		"gemini-3.6-flash-medium",
		"gemini-3.6-flash-high",
		"gemini-3.7-flash-tiered",
		"gemini-3.1-pro-low",
		"gemini-pro-agent",
		"claude-opus-4-6-thinking",
		"claude-sonnet-4-6",
		"gpt-oss-120b-medium",
	}
}

// AntigravityModels returns the explicit/synchronized catalog or safe defaults.
func (a *Account) AntigravityModels() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isAntigravityAPILocked() {
		return nil
	}
	if len(a.Models) > 0 {
		return cloneStringSlice(a.Models)
	}
	return AntigravityDefaultModelIDs()
}

// AntigravitySupportsModel keeps admission aligned with the exposed catalog.
func (a *Account) AntigravitySupportsModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, candidate := range a.AntigravityModels() {
		if strings.EqualFold(strings.TrimSpace(candidate), model) {
			return true
		}
	}
	return false
}

// applyAntigravitySchedulerOverrideLocked keeps a credentialed Antigravity
// account in the dispatch pool even when Codex unauthorized cooling has marked
// it banned. Admin runtime status may still show unauthorized; the 401 refresh
// path is what recovers the token.
func (a *Account) applyAntigravitySchedulerOverrideLocked(baseLimit int64, tier AccountHealthTier, limit int64, available bool) (AccountHealthTier, int64, bool) {
	if a == nil || !a.isAntigravityAPILocked() || !a.hasDispatchCredentialLocked() {
		return tier, limit, available
	}
	// Administrative fences are never bypassed by the OAuth refresh escape hatch.
	if atomic.LoadInt32(&a.Disabled) != 0 || atomic.LoadInt32(&a.DispatchPaused) != 0 || a.Status == StatusError {
		return tier, limit, false
	}
	recoverUnauthorized := a.antigravityUnauthorizedRecoveryLocked(time.Now())
	if !available && !recoverUnauthorized {
		return tier, limit, false
	}
	if recoverUnauthorized {
		available = true
	}
	if limit <= 0 {
		base := a.BaseConcurrencyEffective
		if base <= 0 {
			base = a.effectiveBaseConcurrencyLocked(baseLimit)
		}
		limit = concurrencyLimitForTier(base, HealthTierHealthy)
		if limit <= 0 {
			limit = 1
		}
	}
	if recoverUnauthorized && tier == HealthTierBanned {
		tier = HealthTierWarm
	}
	return tier, limit, available
}

// antigravityUnauthorizedRecoveryLocked is the narrow exception for a live
// bearer that was put into the transient unauthorized cooldown. It must not
// turn a terminal/admin ban or a refresh-failure state back into dispatch.
func (a *Account) antigravityUnauthorizedRecoveryLocked(_ time.Time) bool {
	if a == nil || !a.isAntigravityAPILocked() || strings.TrimSpace(a.APIKey) != "" || !a.hasDispatchCredentialLocked() {
		return false
	}
	if a.Status != StatusCooldown || !strings.EqualFold(strings.TrimSpace(a.CooldownReason), "unauthorized") {
		return false
	}
	if a.PermanentRefreshFailures > 0 {
		return false
	}
	return true
}

// AntigravityCredentials returns the live OAuth bearer and Cloud Code project
// selected during the Antigravity identity sync.
func (a *Account) AntigravityCredentials() (projectID, bearer string) {
	if a == nil {
		return "", ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isAntigravityAPILocked() {
		return "", ""
	}
	return strings.TrimSpace(a.AntigravityProjectID), strings.TrimSpace(a.AccessToken)
}

type AntigravityCredential struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	Email        string
	// VerifiedEmail is an optional claim supplied by an imported credential
	// document. It is deliberately non-authoritative: live Google userinfo
	// remains the only source for the persisted `verified_email` field.
	VerifiedEmail        bool
	VerifiedEmailPresent bool
	Name                 string
	AvatarURL            string
	ProjectID            string
	OAuthClientKey       string
	ClientID             string
	ClientSecret         string
	Scope                string
	ExpiresAt            time.Time
}

type AntigravityProfile struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name,omitempty"`
	GivenName     string `json:"given_name,omitempty"`
	FamilyName    string `json:"family_name,omitempty"`
	Picture       string `json:"picture,omitempty"`
}

type AntigravityTier struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	QuotaTier string `json:"quota_tier,omitempty"`
	Slug      string `json:"slug,omitempty"`
	IsDefault bool   `json:"is_default,omitempty"`
}

type AntigravityIneligibleTier struct {
	ReasonCode string `json:"reason_code,omitempty"`
}

type AntigravityEntitlements struct {
	Allowed         bool                        `json:"allowed"`
	Reason          string                      `json:"reason,omitempty"`
	ProjectID       string                      `json:"project_id,omitempty"`
	EffectiveTier   string                      `json:"effective_tier,omitempty"`
	Restricted      bool                        `json:"restricted,omitempty"`
	CurrentTier     *AntigravityTier            `json:"current_tier,omitempty"`
	PaidTier        *AntigravityTier            `json:"paid_tier,omitempty"`
	AllowedTiers    []AntigravityTier           `json:"allowed_tiers,omitempty"`
	IneligibleTiers []AntigravityIneligibleTier `json:"ineligible_tiers,omitempty"`
	UpdatedAt       time.Time                   `json:"updated_at"`
}

type AntigravityModelQuota struct {
	ModelID           string          `json:"model_id"`
	DisplayName       string          `json:"display_name,omitempty"`
	RemainingFraction float64         `json:"remaining_fraction"`
	RemainingPercent  int             `json:"remaining_percent"`
	ResetTime         string          `json:"reset_time,omitempty"`
	SupportsImages    *bool           `json:"supports_images,omitempty"`
	SupportsThinking  *bool           `json:"supports_thinking,omitempty"`
	ThinkingBudget    *int            `json:"thinking_budget,omitempty"`
	Recommended       *bool           `json:"recommended,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	SupportedMIMEs    map[string]bool `json:"supported_mime_types,omitempty"`
}

type AntigravityQuotaBucket struct {
	BucketID          string  `json:"bucket_id,omitempty"`
	Window            string  `json:"window,omitempty"`
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time,omitempty"`
	DisplayName       string  `json:"display_name,omitempty"`
	Description       string  `json:"description,omitempty"`
}

type AntigravityQuotaGroup struct {
	DisplayName string                   `json:"display_name,omitempty"`
	Description string                   `json:"description,omitempty"`
	Buckets     []AntigravityQuotaBucket `json:"buckets"`
}

type AntigravityAICredits struct {
	Credits    float64 `json:"credits"`
	ExpiryDate string  `json:"expiry_date,omitempty"`
}

type AntigravityQuotaSnapshot struct {
	Models               []AntigravityModelQuota `json:"models"`
	Groups               []AntigravityQuotaGroup `json:"quota_groups,omitempty"`
	ModelForwardingRules map[string]string       `json:"model_forwarding_rules,omitempty"`
	AICredits            *AntigravityAICredits   `json:"ai_credits,omitempty"`
	Forbidden            bool                    `json:"forbidden,omitempty"`
	UpdatedAt            time.Time               `json:"updated_at"`
}

// UnmarshalJSON accepts the historical "groups" field while all newly written
// snapshots use the canonical "quota_groups" name.
func (q *AntigravityQuotaSnapshot) UnmarshalJSON(data []byte) error {
	type quotaAlias AntigravityQuotaSnapshot
	var canonical quotaAlias
	if err := json.Unmarshal(data, &canonical); err != nil {
		return err
	}
	*q = AntigravityQuotaSnapshot(canonical)
	if q.Groups == nil {
		var legacy struct {
			Groups []AntigravityQuotaGroup `json:"groups"`
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		q.Groups = legacy.Groups
	}
	return nil
}

type AntigravitySyncResult struct {
	Credential           AntigravityCredential
	Profile              AntigravityProfile
	Entitlements         AntigravityEntitlements
	Quota                AntigravityQuotaSnapshot
	EntitlementsObserved bool
	QuotaGroupsObserved  bool
	AICreditsObserved    bool
	Warning              string
}

type AntigravityEndpoints struct {
	TokenURL     string
	UserInfoURL  string
	LoadProject  []string
	Quota        []string
	QuotaSummary []string
	AICredits    []string
	OnboardUser  []string
}
