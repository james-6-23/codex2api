package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var batchTestAccountTimeout = 30 * time.Second
var batchTestWhamTimeout = 5 * time.Second

// testEvent SSE 测试事件
type testEvent struct {
	Type    string `json:"type"`              // test_start | content | test_complete | error
	Text    string `json:"text,omitempty"`    // 内容文本
	Model   string `json:"model,omitempty"`   // 测试模型
	Success bool   `json:"success,omitempty"` // 是否成功
	Error   string `json:"error,omitempty"`   // 错误信息
}

// TestConnection 测试账号连接（SSE 流式返回）
// GET /api/admin/accounts/:id/test
func (h *Handler) TestConnection(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的账号 ID"})
		return
	}

	// 查找运行时账号；回收站中的账号不在运行时池，构建临时账号做连通性
	// 测试（不参与调度，测试结果不回写账号状态）。
	account := h.store.FindByID(id)
	isTransient := false
	if account == nil {
		transient, buildErr := h.store.BuildTransientAccountByID(c.Request.Context(), id)
		if buildErr != nil || transient == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账号不在运行时池中"})
			return
		}
		account = transient
		isTransient = true
	}

	isOpenAIResponsesAccount := account.IsOpenAIResponsesAPI()
	if !isOpenAIResponsesAccount && account.GetAccessToken() == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账号没有可用的 Access Token，请先刷新"})
		return
	}

	testModel, err := h.connectionTestModelForAccount(c.Request.Context(), account, strings.TrimSpace(c.Query("model")))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 设置 SSE 响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// 发送 test_start
	sendTestEvent(c, testEvent{Type: "test_start", Model: testModel})

	// 回收站账号：记录最近测试结果；restore_on_success=true 时测试通过自动恢复。
	restoreOnSuccess := isTransient && strings.EqualFold(strings.TrimSpace(c.Query("restore_on_success")), "true")
	transientOutcome := "failed"
	if isTransient {
		defer func() {
			h.persistRecycleBinTestResult(id, transientOutcome)
		}()
	}

	// 构建最小测试请求体（参考 sub2api createOpenAITestPayload）
	payload := buildConnectionTestPayload(h.store, testModel)

	// 发送请求
	start := time.Now()
	var resp *http.Response
	var reqErr error
	if isOpenAIResponsesAccount {
		resp, reqErr = proxy.ExecuteOpenAIResponsesRequest(c.Request.Context(), account, payload, h.store.ResolveProxyForAccount(account), nil)
	} else {
		resp, reqErr = proxy.ExecuteRequest(c.Request.Context(), account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	}
	if reqErr != nil {
		sendTestEvent(c, testEvent{Type: "error", Error: fmt.Sprintf("请求失败: %s", reqErr.Error())})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if !isOpenAIResponsesAccount && !isTransient {
			proxy.SyncCodexUsageState(h.store, account, resp)
		}
		errBody, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(errBody), 500))
		if !isTransient {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", errMsg)
			case http.StatusTooManyRequests:
				if isOpenAIResponsesAccount {
					h.store.MarkCooldown(account, time.Minute, "rate_limited")
				} else {
					proxy.Apply429Cooldown(h.store, account, errBody, resp, testModel)
				}
			}
		}
		// 429 限流代表账号有效、只是被限流，不计为失败。
		if isTransient && resp.StatusCode == http.StatusTooManyRequests {
			transientOutcome = "rate_limited"
		}
		sendTestEvent(c, testEvent{Type: "error", Error: errMsg})
		return
	}

	var usageState proxy.CodexUsageSyncResult
	if !isOpenAIResponsesAccount {
		usageStore := h.store
		if isTransient {
			usageStore = nil // 临时账号只读取用量头用于展示，不写入存储
		}
		usageState = proxy.SyncCodexUsageState(usageStore, account, resp)
		if !isTransient {
			applyUsageLimitedTestState(h.store, account, usageState)
		}
		if msg, limited := formatUsageLimitedTestError(usageState); limited {
			// 用量耗尽属于限流类，不计为失败。
			if isTransient {
				transientOutcome = "rate_limited"
			}
			sendTestEvent(c, testEvent{Type: "error", Error: msg})
			return
		}
	}

	// 解析 SSE 流
	hasContent := false
	gotTerminal := false
	sentTerminal := false
	var lastUpstreamEvent []byte
	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		eventType := gjson.GetBytes(data, "type").String()

		switch eventType {
		case "response.output_text.delta":
			delta := gjson.GetBytes(data, "delta").String()
			if delta != "" {
				hasContent = true
				sendTestEvent(c, testEvent{Type: "content", Text: delta})
			}
		case "response.output_text.done":
			if !hasContent {
				text := gjson.GetBytes(data, "text").String()
				if text != "" {
					hasContent = true
					sendTestEvent(c, testEvent{Type: "content", Text: text})
				}
			}
		case "response.content_part.done":
			if !hasContent {
				text := gjson.GetBytes(data, "part.text").String()
				if text != "" {
					hasContent = true
					sendTestEvent(c, testEvent{Type: "content", Text: text})
				}
			}
		case "response.output_item.done":
			if !hasContent {
				text := extractOutputItemText(gjson.GetBytes(data, "item"))
				if text != "" {
					hasContent = true
					sendTestEvent(c, testEvent{Type: "content", Text: text})
				}
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				sentTerminal = true
				if isTransient && proxy.IsUsageLimitReachedError(data) {
					transientOutcome = "rate_limited"
				}
				sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "上游返回 "+status)})
				return false
			}
			if !hasContent {
				text := extractCompletedOutputText(data)
				if text != "" {
					hasContent = true
					sendTestEvent(c, testEvent{Type: "content", Text: text})
				}
			}
			if !hasContent {
				sentTerminal = true
				sendTestEvent(c, testEvent{Type: "error", Error: formatNoOutputUpstreamError(data)})
				return false
			}
			// 测试成功即重置失败/冷却状态，用量限制由调度器自行判断；
			// 临时账号（回收站）不回写任何调度状态。
			if !isTransient && (isOpenAIResponsesAccount || (!usageState.Premium5hRateLimited && (!usageState.HasUsage7d || usageState.UsagePct7d < 100))) {
				h.store.RecordManualTestSuccess(account, time.Since(start))
			}
			if isTransient {
				transientOutcome = "success"
				if restoreOnSuccess {
					restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
					restoreErr := h.restoreAccountByID(restoreCtx, id)
					restoreCancel()
					if restoreErr != nil {
						sendTestEvent(c, testEvent{Type: "content", Text: "\n\n--- 自动恢复失败: " + restoreErr.Error() + " ---"})
					}
				}
			}
			// 如果上游未返回用量头，清除旧的用量缓存，避免显示过期数据
			if !isOpenAIResponsesAccount && !usageState.HasUsage7d && !usageState.HasUsage5h {
				account.ClearUsageCache()
			}
			duration := time.Since(start).Milliseconds()
			sendTestEvent(c, testEvent{
				Type: "content",
				Text: fmt.Sprintf("\n\n--- 耗时 %dms ---", duration),
			})
			sendTestEvent(c, testEvent{Type: "test_complete", Success: true})
			sentTerminal = true
			return false
		case "response.failed":
			gotTerminal = true
			sentTerminal = true
			if isTransient && proxy.IsUsageLimitReachedError(data) {
				transientOutcome = "rate_limited"
			}
			sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "上游返回 response.failed")})
			return false
		case "error":
			gotTerminal = true
			sentTerminal = true
			if isTransient && proxy.IsUsageLimitReachedError(data) {
				transientOutcome = "rate_limited"
			}
			sendTestEvent(c, testEvent{Type: "error", Error: formatUpstreamTestError(data, "上游返回 error 事件")})
			return false
		}
		return true
	})

	if readErr != nil && !sentTerminal {
		sendTestEvent(c, testEvent{Type: "error", Error: "读取上游流失败: " + readErr.Error()})
		return
	}
	if !gotTerminal && !sentTerminal {
		sendTestEvent(c, testEvent{Type: "error", Error: formatMissingTerminalUpstreamError(lastUpstreamEvent)})
	}
}

func buildConnectionTestPayload(store *auth.Store, model string) []byte {
	content := auth.DefaultTestContent
	if store != nil {
		content = store.GetTestContent()
	}
	return buildTestPayloadWithContent(model, content)
}

// buildTestPayload 构建默认最小测试请求体
func buildTestPayload(model string) []byte {
	return buildTestPayloadWithContent(model, auth.DefaultTestContent)
}

// buildTestPayloadWithContent 构建带自定义用户输入内容的最小测试请求体
func buildTestPayloadWithContent(model string, content string) []byte {
	content = auth.NormalizeTestContent(content)
	payload := []byte(`{}`)
	payload, _ = sjson.SetBytes(payload, "model", model)
	payload, _ = sjson.SetBytes(payload, "input", []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": content,
				},
			},
		},
	})
	payload, _ = sjson.SetBytes(payload, "stream", true)
	payload, _ = sjson.SetBytes(payload, "store", false)
	payload, _ = sjson.SetBytes(payload, "instructions", "You are a helpful assistant. Reply briefly.")
	return payload
}

func formatUsageLimitedTestError(state proxy.CodexUsageSyncResult) (string, bool) {
	if state.Premium5hRateLimited {
		remaining := time.Until(state.Reset5hAt).Round(time.Second)
		if remaining < 0 {
			remaining = 0
		}
		return fmt.Sprintf("上游探针返回 200，但 Codex 5h 用量头已达 %.0f%%，账号已保持限流状态，预计 %s 后恢复。", state.UsagePct5h, remaining), true
	}
	if state.HasUsage7d && state.UsagePct7d >= 100 {
		return fmt.Sprintf("上游探针返回 200，但 Codex 7d 用量头已达 %.0f%%，账号已标记为限流/用量耗尽状态。", state.UsagePct7d), true
	}
	return "", false
}

func applyUsageLimitedAccountState(store *auth.Store, account *auth.Account, state proxy.CodexUsageSyncResult) bool {
	if store == nil || account == nil {
		return false
	}
	if state.Premium5hRateLimited || state.Usage7dRateLimited {
		return true
	}
	if state.HasUsage7d && state.UsagePct7d >= 100 {
		return store.MarkUsage7dRateLimited(account)
	}
	return false
}

func applyUsageLimitedTestState(store *auth.Store, account *auth.Account, state proxy.CodexUsageSyncResult) {
	applyUsageLimitedAccountState(store, account, state)
}

// sendTestEvent 发送 SSE 事件
func sendTestEvent(c *gin.Context, event testEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("序列化测试事件失败: %v", err)
		return
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
		log.Printf("写入 SSE 事件失败: %v", err)
		return
	}
	c.Writer.Flush()
}

// truncate 截断字符串
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func extractCompletedOutputText(data []byte) string {
	if text := gjson.GetBytes(data, "response.output_text").String(); text != "" {
		return text
	}
	return extractOutputItemText(gjson.GetBytes(data, "response"))
}

func extractOutputItemText(item gjson.Result) string {
	var b strings.Builder
	writeTextFromOutputItem(&b, item)
	return b.String()
}

func writeTextFromOutputItem(b *strings.Builder, item gjson.Result) {
	if !item.Exists() {
		return
	}
	switch item.Get("type").String() {
	case "output_text", "text":
		b.WriteString(item.Get("text").String())
	case "message", "assistant":
		writeTextFromContentArray(b, item.Get("content"))
	default:
		if output := item.Get("output"); output.IsArray() {
			output.ForEach(func(_, child gjson.Result) bool {
				writeTextFromOutputItem(b, child)
				return true
			})
		}
		writeTextFromContentArray(b, item.Get("content"))
	}
}

func writeTextFromContentArray(b *strings.Builder, content gjson.Result) {
	if !content.IsArray() {
		return
	}
	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		if partType == "output_text" || partType == "text" {
			b.WriteString(part.Get("text").String())
		}
		return true
	})
}

func formatUpstreamTestError(data []byte, fallback string) string {
	msg := firstNonEmptyGJSONString(data,
		"response.status_details.error.message",
		"response.error.message",
		"error.message",
		"message",
		"response.incomplete_details.reason",
		"response.status_details.message",
	)
	if msg == "" {
		msg = fallback
	}

	code := firstNonEmptyGJSONString(data,
		"response.status_details.error.code",
		"response.error.code",
		"error.code",
	)
	if code != "" && !strings.Contains(msg, code) {
		msg += " (code: " + code + ")"
	}

	return formatUpstreamEventDetail(msg, data)
}

func formatNoOutputUpstreamError(data []byte) string {
	msg := "上游已完成但没有返回文本输出"
	if status := gjson.GetBytes(data, "response.status").String(); status != "" && status != "completed" {
		msg = "上游响应状态: " + status
	}
	if reason := gjson.GetBytes(data, "response.incomplete_details.reason").String(); reason != "" {
		msg += " (" + reason + ")"
	}
	return formatUpstreamEventDetail(msg, data)
}

func formatMissingTerminalUpstreamError(lastEvent []byte) string {
	if len(lastEvent) == 0 {
		return "上游流结束但未收到任何事件"
	}
	return formatUpstreamEventDetail("上游流提前结束，未收到 response.completed 或 response.failed", lastEvent)
}

func firstNonEmptyGJSONString(data []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(data, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func formatUpstreamEventDetail(message string, data []byte) string {
	if len(data) == 0 {
		return message
	}
	detail := string(data)
	var parsed any
	if err := json.Unmarshal(data, &parsed); err == nil {
		if pretty, err := json.MarshalIndent(parsed, "", "  "); err == nil {
			detail = string(pretty)
		}
	}
	return message + "\n\n上游事件:\n" + truncate(detail, 3000)
}

func isSupportedConnectionTestModel(model string) bool {
	if strings.Contains(strings.ToLower(model), "image") {
		return false
	}
	for _, supported := range proxy.SupportedModels {
		if model == supported {
			return true
		}
	}
	return false
}

func (h *Handler) connectionTestModel(ctx context.Context) string {
	model := strings.TrimSpace(h.store.GetTestModel())
	if proxy.IsTextTestModelID(ctx, h.db, model) {
		return model
	}
	models := proxy.TextTestModelIDs(ctx, h.db)
	if len(models) > 0 {
		return models[0]
	}
	return "gpt-5.4"
}

func (h *Handler) connectionTestModelForAccount(ctx context.Context, account *auth.Account, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if account == nil || !account.IsOpenAIResponsesAPI() {
		if requested == "" {
			return h.connectionTestModel(ctx), nil
		}
		if !proxy.IsTextTestModelID(ctx, h.db, requested) {
			return "", fmt.Errorf("不支持的测试模型: %s", requested)
		}
		return requested, nil
	}

	models := account.OpenAIResponsesModels()
	textModels := make([]string, 0, len(models))
	for _, model := range models {
		if isTextConnectionModel(model) {
			textModels = append(textModels, strings.TrimSpace(model))
		}
	}
	if len(textModels) == 0 {
		return "", fmt.Errorf("该 Responses API 账号没有可用于测试的文本模型")
	}
	if requested != "" {
		for _, model := range textModels {
			if strings.EqualFold(model, requested) {
				return model, nil
			}
		}
		return "", fmt.Errorf("该账号不支持测试模型: %s", requested)
	}

	defaultModel := strings.TrimSpace(h.store.GetTestModel())
	for _, model := range textModels {
		if strings.EqualFold(model, defaultModel) {
			return model, nil
		}
	}
	return textModels[0], nil
}

func isTextConnectionModel(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	return model != "" && !strings.Contains(model, "image")
}

type batchTestRequest struct {
	IDs *[]int64 `json:"ids"`
	// RestoreOnSuccess 仅回收站批量测试使用：测试通过的账号自动恢复到账号池。
	RestoreOnSuccess bool `json:"restore_on_success"`
}

// persistRecycleBinTestResult 将回收站测试结果写入账号 credentials，供列表展示。
func (h *Handler) persistRecycleBinTestResult(id int64, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{
		"recycle_last_test_status": status,
		"recycle_last_test_at":     time.Now().Format(time.RFC3339),
	}); err != nil {
		log.Printf("写入回收站测试结果失败 (account %d): %v", id, err)
	}
}

type batchOperationEvent struct {
	Type        string `json:"type"` // start | progress | complete
	Action      string `json:"action"`
	Current     int    `json:"current"`
	Total       int    `json:"total"`
	Success     int64  `json:"success"`
	Failed      int64  `json:"failed"`
	Banned      int64  `json:"banned,omitempty"`
	RateLimited int64  `json:"rate_limited,omitempty"`
	Deleted     int64  `json:"deleted,omitempty"`
	AccountID   int64  `json:"account_id,omitempty"`
	Message     string `json:"message,omitempty"`
	Error       string `json:"error,omitempty"`
}

type batchTestCounts struct {
	Total       int
	Success     int64
	Failed      int64
	Banned      int64
	RateLimited int64
}

func resolveBatchTestAccounts(store *auth.Store, ids *[]int64) ([]*auth.Account, int) {
	if store == nil {
		return nil, 0
	}
	if ids == nil {
		return store.Accounts(), 0
	}

	accounts := make([]*auth.Account, 0, len(*ids))
	missing := 0
	seen := make(map[int64]struct{}, len(*ids))
	for _, id := range *ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		acc := store.FindByID(id)
		if acc == nil {
			missing++
			continue
		}
		accounts = append(accounts, acc)
	}
	return accounts, missing
}

// BatchTest 批量测试账号连接；未传 ids 时测试所有账号，传 ids 时仅测试指定账号。
// POST /api/admin/accounts/batch-test
func (h *Handler) BatchTest(c *gin.Context) {
	var req batchTestRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "请求格式错误")
			return
		}
	}
	if req.IDs != nil && len(*req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要测试的账号 ID 列表")
		return
	}

	accounts, missingCount := resolveBatchTestAccounts(h.store, req.IDs)
	h.serveBatchTest(c, accounts, missingCount, h.runSingleBatchTest)
}

// RecycleBinBatchTest 批量测试回收站账号连接；未传 ids 时测试回收站全部账号。
// 账号以临时对象构建，不参与调度，测试结果不回写任何账号状态。
// POST /api/admin/accounts/recycle-bin/batch-test
func (h *Handler) RecycleBinBatchTest(c *gin.Context) {
	var req batchTestRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "请求格式错误")
			return
		}
	}
	if req.IDs != nil && len(*req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要测试的账号 ID 列表")
		return
	}

	listCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	rows, err := h.db.ListDeleted(listCtx)
	cancel()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	binIDs := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		binIDs[row.ID] = struct{}{}
	}

	wanted := make([]int64, 0, len(rows))
	missing := 0
	if req.IDs == nil {
		for _, row := range rows {
			wanted = append(wanted, row.ID)
		}
	} else {
		seen := make(map[int64]struct{}, len(*req.IDs))
		for _, id := range *req.IDs {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			if _, ok := binIDs[id]; ok {
				wanted = append(wanted, id)
			} else {
				missing++
			}
		}
	}

	accounts := make([]*auth.Account, 0, len(wanted))
	for _, id := range wanted {
		buildCtx, buildCancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		acc, buildErr := h.store.BuildTransientAccountByID(buildCtx, id)
		buildCancel()
		if buildErr != nil || acc == nil {
			missing++
			continue
		}
		accounts = append(accounts, acc)
	}

	restoreOnSuccess := req.RestoreOnSuccess
	testFn := func(ctx context.Context, acc *auth.Account) (string, string) {
		status, msg := h.runRecycleBinSingleTest(ctx, acc)
		h.persistRecycleBinTestResult(acc.DBID, status)
		if status == "success" && restoreOnSuccess {
			restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
			restoreErr := h.restoreAccountByID(restoreCtx, acc.DBID)
			restoreCancel()
			if restoreErr != nil {
				msg = "测试通过，但自动恢复失败: " + restoreErr.Error()
			} else {
				msg = "测试通过，已自动恢复"
			}
		}
		return status, msg
	}
	h.serveBatchTest(c, accounts, missing, testFn)
}

// serveBatchTest 统一处理批量测试的流式/非流式响应；testFn 决定单账号测试行为。
func (h *Handler) serveBatchTest(c *gin.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string)) {
	if strings.EqualFold(c.Query("stream"), "true") {
		h.streamBatchTest(c, accounts, missingCount, testFn)
		return
	}

	if len(accounts) == 0 && missingCount == 0 {
		c.JSON(http.StatusOK, gin.H{"total": 0, "success": 0, "failed": 0, "banned": 0, "rate_limited": 0})
		return
	}

	counts := h.runBatchTest(c.Request.Context(), accounts, missingCount, testFn, nil)
	c.JSON(http.StatusOK, gin.H{
		"total":        counts.Total,
		"success":      counts.Success,
		"failed":       counts.Failed,
		"banned":       counts.Banned,
		"rate_limited": counts.RateLimited,
	})
}

func (h *Handler) streamBatchTest(c *gin.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string)) {
	total := len(accounts) + missingCount
	setupSSE(c)
	sendSSEJSON(c, batchOperationEvent{Type: "start", Action: "batch_test", Total: total})
	if total == 0 {
		sendSSEJSON(c, batchOperationEvent{Type: "complete", Action: "batch_test"})
		return
	}

	events := make(chan batchOperationEvent, len(accounts)+2)
	ctx := c.Request.Context()
	go func() {
		counts := h.runBatchTest(ctx, accounts, missingCount, testFn, func(event batchOperationEvent) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})
		select {
		case events <- batchOperationEvent{
			Type:        "complete",
			Action:      "batch_test",
			Current:     counts.Total,
			Total:       counts.Total,
			Success:     counts.Success,
			Failed:      counts.Failed,
			Banned:      counts.Banned,
			RateLimited: counts.RateLimited,
		}:
		case <-ctx.Done():
		}
		close(events)
	}()

	for event := range events {
		sendSSEJSON(c, event)
	}
}

func (h *Handler) runBatchTest(ctx context.Context, accounts []*auth.Account, missingCount int, testFn func(context.Context, *auth.Account) (string, string), onProgress func(batchOperationEvent)) batchTestCounts {
	total := len(accounts) + missingCount
	concurrency := h.store.GetTestConcurrency()
	if concurrency <= 0 {
		concurrency = 1
	}

	var (
		successCount   int64
		failedCount    = int64(missingCount)
		bannedCount    int64
		rateLimitCount int64
		completedCount = int64(missingCount)
		wg             sync.WaitGroup
		sem            = make(chan struct{}, concurrency)
	)

	if missingCount > 0 && onProgress != nil {
		onProgress(batchOperationEvent{
			Type:    "progress",
			Action:  "batch_test",
			Current: missingCount,
			Total:   total,
			Failed:  failedCount,
			Error:   fmt.Sprintf("%d 个账号不在运行时池中", missingCount),
		})
	}

	for _, account := range accounts {
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				atomic.AddInt64(&failedCount, 1)
				h.emitBatchTestProgress(onProgress, acc.DBID, total, &completedCount, &successCount, &failedCount, &bannedCount, &rateLimitCount, "failed", "测试已取消")
				return
			}
			defer func() { <-sem }()

			status, message := testFn(ctx, acc)
			switch status {
			case "success":
				atomic.AddInt64(&successCount, 1)
			case "banned":
				atomic.AddInt64(&bannedCount, 1)
			case "rate_limited":
				atomic.AddInt64(&rateLimitCount, 1)
			default:
				atomic.AddInt64(&failedCount, 1)
			}
			h.emitBatchTestProgress(onProgress, acc.DBID, total, &completedCount, &successCount, &failedCount, &bannedCount, &rateLimitCount, status, message)
		}(account)
	}

	wg.Wait()
	return batchTestCounts{
		Total:       total,
		Success:     atomic.LoadInt64(&successCount),
		Failed:      atomic.LoadInt64(&failedCount),
		Banned:      atomic.LoadInt64(&bannedCount),
		RateLimited: atomic.LoadInt64(&rateLimitCount),
	}
}

func (h *Handler) emitBatchTestProgress(
	onProgress func(batchOperationEvent),
	accountID int64,
	total int,
	completedCount *int64,
	successCount *int64,
	failedCount *int64,
	bannedCount *int64,
	rateLimitCount *int64,
	status string,
	message string,
) {
	if onProgress == nil {
		return
	}
	current := int(atomic.AddInt64(completedCount, 1))
	event := batchOperationEvent{
		Type:        "progress",
		Action:      "batch_test",
		Current:     current,
		Total:       total,
		Success:     atomic.LoadInt64(successCount),
		Failed:      atomic.LoadInt64(failedCount),
		Banned:      atomic.LoadInt64(bannedCount),
		RateLimited: atomic.LoadInt64(rateLimitCount),
		AccountID:   accountID,
		Message:     message,
	}
	if status == "failed" {
		event.Error = message
	}
	onProgress(event)
}

func (h *Handler) runSingleBatchTest(ctx context.Context, acc *auth.Account) (string, string) {
	testCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()

	if !acc.IsOpenAIResponsesAPI() && acc.GetAccessToken() == "" {
		acc.Mu().RLock()
		hasRefreshToken := acc.RefreshToken != ""
		acc.Mu().RUnlock()
		if !hasRefreshToken {
			h.store.MarkError(acc, "批量测试失败: 账号缺少 access_token 和 refresh_token")
		}
		return "failed", "账号缺少 access_token 和 refresh_token"
	}

	if status, msg, done := h.batchTestWhamPreflight(testCtx, acc); done {
		return status, msg
	}

	testModel, modelErr := h.connectionTestModelForAccount(testCtx, acc, "")
	if modelErr != nil {
		if msg, ok := batchTestContextFailure(testCtx, modelErr); ok {
			return "failed", msg
		}
		h.store.MarkError(acc, "批量测试失败: "+modelErr.Error())
		return "failed", modelErr.Error()
	}
	payload := buildConnectionTestPayload(h.store, testModel)
	start := time.Now()

	var resp *http.Response
	var err error
	if acc.IsOpenAIResponsesAPI() {
		resp, err = proxy.ExecuteOpenAIResponsesRequest(testCtx, acc, payload, h.store.ResolveProxyForAccount(acc), nil)
	} else {
		resp, err = proxy.ExecuteRequest(testCtx, acc, payload, "", h.store.ResolveProxyForAccount(acc), "", nil, nil)
	}
	if err != nil {
		if msg, ok := batchTestContextFailure(testCtx, err); ok {
			if errors.Is(testCtx.Err(), context.DeadlineExceeded) {
				h.store.ReportRequestFailure(acc, "timeout", batchTestAccountTimeout)
			}
			return "failed", msg
		}
		h.store.MarkError(acc, "批量测试请求失败: "+err.Error())
		return "failed", err.Error()
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if !acc.IsOpenAIResponsesAPI() {
			usageState := proxy.SyncCodexUsageState(h.store, acc, resp)
			applyUsageLimitedTestState(h.store, acc, usageState)
			if msg, limited := formatUsageLimitedTestError(usageState); limited {
				return "rate_limited", msg
			}
		}
		status, msg := h.readBatchTestStreamResult(testCtx, acc, resp, testModel)
		if status != "success" {
			return status, msg
		}
		if !acc.IsOpenAIResponsesAPI() {
			if _, hasUsage7d := acc.GetUsagePercent7d(); !hasUsage7d {
				if _, _, hasUsage5h := acc.GetUsageSnapshot5h(); !hasUsage5h {
					acc.ClearUsageCache()
				}
			}
		}
		// 测试成功即重置失败/冷却状态，用量限制由调度器自行判断
		h.store.RecordManualTestSuccess(acc, time.Since(start))
		return "success", msg
	case http.StatusUnauthorized:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		if !acc.IsOpenAIResponsesAPI() {
			proxy.SyncCodexUsageState(h.store, acc, resp)
		}
		h.store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300)))
		return "banned", "账号授权失败"
	case http.StatusTooManyRequests:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		if acc.IsOpenAIResponsesAPI() {
			h.store.MarkCooldown(acc, time.Minute, "rate_limited")
		} else {
			proxy.SyncCodexUsageState(h.store, acc, resp)
			proxy.Apply429Cooldown(h.store, acc, body, resp, testModel)
		}
		return "rate_limited", "账号触发 429 限流"
	default:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			return h.handleBatchTestReadError(testCtx, acc, readErr)
		}
		msg := fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
		if shouldMarkBatchTestAccountError(resp.StatusCode, body) {
			h.store.MarkError(acc, "批量测试"+msg)
		}
		return "failed", msg
	}
}

// runRecycleBinSingleTest 测试单个回收站账号的连通性。账号是临时对象，
// 全程不调用任何会回写账号/调度状态的方法（MarkError/MarkCooldown/
// RecordManualTestSuccess 等），测试结果仅用于展示。
func (h *Handler) runRecycleBinSingleTest(ctx context.Context, acc *auth.Account) (string, string) {
	testCtx, cancel := context.WithTimeout(ctx, batchTestAccountTimeout)
	defer cancel()

	if !acc.IsOpenAIResponsesAPI() && acc.GetAccessToken() == "" {
		return "failed", "账号缺少可用的 Access Token"
	}

	testModel, modelErr := h.connectionTestModelForAccount(testCtx, acc, "")
	if modelErr != nil {
		if msg, ok := batchTestContextFailure(testCtx, modelErr); ok {
			return "failed", msg
		}
		return "failed", modelErr.Error()
	}
	payload := buildConnectionTestPayload(h.store, testModel)

	var resp *http.Response
	var err error
	if acc.IsOpenAIResponsesAPI() {
		resp, err = proxy.ExecuteOpenAIResponsesRequest(testCtx, acc, payload, h.store.ResolveProxyForAccount(acc), nil)
	} else {
		resp, err = proxy.ExecuteRequest(testCtx, acc, payload, "", h.store.ResolveProxyForAccount(acc), "", nil, nil)
	}
	if err != nil {
		if msg, ok := batchTestContextFailure(testCtx, err); ok {
			return "failed", msg
		}
		return "failed", err.Error()
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if !acc.IsOpenAIResponsesAPI() {
			// store 传 nil：只解析用量头用于结果展示，不持久化、不改限流状态。
			usageState := proxy.SyncCodexUsageState(nil, acc, resp)
			if msg, limited := formatUsageLimitedTestError(usageState); limited {
				return "rate_limited", msg
			}
		}
		return readRecycleBinTestStream(testCtx, resp)
	case http.StatusUnauthorized:
		body, _ := readBatchTestErrorBody(testCtx, resp.Body)
		return "banned", fmt.Sprintf("上游返回 401: %s", truncate(string(body), 300))
	case http.StatusTooManyRequests:
		body, _ := readBatchTestErrorBody(testCtx, resp.Body)
		return "rate_limited", fmt.Sprintf("上游返回 429: %s", truncate(string(body), 300))
	default:
		body, readErr := readBatchTestErrorBody(testCtx, resp.Body)
		if readErr != nil {
			if msg, ok := batchTestContextFailure(testCtx, readErr); ok {
				return "failed", msg
			}
			return "failed", readErr.Error()
		}
		return "failed", fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
}

// readRecycleBinTestStream 读取测试 SSE 流并判定结果；与
// readBatchTestStreamResult 等价，但不回写任何账号状态。
func readRecycleBinTestStream(ctx context.Context, resp *http.Response) (string, string) {
	hasContent := false
	gotTerminal := false
	resultStatus := ""
	resultMessage := ""
	var lastUpstreamEvent []byte

	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_text.delta":
			if gjson.GetBytes(data, "delta").String() != "" {
				hasContent = true
			}
		case "response.output_text.done":
			if !hasContent && gjson.GetBytes(data, "text").String() != "" {
				hasContent = true
			}
		case "response.content_part.done":
			if !hasContent && gjson.GetBytes(data, "part.text").String() != "" {
				hasContent = true
			}
		case "response.output_item.done":
			if !hasContent && extractOutputItemText(gjson.GetBytes(data, "item")) != "" {
				hasContent = true
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				resultStatus = "failed"
				if proxy.IsUsageLimitReachedError(data) {
					resultStatus = "rate_limited"
				}
				resultMessage = formatUpstreamTestError(data, "上游返回 "+status)
				return false
			}
			if !hasContent && extractCompletedOutputText(data) != "" {
				hasContent = true
			}
			if !hasContent {
				resultStatus = "failed"
				resultMessage = formatNoOutputUpstreamError(data)
				return false
			}
			resultStatus = "success"
			resultMessage = "测试通过"
			return false
		case "response.failed":
			gotTerminal = true
			resultStatus = "failed"
			if proxy.IsUsageLimitReachedError(data) {
				resultStatus = "rate_limited"
			}
			resultMessage = formatUpstreamTestError(data, "上游返回 response.failed")
			return false
		case "error":
			gotTerminal = true
			resultStatus = "failed"
			resultMessage = formatUpstreamTestError(data, "上游返回 error 事件")
			return false
		}
		return true
	})

	if readErr != nil {
		if msg, ok := batchTestContextFailure(ctx, readErr); ok {
			return "failed", msg
		}
		return "failed", readErr.Error()
	}
	if resultStatus != "" {
		return resultStatus, resultMessage
	}
	if !gotTerminal {
		return "failed", formatMissingTerminalUpstreamError(lastUpstreamEvent)
	}
	return "failed", "上游测试未返回明确结果"
}

func (h *Handler) batchTestWhamPreflight(ctx context.Context, acc *auth.Account) (string, string, bool) {
	if h == nil || h.store == nil || acc == nil || acc.IsOpenAIResponsesAPI() || acc.GetAccessToken() == "" {
		return "", "", false
	}

	whamCtx, cancel := context.WithTimeout(ctx, batchTestWhamTimeout)
	defer cancel()

	usage, resp, err := proxy.QueryWhamUsage(whamCtx, acc, h.store.ResolveProxyForAccount(acc))
	if resp != nil && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg := fmt.Sprintf("WHAM 用量探针返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
			h.store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", msg)
			return "banned", msg, true
		default:
			if shouldMarkUsageProbeAccountError(resp.StatusCode, body) {
				msg := fmt.Sprintf("WHAM 用量探针返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
				h.store.MarkError(acc, msg)
				return "failed", msg, true
			}
		}
	}
	if err != nil || usage == nil {
		return "", "", false
	}

	usageState := proxy.ApplyWhamUsage(h.store, acc, usage)
	applyUsageLimitedTestState(h.store, acc, usageState)
	if msg, limited := formatUsageLimitedTestError(usageState); limited {
		return "rate_limited", msg, true
	}
	return "", "", false
}

func (h *Handler) readBatchTestStreamResult(ctx context.Context, acc *auth.Account, resp *http.Response, model string) (string, string) {
	hasContent := false
	gotTerminal := false
	resultStatus := ""
	resultMessage := ""
	var lastUpstreamEvent []byte

	readErr := proxy.ReadSSEStream(resp.Body, func(data []byte) bool {
		lastUpstreamEvent = append(lastUpstreamEvent[:0], data...)
		eventType := gjson.GetBytes(data, "type").String()

		switch eventType {
		case "response.output_text.delta":
			if gjson.GetBytes(data, "delta").String() != "" {
				hasContent = true
			}
		case "response.output_text.done":
			if !hasContent && gjson.GetBytes(data, "text").String() != "" {
				hasContent = true
			}
		case "response.content_part.done":
			if !hasContent && gjson.GetBytes(data, "part.text").String() != "" {
				hasContent = true
			}
		case "response.output_item.done":
			if !hasContent && extractOutputItemText(gjson.GetBytes(data, "item")) != "" {
				hasContent = true
			}
		case "response.completed":
			gotTerminal = true
			if status := gjson.GetBytes(data, "response.status").String(); status == "failed" || status == "incomplete" {
				resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "上游返回 "+status)
				return false
			}
			if !hasContent && extractCompletedOutputText(data) != "" {
				hasContent = true
			}
			if !hasContent {
				resultStatus = "failed"
				resultMessage = formatNoOutputUpstreamError(data)
				h.markBatchTestStreamFailure(acc, resultMessage)
				return false
			}
			resultStatus = "success"
			resultMessage = "测试通过"
			return false
		case "response.failed":
			gotTerminal = true
			resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "上游返回 response.failed")
			return false
		case "error":
			gotTerminal = true
			resultStatus, resultMessage = h.batchTestTerminalFailure(acc, resp, model, data, "上游返回 error 事件")
			return false
		}
		return true
	})

	if readErr != nil {
		return h.handleBatchTestReadError(ctx, acc, readErr)
	}
	if resultStatus != "" {
		return resultStatus, resultMessage
	}
	if !gotTerminal {
		msg := formatMissingTerminalUpstreamError(lastUpstreamEvent)
		h.markBatchTestStreamFailure(acc, msg)
		return "failed", msg
	}
	msg := "上游测试未返回明确结果"
	h.markBatchTestStreamFailure(acc, msg)
	return "failed", msg
}

func (h *Handler) batchTestTerminalFailure(acc *auth.Account, resp *http.Response, model string, payload []byte, fallback string) (string, string) {
	message := formatUpstreamTestError(payload, fallback)
	if proxy.IsUsageLimitReachedError(payload) {
		proxy.Apply429Cooldown(h.store, acc, payload, resp, model)
		return "rate_limited", message
	}
	h.markBatchTestStreamFailure(acc, message)
	return "failed", message
}

func (h *Handler) markBatchTestStreamFailure(acc *auth.Account, message string) {
	if h == nil || h.store == nil || acc == nil {
		return
	}
	switch acc.RuntimeStatus() {
	case "active", "ready", "refreshing", "error":
		h.store.MarkError(acc, "批量测试失败: "+message)
	}
}

func readBatchTestErrorBody(ctx context.Context, body io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(body, 64<<10))
}

func (h *Handler) handleBatchTestReadError(ctx context.Context, acc *auth.Account, err error) (string, string) {
	if msg, ok := batchTestContextFailure(ctx, err); ok {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			h.store.ReportRequestFailure(acc, "timeout", batchTestAccountTimeout)
		}
		return "failed", msg
	}
	h.store.MarkError(acc, "批量测试读取响应失败: "+err.Error())
	return "failed", err.Error()
}

func batchTestContextFailure(ctx context.Context, err error) (string, bool) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("测试超时: %s 内未完成", batchTestAccountTimeout), true
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "测试已取消", true
	}
	return "", false
}

func shouldMarkBatchTestAccountError(statusCode int, body []byte) bool {
	msg := strings.ToLower(string(body))
	if statusCode == http.StatusPaymentRequired && proxy.IsDeactivatedWorkspaceError(body) {
		return true
	}
	if statusCode == http.StatusForbidden {
		return true
	}
	if statusCode == http.StatusBadRequest {
		for _, needle := range []string{
			"invalid_grant",
			"invalid_client",
			"unauthorized_client",
			"access_denied",
			"account_deactivated",
			"unsupported_country_region_territory",
		} {
			if strings.Contains(msg, needle) {
				return true
			}
		}
	}
	return false
}
