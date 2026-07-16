package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// ==================== Response Writer Tests ====================

func TestWriteError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	writeError(ctx, http.StatusBadRequest, "test error message")

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var resp errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if resp.Error != "test error message" {
		t.Errorf("error = %q, want 'test error message'", resp.Error)
	}
}

func TestWriteMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	writeMessage(ctx, http.StatusOK, "operation successful")

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var resp messageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if resp.Message != "operation successful" {
		t.Errorf("message = %q, want 'operation successful'", resp.Message)
	}
}

func TestWriteInternalError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	writeInternalError(ctx, fmt.Errorf("database connection failed"))

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}

	var resp errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if resp.Error != "database connection failed" {
		t.Errorf("error = %q, want 'database connection failed'", resp.Error)
	}
}

func TestStatsResponse(t *testing.T) {
	resp := statsResponse{
		Total:         10,
		Available:     8,
		RateLimited:   1,
		Error:         2,
		TodayRequests: 1000,
	}

	// Verify JSON serialization includes the expected fields with correct keys
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal statsResponse: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal statsResponse JSON: %v", err)
	}

	// Check that JSON keys match expected field names
	if total, ok := decoded["total"]; !ok || total != float64(10) {
		t.Errorf("expected JSON to contain \"total\" field = 10, got %v", total)
	}
	if available, ok := decoded["available"]; !ok || available != float64(8) {
		t.Errorf("expected JSON to contain \"available\" field = 8, got %v", available)
	}
	if rateLimited, ok := decoded["rate_limited"]; !ok || rateLimited != float64(1) {
		t.Errorf("expected JSON to contain \"rate_limited\" field = 1, got %v", rateLimited)
	}
	if errorField, ok := decoded["error"]; !ok || errorField != float64(2) {
		t.Errorf("expected JSON to contain \"error\" field = 2, got %v", errorField)
	}
	if todayRequests, ok := decoded["today_requests"]; !ok || todayRequests != float64(1000) {
		t.Errorf("expected JSON to contain \"today_requests\" field = 1000, got %v", todayRequests)
	}
}

func TestHealthResponse(t *testing.T) {
	resp := healthResponse{
		Status:    "healthy",
		Available: 8,
		Total:     10,
	}

	if resp.Status != "healthy" {
		t.Errorf("Status = %q, want 'healthy'", resp.Status)
	}
	if resp.Available != 8 {
		t.Errorf("Available = %d, want 8", resp.Available)
	}
}

func TestCreateAccountResponse(t *testing.T) {
	resp := createAccountResponse{
		ID:      123,
		Message: "Account created successfully",
	}

	if resp.ID != 123 {
		t.Errorf("ID = %d, want 123", resp.ID)
	}
	if resp.Message != "Account created successfully" {
		t.Errorf("Message = %q, want 'Account created successfully'", resp.Message)
	}
}

func TestCreateAPIKeyResponse(t *testing.T) {
	resp := createAPIKeyResponse{
		ID:   456,
		Key:  "api-key-secret",
		Name: "Test Key",
	}

	if resp.ID != 456 {
		t.Errorf("ID = %d, want 456", resp.ID)
	}
	if resp.Key != "api-key-secret" {
		t.Errorf("Key = %q, want 'api-key-secret'", resp.Key)
	}
}

func TestOpsResponseTypes(t *testing.T) {
	cpuResp := opsCPUResponse{
		Percent: 45.5,
		Cores:   8,
	}
	if cpuResp.Cores != 8 {
		t.Errorf("Cores = %d, want 8", cpuResp.Cores)
	}

	memResp := opsMemoryResponse{
		Percent:    60.0,
		UsedBytes:  8 * 1024 * 1024 * 1024,
		TotalBytes: 16 * 1024 * 1024 * 1024,
	}
	if memResp.UsedBytes != 8*1024*1024*1024 {
		t.Errorf("UsedBytes = %d, want 8GB", memResp.UsedBytes)
	}

	runtimeResp := opsRuntimeResponse{
		Goroutines:        50,
		AvailableAccounts: 8,
		TotalAccounts:     10,
	}
	if runtimeResp.Goroutines != 50 {
		t.Errorf("Goroutines = %d, want 50", runtimeResp.Goroutines)
	}

	requestsResp := opsRequestsResponse{
		Active: 5,
		Total:  1000,
	}
	if requestsResp.Active != 5 {
		t.Errorf("Active = %d, want 5", requestsResp.Active)
	}

	dbResp := opsDatabaseResponse{
		Healthy:      true,
		Open:         5,
		InUse:        2,
		Idle:         3,
		MaxOpen:      20,
		WaitCount:    0,
		UsagePercent: 25.0,
	}
	if !dbResp.Healthy {
		t.Error("Healthy should be true")
	}

	redisResp := opsRedisResponse{
		Healthy:      true,
		TotalConns:   10,
		IdleConns:    5,
		StaleConns:   0,
		PoolSize:     20,
		UsagePercent: 50.0,
	}
	if redisResp.PoolSize != 20 {
		t.Errorf("PoolSize = %d, want 20", redisResp.PoolSize)
	}

	trafficResp := opsTrafficResponse{
		QPS:           10.5,
		QPSPeak:       50.0,
		TPS:           100.0,
		TPSPeak:       500.0,
		RPM:           600.0,
		TPM:           6000.0,
		ErrorRate:     0.01,
		TodayRequests: 10000,
		TodayTokens:   50000,
		RPMLimit:      1000,
	}
	if trafficResp.RPMLimit != 1000 {
		t.Errorf("RPMLimit = %d, want 1000", trafficResp.RPMLimit)
	}
}

func TestOpsOverviewResponse(t *testing.T) {
	resp := opsOverviewResponse{
		UpdatedAt:      time.Now().Format(time.RFC3339),
		UptimeSeconds:  3600,
		DatabaseDriver: "postgres",
		DatabaseLabel:  "PostgreSQL",
		CacheDriver:    "redis",
		CacheLabel:     "Redis",
		CPU: opsCPUResponse{
			Percent: 30.0,
			Cores:   runtime.NumCPU(),
		},
		Memory: opsMemoryResponse{
			Percent:    50.0,
			UsedBytes:  8 * 1024 * 1024 * 1024,
			TotalBytes: 16 * 1024 * 1024 * 1024,
		},
		Runtime: opsRuntimeResponse{
			Goroutines:        100,
			AvailableAccounts: 5,
			TotalAccounts:     10,
		},
		Requests: opsRequestsResponse{
			Active: 10,
			Total:  5000,
		},
		Postgres: opsDatabaseResponse{
			Healthy:      true,
			Open:         5,
			InUse:        2,
			Idle:         3,
			MaxOpen:      20,
			WaitCount:    0,
			UsagePercent: 25.0,
		},
		Redis: opsRedisResponse{
			Healthy:      true,
			TotalConns:   10,
			IdleConns:    5,
			StaleConns:   0,
			PoolSize:     20,
			UsagePercent: 50.0,
		},
		Traffic: opsTrafficResponse{
			QPS:           5.5,
			QPSPeak:       25.0,
			TPS:           55.0,
			TPSPeak:       250.0,
			RPM:           330.0,
			TPM:           3300.0,
			ErrorRate:     0.005,
			TodayRequests: 5000,
			TodayTokens:   25000,
			RPMLimit:      1000,
		},
	}

	if resp.DatabaseDriver != "postgres" {
		t.Errorf("DatabaseDriver = %q, want 'postgres'", resp.DatabaseDriver)
	}
	if resp.CacheDriver != "redis" {
		t.Errorf("CacheDriver = %q, want 'redis'", resp.CacheDriver)
	}
	if resp.UptimeSeconds != 3600 {
		t.Errorf("UptimeSeconds = %d, want 3600", resp.UptimeSeconds)
	}
}

// ==================== Response Serialization Tests ====================

func TestStatsResponseJSON(t *testing.T) {
	resp := statsResponse{
		Total:         10,
		Available:     8,
		RateLimited:   1,
		Error:         2,
		TodayRequests: 1000,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	var decoded statsResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if decoded.Total != resp.Total {
		t.Errorf("Total mismatch: got %d, want %d", decoded.Total, resp.Total)
	}
}

func TestErrorResponseJSON(t *testing.T) {
	resp := errorResponse{Error: "test error"}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	expected := `{"error":"test error"}`
	if string(data) != expected {
		t.Errorf("JSON = %s, want %s", string(data), expected)
	}
}

func TestMessageResponseJSON(t *testing.T) {
	resp := messageResponse{Message: "success"}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	expected := `{"message":"success"}`
	if string(data) != expected {
		t.Errorf("JSON = %s, want %s", string(data), expected)
	}
}

// ==================== Accounts Response Tests ====================

func TestAccountsResponse(t *testing.T) {
	resp := accountsResponse{
		Accounts: []accountResponse{
			{
				ID:     1,
				Name:   "Account 1",
				Email:  "test1@example.com",
				Status: "active",
			},
			{
				ID:     2,
				Name:   "Account 2",
				Email:  "test2@example.com",
				Status: "cooldown",
			},
		},
	}

	if len(resp.Accounts) != 2 {
		t.Errorf("Accounts count = %d, want 2", len(resp.Accounts))
	}
	if resp.Accounts[0].ID != 1 {
		t.Errorf("First account ID = %d, want 1", resp.Accounts[0].ID)
	}
}

func TestAccountResponseFields(t *testing.T) {
	resp := accountResponse{
		ID:                 1,
		Name:               "Test Account",
		Email:              "test@example.com",
		PlanType:           "plus",
		Status:             "active",
		HealthTier:         "healthy",
		SchedulerScore:     95.5,
		ConcurrencyCap:     4,
		ProxyURL:           "http://proxy:8080",
		CreatedAt:          time.Now().Format(time.RFC3339),
		UpdatedAt:          time.Now().Format(time.RFC3339),
		ActiveRequests:     2,
		TotalRequests:      100,
		LastUsedAt:         time.Now().Format(time.RFC3339),
		SuccessRequests:    95,
		ErrorRequests:      5,
		UsagePercent7d:     floatPtr(75.5),
		UsagePercent5h:     floatPtr(50.0),
		Reset5hAt:          time.Now().Format(time.RFC3339),
		Reset7dAt:          time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339),
		ScoreBreakdown:     schedulerBreakdownResponse{},
		LastUnauthorizedAt: time.Now().Format(time.RFC3339),
		LastRateLimitedAt:  time.Now().Format(time.RFC3339),
		LastTimeoutAt:      time.Now().Format(time.RFC3339),
		LastServerErrorAt:  time.Now().Format(time.RFC3339),
	}

	if resp.Name != "Test Account" {
		t.Errorf("Name = %q, want 'Test Account'", resp.Name)
	}
	if resp.SchedulerScore != 95.5 {
		t.Errorf("SchedulerScore = %v, want 95.5", resp.SchedulerScore)
	}
	if resp.UsagePercent7d == nil || *resp.UsagePercent7d != 75.5 {
		t.Error("UsagePercent7d mismatch")
	}
}

func floatPtr(f float64) *float64 {
	return &f
}

func TestSchedulerBreakdownResponse(t *testing.T) {
	resp := schedulerBreakdownResponse{
		UnauthorizedPenalty: 10.0,
		RateLimitPenalty:    5.0,
		TimeoutPenalty:      3.0,
		ServerPenalty:       2.0,
		FailurePenalty:      1.0,
		SuccessBonus:        4.0,
		UsagePenalty7d:      8.0,
		UsageUrgencyBonus5h: 6.0,
		UsageUrgencyBonus7d: 7.0,
		LatencyPenalty:      2.5,
		SuccessRatePenalty:  1.5,
	}

	if resp.UnauthorizedPenalty != 10.0 {
		t.Errorf("UnauthorizedPenalty = %v, want 10.0", resp.UnauthorizedPenalty)
	}
	if resp.SuccessBonus != 4.0 {
		t.Errorf("SuccessBonus = %v, want 4.0", resp.SuccessBonus)
	}
	if resp.UsageUrgencyBonus5h != 6.0 {
		t.Errorf("UsageUrgencyBonus5h = %v, want 6.0", resp.UsageUrgencyBonus5h)
	}
	if resp.UsageUrgencyBonus7d != 7.0 {
		t.Errorf("UsageUrgencyBonus7d = %v, want 7.0", resp.UsageUrgencyBonus7d)
	}
}

// ==================== Usage Logs Response Tests ====================

func TestUsageLogsResponse(t *testing.T) {
	resp := usageLogsResponse{
		Logs: []*database.UsageLog{
			{
				ID:                  1,
				APIKeyID:            3,
				APIKeyName:          "Team A",
				APIKeyMasked:        "sk-a****...****1111",
				ClientUserAgent:     "codex-tui/0.150.0",
				UpstreamUserAgent:   "codex-tui/0.151.0",
				UserAgentOverridden: true,
			},
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal usageLogsResponse: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("marshaled usageLogsResponse produced invalid JSON: %s", string(data))
	}

	var decoded struct {
		Logs []map[string]interface{} `json:"logs"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal usageLogsResponse JSON: %v", err)
	}
	if len(decoded.Logs) != 1 {
		t.Fatalf("len(decoded.Logs) = %d, want %d", len(decoded.Logs), 1)
	}
	if got := decoded.Logs[0]["api_key_id"]; got != float64(3) {
		t.Fatalf("api_key_id = %v, want %v", got, float64(3))
	}
	if got := decoded.Logs[0]["api_key_name"]; got != "Team A" {
		t.Fatalf("api_key_name = %v, want %q", got, "Team A")
	}
	if got := decoded.Logs[0]["api_key_masked"]; got != "sk-a****...****1111" {
		t.Fatalf("api_key_masked = %v, want %q", got, "sk-a****...****1111")
	}
	if got := decoded.Logs[0]["client_user_agent"]; got != "codex-tui/0.150.0" {
		t.Fatalf("client_user_agent = %v, want %q", got, "codex-tui/0.150.0")
	}
	if got := decoded.Logs[0]["upstream_user_agent"]; got != "codex-tui/0.151.0" {
		t.Fatalf("upstream_user_agent = %v, want %q", got, "codex-tui/0.151.0")
	}
	if got := decoded.Logs[0]["user_agent_overridden"]; got != true {
		t.Fatalf("user_agent_overridden = %v, want true", got)
	}
}

// ==================== API Keys Response Tests ====================

func TestAPIKeysResponse(t *testing.T) {
	resp := apiKeysResponse{
		Keys: nil, // Would contain actual key rows
	}

	// Verify JSON serialization includes the expected "keys" field
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal apiKeysResponse: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal apiKeysResponse JSON: %v", err)
	}

	if _, ok := decoded["keys"]; !ok {
		t.Errorf("expected JSON to contain \"keys\" field, got %v", decoded)
	}
}

// ==================== Benchmarks ====================

func BenchmarkStatsResponseMarshal(b *testing.B) {
	resp := statsResponse{
		Total:         100,
		Available:     80,
		Error:         20,
		TodayRequests: 10000,
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		json.Marshal(resp)
	}
}

func BenchmarkErrorResponseMarshal(b *testing.B) {
	resp := errorResponse{Error: "test error message"}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		json.Marshal(resp)
	}
}

func BenchmarkAccountResponseMarshal(b *testing.B) {
	resp := accountResponse{
		ID:             1,
		Name:           "Test Account",
		Email:          "test@example.com",
		PlanType:       "plus",
		Status:         "active",
		HealthTier:     "healthy",
		SchedulerScore: 95.5,
		ConcurrencyCap: 4,
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		json.Marshal(resp)
	}
}
