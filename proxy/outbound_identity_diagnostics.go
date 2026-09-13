package proxy

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

type OutboundHeaderDiagnostic struct {
	Headers      map[string]string `json:"headers"`
	TurnMetadata map[string]string `json:"-"`
	CaptureStage string            `json:"capture_stage,omitempty"`
}

type outboundBodyDiagnostic struct {
	ClientMetadata map[string]string `json:"-"`
	TurnMetadata   map[string]string `json:"-"`
	Links          map[string]string `json:"-"`
	wire           map[string]any
}

type outboundIdentityDiagnostic struct {
	FormatVersion      int                             `json:"format_version,omitempty"`
	SessionConsistency string                          `json:"session_consistency,omitempty"`
	Truncated          bool                            `json:"truncated,omitempty"`
	HTTP               *OutboundHeaderDiagnostic       `json:"http,omitempty"`
	WSHandshake        *OutboundHeaderDiagnostic       `json:"ws_handshake,omitempty"`
	Body               *outboundBodyDiagnostic         `json:"body,omitempty"`
	AccountMapping     *codexAccountIdentityDiagnostic `json:"account_mapping,omitempty"`
}

func (diagnostic outboundBodyDiagnostic) MarshalJSON() ([]byte, error) {
	return json.Marshal(diagnostic.wire)
}

func (diagnostic *outboundBodyDiagnostic) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(&diagnostic.wire)
}

func (diagnostic *OutboundHeaderDiagnostic) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Headers          map[string]string `json:"headers"`
		DuplicateHeaders []string          `json:"duplicate_headers"`
		CaptureStage     string            `json:"capture_stage"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	diagnostic.Headers = decoded.Headers
	diagnostic.CaptureStage = decoded.CaptureStage
	if diagnostic.Headers == nil {
		diagnostic.Headers = make(map[string]string)
	}
	for _, name := range decoded.DuplicateHeaders {
		diagnostic.Headers[name+"_multiple"] = "true"
	}
	return nil
}

func (diagnostic OutboundHeaderDiagnostic) MarshalJSON() ([]byte, error) {
	headers := make(map[string]string, len(diagnostic.Headers))
	var duplicates []string
	for name, value := range diagnostic.Headers {
		if strings.HasSuffix(name, "_multiple") {
			duplicates = append(duplicates, strings.TrimSuffix(name, "_multiple"))
		} else {
			headers[name] = value
		}
	}
	sort.Strings(duplicates)
	return json.Marshal(struct {
		Headers          map[string]string `json:"headers"`
		DuplicateHeaders []string          `json:"duplicate_headers,omitempty"`
		CaptureStage     string            `json:"capture_stage,omitempty"`
	}{headers, duplicates, diagnostic.CaptureStage})
}

func outboundMetadataJSON(raw gjson.Result) map[string]any {
	values := make(map[string]any)
	if raw.IsObject() {
		if enabled := raw.Get("analytics_enabled"); enabled.Type == gjson.True || enabled.Type == gjson.False {
			values["analytics_enabled"] = enabled.Bool()
		}
	}
	if !raw.IsObject() || len(raw.Raw) > 16384 {
		return values
	}
	for field, value := range diagnosticMetadata(raw) {
		original := raw.Get(field)
		if !original.Exists() {
			continue
		}
		if original.Type == gjson.String {
			values[field] = strings.Clone(value)
		} else if !original.IsArray() && !original.IsObject() {
			values[field] = json.RawMessage(strings.Clone(original.Raw))
		} else {
			values[field] = "[invalid_type]"
		}
	}
	for _, field := range []string{"x-codex-turn-state", "x-openai-memgen-request"} {
		if value := raw.Get(field); value.Exists() {
			if field == "x-codex-turn-state" {
				values[field] = "hash:" + hashRiskIdentity(value.Raw)
			} else if value.Type == gjson.String {
				values[field] = diagnosticLabel(value.String())
			} else if value.Type == gjson.True || value.Type == gjson.False {
				values[field] = value.Bool()
			}
		}
	}
	return values
}

func CaptureOutboundIdentityHeaders(headers http.Header) *OutboundHeaderDiagnostic {
	diagnostic := &OutboundHeaderDiagnostic{Headers: make(map[string]string)}
	for _, name := range []string{
		"User-Agent", "Originator", "Version", "OpenAI-Beta", "X-Codex-Beta-Features",
		"X-Codex-Installation-Id", "X-Installation-Id", "X-Device-Id", "Oai-Device-Id",
		"Session-Id", "Session_id", "Thread-Id", "Conversation-Id", "Conversation_id",
		"X-Client-Request-Id", "X-Request-Id", "X-Codex-Window-Id", "X-Codex-Parent-Thread-Id",
		"X-Codex-Forked-From-Thread-Id", "X-OpenAI-Subagent", "X-OpenAI-Memgen-Request", "Chatgpt-Account-Id",
	} {
		values := headers.Values(name)
		if len(values) == 0 {
			continue
		}
		switch name {
		case "User-Agent", "Originator", "Version", "OpenAI-Beta", "X-Codex-Beta-Features":
			diagnostic.Headers[name] = diagnosticClientText(values[0])
		case "X-OpenAI-Subagent":
			diagnostic.Headers[name] = diagnosticLabel(values[0])
		default:
			diagnostic.Headers[name] = diagnosticIdentifier(values[0])
		}
		if len(values) > 1 {
			diagnostic.Headers[name+"_multiple"] = "true"
		}
	}
	if raw := headers.Get(codexTurnMetadataHeader); raw != "" {
		if len(raw) > 16384 {
			diagnostic.TurnMetadata = map[string]string{"metadata_status": "too_large"}
		} else {
			diagnostic.TurnMetadata = diagnosticMetadata(gjson.Parse(raw))
		}
		if gjson.Valid(raw) && gjson.Parse(raw).IsObject() && (len(raw) <= 16384 || hasCodexAnalyticsState(gjson.Parse(raw))) {
			encoded, _ := json.Marshal(outboundMetadataJSON(gjson.Parse(raw)))
			diagnostic.Headers[codexTurnMetadataHeader] = string(encoded)
		} else {
			diagnostic.Headers[codexTurnMetadataHeader] = "[invalid_or_too_large]"
		}
	}
	diagnostic.Headers = detachedDiagnosticValues(diagnostic.Headers)
	diagnostic.TurnMetadata = detachedDiagnosticValues(diagnostic.TurnMetadata)
	return diagnostic
}

func detachedDiagnosticValues(values map[string]string) map[string]string {
	for key, value := range values {
		values[key] = strings.Clone(value)
	}
	return values
}

func captureOutboundIdentityBody(body []byte) *outboundBodyDiagnostic {
	if !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	diagnostic := &outboundBodyDiagnostic{
		ClientMetadata: detachedDiagnosticValues(diagnosticMetadata(root.Get("client_metadata"))),
		TurnMetadata:   detachedDiagnosticValues(diagnosticMetadata(root.Get("client_metadata.x-codex-turn-metadata"))),
		wire:           make(map[string]any),
	}
	if metadata := root.Get("client_metadata"); metadata.IsObject() {
		wireMetadata := outboundMetadataJSON(metadata)
		if embedded := metadata.Get("x-codex-turn-metadata"); embedded.Exists() {
			if embedded.IsObject() {
				wireMetadata["x-codex-turn-metadata"] = outboundMetadataJSON(embedded)
			} else if embedded.Type == gjson.String && gjson.Valid(embedded.String()) && gjson.Parse(embedded.String()).IsObject() && (len(embedded.String()) <= 16384 || hasCodexAnalyticsState(gjson.Parse(embedded.String()))) {
				encoded, _ := json.Marshal(outboundMetadataJSON(gjson.Parse(embedded.String())))
				wireMetadata["x-codex-turn-metadata"] = string(encoded)
			} else if embedded.Type == gjson.Null {
				wireMetadata["x-codex-turn-metadata"] = nil
			} else {
				wireMetadata["x-codex-turn-metadata"] = "[invalid_or_too_large]"
			}
		}
		diagnostic.wire["client_metadata"] = wireMetadata
	}
	for _, field := range []string{"prompt_cache_key", "previous_response_id"} {
		if value := root.Get(field); value.Exists() {
			if diagnostic.Links == nil {
				diagnostic.Links = make(map[string]string)
			}
			if value.Type != gjson.String {
				diagnostic.Links[field] = "invalid_type"
			} else if strings.TrimSpace(value.String()) != "" {
				diagnostic.Links[field] = "hash:" + hashRiskIdentity(value.String())
			}
			diagnostic.wire[field] = diagnostic.Links[field]
		}
	}
	return diagnostic
}

func (observer *TransportObserver) updateOutboundIdentity(change func(*outboundIdentityDiagnostic)) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		identity := outboundIdentityDiagnostic{FormatVersion: 2}
		if diagnostic.OutboundIdentity != nil {
			identity = *diagnostic.OutboundIdentity
		}
		change(&identity)
		identity.SessionConsistency = outboundSessionConsistency(&identity)
		diagnostic.OutboundIdentity = &identity
	})
}

func outboundSessionConsistency(identity *outboundIdentityDiagnostic) string {
	headers := identity.HTTP
	if headers == nil {
		headers = identity.WSHandshake
	}
	if headers == nil || identity.Body == nil {
		return ""
	}
	values := make([]string, 0, 5)
	for _, name := range []string{codexSessionIDHeader, codexLegacySessionIDHeader, codexConversationIDHeader} {
		if headers.Headers[name+"_multiple"] == "true" {
			return "mismatched"
		}
		if value := headers.Headers[name]; value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return "missing_header"
	}
	if identity.Body.ClientMetadata["session_id"] == "" && identity.Body.TurnMetadata["session_id"] == "" {
		return "missing_body"
	}
	for _, metadata := range []map[string]string{headers.TurnMetadata, identity.Body.ClientMetadata, identity.Body.TurnMetadata} {
		if value := metadata["session_id"]; value != "" {
			values = append(values, value)
		}
	}
	for _, value := range values[1:] {
		if outboundComparableIdentity(value) != outboundComparableIdentity(values[0]) {
			return "mismatched"
		}
	}
	return "matched"
}

func (observer *TransportObserver) OutboundHTTPIdentity(headers http.Header) {
	if observer == nil {
		return
	}
	diagnostic := CaptureOutboundIdentityHeaders(headers)
	observer.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.HTTP = diagnostic
	})
}

func (observer *TransportObserver) OutboundWebsocketHandshake(diagnostic *OutboundHeaderDiagnostic) {
	if observer == nil {
		return
	}
	var copied *OutboundHeaderDiagnostic
	if diagnostic != nil {
		copied = &OutboundHeaderDiagnostic{Headers: maps.Clone(diagnostic.Headers), TurnMetadata: maps.Clone(diagnostic.TurnMetadata), CaptureStage: diagnostic.CaptureStage}
	}
	observer.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.WSHandshake = copied
	})
}
