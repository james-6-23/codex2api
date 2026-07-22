package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// importGrokSSOReq 是 SSO 批量导入的请求体。
// Tokens 是原始导入内容（JSON {"accounts":[...]} 或每行一个 sso token 的纯文本）。
type importGrokSSOReq struct {
	Tokens   string   `json:"tokens"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
	ProxyURL string   `json:"proxy_url"`
}

type grokSSOImportItem struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	ID    int64  `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

const (
	grokSSOImportMaxTokens = 50
	// SSO 每条要跑完整个 device flow（多次往返），并发略高于 refresh 以缩短总墙钟。
	grokSSOImportConcurrent = 6
	grokSSOImportPerToken   = 75 * time.Second
)

// ImportGrokSSO 批量把 Grok Web 的 sso token 转成 Build(OAuth) 账号并入库。
// POST /api/admin/accounts/grok/sso/import
func (h *Handler) ImportGrokSSO(c *gin.Context) {
	var req importGrokSSOReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	baseURL, err := auth.NormalizeGrokBaseURL(req.BaseURL)
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

	seeds, err := auth.ParseGrokSSOTokens([]byte(req.Tokens))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(seeds) > grokSSOImportMaxTokens {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多导入 %d 个 sso token", grokSSOImportMaxTokens))
		return
	}

	// 每个 token 独立超时，有界并发转换；不复用请求 ctx（请求可能先于转换结束）。
	items := make([]grokSSOImportItem, len(seeds))
	sem := make(chan struct{}, grokSSOImportConcurrent)
	var wg sync.WaitGroup
	// 按 subject 去重：预载已有 Grok 账号，锁保护并发读写，避免重复导入同一账号。
	var mu sync.Mutex
	seenSubjects := make(map[string]struct{})
	if h.store != nil {
		for _, acc := range h.store.Accounts() {
			if acc.IsGrokAPI() {
				if sub := strings.TrimSpace(acc.GrokUserID()); sub != "" {
					seenSubjects[sub] = struct{}{}
				}
			}
		}
	}
	for i, seed := range seeds {
		wg.Add(1)
		go func(idx int, s auth.GrokSSOSeed) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			item := grokSSOImportItem{Name: s.Name, Email: s.Email}
			ctx, cancel := context.WithTimeout(context.Background(), grokSSOImportPerToken)
			defer cancel()

			result, convErr := auth.ConvertGrokSSOToBuild(ctx, s.Token, req.ProxyURL)
			if convErr != nil {
				item.Error = convErr.Error()
				items[idx] = item
				return
			}

			mu.Lock()
			if result.Subject != "" {
				if _, dup := seenSubjects[result.Subject]; dup {
					mu.Unlock()
					item.Error = "账号已存在，已跳过"
					items[idx] = item
					return
				}
				seenSubjects[result.Subject] = struct{}{}
			}
			mu.Unlock()

			name := strings.TrimSpace(s.Name)
			if utf8.RuneCountInString(name) > 100 {
				name = string([]rune(name)[:100])
			}
			email := strings.TrimSpace(s.Email)
			if email == "" {
				email = result.Email
			}
			id, createdEmail, createErr := h.createGrokOAuthAccount(ctx, createGrokOAuthAccountInput{
				Name:          name,
				ProxyURL:      req.ProxyURL,
				BaseURL:       baseURL,
				Models:        models,
				Token:         result.Token,
				Subject:       result.Subject,
				Email:         email,
				TokenEndpoint: auth.GrokDefaultTokenURL,
				Source:        "sso_import",
			})
			if createErr != nil {
				item.Error = createErr.Error()
				items[idx] = item
				return
			}
			item.OK = true
			item.ID = id
			if createdEmail != "" {
				item.Email = createdEmail
			}
			items[idx] = item

			// 异步 billing 探针，与其它添加路径一致
			if h.probeUsage != nil {
				go func(accountID int64) {
					acc := h.store.FindByID(accountID)
					if acc == nil {
						return
					}
					probeCtx, probeCancel := context.WithTimeout(context.Background(), 25*time.Second)
					defer probeCancel()
					_ = h.probeUsage(probeCtx, acc)
				}(id)
			}
		}(i, seed)
	}
	wg.Wait()

	imported := 0
	for _, item := range items {
		if item.OK {
			imported++
		}
	}
	security.SecurityAuditLog("GROK_SSO_IMPORTED", fmt.Sprintf("total=%d imported=%d ip=%s", len(seeds), imported, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"total":    len(seeds),
		"imported": imported,
		"failed":   len(seeds) - imported,
		"items":    items,
	})
}

// importGrokRefreshReq 是 refresh_token 批量导入的请求体（Tokens 每行一个 refresh_token）。
type importGrokRefreshReq struct {
	Tokens   string   `json:"tokens"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
	ProxyURL string   `json:"proxy_url"`
}

const (
	grokRefreshImportMaxTokens = 200
	grokRefreshImportPerToken  = 30 * time.Second
)

// parseTokenLines 把纯文本按行拆成去重后的 token 列表（trim、跳过空行与 # 注释）。
func parseTokenLines(raw string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		token := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "\r"))
		if token == "" || strings.HasPrefix(token, "#") {
			continue
		}
		if _, dup := seen[token]; dup {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

// ImportGrokRefreshTokens 批量用 refresh_token 刷出 OAuth 账号并入库。
// POST /api/admin/accounts/grok/refresh/import
func (h *Handler) ImportGrokRefreshTokens(c *gin.Context) {
	var req importGrokRefreshReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	baseURL, err := auth.NormalizeGrokBaseURL(req.BaseURL)
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

	tokens := parseTokenLines(req.Tokens)
	if len(tokens) == 0 {
		writeError(c, http.StatusBadRequest, "未解析到有效的 refresh_token")
		return
	}
	if len(tokens) > grokRefreshImportMaxTokens {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多导入 %d 个 refresh_token", grokRefreshImportMaxTokens))
		return
	}

	items := make([]grokSSOImportItem, len(tokens))
	sem := make(chan struct{}, grokSSOImportConcurrent)
	var wg sync.WaitGroup
	// createGrokOAuthAccount 会写 store，去重锁保护 subject 集合的并发读写。
	var mu sync.Mutex
	seenSubjects := make(map[string]struct{})
	if h.store != nil {
		for _, acc := range h.store.Accounts() {
			if acc.IsGrokAPI() {
				if sub := strings.TrimSpace(acc.GrokUserID()); sub != "" {
					seenSubjects[sub] = struct{}{}
				}
			}
		}
	}

	for i, rt := range tokens {
		wg.Add(1)
		go func(idx int, refreshToken string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			item := grokSSOImportItem{}
			ctx, cancel := context.WithTimeout(context.Background(), grokRefreshImportPerToken)
			defer cancel()

			td, refreshErr := auth.RefreshGrokAccessToken(ctx, auth.GrokRefreshParams{
				RefreshToken: refreshToken,
				ProxyURL:     req.ProxyURL,
			})
			if refreshErr != nil {
				item.Error = refreshErr.Error()
				items[idx] = item
				return
			}
			// 上游未轮换时沿用原 refresh_token
			if strings.TrimSpace(td.RefreshToken) == "" {
				td.RefreshToken = refreshToken
			}
			email, subject := auth.GrokIdentityFromTokens(td.AccessToken, td.IDToken)
			item.Email = email

			mu.Lock()
			if subject != "" {
				if _, dup := seenSubjects[subject]; dup {
					mu.Unlock()
					item.Error = "账号已存在，已跳过"
					items[idx] = item
					return
				}
				seenSubjects[subject] = struct{}{}
			}
			mu.Unlock()

			id, createdEmail, createErr := h.createGrokOAuthAccount(ctx, createGrokOAuthAccountInput{
				ProxyURL:      req.ProxyURL,
				BaseURL:       baseURL,
				Models:        models,
				Token:         td,
				Subject:       subject,
				Email:         email,
				TokenEndpoint: auth.GrokDefaultTokenURL,
				Source:        "refresh_import",
			})
			if createErr != nil {
				item.Error = createErr.Error()
				items[idx] = item
				return
			}
			item.OK = true
			item.ID = id
			if createdEmail != "" {
				item.Email = createdEmail
			}
			items[idx] = item

			if h.probeUsage != nil {
				go func(accountID int64) {
					a := h.store.FindByID(accountID)
					if a == nil {
						return
					}
					probeCtx, probeCancel := context.WithTimeout(context.Background(), 25*time.Second)
					defer probeCancel()
					_ = h.probeUsage(probeCtx, a)
				}(id)
			}
		}(i, rt)
	}
	wg.Wait()

	imported := 0
	for _, item := range items {
		if item.OK {
			imported++
		}
	}
	security.SecurityAuditLog("GROK_REFRESH_IMPORTED", fmt.Sprintf("total=%d imported=%d ip=%s", len(tokens), imported, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"total":    len(tokens),
		"imported": imported,
		"failed":   len(tokens) - imported,
		"items":    items,
	})
}
