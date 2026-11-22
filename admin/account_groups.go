package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	maxAccountGroups            = 64
	maxAccountGroupNameRuneSize = 80
)

type accountGroupResponse struct {
	ID                      int64    `json:"id"`
	Name                    string   `json:"name"`
	Description             string   `json:"description"`
	Color                   string   `json:"color"`
	SortOrder               int64    `json:"sort_order"`
	BaseConcurrencyOverride *int64   `json:"base_concurrency_override"`
	MemberCount             int64    `json:"member_count"`
	AutoPause5hThreshold    float64  `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold    float64  `json:"auto_pause_7d_threshold"`
	ProxyURLs               []string `json:"proxy_urls"`
	Channel                 string   `json:"channel"`
	CreatedAt               string   `json:"created_at"`
	UpdatedAt               string   `json:"updated_at"`
}

func toAccountGroupResponse(g database.AccountGroup) accountGroupResponse {
	proxyURLs := g.ProxyURLs
	if proxyURLs == nil {
		proxyURLs = []string{}
	}
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
		ProxyURLs:               proxyURLs,
		Channel:                 database.NormalizeAccountGroupChannel(g.Channel),
		CreatedAt:               g.CreatedAt.Format(time.RFC3339),
		UpdatedAt:               g.UpdatedAt.Format(time.RFC3339),
	}
}

const maxAccountGroupProxyURLs = 64

// sanitizeAccountGroupProxyURLs 去空行/去重并逐条校验 scheme(http/https/socks5)。
func sanitizeAccountGroupProxyURLs(urls []string) ([]string, error) {
	cleaned := make([]string, 0, len(urls))
	seen := make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		if err := security.ValidateProxyURL(u); err != nil {
			return nil, err
		}
		seen[u] = struct{}{}
		cleaned = append(cleaned, u)
	}
	if len(cleaned) > maxAccountGroupProxyURLs {
		return nil, errors.New("分组代理数量不能超过 64 条")
	}
	return cleaned, nil
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
	ProxyURLs               []string        `json:"proxy_urls"`
	Channel                 string          `json:"channel"`
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
	// 最小 1，无上限（与全局 max_concurrency / 账号级覆盖一致）
	return parseOptionalIntegerField(raw, "base_concurrency_override", 1, math.MaxInt64)
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
	proxyURLs, err := sanitizeAccountGroupProxyURLs(req.ProxyURLs)
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
	channel := database.NormalizeAccountGroupChannel(req.Channel)
	if len(proxyURLs) > 0 || channel != database.AccountGroupChannelCodex {
		opts := &database.UpdateAccountGroupOpts{Channel: &channel}
		if len(proxyURLs) > 0 {
			opts.ProxyURLs = &proxyURLs
		}
		if err := h.db.UpdateAccountGroup(ctx, id, nil, nil, nil, opts); err != nil {
			writeInternalError(c, err)
			return
		}
		if h.store != nil && len(proxyURLs) > 0 {
			h.store.SetGroupProxyURLs(id, proxyURLs)
		}
	}
	if h.store != nil && (req.AutoPause5hThreshold > 0 || req.AutoPause7dThreshold > 0) {
		h.store.SetGroupAutoPauseThresholds(id, req.AutoPause5hThreshold, req.AutoPause7dThreshold)
	}
	if h.store != nil && baseConcurrencyOverride.Value.Valid {
		value := baseConcurrencyOverride.Value.Int64
		h.store.SetGroupBaseConcurrencyOverride(id, &value)
	}
	if h.store != nil {
		h.store.SetGroupName(id, name)
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
	// ProxyURLs 缺省(null)表示不修改;传空数组表示清空组代理。
	ProxyURLs *[]string `json:"proxy_urls"`
	// Channel 缺省表示不修改;仅空组允许改渠道。
	Channel *string `json:"channel"`
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
	if req.ProxyURLs != nil {
		cleaned, err := sanitizeAccountGroupProxyURLs(*req.ProxyURLs)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		req.ProxyURLs = &cleaned
	}
	if req.Channel != nil {
		normalized := database.NormalizeAccountGroupChannel(*req.Channel)
		req.Channel = &normalized
	}
	var opts *database.UpdateAccountGroupOpts
	if req.AutoPause5hThreshold != nil || req.AutoPause7dThreshold != nil || baseConcurrencyOverride.Set || req.ProxyURLs != nil || req.Channel != nil {
		opts = &database.UpdateAccountGroupOpts{
			AutoPause5hThreshold:    req.AutoPause5hThreshold,
			AutoPause7dThreshold:    req.AutoPause7dThreshold,
			BaseConcurrencyOverride: baseConcurrencyOverride,
			ProxyURLs:               req.ProxyURLs,
			Channel:                 req.Channel,
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if req.Channel != nil {
		// 渠道只在空组上可改:带成员改渠道会让存量成员瞬间跨渠道违规。
		groups, err := h.db.ListAccountGroups(ctx)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		for _, group := range groups {
			if group.ID == id && database.NormalizeAccountGroupChannel(group.Channel) != *req.Channel && group.MemberCount > 0 {
				writeError(c, http.StatusConflict, "分组内还有账号,请先移出成员再切换渠道")
				return
			}
		}
	}
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
	if req.ProxyURLs != nil && h.store != nil {
		h.store.SetGroupProxyURLs(id, *req.ProxyURLs)
	}
	if req.Name != nil && h.store != nil {
		h.store.SetGroupName(id, *req.Name)
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
		h.store.DeleteGroupProxyURLs(id)
		h.store.DeleteGroupName(id)
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

// accountRowGroupChannel 返回账号所属的分组渠道。
func accountRowGroupChannel(row *database.AccountRow) string {
	if row != nil && strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamAntigravity) {
		return database.AccountGroupChannelAntigravity
	}
	if row != nil && strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamClaude) {
		return database.AccountGroupChannelClaude
	}
	if isGrokAccountRow(row) {
		return database.AccountGroupChannelGrok
	}
	return database.AccountGroupChannelCodex
}

// validateGroupChannelForRows 校验目标分组的渠道与账号平台一致(issue #487):
// Codex 与 Grok 账号不得进入同一个分组。groupIDs 为空直接放行。
func (h *Handler) validateGroupChannelForRows(ctx context.Context, rows []*database.AccountRow, groupIDs []int64) error {
	if len(groupIDs) == 0 || len(rows) == 0 {
		return nil
	}
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		return fmt.Errorf("校验分组渠道失败: %w", err)
	}
	channelByID := make(map[int64]string, len(groups))
	nameByID := make(map[int64]string, len(groups))
	for _, group := range groups {
		channelByID[group.ID] = database.NormalizeAccountGroupChannel(group.Channel)
		nameByID[group.ID] = group.Name
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		accountChannel := accountRowGroupChannel(row)
		for _, groupID := range groupIDs {
			groupChannel, ok := channelByID[groupID]
			if !ok {
				continue // 不存在的分组由既有校验负责报错
			}
			if groupChannel != accountChannel {
				return fmt.Errorf("分组「%s」是 %s 渠道分组,不能加入 %s 账号(账号 %d)",
					nameByID[groupID], groupChannelDisplayName(groupChannel), groupChannelDisplayName(accountChannel), row.ID)
			}
		}
	}
	return nil
}

func groupChannelDisplayName(channel string) string {
	switch channel {
	case database.AccountGroupChannelGrok:
		return "Grok"
	case database.AccountGroupChannelAntigravity:
		return "Antigravity"
	case database.AccountGroupChannelClaude:
		return "Claude"
	default:
		return "Codex"
	}
}
