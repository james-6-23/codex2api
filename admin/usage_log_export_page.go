package admin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type usageLogPageCursor struct {
	Version     int                           `json:"version"`
	FilterHash  string                        `json:"filter_hash"`
	GeneratedAt time.Time                     `json:"generated_at"`
	Position    database.UsageLogExportCursor `json:"position"`
}

func (h *Handler) exportUsageLogPage(request *gin.Context, scope string, filters map[string]string, filter *database.UsageLogFilter) {
	ctx, cancel := context.WithTimeout(request.Request.Context(), 30*time.Second)
	defer cancel()
	encodedFilter, err := json.Marshal(struct {
		Scope  string
		Filter *database.UsageLogFilter
	}{scope, filter})
	if err != nil {
		writeInternalError(request, err)
		return
	}
	digest := sha256.Sum256(encodedFilter)
	cursor := usageLogPageCursor{Version: 1, FilterHash: hex.EncodeToString(digest[:]), GeneratedAt: time.Now().UTC()}
	if raw := request.Query("cursor"); raw != "" {
		if len(raw) > 2048 {
			writeError(request, http.StatusBadRequest, "导出游标过长，请重新下载")
			return
		}
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(raw)
		if decodeErr != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.Version != 1 || cursor.FilterHash != hex.EncodeToString(digest[:]) || cursor.GeneratedAt.IsZero() || cursor.Position.SnapshotID < 0 || cursor.Position.BeforeID <= 0 || cursor.Position.BeforeID > cursor.Position.SnapshotID || cursor.Position.BeforeTime.IsZero() {
			writeError(request, http.StatusBadRequest, "导出游标无效或筛选已改变，请重新下载")
			return
		}
	} else {
		cursor.Position.SnapshotID, err = h.db.UsageLogExportSnapshot(ctx)
		if err != nil {
			writeInternalError(request, err)
			return
		}
	}
	metadata, err := json.Marshal(map[string]any{"version": 1, "scope": scope, "generated_at": cursor.GeneratedAt, "filters": filters})
	if err != nil {
		writeInternalError(request, err)
		return
	}
	records := make([]string, 0, 200)
	bytes := 0
	more := errors.New("export page complete")
	next := cursor
	err = h.db.WalkUsageLogExportPage(ctx, filter, &cursor.Position, func(entry *database.UsageLogExportEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(records) >= 200 {
			return more
		}
		entry.Diagnostics = proxy.EnrichWebsocketLifecycle(entry.Diagnostics)
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		var record any
		decoder := json.NewDecoder(strings.NewReader(string(payload)))
		decoder.UseNumber()
		if err := decoder.Decode(&record); err != nil {
			return err
		}
		payload, err = json.Marshal(sanitizeUsageExportValue(record, 0))
		if err != nil {
			return err
		}
		if len(records) > 0 && bytes+len(payload) > 4*1024*1024 {
			return more
		}
		records = append(records, string(payload))
		bytes += len(payload)
		next.Position.BeforeID, next.Position.BeforeTime = entry.ID, entry.CreatedAt
		return nil
	})
	if err != nil && !errors.Is(err, more) {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(request, http.StatusGatewayTimeout, "单批日志查询超时，请缩小时间范围或增加用户、账号筛选")
		} else {
			writeInternalError(request, err)
		}
		return
	}
	nextCursor := ""
	if errors.Is(err, more) {
		encoded, marshalErr := json.Marshal(next)
		if marshalErr != nil {
			writeInternalError(request, marshalErr)
			return
		}
		nextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	request.Header("Cache-Control", "no-store")
	request.Header("X-Content-Type-Options", "nosniff")
	request.JSON(http.StatusOK, gin.H{"version": 1, "metadata": string(metadata), "records": records, "next_cursor": nextCursor, "complete": nextCursor == ""})
}
