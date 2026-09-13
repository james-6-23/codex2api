package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type CodexFingerprint struct {
	ids                       *codexFingerprintIDs
	headers                   http.Header
	preserveSessionIDs        bool
	identityValues            []string
	accountIdentityRequested  bool
	accountIdentityInputs     []string
	accountTurnIdentityInputs map[string]codexTurnIdentityInput
	accountIdentityReferences map[string]bool
	accountWindowInputs       map[string]database.SessionOutboundWindowInput
	accountWindowInputError   error
	accountIdentity           *codexAccountIdentity
	accountIdentityDiagnostic *codexAccountIdentityDiagnostic
}

func NewCodexFingerprint(account *auth.Account, headers http.Header, body []byte) *CodexFingerprint {
	body = NormalizeCodexRequestMetadata(body)
	headers = CodexRequestMetadataHeaders(headers, body)
	fingerprint := &CodexFingerprint{ids: resolveCodexFingerprintIDs(account, headers), headers: headers}
	if fingerprint.ids != nil {
		for _, field := range codexLineageMetadataPaths {
			original := gjson.GetBytes(body, "client_metadata."+field).String()
			if original == "" && field == "parent_thread_id" {
				original = headers.Get(codexParentThreadIDHeader)
			}
			if original != "" && fingerprint.ids.lineageValues[field] == "" {
				fingerprint.ids.lineageValues[field] = fingerprint.ids.convergeLineageValue(field, original)
			}
		}
	}
	return fingerprint
}

func (fingerprint *CodexFingerprint) DownstreamHeaders() http.Header {
	return fingerprint.headers.Clone()
}

func (fingerprint *CodexFingerprint) ApplyHeaders(outbound http.Header) {
	applyCodexFingerprintHeaders(outbound, fingerprint.ids, fingerprint.headers)
	fingerprint.ApplySessionHeaders(outbound)
}

func (fingerprint *CodexFingerprint) ApplyBody(body []byte) []byte {
	body = applyCodexFingerprintToBody(NormalizeCodexRequestMetadata(body), fingerprint.ids)
	if fingerprint.accountIdentity != nil {
		body = fingerprint.accountIdentity.rewriteBody(body)
	}
	return StripCodexProjectMetadata(NormalizeCodexRequestMetadata(body))
}

func NormalizeCodexRequestMetadata(body []byte) []byte {
	metadata := gjson.GetBytes(body, "client_metadata")
	canonical := metadata.Get("x-codex-turn-metadata")
	if canonical.Type == gjson.String {
		if !gjson.Valid(canonical.String()) {
			return body
		}
		canonical = gjson.Parse(canonical.String())
	}
	if !metadata.IsObject() || !canonical.IsObject() {
		return body
	}
	for _, projection := range [][2]string{
		{"session_id", "session_id"}, {"thread_id", "thread_id"},
		{"window_id", "window_id"}, {"window_id", "x-codex-window-id"}, {"window_number", "window_number"},
		{"installation_id", "installation_id"}, {"installation_id", "x-codex-installation-id"},
		{"context_window_id", "context_window_id"}, {"context_window_id", "x-codex-context-window-id"},
		{"turn_id", "turn_id"}, {"root_turn_id", "root_turn_id"}, {"parent_turn_id", "parent_turn_id"},
		{"parent_thread_id", "parent_thread_id"}, {"parent_thread_id", "x-codex-parent-thread-id"},
		{"forked_from_thread_id", "forked_from_thread_id"}, {"forked_from_thread_id", "x-codex-forked-from-thread-id"},
		{"subagent_kind", "subagent_kind"}, {"subagent_kind", "x-openai-subagent"},
		{"thread_source", "thread_source"}, {"request_kind", "request_kind"},
	} {
		value, flat := canonical.Get(projection[0]), metadata.Get(projection[1])
		if !value.Exists() || !flat.Exists() {
			continue
		}
		path := "client_metadata." + projection[1]
		var updated []byte
		var err error
		if value.Type == gjson.Null || value.Type == gjson.String && strings.TrimSpace(value.String()) == "" {
			updated, err = sjson.DeleteBytes(body, path)
		} else if flat.Raw != value.Raw {
			updated, err = sjson.SetRawBytes(body, path, []byte(value.Raw))
		} else {
			continue
		}
		if err == nil {
			body = updated
		}
	}
	if kind := canonical.Get("request_kind"); kind.Exists() && metadata.Get("x-openai-memgen-request").Exists() {
		var updated []byte
		var err error
		if strings.EqualFold(kind.String(), "memory") {
			updated, err = sjson.SetBytes(body, "client_metadata.x-openai-memgen-request", "true")
		} else {
			updated, err = sjson.DeleteBytes(body, "client_metadata.x-openai-memgen-request")
		}
		if err == nil {
			body = updated
		}
	}
	return body
}

func CodexRequestMetadataHeaders(headers http.Header, body []byte) http.Header {
	body = NormalizeCodexRequestMetadata(body)
	resolved := headers.Clone()
	if resolved == nil {
		resolved = make(http.Header)
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return resolved
	}
	embedded := metadata.Get("x-codex-turn-metadata")
	canonical := embedded
	if embedded.Type == gjson.String {
		canonical = gjson.Parse(embedded.String())
	}
	frameSnapshot := embedded.Exists()
	sameSnapshot := canonical.IsObject() && canonical.Raw == headers.Get(codexTurnMetadataHeader)
	if frameSnapshot {
		resolved.Del(codexTurnMetadataHeader)
		if canonical.IsObject() {
			resolved.Set(codexTurnMetadataHeader, canonical.Raw)
		}
	}
	for _, projection := range []struct {
		header string
		flat   string
		field  string
	}{
		{codexSessionIDHeader, "session_id", "session_id"},
		{codexThreadIDHeader, "thread_id", "thread_id"},
		{codexWindowIDHeader, "x-codex-window-id", "window_id"},
		{codexParentThreadIDHeader, "x-codex-parent-thread-id", "parent_thread_id"},
		{"X-OpenAI-Subagent", "x-openai-subagent", "subagent_kind"},
		{"X-Codex-Turn-State", "x-codex-turn-state", ""},
		{codexClientRequestIDHeader, "x-client-request-id", "thread_id"},
	} {
		value := metadata.Get(projection.flat)
		if !value.Exists() && sameSnapshot && (projection.header == "X-Codex-Turn-State" || projection.header == codexClientRequestIDHeader) && resolved.Get(projection.header) != "" {
			continue
		}
		if projection.field != "" && canonical.IsObject() {
			if current := canonical.Get(projection.field); current.Exists() && (projection.header != codexClientRequestIDHeader || !value.Exists()) {
				value = current
			}
		}
		if !frameSnapshot && !value.Exists() {
			continue
		}
		resolved.Del(projection.header)
		if projection.header == codexSessionIDHeader {
			resolved.Del(codexLegacySessionIDHeader)
		}
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			resolved.Set(projection.header, strings.TrimSpace(value.String()))
		}
	}
	if frameSnapshot || metadata.Get("x-openai-memgen-request").Exists() {
		resolved.Del("X-OpenAI-Memgen-Request")
		if requestKind := canonical.Get("request_kind").String(); strings.EqualFold(requestKind, "memory") {
			resolved.Set("X-OpenAI-Memgen-Request", "true")
		} else if requestKind == "" && metadata.Get("x-openai-memgen-request").String() != "" {
			resolved.Set("X-OpenAI-Memgen-Request", metadata.Get("x-openai-memgen-request").String())
		}
	}
	if installation := canonical.Get("installation_id").String(); installation != "" && resolved.Get(codexInstallationIDHeader) != "" {
		resolved.Set(codexInstallationIDHeader, installation)
	}
	return resolved
}
