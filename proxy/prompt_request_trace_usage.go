package proxy

import (
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type promptRequestTraceUsage struct {
	mu      sync.Mutex
	metrics database.PromptRequestTraceMetrics
}

func (usage *promptRequestTraceUsage) snapshot() database.PromptRequestTraceMetrics {
	usage.mu.Lock()
	defer usage.mu.Unlock()
	return database.PromptRequestTraceMetrics{
		Attempts:      append([]database.PromptRequestTraceAttempt(nil), usage.metrics.Attempts...),
		TotalAttempts: usage.metrics.TotalAttempts,
	}
}

func appendPromptRequestTraceUsage(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || input.AccountID <= 0 {
		return
	}
	capture := promptResponseFromContext(c)
	if capture == nil || capture.traceUsage == nil {
		return
	}
	// Account selection alone does not imply an upstream call. Capture usage
	// logs with a measured duration, real tokens, or an observed upstream ID.
	if input.DurationMs <= 0 && input.TotalTokens <= 0 && input.UpstreamRequestID == "" {
		return
	}
	usage := capture.traceUsage
	usage.mu.Lock()
	defer usage.mu.Unlock()
	usage.metrics.TotalAttempts++
	if len(usage.metrics.Attempts) >= database.MaxPromptRequestTraceAttempts {
		return
	}
	model := input.UpstreamResponseModel
	if model == "" {
		model = input.EffectiveModel
	}
	if model == "" {
		model = input.Model
	}
	promptTokens := max(input.InputTokens, input.PromptTokens)
	outputTokens := max(input.OutputTokens, input.CompletionTokens)
	totalTokens := input.TotalTokens
	if input.Stream && input.StatusCode >= 400 && promptTokens == 0 && input.CachedTokens == 0 {
		// A broken stream with no usage frame can have an output-only
		// estimate in the billing log. Keep its status and timing here.
		outputTokens, totalTokens = 0, 0
	}
	usage.metrics.Attempts = append(usage.metrics.Attempts, database.PromptRequestTraceAttempt{
		FinishedAt:      time.Now().UTC(),
		Model:           database.SafePromptTraceModel(model),
		StatusCode:      input.StatusCode,
		DurationMs:      input.DurationMs,
		FirstTokenMs:    input.FirstTokenMs,
		InputTokens:     promptTokens,
		OutputTokens:    outputTokens,
		TotalTokens:     totalTokens,
		CachedTokens:    input.CachedTokens,
		ReasoningTokens: input.ReasoningTokens,
		CacheWrite5m:    input.CacheWrite5mTokens,
		CacheWrite1h:    input.CacheWrite1hTokens,
		Stream:          input.Stream,
		Retry:           input.IsRetryAttempt,
	})
}
