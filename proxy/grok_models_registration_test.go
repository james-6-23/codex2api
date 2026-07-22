package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 未声明 models 白名单的 Grok 账号应把默认 Grok 模型集(含 grok-4.5)注册进 /v1/models。
func TestSupportedModelIDsIncludesDefaultGrok(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 1, APIKey: "xai-1", UpstreamType: auth.UpstreamGrok})
	h := NewHandler(store, nil, nil, nil)

	ids := h.supportedModelIDs(context.Background())
	if !containsFold(ids, "grok-4.5") {
		t.Fatalf("grok-4.5 应出现在 /v1/models，实际: %v", ids)
	}
}

// 显式声明 models 的 Grok 账号以白名单为准，不再补默认集。
func TestSupportedModelIDsRespectsDeclaredGrokModels(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 1, APIKey: "xai-1", UpstreamType: auth.UpstreamGrok, Models: []string{"grok-4"}})
	h := NewHandler(store, nil, nil, nil)

	ids := h.supportedModelIDs(context.Background())
	if !containsFold(ids, "grok-4") {
		t.Fatalf("声明的 grok-4 应在列表，实际: %v", ids)
	}
	if containsFold(ids, "grok-4.5") {
		t.Fatalf("已声明白名单不应再补默认集(grok-4.5)，实际: %v", ids)
	}
}

func containsFold(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), target) {
			return true
		}
	}
	return false
}
