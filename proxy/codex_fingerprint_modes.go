package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// singleMachineIdentity deliberately uses one namespace for thread references:
// a child's parent_thread_id must equal the parent's rewritten thread_id.
// It is independent of request timing, account token refreshes and concurrency.
func singleMachineIdentity(accountID int64, original string) string {
	if original == "" {
		return ""
	}
	seed := fmt.Sprintf("codex2api:single-machine-identity:v1:%d:%s", accountID, original)
	return deriveStableCodexUUIDv7(seed, seededIdentityUnixMilli(seed))
}

func convergeFingerprintLineageValue(ids *codexFingerprintIDs, key, original string) string {
	if ids.mode == auth.CodexFingerprintModeSingleMachineMultiWindow && key != "context_window_id" {
		return singleMachineIdentity(ids.accountID, original)
	}
	return convergeCodexLineageValue(ids.accountID, key, original)
}

func convergeFingerprintLineageMetadata(raw string, ids *codexFingerprintIDs) (string, bool) {
	if ids.mode != auth.CodexFingerprintModeSingleMachineMultiWindow {
		return convergeCodexLineageMetadata(raw, ids.accountID)
	}
	changed := false
	for _, key := range codexLineageMetadataPaths {
		v := gjson.Get(raw, key)
		if v.Type != gjson.String || strings.TrimSpace(v.String()) == "" {
			continue
		}
		next, err := sjson.Set(raw, key, convergeFingerprintLineageValue(ids, key, strings.TrimSpace(v.String())))
		if err == nil && next != raw {
			raw = next
			changed = true
		}
	}
	return raw, changed
}

func rewriteSingleMachineBodyLineage(body []byte, ids *codexFingerprintIDs) []byte {
	for _, key := range codexLineageMetadataPaths {
		path := "client_metadata." + key
		v := gjson.GetBytes(body, path)
		if v.Type == gjson.String && strings.TrimSpace(v.String()) != "" {
			body = setExistingJSONString(body, path, convergeFingerprintLineageValue(ids, key, strings.TrimSpace(v.String())))
		}
	}
	body = setExistingJSONString(body, "client_metadata.window_id", ids.windowID)
	return body
}

// PrepareCodexFingerprintHeaders makes body-only identities available to all
// consumers BEFORE any body rewrite. Caller headers are never mutated. This is
// only enabled for the new mode; old modes retain their exact historical input.
// Do not recover identity from prompt_cache_key or user content.
func PrepareCodexFingerprintHeaders(account *auth.Account, headers http.Header, body []byte) http.Header {
	if account == nil || account.EffectiveCodexFingerprintMode() != auth.CodexFingerprintModeSingleMachineMultiWindow {
		return headers
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	embedded := metadata.Get("x-codex-turn-metadata")
	if out.Get(codexTurnMetadataHeader) == "" && embedded.Type == gjson.String && gjson.Valid(embedded.String()) && gjson.Parse(embedded.String()).IsObject() {
		out.Set(codexTurnMetadataHeader, embedded.String())
	}
	session, thread := extractClientCodexIdentity(out)
	if session == "" {
		if v := metadata.Get("session_id"); v.Type == gjson.String && strings.TrimSpace(v.String()) != "" {
			out.Set(codexSessionIDHeader, strings.TrimSpace(v.String()))
		}
	}
	if thread == "" {
		if v := metadata.Get("thread_id"); v.Type == gjson.String && strings.TrimSpace(v.String()) != "" {
			out.Set(codexThreadIDHeader, strings.TrimSpace(v.String()))
		}
	}
	return out
}

// ScopeCodexFingerprintTransportKey keeps pooled WS handshakes consistent with
// per-frame identities. Retain the existing key/API-key partition as a prefix.
// This is a local pool key, never an upstream session or prompt_cache_key.
func ScopeCodexFingerprintTransportKey(key string, account *auth.Account, headers http.Header) string {
	if key == "" || account == nil || account.EffectiveCodexFingerprintMode() != auth.CodexFingerprintModeSingleMachineMultiWindow {
		return key
	}
	ids := resolveCodexFingerprintIDs(account, headers)
	if ids == nil {
		return key
	}
	return key + ":fingerprint:" + deriveStableCodexUUID(ids.sessionID+":"+ids.threadID)
}
