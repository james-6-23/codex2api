package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// isAgentIdentityCredentialRow 判断 DB 账号行是否为 Agent Identity 授权（供列表响应打标）。
func isAgentIdentityCredentialRow(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(row.GetCredential("auth_mode")), auth.CodexAuthModeAgentIdentity) &&
		strings.TrimSpace(row.GetCredential("agent_runtime_id")) != "" &&
		strings.TrimSpace(row.GetCredential("agent_private_key")) != ""
}

// importAgentIdentityReq 是 Agent Identity auth.json 导入的请求体。
type importAgentIdentityReq struct {
	Name     string `json:"name"`
	AuthJSON string `json:"auth_json"`
	ProxyURL string `json:"proxy_url"`
}

// agentIdentityFields 从 auth.json（agent_identity 对象或 auth_mode=agentIdentity 的根）
// 解析出的字段，snake_case / camelCase 均兼容。
type agentIdentityFields struct {
	RuntimeID  string
	PrivateKey string
	TaskID     string
	AccountID  string
	UserID     string
	Email      string
	PlanType   string
	FedRAMP    bool
}

func agentIdentityString(node map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := node[key].(string); ok {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func agentIdentityBool(node map[string]any, keys ...string) bool {
	for _, key := range keys {
		if v, ok := node[key].(bool); ok {
			return v
		}
	}
	return false
}

// resolveAgentIdentityNode 从一个 JSON 对象里定位 Agent Identity 字段所在的节点：
//   - 有 agent_identity / agentIdentity 子对象 → 取子对象；
//   - 根上 auth_mode=agentIdentity 或直接带 agent_runtime_id → 取该对象本身。
//
// 命中返回节点与 true，否则 false。
func resolveAgentIdentityNode(m map[string]any) (map[string]any, bool) {
	if m == nil {
		return nil, false
	}
	if sub, ok := m["agent_identity"].(map[string]any); ok {
		return sub, true
	}
	if subCamel, ok := m["agentIdentity"].(map[string]any); ok {
		return subCamel, true
	}
	mode := agentIdentityString(m, "auth_mode", "authMode")
	if strings.EqualFold(mode, auth.CodexAuthModeAgentIdentity) ||
		agentIdentityString(m, "agent_runtime_id", "agentRuntimeId") != "" {
		return m, true
	}
	return nil, false
}

// parseAgentIdentityAuthJSON 解析 Agent Identity auth.json。
func parseAgentIdentityAuthJSON(raw string) (*agentIdentityFields, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("auth.json 内容为空")
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(trimmed), &root); err != nil {
		return nil, fmt.Errorf("auth.json 不是合法的 JSON: %w", err)
	}
	// 先按根级识别；识别不到再穿透 credentials 包装（sub2api / codex2api
	// 导出的账号 JSON 把 Agent Identity 字段包在 credentials 对象里）。
	node, ok := resolveAgentIdentityNode(root)
	if !ok {
		if wrapped, wok := root["credentials"].(map[string]any); wok {
			node, ok = resolveAgentIdentityNode(wrapped)
		}
	}
	if !ok {
		return nil, fmt.Errorf("auth.json 不是 Agent Identity 格式（缺少 agent_identity 或 auth_mode=agentIdentity）")
	}

	fields := &agentIdentityFields{
		RuntimeID:  agentIdentityString(node, "agent_runtime_id", "agentRuntimeId"),
		PrivateKey: agentIdentityString(node, "agent_private_key", "agentPrivateKey"),
		TaskID:     agentIdentityString(node, "task_id", "taskId"),
		AccountID:  agentIdentityString(node, "account_id", "accountId"),
		UserID:     agentIdentityString(node, "chatgpt_user_id", "chatgptUserId"),
		Email:      agentIdentityString(node, "email"),
		PlanType:   agentIdentityString(node, "plan_type", "planType"),
		FedRAMP:    agentIdentityBool(node, "chatgpt_account_is_fedramp", "chatgptAccountIsFedramp"),
	}
	if fields.RuntimeID == "" || fields.PrivateKey == "" || fields.AccountID == "" || fields.UserID == "" {
		return nil, fmt.Errorf("agent identity 缺少必要字段（agent_runtime_id / agent_private_key / account_id / chatgpt_user_id）")
	}
	if err := auth.ValidateCodexAgentIdentityPrivateKey(fields.PrivateKey); err != nil {
		return nil, fmt.Errorf("agent identity 私钥无效: %w", err)
	}
	return fields, nil
}

// ImportCodexAgentIdentity 导入 Codex Agent Identity auth.json 并创建账号。
// 不保存 OAuth access/refresh token；每次上游请求用私钥动态签名。
// POST /api/admin/accounts/codex/agent-identity
func (h *Handler) ImportCodexAgentIdentity(c *gin.Context) {
	var req importAgentIdentityReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "名称长度不能超过100字符")
		return
	}
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}

	fields, err := parseAgentIdentityAuthJSON(req.AuthJSON)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	// 按 agent_runtime_id 去重：同一 runtime 已存在则拒绝。
	if h.agentIdentityRuntimeExists(fields.RuntimeID) {
		writeError(c, http.StatusConflict, "该 Agent Identity 账号已存在")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	id, err := h.createAgentIdentityAccount(ctx, fields, req.Name, req.ProxyURL, "agent_identity_import")
	if err != nil {
		writeInternalError(c, err)
		return
	}

	security.SecurityAuditLog("CODEX_AGENT_IDENTITY_ADDED", fmt.Sprintf("account_id=%d email=%s ip=%s", id, fields.Email, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"message": "成功导入 Agent Identity 账号",
		"id":      id,
		"email":   fields.Email,
	})
}

// agentIdentityRuntimeExists 判断池中是否已有相同 agent_runtime_id 的账号。
func (h *Handler) agentIdentityRuntimeExists(runtimeID string) bool {
	runtimeID = strings.TrimSpace(runtimeID)
	if runtimeID == "" || h.store == nil {
		return false
	}
	for _, acc := range h.store.Accounts() {
		if acc.IsCodexAgentIdentity() && strings.EqualFold(strings.TrimSpace(acc.AgentRuntimeID), runtimeID) {
			return true
		}
	}
	return false
}

// createAgentIdentityAccount 用解析出的字段建号并加载进池，返回账号 ID。
func (h *Handler) createAgentIdentityAccount(ctx context.Context, fields *agentIdentityFields, name, proxyURL, source string) (int64, error) {
	planType := fields.PlanType
	if planType == "" {
		planType = "free"
	}
	credentials := map[string]interface{}{
		"auth_mode":                  auth.CodexAuthModeAgentIdentity,
		"agent_runtime_id":           fields.RuntimeID,
		"agent_private_key":          fields.PrivateKey,
		"account_id":                 fields.AccountID,
		"chatgpt_user_id":            fields.UserID,
		"email":                      fields.Email,
		"plan_type":                  planType,
		"chatgpt_account_is_fedramp": fields.FedRAMP,
	}
	if fields.TaskID != "" {
		credentials["task_id"] = fields.TaskID
	}

	name = strings.TrimSpace(name)
	if name == "" {
		if fields.Email != "" {
			name = fields.Email
		} else {
			name = "agent-identity"
		}
	}

	id, err := h.db.InsertAccountWithCredentials(ctx, name, credentials, proxyURL)
	if err != nil {
		return 0, err
	}
	h.db.InsertAccountEventAsync(id, "added", source)
	if err := h.store.LoadAccountByID(ctx, id); err != nil {
		return 0, err
	}
	// 导入后异步用量探针：Agent Identity 走 /responses 签名探针，从响应头填充用量进度条。
	h.triggerImportedAccountUsageProbe(id, source)
	return id, nil
}

// importAgentIdentityTokens 处理通用导入(JSON 文件/文件夹扫描)里识别出的 Agent Identity
// 条目：校验私钥、按 runtime_id 去重(批内 + 现有；allowDuplicate 时跳过去重)、建号入池。
// 返回新增/重复跳过/失败计数。
func (h *Handler) importAgentIdentityTokens(ctx context.Context, tokens []importToken, proxyURL string, allowDuplicate bool) (success, duplicate, failed int) {
	if len(tokens) == 0 {
		return 0, 0, 0
	}
	seen := make(map[string]struct{})
	for _, t := range tokens {
		fields := &agentIdentityFields{
			RuntimeID:  strings.TrimSpace(t.agentRuntimeID),
			PrivateKey: strings.TrimSpace(t.agentPrivateKey),
			TaskID:     strings.TrimSpace(t.agentTaskID),
			AccountID:  strings.TrimSpace(t.accountID),
			UserID:     strings.TrimSpace(t.chatgptUserID),
			Email:      strings.TrimSpace(t.email),
			PlanType:   strings.TrimSpace(t.planType),
			FedRAMP:    t.agentFedRAMP,
		}
		if err := auth.ValidateCodexAgentIdentityPrivateKey(fields.PrivateKey); err != nil {
			failed++
			continue
		}
		runtimeKey := strings.ToLower(fields.RuntimeID)
		if !allowDuplicate {
			if _, dup := seen[runtimeKey]; dup || h.agentIdentityRuntimeExists(fields.RuntimeID) {
				duplicate++
				continue
			}
		}
		if _, err := h.createAgentIdentityAccount(ctx, fields, t.name, proxyURL, "import_agent_identity"); err != nil {
			failed++
			continue
		}
		seen[runtimeKey] = struct{}{}
		success++
	}
	return success, duplicate, failed
}

// batchImportAgentIdentityReq 是 Agent Identity 文件批量导入的请求体。
// Files 每项是一个 auth.json 的原始 JSON 内容。
type batchImportAgentIdentityReq struct {
	Files    []string `json:"files"`
	ProxyURL string   `json:"proxy_url"`
}

type agentIdentityImportItem struct {
	Email string `json:"email,omitempty"`
	ID    int64  `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

const agentIdentityBatchMaxFiles = 200

// BatchImportCodexAgentIdentity 批量导入 Agent Identity auth.json 文件。
// POST /api/admin/accounts/codex/agent-identity/import
func (h *Handler) BatchImportCodexAgentIdentity(c *gin.Context) {
	var req batchImportAgentIdentityReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	if len(req.Files) == 0 {
		writeError(c, http.StatusBadRequest, "未提供任何文件内容")
		return
	}
	if len(req.Files) > agentIdentityBatchMaxFiles {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多导入 %d 个文件", agentIdentityBatchMaxFiles))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	seenRuntimes := make(map[string]struct{})
	items := make([]agentIdentityImportItem, 0, len(req.Files))
	imported := 0
	for _, content := range req.Files {
		item := agentIdentityImportItem{}
		fields, err := parseAgentIdentityAuthJSON(content)
		if err != nil {
			item.Error = err.Error()
			items = append(items, item)
			continue
		}
		item.Email = fields.Email
		runtimeKey := strings.ToLower(strings.TrimSpace(fields.RuntimeID))
		if _, dup := seenRuntimes[runtimeKey]; dup || h.agentIdentityRuntimeExists(fields.RuntimeID) {
			item.Error = "账号已存在，已跳过"
			items = append(items, item)
			continue
		}
		id, err := h.createAgentIdentityAccount(ctx, fields, "", req.ProxyURL, "agent_identity_file_import")
		if err != nil {
			item.Error = err.Error()
			items = append(items, item)
			continue
		}
		seenRuntimes[runtimeKey] = struct{}{}
		item.OK = true
		item.ID = id
		items = append(items, item)
		imported++
	}

	security.SecurityAuditLog("CODEX_AGENT_IDENTITY_FILE_IMPORTED", fmt.Sprintf("total=%d imported=%d ip=%s", len(req.Files), imported, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"total":    len(req.Files),
		"imported": imported,
		"failed":   len(req.Files) - imported,
		"items":    items,
	})
}
