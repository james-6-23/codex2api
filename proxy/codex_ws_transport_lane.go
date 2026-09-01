package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// ResolveCodexWebsocketTransportSessionKey returns the local-only connection
// pool lane for one Codex thread. A session tree shares Session-Id while parent
// and child agents have independent Thread-Id values. Only the local transport
// lane is split; upstream session identity, account affinity, and prompt cache
// remain shared.
//
// The lane is derived from raw downstream identity before fingerprint
// convergence. Fingerprint policy controls what the upstream sees, not whether
// independent local streams must serialize on one connection.
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
