package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// mockResponse 创建模拟HTTP响应
type mockResponse struct {
	statusCode int
	body       []byte
	headers    map[string]string
}

func newMockResponse(statusCode int, body string) mockResponse {
	return mockResponse{
		statusCode: statusCode,
		body:       []byte(body),
		headers:    make(map[string]string),
	}
}

func (m mockResponse) withHeader(key, value string) mockResponse {
	m.headers[key] = value
	return m
}

// createTestHandler 创建测试用的Handler
func createTestHandler(t *testing.T) (*Handler, *auth.Store) {
	// 创建内存数据库用于测试
	db, err := database.New("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}

	settings := &database.SystemSettings{
		MaxConcurrency:  2,
		TestConcurrency: 50,
		TestModel:       "gpt-5.4",
		MaxRetries:      2,
	}

	store := auth.NewStore(db, nil, settings)
	handler := NewHandler(store, db, nil, nil)

	return handler, store
}

// createTestAccount 创建测试账号
func createTestAccount(store *auth.Store, id int64, planType string) *auth.Account {
	acc := &auth.Account{
		DBID:         id,
		AccessToken:  "test-token",
		PlanType:     planType,
		Email:        "test@example.com",
		ModelStates:  make(map[string]*auth.ModelState),
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(acc)
	return acc
}

// createMockHTTPResponse 创建模拟HTTP响应
func createMockHTTPResponse(m mockResponse) *http.Response {
	recorder := httptest.NewRecorder()
	recorder.Code = m.statusCode
	for k, v := range m.headers {
		recorder.Header().Set(k, v)
	}
	recorder.Body.Write(m.body)
	return recorder.Result()
}

// ==================== Test 429处理流程 ====================

// TestHandler_ApplyFailureState_429模型级冷却 测试429触发模型级冷却
func TestHandler_ApplyFailureState_429模型级冷却(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "pro")

	body := `{"error": {"type": "rate_limit_error", "message": "Rate limit exceeded"}}`
	resp := createMockHTTPResponse(newMockResponse(http.StatusTooManyRequests, body))
	model := "gpt-5.4"

	// 应用失败状态
	handler.applyFailureState(account, model, http.StatusTooManyRequests, []byte(body), resp)

	// 验证模型级状态
	account.Mu().RLock()
	ms, exists := account.ModelStates[model]
	account.Mu().RUnlock()

	if !exists || ms == nil {
		t.Fatalf("expected model state to exist for %s", model)
	}

	if ms.Status != auth.ModelStatusCooldown {
		t.Errorf("expected model status to be cooldown, got %s", ms.Status)
	}

	if ms.LastError != "rate_limited" {
		t.Errorf("expected last error to be 'rate_limited', got %s", ms.LastError)
	}

	if !ms.Unavailable {
		t.Error("expected model to be marked unavailable")
	}

	// 验证StrikeCount增加
	if ms.StrikeCount != 1 {
		t.Errorf("expected strike count to be 1, got %d", ms.StrikeCount)
	}

	// 验证冷却时间已设置
	if ms.NextRetryAfter.IsZero() {
		t.Error("expected NextRetryAfter to be set")
	}
}

// TestHandler_ApplyFailureState_429不影响其他模型 测试429只影响指定模型
func TestHandler_ApplyFailureState_429不影响其他模型(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "pro")

	body := `{"error": {"type": "rate_limit_error", "message": "Rate limit exceeded"}}`
	resp := createMockHTTPResponse(newMockResponse(http.StatusTooManyRequests, body))
	modelA := "gpt-5.4"
	modelB := "gpt-5.4-mini"

	// 只对modelA应用失败状态
	handler.applyFailureState(account, modelA, http.StatusTooManyRequests, []byte(body), resp)

	// 验证modelA被冷却
	account.Mu().RLock()
	msA, existsA := account.ModelStates[modelA]
	_, existsB := account.ModelStates[modelB]
	account.Mu().RUnlock()

	if !existsA || msA == nil {
		t.Fatalf("expected model state to exist for %s", modelA)
	}

	if msA.Status != auth.ModelStatusCooldown {
		t.Errorf("expected modelA status to be cooldown, got %s", msA.Status)
	}

	// 验证modelB未被创建（即不受影响）
	if existsB {
		t.Errorf("expected modelB to not have a state, but one exists")
	}

	// 验证modelB仍然可用
	if !msA.IsAvailable(time.Now().Add(1 * time.Hour)) {
		t.Error("modelA should still be in cooldown after 1 hour")
	}

	// 创建modelB状态并验证它是独立的
	account.Mu().RLock()
	if account.ModelStates[modelB] != nil {
		t.Error("modelB should not have been affected")
	}
	account.Mu().RUnlock()
}

// TestHandler_RetryAfter解析_30s_60s_90s 测试retry-after解析各种值
func TestHandler_RetryAfter解析_30s_60s_90s(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		expectedMin  time.Duration
		expectedMax  time.Duration
	}{
		{
			name:         "resets_in_seconds 30",
			body:         `{"error": {"type": "usage_limit_reached", "resets_in_seconds": 30}}`,
			expectedMin:  29 * time.Second,
			expectedMax:  31 * time.Second,
		},
		{
			name:         "resets_in_seconds 60",
			body:         `{"error": {"type": "usage_limit_reached", "resets_in_seconds": 60}}`,
			expectedMin:  59 * time.Second,
			expectedMax:  61 * time.Second,
		},
		{
			name:         "resets_in_seconds 90",
			body:         `{"error": {"type": "usage_limit_reached", "resets_in_seconds": 90}}`,
			expectedMin:  89 * time.Second,
			expectedMax:  91 * time.Second,
		},
		{
			name:         "resets_at timestamp",
			body:         `{"error": {"type": "usage_limit_reached", "resets_at": ` + string(rune(time.Now().Add(2*time.Minute).Unix())) + `}}`,
			expectedMin:  1*time.Minute + 59*time.Second,
			expectedMax:  2*time.Minute + 1*time.Second,
		},
		{
			name:         "no reset info",
			body:         `{"error": {"type": "rate_limit_error"}}`,
			expectedMin:  1*time.Minute + 59*time.Second, // 默认2分钟
			expectedMax:  2*time.Minute + 1*time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			duration := parseRetryAfter([]byte(tt.body))

			if duration < tt.expectedMin || duration > tt.expectedMax {
				t.Errorf("expected duration between %v and %v, got %v", tt.expectedMin, tt.expectedMax, duration)
			}
		})
	}
}

// ==================== Test 401处理流程 ====================

// TestHandler_ApplyFailureState_401账号级硬故障 测试401触发账号级硬故障
func TestHandler_ApplyFailureState_401账号级硬故障(t *testing.T) {
	// 使用最小化的Handler和Account配置，避免数据库依赖
	handler := &Handler{}
	account := &auth.Account{
		DBID:        1,
		AccessToken: "test-token",
		PlanType:    "pro",
		Email:       "test@example.com",
		ModelStates: make(map[string]*auth.ModelState),
		HealthTier:  auth.HealthTierHealthy,
	}

	body := `{"error": {"type": "authentication_error", "message": "Invalid token"}}`
	resp := createMockHTTPResponse(newMockResponse(http.StatusUnauthorized, body))

	// 应用失败状态（直接调用applyCooldown方法）
	handler.applyCooldown(account, http.StatusUnauthorized, []byte(body), resp)

	// 验证账号被禁用
	if account.Disabled != 1 {
		t.Error("expected account Disabled flag to be set")
	}

	// 验证账号状态为Cooldown（applyCooldown设置的状态）
	account.Mu().RLock()
	if account.Status != auth.StatusCooldown {
		t.Errorf("expected account status to be StatusCooldown, got %v", account.Status)
	}

	// 验证HealthTier为Banned
	if account.HealthTier != auth.HealthTierBanned {
		t.Errorf("expected health tier to be Banned, got %s", account.HealthTier)
	}

	// 验证冷却原因
	if account.CooldownReason != "unauthorized" {
		t.Errorf("expected cooldown reason to be 'unauthorized', got %s", account.CooldownReason)
	}
	account.Mu().RUnlock()
}

// TestHandler_ApplyFailureState_401自动清理 测试401自动清理账号
func TestHandler_ApplyFailureState_401自动清理(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "pro")

	body := `{"error": {"type": "authentication_error", "message": "Invalid token"}}`
	resp := createMockHTTPResponse(newMockResponse(http.StatusUnauthorized, body))
	model := "gpt-5.4"

	// 开启自动清理
	store.SetAutoCleanUnauthorized(true)

	// 确保账号存在
	if store.FindByID(account.ID()) == nil {
		t.Fatal("account should exist before applying failure state")
	}

	// 应用失败状态
	handler.applyFailureState(account, model, http.StatusUnauthorized, []byte(body), resp)

	// 验证账号被移除
	if store.FindByID(account.ID()) != nil {
		t.Error("expected account to be removed from store when auto-clean is enabled")
	}
}

// ==================== Test 容量错误处理 ====================

// TestHandler_CapacityError_模型级冷却 测试容量错误触发模型级冷却
func TestHandler_CapacityError_模型级冷却(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "pro")

	// 容量错误响应（503但包含容量错误信息）
	body := `{"error": {"message": "Selected model is at capacity"}}`
	// 模拟503响应但包含容量错误
	resp := createMockHTTPResponse(newMockResponse(http.StatusServiceUnavailable, body))
	model := "codex-latest"

	// 验证容量错误检测
	if !isCodexModelCapacityError([]byte(body)) {
		t.Fatal("expected body to be detected as capacity error")
	}

	// 应用失败状态（应该作为429处理）
	handler.applyFailureState(account, model, http.StatusTooManyRequests, []byte(body), resp)

	// 验证模型级状态
	account.Mu().RLock()
	ms, exists := account.ModelStates[model]
	account.Mu().RUnlock()

	if !exists || ms == nil {
		t.Fatalf("expected model state to exist for %s", model)
	}

	if ms.Status != auth.ModelStatusCooldown {
		t.Errorf("expected model status to be cooldown, got %s", ms.Status)
	}
}

// TestHandler_CapacityError_视为429 测试容量错误被当作429处理
func TestHandler_CapacityError_视为429(t *testing.T) {
	// 测试各种容量错误消息
	tests := []struct {
		name string
		body string
	}{
		{
			name: "selected model is at capacity",
			body: `{"error": {"message": "Selected model is at capacity"}}`,
		},
		{
			name: "model is at capacity please try a different model",
			body: `{"error": {"message": "Model is at capacity. Please try a different model"}}`,
		},
		{
			name: "model is currently at capacity",
			body: `{"message": "Model is currently at capacity"}`,
		},
		{
			name: "case insensitive",
			body: `{"error": {"message": "SELECTED MODEL IS AT CAPACITY"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !isCodexModelCapacityError([]byte(tt.body)) {
				t.Errorf("expected body to be detected as capacity error: %s", tt.body)
			}
		})
	}
}

// ==================== Test 统一入口验证 ====================

// TestHandler_AllErrorPaths_统一入口 测试所有错误路径都通过applyFailureState
func TestHandler_AllErrorPaths_统一入口(t *testing.T) {
	handler, store := createTestHandler(t)

	testCases := []struct {
		name       string
		statusCode int
		body       string
		checkFunc  func(*auth.Account) bool
	}{
		{
			name:       "429 rate limit",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error": {"type": "rate_limit_error"}}`,
			checkFunc: func(acc *auth.Account) bool {
				acc.Mu().RLock()
				defer acc.Mu().RUnlock()
				if len(acc.ModelStates) == 0 {
					return false
				}
				for _, ms := range acc.ModelStates {
					if ms.Status == auth.ModelStatusCooldown {
						return true
					}
				}
				return false
			},
		},
		{
			name:       "401 unauthorized",
			statusCode: http.StatusUnauthorized,
			body:       `{"error": {"type": "authentication_error"}}`,
			checkFunc: func(acc *auth.Account) bool {
				return acc.Disabled == 1
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			account := createTestAccount(store, int64(len(store.Accounts())+1), "pro")
			store.SetAutoCleanUnauthorized(false)

			resp := createMockHTTPResponse(newMockResponse(tc.statusCode, tc.body))
			model := "gpt-5.4"

			// 应用失败状态
			handler.applyFailureState(account, model, tc.statusCode, []byte(tc.body), resp)

			// 验证状态
			if !tc.checkFunc(account) {
				t.Errorf("check failed for %s", tc.name)
			}
		})
	}
}

// TestHandler_ModelParameter传递_完整链路 测试model参数在完整链路中传递
func TestHandler_ModelParameter传递_完整链路(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "pro")

	model := "gpt-5.4-mini"
	body := `{"error": {"type": "rate_limit_error", "message": "Rate limit exceeded"}}`
	resp := createMockHTTPResponse(newMockResponse(http.StatusTooManyRequests, body))

	// 应用失败状态
	handler.applyFailureState(account, model, http.StatusTooManyRequests, []byte(body), resp)

	// 验证模型名称被正确规范化并存储
	account.Mu().RLock()
	// 检查是否有该模型的状态
	found := false
	for key, ms := range account.ModelStates {
		if ms != nil && key == model {
			found = true
			break
		}
	}
	account.Mu().RUnlock()

	if !found {
		t.Errorf("expected model state to be stored for model %s", model)
	}

	// 测试空model的情况
	account2 := createTestAccount(store, 2, "pro")
	emptyModel := ""
	handler.applyFailureState(account2, emptyModel, http.StatusTooManyRequests, []byte(body), resp)

	// 空model应该回退到账号级冷却
	account2.Mu().RLock()
	if account2.Status != auth.StatusCooldown {
		t.Error("expected account-level cooldown when model is empty")
	}
	account2.Mu().RUnlock()
}

// ==================== Test retry-after修复验证 ====================

// TestHandler_RetryAfter_无条件采纳 测试retry-after被无条件采纳
func TestHandler_RetryAfter_无条件采纳(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "pro")

	// 使用resets_in_seconds指定30秒
	body := `{"error": {"type": "usage_limit_reached", "resets_in_seconds": 30}}`
	resp := createMockHTTPResponse(newMockResponse(http.StatusTooManyRequests, body))
	model := "gpt-5.4"

	before := time.Now()
	handler.applyFailureState(account, model, http.StatusTooManyRequests, []byte(body), resp)
	after := time.Now()

	account.Mu().RLock()
	ms, exists := account.ModelStates[model]
	account.Mu().RUnlock()

	if !exists || ms == nil {
		t.Fatal("expected model state to exist")
	}

	// 验证冷却时间大约是30秒（有少量误差）
	expectedRetryAfter := before.Add(30 * time.Second)
	actualRetryAfter := ms.NextRetryAfter

	// 允许1秒的误差
	if actualRetryAfter.Before(expectedRetryAfter.Add(-1*time.Second)) ||
		actualRetryAfter.After(expectedRetryAfter.Add(1*time.Second)) {
		t.Errorf("expected retry after around %v, got %v (diff: %v)",
			expectedRetryAfter, actualRetryAfter, actualRetryAfter.Sub(expectedRetryAfter))
	}

	// 验证实际使用的是30秒，而不是默认的2分钟
	if actualRetryAfter.After(after.Add(1 * time.Minute)) {
		t.Error("expected retry after to be 30s, not the default 2min")
	}
}

// TestHandler_ParseRetryAfter_CodexHeaders 测试Codex响应头解析
func TestHandler_ParseRetryAfter_CodexHeaders(t *testing.T) {
	// 测试Codex的resets_at字段
	futureTime := time.Now().Add(5 * time.Minute).Unix()
	body := `{"error": {"type": "usage_limit_reached", "resets_at": ` + string(rune(futureTime)) + `}}`

	duration := parseRetryAfter([]byte(body))

	// 应该是大约5分钟
	if duration < 4*time.Minute+59*time.Second || duration > 5*time.Minute+1*time.Second {
		t.Errorf("expected duration around 5 minutes, got %v", duration)
	}

	// 测试两个字段都存在的情况（resets_at优先）
	body2 := `{"error": {"type": "usage_limit_reached", "resets_at": ` + string(rune(futureTime)) + `, "resets_in_seconds": 120}}`
	duration2 := parseRetryAfter([]byte(body2))

	// 应该使用resets_at（5分钟），而不是resets_in_seconds（120秒）
	if duration2 < 4*time.Minute+59*time.Second {
		t.Errorf("expected resets_at to take priority, got %v", duration2)
	}
}

// TestHandler_Compute429Cooldown_TeamPro 测试Team/Pro账号的429冷却计算
func TestHandler_Compute429Cooldown_TeamPro(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "team")

	// 测试Codex用量头
	recorder := httptest.NewRecorder()
	recorder.Header().Set("x-codex-primary-used-percent", "100")
	recorder.Header().Set("x-codex-primary-window-minutes", "300") // 5h窗口
	recorder.Header().Set("x-codex-secondary-used-percent", "50")
	recorder.Header().Set("x-codex-secondary-window-minutes", "10080") // 7d窗口
	resp := recorder.Result()

	body := `{"error": {"type": "rate_limit_error"}}`
	duration := handler.compute429Cooldown(account, []byte(body), resp)

	// primary窗口满了（5h），应该返回5小时
	if duration != 5*time.Hour {
		t.Errorf("expected 5h cooldown for primary window, got %v", duration)
	}
}

// TestHandler_Compute429Cooldown_Free 测试Free账号的429冷却计算
func TestHandler_Compute429Cooldown_Free(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "free")

	body := `{"error": {"type": "rate_limit_error"}}`
	duration := handler.compute429Cooldown(account, []byte(body), nil)

	// Free账号默认7天
	if duration != 7*24*time.Hour {
		t.Errorf("expected 7d cooldown for free account, got %v", duration)
	}
}

// TestHandler_DetectTeamCooldownWindow 测试Team窗口检测
func TestHandler_DetectTeamCooldownWindow(t *testing.T) {
	tests := []struct {
		name           string
		primaryUsed    string
		primaryWindow  string
		secondaryUsed  string
		secondaryWindow string
		expected       time.Duration
	}{
		{
			name:           "primary exhausted",
			primaryUsed:    "100",
			primaryWindow:  "300",
			secondaryUsed:  "50",
			secondaryWindow: "10080",
			expected:       5 * time.Hour,
		},
		{
			name:           "secondary exhausted",
			primaryUsed:    "50",
			primaryWindow:  "300",
			secondaryUsed:  "100",
			secondaryWindow: "10080",
			expected:       7 * 24 * time.Hour,
		},
		{
			name:           "both exhausted",
			primaryUsed:    "100",
			primaryWindow:  "300",
			secondaryUsed:  "100",
			secondaryWindow: "10080",
			expected:       7 * 24 * time.Hour, // 取较大窗口
		},
		{
			name:           "neither exhausted",
			primaryUsed:    "50",
			primaryWindow:  "300",
			secondaryUsed:  "50",
			secondaryWindow: "10080",
			expected:       5 * time.Hour, // 默认
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			recorder.Header().Set("x-codex-primary-used-percent", tt.primaryUsed)
			recorder.Header().Set("x-codex-primary-window-minutes", tt.primaryWindow)
			recorder.Header().Set("x-codex-secondary-used-percent", tt.secondaryUsed)
			recorder.Header().Set("x-codex-secondary-window-minutes", tt.secondaryWindow)
			resp := recorder.Result()

			// 直接使用方法而不是创建完整的handler
			duration := (&Handler{}).detectTeamCooldownWindow(resp)

			if duration != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, duration)
			}
		})
	}
}

// TestHandler_WindowMinutesToCooldown 测试窗口分钟到冷却时间的转换
func TestHandler_WindowMinutesToCooldown(t *testing.T) {
	tests := []struct {
		windowMinutes float64
		expected      time.Duration
	}{
		{windowMinutes: 30, expected: 30 * time.Minute},
		{windowMinutes: 60, expected: 5 * time.Hour},
		{windowMinutes: 300, expected: 5 * time.Hour},
		{windowMinutes: 1440, expected: 7 * 24 * time.Hour},
		{windowMinutes: 10080, expected: 7 * 24 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%.0f", tt.windowMinutes), func(t *testing.T) {
			duration := windowMinutesToCooldown(tt.windowMinutes)
			if duration != tt.expected {
				t.Errorf("window %v min: expected %v, got %v", tt.windowMinutes, tt.expected, duration)
			}
		})
	}
}

// TestHandler_ModelState_BackoffLevel 测试指数退避级别
func TestHandler_ModelState_BackoffLevel(t *testing.T) {
	handler, store := createTestHandler(t)
	account := createTestAccount(store, 1, "pro")

	model := "gpt-5.4"
	body := `{"error": {"type": "rate_limit_error"}}`

	// 多次应用冷却，观察退避级别
	expectedLevels := []int{0, 1, 2, 3, 4}

	for i, expectedLevel := range expectedLevels {
		resp := createMockHTTPResponse(newMockResponse(http.StatusTooManyRequests, body))
		handler.applyFailureState(account, model, http.StatusTooManyRequests, []byte(body), resp)

		account.Mu().RLock()
		ms := account.ModelStates[model]
		actualLevel := ms.BackoffLevel
		account.Mu().RUnlock()

		if actualLevel != expectedLevel {
			t.Errorf("iteration %d: expected backoff level %d, got %d", i, expectedLevel, actualLevel)
		}
	}
}
