package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type codexIdentityClaimer interface {
	ClaimCodexIdentities(context.Context, []string, string) error
}

type codexIdentityClaimerContextKey struct{}

func codexIdentityRequestError(err error) *api.APIError {
	var requestError *Error
	if !errors.As(err, &requestError) {
		return nil
	}
	switch requestError.Code {
	case "codex_session_identity_invalid", "codex_session_identity_conflict", "codex_session_identity_unavailable", "codex_background_account_mismatch", "codex_session_failover_context_required":
		return api.NewAPIError(api.ErrorCode(requestError.Code), requestError.Message, api.ErrorTypeInvalidRequest)
	default:
		return nil
	}
}

func sendCodexIdentityRequestError(ctx *gin.Context, err error, protocol continuousRetryHTTPProtocol) bool {
	identityError := codexIdentityRequestError(err)
	if identityError == nil {
		return false
	}
	if !claimContinuousRetryTerminal(ctx, protocol) {
		return true
	}
	api.ObserveError(ctx, http.StatusBadRequest, identityError)
	if retryKeepaliveCommitted(ctx) {
		if ctx.Request.Context().Err() != nil {
			return true
		}
		payload := gin.H{"error": identityError}
		prefix := "data: "
		switch protocol {
		case continuousRetryProtocolResponses:
			payload = gin.H{"type": "response.failed", "response": gin.H{"created_at": time.Now().Unix(), "status": "failed", "error": identityError}}
		case continuousRetryProtocolAnthropic:
			payload["type"] = "error"
			prefix = "event: error\ndata: "
		}
		encoded, _ := json.Marshal(payload)
		_, _ = ctx.Writer.WriteString(prefix + string(encoded) + "\n\n")
		ctx.Writer.Flush()
		return true
	}
	if protocol == continuousRetryProtocolAnthropic {
		ctx.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": identityError})
	} else {
		ctx.JSON(http.StatusBadRequest, api.ErrorResponse{Error: *identityError})
	}
	return true
}

type localCodexIdentityClaims struct {
	mu     sync.Mutex
	owners map[string]string
}

func (claims *localCodexIdentityClaims) ClaimCodexIdentities(ctx context.Context, keys []string, owner string) error {
	claims.mu.Lock()
	defer claims.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	newKeys := make(map[string]bool)
	for _, key := range keys {
		if existing := claims.owners[key]; existing != "" && existing != owner {
			return database.ErrCodexIdentityConflict
		} else if existing == "" {
			newKeys[key] = true
		}
	}
	if len(claims.owners)+len(newKeys) > 65536 {
		return errors.New("codex identity registry capacity exhausted")
	}
	if claims.owners == nil {
		claims.owners = make(map[string]string)
	}
	for key := range newKeys {
		claims.owners[key] = owner
	}
	return nil
}

func (handler *Handler) bindCodexIdentityClaims(ctx *gin.Context) {
	var claimer codexIdentityClaimer = &handler.codexIdentityClaims
	if handler.db != nil {
		claimer = handler.db
	}
	ctx.Request = ctx.Request.WithContext(context.WithValue(ctx.Request.Context(), codexIdentityClaimerContextKey{}, claimer))
	handler.bindCodexReferenceRootLookup(ctx)
}

func codexIdentityDigest(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (fingerprint *CodexFingerprint) ClaimSessionIdentity(ctx context.Context, account *auth.Account, apiKey string) (requestErr error) {
	defer func() {
		if requestErr != nil {
			UpstreamTransportObserver(ctx).Failure("gateway", "identity_validation", 0)
		}
	}()
	if err := ValidateBackgroundAccountMatch(ctx, account); err != nil {
		return err
	}
	if outboundEpochFromContext(ctx).identityKey() != "" && !fingerprint.PreservesSessionIdentity() {
		return codexAccountIdentityError("当前迁移会话需要账号级身份模式，不支持 legacy 出站配置。")
	}
	if !fingerprint.PreservesSessionIdentity() || ctx == nil || account == nil {
		return nil
	}
	claimer, _ := ctx.Value(codexIdentityClaimerContextKey{}).(codexIdentityClaimer)
	if claimer == nil {
		if fingerprint.accountIdentityRequested {
			for _, value := range fingerprint.identityValues {
				if strings.TrimSpace(value) != "" {
					return codexAccountIdentityError("账号级出站身份需要持久化存储，当前无法使用。")
				}
			}
		}
		return nil
	}
	owner := verifiedTransportUser(ctx)
	if owner == "" {
		owner = "credential:" + strings.TrimSpace(apiKey)
		if strings.TrimSpace(apiKey) == "" {
			if contextOwner, ok := ctx.Value(codexAnonymousIdentityContextKey{}).(string); ok && contextOwner != "" {
				owner = contextOwner
			} else if connectionID := DownstreamWebsocketConnectionID(ctx); connectionID != "" {
				owner = "anonymous-connection:" + connectionID
			} else {
				owner = "anonymous:" + NewUpstreamSessionUUID()
			}
		}
	}
	owner = codexIdentityDigest("codex-owner-v1", owner)
	if mapping := fingerprint.accountIdentity; mapping != nil && (mapping.owner != owner || mapping.account != strings.TrimSpace(account.EffectiveAccountID()) || mapping.epoch != outboundEpochFromContext(ctx).identityKey()) {
		return codexAccountIdentityError("出站身份快照与当前账号或用户不一致，已停止请求。")
	}
	account.Mu().RLock()
	upstreamAccount := strings.TrimSpace(account.AccountID)
	account.Mu().RUnlock()
	accountScopes := []string{fmt.Sprintf("account:%d", account.ID())}
	if upstreamAccount != "" {
		accountScopes = append(accountScopes, "chatgpt:"+upstreamAccount)
	}
	if effectiveAccount := strings.TrimSpace(account.EffectiveAccountID()); effectiveAccount != "" && effectiveAccount != upstreamAccount {
		accountScopes = append(accountScopes, "chatgpt:"+effectiveAccount)
	}
	keys := make([]string, 0, 16)
	seen := make(map[string]bool)
	for _, value := range fingerprint.identityValues {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		if len(value) > 512 {
			return &Error{Code: "codex_session_identity_invalid", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "会话标识过长，请检查客户端请求。"}
		}
		for _, scope := range accountScopes {
			keys = append(keys, codexIdentityDigest("codex-session-v1", scope, value))
		}
	}
	if len(keys) == 0 {
		return nil
	}
	claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if fingerprint.accountIdentity == nil {
		if err := fingerprint.prepareAccountIdentity(claimCtx, account, owner, accountScopes); err != nil {
			return err
		}
	}
	if err := claimer.ClaimCodexIdentities(claimCtx, keys, owner); err != nil {
		if errors.Is(err, database.ErrCodexIdentityConflict) {
			return &Error{Code: "codex_session_identity_conflict", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "当前会话标识已归属其他用户，请新建会话。"}
		}
		return &Error{Code: "codex_session_identity_unavailable", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "暂时无法核实会话归属，请稍后重试。", Cause: err}
	}
	return nil
}
