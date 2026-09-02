package admin

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// 导入后自动探测的账号数上限。每个账号 = 一次 Cloudflare 防护端点的请求，万级导入
// 全量探测会持续数小时打上游，并显著抬高被 managed challenge 的概率。超出的部分不
// 静默丢弃——弹窗会显示「已抽检 50/2000」，用户想要完整数据可以显式点继续。
const inviteGuideProbeCap = 50

// 单次「继续探测」允许追加的账号数，同样按闸门排队。
const inviteGuideProbeBatch = 50

// inviteGuidePlanState 是单个账号在方案里的状态。
const (
	inviteGuideStatePending    = "pending"    // 还没探测出结果
	inviteGuideStateEligible   = "eligible"   // 有资格且还有奖励次数
	inviteGuideStateExhausted  = "exhausted"  // 有资格但本月奖励次数已用尽
	inviteGuideStateIneligible = "ineligible" // 上游明确判定无资格
)

// inviteGuideAccountPlan 是单个账号的邀请收益评估。
type inviteGuideAccountPlan struct {
	ID       int64  `json:"id"`
	Email    string `json:"email,omitempty"`
	PlanType string `json:"plan_type,omitempty"`
	State    string `json:"state"`

	// remaining_* 用指针透传上游语义：nil = 上游没给这个字段（未知），
	// 0 = 上游明确说没了。两者的处置完全不同，不能都退化成 0。
	RemainingSendCapacity   *int `json:"remaining_send_capacity,omitempty"`
	RemainingRewardCapacity *int `json:"remaining_reward_capacity,omitempty"`

	// GrantAmount 是邀请人（referrer）单次邀请能拿到的额度，不含受邀人那份。
	GrantAmount      float64 `json:"grant_amount,omitempty"`
	PotentialCredits float64 `json:"potential_credits"`
	OfferID          string  `json:"offer_id,omitempty"`
	Title            string  `json:"title,omitempty"`
	IneligibleReason string  `json:"ineligible_reason,omitempty"`

	// 本月发送用量，来自 time_frame_rules 的 send 规则。与下面的 Invites* 不是
	// 同一个窗口：这是「月」，那是跟踪端点的 90 天，混用会给出错误的数字。
	MonthlySent      *int `json:"monthly_sent,omitempty"`
	MonthlySendTotal *int `json:"monthly_send_total,omitempty"`

	// 近 90 天的实际邀请记录，来自 tracking 快照。指针区分「没有跟踪数据」与
	// 「确实是 0」——导入时只探资格不探记录，多数账号这里就是没有数据。
	InvitesSent     *int `json:"invites_sent,omitempty"`
	InvitesAccepted *int `json:"invites_accepted,omitempty"`
	InvitesPending  *int `json:"invites_pending,omitempty"`

	// SuggestedInvites 是分配建议：这个号建议发几封。没有传 emails 预算时等于
	// 剩余奖励次数；传了则按「单次收益高的号优先」贪心分配。
	SuggestedInvites int        `json:"suggested_invites"`
	ObservedAt       *time.Time `json:"observed_at,omitempty"`
}

// inviteTrackingCounts 汇总一份跟踪快照里的发送/接受/在途数量。
// 实测状态取值：redeemed（已兑换）、expired（发出但受邀人未在有效期内使用）。
// expired 既不算接受也不算在途，但仍计入「已发」。
func inviteTrackingCounts(items []proxy.CodexInviteTrackingItem) (sent, accepted, pending int) {
	sent = len(items)
	for _, item := range items {
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "redeemed", "accepted":
			accepted++
		case "pending", "sent":
			pending++
		}
	}
	return sent, accepted, pending
}

// findSendCapacityRule 取 send 维度的月度配额规则（发送次数，与奖励次数不同）。
func findSendCapacityRule(rules []proxy.CodexInviteTimeFrameRule) *proxy.CodexInviteTimeFrameRule {
	for i := range rules {
		if strings.EqualFold(strings.TrimSpace(rules[i].CapacityType), "send") {
			return &rules[i]
		}
	}
	return nil
}

// inviteGuidePlanResponse 是引导弹窗的完整数据。
type inviteGuidePlanResponse struct {
	Enabled bool `json:"enabled"`

	Total     int `json:"total"`     // 请求评估的账号总数
	Probed    int `json:"probed"`    // 已拿到探测结果的
	Pending   int `json:"pending"`   // 仍在排队/探测中的
	Unprobed  int `json:"unprobed"`  // 因封顶而完全没排进队列的
	Eligible  int `json:"eligible"`  // 有资格且有奖励次数的
	ProbeCap  int `json:"probe_cap"` // 自动探测封顶值，前端用于解释「已抽检」
	RewardCap int `json:"total_reward_slots"`

	TotalPotentialCredits float64 `json:"total_potential_credits"`
	// EmailBudget 回显请求里的邀请人邮箱预算（0 = 未限制）。
	EmailBudget int                      `json:"email_budget"`
	Accounts    []inviteGuideAccountPlan `json:"accounts"`
}

// lookupInviteAccount 按 DBID 取账号。
//
// 刻意不用 h.findAccountByID：那个实现会 h.store.Accounts() 复制整个号池再线性
// 扫描，而这里一次请求要查上百个 ID（邀请页下拉一屏就是 100 个账号），万级号池下
// 等于上百次全量切片复制 + 上百万次比较。store.FindByID 走 DBID 索引，O(1)。
func (h *Handler) lookupInviteAccount(id int64) *auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	return h.store.FindByID(id)
}

// inviteGuideCandidate 判断账号能否作为邀请发起方。与前端 isCodexInviteCandidate
// 同口径：中转号与 AT-only 号没有可持续用于 referral 的 Codex OAuth 凭证。
// enabled/locked/status 不参与判断——那些只是调度开关，不影响 access token 可用性。
func inviteGuideCandidate(acc *auth.Account) bool {
	if acc == nil || acc.GetAccessToken() == "" {
		return false
	}
	if acc.IsOpenAIResponsesAPI() || acc.IsGrokAPI() || acc.IsAntigravityAPI() {
		return false
	}
	return acc.RefreshToken != ""
}

// referrerGrantAmount 取「邀请人能拿到的」那份奖励。上游对每次邀请下发两条 grant：
// recipient=referrer 是邀请人（我方收益），recipient=recipient 是受邀人。必须按
// recipient 过滤，否则把受邀人那份也算进自己的收益，预估会正好翻一倍。
func referrerGrantAmount(grants []proxy.CodexInviteGrant) float64 {
	for _, g := range grants {
		if strings.EqualFold(strings.TrimSpace(g.Recipient), "referrer") {
			return g.Amount
		}
	}
	// 上游没标 recipient 时：只有一条就按它算，多条无从分辨则不猜。
	if len(grants) == 1 {
		return grants[0].Amount
	}
	return 0
}

// probeInviteEligibilityForGuide 探测单个账号的邀请资格并写入快照。
// includeTracking 为真时顺带抓已发邀请记录（已发/已接受计数的唯一来源）。
//
// 导入路径刻意传 false：跟踪记录对「先用哪个号发」的排序没有影响，为它把导入的
// 上游请求数翻倍不划算。用户在下拉里显式点「探测积分」时才传 true。
// 失败只记日志：引导是锦上添花，探不到就显示为未知，不影响导入本身。
func (h *Handler) probeInviteEligibilityForGuide(ctx context.Context, accountID int64, includeTracking bool) {
	account := h.lookupInviteAccount(accountID)
	if !inviteGuideCandidate(account) {
		return
	}

	programID, entrypoint := proxy.NormalizeInviteProgram("", "")
	scope := inviteEligibilityScope(programID, entrypoint)
	generation := account.GetCredentialGeneration()

	// 已有未过期快照就不重复打上游：同一账号可能被多次导入触发，也可能刚在
	// 邀请页被查过。这一步让整个引导功能在缓存热的时候零上游开销。
	var cached proxy.CodexInviteEligibility
	if meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
		database.CodexInviteSnapshotEligibility, accountID, generation, scope, &cached); meta != nil {
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := proxy.QueryCodexInviteEligibility(probeCtx, account,
		h.store.ResolveProxyForAccount(account), programID, entrypoint)
	if err != nil {
		log.Printf("邀请资格探测失败: account=%d err=%v", accountID, err)
		return
	}
	// 与邀请页同一条规则：挑战页借用 403，缓存它等于把一次拦截固化成「无资格」。
	if result.OK && !result.Challenged {
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			accountID, generation, scope, result.StatusCode, inviteEligibilitySnapshotTTL, result)
	}
	if !includeTracking {
		return
	}

	// 跟踪记录串在资格之后而不是并发：上游在 Cloudflare bot 管理后面，第一个请求
	// 拿到的 __cf_bm 能让第二个少被挑战——与邀请页的取数顺序一致。
	period, limit := proxy.NormalizeInviteTracking("", 0)
	trackingScope := inviteTrackingScope(programID, period, limit)
	var cachedTracking proxy.CodexInviteTracking
	if h.readInviteCache(ctx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
		accountID, generation, trackingScope, &cachedTracking) != nil {
		h.rememberInviteTrackingRecipients(ctx, accountID, programID, entrypoint, &cachedTracking)
		return
	}
	trackingCtx, trackingCancel := context.WithTimeout(ctx, 30*time.Second)
	defer trackingCancel()
	tracking, err := proxy.QueryCodexInviteTracking(trackingCtx, account,
		h.store.ResolveProxyForAccount(account), programID, period, limit)
	if err != nil {
		log.Printf("邀请记录探测失败: account=%d err=%v", accountID, err)
		return
	}
	if tracking.OK && !tracking.Challenged {
		h.rememberInviteTrackingRecipients(ctx, accountID, programID, entrypoint, tracking)
		h.writeInviteCache(ctx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
			accountID, generation, trackingScope, tracking.StatusCode, inviteTrackingSnapshotTTL, tracking)
	}
}

// enqueueInviteGuideProbes 把账号排进导入探测闸门。返回实际排队数与被封顶挡下的数量。
// 复用 runImportProbeTask 是关键：它带自适应并发许可，不会在大批量导入时瞬时打爆上游。
func (h *Handler) enqueueInviteGuideProbes(ids []int64, limit int, includeTracking bool) (queued int, skipped int) {
	if h == nil || limit <= 0 {
		return 0, len(ids)
	}
	for _, id := range ids {
		if queued >= limit {
			return queued, len(ids) - queued
		}
		accountID := id
		h.runImportProbeTask(func(ctx context.Context) {
			h.probeInviteEligibilityForGuide(ctx, accountID, includeTracking)
		})
		queued++
	}
	return queued, 0
}

// scheduleInviteGuideProbes 是导入路径的入口：筛出可邀请的 Codex 账号并排队探测。
// 开关关闭时直接返回，一个上游请求都不发。
func (h *Handler) scheduleInviteGuideProbes(ctx context.Context, ids []int64) {
	if h == nil || len(ids) == 0 || !h.inviteGuideEnabled(ctx) {
		return
	}
	candidates := make([]int64, 0, len(ids))
	for _, id := range ids {
		if inviteGuideCandidate(h.lookupInviteAccount(id)) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return
	}
	queued, skipped := h.enqueueInviteGuideProbes(candidates, inviteGuideProbeCap, false)
	log.Printf("邀请引导: 导入 %d 个账号，排队探测 %d 个，封顶跳过 %d 个", len(ids), queued, skipped)
}

// inviteGuideEnabled 读取开关。读不到时按默认开启——引导默认可见是产品预期，
// 一次数据库抖动不该把功能静默关掉。
func (h *Handler) inviteGuideEnabled(ctx context.Context) bool {
	if h == nil || h.db == nil {
		return database.InviteGuideDefaultEnabled
	}
	cfg, err := h.db.LoadInviteGuideConfig(ctx)
	if err != nil {
		log.Printf("读取邀请引导设置失败，按默认开启处理: %v", err)
		return database.InviteGuideDefaultEnabled
	}
	return cfg.IsEnabled()
}

// buildInviteGuidePlan 按账号 ID 汇总评估结果并排出建议顺序。
func (h *Handler) buildInviteGuidePlan(ctx context.Context, ids []int64, emailBudget int) inviteGuidePlanResponse {
	programID, entrypoint := proxy.NormalizeInviteProgram("", "")
	scope := inviteEligibilityScope(programID, entrypoint)
	trackingPeriod, trackingLimit := proxy.NormalizeInviteTracking("", 0)
	trackingScope := inviteTrackingScope(programID, trackingPeriod, trackingLimit)

	resp := inviteGuidePlanResponse{
		Enabled:     h.inviteGuideEnabled(ctx),
		ProbeCap:    inviteGuideProbeCap,
		EmailBudget: emailBudget,
		Accounts:    make([]inviteGuideAccountPlan, 0, len(ids)),
	}

	for _, id := range ids {
		account := h.lookupInviteAccount(id)
		if !inviteGuideCandidate(account) {
			continue
		}
		resp.Total++

		item := inviteGuideAccountPlan{
			ID:       id,
			Email:    account.Email,
			PlanType: account.PlanType,
			State:    inviteGuideStatePending,
		}

		var elig proxy.CodexInviteEligibility
		meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, id, account.GetCredentialGeneration(), scope, &elig)
		if meta == nil {
			resp.Pending++
			resp.Accounts = append(resp.Accounts, item)
			continue
		}

		resp.Probed++
		if meta.ObservedAt != nil {
			item.ObservedAt = meta.ObservedAt
		}
		item.RemainingSendCapacity = elig.RemainingSendCapacity
		item.RemainingRewardCapacity = elig.RemainingRewardCapacity
		item.OfferID = elig.OfferID
		item.Title = elig.Title
		item.GrantAmount = referrerGrantAmount(elig.Grants)
		if rule := findSendCapacityRule(elig.TimeFrameRules); rule != nil {
			sent, total := rule.InvitesSent, rule.InvitesTotal
			item.MonthlySent, item.MonthlySendTotal = &sent, &total
		}

		// 已发/已接受来自跟踪快照。只读缓存，没有就不填——导入探测只写资格，
		// 多数账号这里本来就没有数据，编一个 0 会被读成「一封都没发过」。
		var tracking proxy.CodexInviteTracking
		if h.readInviteCache(ctx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
			id, account.GetCredentialGeneration(), trackingScope, &tracking) != nil {
			h.rememberInviteTrackingRecipients(ctx, id, programID, entrypoint, &tracking)
			sent, accepted, pending := inviteTrackingCounts(tracking.Items)
			item.InvitesSent, item.InvitesAccepted, item.InvitesPending = &sent, &accepted, &pending
		}

		switch {
		case !elig.ShouldShow:
			item.State = inviteGuideStateIneligible
			item.IneligibleReason = elig.IneligibleReason
			if item.IneligibleReason == "" {
				item.IneligibleReason = elig.IneligibleReasonCode
			}
		case elig.RemainingRewardCapacity != nil && *elig.RemainingRewardCapacity == 0:
			// 还能发，但发了也拿不到奖励——对「攒积分」这个目标等于没有价值。
			item.State = inviteGuideStateExhausted
		default:
			item.State = inviteGuideStateEligible
			resp.Eligible++
			if elig.RemainingRewardCapacity != nil {
				item.SuggestedInvites = *elig.RemainingRewardCapacity
				resp.RewardCap += item.SuggestedInvites
			}
			item.PotentialCredits = item.GrantAmount * float64(item.SuggestedInvites)
		}

		resp.Accounts = append(resp.Accounts, item)
	}

	sortInviteGuidePlan(resp.Accounts)
	applyInviteGuideEmailBudget(resp.Accounts, emailBudget)

	resp.TotalPotentialCredits = 0
	for i := range resp.Accounts {
		resp.TotalPotentialCredits += resp.Accounts[i].PotentialCredits
	}
	return resp
}

// sortInviteGuidePlan 排出「先用哪个号发」的顺序：可发的排前面，其次按单次收益、
// 剩余奖励次数、预估总收益降序。单次收益优先于剩余次数——邮箱是稀缺资源时，
// 把一封邮件用在单次给得更多的号上才是最优。
func sortInviteGuidePlan(items []inviteGuideAccountPlan) {
	rank := map[string]int{
		inviteGuideStateEligible:   0,
		inviteGuideStatePending:    1,
		inviteGuideStateExhausted:  2,
		inviteGuideStateIneligible: 3,
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if rank[a.State] != rank[b.State] {
			return rank[a.State] < rank[b.State]
		}
		if a.GrantAmount != b.GrantAmount {
			return a.GrantAmount > b.GrantAmount
		}
		if a.SuggestedInvites != b.SuggestedInvites {
			return a.SuggestedInvites > b.SuggestedInvites
		}
		return a.ID < b.ID
	})
}

// applyInviteGuideEmailBudget 在受邀邮箱有限时做贪心分配。列表已按单次收益降序，
// 依次把预算分给靠前的号即为最优——总收益 = Σ(单次收益)，取值最高的前 N 个名额。
func applyInviteGuideEmailBudget(items []inviteGuideAccountPlan, budget int) {
	if budget <= 0 {
		return
	}
	remaining := budget
	for i := range items {
		if items[i].State != inviteGuideStateEligible {
			continue
		}
		if remaining <= 0 {
			items[i].SuggestedInvites = 0
			items[i].PotentialCredits = 0
			continue
		}
		if items[i].SuggestedInvites > remaining {
			items[i].SuggestedInvites = remaining
		}
		remaining -= items[i].SuggestedInvites
		items[i].PotentialCredits = items[i].GrantAmount * float64(items[i].SuggestedInvites)
	}
}

// parseInviteGuideIDs 解析逗号分隔的账号 ID，去重并保序。
func parseInviteGuideIDs(raw string) []int64 {
	seen := make(map[int64]struct{})
	ids := make([]int64, 0, 8)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// GetInviteGuidePlan 汇总一批账号的邀请收益评估。
// GET /api/admin/accounts/invite/plan?ids=1,2,3&emails=5
func (h *Handler) GetInviteGuidePlan(c *gin.Context) {
	ids := parseInviteGuideIDs(c.Query("ids"))
	if len(ids) == 0 {
		writeError(c, http.StatusBadRequest, "缺少账号 ID")
		return
	}
	budget, _ := strconv.Atoi(strings.TrimSpace(c.Query("emails")))
	c.JSON(http.StatusOK, h.buildInviteGuidePlan(c.Request.Context(), ids, budget))
}

// ProbeInviteGuidePlan 为指定账号补探测（弹窗里的「继续探测剩余」）。
// POST /api/admin/accounts/invite/plan/probe  {"ids":[1,2,3]}
func (h *Handler) ProbeInviteGuidePlan(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	candidates := make([]int64, 0, len(req.IDs))
	for _, id := range req.IDs {
		if inviteGuideCandidate(h.lookupInviteAccount(id)) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		writeError(c, http.StatusBadRequest, "没有可探测的 Codex 账号")
		return
	}
	queued, skipped := h.enqueueInviteGuideProbes(candidates, inviteGuideProbeBatch, true)
	c.JSON(http.StatusOK, gin.H{"queued": queued, "skipped": skipped})
}

// GetInviteGuideSettings 返回引导弹窗开关。
// GET /api/admin/settings/invite-guide
func (h *Handler) GetInviteGuideSettings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"enabled": h.inviteGuideEnabled(c.Request.Context())})
}

// UpdateInviteGuideSettings 更新引导弹窗开关。弹窗里的「今后不弹出」打到这里。
// PUT /api/admin/settings/invite-guide  {"enabled":false}
func (h *Handler) UpdateInviteGuideSettings(c *gin.Context) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.Enabled == nil {
		writeError(c, http.StatusBadRequest, "缺少 enabled 字段")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	if err := h.db.SaveInviteGuideConfig(c.Request.Context(),
		database.InviteGuideConfig{Enabled: req.Enabled}); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": *req.Enabled})
}
