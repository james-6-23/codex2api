package proxy

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const usageRequestDiagnosticsContextKey = "usage_request_diagnostics_v1"

type usageRequestResolution struct {
	ThreadSource            string `json:"thread_source"`
	RequestKind             string `json:"request_kind"`
	SubagentKind            string `json:"subagent_kind"`
	IdentitySource          string `json:"identity_source"`
	PolicyStatus            string `json:"newapi_policy_status"`
	RootState               string `json:"root_state"`
	RootID                  string `json:"root_session_id"`
	RootFingerprint         string `json:"root_fingerprint"`
	OriginalRootFingerprint string `json:"original_root_fingerprint,omitempty"`
	RootAssociation         string `json:"root_association,omitempty"`
	RootCandidateCount      *int   `json:"root_candidate_count,omitempty"`
	WindowKeyHash           string `json:"window_key_hash"`
	AffinityKeyHash         string `json:"affinity_key_hash"`
	Related                 bool   `json:"related_to_root"`
	OwnsUserRoot            bool   `json:"owns_user_root"`
	Stable                  bool   `json:"stable_identity"`
	Fingerprint             bool   `json:"has_request_fingerprint"`
	Passive                 bool   `json:"passive_authorized"`
	WindowBypass            bool   `json:"window_accounting_bypass"`
	Unlinked                bool   `json:"unlinked_fallback_only"`
	RequiresRootAccount     bool   `json:"requires_root_account"`
}

type usageRequestAuthorization struct {
	Passive     bool `json:"passive_authorized"`
	ModelBypass bool `json:"passive_models_allowed"`
}

type usageRecentAccountDiagnostic struct {
	Enabled    bool      `json:"enabled"`
	Result     string    `json:"result"`
	Scope      string    `json:"scope_hash,omitempty"`
	AccountID  int64     `json:"account_id,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

type usageRequestDiagnostics struct {
	Version               int                          `json:"version"`
	StartedAt             time.Time                    `json:"started_at"`
	CompletedAt           time.Time                    `json:"completed_at"`
	CorrelationID         string                       `json:"correlation_id"`
	NewAPIRequestID       string                       `json:"newapi_request_id,omitempty"`
	Incoming              map[string]map[string]string `json:"incoming"`
	Resolved              *usageRequestResolution      `json:"resolved,omitempty"`
	Audit                 *usageRequestAuthorization   `json:"audit,omitempty"`
	Dispatch              *usageRequestAuthorization   `json:"dispatch,omitempty"`
	ClassificationChanged bool                         `json:"classification_changed"`
	RootAccountID         int64                        `json:"root_account_id,omitempty"`
	RootAccountLookup     string                       `json:"root_account_lookup"`
	RootAccountWait       string                       `json:"root_account_wait,omitempty"`
	RootAccountWaitMillis int64                        `json:"root_account_wait_millis,omitempty"`
	Naming                string                       `json:"naming,omitempty"`
	WindowGrant           string                       `json:"window_grant,omitempty"`
	UserWindowKeyHash     string                       `json:"user_window_key_hash,omitempty"`
	UserWindow            string                       `json:"user_window"`
	AccountWindow         string                       `json:"account_window"`
	Recent                usageRecentAccountDiagnostic `json:"recent_account"`
	SelectedAccountID     int64                        `json:"selected_account_id,omitempty"`
	Selection             string                       `json:"selection"`
	CandidateRejections   []string                     `json:"candidate_rejections,omitempty"`
	Attempt               int                          `json:"attempt"`
	Truncated             bool                         `json:"truncated,omitempty"`
	rootCaptured          bool
}

func usageRequestDiagnosticState(c *gin.Context) *usageRequestDiagnostics {
	if c == nil {
		return nil
	}
	if value, found := c.Get(usageRequestDiagnosticsContextKey); found {
		if state, ok := value.(*usageRequestDiagnostics); ok && state != nil {
			return state
		}
	}
	state := &usageRequestDiagnostics{
		Version: 1, StartedAt: time.Now().UTC(), CorrelationID: ensurePromptPolicyRequestCorrelationID(c),
		Incoming: make(map[string]map[string]string), UserWindow: "not_reached", AccountWindow: "not_reached", RootAccountLookup: "not_checked",
		Recent: usageRecentAccountDiagnostic{Result: "not_attempted"}, Selection: "not_recorded", Attempt: 1,
	}
	c.Set(usageRequestDiagnosticsContextKey, state)
	return state
}

func diagnosticIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 128 && validSessionGraphUUID(value) {
		return value
	}
	if root, suffix, found := strings.Cut(value, ":"); found && validSessionGraphUUID(root) && len(suffix) > 0 && len(suffix) <= 8 {
		valid := true
		for _, char := range suffix {
			valid = valid && char >= '0' && char <= '9'
		}
		if valid {
			return value
		}
	}
	return "hash:" + hashRiskIdentity(value)
}

func diagnosticRequestID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 20 && len(value) <= 96 {
		valid := true
		for index, char := range value {
			if index < 14 {
				valid = valid && char >= '0' && char <= '9'
			} else {
				valid = valid && ((char >= '0' && char <= '9') || (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z'))
			}
		}
		if valid {
			return value
		}
	}
	return diagnosticIdentifier(value)
}

func diagnosticLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > 64 || strings.HasPrefix(strings.ToLower(value), "sk-") || strings.HasPrefix(strings.ToLower(value), "eyj") {
		return diagnosticIdentifier(value)
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.') {
			return diagnosticIdentifier(value)
		}
	}
	return value
}

func diagnosticMetadataObject(raw gjson.Result) gjson.Result {
	for depth := 0; raw.Type == gjson.String && depth < 4; depth++ {
		if len(raw.String()) > 16384 {
			return raw
		}
		raw = gjson.Parse(raw.String())
	}
	return raw
}

func diagnosticMetadata(raw gjson.Result) map[string]string {
	raw = diagnosticMetadataObject(raw)
	if !raw.Exists() {
		return nil
	}
	if !raw.IsObject() || len(raw.Raw) > 16384 {
		return map[string]string{"metadata_status": "invalid_or_too_large"}
	}
	result := make(map[string]string)
	for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id", "window_id", "turn_id", "parent_turn_id", "root_turn_id", "thread_source", "request_kind", "subagent_kind", "client_request_id", "x-client-request-id", "x_client_request_id", "x-codex-window-id", "x_codex_window_id", "x-codex-parent-thread-id", "x_codex_parent_thread_id", "x-codex-forked-from-thread-id", "x_codex_forked_from_thread_id", "x-openai-subagent", "x_openai_subagent"} {
		value := raw.Get(field)
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.String {
			result[field] = "invalid_type"
			continue
		}
		if field == "thread_source" || field == "request_kind" || field == "subagent_kind" || field == "x-openai-subagent" || field == "x_openai_subagent" {
			result[field] = diagnosticLabel(value.String())
		} else {
			result[field] = diagnosticIdentifier(value.String())
		}
	}
	return result
}

func captureUsageDiagnosticMetadata(state *usageRequestDiagnostics, source string, raw gjson.Result, depth int) {
	if !raw.Exists() {
		return
	}
	if depth > 3 || len(state.Incoming) >= 10 {
		state.Truncated = true
		return
	}
	raw = diagnosticMetadataObject(raw)
	state.Incoming[source] = diagnosticMetadata(raw)
	if !raw.IsObject() || len(raw.Raw) > 16384 {
		return
	}
	for _, key := range []string{"x-codex-turn-metadata", "x_codex_turn_metadata", "client_metadata"} {
		captureUsageDiagnosticMetadata(state, source+"."+key, raw.Get(key), depth+1)
	}
}

func captureUsageRequestIngress(c *gin.Context, body []byte) {
	state := usageRequestDiagnosticState(c)
	if state == nil || state.rootCaptured {
		return
	}
	state.rootCaptured = true
	headers := make(map[string]string)
	if c.Request != nil {
		for _, name := range []string{"Session-Id", "Session_id", "Conversation-Id", "Thread-Id", "X-Client-Request-Id", "X-Codex-Window-Id", "X-Codex-Parent-Thread-Id", "X-Codex-Forked-From-Thread-Id", "X-OpenAI-Subagent"} {
			values := c.Request.Header.Values(name)
			if len(values) > 0 {
				value := values[0]
				if name == "X-OpenAI-Subagent" {
					headers[name] = diagnosticLabel(value)
				} else {
					headers[name] = diagnosticIdentifier(value)
				}
				if len(values) > 1 {
					headers[name+"_multiple"] = "true"
				}
			}
		}
		if raw := c.GetHeader(codexTurnMetadataHeader); raw != "" {
			if len(raw) <= 16384 {
				state.Incoming["turn_metadata_header"] = diagnosticMetadata(gjson.Parse(raw))
			} else {
				state.Incoming["turn_metadata_header"] = map[string]string{"metadata_status": "too_large"}
			}
		}
	}
	state.Incoming["headers"] = headers
	if len(body) > 0 {
		captureUsageDiagnosticMetadata(state, "client_metadata", gjson.GetBytes(body, "client_metadata"), 0)
	}
}

func (h *Handler) captureUsageRequestResolution(c *gin.Context, body []byte, identity requestSessionIdentity, root requestRootSessionIdentity, policy verifiedNewAPIPolicyContext, status string) {
	captureUsageRequestIngress(c, body)
	state := usageRequestDiagnosticState(c)
	if state == nil {
		return
	}
	resolution := &usageRequestResolution{
		ThreadSource: diagnosticLabel(root.threadSource), RequestKind: diagnosticLabel(root.requestKind), SubagentKind: diagnosticLabel(root.subagentKind),
		PolicyStatus: status, RootState: "unavailable", RootID: diagnosticIdentifier(root.sessionID), RootFingerprint: root.fingerprint,
		Related: identity.relatedToRoot, OwnsUserRoot: identity.ownsRootBinding, Stable: identity.stableIdentity, Fingerprint: identity.hasRequestFingerprint,
		Passive: passiveInternalRequestAuthorized(c), WindowBypass: identity.bypassWindowAccounting,
		Unlinked: identity.unlinkedFallbackOnly, RequiresRootAccount: identity.requiresRootAccount, IdentitySource: "unavailable",
	}
	if root.stable {
		resolution.RootState = "resolved"
		resolution.IdentitySource = "explicit_session"
	}
	if root.nativeRoot {
		resolution.IdentitySource = "native_graph"
	}
	if root.authoritative {
		resolution.IdentitySource = "signed_newapi"
	}
	if root.conflict {
		resolution.RootState = "conflict"
	}
	if root.sessionID != "" {
		resolution.WindowKeyHash = hashRiskIdentity(root.sessionID)
	}
	if key := capacityAwareSessionAffinityKey(identity, requestAPIKeyID(c)); key != "" {
		resolution.AffinityKeyHash = hashRiskIdentity(key)
	}
	state.Resolved = resolution
	if policy.MetaVerified {
		resolution.OriginalRootFingerprint = policy.Meta.OriginalRootFingerprint
		resolution.RootAssociation = policy.Meta.RootAssociation
		if policy.Meta.RootCandidateCount != nil {
			count := *policy.Meta.RootCandidateCount
			resolution.RootCandidateCount = &count
		}
		state.NewAPIRequestID = diagnosticRequestID(policy.Identity.RequestID)
		state.Incoming["signed_newapi"] = map[string]string{
			"thread_source": diagnosticLabel(policy.Meta.ThreadSource), "request_kind": diagnosticLabel(policy.Meta.RequestKind),
			"subagent_kind": diagnosticLabel(policy.Meta.SubagentKind), "root_state": policy.Meta.RootSessionState,
			"root_relation": policy.Meta.RootSessionRelation, "root_fingerprint": policy.Meta.RootSessionFingerprint,
			"session_fingerprint": policy.Meta.SessionFingerprint, "session_accounting": policy.Meta.SessionAccounting,
			"passive_feature": policy.Meta.PassiveFeature,
		}
	}
	if h != nil && h.store != nil {
		state.Recent.Enabled = h.store.CodexUnlinkedAccountFallbackEnabled()
	}
	state.Recent.Scope = identity.unlinkedFallbackScope
}

func (h *Handler) recordUsageAuthorization(c *gin.Context, stage string) {
	state := usageRequestDiagnosticState(c)
	if state == nil {
		return
	}
	value := &usageRequestAuthorization{Passive: passiveInternalRequestAuthorized(c), ModelBypass: h.passiveInternalModelsAllowed(c)}
	if stage == "audit" {
		state.Audit = value
	} else {
		state.Dispatch = value
	}
	if state.Resolved != nil && state.Resolved.Passive != value.Passive || state.Audit != nil && state.Dispatch != nil && *state.Audit != *state.Dispatch {
		state.ClassificationChanged = true
	}
}

func setUsageUserWindow(c *gin.Context, result string) {
	if state := usageRequestDiagnosticState(c); state != nil {
		state.UserWindow = result
	}
}

func recordUsageRootAccount(c *gin.Context, accountID int64, found bool) {
	if state := usageRequestDiagnosticState(c); state != nil {
		if found {
			state.RootAccountID = accountID
			state.RootAccountLookup = "found"
		} else if state.RootAccountLookup != "found" {
			state.RootAccountLookup = "missing"
		}
	}
}

func beginUsageSelectionAttempt(c *gin.Context, attempt int) {
	if state := usageRequestDiagnosticState(c); state != nil {
		state.Attempt = attempt
		state.UserWindow, state.AccountWindow = "not_reached", "not_reached"
		state.Recent.Result, state.Recent.AccountID = "not_attempted", 0
		state.Recent.ObservedAt = time.Time{}
	}
}

func populateUsageRequestDiagnostics(c *gin.Context, input *database.UsageLogInput) {
	if input == nil {
		return
	}
	state := usageRequestDiagnosticState(c)
	if state == nil {
		return
	}
	snapshot := *state
	snapshot.CompletedAt = time.Now().UTC()
	snapshot.SelectedAccountID = input.AccountID
	if input.AttemptIndex > 0 {
		snapshot.Attempt = input.AttemptIndex
	}
	input.RequestType = "unknown"
	if snapshot.Resolved != nil {
		resolved := snapshot.Resolved
		passive := resolved.Passive
		if snapshot.Dispatch != nil {
			passive = snapshot.Dispatch.Passive
		}
		switch {
		case resolved.RequestKind == "compaction" || input.Compact:
			input.RequestType = "compaction"
		case passive && resolved.Related:
			input.RequestType = "related_internal"
		case passive:
			input.RequestType = "independent_internal"
		case resolved.Related && resolved.ThreadSource != "user":
			input.RequestType = "related_unclassified"
		case resolved.RootState == "resolved" && (resolved.ThreadSource == "user" || resolved.ThreadSource == ""):
			input.RequestType = "user"
		}
	}
	if input.InternalReason != "" {
		input.RequestType = "gateway_internal"
	} else if input.Compact {
		input.RequestType = "compaction"
	}
	if input.AccountID > 0 {
		switch {
		case snapshot.RootAccountID == input.AccountID:
			snapshot.Selection = "matches_observed_root"
		case snapshot.Recent.Result == "selected" && snapshot.Recent.AccountID == input.AccountID:
			snapshot.Selection = "recent_account"
		case snapshot.Resolved != nil && snapshot.Resolved.Unlinked:
			snapshot.Selection = "unlinked_scheduling"
		default:
			snapshot.Selection = "scheduled"
		}
	} else {
		snapshot.Selection = "no_account"
	}
	if trace := selectionTraceForRequest(c); trace != nil {
		snapshot.CandidateRejections = trace.Snapshot().Reasons
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	if len(payload) > database.MaxUsageRequestDiagnosticsBytes {
		snapshot.Incoming = nil
		snapshot.Truncated = true
		payload, err = json.Marshal(snapshot)
	}
	if err == nil && len(payload) <= database.MaxUsageRequestDiagnosticsBytes {
		input.RequestDiagnostics = string(payload)
	}
}

func recordUsageAccountWindow(c *gin.Context, account *auth.Account, affinityKey string, previous int64, exceeded bool) {
	state := usageRequestDiagnosticState(c)
	if state == nil || account == nil {
		return
	}
	if previous > 0 {
		recordUsageRootAccount(c, previous, true)
	}
	enabled, _, _ := account.SessionCapacityConfig()
	switch {
	case !enabled:
		state.AccountWindow = "disabled"
	case exceeded:
		state.AccountWindow = "rejected"
	case affinityKey == "" || state.Resolved != nil && (!state.Resolved.Stable || state.Resolved.WindowBypass):
		state.AccountWindow = "exempt_or_unstable"
	case state.Resolved != nil && state.Resolved.Related && !state.Resolved.OwnsUserRoot:
		state.AccountWindow = "root_reused"
	case previous == account.ID():
		state.AccountWindow = "reused"
	default:
		state.AccountWindow = "admitted"
	}
}
