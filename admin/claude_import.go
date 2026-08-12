package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// ==================== Claude 凭据文件导入 ====================
//
// 交互式授权一次只能加一个号，而池子通常是从别处（Claude Code 的
// ~/.claude/.credentials.json、各类导出脚本）批量拿到凭据的。这条路径与 Grok 的
// auth.json 导入同构：解析 → 刷新一次 AT → bootstrap 探针补身份 → 落库入池。

type importClaudeAccountsReq struct {
	// Files 每项是一份凭据内容（单对象 / 数组 / 每行一个都接受）。
	Files []string `json:"files"`
	// Content 是单份粘贴内容的便捷入口，与 Files 合并处理。
	Content  string   `json:"content"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
	ProxyURL string   `json:"proxy_url"`
	// GroupIDs 让导入时就把新账号绑进指定分组。
	GroupIDs json.RawMessage `json:"group_ids"`
}

type claudeImportItem struct {
	Email string `json:"email,omitempty"`
	ID    int64  `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Warning 用于「导入成功但很可能用不了」的情况：scope 不足或 RT 刷新失败。
	Warning string `json:"warning,omitempty"`
}

const claudeImportMaxCredentials = 2000

// 导入是串行落库 + 每个号一次 token 刷新与一次探针，墙钟时间随数量线性增长；
// 按条数缩放超时，封顶避免一个超大请求无限占住连接（与 Grok 导入同策略）。
const (
	claudeImportBaseTimeout    = 30 * time.Second
	claudeImportPerItemTimeout = 15 * time.Second
	claudeImportMaxTimeout     = 10 * time.Minute
)

func claudeImportTimeout(items int) time.Duration {
	if items < 0 {
		items = 0
	}
	timeout := claudeImportBaseTimeout + time.Duration(items)*claudeImportPerItemTimeout
	if timeout > claudeImportMaxTimeout {
		return claudeImportMaxTimeout
	}
	return timeout
}

// ImportClaudeAccounts 从 Claude Code 凭据文件导入账号
// （POST /api/admin/accounts/claude/import）。
func (h *Handler) ImportClaudeAccounts(c *gin.Context) {
	var req importClaudeAccountsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	baseURL, err := auth.NormalizeClaudeBaseURL(req.BaseURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	models := auth.NormalizeAccountModels(req.Models)
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}

	payloads := make([]string, 0, len(req.Files)+1)
	for _, file := range req.Files {
		if strings.TrimSpace(file) != "" {
			payloads = append(payloads, file)
		}
	}
	if strings.TrimSpace(req.Content) != "" {
		payloads = append(payloads, req.Content)
	}
	if len(payloads) == 0 {
		writeError(c, http.StatusBadRequest, "未提供任何凭据内容")
		return
	}

	// 先整份解析：一份文件里的语法错误应该立刻回报，而不是导到一半才发现。
	items := make([]claudeImportItem, 0)
	credentials := make([]auth.ClaudeCredential, 0, len(payloads))
	for i, payload := range payloads {
		parsed, parseErr := auth.ParseClaudeCredentialsJSON([]byte(payload))
		if parseErr != nil {
			items = append(items, claudeImportItem{
				Error: fmt.Sprintf("第 %d 份内容解析失败: %s", i+1, parseErr.Error()),
			})
			continue
		}
		credentials = append(credentials, parsed...)
	}
	if len(credentials) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"total": len(items), "imported": 0, "failed": len(items), "items": items,
		})
		return
	}
	if len(credentials) > claudeImportMaxCredentials {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多导入 %d 个账号", claudeImportMaxCredentials))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), claudeImportTimeout(len(credentials)))
	defer cancel()
	groupIDs, err := h.resolveImportGroupIDsJSON(ctx, req.GroupIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	// 去重：同一个账号只能在池里出现一次，两行争着消费同一个 refresh_token 时，
	// 上游轮换后会互相把对方踢成 invalid_grant。
	// 运行时账号不暴露 refresh_token（没有取值方法，直接读字段要拿 Account.mu），
	// 所以跨请求去重用 account_uuid + email，批内再按 refresh_token 补一层。
	existingRefreshTokens := make(map[string]struct{})
	existingAccountUUIDs := make(map[string]struct{})
	existingEmails := make(map[string]struct{})
	if h.store != nil {
		for _, acc := range h.store.Accounts() {
			if !acc.IsClaudeAPI() {
				continue
			}
			if uuid := acc.GetClaudeAccountUUID(); uuid != "" {
				existingAccountUUIDs[uuid] = struct{}{}
			}
			acc.Mu().RLock()
			email := strings.ToLower(strings.TrimSpace(acc.Email))
			acc.Mu().RUnlock()
			if email != "" {
				existingEmails[email] = struct{}{}
			}
		}
	}

	imported := 0
	createdIDs := make([]int64, 0, len(credentials))
	for i, cred := range credentials {
		item := claudeImportItem{Email: cred.Email}
		if _, dup := existingRefreshTokens[strings.TrimSpace(cred.RefreshToken)]; dup {
			item.Error = "账号已存在，已跳过"
			items = append(items, item)
			continue
		}
		if cred.AccountUUID != "" {
			if _, dup := existingAccountUUIDs[cred.AccountUUID]; dup {
				item.Error = "账号已存在，已跳过"
				items = append(items, item)
				continue
			}
		}
		credEmail := strings.ToLower(strings.TrimSpace(cred.Email))
		if credEmail != "" {
			if _, dup := existingEmails[credEmail]; dup {
				item.Error = "账号已存在，已跳过"
				items = append(items, item)
				continue
			}
		}

		// 文件里的 AT 通常已经过期（导出到导入之间隔了几天），先用 RT 换一份新的：
		// 紧接着的 bootstrap 探针需要一个能用的 AT，且账号入池即可调度。
		token, refreshErr := auth.RefreshClaudeAccessToken(ctx, auth.ClaudeRefreshParams{
			RefreshToken: cred.RefreshToken,
			ProxyURL:     req.ProxyURL,
		})
		if refreshErr != nil {
			// invalid_grant / invalid_client 是永久失败：RT 已被上游作废，这个号无论
			// AT 还剩多久都活不过一小时，导进来只会变成一行 error。直接拒。
			if auth.IsClaudeRefreshPermanentError(refreshErr) ||
				cred.AccessToken == "" || cred.ExpiresAt.IsZero() || time.Now().After(cred.ExpiresAt) {
				item.Error = "刷新凭据失败: " + refreshErr.Error()
				items = append(items, item)
				continue
			}
			// RT 刷新失败但文件里的 AT 还没过期：先入池用着，后台刷新会再试一次；
			// 真失效了会转 error 状态，比直接丢掉整个号更有用。
			token = &auth.ClaudeTokenData{
				AccessToken:  cred.AccessToken,
				RefreshToken: cred.RefreshToken,
				Scope:        cred.Scope,
				ExpiresAt:    cred.ExpiresAt,
			}
			item.Warning = "刷新凭据失败，暂用文件内的 access_token: " + refreshErr.Error()
		}
		if token.RefreshToken == "" {
			token.RefreshToken = cred.RefreshToken
		}
		if token.Scope == "" {
			token.Scope = cred.Scope
		}

		name := strings.TrimSpace(cred.Email)
		if name == "" {
			name = fmt.Sprintf("claude-%d", i+1)
		}
		id, email, createErr := h.createClaudeOAuthAccount(ctx, createClaudeOAuthAccountInput{
			Name:                     name,
			ProxyURL:                 req.ProxyURL,
			BaseURL:                  baseURL,
			Models:                   models,
			Token:                    token,
			Source:                   "claude_file_import",
			FallbackEmail:            cred.Email,
			FallbackPlanType:         auth.ClaudePlanFromSubscriptionType(cred.SubscriptionType),
			FallbackAccountUUID:      cred.AccountUUID,
			FallbackOrganizationUUID: cred.OrganizationUUID,
		})
		if createErr != nil {
			item.Error = createErr.Error()
			items = append(items, item)
			continue
		}

		existingRefreshTokens[strings.TrimSpace(token.RefreshToken)] = struct{}{}
		existingRefreshTokens[strings.TrimSpace(cred.RefreshToken)] = struct{}{}
		if cred.AccountUUID != "" {
			existingAccountUUIDs[cred.AccountUUID] = struct{}{}
		}
		if email != "" {
			item.Email = email
		}
		if resolved := strings.ToLower(strings.TrimSpace(item.Email)); resolved != "" {
			existingEmails[resolved] = struct{}{}
		}
		// scope 缺少推理权限的凭据（只有 user:profile 之类）建号能成，但真实请求会被
		// 上游拒；建号那一刻就说清楚，省得用户以后去查一个 403。
		if !auth.ClaudeSubscriptionScopeSufficient(token.Scope) {
			item.Warning = strings.TrimSpace(item.Warning + " 凭据 scope 缺少 user:inference，上游可能拒绝推理请求。")
		}
		item.OK = true
		item.ID = id
		items = append(items, item)
		imported++
		createdIDs = append(createdIDs, id)
	}

	security.SecurityAuditLog("CLAUDE_FILE_IMPORTED", fmt.Sprintf("total=%d imported=%d ip=%s", len(credentials), imported, c.ClientIP()))
	response := gin.H{
		"total":     len(items),
		"imported":  imported,
		"failed":    len(items) - imported,
		"items":     items,
		"group_ids": groupIDs,
	}
	if err := h.bindImportedAccountGroups(ctx, createdIDs, groupIDs); err != nil {
		response["group_bind_error"] = err.Error()
	}
	c.JSON(http.StatusOK, response)
}
