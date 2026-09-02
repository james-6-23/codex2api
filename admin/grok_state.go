package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	grokFactFreshness        = 5 * time.Minute
	grokUserUpgradeFreshness = 60 * time.Second
	grokBillingHotFreshness  = 30 * time.Second
	grokCapabilityOKTTL      = 24 * time.Hour
	// Automatic capability rebuilds are maintenance, not health checks. Cache
	// negative/rate-limited observations for a day so the 30-second freshness
	// scanner cannot turn an unsupported protocol into recurring generation
	// traffic. Operators can still bypass this TTL with the explicit force probe.
	grokCapabilityFailureTTL   = 24 * time.Hour
	grokCapabilityProbeTimeout = 45 * time.Second
)

var (
	errGrokAccountRequired     = errors.New("仅 Grok 账号支持该操作")
	errGrokCredentialChanged   = errors.New("Grok 凭据已在操作期间变化，请重试")
	grokCapabilitySlots        = make(chan struct{}, 4)
	grokCapabilityAccountLocks sync.Map // account id -> chan struct{} (capacity 1)
)

type grokStateSyncResult struct {
	State       *database.GrokAccountState `json:"state"`
	Models      []string                   `json:"models"`
	SyncedFacts []string                   `json:"synced_facts,omitempty"`
	Errors      map[string]string          `json:"errors,omitempty"`
	// capabilityGeneration is an internal scheduling signal. It is set when a
	// sync crosses a credential-generation boundary and the new generation has
	// no fresh native protocol facts yet; callers may enqueue the bounded probe
	// after releasing the synchronous request context.
	capabilityGeneration int64
}

// grokStateSyncSelection lets the periodic freshness worker refresh only the
// independently-expired observations. Manual/admin/import syncs use the full
// selection so their external contract remains unchanged.
type grokStateSyncSelection struct {
	FactKinds map[proxy.GrokControlPlaneFactKind]struct{}
	Catalog   bool
}

func fullGrokStateSyncSelection() grokStateSyncSelection {
	selection := grokStateSyncSelection{
		FactKinds: make(map[proxy.GrokControlPlaneFactKind]struct{}, 4),
		Catalog:   true,
	}
	for _, kind := range []proxy.GrokControlPlaneFactKind{
		proxy.GrokControlPlaneUser, proxy.GrokControlPlaneSettings,
		proxy.GrokControlPlaneBilling, proxy.GrokControlPlaneAutoTopup,
	} {
		selection.FactKinds[kind] = struct{}{}
	}
	return selection
}

func (s grokStateSyncSelection) includesFact(kind proxy.GrokControlPlaneFactKind) bool {
	_, ok := s.FactKinds[kind]
	return ok
}

func (s grokStateSyncSelection) empty() bool {
	return len(s.FactKinds) == 0 && !s.Catalog
}

type grokCapabilityProbeResult struct {
	ModelID           string    `json:"model_id"`
	Protocol          string    `json:"protocol"`
	Status            string    `json:"status"`
	HTTPStatus        int       `json:"http_status,omitempty"`
	ProviderCode      string    `json:"provider_code,omitempty"`
	RetryAfterSeconds int64     `json:"retry_after_seconds,omitempty"`
	ObservedAt        time.Time `json:"observed_at"`
}

type grokCapabilityProbeResponse struct {
	State   *database.GrokAccountState  `json:"state"`
	Results []grokCapabilityProbeResult `json:"results"`
}

func parseGrokAdminAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return 0, false
	}
	return id, true
}

func writeGrokAdminError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(c, http.StatusNotFound, "账号不存在")
	case errors.Is(err, errGrokAccountRequired):
		writeError(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, errGrokCredentialChanged):
		writeError(c, http.StatusConflict, err.Error())
	default:
		writeInternalError(c, err)
	}
}

func (h *Handler) grokAdminAccount(ctx context.Context, id int64) (*auth.Account, *database.AccountRow, error) {
	if h == nil || h.db == nil || h.store == nil {
		return nil, nil, errors.New("Grok 管理服务尚未初始化")
	}
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamGrok) {
		return nil, nil, errGrokAccountRequired
	}
	account := h.store.FindByID(id)
	if account == nil {
		account, err = h.store.BuildGrokAdministrativeAccountByID(ctx, id)
		if err != nil {
			return nil, nil, err
		}
	}
	if account == nil || !account.IsGrokAPI() {
		return nil, nil, errGrokAccountRequired
	}
	// A just-edited identity can advance the DB generation before an older
	// runtime object observes it. Build a fenced transient view for this
	// operation; the successful sync will republish state through the store.
	if account.GetCredentialGeneration() != row.CredentialGeneration {
		account, err = h.store.BuildTransientAccountByID(ctx, id)
		if err != nil {
			return nil, nil, err
		}
	}
	return account, row, nil
}

// GetGrokAccountState returns only sanitized, non-secret persisted facts.
func (h *Handler) GetGrokAccountState(c *gin.Context) {
	id, ok := parseGrokAdminAccountID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if _, _, err := h.grokAdminAccount(ctx, id); err != nil {
		writeGrokAdminError(c, err)
		return
	}
	state, err := h.db.GetGrokAccountState(ctx, id)
	if err != nil {
		writeGrokAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, state)
}

// SyncGrokAccountState refreshes the read-only control plane and rich model
// catalog. Upstream partial failures are reported without erasing prior facts.
func (h *Handler) SyncGrokAccountState(c *gin.Context) {
	id, ok := parseGrokAdminAccountID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 110*time.Second)
	defer cancel()
	result, err := h.syncGrokAccountState(ctx, id)
	if err != nil {
		writeGrokAdminError(c, err)
		return
	}
	if result.capabilityGeneration > 0 {
		h.triggerGrokCapabilityProbeForGeneration(id, result.capabilityGeneration)
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "Grok 控制面与模型目录同步完成", "state": result.State,
		"models": result.Models, "synced_facts": result.SyncedFacts, "errors": result.Errors,
	})
}

func factStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired:
		return "exhausted"
	case http.StatusForbidden:
		return "subscription_required"
	case http.StatusUpgradeRequired:
		return "version_required"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	if status >= 200 && status < 300 {
		return "ok"
	}
	if status >= 500 || status == 0 {
		return "unavailable"
	}
	return "error"
}

func grokControlPlaneFailureStatus(status int, body []byte) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired:
		code := strings.ToLower(providerCodeFromJSON(body))
		message := strings.ToLower(strings.TrimSpace(firstNonEmpty(
			gjson.GetBytes(body, "error.message").String(),
			gjson.GetBytes(body, "response.error.message").String(),
			gjson.GetBytes(body, "message").String(),
		)))
		evidence := code + " " + message
		if (strings.Contains(evidence, "balance") || strings.Contains(evidence, "credit")) &&
			(strings.Contains(evidence, "exhaust") || strings.Contains(evidence, "insufficient") || strings.Contains(evidence, "deplet")) {
			return "exhausted"
		}
		return "unknown"
	case http.StatusForbidden:
		code := strings.ToLower(providerCodeFromJSON(body))
		message := strings.ToLower(strings.TrimSpace(firstNonEmpty(
			gjson.GetBytes(body, "error.message").String(),
			gjson.GetBytes(body, "response.error.message").String(),
			gjson.GetBytes(body, "message").String(),
		)))
		evidence := code + " " + message
		if strings.Contains(evidence, "subscription") || strings.Contains(evidence, "free-usage") || strings.Contains(evidence, "plan_required") {
			return "subscription_required"
		}
		return "unknown"
	case http.StatusUpgradeRequired:
		return "version_required"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	if status >= 500 || status == 0 {
		return "unavailable"
	}
	if status >= 200 && status < 300 {
		return "ok"
	}
	return "error"
}

func grokUserUpgradePending(account *auth.Account, payload map[string]any, presence map[string]string) bool {
	if account == nil || presence["subscriptionTier"] != "value" {
		return false
	}
	live, _ := payload["subscriptionTier"].(string)
	live = auth.CanonicalGrokLivePlanFilter(live)
	if live == "" || live == "free" {
		return false
	}
	account.Mu().RLock()
	tokenTier := auth.CanonicalGrokLivePlanFilter(account.PlanType)
	account.Mu().RUnlock()
	return tokenTier == "" || tokenTier != live
}

func grokFactExpiresAt(account *auth.Account, kind proxy.GrokControlPlaneFactKind, status string, payload map[string]any, presence map[string]string, observed time.Time) time.Time {
	if status != "ok" {
		if kind == proxy.GrokControlPlaneBilling && status == "exhausted" {
			return observed.Add(grokBillingHotFreshness)
		}
		return observed
	}
	if kind == proxy.GrokControlPlaneUser && grokUserUpgradePending(account, payload, presence) {
		return observed.Add(grokUserUpgradeFreshness)
	}
	if kind == proxy.GrokControlPlaneBilling {
		if pct := gjson.GetBytes(mustJSON(payload), "config.creditUsagePercent"); pct.Exists() && pct.Float() >= 99 {
			return observed.Add(grokBillingHotFreshness)
		}
	}
	return observed.Add(grokFactFreshness)
}

func grokSubscriptionFactChanged(old *database.GrokAccountFact, payload map[string]any, presence map[string]string) bool {
	// Only an explicit live value is subscription evidence. Missing/null must
	// not invalidate a capability set merely because no prior fact existed.
	if presence["subscriptionTier"] != "value" {
		return false
	}
	signature := func(body map[string]any, fields map[string]string) string {
		value, exists := body["subscriptionTier"]
		encoded, _ := json.Marshal([]any{fields["subscriptionTier"], exists, value})
		return string(encoded)
	}
	if old == nil {
		return true
	}
	return signature(old.Payload, old.FieldPresence) != signature(payload, presence)
}

func preserveFreshGrokFactOnFailure(kind proxy.GrokControlPlaneFactKind, status string, old *database.GrokAccountFact, observed time.Time) bool {
	authoritativeFailure := status == "unauthorized" || (kind == proxy.GrokControlPlaneBilling && status == "exhausted")
	return status != "ok" && !authoritativeFailure && old != nil && old.Status == "ok" && observed.Before(old.ExpiresAt)
}

func jsonPresence(value any, exists bool) string {
	if !exists {
		return "missing"
	}
	if value == nil {
		return "null"
	}
	return "value"
}

func firstJSONField(source map[string]any, names ...string) (any, bool) {
	for _, name := range names {
		if value, ok := source[name]; ok {
			return value, true
		}
	}
	return nil, false
}

func copySafeScalar(dst map[string]any, presence map[string]string, source map[string]any, canonical string, names ...string) {
	value, exists := firstJSONField(source, names...)
	presence[canonical] = jsonPresence(value, exists)
	if !exists {
		return
	}
	if value == nil {
		dst[canonical] = nil
		return
	}
	switch value.(type) {
	case string, bool, float64, json.Number:
		dst[canonical] = value
	default:
		presence[canonical] = "invalid"
	}
}

func copyMoneyValue(dst map[string]any, presence map[string]string, source map[string]any, canonical string, names ...string) {
	raw, exists := firstJSONField(source, names...)
	presence[canonical] = jsonPresence(raw, exists)
	if !exists {
		return
	}
	if raw == nil {
		dst[canonical] = nil
		return
	}
	object, ok := raw.(map[string]any)
	if !ok {
		presence[canonical] = "invalid"
		return
	}
	value, valueExists := firstJSONField(object, "val", "value")
	presence[canonical+".val"] = jsonPresence(value, valueExists)
	// Proto3 encodes an explicit zero-valued money message as {}.
	if !valueExists {
		dst[canonical] = map[string]any{"val": float64(0)}
		return
	}
	switch value.(type) {
	case float64, json.Number:
		dst[canonical] = map[string]any{"val": value}
	case nil:
		dst[canonical] = nil
	default:
		presence[canonical+".val"] = "invalid"
	}
}

func decodeJSONObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		object = map[string]any{}
	}
	return object, nil
}

func sanitizeGrokFact(kind proxy.GrokControlPlaneFactKind, body []byte) (map[string]any, map[string]string, error) {
	source, err := decodeJSONObject(body)
	if err != nil {
		return nil, nil, err
	}
	payload := map[string]any{}
	presence := map[string]string{}
	switch kind {
	case proxy.GrokControlPlaneUser:
		copySafeScalar(payload, presence, source, "subscriptionTier", "subscriptionTier", "subscription_tier")
	case proxy.GrokControlPlaneSettings:
		for canonical, names := range map[string][]string{
			"allow_access":               {"allow_access", "allowAccess"},
			"subscription_tier_display":  {"subscription_tier_display", "subscriptionTierDisplay"},
			"on_demand_enabled":          {"on_demand_enabled", "onDemandEnabled"},
			"default_model":              {"default_model", "defaultModel"},
			"min_client_version":         {"min_client_version", "minClientVersion"},
			"force_update":               {"force_update", "forceUpdate"},
			"usage_billing_redirect_url": {"usage_billing_redirect_url", "usageBillingRedirectUrl"},
		} {
			copySafeScalar(payload, presence, source, canonical, names...)
		}
	case proxy.GrokControlPlaneBilling:
		configRaw, exists := firstJSONField(source, "config")
		config, ok := configRaw.(map[string]any)
		if !exists {
			config = source // tolerate an unwrapped compatible response
			ok = true
		}
		presence["config"] = jsonPresence(configRaw, exists)
		if ok {
			safeConfig := map[string]any{}
			copySafeScalar(safeConfig, presence, config, "creditUsagePercent", "creditUsagePercent", "credit_usage_percent")
			copySafeScalar(safeConfig, presence, config, "isUnifiedBillingUser", "isUnifiedBillingUser", "is_unified_billing_user")
			for _, key := range []string{"monthlyLimit", "used", "onDemandCap", "onDemandUsed", "prepaidBalance"} {
				snake := strings.NewReplacer("D", "_d", "C", "_c", "U", "_u", "B", "_b", "L", "_l").Replace(key)
				copyMoneyValue(safeConfig, presence, config, key, key, strings.ToLower(snake))
			}
			periodRaw, periodExists := firstJSONField(config, "currentPeriod", "current_period")
			presence["currentPeriod"] = jsonPresence(periodRaw, periodExists)
			if period, periodOK := periodRaw.(map[string]any); periodOK {
				safePeriod := map[string]any{}
				for _, key := range []string{"type", "start", "end"} {
					periodPresence := map[string]string{}
					copySafeScalar(safePeriod, periodPresence, period, key, key)
					presence["currentPeriod."+key] = periodPresence[key]
				}
				safeConfig["currentPeriod"] = safePeriod
			}
			copySafeScalar(safeConfig, presence, config, "billingPeriodStart", "billingPeriodStart", "billing_period_start")
			copySafeScalar(safeConfig, presence, config, "billingPeriodEnd", "billingPeriodEnd", "billing_period_end")
			payload["config"] = safeConfig
		}
	case proxy.GrokControlPlaneAutoTopup:
		ruleRaw, exists := firstJSONField(source, "rule")
		presence["rule"] = jsonPresence(ruleRaw, exists)
		if rule, ok := ruleRaw.(map[string]any); ok {
			safeRule := map[string]any{}
			copySafeScalar(safeRule, presence, rule, "enabled", "enabled")
			if presence["enabled"] == "missing" {
				safeRule["enabled"] = false
			}
			copyMoneyValue(safeRule, presence, rule, "minBeforeHittingSl", "minBeforeHittingSl", "min_before_hitting_sl")
			copyMoneyValue(safeRule, presence, rule, "topupAmount", "topupAmount", "topup_amount")
			copyMoneyValue(safeRule, presence, rule, "maxAmountPerMonth", "maxAmountPerMonth", "max_amount_per_month")
			payload["rule"] = safeRule
		}
	}
	return payload, presence, nil
}

func safeGrokUpstreamError(err error) string {
	var upstream *proxy.GrokHTTPError
	if proxy.AsGrokHTTPError(err, &upstream) {
		if code := sanitizeGrokProviderCode(upstream.Code); code != "" {
			return fmt.Sprintf("upstream status %d (%s)", upstream.StatusCode, code)
		}
		return fmt.Sprintf("upstream status %d", upstream.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "upstream request timed out or was canceled"
	}
	return "upstream request failed"
}

func persistedCatalogItems(models []proxy.GrokModelCatalogItem) []database.GrokModelCatalogItem {
	items := make([]database.GrokModelCatalogItem, 0, len(models))
	for _, model := range models {
		reasoning := make([]string, 0, len(model.ReasoningEfforts))
		for _, option := range model.ReasoningEfforts {
			if option.ID != "" {
				reasoning = append(reasoning, option.ID)
			}
		}
		headers := make(map[string]string, len(model.ExtraHeaders))
		for name, values := range model.ExtraHeaders {
			if len(values) > 0 {
				headers[name] = values[len(values)-1]
			}
		}
		boolValue := func(value *bool, fallback bool) bool {
			if value == nil {
				return fallback
			}
			return *value
		}
		items = append(items, database.GrokModelCatalogItem{
			ModelID: model.ID, DisplayName: model.DisplayName, Description: model.Description,
			BaseURL: model.BaseURL, APIBaseURL: model.APIBaseURL, APIBackend: string(model.APIBackend),
			ContextWindow: model.ContextWindow, MaxOutputTokens: model.MaxCompletionTokens,
			ReasoningEffort: model.ReasoningEffort, ReasoningEfforts: reasoning,
			SupportsReasoningEffort: boolValue(model.SupportsReasoningEffort, false),
			SupportsBackendSearch:   boolValue(model.SupportsBackendSearch, false),
			StreamToolCalls:         boolValue(model.StreamToolCalls, false), SupportedInAPI: boolValue(model.SupportedInAPI, true),
			Hidden: boolValue(model.Hidden, false), ExtraHeaders: headers, FieldPresence: model.FieldPresence,
		})
	}
	return items
}

func visiblePersistedModelIDs(items []database.GrokModelCatalogItem, authKind string) []string {
	seen := map[string]struct{}{}
	models := make([]string, 0, len(items))
	for _, item := range items {
		if item.Hidden || (authKind == auth.GrokAuthKindAPIKey && item.FieldPresence["supported_in_api"] == "value" && !item.SupportedInAPI) {
			continue
		}
		id := strings.TrimSpace(item.ModelID)
		key := strings.ToLower(id)
		if id == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, id)
	}
	sort.SliceStable(models, func(i, j int) bool { return strings.ToLower(models[i]) < strings.ToLower(models[j]) })
	return models
}

// grokAccessTokenStale reports whether a control-plane sync should refresh an
// OAuth access token before issuing its read-only requests.
func grokAccessTokenStale(account *auth.Account) bool {
	if account == nil {
		return true
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	if strings.TrimSpace(account.AccessToken) == "" {
		return true
	}
	return !account.ExpiresAt.IsZero() && time.Now().Add(2*time.Minute).After(account.ExpiresAt)
}

func (h *Handler) syncGrokAccountState(ctx context.Context, id int64) (*grokStateSyncResult, error) {
	return h.syncGrokAccountStateSelected(ctx, id, fullGrokStateSyncSelection())
}

func (h *Handler) syncGrokAccountStateSelected(ctx context.Context, id int64, selection grokStateSyncSelection) (*grokStateSyncResult, error) {
	account, row, err := h.grokAdminAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	selectedGeneration := row.CredentialGeneration
	result := &grokStateSyncResult{Errors: map[string]string{}}
	if account.GrokAuthKind() == auth.GrokAuthKindOAuth && grokAccessTokenStale(account) {
		if err := h.store.RefreshGrokAccountByID(ctx, id); err != nil {
			result.Errors["refresh"] = "credential refresh failed"
		} else {
			account, row, err = h.grokAdminAccount(ctx, id)
			if err != nil {
				return nil, err
			}
		}
	}
	// The selection was computed against selectedGeneration. An OAuth refresh
	// may rotate AT/RT and advance the fence before any selected request runs;
	// in that case every prior fact/catalog is stale for the new identity and
	// this same pass must rebuild the full observation set.
	if row.CredentialGeneration != selectedGeneration {
		selection = fullGrokStateSyncSelection()
	}
	generation := row.CredentialGeneration
	if generation <= 0 {
		generation = 1
	}
	proxyURL := h.store.ResolveProxyForAccount(account)
	legacyUpdates := map[string]any{}

	if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
		for _, kind := range []proxy.GrokControlPlaneFactKind{
			proxy.GrokControlPlaneUser, proxy.GrokControlPlaneSettings,
			proxy.GrokControlPlaneBilling, proxy.GrokControlPlaneAutoTopup,
		} {
			if !selection.includesFact(kind) {
				continue
			}
			observed := time.Now()
			factResult, fetchErr := proxy.FetchGrokControlPlaneFact(ctx, account, proxyURL, kind, "")
			status, httpStatus := "unavailable", 0
			var payload map[string]any
			var presence map[string]string
			var old *database.GrokAccountFact
			if previous, oldErr := h.db.GetGrokAccountFact(ctx, id, string(kind)); oldErr == nil && previous.CredentialGeneration == generation {
				old = previous
				payload, presence = previous.Payload, previous.FieldPresence
			}
			if fetchErr != nil {
				result.Errors[string(kind)] = safeGrokUpstreamError(fetchErr)
			} else {
				observed, httpStatus = factResult.ObservedAt, factResult.StatusCode
				status = grokControlPlaneFailureStatus(factResult.StatusCode, factResult.Body)
				if status == "ok" && !factResult.NotModified {
					payload, presence, fetchErr = sanitizeGrokFact(kind, factResult.Body)
					if fetchErr != nil {
						status = "error"
						result.Errors[string(kind)] = "upstream response could not be safely parsed"
					}
				} else if status != "ok" {
					result.Errors[string(kind)] = fmt.Sprintf("upstream status %d", httpStatus)
				}
			}
			if payload == nil {
				payload = map[string]any{}
			}
			if presence == nil {
				presence = map[string]string{}
			}
			// Transient/policy failures cannot replace an earlier successful fact
			// while it remains fresh. Credential rejection and an explicitly
			// classified balance exhaustion are authoritative hard changes.
			if preserveFreshGrokFactOnFailure(kind, status, old, observed) {
				continue
			}
			expires := grokFactExpiresAt(account, kind, status, payload, presence, observed)
			fact := database.GrokAccountFact{
				AccountID: id, Kind: string(kind), CredentialGeneration: generation,
				Status: status, HTTPStatus: httpStatus, Source: "grok_control_plane",
				Payload: payload, FieldPresence: presence, ObservedAt: observed, ExpiresAt: expires,
			}
			var applied bool
			var persistErr error
			if status == "ok" && kind == proxy.GrokControlPlaneUser && grokSubscriptionFactChanged(old, payload, presence) {
				applied, persistErr = h.db.UpsertGrokAccountFactAndExpireCapabilities(ctx, fact, observed)
			} else {
				applied, persistErr = h.db.UpsertGrokAccountFact(ctx, fact)
			}
			if persistErr != nil {
				return nil, persistErr
			}
			if !applied {
				return nil, errGrokCredentialChanged
			}
			if status == "ok" && kind == proxy.GrokControlPlaneBilling {
				for key, value := range proxy.LegacyGrokBillingCredentialsFromFact(account, payload, presence) {
					legacyUpdates[key] = value
				}
			}
			result.SyncedFacts = append(result.SyncedFacts, string(kind))
		}
	}

	if selection.Catalog {
		origin, _ := account.GrokCredentials()
		origin = strings.TrimRight(strings.TrimSpace(origin), "/")
		var oldSnapshot *database.GrokModelCatalogSnapshot
		var oldItems []database.GrokModelCatalogItem
		if snapshot, items, oldErr := h.db.GetGrokModelCatalog(ctx, id, origin); oldErr == nil && snapshot.CredentialGeneration == generation {
			oldSnapshot, oldItems = snapshot, items
		}
		ifNoneMatch := ""
		if oldSnapshot != nil {
			ifNoneMatch = oldSnapshot.HTTPETag
		}
		catalog, catalogErr := proxy.FetchGrokModelCatalog(ctx, account, proxyURL, ifNoneMatch)
		if catalogErr != nil {
			result.Errors["models"] = safeGrokUpstreamError(catalogErr)
			status := "unavailable"
			var upstream *proxy.GrokHTTPError
			if proxy.AsGrokHTTPError(catalogErr, &upstream) {
				status = factStatus(upstream.StatusCode)
			}
			snapshot := database.GrokModelCatalogSnapshot{AccountID: id, Origin: origin, CredentialGeneration: generation,
				AuthKind: account.GrokAuthKind(), Status: status, ObservedAt: time.Now(), ExpiresAt: time.Now()}
			if oldSnapshot != nil {
				snapshot.HTTPETag, snapshot.ETagHint = oldSnapshot.HTTPETag, oldSnapshot.ETagHint
				snapshot.ETagHintObservedAt = oldSnapshot.ETagHintObservedAt
			}
			// ReplaceGrokModelCatalog records the failed attempt without replacing a
			// successful snapshot. In particular, this attempt timestamp must never
			// become the baseline for the one-hour stale-if-error routing window.
			applied, persistErr := h.db.ReplaceGrokModelCatalog(ctx, snapshot, oldItems)
			if persistErr != nil {
				return nil, persistErr
			}
			if !applied {
				return nil, errGrokCredentialChanged
			}
			result.Models = visiblePersistedModelIDs(oldItems, account.GrokAuthKind())
		} else if catalog.NotModified {
			applied, touchErr := h.db.TouchGrokModelCatalogNotModified(ctx, id, origin, generation, catalog.ObservedAt, catalog.ObservedAt.Add(grokFactFreshness))
			if touchErr != nil {
				return nil, touchErr
			}
			if !applied {
				return nil, errGrokCredentialChanged
			}
			if catalog.ModelsETagHint != "" {
				_, _ = h.db.UpdateGrokModelsETagHint(ctx, id, origin, generation, catalog.ModelsETagHint, catalog.ObservedAt)
			}
			result.Models = visiblePersistedModelIDs(oldItems, account.GrokAuthKind())
		} else {
			items := persistedCatalogItems(catalog.Models)
			snapshot := database.GrokModelCatalogSnapshot{
				AccountID: id, Origin: origin, CredentialGeneration: generation, AuthKind: account.GrokAuthKind(),
				Status: "ok", HTTPETag: catalog.HTTPETag, ETagHint: catalog.ModelsETagHint,
				ObservedAt: catalog.ObservedAt, ExpiresAt: catalog.ObservedAt.Add(grokFactFreshness),
			}
			if catalog.ModelsETagHint != "" {
				snapshot.ETagHintObservedAt = catalog.ObservedAt
			}
			applied, persistErr := h.db.ReplaceGrokModelCatalog(ctx, snapshot, items)
			if persistErr != nil {
				return nil, persistErr
			}
			if !applied {
				return nil, errGrokCredentialChanged
			}
			result.Models = proxy.VisibleGrokModelIDs(catalog.Models, account.GrokAuthKind())
		}
	}
	// credentials.models 是运营声明的调度白名单（空=未声明）。上游目录已经
	// 落在 grok_model_catalog；再写回 models 会把批量/编辑设置的白名单覆盖成
	// 当前可见目录（免费 OAuth 常见只剩 grok-4.6），几分钟后列表就“丢模型”。
	if len(legacyUpdates) > 0 {
		applied, persistErr := h.db.MergeAccountCredentialsForGeneration(ctx, id, generation, legacyUpdates)
		if persistErr != nil {
			return nil, persistErr
		}
		if !applied {
			return nil, errGrokCredentialChanged
		}
	}
	if err := h.store.ReloadGrokPersistentState(ctx, id); err != nil {
		return nil, err
	}
	result.State, err = h.db.GetGrokAccountState(ctx, id)
	if err == nil && grokGenerationNeedsCapabilityProbe(account, result.State, generation, time.Now()) {
		// A successful catalog replacement can invalidate capabilities without
		// changing credentials. Always inspect the current generation after a
		// sync instead of limiting rebuilds to OAuth rotations.
		result.capabilityGeneration = generation
	}
	if len(result.Errors) == 0 {
		result.Errors = nil
	}
	return result, err
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

// ProbeGrokAccountCapabilities forces a fresh minimal request to every native
// endpoint for each visible catalog model.
func (h *Handler) ProbeGrokAccountCapabilities(c *gin.Context) {
	id, ok := parseGrokAdminAccountID(c)
	if !ok {
		return
	}
	response, err := h.runGrokCapabilityProbe(c.Request.Context(), id, true)
	if err != nil {
		writeGrokAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Grok 三协议能力探测完成", "state": response.State, "results": response.Results})
}

func accountProbeLock(id int64) chan struct{} {
	lock, _ := grokCapabilityAccountLocks.LoadOrStore(id, make(chan struct{}, 1))
	return lock.(chan struct{})
}

type grokCapabilityProbeTarget struct {
	model        string
	origin       string
	extraHeaders map[string]string
}

func normalizeGrokProbeOrigin(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

// grokCapabilityTargetOrigin mirrors auth.Account.GetGrokModelRoute: OAuth
// models prefer base_url, API-key models prefer api_base_url, and both fall
// back to the account credential origin before their conservative defaults.
func grokCapabilityTargetOrigin(account *auth.Account, item database.GrokModelCatalogItem) string {
	if account == nil {
		return ""
	}
	origin := normalizeGrokProbeOrigin(item.BaseURL)
	if account.GrokAuthKind() == auth.GrokAuthKindAPIKey {
		if apiOrigin := normalizeGrokProbeOrigin(item.APIBaseURL); apiOrigin != "" {
			origin = apiOrigin
		}
	}
	if origin == "" {
		credentialOrigin, _ := account.GrokCredentials()
		origin = normalizeGrokProbeOrigin(credentialOrigin)
	}
	if origin == "" {
		if account.GrokAuthKind() == auth.GrokAuthKindAPIKey {
			origin = auth.GrokDefaultAPIBaseURL
		} else {
			origin = auth.GrokDefaultChatProxyBaseURL
		}
	}
	return normalizeGrokProbeOrigin(origin)
}

func cloneGrokProbeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(headers))
	for name, value := range headers {
		cloned[name] = value
	}
	return cloned
}

func grokCapabilityProbeKey(model, origin, protocol string) string {
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" +
		strings.ToLower(normalizeGrokProbeOrigin(origin)) + "\x00" +
		strings.ToLower(strings.TrimSpace(protocol))
}

func grokCapabilityProbeTargets(account *auth.Account, state *database.GrokAccountState, generation int64) ([]grokCapabilityProbeTarget, bool) {
	seen := map[string]struct{}{}
	var targets []grokCapabilityProbeTarget
	catalogKnown := false
	if state != nil {
		for _, catalog := range state.Catalogs {
			if catalog.Snapshot.CredentialGeneration != generation {
				continue
			}
			catalogKnown = true
			for _, item := range catalog.Items {
				model := strings.TrimSpace(item.ModelID)
				if item.CredentialGeneration != generation || model == "" || item.Hidden ||
					(account.GrokAuthKind() == auth.GrokAuthKindAPIKey && item.FieldPresence["supported_in_api"] == "value" && !item.SupportedInAPI) {
					continue
				}
				origin := grokCapabilityTargetOrigin(account, item)
				key := strings.ToLower(model) + "\x00" + strings.ToLower(origin)
				if origin == "" {
					continue
				}
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				targets = append(targets, grokCapabilityProbeTarget{
					model: model, origin: origin, extraHeaders: cloneGrokProbeHeaders(item.ExtraHeaders),
				})
			}
		}
	}
	if len(targets) == 0 && !catalogKnown {
		origin, _ := account.GrokCredentials()
		origin = normalizeGrokProbeOrigin(origin)
		for _, model := range proxy.DefaultGrokModelIDsForAccount(account) {
			targets = append(targets, grokCapabilityProbeTarget{model: model, origin: origin})
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		left, right := strings.ToLower(targets[i].model), strings.ToLower(targets[j].model)
		if left == right {
			return strings.ToLower(targets[i].origin) < strings.ToLower(targets[j].origin)
		}
		return left < right
	})
	return targets, catalogKnown
}

func grokGenerationNeedsCapabilityProbe(account *auth.Account, state *database.GrokAccountState, generation int64, now time.Time) bool {
	if account == nil || state == nil || state.CredentialGeneration != generation {
		return true
	}
	targets, _ := grokCapabilityProbeTargets(account, state, generation)
	if len(targets) == 0 {
		// An authoritative empty catalog has nothing to probe. Treat it as
		// complete so the maintenance scan does not generate an endless no-op.
		return false
	}
	fresh := make(map[string]struct{}, len(state.Capabilities))
	for _, capability := range state.Capabilities {
		if capability.CredentialGeneration == generation && !capability.ExpiresAt.IsZero() && now.Before(capability.ExpiresAt) {
			fresh[grokCapabilityProbeKey(capability.ModelID, capability.Origin, capability.Protocol)] = struct{}{}
		}
	}
	for _, target := range targets {
		for _, protocol := range []proxy.GrokProtocol{proxy.GrokProtocolResponses, proxy.GrokProtocolChatCompletions, proxy.GrokProtocolMessages} {
			if _, ok := fresh[grokCapabilityProbeKey(target.model, target.origin, string(protocol))]; !ok {
				return true
			}
		}
	}
	return false
}

func (h *Handler) runGrokCapabilityProbe(ctx context.Context, id int64, force bool) (*grokCapabilityProbeResponse, error) {
	lock := accountProbeLock(id)
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-lock }()
	select {
	case grokCapabilitySlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-grokCapabilitySlots }()

	account, row, err := h.grokAdminAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	state, err := h.db.GetGrokAccountState(ctx, id)
	if err != nil {
		return nil, err
	}
	generation := row.CredentialGeneration
	targets, _ := grokCapabilityProbeTargets(account, state, generation)
	existing := map[string]database.GrokModelCapability{}
	for _, capability := range state.Capabilities {
		key := grokCapabilityProbeKey(capability.ModelID, capability.Origin, capability.Protocol)
		existing[key] = capability
	}
	protocols := []proxy.GrokProtocol{proxy.GrokProtocolResponses, proxy.GrokProtocolChatCompletions, proxy.GrokProtocolMessages}
	response := &grokCapabilityProbeResponse{}
	for _, target := range targets {
		for _, protocol := range protocols {
			key := grokCapabilityProbeKey(target.model, target.origin, string(protocol))
			if previous, ok := existing[key]; !force && ok && previous.CredentialGeneration == generation && time.Now().Before(previous.ExpiresAt) {
				continue
			}
			probeCtx, cancel := context.WithTimeout(ctx, grokCapabilityProbeTimeout)
			started := time.Now()
			resp, requestErr := proxy.ExecuteGrokNativeProtocolProbeAtOriginWithHeaders(probeCtx, account, protocol, target.model, proxy.MinimalGrokProbeBody(protocol, target.model), target.origin, h.store.ResolveProxyForAccount(account), target.extraHeaders)
			observation := inspectGrokProbeResponse(probeCtx, protocol, resp, requestErr, started)
			cancel()
			currentGeneration, _, generationErr := h.db.GetAccountCredentialState(ctx, id)
			if generationErr != nil {
				return nil, generationErr
			}
			if currentGeneration != generation {
				return nil, errGrokCredentialChanged
			}
			observed := time.Now()
			ttl := grokCapabilityFailureTTL
			if observation.status == "ok" {
				ttl = grokCapabilityOKTTL
			}
			capability := database.GrokModelCapability{
				AccountID: id, ModelID: target.model, Origin: target.origin, Protocol: string(protocol),
				CredentialGeneration: generation, Status: observation.status, HTTPStatus: observation.httpStatus,
				ProviderCode: observation.providerCode, Source: "native_probe", RetryAfterSeconds: observation.retryAfter,
				ObservedAt: observed, ExpiresAt: observed.Add(ttl),
			}
			applied, persistErr := h.db.UpsertGrokModelCapability(ctx, capability)
			if persistErr != nil {
				return nil, persistErr
			}
			if !applied {
				return nil, errGrokCredentialChanged
			}
			_ = h.db.InsertUsageLog(context.Background(), &database.UsageLogInput{
				AccountID: id, CredentialGeneration: generation, Channel: database.UpstreamChannelGrok, InternalReason: "grok_capability_probe",
				Endpoint: grokProtocolPath(protocol), InboundEndpoint: grokProtocolPath(protocol), UpstreamEndpoint: grokProtocolPath(protocol),
				Model: target.model, EffectiveModel: target.model, StatusCode: observation.httpStatus,
				DurationMs: int(time.Since(started).Milliseconds()), FirstTokenMs: observation.firstTokenMs,
				InputTokens: observation.inputTokens, OutputTokens: observation.outputTokens,
				PromptTokens: observation.inputTokens, CompletionTokens: observation.outputTokens,
				TotalTokens:     observation.inputTokens + observation.outputTokens,
				ReasoningTokens: observation.reasoningTokens, Stream: true,
			})
			response.Results = append(response.Results, grokCapabilityProbeResult{
				ModelID: target.model, Protocol: string(protocol), Status: observation.status,
				HTTPStatus: observation.httpStatus, ProviderCode: observation.providerCode,
				RetryAfterSeconds: observation.retryAfter, ObservedAt: observed,
			})
		}
	}
	if err := h.store.ReloadGrokPersistentState(ctx, id); err != nil {
		return nil, err
	}
	response.State, err = h.db.GetGrokAccountState(ctx, id)
	return response, err
}

type grokProbeObservation struct {
	status                                     string
	httpStatus                                 int
	providerCode                               string
	retryAfter                                 int64
	inputTokens, outputTokens, reasoningTokens int
	firstTokenMs                               int
}

func grokProtocolPath(protocol proxy.GrokProtocol) string {
	switch protocol {
	case proxy.GrokProtocolChatCompletions:
		return "/v1/chat/completions"
	case proxy.GrokProtocolMessages:
		return "/v1/messages"
	default:
		return "/v1/responses"
	}
}

func sanitizeGrokProviderCode(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 {
		value = value[:64]
	}
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func providerCodeFromJSON(data []byte) string {
	for _, path := range []string{"error.code", "response.error.code", "error.type", "code", "type"} {
		if value := sanitizeGrokProviderCode(gjson.GetBytes(data, path).String()); value != "" && value != "error" {
			return value
		}
	}
	return ""
}

func classifyProbeStatus(status int, code string) string {
	if status == http.StatusUnauthorized {
		return "unauthorized"
	}
	if status == http.StatusForbidden {
		return "subscription_required"
	}
	if status == http.StatusTooManyRequests {
		return "rate_limited"
	}
	if status == http.StatusUpgradeRequired {
		return "version_required"
	}
	lower := strings.ToLower(strings.TrimSpace(code))
	explicitBalanceExhaustion := (strings.Contains(lower, "balance") || strings.Contains(lower, "credit")) &&
		(strings.Contains(lower, "exhaust") || strings.Contains(lower, "insufficient") || strings.Contains(lower, "deplet"))
	if status == http.StatusPaymentRequired {
		if explicitBalanceExhaustion {
			return "exhausted"
		}
		// A bare or generic 402 is not proof that prepaid/PAYG balance is
		// exhausted (for example credit_card_required).
		return "payment_required"
	}
	switch {
	case strings.Contains(lower, "unauth"), strings.Contains(lower, "token"):
		return "unauthorized"
	case explicitBalanceExhaustion:
		return "exhausted"
	case strings.Contains(lower, "subscription"), strings.Contains(lower, "permission"), strings.Contains(lower, "forbidden"):
		return "subscription_required"
	case strings.Contains(lower, "rate"):
		return "rate_limited"
	case strings.Contains(lower, "version"), strings.Contains(lower, "upgrade"):
		return "version_required"
	default:
		return "unavailable"
	}
}

// grokProbeResponsesTerminalOK 判断 Responses 探针是否收到了"这口通了"的终态。
// completed 与 incomplete 同为正常终态：探针体只给 1 个 output token，思考型
// 模型必然以 incomplete 收场，把它排除在外等于永远判不出 ok。
// status 取自非流式 body 的 response.status，eventType 取自流式事件的 type；
// 调用方只关心其中一个时另一个传空串。
func grokProbeResponsesTerminalOK(status, eventType string) bool {
	switch status {
	case "completed", "incomplete":
		return true
	}
	switch eventType {
	case "response.completed", "response.incomplete":
		return true
	}
	return false
}

func retryAfterSeconds(header http.Header, now time.Time) int64 {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return seconds
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return int64(time.Until(at).Seconds())
	}
	return 0
}

func updateProbeUsage(observation *grokProbeObservation, protocol proxy.GrokProtocol, data []byte) {
	read := func(paths ...string) int {
		for _, path := range paths {
			if value := gjson.GetBytes(data, path); value.Exists() {
				return int(value.Int())
			}
		}
		return 0
	}
	switch protocol {
	case proxy.GrokProtocolChatCompletions:
		observation.inputTokens = max(observation.inputTokens, read("usage.prompt_tokens"))
		observation.outputTokens = max(observation.outputTokens, read("usage.completion_tokens"))
		observation.reasoningTokens = max(observation.reasoningTokens, read("usage.completion_tokens_details.reasoning_tokens"))
	case proxy.GrokProtocolMessages:
		observation.inputTokens = max(observation.inputTokens, read("message.usage.input_tokens", "usage.input_tokens"))
		observation.outputTokens = max(observation.outputTokens, read("message.usage.output_tokens", "usage.output_tokens"))
	default:
		observation.inputTokens = max(observation.inputTokens, read("response.usage.input_tokens", "usage.input_tokens"))
		observation.outputTokens = max(observation.outputTokens, read("response.usage.output_tokens", "usage.output_tokens"))
		observation.reasoningTokens = max(observation.reasoningTokens, read("response.usage.output_tokens_details.reasoning_tokens", "usage.output_tokens_details.reasoning_tokens"))
	}
}

func inspectGrokProbeResponse(ctx context.Context, protocol proxy.GrokProtocol, resp *http.Response, requestErr error, startedAt time.Time) grokProbeObservation {
	observation := grokProbeObservation{status: "unavailable"}
	if requestErr != nil || resp == nil {
		return observation
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	observation.httpStatus = resp.StatusCode
	observation.retryAfter = retryAfterSeconds(resp.Header, time.Now())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		observation.providerCode = providerCodeFromJSON(body)
		observation.status = classifyProbeStatus(resp.StatusCode, observation.providerCode)
		return observation
	}
	streaming := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream")
	if !streaming {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		updateProbeUsage(&observation, protocol, body)
		observation.providerCode = providerCodeFromJSON(body)
		switch protocol {
		case proxy.GrokProtocolChatCompletions:
			if gjson.GetBytes(body, "choices.0.finish_reason").String() != "" {
				observation.status = "ok"
			}
		case proxy.GrokProtocolMessages:
			if gjson.GetBytes(body, "type").String() == "message" && gjson.GetBytes(body, "stop_reason").String() != "" {
				observation.status = "ok"
			}
		default:
			// 探针体带 max_output_tokens:1，正常收尾必然是 incomplete 而非
			// completed；只认 completed 会把每个通的 Responses 口判成
			// unavailable（http_status=200 却 status=unavailable）。
			if grokProbeResponsesTerminalOK(gjson.GetBytes(body, "status").String(), gjson.GetBytes(body, "type").String()) {
				observation.status = "ok"
			}
		}
		if observation.status != "ok" {
			observation.status = classifyProbeStatus(resp.StatusCode, observation.providerCode)
		}
		if observation.status == "ok" && observation.firstTokenMs == 0 && !startedAt.IsZero() {
			observation.firstTokenMs = max(int(time.Since(startedAt).Milliseconds()), 1)
		}
		return observation
	}
	var responsesCompleted, chatFinished, messagesStopReason, messagesStopped, failed bool
	_ = proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		updateProbeUsage(&observation, protocol, data)
		if observation.firstTokenMs == 0 && proxy.GrokStreamEventIsVisible(protocol, data) && !startedAt.IsZero() {
			observation.firstTokenMs = max(int(time.Since(startedAt).Milliseconds()), 1)
		}
		eventType := gjson.GetBytes(data, "type").String()
		switch protocol {
		case proxy.GrokProtocolResponses:
			// 同上：截断终态 response.incomplete 也是"这口通了"的证据。
			if grokProbeResponsesTerminalOK("", eventType) && gjson.GetBytes(data, "response.status").String() != "failed" {
				responsesCompleted = true
			}
			if eventType == "response.failed" || eventType == "error" {
				failed = true
			}
		case proxy.GrokProtocolChatCompletions:
			if gjson.GetBytes(data, "choices.0.finish_reason").String() != "" {
				chatFinished = true
			}
			if eventType == "error" {
				failed = true
			}
		case proxy.GrokProtocolMessages:
			if eventType == "message_delta" && gjson.GetBytes(data, "delta.stop_reason").String() != "" {
				messagesStopReason = true
			}
			if eventType == "message_stop" {
				messagesStopped = true
			}
			if eventType == "error" {
				failed = true
			}
		}
		if failed && observation.providerCode == "" {
			observation.providerCode = providerCodeFromJSON(data)
		}
		return !failed
	})
	if ctx.Err() != nil {
		return observation
	}
	if !failed && (responsesCompleted || chatFinished || (messagesStopReason && messagesStopped)) {
		observation.status = "ok"
	} else {
		observation.status = classifyProbeStatus(resp.StatusCode, observation.providerCode)
	}
	// Grok often emits the only visible token on the terminal frame. Without a
	// prior delta, first_token_ms would stay 0 and the usage table shows 首字-.
	if observation.status == "ok" && observation.firstTokenMs == 0 && !startedAt.IsZero() {
		observation.firstTokenMs = max(int(time.Since(startedAt).Milliseconds()), 1)
	}
	return observation
}

func (h *Handler) triggerGrokCapabilityProbeForGeneration(accountID, generation int64) {
	if h == nil || h.db == nil || accountID <= 0 || generation <= 0 {
		return
	}
	h.startDBBackgroundTask(func(parent context.Context) {
		current, _, err := h.db.GetAccountCredentialState(parent, accountID)
		if err != nil || current != generation {
			return
		}
		if _, err = h.runGrokCapabilityProbe(parent, accountID, false); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[账号 %d] generation %d Grok 协议能力重建失败: %v", accountID, generation, err)
		}
	})
}

// RunGrokAcceptanceSync exposes the same fenced control-plane/catalog sync and
// low-cost native protocol probes used by the admin endpoints to local
// acceptance harnesses. It intentionally addresses an explicit account ID and
// therefore does not reserve/dispatch the account or bypass credential fencing.
func (h *Handler) RunGrokAcceptanceSync(ctx context.Context, accountID int64) (*database.GrokAccountState, error) {
	if _, err := h.syncGrokAccountState(ctx, accountID); err != nil {
		return nil, err
	}
	result, err := h.runGrokCapabilityProbe(ctx, accountID, true)
	if err != nil {
		return nil, err
	}
	return result.State, nil
}
