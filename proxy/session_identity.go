package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	newAPIPolicyRootSessionResolved    = "resolved"
	newAPIPolicyRootSessionConflict    = "conflict"
	newAPIPolicyRootSessionUnavailable = "unavailable"
)

// requestRootSessionIdentity is deliberately separate from
// requestSessionIdentity so root aggregation cannot silently change existing
// account routing, affinity, or upstream execution-slot semantics. This
// identity is only for user-window accounting and operational session
// aggregation, where all leaves belonging to one Codex task must share the
// root Session-Id.
type requestRootSessionIdentity struct {
	sessionID   string
	fingerprint string
	stable      bool
	conflict    bool
	// nativeRoot is true only when a structured Codex graph proved the root.
	// Weak legacy IDs remain stable for standalone enforcement but must not
	// override an older NewAPI sender's signed leaf fingerprint.
	nativeRoot bool
	// authoritative means a signed, root-session-capable NewAPI sender made
	// the resolution decision. Callers must not reinterpret raw client fields
	// when that sender deliberately omitted a root identity.
	authoritative bool
}

type codexSessionGraphSignals struct {
	headerSessions []string
	metadataRoots  []string
	threads        []string
	clientRequests []string
	windows        []string
	parents        []string
	subagent       bool
	structured     bool
	malformed      bool
}

const requestRootSessionBindingContextKey = "request_root_session_binding_v1"

type requestRootSessionBinding struct {
	fingerprint string
}

// resolveRequestRootSessionIdentity resolves a root session without changing
// the existing routing/affinity identity. Codex currently carries the same
// graph through several transports, so all present carriers are compared. A
// disagreement fails closed: it must never merge two unrelated conversations
// merely because one untrusted field happened to contain a plausible root.
func resolveRequestRootSessionIdentity(headers http.Header, body []byte) requestRootSessionIdentity {
	signals := collectCodexSessionGraphSignals(headers, body)
	headerSession, headerSessionConflict := oneSessionGraphValue(signals.headerSessions)
	metadataRoot, rootConflict := oneSessionGraphValue(signals.metadataRoots)
	thread, threadConflict := oneSessionGraphValue(signals.threads)
	clientRequest, clientConflict := oneSessionGraphValue(signals.clientRequests)
	parent, parentConflict := oneSessionGraphValue(signals.parents)
	windowThread, windowConflict := resolveSessionGraphWindow(signals.windows)

	conflict := signals.malformed || headerSessionConflict || rootConflict || threadConflict || clientConflict || parentConflict || windowConflict
	leaf := ""
	for _, candidate := range []string{thread, clientRequest, windowThread} {
		if candidate == "" {
			continue
		}
		if leaf == "" {
			leaf = candidate
			continue
		}
		if leaf != candidate {
			conflict = true
		}
	}
	if conflict {
		return requestRootSessionIdentity{conflict: true}
	}

	if metadataRoot != "" {
		// Some Guardian transports put the exact leaf in Session-Id while turn
		// metadata.session_id carries the user-visible root. Accept that shape
		// only when the parent corroborates the root and the complete leaf graph
		// corroborates the header session. Other cross-carrier disagreements are
		// conflicts rather than a reason to merge.
		if leaf == "" || !validSessionGraphUUID(metadataRoot) || !validSessionGraphUUID(leaf) || (parent != "" && !validSessionGraphUUID(parent)) {
			return requestRootSessionIdentity{conflict: true}
		}
		// parent_thread_id is the immediate parent, not necessarily the root.
		// For main -> subagent -> Guardian, Session-Id/metadata.session_id still
		// carry the main task while parent points at the intermediate subagent.
		// Its presence corroborates a child lineage; equality with the root must
		// not be required.
		if leaf != metadataRoot && parent == "" && !signals.subagent {
			return requestRootSessionIdentity{conflict: true}
		}
		if headerSession != "" && headerSession != metadataRoot {
			if (parent == "" && !signals.subagent) || headerSession != leaf || !validSessionGraphUUID(headerSession) {
				return requestRootSessionIdentity{conflict: true}
			}
		}
		return requestRootSessionIdentity{sessionID: strings.ToLower(metadataRoot), stable: true, nativeRoot: true}
	}

	if headerSession != "" {
		// A lone Session-Id remains compatible with ordinary SDKs and older
		// Codex clients. Once graph fields are present, however, require the
		// native UUID shape before collapsing a child leaf into its root.
		nativeRoot := false
		if signals.structured {
			if !validSessionGraphUUID(headerSession) || (leaf != "" && !validSessionGraphUUID(leaf)) || (parent != "" && !validSessionGraphUUID(parent)) {
				return requestRootSessionIdentity{conflict: true}
			}
			if leaf != "" {
				if headerSession == leaf {
					nativeRoot = true
				} else if parent != "" || signals.subagent {
					nativeRoot = true
				} else {
					return requestRootSessionIdentity{conflict: true}
				}
			}
			// parent is allowed to be an intermediate subagent. The coherent
			// Session-Id + leaf graph identifies the root independently.
			headerSession = strings.ToLower(headerSession)
		}
		return requestRootSessionIdentity{sessionID: headerSession, stable: true, nativeRoot: nativeRoot}
	}

	// A graph without Session-Id cannot prove the main task root. Do not infer
	// it from parent_thread_id (which may be only an immediate parent). A lone
	// leaf is still a stable exact-session fallback; a parent-bearing child is
	// deliberately left unenforced rather than incorrectly merged.
	if leaf != "" {
		if parent != "" {
			return requestRootSessionIdentity{}
		}
		if signals.structured {
			if !validSessionGraphUUID(leaf) {
				return requestRootSessionIdentity{conflict: true}
			}
			leaf = strings.ToLower(leaf)
		}
		return requestRootSessionIdentity{sessionID: leaf, stable: true}
	}

	// Preserve the existing non-Codex compatibility order for clients that do
	// not send a native session graph.
	if headers != nil {
		if conversation, bad := oneHeaderValue(headers, "Conversation-Id", "Conversation_id"); bad {
			return requestRootSessionIdentity{conflict: true}
		} else if conversation != "" {
			return requestRootSessionIdentity{sessionID: conversation, stable: true}
		}
		for _, name := range []string{"X-Session-ID", "OpenAI-Session-ID"} {
			if explicitSession, bad := oneHeaderValue(headers, name); bad {
				return requestRootSessionIdentity{conflict: true}
			} else if explicitSession != "" {
				return requestRootSessionIdentity{sessionID: explicitSession, stable: true}
			}
		}
	}
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); promptCacheKey != "" {
		return requestRootSessionIdentity{sessionID: promptCacheKey, stable: true}
	}
	return requestRootSessionIdentity{}
}

// resolveRequestRootSessionIdentityForContext upgrades the locally resolved
// root with authenticated NewAPI metadata. Older NewAPI deployments retain a
// leaf fallback for ordinary HTTP traffic; Responses WS instead binds the
// first coherent native root to the connection so later frames cannot drift
// back to a Guardian leaf.
func (h *Handler) resolveRequestRootSessionIdentityForContext(c *gin.Context, body []byte) requestRootSessionIdentity {
	if c == nil {
		return requestRootSessionIdentity{}
	}
	webSocketFrame := isResponsesWebSocketUpgradeRequest(c.Request) && isRootSessionWebSocketFrame(body)
	identity := resolveRequestRootSessionIdentity(c.Request.Header, body)
	frameIdentity := identity
	if webSocketFrame {
		// Upgrade headers belong to the connection, while client_metadata belongs
		// to one response.create frame. Resolve the latter independently so an old
		// handshake leaf cannot create a false cross-carrier conflict.
		frameIdentity = resolveRequestRootSessionIdentity(nil, body)
	}
	status, policyContext := h.cachedNewAPIPolicyAuditState(c)
	if (status != "verified" && status != "signed_response") || !policyContext.MetaVerified {
		return identity
	}

	if policyContext.Meta.RootSessionVersion >= 1 {
		switch policyContext.Meta.RootSessionState {
		case newAPIPolicyRootSessionResolved:
			fingerprint := strings.TrimSpace(policyContext.Meta.RootSessionFingerprint)
			if frameIdentity.conflict {
				return requestRootSessionIdentity{conflict: true, authoritative: true}
			}
			if frameIdentity.nativeRoot && frameIdentity.stable && frameIdentity.sessionID != "" {
				frameFingerprint := newAPIRootSessionFingerprint(policyContext.Platform, policyContext.Identity.UserID, frameIdentity.sessionID)
				if !equalRootSessionFingerprint(fingerprint, frameFingerprint) {
					return requestRootSessionIdentity{conflict: true, authoritative: true}
				}
			}
			c.Set(requestRootSessionBindingContextKey, requestRootSessionBinding{fingerprint: fingerprint})
			return requestRootSessionIdentity{
				sessionID:     fingerprint,
				fingerprint:   fingerprint,
				stable:        fingerprint != "",
				authoritative: true,
			}
		case newAPIPolicyRootSessionConflict:
			// A root-capable signed gateway explicitly found contradictory graph
			// carriers. Never turn its leaf fingerprint into an operational root;
			// callers enforcing a window limit surface a dedicated identity error.
			return requestRootSessionIdentity{conflict: true, authoritative: true}
		case newAPIPolicyRootSessionUnavailable:
			// A WebSocket handshake can legitimately lack response.create metadata.
			// Only an explicit unavailable state may be completed from a structured
			// native graph in the current frame; a signed conflict is final.
			if bound, ok := requestRootSessionBindingFromContext(c); ok {
				if frameIdentity.conflict {
					return requestRootSessionIdentity{conflict: true, authoritative: true}
				}
				if frameIdentity.nativeRoot && frameIdentity.stable && frameIdentity.sessionID != "" {
					frameFingerprint := newAPIRootSessionFingerprint(policyContext.Platform, policyContext.Identity.UserID, frameIdentity.sessionID)
					if !equalRootSessionFingerprint(bound.fingerprint, frameFingerprint) {
						return requestRootSessionIdentity{conflict: true, authoritative: true}
					}
				}
				return requestRootSessionIdentity{sessionID: bound.fingerprint, fingerprint: bound.fingerprint, stable: true, authoritative: true}
			}
			if webSocketFrame {
				if frameIdentity.conflict {
					return requestRootSessionIdentity{conflict: true, authoritative: true}
				}
				if frameIdentity.nativeRoot && frameIdentity.stable && frameIdentity.sessionID != "" {
					fingerprint := newAPIRootSessionFingerprint(policyContext.Platform, policyContext.Identity.UserID, frameIdentity.sessionID)
					if fingerprint != "" {
						c.Set(requestRootSessionBindingContextKey, requestRootSessionBinding{fingerprint: fingerprint})
						return requestRootSessionIdentity{
							sessionID:     fingerprint,
							fingerprint:   fingerprint,
							stable:        true,
							authoritative: true,
						}
					}
				}
				// Do not count a connection-scoped signed leaf before a frame proves
				// its root. Otherwise a metadata-free first frame followed by a normal
				// Codex frame would consume both the leaf and the root window.
			}
			return requestRootSessionIdentity{authoritative: true}
		}
		// Version validation rejects unknown states before this point. Keep a
		// defensive empty authoritative result rather than reinterpreting a leaf.
		return requestRootSessionIdentity{authoritative: true}
	}
	// During a rolling NewAPI upgrade the signed metadata may still contain
	// only the exact leaf fingerprint. Prefer a locally verified native root so
	// Guardian requests do not temporarily consume extra user windows.
	if webSocketFrame {
		if bound, ok := requestRootSessionBindingFromContext(c); ok {
			if frameIdentity.conflict {
				return requestRootSessionIdentity{conflict: true, authoritative: true}
			}
			// Once a frame-local root has won, upgrade headers are historical
			// connection metadata. Validate only a new explicit frame graph; a
			// metadata-free continuation simply reuses the binding.
			if frameIdentity.nativeRoot && frameIdentity.stable && frameIdentity.sessionID != "" {
				candidateFingerprint := newAPIRootSessionFingerprint(policyContext.Platform, policyContext.Identity.UserID, frameIdentity.sessionID)
				if !equalRootSessionFingerprint(bound.fingerprint, candidateFingerprint) {
					return requestRootSessionIdentity{conflict: true, authoritative: true}
				}
			}
			return requestRootSessionIdentity{sessionID: bound.fingerprint, fingerprint: bound.fingerprint, stable: true, authoritative: true}
		}
		if frameIdentity.conflict {
			return requestRootSessionIdentity{conflict: true, authoritative: true}
		}
		// A v0 upgrade did not sign a root. Bind only a complete frame-local graph;
		// even coherent-looking upgrade headers may describe a stale Guardian leaf.
		if frameIdentity.nativeRoot && frameIdentity.stable && frameIdentity.sessionID != "" {
			fingerprint := newAPIRootSessionFingerprint(policyContext.Platform, policyContext.Identity.UserID, frameIdentity.sessionID)
			if fingerprint != "" {
				c.Set(requestRootSessionBindingContextKey, requestRootSessionBinding{fingerprint: fingerprint})
				return requestRootSessionIdentity{sessionID: fingerprint, fingerprint: fingerprint, stable: true, authoritative: true}
			}
		}
		// Do not count the v0 handshake leaf while waiting for a frame that can
		// prove the user-visible root; otherwise the same connection can consume
		// one leaf window and one root window.
		return requestRootSessionIdentity{authoritative: true}
	}
	if identity.nativeRoot && identity.stable && !identity.conflict && identity.sessionID != "" {
		fingerprint := newAPIRootSessionFingerprint(policyContext.Platform, policyContext.Identity.UserID, identity.sessionID)
		if fingerprint != "" {
			return requestRootSessionIdentity{
				sessionID:   fingerprint,
				fingerprint: fingerprint,
				stable:      true,
			}
		}
	}
	if fingerprint := strings.TrimSpace(policyContext.Meta.SessionFingerprint); fingerprint != "" {
		return requestRootSessionIdentity{
			sessionID:   fingerprint,
			fingerprint: fingerprint,
			stable:      true,
		}
	}
	return identity
}

func isRootSessionWebSocketFrame(body []byte) bool {
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "type").String()), "response.create")
}

func requestRootSessionBindingFromContext(c *gin.Context) (requestRootSessionBinding, bool) {
	if c == nil {
		return requestRootSessionBinding{}, false
	}
	raw, exists := c.Get(requestRootSessionBindingContextKey)
	if !exists {
		return requestRootSessionBinding{}, false
	}
	binding, ok := raw.(requestRootSessionBinding)
	binding.fingerprint = strings.TrimSpace(binding.fingerprint)
	return binding, ok && binding.fingerprint != ""
}

func equalRootSessionFingerprint(left, right string) bool {
	left = strings.ToLower(strings.TrimSpace(left))
	right = strings.ToLower(strings.TrimSpace(right))
	if len(left) != len(right) || left == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func collectCodexSessionGraphSignals(headers http.Header, body []byte) codexSessionGraphSignals {
	var signals codexSessionGraphSignals
	if headers != nil {
		appendHeaderSessionGraphValues(&signals.headerSessions, headers, "Session-Id", "Session_id")
		appendHeaderSessionGraphValues(&signals.threads, headers, "Thread-Id")
		appendHeaderSessionGraphValues(&signals.clientRequests, headers, "X-Client-Request-Id")
		appendHeaderSessionGraphValues(&signals.windows, headers, "X-Codex-Window-Id")
		appendHeaderSessionGraphValues(&signals.parents, headers, "X-Codex-Parent-Thread-Id")
		appendSessionGraphSubagentHeader(&signals, headers, "X-OpenAI-Subagent")
		if len(signals.threads)+len(signals.clientRequests)+len(signals.windows)+len(signals.parents) > 0 {
			signals.structured = true
		}
		for _, raw := range headers.Values(codexTurnMetadataHeader) {
			raw = strings.TrimSpace(raw)
			if raw == "" || strings.EqualFold(raw, "null") {
				continue
			}
			signals.structured = true
			collectEncodedCodexSessionGraphJSON(&signals, gjson.Parse(raw), gjson.Valid(raw))
		}
	}

	if len(body) == 0 || !gjson.ValidBytes(body) {
		return signals
	}
	collectCodexClientMetadata(&signals, gjson.GetBytes(body, "client_metadata"))
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "type").String()), "response.create") {
		// Realtime-style response.create envelopes may place request metadata
		// under response, while native Responses WS v2 keeps it at the top level.
		collectCodexClientMetadata(&signals, gjson.GetBytes(body, "response.client_metadata"))
	}
	return signals
}

func collectCodexClientMetadata(signals *codexSessionGraphSignals, metadata gjson.Result) {
	collectCodexClientMetadataAtDepth(signals, metadata, 0)
}

func collectCodexClientMetadataAtDepth(signals *codexSessionGraphSignals, metadata gjson.Result, depth int) {
	if signals == nil || !metadata.Exists() {
		return
	}
	if depth > 3 {
		signals.malformed = true
		return
	}
	if metadata.Type == gjson.Null && strings.EqualFold(strings.TrimSpace(metadata.Raw), "null") {
		return
	}
	for depth := 0; depth < 4 && !metadata.IsObject(); depth++ {
		if metadata.Type != gjson.String {
			signals.malformed = true
			return
		}
		raw := strings.TrimSpace(metadata.String())
		if raw == "" || !gjson.Valid(raw) {
			signals.malformed = true
			return
		}
		metadata = gjson.Parse(raw)
	}
	if !metadata.IsObject() {
		signals.malformed = true
		return
	}
	before := len(signals.metadataRoots) + len(signals.threads) + len(signals.clientRequests) + len(signals.windows) + len(signals.parents)
	appendSessionGraphJSONField(signals, &signals.metadataRoots, metadata, "session_id")
	appendSessionGraphJSONField(signals, &signals.threads, metadata, "thread_id")
	appendSessionGraphJSONField(signals, &signals.clientRequests, metadata, "client_request_id", "x-client-request-id", "x_client_request_id")
	appendSessionGraphJSONField(signals, &signals.windows, metadata, "window_id", "x-codex-window-id", "x_codex_window_id")
	appendSessionGraphJSONField(signals, &signals.parents, metadata, "parent_thread_id", "x-codex-parent-thread-id", "x_codex_parent_thread_id")
	appendSessionGraphJSONSubagent(signals, metadata, "subagent_kind", "x-openai-subagent", "x_openai_subagent")

	for _, key := range []string{"x-codex-turn-metadata", "x_codex_turn_metadata"} {
		embedded := metadata.Get(key)
		if embedded.Exists() {
			switch embedded.Type {
			case gjson.String:
				signals.structured = true
				collectEncodedCodexSessionGraphJSON(signals, embedded, true)
			case gjson.JSON:
				signals.structured = true
				collectEncodedCodexSessionGraphJSON(signals, embedded, embedded.IsObject())
			case gjson.Null:
				continue
			default:
				signals.structured = true
				signals.malformed = true
			}
		}
	}

	nested := metadata.Get("client_metadata")
	if nested.Exists() && nested.Type != gjson.Null {
		signals.structured = true
		collectCodexClientMetadataAtDepth(signals, nested, depth+1)
	}
	after := len(signals.metadataRoots) + len(signals.threads) + len(signals.clientRequests) + len(signals.windows) + len(signals.parents)
	if after > before {
		signals.structured = true
	}
}

func collectEncodedCodexSessionGraphJSON(signals *codexSessionGraphSignals, metadata gjson.Result, valid bool) {
	if signals == nil || !valid {
		if signals != nil {
			signals.malformed = true
		}
		return
	}
	for depth := 0; depth < 4 && !metadata.IsObject(); depth++ {
		if metadata.Type != gjson.String {
			signals.malformed = true
			return
		}
		raw := strings.TrimSpace(metadata.String())
		if raw == "" || !gjson.Valid(raw) {
			signals.malformed = true
			return
		}
		metadata = gjson.Parse(raw)
	}
	collectCodexSessionGraphJSON(signals, metadata, metadata.IsObject())
}

func collectCodexSessionGraphJSON(signals *codexSessionGraphSignals, metadata gjson.Result, valid bool) {
	if signals == nil {
		return
	}
	if !valid || !metadata.IsObject() {
		signals.malformed = true
		return
	}
	appendSessionGraphJSONField(signals, &signals.metadataRoots, metadata, "session_id")
	appendSessionGraphJSONField(signals, &signals.threads, metadata, "thread_id")
	appendSessionGraphJSONField(signals, &signals.clientRequests, metadata, "client_request_id", "x-client-request-id")
	appendSessionGraphJSONField(signals, &signals.clientRequests, metadata, "x_client_request_id")
	appendSessionGraphJSONField(signals, &signals.windows, metadata, "window_id", "x-codex-window-id", "x_codex_window_id")
	appendSessionGraphJSONField(signals, &signals.parents, metadata, "parent_thread_id", "x-codex-parent-thread-id", "x_codex_parent_thread_id")
	appendSessionGraphJSONSubagent(signals, metadata, "subagent_kind", "x-openai-subagent", "x_openai_subagent")
}

func appendSessionGraphSubagentHeader(signals *codexSessionGraphSignals, headers http.Header, names ...string) {
	if signals == nil || headers == nil {
		return
	}
	for _, name := range names {
		for _, value := range headers.Values(name) {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if len(value) > 64 || strings.ContainsAny(value, "\r\n\x00") {
				signals.malformed = true
				continue
			}
			signals.subagent = true
		}
	}
}

func appendSessionGraphJSONSubagent(signals *codexSessionGraphSignals, object gjson.Result, paths ...string) {
	for _, path := range paths {
		value := object.Get(path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.Null {
			continue
		}
		if value.Type != gjson.String {
			signals.malformed = true
			continue
		}
		text := strings.TrimSpace(value.String())
		if text == "" {
			continue
		}
		if len(text) > 64 || strings.ContainsAny(text, "\r\n\x00") {
			signals.malformed = true
			continue
		}
		signals.subagent = true
	}
}

func appendSessionGraphJSONField(signals *codexSessionGraphSignals, target *[]string, object gjson.Result, paths ...string) {
	for _, path := range paths {
		value := object.Get(path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.Null {
			continue
		}
		if value.Type != gjson.String {
			signals.malformed = true
			continue
		}
		if trimmed := strings.TrimSpace(value.String()); trimmed != "" {
			*target = append(*target, trimmed)
		}
	}
}

func appendHeaderSessionGraphValues(target *[]string, headers http.Header, names ...string) {
	for _, name := range names {
		for _, value := range headers.Values(name) {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				*target = append(*target, trimmed)
			}
		}
	}
}

func oneHeaderValue(headers http.Header, names ...string) (string, bool) {
	values := make([]string, 0, len(names))
	appendHeaderSessionGraphValues(&values, headers, names...)
	return oneSessionGraphValue(values)
}

func oneSessionGraphValue(values []string) (string, bool) {
	if len(values) == 0 {
		return "", false
	}
	value := normalizeSessionGraphValue(values[0])
	for _, candidate := range values[1:] {
		if value != normalizeSessionGraphValue(candidate) {
			return "", true
		}
	}
	return value, false
}

func normalizeSessionGraphValue(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := uuid.Parse(value); err == nil {
		return strings.ToLower(parsed.String())
	}
	return value
}

func resolveSessionGraphWindow(values []string) (string, bool) {
	threads := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		separator := strings.LastIndexByte(value, ':')
		if separator <= 0 || separator == len(value)-1 {
			return "", true
		}
		thread := strings.TrimSpace(value[:separator])
		sequence := strings.TrimSpace(value[separator+1:])
		if thread == "" {
			return "", true
		}
		if _, err := strconv.ParseUint(sequence, 10, 64); err != nil {
			return "", true
		}
		threads = append(threads, thread)
	}
	return oneSessionGraphValue(threads)
}

func validSessionGraphUUID(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}

// newAPIRootSessionFingerprint mirrors NewAPI's signed policy-meta canonical
// exactly. It lets a rolling deployment (old NewAPI leaf-only metadata, new
// Codex2API resolver) produce the same key as a fully upgraded gateway.
func newAPIRootSessionFingerprint(platform, userID, rootSessionID string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	userID = strings.TrimSpace(userID)
	rootSessionID = strings.TrimSpace(rootSessionID)
	if parsed, err := uuid.Parse(rootSessionID); err == nil {
		rootSessionID = strings.ToLower(parsed.String())
	}
	if platform == "" || userID == "" || rootSessionID == "" || len(rootSessionID) > 1024 || strings.ContainsAny(rootSessionID, "\r\n\x00") {
		return ""
	}
	canonical := strings.Join([]string{"policy-root-session-v1", platform, userID, rootSessionID}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:16])
}
