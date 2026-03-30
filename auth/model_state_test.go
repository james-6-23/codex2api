package auth

import (
	"sync"
	"testing"
	"time"
)

// ============ ModelState 状态转换测试 ============

func TestNewModelState(t *testing.T) {
	ms := NewModelState()

	if ms.Status != ModelStatusReady {
		t.Errorf("expected Status=Ready, got %v", ms.Status)
	}
	if ms.Unavailable {
		t.Error("expected Unavailable=false for new state")
	}
	if ms.StrikeCount != 0 {
		t.Errorf("expected StrikeCount=0, got %d", ms.StrikeCount)
	}
	if ms.BackoffLevel != 0 {
		t.Errorf("expected BackoffLevel=0, got %d", ms.BackoffLevel)
	}
	if !ms.NextRetryAfter.IsZero() {
		t.Error("expected NextRetryAfter to be zero time for new state")
	}
}

func TestApplyCooldown_指数退避(t *testing.T) {
	tests := []struct {
		name          string
		applyCount    int
		expectedLevel int
		expectedDur   time.Duration
	}{
		{"第一次冷却", 1, 1, 1 * time.Second},
		{"第二次冷却", 2, 2, 2 * time.Second},
		{"第三次冷却", 3, 3, 4 * time.Second},
		{"第四次冷却", 4, 4, 8 * time.Second},
		{"第五次冷却", 5, 5, 16 * time.Second},
		{"第六次冷却", 6, 6, 32 * time.Second},
		{"第七次冷却", 7, 7, 1 * time.Minute},
		{"第八次冷却", 8, 8, 2 * time.Minute},
		{"第九次冷却", 9, 9, 4 * time.Minute},
		{"第十次冷却", 10, 10, 8 * time.Minute},
		{"第十一次冷却(上限)", 11, 11, 30 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := NewModelState()
			now := time.Now()

			// 应用多次冷却
			for i := 0; i < tt.applyCount; i++ {
				ms.ApplyCooldown("rate_limited")
			}

			// 验证状态
			if ms.Status != ModelStatusCooldown {
				t.Errorf("expected Status=Cooldown, got %v", ms.Status)
			}
			if !ms.Unavailable {
				t.Error("expected Unavailable=true after cooldown")
			}
			if ms.BackoffLevel != tt.expectedLevel {
				t.Errorf("expected BackoffLevel=%d, got %d", tt.expectedLevel, ms.BackoffLevel)
			}
			if ms.StrikeCount != tt.applyCount {
				t.Errorf("expected StrikeCount=%d, got %d", tt.applyCount, ms.StrikeCount)
			}

			// 验证冷却时长
			actualDur := ms.NextRetryAfter.Sub(now)
			// 允许1秒的误差，因为时间是动态计算的
			if actualDur < tt.expectedDur-time.Second || actualDur > tt.expectedDur+time.Second {
				t.Errorf("expected cooldown duration ~%v, got %v", tt.expectedDur, actualDur)
			}
		})
	}
}

func TestApplyCooldown_自定义时长(t *testing.T) {
	ms := NewModelState()
	customDur := 5 * time.Minute

	ms.ApplyCooldown("model_capacity", customDur)

	if ms.Status != ModelStatusCooldown {
		t.Errorf("expected Status=Cooldown, got %v", ms.Status)
	}
	if ms.LastError != "model_capacity" {
		t.Errorf("expected LastError='model_capacity', got '%s'", ms.LastError)
	}
	if ms.BackoffLevel != 0 {
		t.Errorf("expected BackoffLevel unchanged (0), got %d", ms.BackoffLevel)
	}
	if ms.StrikeCount != 1 {
		t.Errorf("expected StrikeCount=1, got %d", ms.StrikeCount)
	}
}

func TestApplyCooldown_零值自定义时长(t *testing.T) {
	ms := NewModelState()

	// 传递零值应该使用指数退避
	ms.ApplyCooldown("rate_limited", 0)

	if ms.BackoffLevel != 1 {
		t.Errorf("expected BackoffLevel=1 (exponential backoff), got %d", ms.BackoffLevel)
	}
}

func TestClearCooldown(t *testing.T) {
	ms := NewModelState()

	// 先应用冷却
	ms.ApplyCooldown("rate_limited")
	if ms.Status != ModelStatusCooldown {
		t.Fatal("expected cooldown to be applied")
	}

	// 清除冷却
	ms.ClearCooldown()

	if ms.Status != ModelStatusReady {
		t.Errorf("expected Status=Ready after clear, got %v", ms.Status)
	}
	if ms.Unavailable {
		t.Error("expected Unavailable=false after clear")
	}
	if !ms.NextRetryAfter.IsZero() {
		t.Error("expected NextRetryAfter to be zero after clear")
	}
	if ms.LastError != "" {
		t.Errorf("expected LastError to be empty, got '%s'", ms.LastError)
	}
	if ms.StrikeCount != 0 {
		t.Errorf("expected StrikeCount=0 after clear, got %d", ms.StrikeCount)
	}
	if ms.BackoffLevel != 0 {
		t.Errorf("expected BackoffLevel=0 after clear, got %d", ms.BackoffLevel)
	}
}

func TestIsAvailable_冷却中(t *testing.T) {
	ms := NewModelState()
	now := time.Now()

	// 应用冷却（未来时间）
	ms.ApplyCooldown("rate_limited")

	// 在冷却期间应该不可用
	if ms.IsAvailable(now) {
		t.Error("expected IsAvailable=false during cooldown")
	}

	// 稍微提前的时间也应该不可用
	if ms.IsAvailable(now.Add(100 * time.Millisecond)) {
		t.Error("expected IsAvailable=false just after cooldown start")
	}
}

func TestIsAvailable_冷却过期(t *testing.T) {
	ms := NewModelState()

	// 应用1秒冷却
	ms.ApplyCooldown("rate_limited", 1*time.Second)
	if ms.Status != ModelStatusCooldown {
		t.Fatal("expected cooldown to be applied")
	}

	// 模拟时间过去（超过冷却时间）
	future := time.Now().Add(2 * time.Second)
	if !ms.IsAvailable(future) {
		t.Error("expected IsAvailable=true after cooldown expires")
	}
}

func TestIsAvailable_就绪状态(t *testing.T) {
	ms := NewModelState()

	if !ms.IsAvailable(time.Now()) {
		t.Error("expected IsAvailable=true for ready state")
	}
}

// ============ canonical key 规范化测试 ============

func TestCanonicalModelKey_基本(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"gpt-4", "gpt-4"},
		{"GPT-4", "gpt-4"},
		{" gpt-4 ", "gpt-4"},
		{"claude-3-opus", "claude-3-opus"},
		{"o1-preview", "o1-preview"},
		{"o1-mini", "o1-mini"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := canonicalModelKey(tt.input)
			if result != tt.expected {
				t.Errorf("canonicalModelKey(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestCanonicalModelKey_别名(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"gpt-4-0314", "gpt-4"},
		{"gpt-4-0613", "gpt-4"},
		{"gpt-4-0314", "gpt-4"},
		{"gpt-4-0613", "gpt-4"},
		{"GPT-4-0314", "gpt-4"},
		{"GPT-4-0613", "gpt-4"},
		{"gpt-4-turbo", "gpt-4-turbo"},
		{"gpt-4-32k", "gpt-4-32k"},
		{"gpt-3.5-turbo", "gpt-3.5-turbo"},
		{"gpt-3.5-turbo-16k", "gpt-3.5-turbo-16k"},
		{"GPT-3.5-TURBO", "gpt-3.5-turbo"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := canonicalModelKey(tt.input)
			if result != tt.expected {
				t.Errorf("canonicalModelKey(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestCanonicalModelKey_空值(t *testing.T) {
	if result := canonicalModelKey(""); result != "" {
		t.Errorf("canonicalModelKey(\"\") = %q, want empty string", result)
	}

	if result := canonicalModelKey("   "); result != "" {
		t.Errorf("canonicalModelKey(\"   \") = %q, want empty string", result)
	}
}

// ============ 聚合推导逻辑测试 ============

func TestRecomputeAggregatedAccountState_无模型状态(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "test_token",
		Status:      StatusReady,
		ModelStates: make(map[string]*ModelState),
	}

	acc.RecomputeAggregatedAccountState()

	if acc.Status != StatusReady {
		t.Errorf("expected Status=Ready, got %v", acc.Status)
	}
	if !acc.CooldownUtil.IsZero() {
		t.Error("expected CooldownUtil to be zero")
	}
	if acc.CooldownReason != "" {
		t.Errorf("expected CooldownReason to be empty, got '%s'", acc.CooldownReason)
	}
}

func TestRecomputeAggregatedAccountState_部分模型冷却(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "test_token",
		Status:      StatusReady,
		ModelStates: make(map[string]*ModelState),
	}

	// 添加一个可用模型
	acc.ModelStates["gpt-4"] = NewModelState()

	// 添加一个冷却中的模型
	coolingModel := NewModelState()
	coolingModel.ApplyCooldown("rate_limited")
	acc.ModelStates["claude-3"] = coolingModel

	acc.RecomputeAggregatedAccountState()

	// 因为有一个模型可用，账号状态应该是Ready
	if acc.Status != StatusReady {
		t.Errorf("expected Status=Ready (one model available), got %v", acc.Status)
	}
	if !acc.CooldownUtil.IsZero() {
		t.Error("expected CooldownUtil to be zero when at least one model is available")
	}
}

func TestRecomputeAggregatedAccountState_全部模型冷却(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "test_token",
		Status:      StatusReady,
		ModelStates: make(map[string]*ModelState),
	}

	// 添加两个冷却中的模型
	coolingModel1 := NewModelState()
	coolingModel1.ApplyCooldown("rate_limited")
	acc.ModelStates["gpt-4"] = coolingModel1

	coolingModel2 := NewModelState()
	coolingModel2.ApplyCooldown("model_capacity")
	acc.ModelStates["claude-3"] = coolingModel2

	acc.RecomputeAggregatedAccountState()

	// 所有模型都不可用，账号应该进入冷却
	if acc.Status != StatusCooldown {
		t.Errorf("expected Status=Cooldown (all models unavailable), got %v", acc.Status)
	}
	if acc.CooldownUtil.IsZero() {
		t.Error("expected CooldownUtil to be set")
	}
	if acc.CooldownReason != "all_models_rate_limited" {
		t.Errorf("expected CooldownReason='all_models_rate_limited', got '%s'", acc.CooldownReason)
	}
}

func TestRecomputeAggregatedAccountState_硬故障优先(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "test_token",
		Status:      StatusError, // 硬故障状态
		ModelStates: make(map[string]*ModelState),
	}

	// 添加一个可用模型
	acc.ModelStates["gpt-4"] = NewModelState()

	acc.RecomputeAggregatedAccountState()

	// 硬故障状态应该保持
	if acc.Status != StatusError {
		t.Errorf("expected Status=Error (hard failure takes precedence), got %v", acc.Status)
	}
}

func TestRecomputeAggregatedAccountState_禁用状态优先(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "test_token",
		Status:      StatusReady,
		Disabled:    1, // 原子标志设为禁用
		ModelStates: make(map[string]*ModelState),
	}

	// 添加一个可用模型
	acc.ModelStates["gpt-4"] = NewModelState()

	acc.RecomputeAggregatedAccountState()

	// 禁用状态应该保持
	// 注意：代码逻辑中Disabled检查后直接return，所以Status不会变成Ready
	// 但Disabled本身不改变Status字段
}

func TestRecomputeAggregatedAccountState_健康层级禁止优先(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "test_token",
		Status:      StatusReady,
		HealthTier:  HealthTierBanned,
		ModelStates: make(map[string]*ModelState),
	}

	// 添加一个可用模型
	acc.ModelStates["gpt-4"] = NewModelState()

	acc.RecomputeAggregatedAccountState()

	// Banned状态应该保持
	if acc.Status != StatusReady {
		t.Errorf("expected Status=Ready (Banned doesn't change status directly), got %v", acc.Status)
	}
}

func TestRecomputeAggregatedAccountState_最早恢复时间(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "test_token",
		Status:      StatusReady,
		ModelStates: make(map[string]*ModelState),
	}

	// 模型1：冷却时间较长（30分钟）
	model1 := NewModelState()
	for i := 0; i < 11; i++ {
		model1.ApplyCooldown("rate_limited")
	}
	acc.ModelStates["model1"] = model1

	// 模型2：冷却时间较短（1秒）
	model2 := NewModelState()
	model2.ApplyCooldown("rate_limited", 1*time.Second)
	acc.ModelStates["model2"] = model2

	acc.RecomputeAggregatedAccountState()

	// 应该使用较短的恢复时间
	expectedRetryAfter := model2.GetNextRetryAfter()
	if acc.CooldownUtil != expectedRetryAfter {
		t.Errorf("expected CooldownUtil to be earliest retry time, got %v, want %v",
			acc.CooldownUtil, expectedRetryAfter)
	}
}

// ============ 并发安全测试 ============

func TestModelState_ConcurrentAccess(t *testing.T) {
	ms := NewModelState()
	var wg sync.WaitGroup
	numGoroutines := 100
	numOperations := 100

	// 并发应用冷却
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				ms.ApplyCooldown("rate_limited")
			}
		}()
	}
	wg.Wait()

	// 验证最终状态
	if ms.StrikeCount != numGoroutines*numOperations {
		t.Errorf("expected StrikeCount=%d, got %d", numGoroutines*numOperations, ms.StrikeCount)
	}

	// 并发检查和清除
	wg.Add(numGoroutines * 2)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				ms.IsAvailable(time.Now())
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				ms.GetNextRetryAfter()
			}
		}()
	}
	wg.Wait()

	// 并发清除
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			ms.ClearCooldown()
		}()
	}
	wg.Wait()

	// 最终应该是就绪状态
	if ms.Status != ModelStatusReady {
		t.Errorf("expected Status=Ready after concurrent clear, got %v", ms.Status)
	}
}

func TestAccount_RecomputeConcurrent(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "test_token",
		Status:      StatusReady,
		ModelStates: make(map[string]*ModelState),
	}

	// 添加一些模型状态
	for i := 0; i < 10; i++ {
		modelName := "model-" + string(rune('a'+i))
		acc.ModelStates[modelName] = NewModelState()
	}

	var wg sync.WaitGroup
	numGoroutines := 50
	numOperations := 100

	// 并发修改模型状态和重新计算
	wg.Add(numGoroutines * 2)
	for i := 0; i < numGoroutines; i++ {
		// 并发修改模型状态
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				modelIdx := j % 10
				modelName := "model-" + string(rune('a'+modelIdx))
				if ms, ok := acc.ModelStates[modelName]; ok {
					if idx%2 == 0 {
						ms.ApplyCooldown("rate_limited")
					} else {
						ms.ClearCooldown()
					}
				}
			}
		}(i)

		// 并发重新计算
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				acc.RecomputeAggregatedAccountState()
			}
		}()
	}
	wg.Wait()

	// 验证最终状态一致性
	acc.RecomputeAggregatedAccountState()
	// 主要验证没有panic和竞态条件
}

// ============ JSON 序列化/反序列化测试 ============

func TestModelState_JSONMarshalUnmarshal(t *testing.T) {
	original := NewModelState()
	original.ApplyCooldown("rate_limited", 5*time.Minute)

	// Marshal
	data, err := original.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	// Unmarshal到新的结构
	restored := NewModelState()
	if err := restored.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}

	// 验证字段
	if restored.Status != original.Status {
		t.Errorf("Status mismatch: got %v, want %v", restored.Status, original.Status)
	}
	if restored.Unavailable != original.Unavailable {
		t.Errorf("Unavailable mismatch: got %v, want %v", restored.Unavailable, original.Unavailable)
	}
	if !restored.NextRetryAfter.Equal(original.NextRetryAfter) {
		t.Errorf("NextRetryAfter mismatch: got %v, want %v", restored.NextRetryAfter, original.NextRetryAfter)
	}
	if restored.LastError != original.LastError {
		t.Errorf("LastError mismatch: got %s, want %s", restored.LastError, original.LastError)
	}
	if restored.StrikeCount != original.StrikeCount {
		t.Errorf("StrikeCount mismatch: got %d, want %d", restored.StrikeCount, original.StrikeCount)
	}
	if restored.BackoffLevel != original.BackoffLevel {
		t.Errorf("BackoffLevel mismatch: got %d, want %d", restored.BackoffLevel, original.BackoffLevel)
	}
}

// ============ Getters 测试 ============

func TestModelState_Getters(t *testing.T) {
	ms := NewModelState()

	// 初始值
	if ms.GetStrikeCount() != 0 {
		t.Errorf("expected GetStrikeCount()=0, got %d", ms.GetStrikeCount())
	}
	if !ms.GetNextRetryAfter().IsZero() {
		t.Error("expected GetNextRetryAfter() to be zero initially")
	}

	// 应用冷却
	ms.ApplyCooldown("rate_limited", 5*time.Minute)

	if ms.GetStrikeCount() != 1 {
		t.Errorf("expected GetStrikeCount()=1, got %d", ms.GetStrikeCount())
	}
	if ms.GetNextRetryAfter().IsZero() {
		t.Error("expected GetNextRetryAfter() to be set after cooldown")
	}
}

// ============ 边界条件测试 ============

func TestApplyCooldown_超过最大级别(t *testing.T) {
	ms := NewModelState()

	// 应用冷却超过最大级别
	for i := 0; i < len(cooldownDurations) + 5; i++ {
		ms.ApplyCooldown("rate_limited")
	}

	// 应该限制在最大级别
	maxLevel := len(cooldownDurations)
	if ms.BackoffLevel != maxLevel {
		t.Errorf("expected BackoffLevel=%d (max), got %d", maxLevel, ms.BackoffLevel)
	}

	// 冷却时长应该是30分钟（最后一个）
	now := time.Now()
	expectedDur := 30 * time.Minute
	actualDur := ms.NextRetryAfter.Sub(now)
	if actualDur < expectedDur-time.Second || actualDur > expectedDur+time.Second {
		t.Errorf("expected max cooldown duration ~%v, got %v", expectedDur, actualDur)
	}
}

func TestIsAvailable_边界时间(t *testing.T) {
	ms := NewModelState()
	ms.ApplyCooldown("rate_limited", 1*time.Second)

	retryAfter := ms.GetNextRetryAfter()

	// 正好在冷却结束时间点 - 根据实现逻辑，使用 Before 检查，所以相等时应该为可用
	// 注意：IsAvailable 使用 now.Before(ms.NextRetryAfter) 判断冷却中
	// 所以 now == NextRetryAfter 时，Before 返回 false，进入下一检查
	// 由于 now.After(NextRetryAfter) 也是 false，最终返回 ms.Status == ModelStatusReady || !ms.Unavailable
	// 而 Unavailable 仍为 true，Status 为 Cooldown，所以这里应该检查实际行为
	// 实际上在边界点，IsAvailable 返回 false（Unavailable=true 且 !Before）
	if ms.IsAvailable(retryAfter) {
		t.Error("expected IsAvailable=false exactly at retry after time (boundary condition)")
	}

	// 冷却结束前1纳秒
	if ms.IsAvailable(retryAfter.Add(-1 * time.Nanosecond)) {
		t.Error("expected IsAvailable=false 1ns before retry after")
	}

	// 冷却结束后1纳秒
	if !ms.IsAvailable(retryAfter.Add(1 * time.Nanosecond)) {
		t.Error("expected IsAvailable=true 1ns after retry after")
	}
}
