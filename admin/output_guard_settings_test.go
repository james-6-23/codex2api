package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func newTestAdminHandlerFixture(t *testing.T) (*Handler, *database.DB) {
	t.Helper()
	handler, db, _ := newTestAdminHandlerFixtureWithPath(t)
	return handler, db
}

func newTestAdminHandlerFixtureWithPath(t *testing.T) (*Handler, *database.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "admin-output-guard-test.sqlite")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("new test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	return handler, db, dbPath
}

// execAdminOutputGuardTestSQLite 在测试库上执行一条原始 SQL，用于模拟防护列
// 读取/写入失败（例如 schema 缺失），而不影响 system_settings 的其他列。
func execAdminOutputGuardTestSQLite(t *testing.T, dbPath, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite db: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func queryAdminOutputGuardTestSQLiteBool(t *testing.T, dbPath, query string) bool {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite db: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var value int
	if err := raw.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value != 0
}

// lockedLogBuffer 是并发安全的日志捕获器：handler 和后台 goroutine 都可能写
// 标准 logger，测试断言读取时必须加锁。
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func invokeAdminSettingsEndpoint(t *testing.T, handler *Handler, method string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var bodyReader *strings.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload failed: %v", err)
		}
		bodyReader = strings.NewReader(string(body))
	} else {
		bodyReader = strings.NewReader("")
	}
	c.Request = httptest.NewRequest(method, "/api/admin/settings", bodyReader)
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodGet {
		handler.GetSettings(c)
	} else {
		handler.UpdateSettings(c)
	}
	return w
}

func TestAdminOutputGuardSettings_GetSymmetricFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newTestAdminHandlerFixture(t)

	w := invokeAdminSettingsEndpoint(t, handler, http.MethodGet, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	fields := []string{
		"output_guard_enabled", "output_guard_max_tool_argument_bytes",
		"output_guard_max_tool_argument_deltas", "output_guard_max_channel_bytes",
		"output_guard_max_request_bytes", "output_guard_config_generation",
	}
	for _, f := range fields {
		if _, ok := resp[f]; !ok {
			t.Fatalf("expected field %q in GET /api/admin/settings response", f)
		}
	}
	if resp["output_guard_enabled"] != false {
		t.Fatalf("expected default enabled = false, got %v", resp["output_guard_enabled"])
	}
	if int64(resp["output_guard_max_tool_argument_bytes"].(float64)) != 262144 {
		t.Fatalf("expected default tool bytes 262144, got %v", resp["output_guard_max_tool_argument_bytes"])
	}
}

func TestAdminOutputGuardSettings_PutValidationAndReadonlyRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newTestAdminHandlerFixture(t)

	// 1. 提交只读 generation 必须报错 400
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_config_generation": 999})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "只读") {
			t.Fatalf("expected 400 with readonly rejection, got code=%d body=%s", w.Code, w.Body.String())
		}
	}
	// 2. 超出单个字段合法范围必须报错 400
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_max_tool_argument_bytes": 1024})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on out of range tool bytes, got %d", w.Code)
		}
	}
	// 3. 违反 tool <= channel <= request 层级校验必须报错 400 且不改写 DB
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
			"output_guard_max_tool_argument_bytes": 3145728,
			"output_guard_max_channel_bytes":       2097152,
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on hierarchy violation, got %d", w.Code)
		}
		current, err := db.GetOutputGuardSettings(context.Background())
		if err != nil || current.MaxToolArgumentBytes == 3145728 {
			t.Fatalf("database should not be updated on illegal hierarchy validation, err=%v", err)
		}
	}
	// 4. 显式 true 准确持久化与热应用
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_enabled": true})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on explicit true update, got %d, body: %s", w.Code, w.Body.String())
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["output_guard_enabled"] != true {
			t.Fatalf("expected output_guard_enabled = true in response, got %v", resp["output_guard_enabled"])
		}
		current, err := db.GetOutputGuardSettings(context.Background())
		if err != nil || current.Enabled == nil || *current.Enabled != true {
			t.Fatalf("database must store explicit true")
		}
	}
	// 5. 局部字段更新隔离：仅更新 deltas 不改变其他字段，并立即热应用
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_max_tool_argument_deltas": 8192})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on partial update, got %d", w.Code)
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if int(resp["output_guard_max_tool_argument_deltas"].(float64)) != 8192 || resp["output_guard_enabled"] != true {
			t.Fatalf("expected deltas = 8192 and preserved enabled=true, got %v", resp)
		}
		if runtimeCfg := proxy.CurrentOutputGuardConfig(); runtimeCfg.MaxToolArgumentDeltas != 8192 {
			t.Fatalf("runtime snapshot must be immediately updated, got %d", runtimeCfg.MaxToolArgumentDeltas)
		}
	}
}

// TestAdminOutputGuardSettings_NotPersistedOrAppliedWhenLaterValidationFails 覆盖
// “防护配置先于后续校验落库并热应用”的部分提交回归：同一请求里后续字段校验
// 失败时，防护配置必须既未写库，也未热应用。
func TestAdminOutputGuardSettings_NotPersistedOrAppliedWhenLaterValidationFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()

	beforeDB, err := db.GetOutputGuardSettings(ctx)
	if err != nil {
		t.Fatalf("read guard settings before request: %v", err)
	}
	beforeRuntime := proxy.CurrentOutputGuardConfig()

	// models_list_read_max_bytes 的校验发生在防护设置之后；它失败时整个保存
	// 请求返回 400，防护设置不应留下任何已提交或已生效的痕迹。
	w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"output_guard_enabled":       true,
		"models_list_read_max_bytes": 1,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid models_list_read_max_bytes, got %d, body: %s", w.Code, w.Body.String())
	}

	afterDB, err := db.GetOutputGuardSettings(ctx)
	if err != nil {
		t.Fatalf("read guard settings after request: %v", err)
	}
	if afterDB.ConfigGeneration != beforeDB.ConfigGeneration ||
		afterDB.MaxToolArgumentBytes != beforeDB.MaxToolArgumentBytes ||
		afterDB.MaxChannelBytes != beforeDB.MaxChannelBytes ||
		afterDB.MaxRequestBytes != beforeDB.MaxRequestBytes ||
		afterDB.MaxToolArgumentDeltas != beforeDB.MaxToolArgumentDeltas ||
		afterDB.Enabled == nil || beforeDB.Enabled == nil || *afterDB.Enabled != *beforeDB.Enabled {
		t.Fatalf("guard settings must not be committed when later validation fails: before=%+v after=%+v", beforeDB, afterDB)
	}

	afterRuntime := proxy.CurrentOutputGuardConfig()
	if afterRuntime.Generation != beforeRuntime.Generation ||
		afterRuntime.Enabled != beforeRuntime.Enabled ||
		afterRuntime.MaxToolArgumentBytes != beforeRuntime.MaxToolArgumentBytes {
		t.Fatalf("guard runtime config must not be applied when later validation fails: before=%+v after=%+v", beforeRuntime, afterRuntime)
	}
}

// TestAdminOutputGuardSettings_GetSurfacesReadFailure 覆盖 F1：防护设置读取失败
// 时不得用 DefaultOutputGuardSettings 静默兜底（那会让 UI 显示 enabled=false，
// 而代理可能仍在用已启用的旧配置），必须返回明确错误并记录日志。
func TestAdminOutputGuardSettings_GetSurfacesReadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, dbPath := newTestAdminHandlerFixtureWithPath(t)
	execAdminOutputGuardTestSQLite(t, dbPath,
		"ALTER TABLE system_settings DROP COLUMN output_guard_config_generation")

	var logs lockedLogBuffer
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)

	w := invokeAdminSettingsEndpoint(t, handler, http.MethodGet, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 when guard settings read fails, got %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "读取防护设置失败") {
		t.Fatalf("expected explicit guard read failure in response, got: %s", body)
	}
	if strings.Contains(body, "output_guard_enabled") {
		t.Fatalf("must not silently return default guard settings on read failure, got: %s", body)
	}
	if logText := logs.String(); !strings.Contains(logText, "读取输出防护设置失败") {
		t.Fatalf("expected guard read failure to be logged, got logs: %s", logText)
	}
}

// TestAdminOutputGuardSettings_UpdateDBErrorMapsTo500 覆盖 F2：防护设置落库失败
// （非校验错误）必须映射 500。这里删除一个防护列，使事务内“读取-合并-校验”
// 的读取步骤失败；同时验证整个事务回滚、enabled 未被写入。
func TestAdminOutputGuardSettings_UpdateDBErrorMapsTo500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, dbPath := newTestAdminHandlerFixtureWithPath(t)
	execAdminOutputGuardTestSQLite(t, dbPath,
		"ALTER TABLE system_settings DROP COLUMN output_guard_max_channel_bytes")

	w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_enabled": true})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 on database error during guard update, got %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "保存防护设置失败") {
		t.Fatalf("expected guard persistence failure in response, got: %s", w.Body.String())
	}
	if queryAdminOutputGuardTestSQLiteBool(t, dbPath, "SELECT output_guard_enabled FROM system_settings WHERE id = 1") {
		t.Fatalf("guard update must be rolled back when the transaction fails")
	}
}

// TestAdminOutputGuardSettings_MaxConcurrencyNotChangedWhenGuardSaveFails 覆盖 review 意见：
// 当 UpdateSettings 携带 max_concurrency / max_retries / max_rate_limit_retries 且 guard 保存失败（如非法参数返回 400 或 DB 失败返回 500）时，
// 运行时配置必须保持旧值，不能被污染；数据库亦保持旧值；成功路径下正常热应用并持久化。
func TestAdminOutputGuardSettings_MaxConcurrencyNotChangedWhenGuardSaveFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()

	// 准备初始状态：max_concurrency 初始设为 5，max_retries 初始设为 3，max_rate_limit_retries 设为 1
	initMaxConcurrency := 5
	initMaxRetries := 3
	initMaxRateLimitRetries := 1
	wInit := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":        initMaxConcurrency,
		"max_retries":            initMaxRetries,
		"max_rate_limit_retries": initMaxRateLimitRetries,
	})
	if wInit.Code != http.StatusOK {
		t.Fatalf("init settings failed: code=%d body=%s", wInit.Code, wInit.Body.String())
	}
	if got := handler.store.GetMaxConcurrency(); got != initMaxConcurrency {
		t.Fatalf("expected store max_concurrency=%d, got %d", initMaxConcurrency, got)
	}
	if got := handler.store.GetMaxRetries(); got != initMaxRetries {
		t.Fatalf("expected store max_retries=%d, got %d", initMaxRetries, got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != initMaxRateLimitRetries {
		t.Fatalf("expected store max_rate_limit_retries=%d, got %d", initMaxRateLimitRetries, got)
	}

	// 1. PUT max_concurrency: 99, max_retries: 7, max_rate_limit_retries: 8 + 非法 output_guard_max_channel_bytes
	// 期望返回 400，运行时保持旧值，数据库中亦保持旧值
	wErr := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":                99,
		"max_retries":                    7,
		"max_rate_limit_retries":         8,
		"output_guard_max_channel_bytes": 10, // 低于 MinOutputGuardMaxChannelBytes (128KiB)
	})
	if wErr.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 on invalid guard settings, got %d, body: %s", wErr.Code, wErr.Body.String())
	}
	if got := handler.store.GetMaxConcurrency(); got != initMaxConcurrency {
		t.Fatalf("store max_concurrency must remain %d after guard validation failure, got %d", initMaxConcurrency, got)
	}
	if got := handler.store.GetMaxRetries(); got != initMaxRetries {
		t.Fatalf("store max_retries must remain %d after guard validation failure, got %d", initMaxRetries, got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != initMaxRateLimitRetries {
		t.Fatalf("store max_rate_limit_retries must remain %d after guard validation failure, got %d", initMaxRateLimitRetries, got)
	}
	persisted, err := db.GetSystemSettings(ctx)
	if err != nil || persisted == nil {
		t.Fatalf("get persisted settings: %v", err)
	}
	if persisted.MaxConcurrency != initMaxConcurrency {
		t.Fatalf("persisted max_concurrency must remain %d, got %d", initMaxConcurrency, persisted.MaxConcurrency)
	}
	if persisted.MaxRetries != initMaxRetries {
		t.Fatalf("persisted max_retries must remain %d, got %d", initMaxRetries, persisted.MaxRetries)
	}
	if persisted.MaxRateLimitRetries != initMaxRateLimitRetries {
		t.Fatalf("persisted max_rate_limit_retries must remain %d, got %d", initMaxRateLimitRetries, persisted.MaxRateLimitRetries)
	}

	// 2. 成功路径：PUT max_concurrency: 12, max_retries: 6, max_rate_limit_retries: 4 + 合法 output_guard_max_channel_bytes
	// 期望返回 200，运行时立即更新，数据库持久化更新
	targetChannelBytes := database.MinOutputGuardMaxChannelBytes * 2
	wSuccess := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":                12,
		"max_retries":                    6,
		"max_rate_limit_retries":         4,
		"output_guard_max_channel_bytes": targetChannelBytes,
	})
	if wSuccess.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 on valid update, got %d, body: %s", wSuccess.Code, wSuccess.Body.String())
	}
	if got := handler.store.GetMaxConcurrency(); got != 12 {
		t.Fatalf("store max_concurrency must be updated to 12 on success, got %d", got)
	}
	if got := handler.store.GetMaxRetries(); got != 6 {
		t.Fatalf("store max_retries must be updated to 6 on success, got %d", got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != 4 {
		t.Fatalf("store max_rate_limit_retries must be updated to 4 on success, got %d", got)
	}
	persistedSuccess, err := db.GetSystemSettings(ctx)
	if err != nil || persistedSuccess == nil {
		t.Fatalf("get persisted settings on success: %v", err)
	}
	if persistedSuccess.MaxConcurrency != 12 {
		t.Fatalf("persisted max_concurrency must be 12 on success, got %d", persistedSuccess.MaxConcurrency)
	}
	if persistedSuccess.MaxRetries != 6 {
		t.Fatalf("persisted max_retries must be 6 on success, got %d", persistedSuccess.MaxRetries)
	}
	if persistedSuccess.MaxRateLimitRetries != 4 {
		t.Fatalf("persisted max_rate_limit_retries must be 4 on success, got %d", persistedSuccess.MaxRateLimitRetries)
	}
	// 3. 500 场景：当数据库出错导致 guard 保存失败返回 500 时，运行时与 DB 同样保持旧值
	{
		handler500, db500, dbPath := newTestAdminHandlerFixtureWithPath(t)
		wInit500 := invokeAdminSettingsEndpoint(t, handler500, http.MethodPut, map[string]any{
			"max_concurrency":        7,
			"max_retries":            2,
			"max_rate_limit_retries": 1,
		})
		if wInit500.Code != http.StatusOK {
			t.Fatalf("init settings for 500 test failed: %d", wInit500.Code)
		}
		// 破坏 output_guard_max_channel_bytes 列，使事务执行 guard 更新时报 DB 错误
		execAdminOutputGuardTestSQLite(t, dbPath,
			"ALTER TABLE system_settings DROP COLUMN output_guard_max_channel_bytes")

		wErr500 := invokeAdminSettingsEndpoint(t, handler500, http.MethodPut, map[string]any{
			"max_concurrency":        88,
			"max_retries":            9,
			"max_rate_limit_retries": 5,
			"output_guard_enabled":   true,
		})
		if wErr500.Code != http.StatusInternalServerError {
			t.Fatalf("expected HTTP 500 on db error during guard update, got %d, body: %s", wErr500.Code, wErr500.Body.String())
		}
		if got := handler500.store.GetMaxConcurrency(); got != 7 {
			t.Fatalf("store max_concurrency must remain 7 after guard db failure, got %d", got)
		}
		if got := handler500.store.GetMaxRetries(); got != 2 {
			t.Fatalf("store max_retries must remain 2 after guard db failure, got %d", got)
		}
		if got := handler500.store.GetMaxRateLimitRetries(); got != 1 {
			t.Fatalf("store max_rate_limit_retries must remain 1 after guard db failure, got %d", got)
		}
		persisted500, err := db500.GetSystemSettings(ctx)
		if err != nil || persisted500 == nil {
			t.Fatalf("get persisted settings after 500: %v", err)
		}
		if persisted500.MaxConcurrency != 7 {
			t.Fatalf("persisted max_concurrency must remain 7 after 500, got %d", persisted500.MaxConcurrency)
		}
		if persisted500.MaxRetries != 2 {
			t.Fatalf("persisted max_retries must remain 2 after 500, got %d", persisted500.MaxRetries)
		}
		if persisted500.MaxRateLimitRetries != 1 {
			t.Fatalf("persisted max_rate_limit_retries must remain 1 after 500, got %d", persisted500.MaxRateLimitRetries)
		}
	}
}

// TestAdminOutputGuardSettings_MaxConcurrencyNotChangedWhenModelsListWriteFails 覆盖 review 意见：
// 当 settings 事务已成功提交，但后续 models-list-read-max-bytes 独立写入失败（返回 500）时，
// 事务内提交的 max_concurrency / max_retries 等系统设置在事务成功即已热应用生效，
// 运行时与数据库保持一致，不应保持旧值。
func TestAdminOutputGuardSettings_MaxConcurrencyNotChangedWhenModelsListWriteFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db, dbPath := newTestAdminHandlerFixtureWithPath(t)
	ctx := context.Background()

	// 初始状态：max_concurrency 设为 5，max_retries 设为 2
	wInit := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency": 5,
		"max_retries":     2,
	})
	if wInit.Code != http.StatusOK {
		t.Fatalf("init settings failed: %d", wInit.Code)
	}

	// 在 SQLite 上安装触发器：禁止修改 models_list_read_max_bytes，模拟 UpdateModelsListReadMaxBytes 抛错返回 500
	execAdminOutputGuardTestSQLite(t, dbPath, `
		CREATE TRIGGER fail_models_list_update BEFORE UPDATE OF models_list_read_max_bytes ON system_settings
		BEGIN
			SELECT RAISE(ABORT, 'simulated models-list-read-max-bytes write failure');
		END;
	`)

	// PUT 更新 max_concurrency: 15, max_retries: 4, models_list_read_max_bytes: 16MiB (合法值但被触发器拒绝)
	wFail := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":            15,
		"max_retries":                4,
		"models_list_read_max_bytes": int64(16 << 20),
	})
	if wFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 when models_list write fails, got %d, body: %s", wFail.Code, wFail.Body.String())
	}

	// 事务提交成功即热应用：此时 max_concurrency 必须热应用为 15，max_retries 热应用为 4，与 DB 一致
	if got := handler.store.GetMaxConcurrency(); got != 15 {
		t.Fatalf("store max_concurrency should be applied on settings transaction success, expected 15, got %d", got)
	}
	if got := handler.store.GetMaxRetries(); got != 4 {
		t.Fatalf("store max_retries should be applied on settings transaction success, expected 4, got %d", got)
	}

	persisted, err := db.GetSystemSettings(ctx)
	if err != nil || persisted == nil {
		t.Fatalf("get persisted settings: %v", err)
	}
	if persisted.MaxConcurrency != 15 {
		t.Fatalf("persisted max_concurrency must be 15, got %d", persisted.MaxConcurrency)
	}
	if persisted.MaxRetries != 4 {
		t.Fatalf("persisted max_retries must be 4, got %d", persisted.MaxRetries)
	}
}

// TestAdminSettings_UpdateSystemSettingsDBErrorReturns500 覆盖 review 意见：
// 当仅修改 max_concurrency / max_retries / max_rate_limit_retries 且 UpdateSystemSettings 失败时，
// 必须返回 500 且运行时和数据库均不应用变更（不能落到成功路径返回 200）。
func TestAdminSettings_UpdateSystemSettingsDBErrorReturns500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db, dbPath := newTestAdminHandlerFixtureWithPath(t)
	ctx := context.Background()

	// 初始状态：max_concurrency 设为 3，max_retries 设为 2，max_rate_limit_retries 设为 1
	wInit := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":        3,
		"max_retries":            2,
		"max_rate_limit_retries": 1,
	})
	if wInit.Code != http.StatusOK {
		t.Fatalf("init settings failed: %d, body: %s", wInit.Code, wInit.Body.String())
	}

	// 在 SQLite 上安装触发器：禁止修改 max_concurrency，模拟 UpdateSystemSettings 抛错返回 500
	execAdminOutputGuardTestSQLite(t, dbPath, `
		CREATE TRIGGER fail_system_settings_update BEFORE UPDATE OF max_concurrency ON system_settings
		BEGIN
			SELECT RAISE(ABORT, 'simulated system_settings write failure');
		END;
	`)

	// 仅修改 max_concurrency, max_retries, max_rate_limit_retries
	wFail := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":        9,
		"max_retries":            5,
		"max_rate_limit_retries": 4,
	})
	if wFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 when UpdateSystemSettings fails, got %d, body: %s", wFail.Code, wFail.Body.String())
	}
	if !strings.Contains(wFail.Body.String(), "保存系统设置失败") {
		t.Fatalf("expected error message to contain '保存系统设置失败', got: %s", wFail.Body.String())
	}

	// 断言运行时配置保持旧值，未被热应用
	if got := handler.store.GetMaxConcurrency(); got != 3 {
		t.Fatalf("store max_concurrency must remain 3 after DB failure, got %d", got)
	}
	if got := handler.store.GetMaxRetries(); got != 2 {
		t.Fatalf("store max_retries must remain 2 after DB failure, got %d", got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != 1 {
		t.Fatalf("store max_rate_limit_retries must remain 1 after DB failure, got %d", got)
	}

	// 断言数据库保持旧值
	persisted, err := db.GetSystemSettings(ctx)
	if err != nil || persisted == nil {
		t.Fatalf("get persisted settings: %v", err)
	}
	if persisted.MaxConcurrency != 3 {
		t.Fatalf("persisted max_concurrency must remain 3, got %d", persisted.MaxConcurrency)
	}
	if persisted.MaxRetries != 2 {
		t.Fatalf("persisted max_retries must remain 2, got %d", persisted.MaxRetries)
	}
	if persisted.MaxRateLimitRetries != 1 {
		t.Fatalf("persisted max_rate_limit_retries must remain 1, got %d", persisted.MaxRateLimitRetries)
	}
}
