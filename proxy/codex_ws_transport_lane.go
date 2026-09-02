package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// ResolveCodexWebsocketTransportSessionKey returns the local-only connection
// pool lane for one explicit Codex session. A Codex session tree shares
// session-id while child agents have independent thread-id values. The shared
// upstream session remains responsible for account affinity and prompt cache;
// only a genuine child/alternate thread gets a separate transport lane.
//
// The lane is derived from raw downstream identity before any account
// fingerprint convergence. Fingerprint policy controls what the upstream sees,
// not whether independent local streams must serialize on one connection.
func ResolveCodexWebsocketTransportSessionKey(upstreamSessionID string, downstreamHeaders http.Header) string {
	upstreamSessionID = strings.TrimSpace(upstreamSessionID)
	if upstreamSessionID == "" || IsStatelessWebsocketSessionID(upstreamSessionID) {
		return upstreamSessionID
	}
	clientSessionID, clientThreadID := extractClientCodexIdentity(downstreamHeaders)
	if clientThreadID == "" || (clientSessionID != "" && clientThreadID == clientSessionID) {
		return upstreamSessionID
	}
	sum := sha256.Sum256([]byte("codex2api:ws-transport-lane:v1\x00" + upstreamSessionID + "\x00" + clientSessionID + "\x00" + clientThreadID))
	return "ws-thread-" + hex.EncodeToString(sum[:16])
}
