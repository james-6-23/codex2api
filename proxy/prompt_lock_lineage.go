package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func (handler *Handler) promptLockLineageKeys(request *gin.Context, body []byte, policy verifiedNewAPIPolicyContext, verified bool, kind, ownKey string) ([]string, *api.APIError) {
	root := handler.resolveRequestRootSessionIdentityForContext(request, body)
	handler.captureSessionOperationsIdentity(request, body, root, policy, verified)
	identity, known := sessionOperationsIdentity(request)
	if !known {
		return []string{ownKey}, nil
	}
	ctx, cancel := context.WithTimeout(request.Request.Context(), 2*time.Second)
	defer cancel()
	if ownKey != "" {
		if err := handler.db.RecordSessionPolicyLockIdentity(ctx, identity.Key, kind, ownKey); err != nil {
			return nil, promptLockLineageError(err)
		}
	}
	entries := make(map[string]string)
	if kind == "window" && identity.ParentKey != "" {
		parent := root.forkedFromSessionID
		if verified {
			parent = policy.Meta.ForkedFromSessionFingerprint
			if parent == "" && root.forkedFromSessionID != "" {
				parent = newAPIRootSessionFingerprint(policy.Platform, policy.Identity.UserID, root.forkedFromSessionID)
			}
		}
		if parent != "" {
			entries[identity.ParentKey] = hashRiskIdentity(parent)
		}
	}
	if kind == "cyber" && !verified && identity.ParentKey != "" {
		if lock, ok := promptConversationLockFallbackIdentityForSession(requestAPIKeyID(request), root.forkedFromSessionID); ok {
			entries[identity.ParentKey] = lock.LockKey
		}
	}
	if kind == "cyber" && verified && policy.VerificationSecret != "" {
		headers := request.Request.Header
		if isResponsesWebSocketUpgradeRequest(request.Request) {
			headers = nil
		}
		native := resolveRequestRootSessionIdentity(headers, body)
		parentFingerprint := policy.Meta.ForkedFromSessionFingerprint
		if parentFingerprint == "" && root.forkedFromSessionID != "" {
			parentFingerprint = newAPIRootSessionFingerprint(policy.Platform, policy.Identity.UserID, root.forkedFromSessionID)
		}
		references := []struct{ key, original, fingerprint string }{
			{identity.Key, policy.Meta.RootSessionID, root.fingerprint},
			{identity.ParentKey, native.forkedFromSessionID, parentFingerprint},
		}
		for _, reference := range references {
			if reference.key == "" || reference.original == "" || !equalRootSessionFingerprint(reference.fingerprint, newAPIRootSessionFingerprint(policy.Platform, policy.Identity.UserID, reference.original)) {
				continue
			}
			mac := hmac.New(sha256.New, []byte(policy.VerificationSecret))
			_, _ = mac.Write([]byte(strings.Join([]string{"policy-session-v1", strings.TrimSpace(policy.Platform), strings.TrimSpace(policy.Identity.UserID), strings.TrimSpace(reference.original)}, "\n")))
			parentPolicy := policy
			parentPolicy.Meta.SessionFingerprint = hex.EncodeToString(mac.Sum(nil))[:32]
			if lock, ok := verifiedPromptConversationLockIdentity(request, parentPolicy); ok {
				entries[reference.key] = lock.LockKey
			}
		}
	}
	for key, value := range entries {
		if err := handler.db.RecordSessionPolicyLockIdentity(ctx, key, kind, value); err != nil {
			return nil, promptLockLineageError(err)
		}
	}
	keys, err := handler.db.SessionPolicyLockKeys(ctx, identity.Key, identity.ParentKey, kind)
	if err != nil {
		return nil, promptLockLineageError(err)
	}
	return keys, nil
}

func promptLockLineageError(err error) *api.APIError {
	if errors.Is(err, database.ErrSessionLineageConflict) {
		return api.NewAPIError("session_lineage_invalid", "会话派生关系冲突或过深，已停止请求，请联系管理员。", api.ErrorTypeInvalidRequest)
	}
	return api.NewAPIError(api.ErrCodeServiceUnavailable, "暂时无法确认父会话锁定状态，已停止请求，请稍后重试。", api.ErrorTypeServer)
}

func (handler *Handler) requestPromptConversationLock(request *gin.Context, cfg promptfilter.Config, rootBody, signedBody []byte, endpoint, model string) (*database.PromptConversationLock, bool, *api.APIError) {
	if handler.db != nil && cfg.Advanced.Enforcement.ConversationLockEnabled {
		policy, verified := handler.verifyNewAPIPolicyContext(request, cfg.Advanced.NewAPI, signedBody)
		identity, known := verifiedPromptConversationLockIdentity(request, policy)
		if !verified {
			identity, known = promptConversationLockFallbackIdentity(request)
		}
		directLockKey := identity.LockKey
		if !known && !verified {
			root := handler.resolveRequestRootSessionIdentityForContext(request, rootBody)
			if root.stable && !root.conflict {
				identity, known = promptConversationLockFallbackIdentityForSession(requestAPIKeyID(request), root.sessionID)
			}
		}
		if known || verified && policy.MetaVerified {
			keys, failure := handler.promptLockLineageKeys(request, rootBody, policy, verified, "cyber", identity.LockKey)
			if failure != nil {
				return nil, false, failure
			}
			ctx, cancel := context.WithTimeout(request.Request.Context(), 2*time.Second)
			defer cancel()
			for _, key := range keys {
				if key == directLockKey {
					continue
				}
				lock, err := handler.db.GetActivePromptConversationLockWithTTL(ctx, key, promptConversationLockTTL(cfg))
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return nil, false, promptLockLineageError(err)
				}
				return promptCyberRestrictionLock(lock, true), true, nil
			}
		}
	}
	lock, active := handler.activePromptConversationLock(request, cfg, signedBody, endpoint, model)
	return lock, active, nil
}
