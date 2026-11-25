package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
	"github.com/gin-gonic/gin"
)

type reauthIdentity struct{ user, workspace string }

func (i reauthIdentity) valid() bool { return i.user != "" && i.workspace != "" }
func scopedIdentity(a database.KeyUsageAccountAccess) reauthIdentity {
	if a.Platform != "openai" || a.Type != "oauth" || !a.HasRefreshToken || (a.UpstreamType != "" && a.UpstreamType != "codex") {
		return reauthIdentity{}
	}
	workspace := a.WorkspaceID
	if workspace == "" {
		workspace = a.AccountID
	} else if a.AccountID != "" && a.AccountID != workspace {
		return reauthIdentity{}
	}
	return reauthIdentity{strings.TrimSpace(a.UserID), openaiidentity.NormalizeWorkspaceID(workspace)}
}

// A complete stable pair must appear together in at least one issuer-returned
// token. Reject contradictions instead of merging claims from different subjects.
func sameExchangedIdentity(tokens *rawOAuthTokenResp, want reauthIdentity) bool {
	if !want.valid() {
		return false
	}
	complete := false
	check := func(user, workspace string) bool {
		if user != "" && user != want.user || workspace != "" && workspace != want.workspace {
			return false
		}
		if user == want.user && workspace == want.workspace {
			complete = true
		}
		return true
	}
	if id := auth.ParseIDToken(tokens.IDToken); id != nil && !check(id.UserID, id.ChatGPTAccountID) {
		return false
	}
	if at := auth.ParseAccessToken(tokens.AccessToken); at != nil && !check(at.UserID, at.ChatGPTAccountID) {
		return false
	}
	return complete
}

type scopedSession struct {
	keyID, targetID int64
	keyDigest       [32]byte
	identity        reauthIdentity
	state, verifier string
	expires         time.Time
}

// Separate store: admin polling, callback and new-account submission cannot claim these sessions.
type scopedReauthStore struct {
	sync.Mutex
	sessions map[string]scopedSession
}

func (s *scopedReauthStore) add(id string, v scopedSession) bool {
	s.Lock()
	defer s.Unlock()
	now := time.Now()
	if s.sessions == nil {
		s.sessions = make(map[string]scopedSession)
	}
	for k, old := range s.sessions {
		if !now.Before(old.expires) || old.keyID == v.keyID && old.targetID == v.targetID {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= 4096 {
		return false
	}
	s.sessions[id] = v
	return true
}
func (s *scopedReauthStore) claim(id, state string, key, target int64, keyDigest [32]byte, identity reauthIdentity) (scopedSession, bool) {
	s.Lock()
	defer s.Unlock()
	v, ok := s.sessions[id]
	if !ok || !time.Now().Before(v.expires) || v.state != state || v.keyDigest != keyDigest || v.keyID != key || v.targetID != target || v.identity != identity {
		return scopedSession{}, false
	}
	delete(s.sessions, id)
	return v, true
}

type scopedCodeExchange func(context.Context, string, string, string, string, ...string) (*rawOAuthTokenResp, *auth.AccountInfo, error)

func (h *Handler) reauthTarget(c *gin.Context, ctx context.Context) (*database.APIKeyRow, *database.AccountRow, reauthIdentity) {
	c.Header("Cache-Control", "no-store")
	var none reauthIdentity
	key := h.publicAPIKeyUsageRow(c, ctx)
	if key == nil {
		return nil, nil, none
	}
	if key.IsExpired(time.Now()) {
		writeError(c, 403, "API Key 已过期")
		return nil, nil, none
	}
	policy, err := h.keyUsagePermissions(ctx, key.ID)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return nil, nil, none
	}
	if !policy.ViewEnabled {
		writeError(c, 404, "账号用量查询未启用")
		return nil, nil, none
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, 400, "无效的账号 ID")
		return nil, nil, none
	}
	if h.store == nil {
		writeError(c, 503, "账号池未就绪")
		return nil, nil, none
	}
	row, err := h.db.GetAccountByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, 404, "账号不存在或无权访问")
		return nil, nil, none
	}
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return nil, nil, none
	}
	a := database.KeyUsageAccountAccess{Platform: row.Platform, Type: row.Type, UpstreamType: row.GetCredential("upstream_type"), PlanType: row.GetCredential("plan_type"), UserID: row.GetCredential("user_id"), WorkspaceID: row.GetCredential("workspace_id"), AccountID: row.GetCredential("account_id"), HasRefreshToken: row.GetCredential("refresh_token") != ""}
	// Match the list projection's strict decoding: malformed restrictions never mean unrestricted.
	if raw, ok := row.Credentials["allowed_api_key_ids"]; ok {
		b, e := json.Marshal(raw)
		if e != nil || json.Unmarshal(b, &a.AllowedAPIKeyIDs) != nil {
			writeError(c, 404, "账号不存在或无权访问")
			return nil, nil, none
		}
	}
	groups, err := h.db.GetAccountGroupIDs(ctx, id)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return nil, nil, none
	}
	runtime := h.store.FindByID(id)
	if !h.keyUsageAccountVisible(key, a, runtime, groups) {
		writeError(c, 404, "账号不存在或无权访问")
		return nil, nil, none
	}
	identity := scopedIdentity(a)
	if !identity.valid() {
		writeError(c, 409, "该账号不支持自助重新授权")
		return nil, nil, none
	}
	return key, row, identity
}

func scopedJSON(c *gin.Context, target any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16384))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(c, 400, "请求格式错误")
		return false
	}
	return true
}
func (h *Handler) startScopedReauth(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), publicAPIKeyUsageTimeout)
	defer cancel()
	key, row, identity := h.reauthTarget(c, ctx)
	if key == nil {
		return
	}
	if !scopedJSON(c, &struct{}{}) {
		return
	}
	state, err := oauthRandomHex(32)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	verifier, err := oauthRandomHex(64)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	session, err := oauthRandomHex(16)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	url, err := buildOAuthAuthorizeURL(oauthDefaultRedirectURI, state, verifier)
	if err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	if !h.reauth.add(session, scopedSession{keyID: key.ID, targetID: row.ID, keyDigest: sha256.Sum256([]byte(extractPublicAPIKey(c))), identity: identity, state: state, verifier: verifier, expires: time.Now().Add(5 * time.Minute)}) {
		writeError(c, 429, "请稍后再试")
		return
	}
	c.JSON(200, gin.H{"auth_url": url, "session_id": session})
}
func (h *Handler) completeScopedReauth(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 35*time.Second)
	defer cancel()
	key, row, identity := h.reauthTarget(c, ctx)
	if key == nil {
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
		Code      string `json:"code"`
	}
	if !scopedJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeError(c, 400, "缺少授权码")
		return
	}
	sess, ok := h.reauth.claim(req.SessionID, req.State, key.ID, row.ID, sha256.Sum256([]byte(extractPublicAPIKey(c))), identity)
	if !ok {
		writeError(c, 400, "授权会话无效或已过期")
		return
	}
	proxyURL := row.ProxyURL
	if proxyURL == "" {
		proxyURL = h.store.GetProxyURL()
	}
	exchange := h.reauthExchange
	if exchange == nil {
		exchange = doOAuthCodeExchange
	}
	tokens, _, err := exchange(ctx, req.Code, sess.verifier, oauthDefaultRedirectURI, proxyURL, strconv.FormatInt(row.ID, 10))
	if err != nil || tokens == nil || strings.TrimSpace(tokens.RefreshToken) == "" || strings.TrimSpace(tokens.AccessToken) == "" {
		writeError(c, 502, "授权码兑换失败")
		return
	}
	// Only tokens returned by the server's PKCE exchange are accepted. Neither email nor a shared workspace alone identifies the owner.
	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{refreshToken: tokens.RefreshToken, accessToken: tokens.AccessToken, idToken: tokens.IDToken, expiresIn: tokens.ExpiresIn})
	if !sameExchangedIdentity(tokens, identity) || seed.userID != identity.user || seed.accountID != identity.workspace || (seed.workspaceID != "" && seed.workspaceID != identity.workspace) {
		writeError(c, 409, "授权身份与目标账号不匹配")
		return
	}
	h.mergeDuplicateMu.Lock()
	defer h.mergeDuplicateMu.Unlock()
	current, target, currentIdentity := h.reauthTarget(c, ctx)
	if current == nil {
		return
	}
	if current.ID != sess.keyID || currentIdentity != sess.identity {
		writeError(c, 409, "账号授权已变更")
		return
	}
	seed.customHeaders = target.GetCredentialStringMap("custom_headers")
	if duplicate, err := h.findOAuthIdentityDuplicate(ctx, seed, target.ID); err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	} else if duplicate > 0 {
		writeError(c, 409, "授权身份与其他账号冲突")
		return
	}
	seed.customHeaders = nil // routing configuration is never part of a scoped update
	if err := h.db.UpdateScopedOAuthCredentials(ctx, target, tokenCredentialMap(seed)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, 409, "账号授权已变更")
		} else {
			writePublicKeyUsageInternalError(c, err)
		}
		return
	}
	if accountErrorStateNeedsReset(target) {
		h.store.ForgetCachedAccountCooldown(target.ID)
	}
	h.store.RemoveAccount(target.ID)
	if err := h.store.LoadAccountByID(ctx, target.ID); err != nil {
		writePublicKeyUsageInternalError(c, err)
		return
	}
	h.db.InsertAccountEventAsync(target.ID, "updated", "key_usage_reauth")
	c.JSON(200, gin.H{"id": target.ID})
}
