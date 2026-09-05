package admin

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestRunRefreshAllModels_PartialFailureKeepsOtherChannels(t *testing.T) {
	h := &Handler{modelRefreshFuncs: map[string]channelModelRefreshFunc{
		database.UpstreamChannelGrok: func(ctx context.Context, _ modelRefreshEmitter) channelModelRefreshResult {
			return channelModelRefreshResult{Refreshed: 1, Added: []string{"grok-5"}}
		},
		database.UpstreamChannelCodex: func(ctx context.Context, _ modelRefreshEmitter) channelModelRefreshResult {
			return channelModelRefreshResult{Error: "官方模型页同步失败: boom", Failed: 1}
		},
		database.UpstreamChannelClaude: func(ctx context.Context, _ modelRefreshEmitter) channelModelRefreshResult {
			panic("claude exploded")
		},
	}}
	resp := h.runRefreshAllModels(context.Background(), nil)

	if len(resp.Channels) != 3 {
		t.Fatalf("channels = %d, want 3: %+v", len(resp.Channels), resp.Channels)
	}
	// 固定顺序：codex → claude → grok
	if resp.Channels[0].Channel != database.UpstreamChannelCodex || resp.Channels[1].Channel != database.UpstreamChannelClaude || resp.Channels[2].Channel != database.UpstreamChannelGrok {
		t.Fatalf("unexpected channel order: %+v", resp.Channels)
	}
	if resp.Channels[0].Error == "" || resp.Channels[0].Failed != 1 {
		t.Fatalf("codex failure should be reported: %+v", resp.Channels[0])
	}
	if resp.Channels[1].Error == "" || resp.Channels[1].Error[:5] != "panic" {
		t.Fatalf("claude panic should be captured as error: %+v", resp.Channels[1])
	}
	if resp.Channels[2].Refreshed != 1 || len(resp.Channels[2].Added) != 1 {
		t.Fatalf("grok result lost: %+v", resp.Channels[2])
	}
	if len(resp.Added) != 1 || resp.Added[0] != "grok-5" {
		t.Fatalf("aggregated added = %v", resp.Added)
	}
	for _, ch := range resp.Channels {
		if ch.Added == nil {
			t.Fatalf("added must serialize as [] not null: %+v", ch)
		}
	}
}

func TestRunRefreshAllModels_TimeoutIsReportedPerChannel(t *testing.T) {
	h := &Handler{modelRefreshFuncs: map[string]channelModelRefreshFunc{
		database.UpstreamChannelAntigravity: func(ctx context.Context, _ modelRefreshEmitter) channelModelRefreshResult {
			<-ctx.Done()
			return channelModelRefreshResult{}
		},
		database.UpstreamChannelCodex: func(ctx context.Context, _ modelRefreshEmitter) channelModelRefreshResult {
			return channelModelRefreshResult{Refreshed: 1}
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	resp := h.runRefreshAllModels(ctx, nil)
	if time.Since(started) > 2*time.Second {
		t.Fatalf("refresh did not honour context deadline")
	}
	if resp.Channels[1].Channel != database.UpstreamChannelAntigravity || !errors.Is(context.DeadlineExceeded, context.DeadlineExceeded) || resp.Channels[1].Error != context.DeadlineExceeded.Error() {
		t.Fatalf("antigravity timeout should be reported: %+v", resp.Channels)
	}
	if resp.Channels[0].Error != "" {
		t.Fatalf("codex should not inherit the timeout error: %+v", resp.Channels[0])
	}
}

func TestNewlyAddedModels(t *testing.T) {
	added := newlyAddedModels([]string{"gpt-5.5", "GPT-5.4"}, []string{"gpt-5.4", "gpt-6-astra", "gpt-5.5", "gpt-6-astra", " "})
	if len(added) != 1 || added[0] != "gpt-6-astra" {
		t.Fatalf("added = %v", added)
	}
}

func TestGroupAccountsByPlan_OneSamplePerPlan(t *testing.T) {
	mk := func(id int64, plan string, disabled bool) *auth.Account {
		acc := &auth.Account{PlanType: plan, DBID: id}
		if disabled {
			atomic.StoreInt32(&acc.Disabled, 1)
		}
		return acc
	}
	accounts := []*auth.Account{
		mk(1, "pro", false), mk(2, "pro", false), mk(3, "pro", false),
		mk(4, "pro-20x", false), mk(5, "api", true), mk(6, "", false), mk(7, "prolite", false),
	}
	groups := groupAccountsByPlan(accounts, nil, func(n int) int { return n - 1 })
	plans := make([]string, 0, len(groups))
	for _, g := range groups {
		plans = append(plans, g.Plan)
	}
	// api 组全部禁用 → 不出现；prolite 归一化为 pro；空套餐归入 unknown；按名字排序。
	if strings.Join(plans, ",") != "pro,pro-20x,unknown" {
		t.Fatalf("plans = %v", plans)
	}
	if groups[0].Sample.ID() != 7 || len(groups[0].Members) != 4 {
		t.Fatalf("pro group should sample the last member (7) of 4, got sample=%d members=%d", groups[0].Sample.ID(), len(groups[0].Members))
	}
	if groups[1].Sample.ID() != 4 || len(groups[1].Members) != 1 {
		t.Fatalf("pro-20x group = %+v", groups[1])
	}
}

func TestRunRefreshAllModels_StreamsProgressEvents(t *testing.T) {
	h := &Handler{modelRefreshFuncs: map[string]channelModelRefreshFunc{
		database.UpstreamChannelCodex: func(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult {
			emit(modelRefreshEvent{Type: "start", Channel: database.UpstreamChannelCodex, Groups: 1})
			emit(modelRefreshEvent{Type: "progress", Channel: database.UpstreamChannelCodex, Current: 1, Total: 1, Plan: "pro", Status: "ok", Added: []string{"gpt-6-astra"}})
			return channelModelRefreshResult{Refreshed: 1, Groups: 1, Added: []string{"gpt-6-astra"}}
		},
	}}
	var events []modelRefreshEvent
	resp := h.runRefreshAllModels(context.Background(), func(e modelRefreshEvent) { events = append(events, e) })
	if len(events) != 2 || events[0].Type != "start" || events[1].Type != "progress" || events[1].Plan != "pro" {
		t.Fatalf("events = %+v", events)
	}
	if resp.Type != "complete" || resp.Channels[0].Groups != 1 {
		t.Fatalf("summary = %+v", resp)
	}
}
