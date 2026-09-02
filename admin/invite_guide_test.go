package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func intPtr(v int) *int { return &v }

// 上游对每次邀请下发两条 grant，邀请人和受邀人各一条，金额相同。取错会让预估翻倍。
func TestReferrerGrantAmount(t *testing.T) {
	t.Run("picks referrer not recipient", func(t *testing.T) {
		// 形状取自实测响应（offer credits_1000）。
		grants := []proxy.CodexInviteGrant{
			{Recipient: "referrer", Amount: 1000, GrantType: "personal_credits"},
			{Recipient: "recipient", Amount: 1000, GrantType: "personal_credits"},
		}
		if got := referrerGrantAmount(grants); got != 1000 {
			t.Fatalf("got %v, want 1000", got)
		}
	})

	t.Run("asymmetric amounts must not take recipient", func(t *testing.T) {
		grants := []proxy.CodexInviteGrant{
			{Recipient: "recipient", Amount: 9999},
			{Recipient: "referrer", Amount: 500},
		}
		if got := referrerGrantAmount(grants); got != 500 {
			t.Fatalf("got %v, want 500 (referrer's share)", got)
		}
	})

	t.Run("single unlabeled grant is used as-is", func(t *testing.T) {
		if got := referrerGrantAmount([]proxy.CodexInviteGrant{{Amount: 250}}); got != 250 {
			t.Fatalf("got %v, want 250", got)
		}
	})

	t.Run("ambiguous multiple unlabeled grants yield zero", func(t *testing.T) {
		// 分不清归属时不猜——宁可显示 0 也不给用户一个编造的收益数字。
		grants := []proxy.CodexInviteGrant{{Amount: 100}, {Amount: 200}}
		if got := referrerGrantAmount(grants); got != 0 {
			t.Fatalf("got %v, want 0", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := referrerGrantAmount(nil); got != 0 {
			t.Fatalf("got %v, want 0", got)
		}
	})
}

func TestSortInviteGuidePlan(t *testing.T) {
	items := []inviteGuideAccountPlan{
		{ID: 1, State: inviteGuideStateIneligible},
		{ID: 2, State: inviteGuideStateEligible, GrantAmount: 500, SuggestedInvites: 3},
		{ID: 3, State: inviteGuideStatePending},
		{ID: 4, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 1},
		{ID: 5, State: inviteGuideStateExhausted},
		{ID: 6, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 2},
	}
	sortInviteGuidePlan(items)

	got := make([]int64, len(items))
	for i := range items {
		got[i] = items[i].ID
	}
	// 可发的在前；同为可发时先比单次收益（1000 > 500），再比剩余次数（2 > 1）。
	want := []int64{6, 4, 2, 3, 5, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// 邮箱有限时把名额分给单次收益最高的号，才是总收益最大的分法。
func TestApplyInviteGuideEmailBudget(t *testing.T) {
	t.Run("greedy allocation by per-invite value", func(t *testing.T) {
		items := []inviteGuideAccountPlan{
			{ID: 1, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 2},
			{ID: 2, State: inviteGuideStateEligible, GrantAmount: 500, SuggestedInvites: 3},
		}
		applyInviteGuideEmailBudget(items, 3) // 只有 3 个受邀邮箱

		if items[0].SuggestedInvites != 2 || items[0].PotentialCredits != 2000 {
			t.Fatalf("top account: invites=%d credits=%v, want 2/2000", items[0].SuggestedInvites, items[0].PotentialCredits)
		}
		// 预算只剩 1 个名额给第二个号。
		if items[1].SuggestedInvites != 1 || items[1].PotentialCredits != 500 {
			t.Fatalf("second account: invites=%d credits=%v, want 1/500", items[1].SuggestedInvites, items[1].PotentialCredits)
		}
	})

	t.Run("budget exhausted zeroes out the tail", func(t *testing.T) {
		items := []inviteGuideAccountPlan{
			{ID: 1, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 2, PotentialCredits: 2000},
			{ID: 2, State: inviteGuideStateEligible, GrantAmount: 900, SuggestedInvites: 5, PotentialCredits: 4500},
		}
		applyInviteGuideEmailBudget(items, 2)
		if items[1].SuggestedInvites != 0 || items[1].PotentialCredits != 0 {
			t.Fatalf("tail should be zeroed, got invites=%d credits=%v", items[1].SuggestedInvites, items[1].PotentialCredits)
		}
	})

	t.Run("zero budget means unlimited and changes nothing", func(t *testing.T) {
		items := []inviteGuideAccountPlan{
			{ID: 1, State: inviteGuideStateEligible, GrantAmount: 1000, SuggestedInvites: 4, PotentialCredits: 4000},
		}
		applyInviteGuideEmailBudget(items, 0)
		if items[0].SuggestedInvites != 4 || items[0].PotentialCredits != 4000 {
			t.Fatalf("unlimited budget must not modify plan: %+v", items[0])
		}
	})

	t.Run("ineligible accounts never consume budget", func(t *testing.T) {
		items := []inviteGuideAccountPlan{
			{ID: 1, State: inviteGuideStateIneligible, SuggestedInvites: 0},
			{ID: 2, State: inviteGuideStateEligible, GrantAmount: 100, SuggestedInvites: 3},
		}
		applyInviteGuideEmailBudget(items, 3)
		if items[1].SuggestedInvites != 3 {
			t.Fatalf("eligible account should get the whole budget, got %d", items[1].SuggestedInvites)
		}
	})
}

func TestParseInviteGuideIDs(t *testing.T) {
	got := parseInviteGuideIDs(" 3, 1 ,3,,abc,-2,0,7 ")
	want := []int64{3, 1, 7}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (dedup + order preserved)", got, want)
		}
	}
}

// 「没配过」必须读成开启，否则功能默认关闭，与产品预期相反。
func TestInviteGuideConfigDefault(t *testing.T) {
	var unset database.InviteGuideConfig
	if !unset.IsEnabled() {
		t.Fatal("unset config must default to enabled")
	}

	off := false
	if (database.InviteGuideConfig{Enabled: &off}).IsEnabled() {
		t.Fatal("explicit false must be honored")
	}

	on := true
	if !(database.InviteGuideConfig{Enabled: &on}).IsEnabled() {
		t.Fatal("explicit true must be honored")
	}
}

// remaining_reward_capacity=0 时账号还能发，但发了拿不到积分——对「攒积分」这个
// 目标没有价值，必须与「有资格」区分开，否则会被排到前面误导用户。
func TestInviteGuideExhaustedIsNotEligible(t *testing.T) {
	items := []inviteGuideAccountPlan{
		{ID: 1, State: inviteGuideStateExhausted, RemainingSendCapacity: intPtr(5), RemainingRewardCapacity: intPtr(0)},
		{ID: 2, State: inviteGuideStateEligible, GrantAmount: 10, SuggestedInvites: 1},
	}
	sortInviteGuidePlan(items)
	if items[0].ID != 2 {
		t.Fatalf("eligible account must outrank exhausted one, got order %d,%d", items[0].ID, items[1].ID)
	}
}

// 封顶是这个功能最关键的安全阀:万级导入若不设上限，会持续数小时打 Cloudflare
// 防护的上游端点。被挡下的部分必须如实回报，让弹窗能显示「已抽检 N/M」。
func TestEnqueueInviteGuideProbesCap(t *testing.T) {
	h := &Handler{}
	ids := make([]int64, 120)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	queued, skipped := h.enqueueInviteGuideProbes(ids, inviteGuideProbeCap, false)
	if queued != inviteGuideProbeCap {
		t.Fatalf("queued = %d, want %d", queued, inviteGuideProbeCap)
	}
	if skipped != len(ids)-inviteGuideProbeCap {
		t.Fatalf("skipped = %d, want %d", skipped, len(ids)-inviteGuideProbeCap)
	}

	t.Run("under cap queues everything", func(t *testing.T) {
		q, s := h.enqueueInviteGuideProbes(ids[:10], inviteGuideProbeCap, false)
		if q != 10 || s != 0 {
			t.Fatalf("queued=%d skipped=%d, want 10/0", q, s)
		}
	})

	t.Run("non-positive limit queues nothing", func(t *testing.T) {
		q, s := h.enqueueInviteGuideProbes(ids[:5], 0, false)
		if q != 0 || s != 5 {
			t.Fatalf("queued=%d skipped=%d, want 0/5", q, s)
		}
	})
}

// created_ids 是前端拉取方案的唯一入口；漏发等于整个引导不触发。空值必须省略，
// 否则老前端会收到一个多余字段。
func TestImportEventCreatedIDsSerialization(t *testing.T) {
	withIDs, err := json.Marshal(importEvent{Type: "complete", CreatedIDs: []int64{7, 9}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(withIDs), `"created_ids":[7,9]`) {
		t.Fatalf("created_ids missing from complete event: %s", withIDs)
	}

	empty, err := json.Marshal(importEvent{Type: "progress"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(empty), "created_ids") {
		t.Fatalf("empty created_ids must be omitted: %s", empty)
	}
}

// 状态取值取自实测跟踪响应：redeemed / expired。expired 是「发出了但受邀人没在
// 有效期内使用」——仍算已发，但既不算接受也不算在途，混进任何一边都会给出错数。
func TestInviteTrackingCounts(t *testing.T) {
	items := []proxy.CodexInviteTrackingItem{
		{Status: "redeemed"},
		{Status: "expired"},
		{Status: "expired"},
		{Status: "pending"},
		{Status: "ACCEPTED"}, // 大小写不敏感
		{Status: ""},         // 上游没给状态：只计入已发
	}
	sent, accepted, pending := inviteTrackingCounts(items)
	if sent != 6 {
		t.Fatalf("sent = %d, want 6 (每条记录都算已发)", sent)
	}
	if accepted != 2 {
		t.Fatalf("accepted = %d, want 2 (redeemed + ACCEPTED)", accepted)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1", pending)
	}

	t.Run("empty", func(t *testing.T) {
		s, a, p := inviteTrackingCounts(nil)
		if s != 0 || a != 0 || p != 0 {
			t.Fatalf("got %d/%d/%d, want 0/0/0", s, a, p)
		}
	})
}

// send 与 reward 是两条独立的配额规则，取错会把「本月已发 7/10」显示成
// 「1/3」（那是奖励次数）。
func TestFindSendCapacityRule(t *testing.T) {
	rules := []proxy.CodexInviteTimeFrameRule{
		{CapacityType: "reward", InvitesSent: 1, InvitesTotal: 3},
		{CapacityType: "send", InvitesSent: 7, InvitesTotal: 10},
	}
	got := findSendCapacityRule(rules)
	if got == nil || got.InvitesSent != 7 || got.InvitesTotal != 10 {
		t.Fatalf("got %+v, want send rule 7/10", got)
	}
	if findSendCapacityRule(nil) != nil {
		t.Fatal("missing rules must yield nil, not a zero-valued rule")
	}
}
