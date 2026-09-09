package admin

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func (h *Handler) promptWindowOperationSubject(c *gin.Context, ctx context.Context) (*database.PromptRiskProfile, string, bool) {
	sessionHash := strings.ToLower(strings.TrimSpace(c.Param("session_hash")))
	decoded, err := hex.DecodeString(sessionHash)
	if c.Param("subject_type") != database.PromptRiskSubjectNewAPIUser || strings.TrimSpace(c.Param("subject_key")) == "" || err != nil || len(decoded) != 12 {
		writeError(c, http.StatusBadRequest, "用户会话窗口标识无效")
		return nil, "", false
	}
	profiles, _, err := h.db.ListPromptRiskProfiles(ctx, database.PromptRiskProfileQuery{SubjectType: database.PromptRiskSubjectNewAPIUser, SubjectKey: strings.TrimSpace(c.Param("subject_key")), Page: 1, PageSize: 1})
	if err != nil {
		writeInternalError(c, err)
		return nil, "", false
	}
	if len(profiles) != 1 || profiles[0].NewAPIUserID == "" || profiles[0].Platform == "" {
		writeError(c, http.StatusNotFound, "用户画像不存在")
		return nil, "", false
	}
	return profiles[0], sessionHash, true
}

func (h *Handler) LockPromptUserWindow(c *gin.Context) {
	var request struct {
		WindowExpiresAt time.Time `json:"window_expires_at"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.WindowExpiresAt.IsZero() {
		writeError(c, http.StatusBadRequest, "请提供当前窗口的到期时间并刷新确认")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	profile, sessionHash, ok := h.promptWindowOperationSubject(c, ctx)
	if !ok {
		return
	}
	now := time.Now().UTC()
	found := false
	for _, window := range h.promptRiskSessionWindows(ctx, profile, now) {
		if window.SessionHash == sessionHash && window.ExpiresAt.Equal(request.WindowExpiresAt) {
			found = true
			break
		}
	}
	if !found {
		writeError(c, http.StatusConflict, "窗口已到期或发生变化，请刷新后重试")
		return
	}
	ttl, _ := h.promptConversationRestrictionTTLs()
	item, err := h.db.LockPromptUserWindow(ctx, profile.Platform, profile.NewAPIUserID, sessionHash, now, ttl)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"lock": item})
}

func (h *Handler) UnlockPromptUserWindow(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	profile, sessionHash, ok := h.promptWindowOperationSubject(c, ctx)
	if !ok {
		return
	}
	err := h.db.UnlockPromptUserWindow(ctx, profile.Platform, profile.NewAPIUserID, sessionHash, time.Now().UTC())
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "该会话未被手动锁定或已解锁")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) attachPromptWindowAccountErrors(ctx context.Context, profile *database.PromptRiskProfile, windows []promptRiskSessionWindowResponse) error {
	if len(windows) == 0 {
		return nil
	}
	items, err := h.db.ListPromptSessionAccountErrors(ctx, profile.Platform, profile.NewAPIUserID, time.Now().UTC())
	if err != nil {
		return err
	}
	latest := make(map[string]time.Time, len(items))
	for _, item := range items {
		latest[item.SessionHash+":"+strconv.FormatInt(item.AccountID, 10)+":"+strconv.FormatInt(item.WindowExpiresAt, 10)] = item.Last500At
	}
	for index := range windows {
		window := &windows[index]
		at, found := latest[window.SessionHash+":"+strconv.FormatInt(window.AccountID, 10)+":"+strconv.FormatInt(window.ExpiresAt.UnixNano(), 10)]
		if found {
			window.Last500At = &at
		}
	}
	return nil
}
