package proxy

import "strings"

// shouldUseSessionScopedWebsocket only allows upstream WebSocket reuse when the
// downstream request carries an explicit, stable session identifier. Sessionless
// traffic stays on HTTP to avoid reusing a connection whose handshake-level
// Session_id/Conversation_id may belong to a different logical conversation.
func shouldUseSessionScopedWebsocket(candidate bool, explicitSessionID string) bool {
	return candidate && strings.TrimSpace(explicitSessionID) != ""
}
