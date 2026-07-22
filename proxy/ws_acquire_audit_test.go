package proxy

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestWsAcquireAuditAccumulatesResetsAndTolerates(t *testing.T) {
	ctx := withWsAcquireAudit(context.Background())
	AddWsAcquireDuration(ctx, 150*time.Millisecond)
	AddWsAcquireDuration(ctx, 350*time.Millisecond)
	if got := wsAcquireAuditMs(ctx); got != 500 {
		t.Fatalf("wsAcquireAuditMs = %d, want 500 (accumulated)", got)
	}

	// 每 attempt 清零：换号重试后旧值不得泄漏进新 attempt 的日志行
	resetWsAcquireAudit(ctx)
	if got := wsAcquireAuditMs(ctx); got != 0 {
		t.Fatalf("wsAcquireAuditMs after reset = %d, want 0", got)
	}

	// 非正时长与未挂载 audit 的 ctx 都必须安全无操作
	AddWsAcquireDuration(ctx, 0)
	AddWsAcquireDuration(context.Background(), time.Second)
	if got := wsAcquireAuditMs(context.Background()); got != 0 {
		t.Fatalf("wsAcquireAuditMs on bare ctx = %d, want 0", got)
	}
}

func TestPopulateWsAcquireFirstTokenExclusion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newCtx := func(acquire time.Duration) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		attachWsAcquireAudit(c)
		AddWsAcquireDuration(c.Request.Context(), acquire)
		return c
	}
	setExclude := func(t *testing.T, enabled bool) {
		prev := CurrentRuntimeSettings()
		t.Cleanup(func() { ApplyRuntimeSettings(prev) })
		next := prev
		next.FirstTokenExcludesWsAcquire = enabled
		ApplyRuntimeSettings(next)
	}

	// 默认关闭：保持含取连的历史口径
	setExclude(t, false)
	input := &database.UsageLogInput{FirstTokenMs: 1500}
	populateWsAcquireFromRequest(newCtx(900*time.Millisecond), input)
	if input.FirstTokenMs != 1500 || input.WsAcquireMs != 900 {
		t.Fatalf("disabled: first_token=%d acquire=%d, want 1500/900", input.FirstTokenMs, input.WsAcquireMs)
	}

	// 开启：扣除取连，两列相加可还原
	setExclude(t, true)
	input = &database.UsageLogInput{FirstTokenMs: 1500}
	populateWsAcquireFromRequest(newCtx(900*time.Millisecond), input)
	if input.FirstTokenMs != 600 || input.WsAcquireMs != 900 {
		t.Fatalf("enabled: first_token=%d acquire=%d, want 600/900", input.FirstTokenMs, input.WsAcquireMs)
	}

	// 扣除后不归零：已打点的行钳到 1ms，避免前端把 0 当作未记录
	input = &database.UsageLogInput{FirstTokenMs: 800}
	populateWsAcquireFromRequest(newCtx(900*time.Millisecond), input)
	if input.FirstTokenMs != 1 {
		t.Fatalf("clamped: first_token=%d, want 1", input.FirstTokenMs)
	}

	// 未打点（0）的行不动：0 表示无首字，不能被扣成负/假值
	input = &database.UsageLogInput{FirstTokenMs: 0}
	populateWsAcquireFromRequest(newCtx(900*time.Millisecond), input)
	if input.FirstTokenMs != 0 {
		t.Fatalf("unrecorded: first_token=%d, want 0", input.FirstTokenMs)
	}
}
