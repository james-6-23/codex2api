package proxy

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const sessionContinuityContextKey = "codex_session_continuity"

type sessionContinuityDiagnostic struct {
	Mode            string                            `json:"mode"`
	Result          string                            `json:"result"`
	WouldBlock      bool                              `json:"would_block"`
	Action          string                            `json:"action"`
	ThreadID        string                            `json:"thread_id,omitempty"`
	Previous        *uint64                           `json:"previous_number,omitempty"`
	Current         *uint64                           `json:"current_number,omitempty"`
	OwnerSource     string                            `json:"owner_source"`
	OwnerAccount    int64                             `json:"owner_account_id,omitempty"`
	AccountFailover *sessionAccountFailoverDiagnostic `json:"account_failover,omitempty"`
}

type sessionContinuityCacheEntry struct {
	Record    database.SessionContinuityRecord
	CheckedAt time.Time
	WrittenAt time.Time
}

type sessionContinuityRequest struct {
	Key        string
	ThreadID   string
	Number     uint64
	Known      bool
	Record     database.SessionContinuityRecord
	Admitted   bool
	StartedAt  time.Time
	Diagnostic *sessionContinuityDiagnostic
}

func continuityRequest(request *gin.Context) *sessionContinuityRequest {
	if request == nil {
		return nil
	}
	value, _ := request.Get(sessionContinuityContextKey)
	state, _ := value.(*sessionContinuityRequest)
	return state
}

func requestRequiresCompactionOwner(request *gin.Context, body []byte) bool {
	websocket := isResponsesWebSocketUpgradeRequest(request.Request)
	resolved := usageRequestDiagnosticState(request).Resolved
	if !websocket && resolved != nil && strings.EqualFold(strings.TrimSpace(resolved.RequestKind), "compaction") {
		return true
	}
	if request.Request.URL != nil && isCompactUsageEndpoint(request.Request.URL.Path) {
		return true
	}
	if requestBodyCompactionMeta(body).ProtocolTriggered {
		return true
	}
	headers := request.Request.Header
	if websocket {
		headers = nil
	}
	headers = CodexRequestMetadataHeaders(headers, body)
	return turnMetadataIndicatesCompaction(headers.Get(codexTurnMetadataHeader))
}

func parseContinuityWindow(headers http.Header, body []byte, websocket bool) (string, uint64, bool, string) {
	if websocket {
		headers = nil
	}
	headers = CodexRequestMetadataHeaders(headers, body)
	window := strings.TrimSpace(headers.Get(codexWindowIDHeader))
	thread := normalizeSessionGraphValue(headers.Get(codexThreadIDHeader))
	if window == "" {
		return thread, 0, false, "window_missing"
	}
	separator := strings.LastIndexByte(window, ':')
	if separator <= 0 || separator == len(window)-1 {
		return thread, 0, false, "window_invalid"
	}
	windowThread := normalizeSessionGraphValue(window[:separator])
	number, err := strconv.ParseUint(window[separator+1:], 10, 64)
	if err != nil || !validSessionGraphUUID(windowThread) {
		return thread, 0, false, "window_invalid"
	}
	if thread != "" && thread != windowThread {
		return thread, number, true, "thread_conflict"
	}
	metadataNumber := gjson.Get(headers.Get(codexTurnMetadataHeader), "window_number")
	if metadataNumber.Exists() {
		canonicalNumber, parseErr := strconv.ParseUint(metadataNumber.Raw, 10, 64)
		if metadataNumber.Type != gjson.Number || parseErr != nil || canonicalNumber != number {
			return windowThread, number, true, "number_conflict"
		}
	}
	return windowThread, number, true, ""
}

func evaluateSessionContinuity(previous database.SessionContinuityRecord, found bool, thread string, number uint64, owner int64) (string, bool) {
	if found && previous.ThreadID != "" && previous.ThreadID != thread {
		return "thread_conflict", true
	}
	if !found || !previous.NumberKnown {
		if owner > 0 {
			return "baseline", false
		}
		if number == 0 {
			return "new_root", false
		}
		return "unbound_nonzero", true
	}
	if number == previous.Number {
		return "same_window", false
	}
	if number > previous.Number && number-previous.Number == 1 {
		return "window_advanced", false
	}
	if number < previous.Number {
		return "window_regressed", owner <= 0 || previous.AccountID != owner
	}
	return "window_gap", true
}

func (handler *Handler) readSessionContinuity(ctx context.Context, key string) (sessionContinuityCacheEntry, bool, error) {
	handler.continuityMu.Lock()
	entry, found := handler.continuityRecords[key]
	handler.continuityMu.Unlock()
	if handler.db == nil {
		return entry, entry.Record.AccountID > 0, nil
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	record, exists, err := handler.db.ReadSessionContinuity(lookup, key)
	if err != nil {
		return entry, false, err
	}
	if found && exists && entry.Record.AccountID == record.AccountID && entry.Record.FailoverCount == record.FailoverCount && entry.Record.NumberKnown && (!record.NumberKnown || entry.Record.Number > record.Number) {
		record.Number, record.NumberKnown = entry.Record.Number, true
	}
	entry = sessionContinuityCacheEntry{Record: record, CheckedAt: time.Now(), WrittenAt: record.LastSeen}
	if exists {
		handler.cacheSessionContinuity(key, entry)
	}
	return entry, exists, nil
}

func (handler *Handler) cacheSessionContinuity(key string, entry sessionContinuityCacheEntry) {
	handler.continuityMu.Lock()
	defer handler.continuityMu.Unlock()
	if handler.continuityRecords == nil {
		handler.continuityRecords = make(map[string]sessionContinuityCacheEntry)
	}
	if len(handler.continuityRecords) >= 32768 {
		if _, exists := handler.continuityRecords[key]; !exists {
			for candidate := range handler.continuityRecords {
				delete(handler.continuityRecords, candidate)
				break
			}
		}
	}
	handler.continuityRecords[key] = entry
}

func (handler *Handler) resolveForkSourceOwner(ctx context.Context, identity requestSessionIdentity, targetKey string, apiKeyID int64) (int64, string, error) {
	if strings.TrimSpace(identity.forkSourceAffinityID) == "" {
		return 0, "", nil
	}
	sourceKey := sessionAffinityKey(identity.forkSourceAffinityID, apiKeyID)
	if sourceKey == "" || sourceKey == targetKey {
		return 0, "", nil
	}
	entry, found, err := handler.readSessionContinuity(ctx, hashRiskIdentity(sourceKey))
	if err != nil {
		return 0, "", err
	}
	if found {
		return entry.Record.AccountID, "fork_source_persistent", nil
	}
	accountID, _ := handler.store.LiveSessionAccountID(sourceKey, time.Now())
	return accountID, "fork_source_live", nil
}

func (handler *Handler) prepareSessionContinuity(request *gin.Context, identity requestSessionIdentity, affinityKey string, body []byte) *api.APIError {
	request.Set(sessionContinuityContextKey, nil)
	if !identity.stableIdentity || identity.relatedToRoot && !identity.ownsRootBinding || identity.bypassWindowAccounting || identity.unlinkedFallbackOnly || affinityKey == "" {
		return nil
	}
	resolved := usageRequestDiagnosticState(request).Resolved
	if resolved == nil || identity.requiresRootAccount || resolved.SubagentKind != "" {
		return nil
	}
	source := strings.TrimSpace(resolved.ThreadSource)
	if source != "" && !strings.EqualFold(source, "user") || source == "" && resolved.RootState != "resolved" {
		return nil
	}
	mode := handler.promptFilterConfigForRequest(request).Advanced.Risk.SessionContinuityMode
	if mode == "" {
		mode = "observe"
	}
	thread, number, known, invalid := parseContinuityWindow(request.Request.Header, body, isResponsesWebSocketUpgradeRequest(request.Request))
	diagnostic := &sessionContinuityDiagnostic{Mode: mode, ThreadID: diagnosticIdentifier(thread), OwnerSource: "missing", Action: "allow"}
	if known {
		diagnostic.Current = &number
	}
	usageRequestDiagnosticState(request).Continuity = diagnostic
	key := hashRiskIdentity(affinityKey)
	shard, _ := strconv.ParseUint(key[:2], 16, 8)
	lock := &handler.continuityLocks[shard%uint64(len(handler.continuityLocks))]
	lock.Lock()
	defer lock.Unlock()
	entry, found, err := handler.readSessionContinuity(request.Request.Context(), key)
	owner, _ := handler.store.LiveSessionAccountID(affinityKey, time.Now())
	if found && entry.Record.FailoverCount > 0 && owner > 0 && owner != entry.Record.AccountID {
		handler.store.UnbindSessionAffinity(affinityKey, owner)
		owner = entry.Record.AccountID
	}
	if owner > 0 {
		diagnostic.OwnerSource = "live_binding"
	}
	if grant := windowGrantForRequest(request); grant != nil && grant.Grant.OwnerAccountID > 0 && grant.Grant.OwnerKey == affinityKey {
		if owner > 0 && owner != grant.Grant.OwnerAccountID {
			invalid = "owner_conflict"
		}
		owner, diagnostic.OwnerSource = grant.Grant.OwnerAccountID, "window_grant"
	}
	if found {
		if epoch := outboundEpochFromContext(request.Request.Context()); epoch != nil && (epoch.record.AccountID != entry.Record.AccountID || epoch.record.FailoverCount != entry.Record.FailoverCount) {
			return sessionContinuityError("owner_conflict")
		}
		handler.attachSessionOutboundEpoch(request, key, entry.Record)
		diagnostic.OwnerSource = "persistent_binding"
		if owner > 0 && owner != entry.Record.AccountID {
			invalid = "owner_conflict"
		}
		owner = entry.Record.AccountID
		if entry.Record.FailoverCount > 0 {
			diagnostic.AccountFailover = &sessionAccountFailoverDiagnostic{Result: "restored", PreviousAccountID: entry.Record.PreviousAccountID, AccountID: owner, Generation: entry.Record.FailoverCount, Reason: entry.Record.LastFailoverReason}
			if entry.Record.LossyContextRestart {
				diagnostic.AccountFailover = usageRequestDiagnosticState(request).AccountFailover
			}
			if err := handler.validateMigratedSessionContext(request, body, entry.Record, affinityKey); err != nil {
				return err
			}
		}
		if entry.Record.NumberKnown {
			previous := entry.Record.Number
			diagnostic.Previous = &previous
		}
	}
	if err == nil && !found && owner == 0 && invalid == "" && known && identity.forkSourceAffinityID != "" && !requestRequiresCompactionOwner(request, body) {
		owner, diagnostic.OwnerSource, err = handler.resolveForkSourceOwner(request.Request.Context(), identity, affinityKey, requestAPIKeyID(request))
		if err != nil || owner == 0 || handler.store.FindByID(owner) == nil {
			diagnostic.Result, diagnostic.WouldBlock, diagnostic.Action = "fork_owner_unavailable", true, "blocked"
			return sessionContinuityError("fork_owner_unavailable")
		}
	}
	diagnostic.OwnerAccount = owner
	if owner > 0 {
		selectionTraceForRequest(request).PinAccount(owner)
		recordUsageRootAccount(request, owner, true)
	}
	if err != nil {
		diagnostic.Result, diagnostic.WouldBlock = "storage_unavailable", true
	} else if invalid != "" {
		diagnostic.Result, diagnostic.WouldBlock = invalid, true
	} else {
		diagnostic.Result, diagnostic.WouldBlock = evaluateSessionContinuity(entry.Record, found, thread, number, owner)
	}
	if invalid == "owner_conflict" || err != nil && owner == 0 {
		diagnostic.Result, diagnostic.WouldBlock, diagnostic.Action = "ownership_unavailable", true, "blocked"
		return sessionContinuityError("ownership_unavailable")
	}
	if owner == 0 && requestRequiresCompactionOwner(request, body) {
		diagnostic.Result, diagnostic.WouldBlock, diagnostic.Action = "unbound_compaction", true, "blocked"
		return sessionContinuityError("unbound_compaction")
	}
	if mode == "off" {
		diagnostic.Result, diagnostic.WouldBlock, diagnostic.Action = "disabled", false, "disabled"
	} else if diagnostic.WouldBlock {
		diagnostic.Action = "observe"
		if mode == "enforce" {
			diagnostic.Action = "blocked"
			return sessionContinuityError(diagnostic.Result)
		}
	}
	state := &sessionContinuityRequest{Key: key, ThreadID: thread, Number: number, Known: known && invalid == "", Record: entry.Record, StartedAt: time.Now().UTC(), Diagnostic: diagnostic}
	request.Set(sessionContinuityContextKey, state)
	if owner > 0 {
		if err := handler.bindWindowGrantOwner(request, owner, affinityKey); err != nil {
			return requestWindowGrantAPIError(err)
		}
	}
	return nil
}

func sessionContinuityError(reason string) *api.APIError {
	message := "会话上下文序号不连续，请恢复正确的对话后重试。"
	if reason == "unbound_nonzero" {
		message = "当前请求来自已有上下文窗口，请新开对话后重试。"
	} else if reason == "unbound_compaction" {
		message = "无法恢复当前压缩请求的原会话账号，请先恢复主会话连接；无法恢复时请新开对话。"
	} else if reason == "fork_owner_unavailable" {
		message = "无法核实 fork 父会话的原始账号归属，请先恢复父会话后重试。"
	} else if reason == "window_missing" || reason == "window_invalid" || reason == "number_conflict" || reason == "thread_conflict" {
		message = "会话窗口标识缺失或不一致，请重新连接正确的对话。"
	} else if reason == "ownership_unavailable" || reason == "storage_unavailable" {
		message = "会话账号归属暂时无法确认，请稍后重试。"
	}
	result := api.NewAPIError(api.ErrorCode("codex_session_continuity_"+reason), message, api.ErrorTypeInvalidRequest)
	result.Details = gin.H{"reason": reason, "retry": "stop"}
	return result
}

func (handler *Handler) commitSessionContinuity(request *gin.Context, account *auth.Account) *api.APIError {
	state := continuityRequest(request)
	if state == nil || account == nil {
		return nil
	}
	if state.Diagnostic.OwnerAccount > 0 && state.Diagnostic.OwnerAccount != account.ID() || state.Record.AccountID > 0 && state.Record.AccountID != account.ID() {
		return sessionContinuityError("owner_conflict")
	}
	if state.Admitted {
		return nil
	}
	shard, _ := strconv.ParseUint(state.Key[:2], 16, 8)
	lock := &handler.continuityLocks[shard%uint64(len(handler.continuityLocks))]
	lock.Lock()
	defer lock.Unlock()
	entry, found, err := handler.readSessionContinuity(request.Request.Context(), state.Key)
	if err != nil {
		return sessionContinuityError("ownership_unavailable")
	}
	if found && (entry.Record.AccountID != account.ID() || state.Record.AccountID > 0 && entry.Record.FailoverCount != state.Record.FailoverCount) {
		return sessionContinuityError("owner_conflict")
	}
	if state.Diagnostic.Mode == "enforce" && state.Known {
		if reason, blocked := evaluateSessionContinuity(entry.Record, found, state.ThreadID, state.Number, account.ID()); blocked {
			state.Diagnostic.Result, state.Diagnostic.WouldBlock, state.Diagnostic.Action = reason, true, "blocked"
			return sessionContinuityError(reason)
		}
	}
	next := entry.Record
	next.AccountID, next.LastSeen = account.ID(), state.StartedAt
	trackNumber := state.Known && state.ThreadID != "" && (next.ThreadID == "" || next.ThreadID == state.ThreadID)
	changed := !found || trackNumber && (!next.NumberKnown || state.Number > next.Number)
	if trackNumber && (!next.NumberKnown || state.Number > next.Number) {
		next.ThreadID = state.ThreadID
		next.Number, next.NumberKnown = state.Number, true
	}
	if handler.db != nil && (changed || time.Since(entry.WrittenAt) >= 5*time.Minute) {
		ctx, cancel := context.WithTimeout(request.Request.Context(), time.Second)
		next, err = handler.db.CommitSessionContinuity(ctx, state.Key, next)
		cancel()
		if err != nil {
			if errors.Is(err, database.ErrSessionOwnerConflict) {
				return sessionContinuityError("owner_conflict")
			}
			return sessionContinuityError("ownership_unavailable")
		}
		entry.WrittenAt = time.Now()
	}
	entry.Record, entry.CheckedAt = next, time.Now()
	handler.cacheSessionContinuity(state.Key, entry)
	state.Record, state.Admitted = next, true
	handler.attachSessionOutboundEpoch(request, state.Key, next)
	selectionTraceForRequest(request).PinAccount(account.ID())
	return nil
}

func (handler *Handler) completeSessionContinuity(request *gin.Context, input *database.UsageLogInput) {
	state := continuityRequest(request)
	if state == nil || !state.Admitted || !state.Known || state.ThreadID != state.Record.ThreadID || input == nil || input.AccountID != state.Record.AccountID {
		return
	}
	handler.continuityMu.Lock()
	defer handler.continuityMu.Unlock()
	entry, found := handler.continuityRecords[state.Key]
	if found && entry.Record.AccountID == input.AccountID && entry.Record.FailoverCount == state.Record.FailoverCount {
		number := state.Number
		entry.Record.LastCompleted, entry.Record.CompletedNumber, entry.Record.LastStatus = time.Now().UTC(), &number, input.StatusCode
		handler.continuityRecords[state.Key] = entry
	}
}

func (handler *Handler) pinnedSessionCapacityError(request *gin.Context, key string) *api.APIError {
	trace := selectionTraceForRequest(request)
	accountID := trace.PinnedAccount()
	if accountID == 0 {
		return nil
	}
	account := handler.store.FindByID(accountID)
	if account == nil {
		return api.NewAPIError(api.ErrorCode("codex_sticky_account_unavailable"), "当前窗口绑定账号已不可用，请新开对话。", api.ErrorTypeInvalidRequest)
	}
	if handler.store.CanAdmitAccountSession(account, key, time.Now(), trace) {
		return nil
	}
	limits := account.SessionCapacityLimits()
	total, reserved := handler.store.AccountSessionSlotCounts(accountID, time.Now())
	code := "codex_sticky_account_capacity_full"
	message := "当前窗口绑定账号的普通及扩容会话容量均已满，请新开对话或切换其他窗口使用。"
	if !trace.ExpandedWindow() && total < limits.Total && reserved < limits.Reserved {
		code = "codex_sticky_expansion_required"
		message = "当前窗口绑定账号的普通会话容量已满，请在「窗口管理」开启扩容并确认此窗口的扩容费用后重试。"
	}
	return api.NewAPIError(api.ErrorCode(code), message, api.ErrorTypeInvalidRequest)
}
