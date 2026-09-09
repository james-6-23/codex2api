package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

type CodexFingerprint struct {
	ids     *codexFingerprintIDs
	headers http.Header
}

func NewCodexFingerprint(account *auth.Account, headers http.Header, body []byte) *CodexFingerprint {
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
}

func (fingerprint *CodexFingerprint) ApplyBody(body []byte) []byte {
	return applyCodexFingerprintToBody(body, fingerprint.ids)
}

func CodexRequestMetadataHeaders(headers http.Header, body []byte) http.Header {
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
