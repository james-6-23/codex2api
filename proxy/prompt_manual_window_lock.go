package proxy

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const promptManualWindowLockedCode = api.ErrorCode("conversation_manually_locked")

func (h *Handler) promptManualWindowLockError(c *gin.Context, cfg promptfilter.Config, rootBody, signedBody []byte) *api.APIError {
	if h == nil || h.db == nil || c == nil {
		return nil
	}
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified || !policyContext.MetaVerified || strings.TrimSpace(policyContext.Identity.UserID) == "" {
		return nil
	}
	root := h.resolveRequestRootSessionIdentityForContext(c, rootBody)
	sessionID := ""
	if root.stable && !root.conflict {
		sessionID = strings.TrimSpace(root.sessionID)
	}
	if sessionID == "" && !root.authoritative && c.Request != nil {
		sessionID = ResolveStableExplicitSessionID(c.Request.Header, rootBody)
	}
	if sessionID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	_, err := h.db.GetActivePromptUserWindowLock(ctx, policyContext.Platform, policyContext.Identity.UserID, hashRiskIdentity(sessionID), time.Now().UTC())
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		log.Printf("check manual conversation lock failed: %v", err)
		return api.NewAPIError(api.ErrCodeServiceUnavailable, "暂时无法确认会话状态，请稍后重试", api.ErrorTypeServer)
	}
	return api.NewAPIError(promptManualWindowLockedCode, "当前会话已由管理员锁定，请联系管理员解锁或等待锁定到期。", api.ErrorTypeInvalidRequest)
}
