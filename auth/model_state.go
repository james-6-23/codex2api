package auth

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ModelStatus 模型级别状态
type ModelStatus string

const (
	ModelStatusReady       ModelStatus = "ready"        // 模型可用
	ModelStatusCooldown    ModelStatus = "cooldown"     // 模型级冷却中
	ModelStatusUnavailable ModelStatus = "unavailable"  // 模型不可用
)

// ModelState per-model 状态隔离核心结构
// 每个账号对每个模型独立维护状态，429/model_capacity 只影响该模型
type ModelState struct {
	mu             sync.RWMutex
	Status         ModelStatus `json:"status"`
	Unavailable    bool        `json:"unavailable"`
	NextRetryAfter time.Time   `json:"next_retry_after,omitempty"`
	LastError      string      `json:"last_error,omitempty"`
	StrikeCount    int         `json:"strike_count"`      // 连续 429/capacity 次数
	BackoffLevel   int         `json:"backoff_level"`     // 指数退避级别 (0-10)
	UpdatedAt      time.Time   `json:"updated_at"`
}

// cooldownDurations 11级指数退避序列：1s → 2s → 4s → ... → 30min
var cooldownDurations = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	32 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	4 * time.Minute,
	8 * time.Minute,
	30 * time.Minute, // 上限
}

// NewModelState 创建初始模型状态
func NewModelState() *ModelState {
	return &ModelState{
		Status:      ModelStatusReady,
		UpdatedAt:   time.Now(),
	}
}

// ApplyCooldown 应用模型级冷却（指数退避）
// reason: "rate_limited" 或 "model_capacity"
func (ms *ModelState) ApplyCooldown(reason string, customDuration ...time.Duration) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.Status = ModelStatusCooldown
	ms.LastError = reason
	ms.StrikeCount++
	ms.UpdatedAt = time.Now()

	// 计算冷却时长
	var duration time.Duration
	if len(customDuration) > 0 && customDuration[0] > 0 {
		// 优先使用上游提供的精确时长
		duration = customDuration[0]
	} else {
		// 否则使用指数退避
		if ms.BackoffLevel < len(cooldownDurations) {
			duration = cooldownDurations[ms.BackoffLevel]
			ms.BackoffLevel++ // 下次退避级别提升
		} else {
			duration = cooldownDurations[len(cooldownDurations)-1] // 上限 30min
		}
	}

	ms.NextRetryAfter = time.Now().Add(duration)
	ms.Unavailable = true
}

// ClearCooldown 清除模型级冷却（请求成功后调用）
func (ms *ModelState) ClearCooldown() {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.Status = ModelStatusReady
	ms.Unavailable = false
	ms.NextRetryAfter = time.Time{}
	ms.LastError = ""
	ms.StrikeCount = 0
	ms.BackoffLevel = 0 // 重置退避级别
	ms.UpdatedAt = time.Now()
}

// IsAvailable 检查模型是否可用（考虑冷却时间）
func (ms *ModelState) IsAvailable(now time.Time) bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	if ms.Unavailable && now.Before(ms.NextRetryAfter) {
		return false
	}

	// 冷却时间已过，自动恢复
	if ms.Status == ModelStatusCooldown && now.After(ms.NextRetryAfter) {
		return true
	}

	return ms.Status == ModelStatusReady || !ms.Unavailable
}

// GetNextRetryAfter 获取下次可重试时间
func (ms *ModelState) GetNextRetryAfter() time.Time {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.NextRetryAfter
}

// GetStrikeCount 获取连续失败次数
func (ms *ModelState) GetStrikeCount() int {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.StrikeCount
}

// canonicalModelKey 规范化模型名称，避免别名分裂
// 例如: "gpt-4", "gpt-4-turbo", "gpt-4-turbo-preview" 等统一处理
func canonicalModelKey(model string) string {
	if model == "" {
		return ""
	}

	// 移除常见后缀
	model = strings.TrimSpace(model)
	model = strings.ToLower(model)

	// 处理别名映射（可根据实际情况扩展）
	// 例如: gpt-4-0314, gpt-4-0613 → gpt-4
	aliasMappings := map[string]string{
		"gpt-4-0314":      "gpt-4",
		"gpt-4-0613":      "gpt-4",
		"gpt-4-turbo":     "gpt-4-turbo",
		"gpt-4-32k":       "gpt-4-32k",
		"gpt-3.5-turbo":   "gpt-3.5-turbo",
		"gpt-3.5-turbo-16k": "gpt-3.5-turbo-16k",
	}

	if canonical, ok := aliasMappings[model]; ok {
		return canonical
	}

	return model
}

// MarshalJSON 自定义 JSON 序列化（用于持久化）
func (ms *ModelState) MarshalJSON() ([]byte, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	// 创建临时结构避免锁递归
	tmp := struct {
		Status         ModelStatus `json:"status"`
		Unavailable    bool        `json:"unavailable"`
		NextRetryAfter time.Time   `json:"next_retry_after,omitempty"`
		LastError      string      `json:"last_error,omitempty"`
		StrikeCount    int         `json:"strike_count"`
		BackoffLevel   int         `json:"backoff_level"`
		UpdatedAt      time.Time   `json:"updated_at"`
	}{
		Status:         ms.Status,
		Unavailable:    ms.Unavailable,
		NextRetryAfter: ms.NextRetryAfter,
		LastError:      ms.LastError,
		StrikeCount:    ms.StrikeCount,
		BackoffLevel:   ms.BackoffLevel,
		UpdatedAt:      ms.UpdatedAt,
	}

	return json.Marshal(tmp)
}

// UnmarshalJSON 自定义 JSON 反序列化（用于加载）
func (ms *ModelState) UnmarshalJSON(data []byte) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	tmp := struct {
		Status         ModelStatus `json:"status"`
		Unavailable    bool        `json:"unavailable"`
		NextRetryAfter time.Time   `json:"next_retry_after,omitempty"`
		LastError      string      `json:"last_error,omitempty"`
		StrikeCount    int         `json:"strike_count"`
		BackoffLevel   int         `json:"backoff_level"`
		UpdatedAt      time.Time   `json:"updated_at"`
	}{}

	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}

	ms.Status = tmp.Status
	ms.Unavailable = tmp.Unavailable
	ms.NextRetryAfter = tmp.NextRetryAfter
	ms.LastError = tmp.LastError
	ms.StrikeCount = tmp.StrikeCount
	ms.BackoffLevel = tmp.BackoffLevel
	ms.UpdatedAt = tmp.UpdatedAt

	return nil
}

// RecomputeAggregatedAccountState 从所有模型状态聚合推导账号级状态
// 规则：只有当所有可路由模型都不可用时，账号才标记为 StatusCooldown
func (acc *Account) RecomputeAggregatedAccountState() {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	// 1. 若账号已有硬故障（StatusError/Disabled/HealthTierBanned），直接返回
	if acc.Status == StatusError || atomic.LoadInt32(&acc.Disabled) == 1 || acc.HealthTier == HealthTierBanned {
		return
	}

	// 2. 若无模型级状态，账号保持可用
	if len(acc.ModelStates) == 0 {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
		return
	}

	// 3. 检查所有模型状态
	now := time.Now()
	earliestRetryAfter := time.Time{}
	allModelsUnavailable := true
	hasAnyModelState := false

	for _, ms := range acc.ModelStates {
		if ms == nil {
			continue
		}
		hasAnyModelState = true

		if ms.IsAvailable(now) {
			allModelsUnavailable = false
			break // 至少一个模型可用，账号保持 Ready
		}

		// 记录最早恢复时间
		retryAfter := ms.GetNextRetryAfter()
		if earliestRetryAfter.IsZero() || retryAfter.Before(earliestRetryAfter) {
			earliestRetryAfter = retryAfter
		}
	}

	// 4. 更新账号聚合状态
	if hasAnyModelState && allModelsUnavailable {
		acc.Status = StatusCooldown
		acc.CooldownUtil = earliestRetryAfter
		acc.CooldownReason = "all_models_rate_limited"
	} else {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
}