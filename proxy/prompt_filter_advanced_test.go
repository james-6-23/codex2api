package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/cache"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestPromptRiskBlocksRepeatedRequests(t *testing.T) {
	h := &Handler{cache: cache.NewMemory(1)}
	cfg := promptfilter.DefaultConfig()
	cfg.Enabled = true
	cfg.Advanced.Risk = promptfilter.RiskConfig{Enabled: true, WindowSeconds: 600, BlockThreshold: 100, UserWeightPercent: 100}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contextAPIKeyID, int64(42))
	v := promptfilter.Verdict{Enabled: true, Action: promptfilter.ActionAllow, Score: 60, RawScore: 60}
	if first := h.applyPromptRisk(c, v, cfg); first.Action == promptfilter.ActionBlock {
		t.Fatalf("first request blocked: %+v", first)
	}
	if second := h.applyPromptRisk(c, v, cfg); second.Action != promptfilter.ActionBlock {
		t.Fatalf("repeated risk was not blocked: %+v", second)
	}
}

func TestSidecarCanEscalateButCannotDowngrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"allow","score":5,"reason":"classifier allow"}`))
	}))
	defer server.Close()
	cfg := promptfilter.DefaultConfig()
	cfg.Advanced.Sidecar = promptfilter.SidecarConfig{Enabled: true, BaseURL: server.URL, TimeoutSeconds: 2}
	blocked := promptfilter.Verdict{Action: promptfilter.ActionBlock, Score: 80}
	got := applyPromptSidecar(t.Context(), "test", blocked, cfg)
	if got.Action != promptfilter.ActionBlock {
		t.Fatalf("sidecar downgraded block: %+v", got)
	}
}
