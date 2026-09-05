package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	publicAPIKeyUsageDefaultLogPageSize = 25
	publicAPIKeyUsageMaxLogPageSize     = 100
)

type publicAPIKeyUsageResponse struct {
	Key               publicAPIKeyUsageKey               `json:"key"`
	Range             publicAPIKeyUsageRange             `json:"range"`
	Usage             *database.APIKeySelfUsageReport    `json:"usage"`
	ModelRequestUsage []database.APIKeyModelRequestUsage `json:"model_request_usage"`
}

type publicAPIKeyUsageKey struct {
	Name        string                `json:"name"`
	Key         string                `json:"key"`
	QuotaLimit  float64               `json:"quota_limit"`
	QuotaUsed   float64               `json:"quota_used"`
	TotalUsed   float64               `json:"total_used"`
	ResetCount  int                   `json:"reset_count"`
	LastResetAt *string               `json:"last_reset_at,omitempty"`
	ExpiresAt   *string               `json:"expires_at,omitempty"`
	Limits      database.APIKeyLimits `json:"limits"`
	Status      string                `json:"status"`
	CreatedAt   string                `json:"created_at"`
}

type publicAPIKeyUsageRange struct {
	Name  string  `json:"name"`
	Start *string `json:"start,omitempty"`
	End   string  `json:"end"`
}

func (h *Handler) GetPublicAPIKeyUsageSummary(c *gin.Context) {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "服务未就绪")
		return
	}
	enabled, err := h.PublicAPIKeyUsagePageEnabled(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if !enabled {
		writeError(c, http.StatusNotFound, "API Key 自助用量页未启用")
		return
	}
	key := extractPublicAPIKey(c)
	if key == "" {
		writeError(c, http.StatusUnauthorized, "缺少 Authorization Bearer API Key")
		return
	}

	rangeName, rangeStart, rangeEnd, err := parsePublicAPIKeyUsageRange(c.Query("range"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	logPage, logPageSize, err := parsePublicAPIKeyUsageLogPagination(c.Query("page"), c.Query("page_size"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()

	row, err := h.db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			security.SecurityAuditLog("KEY_USAGE_AUTH_FAILED", "ip="+security.SanitizeLog(c.ClientIP())+" key="+security.MaskAPIKey(key))
			writeError(c, http.StatusUnauthorized, "API Key 无效或不存在")
			return
		}
		writeInternalError(c, err)
		return
	}

	report, err := h.db.GetAPIKeySelfUsageReport(ctx, row.ID, rangeStart, rangeEnd, logPage, logPageSize)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	modelRequestUsage, err := h.db.GetAPIKeyModelRequestUsage(ctx, row.ID, row.Limits.ModelRequestLimits, rangeEnd)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if modelRequestUsage == nil {
		modelRequestUsage = []database.APIKeyModelRequestUsage{}
	}

	c.JSON(http.StatusOK, publicAPIKeyUsageResponse{
		Key:               newPublicAPIKeyUsageKey(row),
		Range:             newPublicAPIKeyUsageRange(rangeName, rangeStart, rangeEnd),
		Usage:             report,
		ModelRequestUsage: modelRequestUsage,
	})
}

func (h *Handler) PublicAPIKeyUsagePageEnabled(ctx context.Context) (bool, error) {
	if h == nil || h.db == nil {
		return false, nil
	}
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings == nil {
		return true, nil
	}
	return settings.PublicKeyUsagePageEnabled, nil
}

func extractPublicAPIKey(c *gin.Context) string {
	if c == nil {
		return ""
	}
	authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
	if authHeader != "" {
		fields := strings.Fields(authHeader)
		if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
			return strings.TrimSpace(fields[1])
		}
	}
	for _, header := range []string{"x-api-key", "anthropic-auth-token"} {
		if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
			return value
		}
	}
	return ""
}

func parsePublicAPIKeyUsageLogPagination(pageRaw, pageSizeRaw string) (int, int, error) {
	page := 1
	pageRaw = strings.TrimSpace(pageRaw)
	if pageRaw != "" {
		parsed, err := strconv.Atoi(pageRaw)
		if err != nil || parsed < 1 {
			return 0, 0, errors.New("page 参数无效，需要正整数")
		}
		page = parsed
	}

	pageSize := publicAPIKeyUsageDefaultLogPageSize
	pageSizeRaw = strings.TrimSpace(pageSizeRaw)
	if pageSizeRaw != "" {
		parsed, err := strconv.Atoi(pageSizeRaw)
		if err != nil || parsed < 1 {
			return 0, 0, errors.New("page_size 参数无效，需要正整数")
		}
		if parsed > publicAPIKeyUsageMaxLogPageSize {
			return 0, 0, errors.New("page_size 最大支持 100")
		}
		pageSize = parsed
	}

	return page, pageSize, nil
}

func parsePublicAPIKeyUsageRange(raw string) (string, time.Time, time.Time, error) {
	now := time.Now()
	rangeName := strings.ToLower(strings.TrimSpace(raw))
	if rangeName == "" {
		rangeName = "30d"
	}
	switch rangeName {
	case "today":
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		return rangeName, start, now, nil
	case "7d", "week":
		return "7d", now.Add(-7 * 24 * time.Hour), now, nil
	case "30d", "month":
		return "30d", now.Add(-30 * 24 * time.Hour), now, nil
	case "all":
		return rangeName, time.Time{}, now, nil
	default:
		return "", time.Time{}, time.Time{}, errors.New("range 参数仅支持 today、7d、30d、all")
	}
}

func newPublicAPIKeyUsageRange(name string, start time.Time, end time.Time) publicAPIKeyUsageRange {
	resp := publicAPIKeyUsageRange{
		Name: name,
		End:  end.Format(time.RFC3339),
	}
	if !start.IsZero() {
		formatted := start.Format(time.RFC3339)
		resp.Start = &formatted
	}
	return resp
}

func newPublicAPIKeyUsageKey(row *database.APIKeyRow) publicAPIKeyUsageKey {
	if row == nil {
		return publicAPIKeyUsageKey{}
	}
	var expiresAt *string
	if row.ExpiresAt.Valid {
		formatted := row.ExpiresAt.Time.Format(time.RFC3339)
		expiresAt = &formatted
	}
	var lastResetAt *string
	if row.LastResetAt.Valid {
		formatted := row.LastResetAt.Time.Format(time.RFC3339)
		lastResetAt = &formatted
	}
	status := "active"
	if row.IsExpired(time.Now()) {
		status = "expired"
	} else if row.IsQuotaExhausted() {
		status = "quota_exhausted"
	}
	return publicAPIKeyUsageKey{
		Name:        row.Name,
		Key:         security.MaskAPIKey(row.Key),
		QuotaLimit:  row.QuotaLimit,
		QuotaUsed:   row.QuotaUsed,
		TotalUsed:   row.TotalUsed,
		ResetCount:  row.ResetCount,
		LastResetAt: lastResetAt,
		ExpiresAt:   expiresAt,
		Limits:      row.Limits,
		Status:      status,
		CreatedAt:   row.CreatedAt.Format(time.RFC3339),
	}
}
