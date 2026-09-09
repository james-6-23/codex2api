package admin

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type accountLiveItem struct {
	ActiveRequests                 int64 `json:"active_requests"`
	OccupiedRequests               int64 `json:"occupied_requests"`
	SessionCapacityCurrent         int64 `json:"session_capacity_current"`
	SessionCapacityMax             int64 `json:"session_capacity_max"`
	SessionCapacityReserved        int64 `json:"session_capacity_reserved"`
	SessionCapacityReservedCurrent int64 `json:"session_capacity_reserved_current"`
}

type accountSessionResponse struct {
	auth.AccountSessionSnapshot
	UserSessionUsage *database.SessionUsageStats `json:"user_session_usage,omitempty"`
}

// GetAccountSessions lazily exposes active session slots for one account.
func (h *Handler) GetAccountSessions(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 || h.store.FindByID(id) == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	snapshots := h.store.AccountSessionSnapshots(id, time.Now())
	users := make([]database.SessionUsageUser, 0, len(snapshots))
	for _, snapshot := range snapshots {
		users = append(users, database.SessionUsageUser{Platform: snapshot.Owner.Platform, UserID: snapshot.Owner.UserID})
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	stats, err := h.db.GetNewAPIUsersSessionUsage(ctx, users)
	if err != nil {
		log.Printf("读取账号窗口用户平均用时失败: account_id=%d err=%v", id, err)
	}
	sessions := make([]accountSessionResponse, 0, len(snapshots))
	for _, snapshot := range snapshots {
		user := database.SessionUsageUser{Platform: strings.TrimSpace(snapshot.Owner.Platform), UserID: strings.TrimSpace(snapshot.Owner.UserID)}
		sessions = append(sessions, accountSessionResponse{AccountSessionSnapshot: snapshot, UserSessionUsage: stats[user]})
	}
	c.JSON(http.StatusOK, gin.H{"sessions": sessions})
}

// DeleteAccountSessions releases either one named session or every session of
// the account. It never interrupts requests already in flight.
func (h *Handler) DeleteAccountSessions(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 || h.store.FindByID(id) == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	sessionID := strings.TrimSpace(c.Query("session_id"))
	if sessionID == "" {
		for _, releasedID := range h.store.ClearAccountSessions(id) {
			h.store.UnbindSessionAffinity(releasedID, id)
		}
		c.JSON(http.StatusOK, gin.H{"message": "已释放全部会话槽"})
		return
	}
	if !h.store.RemoveAccountSession(id, sessionID) {
		writeError(c, http.StatusNotFound, "会话不存在或已释放")
		return
	}
	h.store.UnbindSessionAffinity(sessionID, id)
	c.JSON(http.StatusOK, gin.H{"message": "会话槽已释放"})
}

// GetAccountLiveState returns request-local runtime counters for the visible
// account page. It intentionally reads only in-memory atomics, so frequent UI
// polling does not touch the database or rebuild the paged account snapshot.
func (h *Handler) GetAccountLiveState(c *gin.Context) {
	ids, err := parseAccountListIDs(c.Query("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "ids 参数无效")
		return
	}
	if len(ids) > accountListPageMax {
		writeError(c, http.StatusBadRequest, "ids 最多允许 500 个")
		return
	}

	live := make(map[int64]accountLiveItem, len(ids))
	now := time.Now()
	for _, id := range ids {
		account := h.store.FindByID(id)
		if account == nil {
			continue
		}
		capacityEnabled, capacityMax, _ := account.SessionCapacityConfig()
		capacityCurrent := int64(0)
		reservedCurrent := int64(0)
		if capacityEnabled {
			capacityCurrent, reservedCurrent = h.store.AccountSessionSlotCounts(id, now)
		}
		live[id] = accountLiveItem{
			ActiveRequests:                 account.GetActiveRequests(),
			OccupiedRequests:               account.GetOccupiedRequests(),
			SessionCapacityCurrent:         capacityCurrent,
			SessionCapacityMax:             capacityMax,
			SessionCapacityReserved:        account.SessionCapacityLimits().Reserved,
			SessionCapacityReservedCurrent: reservedCurrent,
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"accounts":                    live,
		"session_slot_buffer_enabled": h.store.SessionSlotBufferEnabled(),
	})
}
