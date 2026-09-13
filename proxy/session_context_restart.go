package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func cleanSessionRestartContext(headers http.Header, body []byte, known sessionContextTokenVerifier) ([]byte, http.Header, *database.SessionContextCleanup, error) {
	report := &database.SessionContextCleanup{Mode: "lossy_restart", Phase: "prepared", Removed: make(map[string]int)}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, nil, report, codexAccountIdentityError("换号重开无法解析请求正文。")
	}
	allowed := func(kind, value string) bool { return known != nil && known(kind, value) }
	remove := func(kind string) { report.Removed[kind]++ }
	for _, field := range []string{"previous_response_id", "conversation", "conversation_id"} {
		value := gjson.ParseBytes(payload[field])
		if value.Exists() && value.Type != gjson.Null && value.String() != "" && !allowed(field, value.String()) {
			delete(payload, field)
			remove(field)
		}
	}
	headers = headers.Clone()
	if token := headers.Get("X-Codex-Turn-State"); token != "" && !allowed("turn_state", token) {
		headers.Del("X-Codex-Turn-State")
		remove("turn_state")
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(payload["client_metadata"], &metadata) == nil && metadata != nil {
		if token := gjson.ParseBytes(metadata["x-codex-turn-state"]).String(); token != "" && !allowed("turn_state", token) {
			delete(metadata, "x-codex-turn-state")
			payload["client_metadata"], _ = json.Marshal(metadata)
			remove("turn_state")
		}
	}
	var scrub func(gjson.Result, int) (json.RawMessage, bool)
	scrub = func(value gjson.Result, depth int) (json.RawMessage, bool) {
		if depth > 64 {
			remove("unsupported_depth")
			return nil, false
		}
		if value.IsArray() {
			items := make([]json.RawMessage, 0)
			for _, item := range value.Array() {
				if cleaned, keep := scrub(item, depth+1); keep {
					items = append(items, cleaned)
				}
			}
			raw, _ := json.Marshal(items)
			return raw, true
		}
		if !value.IsObject() {
			return json.RawMessage(value.Raw), true
		}
		kind := value.Get("type").String()
		if token := value.Get("encrypted_content"); token.Exists() && token.Type != gjson.Null && token.String() != "" && !allowed("encrypted_content", token.String()) {
			category := "encrypted_content"
			if kind == "reasoning" {
				category = "reasoning"
			}
			if kind == "compaction" || kind == "context_compaction" || kind == "compaction_summary" {
				category = "compaction"
			}
			remove(category)
			return nil, false
		}
		if kind == "item_reference" && !allowed("item_reference", value.Get("id").String()) {
			remove("item_reference")
			return nil, false
		}
		var object map[string]json.RawMessage
		_ = json.Unmarshal([]byte(value.Raw), &object)
		if token := value.Get("file_id"); token.Exists() && token.Type != gjson.Null && token.String() != "" && !allowed("file_id", token.String()) {
			remove("file_reference")
			if value.Get("file_data").String() == "" && value.Get("file_url").String() == "" && value.Get("image_url").Type != gjson.String {
				return nil, false
			}
			delete(object, "file_id")
		}
		for field, raw := range object {
			child := gjson.ParseBytes(raw)
			if !child.IsArray() && !child.IsObject() {
				continue
			}
			cleaned, keep := scrub(child, depth+1)
			if keep {
				object[field] = cleaned
			} else {
				delete(object, field)
			}
		}
		if kind == "message" || value.Get("role").String() != "" {
			content := gjson.ParseBytes(object["content"])
			if !content.Exists() || content.IsArray() && len(content.Array()) == 0 || content.Type == gjson.String && strings.TrimSpace(content.String()) == "" {
				remove("empty_message")
				return nil, false
			}
		}
		raw, _ := json.Marshal(object)
		return raw, true
	}
	input := gjson.ParseBytes(payload["input"])
	if input.IsArray() {
		items := make([]json.RawMessage, 0)
		for _, item := range input.Array() {
			if cleaned, keep := scrub(item, 0); keep {
				if identifier := item.Get("id").String(); identifier != "" && item.Get("type").String() != "item_reference" && !allowed("item_reference", identifier) {
					var fullItem map[string]json.RawMessage
					if json.Unmarshal(cleaned, &fullItem) == nil && fullItem != nil {
						delete(fullItem, "id")
						cleaned, _ = json.Marshal(fullItem)
						remove("item_id")
					}
				}
				items = append(items, cleaned)
			}
		}
		if gjson.ParseBytes(payload["previous_response_id"]).String() == "" {
			calls := make(map[string]bool)
			for _, item := range items {
				if strings.HasSuffix(gjson.GetBytes(item, "type").String(), "_call") {
					calls[gjson.GetBytes(item, "call_id").String()] = true
				}
			}
			paired := items[:0]
			for _, item := range items {
				if strings.HasSuffix(gjson.GetBytes(item, "type").String(), "_call_output") && !calls[gjson.GetBytes(item, "call_id").String()] {
					remove("orphan_tool_output")
					continue
				}
				paired = append(paired, item)
			}
			items = paired
		}
		payload["input"], _ = json.Marshal(items)
	}
	input = gjson.ParseBytes(payload["input"])
	if (!input.Exists() || input.Type == gjson.Null || input.IsArray() && len(input.Array()) == 0 || input.Type == gjson.String && strings.TrimSpace(input.String()) == "") && gjson.ParseBytes(payload["previous_response_id"]).String() == "" {
		return nil, headers, report, &Error{Code: "codex_session_failover_context_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "清理旧账号上下文后没有可用输入，请补充当前问题和必要资料，或新开对话。"}
	}
	cleaned, err := json.Marshal(payload)
	return cleaned, headers, report, err
}

func (epoch *sessionOutboundEpoch) restartContextVerifier(ctx context.Context) (sessionContextTokenVerifier, context.CancelFunc) {
	known, cancel := epoch.handler.sessionContextVerifierForScope(ctx, sessionContextScope(epoch.owner, epoch.key, epoch.upstreamAccount, epoch.record))
	return func(kind, value string) bool {
		if kind != "previous_response_id" {
			return known(kind, value)
		}
		lookup, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		affinity, found := lookupResponseAccountAffinity(lookup, epoch.handler.cache, epoch.owner, value)
		return found && affinity.AccountID == epoch.record.AccountID && affinity.OutboundSegment == epoch.identityKey()
	}, cancel
}

func PrepareSessionRestartOutbound(ctx context.Context, account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header, error) {
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil || !epoch.record.LossyContextRestart || account == nil || account.IsRelayStyle() {
		return body, headers, nil
	}
	if err := validateSessionOutboundEpoch(ctx, account); err != nil {
		return nil, nil, err
	}
	known, cancel := epoch.restartContextVerifier(ctx)
	defer cancel()
	cleaned, outgoingHeaders, report, err := cleanSessionRestartContext(headers, body, known)
	if epoch.diagnostic != nil && len(report.Removed) > 0 {
		report.Phase = "outbound"
		previous := epoch.diagnostic.ContextCleanup
		if previous != nil && previous.Phase == "outbound" {
			for kind, count := range previous.Removed {
				report.Removed[kind] += count
			}
		}
		epoch.diagnostic.ContextCleanup = report
	}
	return cleaned, outgoingHeaders, err
}

func sessionRestartRoutingContext(request *gin.Context, body []byte) ([]byte, http.Header) {
	plan, _ := request.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan)
	epoch := outboundEpochFromContext(request.Request.Context())
	if plan == nil && (epoch == nil || !epoch.record.LossyContextRestart) {
		return body, sessionFailoverRequestHeaders(request)
	}
	var known sessionContextTokenVerifier
	if plan == nil {
		var cancel context.CancelFunc
		known, cancel = epoch.restartContextVerifier(request.Request.Context())
		defer cancel()
	}
	cleaned, headers, _, err := cleanSessionRestartContext(sessionFailoverRequestHeaders(request), body, known)
	if err != nil {
		return body, sessionFailoverRequestHeaders(request)
	}
	return cleaned, headers
}
