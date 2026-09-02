package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const promptRequestSecurityContextKey = "prompt_filter_request_security_context"
const promptPolicyRequestCorrelationContextKey = "prompt_policy_request_correlation_id"

// promptRequestSecurityContext owns request-local prompt security state. It is
// deliberately separate from the verified NewAPI identity keys because an HTTP
// request has one body/config snapshot while a WebSocket connection can carry
// multiple logical request frames under one verified connection identity.
type promptRequestSecurityContext struct {
	configOwner        *Handler
	config             promptfilter.Config
	configReady        bool
	digestBody         []byte
	digest             [sha256.Size]byte
	digestHex          string
	digestReady        bool
	digestComputations uint8
}

func promptRequestSecurityState(c *gin.Context) *promptRequestSecurityContext {
	if c == nil {
		return nil
	}
	if value, ok := c.Get(promptRequestSecurityContextKey); ok {
		if state, valid := value.(*promptRequestSecurityContext); valid && state != nil {
			return state
		}
	}
	state := &promptRequestSecurityContext{}
	c.Set(promptRequestSecurityContextKey, state)
	return state
}

// resetPromptRequestSecurityFrame starts a fresh per-frame config/digest scope.
// The verified NewAPI identity remains connection-scoped, while the WebSocket
// turn boundary first refreshes/revokes its API-key binding. This keeps policy
// changes hot without letting a removed tenant or expired secret survive on an
// old connection.
func resetPromptRequestSecurityFrame(c *gin.Context) {
	if c != nil {
		c.Set(promptRequestSecurityContextKey, &promptRequestSecurityContext{})
		// A WebSocket connection carries multiple logical requests. Never let a
		// prior turn's upstream CYB decision leak into the next turn.
		c.Set(newAPIUpstreamCyberDecisionContextKey, nil)
	}
}

func (h *Handler) promptFilterConfigForRequest(c *gin.Context) promptfilter.Config {
	if h == nil || h.store == nil {
		return promptfilter.DefaultConfig()
	}
	state := promptRequestSecurityState(c)
	if state == nil {
		return h.store.GetPromptFilterConfigSnapshot()
	}
	if state.configReady && state.configOwner == h {
		return state.config
	}
	state.config = h.store.GetPromptFilterConfigSnapshot()
	// Signed NewAPI identity is available only through an explicit API-key
	// binding. Ignore any retired persisted enablement value.
	state.config.Advanced.NewAPI.Enabled = false
	if binding, bound := h.resolvePromptFilterNewAPIBinding(c); bound {
		state.config.Advanced.NewAPI.Enabled = binding.Enabled
		if binding.Enabled {
			switch binding.PromptFilterScope {
			case database.PromptFilterScopeLocalOnly:
				// Keep the deterministic local GuardPipeline, session correlation,
				// audit evidence and risk profiling, but remove the synchronous
				// remote model hop for this API key.
				state.config.Review.Enabled = false
			case database.PromptFilterScopeOff:
				// This explicit admin-side API-key exception disables Prompt checks
				// only. NewAPI signature verification remains enabled above and API
				// key authentication is enforced before this request-local snapshot.
				state.config.Enabled = false
			}
		}
	}
	state.configOwner = h
	state.configReady = true
	return state.config
}

// capturePromptRequestIngress retains the already-owned request buffer by
// reference when signed NewAPI verification or a Codex-local conversation lock
// can need the pre-mapping body. The latter is required for clients whose only
// stable session signal lives in client_metadata. Callers must treat the buffer
// as immutable; body rewrites use a new slice.
func (h *Handler) capturePromptRequestIngress(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	ensurePromptPolicyRequestCorrelationID(c)
	if h == nil || h.store == nil {
		return
	}
	cfg := h.promptFilterConfigForRequest(c)
	needsSignedBody := cfg.Advanced.NewAPI.Enabled && strings.TrimSpace(c.GetHeader("X-NewAPI-Signature")) != ""
	needsFallbackSessionBody := cfg.Enabled && cfg.Advanced.Enforcement.ConversationLockEnabled
	if !needsSignedBody && !needsFallbackSessionBody {
		return
	}
	setIngressRequestBodyIfAbsent(c, body)
}

func ensurePromptPolicyRequestCorrelationID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if raw, exists := c.Get(promptPolicyRequestCorrelationContextKey); exists {
		if value, ok := raw.(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	value := uuid.NewString()
	c.Set(promptPolicyRequestCorrelationContextKey, value)
	return value
}

func resetPromptPolicyRequestCorrelationID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value := uuid.NewString()
	c.Set(promptPolicyRequestCorrelationContextKey, value)
	return value
}

func sameRequestBodyBuffer(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	if len(left) == 0 {
		return true
	}
	return &left[0] == &right[0]
}

func promptRequestBodyDigest(c *gin.Context, body []byte) ([sha256.Size]byte, string) {
	state := promptRequestSecurityState(c)
	if state != nil && state.digestReady && sameRequestBodyBuffer(state.digestBody, body) {
		return state.digest, state.digestHex
	}
	digest := sha256.Sum256(body)
	digestHex := hex.EncodeToString(digest[:])
	if state != nil {
		state.digestBody = body
		state.digest = digest
		state.digestHex = digestHex
		state.digestReady = true
		state.digestComputations++
	}
	return digest, digestHex
}

func promptRequestDigestComputationCount(c *gin.Context) int {
	state := promptRequestSecurityState(c)
	if state == nil {
		return 0
	}
	return int(state.digestComputations)
}
