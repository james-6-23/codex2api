package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	maxAccountGroups            = 64
	maxAccountGroupNameRuneSize = 80
)

type accountGroupResponse struct {
	ID                      int64   `json:"id"`
	Name                    string  `json:"name"`
	Description             string  `json:"description"`
	Color                   string  `json:"color"`
	SortOrder               int64   `json:"sort_order"`
	BaseConcurrencyOverride *int64  `json:"base_concurrency_override"`
	MemberCount             int64   `json:"member_count"`
	AutoPause5hThreshold    float64 `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold    float64 `json:"auto_pause_7d_threshold"`
	CreatedAt               string  `json:"created_at"`
	UpdatedAt               string  `json:"updated_at"`
}

func toAccountGroupResponse(g database.AccountGroup) accountGroupResponse {
	return accountGroupResponse{
		ID:                      g.ID,
		Name:                    g.Name,
		Description:             g.Description,
		Color:                   g.Color,
		SortOrder:               g.SortOrder,
		BaseConcurrencyOverride: nullableInt64Pointer(g.BaseConcurrencyOverride),
		MemberCount:             g.MemberCount,
		AutoPause5hThreshold:    g.AutoPause5hThreshold,
		AutoPause7dThreshold:    g.AutoPause7dThreshold,
		CreatedAt:               g.CreatedAt.Format(time.RFC3339),
		UpdatedAt:               g.UpdatedAt.Format(time.RFC3339),
	}
}

func (h *Handler) ListAccountGroups(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	out := make([]accountGroupResponse, 0, len(groups))
	for _, group := range groups {
		out = append(out, toAccountGroupResponse(group))
	}
	c.JSON(http.StatusOK, gin.H{"groups": out})
}

type createAccountGroupReq struct {
	Name                    string          `json:"name"`
	Description             string          `json:"description"`
	Color                   string          `json:"color"`
	SortOrder               *int64          `json:"sort_order"`
	BaseConcurrencyOverride json.RawMessage `json:"base_concurrency_override"`
	AutoPause5hThreshold    float64         `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold    float64         `json:"auto_pause_7d_threshold"`
}

func validateAutoPauseThreshold(name string, value float64) error {
	if value < 0 || value > 1 {
		return errors.New(name + " 需在 0 到 1 之间")
	}
	return nil
}

func parseAccountGroupBaseConcurrencyOverride(raw json.RawMessage) (database.OptionalNullInt64, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		return database.OptionalNullInt64{}, errors.New("base_concurrency_override 必须是整数或 null")
	}
	return parseOptionalIntegerField(raw, "base_concurrency_override", 1, 50)
}

func (h *Handler) CreateAccountGroup(c *gin.Context) {
	var req createAccountGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	name, err := sanitizeAccountGroupName(req.Name)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	description := strings.TrimSpace(req.Description)
	if utf8.RuneCountInString(description) > 240 {
		writeError(c, http.StatusBadRequest, "描述长度不能超过 240 字符")
		return
	}
	color := strings.TrimSpace(req.Color)
	if utf8.RuneCountInString(color) > 20 {
		writeError(c, http.StatusBadRequest, "颜色长度不能超过 20 字符")
		return
	}
	sortOrder := int64(0)
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	if err := validateAutoPauseThreshold("auto_pause_5h_threshold", req.AutoPause5hThreshold); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateAutoPauseThreshold("auto_pause_7d_threshold", req.AutoPause7dThreshold); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	baseConcurrencyOverride, err := parseAccountGroupBaseConcurrencyOverride(req.BaseConcurrencyOverride)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if len(groups) >= maxAccountGroups {
		writeError(c, http.StatusBadRequest, "分组数量已达上限")
		return
	}
	id, err := h.db.CreateAccountGroup(ctx, name, description, color, req.AutoPause5hThreshold, req.AutoPause7dThreshold, baseConcurrencyOverride.Value, sortOrder)
	if err != nil {
		if errors.Is(err, database.ErrDuplicateAccountGroupName) {
			writeError(c, http.StatusConflict, err.Error())
			return
		}
		writeInternalError(c, err)
		return
	}
	if h.store != nil && (req.AutoPause5hThreshold > 0 || req.AutoPause7dThreshold > 0) {
		h.store.SetGroupAutoPauseThresholds(id, req.AutoPause5hThreshold, req.AutoPause7dThreshold)
	}
	if h.store != nil && baseConcurrencyOverride.Value.Valid {
		value := baseConcurrencyOverride.Value.Int64
		h.store.SetGroupBaseConcurrencyOverride(id, &value)
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "message": "分组已创建"})
}

type updateAccountGroupReq struct {
	Name                    *string         `json:"name"`
	Description             *string         `json:"description"`
	Color                   *string         `json:"color"`
	SortOrder               *int64          `json:"sort_order"`
	BaseConcurrencyOverride json.RawMessage `json:"base_concurrency_override"`
	AutoPause5hThreshold    *float64        `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold    *float64        `json:"auto_pause_7d_threshold"`
}

func (h *Handler) UpdateAccountGroup(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的分组 ID")
		return
	}
	var req updateAccountGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Name != nil {
		name, err := sanitizeAccountGroupName(*req.Name)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		req.Name = &name
	}
	if req.Description != nil {
		desc := strings.TrimSpace(*req.Description)
		if utf8.RuneCountInString(desc) > 240 {
			writeError(c, http.StatusBadRequest, "描述长度不能超过 240 字符")
			return
		}
		req.Description = &desc
	}
	if req.Color != nil {
		color := strings.TrimSpace(*req.Color)
		if utf8.RuneCountInString(color) > 20 {
			writeError(c, http.StatusBadRequest, "颜色长度不能超过 20 字符")
			return
		}
		req.Color = &color
	}
	if req.AutoPause5hThreshold != nil {
		if err := validateAutoPauseThreshold("auto_pause_5h_threshold", *req.AutoPause5hThreshold); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.AutoPause7dThreshold != nil {
		if err := validateAutoPauseThreshold("auto_pause_7d_threshold", *req.AutoPause7dThreshold); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
	}
	baseConcurrencyOverride, err := parseAccountGroupBaseConcurrencyOverride(req.BaseConcurrencyOverride)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	var opts *database.UpdateAccountGroupOpts
	if req.AutoPause5hThreshold != nil || req.AutoPause7dThreshold != nil || baseConcurrencyOverride.Set {
		opts = &database.UpdateAccountGroupOpts{
			AutoPause5hThreshold:    req.AutoPause5hThreshold,
			AutoPause7dThreshold:    req.AutoPause7dThreshold,
			BaseConcurrencyOverride: baseConcurrencyOverride,
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.db.UpdateAccountGroup(ctx, id, req.Name, req.Description, req.Color, opts, req.SortOrder); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "分组不存在")
			return
		}
		if errors.Is(err, database.ErrDuplicateAccountGroupName) {
			writeError(c, http.StatusConflict, err.Error())
			return
		}
		writeInternalError(c, err)
		return
	}
	if opts != nil && h.store != nil && (opts.AutoPause5hThreshold != nil || opts.AutoPause7dThreshold != nil) {
		t5h, t7d := h.store.GetGroupAutoPauseThresholds(id)
		if opts.AutoPause5hThreshold != nil {
			t5h = *opts.AutoPause5hThreshold
		}
		if opts.AutoPause7dThreshold != nil {
			t7d = *opts.AutoPause7dThreshold
		}
		h.store.SetGroupAutoPauseThresholds(id, t5h, t7d)
	}
	if baseConcurrencyOverride.Set && h.store != nil {
		h.store.SetGroupBaseConcurrencyOverride(id, nullableInt64Pointer(baseConcurrencyOverride.Value))
	}
	writeMessage(c, http.StatusOK, "分组已更新")
}

func (h *Handler) DeleteAccountGroup(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的分组 ID")
		return
	}
	force := strings.EqualFold(c.Query("force"), "true")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.db.DeleteAccountGroup(ctx, id, force); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "分组不存在")
			return
		}
		if errors.Is(err, database.ErrAccountGroupNotEmpty) {
			writeError(c, http.StatusConflict, err.Error())
			return
		}
		writeInternalError(c, err)
		return
	}
	if h.store != nil {
		h.store.DeleteGroupAutoPauseThresholds(id)
		h.store.DeleteGroupBaseConcurrencyOverride(id)
		for _, acc := range h.store.Accounts() {
			acc.Mu().RLock()
			groups := removeInt64(acc.GroupIDs, id)
			acc.Mu().RUnlock()
			h.store.ApplyAccountGroups(acc.DBID, groups)
		}
	}
	h.refreshAPIKeyAllowedGroupsAfterGroupDelete(ctx, id)
	writeMessage(c, http.StatusOK, "分组已删除")
}

func (h *Handler) refreshAPIKeyAllowedGroupsAfterGroupDelete(ctx context.Context, groupID int64) {
	if h == nil || h.db == nil || groupID <= 0 {
		return
	}
	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		return
	}
	for _, key := range keys {
		if key == nil {
			continue
		}
		if h.store != nil {
			h.store.SetAPIKeyAllowedGroups(key.ID, key.AllowedGroupIDs)
		}
		h.invalidateAPIKeyRuntimeCaches(ctx, key.Key)
	}
}

func sanitizeAccountGroupName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("分组名称不能为空")
	}
	if utf8.RuneCountInString(name) > maxAccountGroupNameRuneSize {
		return "", errors.New("分组名称长度超过 80 字符")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("分组名称包含非法控制字符")
		}
	}
	return name, nil
}

func removeInt64(slice []int64, target int64) []int64 {
	out := make([]int64, 0, len(slice))
	for _, v := range slice {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

func containsInt64(slice []int64, target int64) bool {
	for _, v := range slice {
		if v == target {
			return true
		}
	}
	return false
}

func dedupeInt64(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
