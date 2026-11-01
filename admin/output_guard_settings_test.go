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
