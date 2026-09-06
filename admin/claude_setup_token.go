package admin

// Claude 账号的两条补充授权入口:
//   1. sessionKey(cookie)一键换号:POST /accounts/claude/oauth/exchange-session-key
//      用户从已登录的 claude.ai 复制 sessionKey,服务端代替浏览器跑完 OAuth 三步,
//      可选择换出可刷新 OAuth 凭据或长效 Setup Token;
//   2. 令牌批量粘贴导入:POST /accounts/claude/import-tokens(旧名 import-setup-tokens)
//      直接吃官方 `claude setup-token` 产出的 sk-ant-oat01-… 长效令牌,以及
//      sk-ant-ort01-… OAuth refresh token(入库前先刷新换出 AT),多枚任意分隔。
//
// 两条入口最终都汇入 createClaudeAccount,与网页 OAuth / JSON 导入共享查重、指纹、
// 分组、预热等后续流程。

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// exchangeClaudeSessionKeyReq 是 sessionKey 一键换号请求体。
type exchangeClaudeSessionKeyReq struct {
	SessionKey string `json:"session_key"`
	// Mode:oauth(默认)或 setup_token。
	Mode         string `json:"mode"`
	Name         string `json:"name"`
	ProxyURL     string `json:"proxy_url"`
	UseProxyPool bool   `json:"use_proxy_pool"`
	Timezone     string `json:"timezone"`
}

// ExchangeClaudeSessionKey 用 claude.ai sessionKey 一键换取凭据并入库。
func (h *Handler) ExchangeClaudeSessionKey(c *gin.Context) {
	var req exchangeClaudeSessionKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	sessionKey := auth.NormalizeClaudeSessionKey(req.SessionKey)
	if sessionKey == "" {
		writeError(c, http.StatusBadRequest, "session_key 为必填")
		return
	}
	setupToken, err := parseClaudeAuthMode(req.Mode)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	proxyURL, err := h.resolveClaudeLoginProxy(req.ProxyURL, req.UseProxyPool)
	if err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	td, err := auth.NewClaudeAuth(proxyURL).ExchangeSessionKey(ctx, sessionKey, setupToken)
	if err != nil {
		writeError(c, http.StatusBadGateway, "sessionKey 换取凭据失败: "+err.Error())
		return
	}
	source := "manual_claude_session_key"
	if setupToken {
		source = "manual_claude_session_key_setup_token"
	}
	h.insertClaudeAccount(c, ctx, req.Name, proxyURL, req.Timezone, td, source)
}

// refreshClaudeCredentialsForImport 用裸 RT 换出 AT 与身份(邮箱/账号 UUID/套餐)。
// 测试可通过 Handler.refreshClaudeTokensForImport 注入,避免真打 platform.claude.com。
func (h *Handler) refreshClaudeCredentialsForImport(ctx context.Context, proxyURL, refreshToken string) (*auth.ClaudeTokenData, error) {
	if h != nil && h.refreshClaudeTokensForImport != nil {
		return h.refreshClaudeTokensForImport(ctx, proxyURL, refreshToken)
	}
	return auth.NewClaudeAuth(proxyURL).RefreshTokens(ctx, refreshToken)
}

// importClaudeSetupTokensReq 是令牌批量粘贴导入请求体。text 与 tokens 可同时给,
// 服务端统一按前缀抽取并去重:sk-ant-oat01- 按 Setup Token(长效)入库,
// sk-ant-ort01- 按 OAuth refresh token 入库(入库前先刷新换出 AT)。
type importClaudeSetupTokensReq struct {
	Text   string   `json:"text"`
	Tokens []string `json:"tokens"`
	// Name 作为备注前缀;空=沿用现有 claude-N 序号继续编号。单枚令牌时直接用作备注。
	Name         string           `json:"name"`
	ProxyURL     string           `json:"proxy_url"`
	UseProxyPool bool             `json:"use_proxy_pool"`
	Timezone     string           `json:"timezone"`
	GroupRefs    []claudeGroupRef `json:"group_refs"`
}

// claudeSetupTokenImportMaxEntries 单次粘贴上限,防止误贴整份日志。
const claudeSetupTokenImportMaxEntries = 200

// ImportClaudeSetupTokens 批量导入 Setup Token。
func (h *Handler) ImportClaudeSetupTokens(c *gin.Context) {
	var req importClaudeSetupTokensReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	text := req.Text
	if len(req.Tokens) > 0 {
		text += "\n" + strings.Join(req.Tokens, "\n")
	}
	setupTokens := auth.ExtractClaudeSetupTokens(text)
	refreshTokens := auth.ExtractClaudeRefreshTokens(text)
	type pastedToken struct {
		value    string
		authKind string
	}
	tokens := make([]pastedToken, 0, len(setupTokens)+len(refreshTokens))
	for _, token := range setupTokens {
		tokens = append(tokens, pastedToken{value: token, authKind: auth.ClaudeAuthKindSetupToken})
	}
	for _, token := range refreshTokens {
		tokens = append(tokens, pastedToken{value: token, authKind: auth.ClaudeAuthKindOAuth})
	}
	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "未找到 sk-ant-oat01-(Setup Token)或 sk-ant-ort01-(Refresh Token)开头的令牌")
		return
	}
	if len(tokens) > claudeSetupTokenImportMaxEntries {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多导入 %d 枚令牌", claudeSetupTokenImportMaxEntries))
		return
	}
	if strings.TrimSpace(req.ProxyURL) != "" {
		if err := security.ValidateProxyURL(strings.TrimSpace(req.ProxyURL)); err != nil {
			writeError(c, http.StatusBadRequest, "代理URL无效")
			return
		}
	}
	refs, err := normalizeClaudeGroupRefs(req.GroupRefs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), claudeImportTimeout(len(tokens)))
	defer cancel()

	resolvedGroupIDs, missingGroups, groupErr := h.resolveClaudeGroupRefs(ctx, refs)
	if groupErr != nil {
		writeError(c, http.StatusInternalServerError, "分组映射失败: "+groupErr.Error())
		return
	}
	nextIndex := h.nextClaudeSetupTokenIndex(ctx, req.Name)

	items := make([]claudeImportResultItem, 0, len(tokens))
	imported := 0
	setupIndex := 0
	for _, token := range tokens {
		item := claudeImportResultItem{}
		// 代理:显式 proxy_url 对全部令牌固定;use_proxy_pool 时每枚令牌各取一条,
		// 避免一批号挤在同一出口。
		proxyURL, perr := h.resolveClaudeLoginProxy(req.ProxyURL, req.UseProxyPool)
		if perr != nil {
			item.Error = "代理URL无效"
			item.status = http.StatusBadRequest
			items = append(items, item)
			continue
		}
		// 备注:Setup Token 没有身份信息,按 <prefix>-N 编号;Refresh Token 刷新后能拿到
		// 邮箱,留空让 createClaudeAccount 回退到邮箱(单枚且用户给了备注时直接采用)。
		name := req.Name
		var td *auth.ClaudeTokenData
		source := "manual_claude_refresh_token_import"
		if token.authKind == auth.ClaudeAuthKindSetupToken {
			if len(tokens) > 1 || name == "" {
				name = claudeSetupTokenAccountName(req.Name, nextIndex+setupIndex)
			}
			setupIndex++
			td = &auth.ClaudeTokenData{
				AccessToken: token.value,
				ExpiresAt:   time.Now().Add(auth.ClaudeSetupTokenLifetime),
				AuthKind:    auth.ClaudeAuthKindSetupToken,
			}
			source = "manual_claude_setup_token_import"
		} else {
			if len(tokens) > 1 {
				name = ""
			}
			td = &auth.ClaudeTokenData{RefreshToken: token.value, AuthKind: auth.ClaudeAuthKindOAuth}
		}
		created, createErr := h.createClaudeAccount(ctx, name, proxyURL, req.Timezone, td, source, &claudeAccountImportOptions{
			AuthKind:         token.authKind,
			GroupRefs:        refs,
			ResolvedGroupIDs: resolvedGroupIDs,
			SkipModelFetch:   len(tokens) > 1,
		})
		if createErr != nil {
			item.Error = createErr.Error()
			if typedErr, ok := createErr.(*claudeAccountCreateError); ok {
				item.status = typedErr.Status
			}
			items = append(items, item)
			continue
		}
		imported++
		item.OK = true
		item.ID = created.ID
		item.Email = created.Email
		item.Warnings = append(item.Warnings, created.Warnings...)
		if len(missingGroups) > 0 {
			item.Warnings = append(item.Warnings, "部分分组未找到: "+strings.Join(missingGroups, ", "))
		}
		security.SecurityAuditLog("CLAUDE_TOKEN_IMPORTED", fmt.Sprintf("account_id=%d kind=%s ip=%s", created.ID, token.authKind, c.ClientIP()))
		items = append(items, item)
	}
	if len(tokens) == 1 && !items[0].OK {
		status := items[0].status
		if status <= 0 {
			status = http.StatusInternalServerError
		}
		writeError(c, status, items[0].Error)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"total": len(tokens), "imported": imported, "failed": len(tokens) - imported, "items": items,
	})
}

// claudeSetupTokenNamePrefix 是自动编号备注的默认前缀。
const claudeSetupTokenNamePrefix = "claude"

// claudeSetupTokenAccountName 生成 `<prefix>-N` 形式的备注。
func claudeSetupTokenAccountName(prefix string, index int) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = claudeSetupTokenNamePrefix
	}
	return prefix + "-" + strconv.Itoa(index)
}

// nextClaudeSetupTokenIndex 扫描现有 Claude 账号里 `<prefix>-N` 形式的备注,返回最大 N+1;
// 没有则从 1 开始。读库失败时也从 1 开始(备注允许重复,不影响正确性)。
func (h *Handler) nextClaudeSetupTokenIndex(ctx context.Context, prefix string) int {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = claudeSetupTokenNamePrefix
	}
	if h == nil || h.db == nil {
		return 1
	}
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelClaude)
	if err != nil {
		return 1
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			names = append(names, row.Name)
		}
	}
	return nextClaudeIndexedName(names, prefix)
}

// nextClaudeIndexedName 在名字集合里找 `<prefix>-N`(前缀不区分大小写)的最大 N,返回 N+1。
func nextClaudeIndexedName(names []string, prefix string) int {
	maxIndex := 0
	lowerPrefix := strings.ToLower(prefix) + "-"
	for _, name := range names {
		name = strings.TrimSpace(name)
		if len(name) <= len(lowerPrefix) || !strings.HasPrefix(strings.ToLower(name), lowerPrefix) {
			continue
		}
		n, err := strconv.Atoi(name[len(lowerPrefix):])
		if err != nil || n <= 0 {
			continue
		}
		if n > maxIndex {
			maxIndex = n
		}
	}
	return maxIndex + 1
}

// claudeAuthKindForRow 返回行的 Claude 凭据形态(非 Claude 行返回空),供列表/详情投影使用。
func claudeAuthKindForRow(row *database.AccountRow, isClaude bool) string {
	if row == nil || !isClaude {
		return ""
	}
	return auth.InferClaudeAuthKind(row.GetCredential(auth.ClaudeAuthKindCredentialKey), row.GetCredential("access_token"), row.GetCredential("refresh_token"))
}
