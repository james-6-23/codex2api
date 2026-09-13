package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

var usageExportSecretPattern = regexp.MustCompile(`(?i)(bearer\s+|sk-)[A-Za-z0-9._~+/=-]+|(access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|password|secret)["']?\s*[:=]\s*["']?[^"'\s&,]+`)

func (h *Handler) ExportUsageLogs(c *gin.Context) {
	if c.Query("confirmed") != "true" {
		writeError(c, http.StatusBadRequest, "请先确认下载使用日志")
		return
	}
	scope := c.Query("scope")
	var filter *database.UsageLogFilter
	filters := make(map[string]string)
	switch scope {
	case "all":
	case "filtered":
		if raw := strings.ToLower(strings.TrimSpace(c.Query("channel"))); raw != "" && raw != "all" && parseUsageChannel(c) == "" {
			writeError(c, http.StatusBadRequest, "无效的渠道筛选")
			return
		}
		start, startErr := time.Parse(time.RFC3339, c.Query("start"))
		end, endErr := time.Parse(time.RFC3339, c.Query("end"))
		if startErr != nil || endErr != nil || end.Before(start) {
			writeError(c, http.StatusBadRequest, "请提供有效的 start/end 时间范围（RFC3339，开始不晚于结束）")
			return
		}
		parsed, ok := parseUsageLogsFilter(c, start, end)
		if !ok {
			return
		}
		filter = &parsed
		for _, key := range []string{"start", "end", "q", "search_scope", "request_type", "model", "endpoint", "api_key_id", "account_id", "fast", "stream", "compact", "has_compaction_history", "channel", "status", "status_code", "error_only", "error_kind", "retry", "via_websocket", "include_canceled", "email", "request_id", "upstream_request_id"} {
			if value := c.Query(key); value != "" {
				filters[key] = usageExportSecretPattern.ReplaceAllString(security.MaskURLCredentials(value), "[REDACTED]")
			}
		}
	default:
		writeError(c, http.StatusBadRequest, "scope 必须为 filtered 或 all")
		return
	}
	if !h.usageLogExportBusy.CompareAndSwap(false, true) {
		writeError(c, http.StatusConflict, "已有使用日志正在导出，请等待完成后重试")
		return
	}
	defer h.usageLogExportBusy.Store(false)
	if c.Query("paged") == "true" {
		h.exportUsageLogPage(c, scope, filters, filter)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()
	file, err := os.CreateTemp("", "codex2api-usage-export-*.json")
	if err != nil {
		writeInternalError(c, err)
		return
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	generatedAt := time.Now().UTC()
	writer := bufio.NewWriterSize(file, 64*1024)
	err = h.writeUsageLogExport(ctx, writer, scope, filters, filter, generatedAt)
	if err == nil {
		err = writer.Flush()
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		writeInternalError(c, err)
		return
	}
	filename := fmt.Sprintf("usage-logs-%s-%s.json", scope, generatedAt.Format("20060102-150405"))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.DataFromReader(http.StatusOK, info.Size(), "application/json; charset=utf-8", file, nil)
}

func (h *Handler) writeUsageLogExport(ctx context.Context, writer io.Writer, scope string, filters map[string]string, filter *database.UsageLogFilter, generatedAt time.Time) error {
	metadata, err := json.Marshal(map[string]any{
		"version": 1, "scope": scope, "generated_at": generatedAt, "filters": filters,
	})
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(writer, "%s,\"logs\":[\n", metadata[:len(metadata)-1]); err != nil {
		return err
	}
	count := int64(0)
	encoder := json.NewEncoder(writer)
	err = h.db.WalkUsageLogsForExport(ctx, filter, func(entry *database.UsageLogExportEntry) error {
		if err := ctx.Err(); err != nil {
			return err
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
		if count > 0 {
			if _, err := io.WriteString(writer, ","); err != nil {
				return err
			}
		}
		if err := encoder.Encode(sanitizeUsageExportValue(record, 0)); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "],\"total\":%d,\"complete\":true}\n", count)
	return err
}

func sanitizeUsageExportValue(value any, depth int) any {
	if depth > 32 {
		return "[omitted: nesting limit]"
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			leaf := key[strings.LastIndex(key, ".")+1:]
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(leaf, "-", ""), "_", ""))
			switch normalized {
			case "authorization", "proxyauthorization", "cookie", "setcookie", "apikey", "xapikey", "xadminkey", "accesstoken", "refreshtoken", "idtoken", "clientsecret", "password", "secret", "token", "credentials", "requestbody", "responsebody", "prompt", "instructions", "input", "output", "encryptedcontent":
				delete(typed, key)
				continue
			}
			if normalized == "xcodexturnmetadata" {
				if text, ok := item.(string); ok && json.Valid([]byte(text)) {
					decoder := json.NewDecoder(strings.NewReader(text))
					decoder.UseNumber()
					var metadata any
					if decoder.Decode(&metadata) == nil {
						item = metadata
					}
				}
			}
			typed[key] = sanitizeUsageExportValue(item, depth+1)
		}
	case []any:
		for index, item := range typed {
			typed[index] = sanitizeUsageExportValue(item, depth+1)
		}
	case string:
		return usageExportSecretPattern.ReplaceAllString(security.MaskURLCredentials(typed), "[REDACTED]")
	}
	return value
}
