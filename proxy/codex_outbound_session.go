package proxy

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func codexOutboundSessionMode() string {
	if CurrentRuntimeSettings().CodexSessionFailoverEnabled {
		return "account"
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_OUTBOUND_SESSION_MODE"))) {
	case "legacy", "off":
		return "legacy"
	case "observe":
		return "observe"
	case "account":
		return "account"
	default:
		return "preserve"
	}
}

func NewCodexTransportFingerprint(account *auth.Account, headers http.Header, body []byte, upstreamSessionID string) *CodexFingerprint {
	fingerprint := NewCodexFingerprint(account, headers, body)
	mode := codexOutboundSessionMode()
	if account == nil || account.IsRelayStyle() || (mode != "preserve" && mode != "account") {
		return fingerprint
	}
	fingerprint.preserveSessionIDs = true
	fingerprint.identityValues = codexTransportIdentityValues(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	fingerprint.accountIdentityRequested = mode == "account"
	fingerprint.accountIdentityInputs = codexAccountIdentityInputs(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	fingerprint.accountTurnIdentityInputs = codexAccountTurnIdentityInputs(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	fingerprint.accountIdentityReferences = codexAccountIdentityReferences(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	fingerprint.accountWindowInputs, fingerprint.accountWindowInputError = codexAccountWindowIdentities(fingerprint.headers, NormalizeCodexRequestMetadata(body))
	if fingerprint.ids != nil {
		fingerprint.ids.mode = auth.CodexFingerprintModeDevice
		fingerprint.ids.sessionID = ""
		fingerprint.ids.threadID = ""
		fingerprint.ids.windowID = ""
		fingerprint.ids.clientRequestID = ""
		fingerprint.ids.lineageValues = nil
	}
	return fingerprint
}

func (fingerprint *CodexFingerprint) PreservesSessionIdentity() bool {
	return fingerprint != nil && fingerprint.preserveSessionIDs
}

func (fingerprint *CodexFingerprint) ApplySessionHeaders(outbound http.Header) {
	if !fingerprint.PreservesSessionIdentity() || outbound == nil {
		return
	}
	for _, name := range []string{codexSessionIDHeader, codexLegacySessionIDHeader, codexConversationIDHeader, "Conversation-Id"} {
		outbound.Del(name)
	}
	sessionID := fingerprint.headers.Get(codexSessionIDHeader)
	if sessionID == "" {
		sessionID = fingerprint.headers.Get(codexLegacySessionIDHeader)
	}
	if sessionID != "" {
		outbound.Set(codexSessionIDHeader, sessionID)
	}
	for _, name := range []string{codexThreadIDHeader, codexClientRequestIDHeader, codexWindowIDHeader, codexParentThreadIDHeader, "X-Codex-Forked-From-Thread-Id", "X-OpenAI-Subagent", "X-OpenAI-Memgen-Request", codexTurnMetadataHeader} {
		outbound.Del(name)
		if value := fingerprint.headers.Get(name); value != "" {
			outbound.Set(name, value)
		}
	}
	if outbound.Get(codexClientRequestIDHeader) == "" && outbound.Get(codexThreadIDHeader) != "" {
		outbound.Set(codexClientRequestIDHeader, outbound.Get(codexThreadIDHeader))
	}
	applyCodexFingerprintHeaders(outbound, fingerprint.ids, fingerprint.headers)
	if fingerprint.accountIdentity != nil {
		outbound.Set("Chatgpt-Account-Id", fingerprint.accountIdentity.account)
	}
}

func ScopeCodexPromptCacheKey(ctx context.Context, cacheKey string) string {
	if owner := verifiedTransportUser(ctx); owner != "" && strings.TrimSpace(cacheKey) != "" {
		return DeriveStableSessionUUIDv7("codex-cache-user-v2:" + owner + ":" + cacheKey)
	}
	return cacheKey
}

func codexTransportIdentityValues(headers http.Header, body []byte) []string {
	values := []string{headers.Get(codexSessionIDHeader), headers.Get(codexLegacySessionIDHeader), headers.Get(codexThreadIDHeader), headers.Get(codexParentThreadIDHeader), headers.Get("X-Codex-Forked-From-Thread-Id")}
	metadata := gjson.Parse(headers.Get(codexTurnMetadataHeader))
	for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id"} {
		if value := metadata.Get(field); value.Type == gjson.String {
			values = append(values, value.String())
		}
	}
	for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id", "x-codex-parent-thread-id", "x_codex_parent_thread_id", "x-codex-forked-from-thread-id", "x_codex_forked_from_thread_id"} {
		if value := gjson.GetBytes(body, "client_metadata."+field); value.Type == gjson.String {
			values = append(values, value.String())
		}
	}
	return values
}
