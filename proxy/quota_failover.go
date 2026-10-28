package proxy

import (
	"log"
	"net/http"
	"strings"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// logQuotaFailoverRejected records why an exhausted account's continuation
// could not move to another account. The access log only shows a 409 that
// reads like a lost parent response; the item counts tell opaque state,
// incomplete tool context and a parent reference apart without logging any
// conversation content.
func logQuotaFailoverRejected(c *gin.Context, transport string, accountID int64, attempt int, rawBody []byte, apiErr *api.APIError) {
	if apiErr == nil {
		return
	}
	reason := ""
	if detail, ok := apiErr.Details.(api.ErrorDetail); ok {
		reason = detail.Message
	}
	input := gjson.GetBytes(rawBody, "input")
	inputKind := "array"
	switch {
	case !input.Exists():
		inputKind = "missing"
	case input.Type == gjson.String:
		inputKind = "string"
	case !input.IsArray():
		inputKind = "other"
	}
	var items, reasoningEncrypted, compactionEncrypted, otherEncrypted, references, calls, outputs int
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			items++
			itemType := item.Get("type").String()
			switch {
			case itemType == "item_reference":
				references++
			case item.Get("encrypted_content").String() != "":
				switch {
				case itemType == "reasoning":
					reasoningEncrypted++
				case isEncryptedCompactionItemType(itemType):
					compactionEncrypted++
				default:
					otherEncrypted++
				}
			}
			if _, call, output := responseContextPairType(itemType); call {
				calls++
			} else if output {
				outputs++
			}
			return true
		})
	}
	log.Printf("[quota-failover] rejected transport=%s code=%s reason=%s account=%d attempt=%d api_key_id=%d previous_response_id=%t input=%s items=%d reasoning_encrypted=%d compaction_encrypted=%d other_encrypted=%d item_references=%d tool_calls=%d tool_outputs=%d",
		transport, apiErr.Code, reason, accountID, attempt, requestAPIKeyID(c),
		strings.TrimSpace(gjson.GetBytes(rawBody, "previous_response_id").String()) != "",
		inputKind, items, reasoningEncrypted, compactionEncrypted, otherEncrypted, references, calls, outputs)
}

// A turn token is an affinity hint when its context can be replayed. It must
// not pin a complete request to an account that can no longer serve it. Keep
// the existing WHAM continuation exception: a local 100% observation alone
// does not prove that the upstream will reject an already active turn. A
// configured auto-pause threshold has no such exception: the scheduler never
// selects that account, so a pinned continuation would only wait it out.
func (h *Handler) quotaBlockedContinuationAccount(accountID int64) *auth.Account {
	if h == nil || h.store == nil || accountID == 0 {
		return nil
	}
	account := h.store.FindByID(accountID)
	if account == nil {
		return nil
	}
	if account.QuotaAutoPaused() {
		return account
	}
	if !account.FreshDispatchUsageLimited() || account.UsageLimitContinuationEligible() {
		return nil
	}
	return account
}

// quotaFailoverHasCandidate reports whether a just-released quota continuation
// can still be served by the ordinary pool. Without one, callers report quota
// exhaustion at once instead of waiting out the account selection budget.
// Saturated accounts still count as candidates, so a full pool keeps waiting.
func (h *Handler) quotaFailoverHasCandidate(affinityKey string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy) bool {
	return h.store.HasDispatchCandidate(affinityKey, apiKeyID, exclude, filter, false, policy)
}

// quotaFailoverOpaqueItem reports input state that cannot move to another
// account. Encrypted reasoning is the exception: an ordinary account switch
// already carries it, and the replacement account's invalid_encrypted_content
// rejection strips it and retries once. The conversation and tool history stay
// intact; only that account's hidden reasoning is lost. An item reference or
// encrypted compaction would silently drop history, so those still fail.
func quotaFailoverOpaqueItem(item gjson.Result) bool {
	itemType := item.Get("type").String()
	if itemType == "item_reference" {
		return true
	}
	return item.Get("encrypted_content").String() != "" && itemType != "reasoning"
}

// quotaFailoverEncryptedReasoning counts the reasoning items a failover
// carries to the replacement account, which may strip them on rejection.
func quotaFailoverEncryptedReasoning(body []byte) int {
	count := 0
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").String() != "" {
			count++
		}
		return true
	})
	return count
}

// Validate without filtering, truncating or decoding opaque state. In
// particular, a tool output alone is not a complete conversation, and neither
// an item reference nor encrypted history may silently disappear on failover.
func responsesQuotaOpaqueInput(body []byte) bool {
	opaque := false
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		opaque = quotaFailoverOpaqueItem(item)
		return !opaque
	})
	return opaque
}

func responsesQuotaReplayError(body []byte) *api.APIError {
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return responsesWSContextUnavailable(http.StatusConflict, "quota_failover_context_missing")
	}
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		return nil
	}
	if !input.IsArray() {
		return responsesWSContextUnavailable(http.StatusConflict, "quota_failover_context_missing")
	}
	calls := make(map[string]bool)
	var reason string
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() || quotaFailoverOpaqueItem(item) {
			reason = "quota_failover_nonportable_context"
			return false
		}
		kind, call, output := responseContextPairType(item.Get("type").String())
		if call || output {
			id := item.Get("call_id").String()
			key := kind + ":" + id
			if id == "" || (output && !calls[key]) {
				reason = "quota_failover_incomplete_tool_context"
				return false
			}
			if call {
				calls[key] = true
			}
		}
		return true
	})
	if reason != "" {
		return responsesWSContextUnavailable(http.StatusConflict, reason)
	}
	return nil
}

// Keep provider-native fields from rawBody, replacing only reconstructed
// lineage and account-bound continuation metadata. The request's ordinary
// model/group/scope filters still govern selection of the replacement account.
func responsesQuotaReplayBody(rawBody, replayBody []byte) ([]byte, *api.APIError) {
	// Compatibility preparation can repair orphan tool outputs or omit opaque
	// reasoning. Check the client's complete input before those rewrites too.
	if responsesQuotaOpaqueInput(rawBody) {
		return nil, responsesWSContextUnavailable(http.StatusConflict, "quota_failover_nonportable_context")
	}
	hasPrevious := strings.TrimSpace(gjson.GetBytes(rawBody, "previous_response_id").String()) != ""
	if !hasPrevious {
		if err := responsesQuotaReplayError(rawBody); err != nil {
			return nil, err
		}
	}
	if err := responsesQuotaReplayError(replayBody); err != nil {
		return nil, err
	}
	// Complete requests already contain their input. Preserve native provider
	// fields and avoid an extra full-body copy for a large Desktop request.
	body := rawBody
	var err error
	if hasPrevious {
		body, err = sjson.SetRawBytes(body, "input", []byte(gjson.GetBytes(replayBody, "input").Raw))
	}
	if err == nil {
		body, err = sjson.DeleteBytes(body, "previous_response_id")
	}
	if err == nil {
		body, err = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-state")
	}
	if err != nil {
		return nil, responsesWSContextUnavailable(http.StatusConflict, "invalid_context")
	}
	return body, nil
}

func responsesWSQuotaReplayBody(rawBody, body []byte, source *responsesWSReplaySource) ([]byte, []byte, *api.APIError) {
	// The existing replay normalizer may omit unsupported input items. Inspect
	// opaque client items before reconstruction so none can disappear first.
	if responsesQuotaOpaqueInput(rawBody) {
		return nil, nil, responsesWSContextUnavailable(http.StatusConflict, "quota_failover_nonportable_context")
	}
	replayBody := body
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		// Quota failover never uses CODEX_WS_CONTINUATION_FAIL_OPEN. Recover the
		// whole lineage before unbinding, including owner and size checks.
		input := source.Input()
		if source.err != nil {
			return nil, nil, source.err
		}
		var err error
		replayBody, err = sjson.SetRawBytes(body, "input", []byte(input))
		if err == nil {
			replayBody, err = sjson.DeleteBytes(replayBody, "previous_response_id")
		}
		if err != nil {
			return nil, nil, responsesWSContextUnavailable(http.StatusConflict, "invalid_context")
		}
	}
	raw, apiErr := responsesQuotaReplayBody(rawBody, replayBody)
	if apiErr != nil {
		return nil, nil, apiErr
	}
	prepared, apiErr := responsesQuotaReplayBody(replayBody, replayBody)
	return raw, prepared, apiErr
}
