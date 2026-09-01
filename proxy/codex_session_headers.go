package proxy

import (
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
)

// Codex session identity headers use the same shape as the native client:
// Session-Id identifies the session tree, while Thread-Id and
// X-Client-Request-Id identify one concrete parent/subagent thread.
const (
	codexSessionHeaderModeNative = "native"
	codexSessionHeaderModeLegacy = "legacy"

	codexSessionIDHeader       = "Session-Id"
	codexLegacySessionIDHeader = "Session_id"
	codexConversationIDHeader  = "Conversation_id"
)

func codexSessionHeaderModeFromEnv() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_SESSION_HEADER_MODE")), codexSessionHeaderModeLegacy) {
		return codexSessionHeaderModeLegacy
	}
	return codexSessionHeaderModeNative
}

// codexSessionHeaderAlignsConverged controls whether the upstream Session-Id
// also uses the account-converged session identity. It defaults off so the
// existing prompt-cache/session isolation key remains authoritative.
func codexSessionHeaderAlignsConverged() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// ConvergedCodexSessionIdentity returns the account fingerprint policy's
// outbound session/thread identity. off mode (and device mode for session
// fields) returns empty values so callers can preserve the raw client thread.
func ConvergedCodexSessionIdentity(account *auth.Account, downstreamHeaders http.Header) (sessionID, threadID string) {
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return "", ""
	}
	return ids.sessionID, ids.threadID
}

// ApplyCodexSessionHeaders writes the native Codex session header set.
// fallbackSessionID remains the upstream session/cache key. Thread identity is
// taken from the converged fingerprint when configured, otherwise from the raw
// downstream Thread-Id (with turn metadata as a fallback).
//
// This must run after ApplyCodexFingerprintHeaders and before account custom
// headers so an explicit operator override retains final precedence.
func ApplyCodexSessionHeaders(outbound http.Header, account *auth.Account, fallbackSessionID string, downstreamHeaders http.Header, legacyConversationID bool) {
	if outbound == nil {
		return
	}
	fallbackSessionID = strings.TrimSpace(fallbackSessionID)
	if fallbackSessionID == "" {
		return
	}

	if codexSessionHeaderModeFromEnv() == codexSessionHeaderModeLegacy {
		outbound.Set(codexLegacySessionIDHeader, fallbackSessionID)
		if legacyConversationID {
			outbound.Set(codexConversationIDHeader, fallbackSessionID)
		} else {
			outbound.Del(codexConversationIDHeader)
		}
		return
	}

	convergedSessionID, threadID := ConvergedCodexSessionIdentity(account, downstreamHeaders)
	if threadID == "" {
		// off/device modes intentionally do not converge thread identity. Keep
		// the client's real parent/subagent thread instead of flattening the
		// whole tree into a single-thread shape.
		_, threadID = extractClientCodexIdentity(downstreamHeaders)
	}

	sessionID := fallbackSessionID
	if convergedSessionID != "" && codexSessionHeaderAlignsConverged() {
		sessionID = convergedSessionID
	}
	if threadID == "" {
		threadID = sessionID
	}

	outbound.Set(codexSessionIDHeader, sessionID)
	outbound.Set(codexThreadIDHeader, threadID)
	if strings.TrimSpace(outbound.Get(codexClientRequestIDHeader)) == "" {
		outbound.Set(codexClientRequestIDHeader, threadID)
	}
	outbound.Del(codexLegacySessionIDHeader)
	outbound.Del(codexConversationIDHeader)
}
