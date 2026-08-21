package proxy

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// x-codex-turn-state 是上游在响应中铸造的不透明回合状态 blob,客户端在同一
// 回合的后续请求原样回带。blob 与铸造账号的出站身份绑定,同账号回放自洽;
// 跨账号回放(failover 换号后客户端仍回带旧账号的 blob)是代理链独有、真实
// Codex 永远不会产生的矛盾信号。溯源表记录每个下游会话最近一次向客户端
// 下发该 blob 的账号,出站守卫据此剥离已知异账号的回带值。只剥离、不注入:
// 下游是真实 Codex 客户端,会按自身回合语义自行回带。
//
// 键使用 affinityKey(下游会话标识 + API Key,与账号粘性绑定同源),保证
// 同一段对话的记录/守卫两侧落在同一键上。无会话标识时不做跟踪(保持透传)。
const codexTurnStateProvenanceTTL = time.Hour

type codexTurnStateOrigin struct {
	accountID int64
	expiresAt time.Time
}

var (
	codexTurnStateOrigins sync.Map // affinityKey(string) -> codexTurnStateOrigin
	codexTurnStateWrites  atomic.Uint64
)

// relayCodexTurnStateResponseHeader 把上游响应的 turn-state 写入下游响应头,
// 并记录铸造账号。上游没有该头时主动清除 writer 上可能残留的上一 failover
// attempt 的值——否则换号重试后旧账号的 blob 会粘到新账号的响应上,正是本
// 文件要防止的跨账号矛盾。(流式响应一旦提交,对 writer 头的改动是无害空操作。)
func relayCodexTurnStateResponseHeader(c *gin.Context, affinityKey string, account *auth.Account, headers http.Header) {
	if c == nil {
		return
	}
	token := ""
	if headers != nil {
		token = strings.TrimSpace(headers.Get(codexTurnStateHeader))
	}
	if token == "" {
		c.Writer.Header().Del(codexTurnStateHeader)
		return
	}
	c.Header(codexTurnStateHeader, token)
	noteCodexTurnStateProvenance(affinityKey, account)
}

// commitResponsesStreamAttempt publishes the winning account's turn-state only
// when a retry heartbeat has not already committed the HTTP headers. The token
// must not be exposed before the attempt reaches a successful terminal because
// a later failover may use another account. If a heartbeat already committed
// the response, no documented Responses event is equivalent to this header, so
// the token is intentionally omitted instead of leaking stale account state.
func (h *Handler) commitResponsesStreamAttempt(c *gin.Context, attempt *continuousRetryStreamAttempt, affinityKey string, account *auth.Account, headers http.Header) error {
	if attempt == nil {
		return h.commitStreamAttempt(c, attempt)
	}

	token := ""
	stagedHeader := false
	if headers != nil {
		token = strings.TrimSpace(headers.Get(codexTurnStateHeader))
	}
	if c != nil && c.Writer != nil && !c.Writer.Written() {
		stagedHeader = true
		if token == "" {
			c.Writer.Header().Del(codexTurnStateHeader)
		} else {
			c.Header(codexTurnStateHeader, token)
		}
	}

	if err := h.commitStreamAttempt(c, attempt); err != nil {
		// Headers are staged before replay/filter commit; remove the token on
		// failure so local replay errors cannot expose turn state or provenance.
		// Header 会先于回放/过滤提交暂存；失败时移除 token，避免本地回放错误
		// 暴露账号绑定的续链状态或出处数据。
		if stagedHeader && c != nil && c.Writer != nil && !c.Writer.Written() {
			c.Writer.Header().Del(codexTurnStateHeader)
		}
		return err
	}
	if stagedHeader && token != "" {
		noteCodexTurnStateProvenance(affinityKey, account)
	}
	return nil
}

// guardCodexTurnStateEcho 出站守卫:客户端回带的 turn-state 若已知由其他账号
// 铸造则从下游头剥离(HTTP 直传与 WS 握手都从这份头取值),同账号或无溯源
// 记录时保持原样。按 attempt 调用:failover 换号后同一请求的下一次尝试必须
// 重新裁决。
func guardCodexTurnStateEcho(affinityKey string, account *auth.Account, headers http.Header) {
	if headers == nil || account == nil || strings.TrimSpace(affinityKey) == "" {
		return
	}
	if strings.TrimSpace(headers.Get(codexTurnStateHeader)) == "" {
		return
	}
	raw, ok := codexTurnStateOrigins.Load(affinityKey)
	if !ok {
		return
	}
	origin, ok := raw.(codexTurnStateOrigin)
	if !ok {
		codexTurnStateOrigins.Delete(affinityKey)
		return
	}
	if !origin.expiresAt.IsZero() && time.Now().After(origin.expiresAt) {
		codexTurnStateOrigins.Delete(affinityKey)
		return
	}
	if origin.accountID != account.ID() {
		headers.Del(codexTurnStateHeader)
	}
}

func noteCodexTurnStateProvenance(affinityKey string, account *auth.Account) {
	if strings.TrimSpace(affinityKey) == "" || account == nil || account.ID() <= 0 {
		return
	}
	codexTurnStateOrigins.Store(affinityKey, codexTurnStateOrigin{
		accountID: account.ID(),
		expiresAt: time.Now().Add(codexTurnStateProvenanceTTL),
	})
	sweepCodexTurnStateOrigins()
}

// sweepCodexTurnStateOrigins 机会式清扫:每 256 次写入全量遍历一轮,防止仅靠
// 读侧惰性删除导致的慢泄漏(会话键无上界)。
func sweepCodexTurnStateOrigins() {
	if codexTurnStateWrites.Add(1)%256 != 0 {
		return
	}
	now := time.Now()
	codexTurnStateOrigins.Range(func(key, value any) bool {
		origin, ok := value.(codexTurnStateOrigin)
		if !ok || (!origin.expiresAt.IsZero() && now.After(origin.expiresAt)) {
			codexTurnStateOrigins.Delete(key)
		}
		return true
	})
}
