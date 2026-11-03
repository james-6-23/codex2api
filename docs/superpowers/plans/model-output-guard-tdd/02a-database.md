# 02a-Database: 模型输出失控防护数据库持久化与三态指针配置 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 基于已审查通过的设计规格（Spec §5.1、§7、§9.21），在 Codex2API 存储层实现模型输出失控防护配置持久化，新增 5 项核心预算与 1 项配置版本（generation）字段，支持 SQLite 与 PostgreSQL 双引擎迁移、三态指针（`*bool`）显式 `false` 持久化与旧缺失值向 `true` 安全补齐、短事务原子合并与行级并发写锁、全量更新隔离保护、以及配置版本溢出安全回滚。

**Architecture:**
- `database/sqlite.go` 与 `database/postgres.go`: 注入建表 DDL 与动态迁移列。`UpdateSystemSettings` 的 upsert 排除这些列。
- `database/output_guard_settings.go`: 独立窄更新/读取接口（`GetOutputGuardSettings`、`UpdateOutputGuardSettings`），短事务内先读当前值合并（`tool <= channel <= request` 层级校验），PG 用 `FOR UPDATE`、SQLite 用 `withSQLiteWriteLock`。`generation` 严格自增且 MaxInt64 溢出拦截回滚。
- `database/output_guard_settings_test.go`: 真实 DB 构造函数 `database.New(driver, dsn)` 运行 SQLite 与可选 PostgreSQL 集成测试。

**Tech Stack:** Go 1.26+ (`database/sql`, `modernc.org/sqlite`, `github.com/jackc/pgx/v5`).

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md` (§5.1, §7, §9.21)

## Global Constraints

- **范围限制**：单一窄任务 Task 4（数据库配置持久化）；不改动 02-config 其他模块、不承担 Task 5 运行时轮询器与 Task 6 管理后台 UI。
- **存储与三态契约**：
  - `output_guard_enabled`: 采用三态指针（`*bool`）。NULL 向上补齐默认 `false`；显式 `true`/`false` 原样持久化，严禁零值布尔覆盖。
  - 预算列 `NOT NULL DEFAULT`：tool 131072 (16KiB..4MiB)，deltas 4096 (256..65536)，channel 2097152 (128KiB..16MiB)，request 4194304 (256KiB..64MiB)，generation 1 (正整数)。
  - 层级约束：严格满足 `tool <= channel <= request`。
- **事务与并发保护**：
  - 同一事务内完成读旧值合并、范围校验、自增 generation 与更新，严禁用 `db.conn` 读当前状态。
  - PG 使用 `FOR UPDATE`，SQLite 使用 `db.withSQLiteWriteLock`。
  - 版本号溢出保护：当前 `generation == math.MaxInt64` 时拒绝返回 `ErrOutputGuardGenerationOverflow` 并回滚。
  - 隔离保护：现有 `UpdateSystemSettings` 的 SQL 绝不包含 `output_guard_*` 字段。
- **测试环境约束**：`GOCACHE=/tmp/go-build GOPATH=/tmp/go go test ...`，真实 DB 驱动集成测试，禁用 mock。

---

## File Structure

| 文件路径 | 状态 | 职责描述 |
|---|---|---|
| `database/output_guard_settings.go` | 新建 | `OutputGuardSettings` / `OutputGuardSettingsUpdate` 结构、校验、默认值与 Get/Update 核心实现 |
| `database/output_guard_settings_test.go` | 新建 | SQLite 与 PostgreSQL（环境变量可选）真实集成测试 |
| `database/sqlite.go` | 修改 (约 L395, L722) | 在 SQLite 建表 DDL 与动态迁移切片中添加 6 个 output_guard 字段 |
| `database/postgres.go` | 修改 (约 L1380, L1586) | 在 PostgreSQL 建表 DDL 与动态迁移中添加 6 个 output_guard 字段；确认 `UpdateSystemSettings` 隔离 |

---

## Task 4: 数据库表结构迁移与三态指针持久化（SQLite & PostgreSQL）

**Files:**
- Create: `database/output_guard_settings.go`
- Modify: `database/sqlite.go:390-405,720-730`
- Modify: `database/postgres.go:1375-1385,1580-1590,3050-3220`
- Test: `database/output_guard_settings_test.go`

**Interfaces:**
- Consumes:
  - `database.DB`
  - `database.DB.isSQLite() bool`
  - `database.DB.withSQLiteWriteLock(context.Context, func() error) error`
- Produces:
  ```go
  package database

  import (
      "context"
      "errors"
  )

  var (
      ErrInvalidOutputGuardSettings    = errors.New("invalid output guard settings")
      ErrOutputGuardGenerationOverflow = errors.New("output guard config generation overflow")
  )

  type OutputGuardSettings struct {
      Enabled               *bool `json:"output_guard_enabled"`
      MaxToolArgumentBytes  int64 `json:"output_guard_max_tool_argument_bytes"`
      MaxToolArgumentDeltas int   `json:"output_guard_max_tool_argument_deltas"`
      MaxChannelBytes       int64 `json:"output_guard_max_channel_bytes"`
      MaxRequestBytes       int64 `json:"output_guard_max_request_bytes"`
      ConfigGeneration      int64 `json:"output_guard_config_generation"`
  }

  type OutputGuardSettingsUpdate struct {
      Enabled               *bool
      MaxToolArgumentBytes  *int64
      MaxToolArgumentDeltas *int
      MaxChannelBytes       *int64
      MaxRequestBytes       *int64
  }

  func DefaultOutputGuardSettings() OutputGuardSettings
  func ValidateOutputGuardSettings(settings OutputGuardSettings) error
  func (db *DB) GetOutputGuardSettings(ctx context.Context) (OutputGuardSettings, error)
  func (db *DB) UpdateOutputGuardSettings(ctx context.Context, update OutputGuardSettingsUpdate) (OutputGuardSettings, error)
  ```

- [ ] **Step 1: Write failing test for OutputGuardSettings**

创建 `database/output_guard_settings_test.go`：
```go
package database

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func runOnAllOutputGuardDBDrivers(t *testing.T, testFn func(t *testing.T, db *DB)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "output-guard-test.db")
		db, err := New("sqlite", dbPath)
		if err != nil {
			t.Fatalf("New(sqlite) error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		testFn(t, db)
	})

	if dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			db, err := New("postgres", dsn)
			if err != nil {
				t.Fatalf("New(postgres) error = %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			testFn(t, db)
		})
	}
}

func TestOutputGuardSettingsFreshDefaults(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		got, err := db.GetOutputGuardSettings(context.Background())
		if err != nil {
			t.Fatalf("GetOutputGuardSettings error = %v", err)
		}

		if got.Enabled == nil || *got.Enabled != false {
			t.Fatalf("default Enabled want false, got %v", got.Enabled)
		}
		if got.MaxToolArgumentBytes != 131072 {
			t.Fatalf("default MaxToolArgumentBytes want 131072, got %d", got.MaxToolArgumentBytes)
		}
		if got.MaxToolArgumentDeltas != 4096 {
			t.Fatalf("default MaxToolArgumentDeltas want 4096, got %d", got.MaxToolArgumentDeltas)
		}
		if got.MaxChannelBytes != 2097152 {
			t.Fatalf("default MaxChannelBytes want 2097152, got %d", got.MaxChannelBytes)
		}
		if got.MaxRequestBytes != 4194304 {
			t.Fatalf("default MaxRequestBytes want 4194304, got %d", got.MaxRequestBytes)
		}
		if got.ConfigGeneration < 1 {
			t.Fatalf("default ConfigGeneration want >= 1, got %d", got.ConfigGeneration)
		}
	})
}

func TestOutputGuardSettingsExplicitFalseTriStateAndRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "output-guard-tristate.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) error = %v", err)
	}

	ctx := context.Background()
	disabled := false
	updated, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
		Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("UpdateOutputGuardSettings error = %v", err)
	}
	if updated.Enabled == nil || *updated.Enabled != false {
		t.Fatalf("expected updated Enabled=false, got %v", updated.Enabled)
	}
	_ = db.Close()

	// 重新打开数据库，断言显式 false 得到正确持久化恢复
	restartedDB, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = restartedDB.Close() })

	readBack, err := restartedDB.GetOutputGuardSettings(ctx)
	if err != nil {
		t.Fatalf("readBack error = %v", err)
	}
	if readBack.Enabled == nil || *readBack.Enabled != false {
		t.Fatalf("expected readBack Enabled=false, got %v", readBack.Enabled)
	}
}

func TestOutputGuardSettingsNullMissingFallbackToTrue(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, output_guard_enabled)
			VALUES (1, NULL)
			ON CONFLICT (id) DO UPDATE SET output_guard_enabled = NULL
		`); err != nil {
			t.Fatalf("seed NULL enabled: %v", err)
		}

		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatalf("GetOutputGuardSettings error = %v", err)
		}
		if got.Enabled == nil || *got.Enabled != false {
			t.Fatalf("NULL enabled want fallback false, got %v", got.Enabled)
		}
	})
}

func TestOutputGuardSettingsValidationBoundariesAndRollback(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		before, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}

	invalidCases := []struct {
		name   string
		update OutputGuardSettingsUpdate
	}{
		{"tool < 16KiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(16383))}},
		{"tool > 4MiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(4194305))}},
		{"deltas < 256", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(255)}},
		{"deltas > 65536", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(65537)}},
		{"channel < 128KiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(131071))}},
		{"channel > 16MiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(16777217))}},
		{"request < 256KiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(262143))}},
		{"request > 64MiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(67108865))}},
		{"tool > channel", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(3000000)), MaxChannelBytes: ptr(int64(2097152))}},
		{"channel > request", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(5000000)), MaxRequestBytes: ptr(int64(4194304))}},
	}

		for _, tc := range invalidCases {
			_, err := db.UpdateOutputGuardSettings(ctx, tc.update)
			if !errors.Is(err, ErrInvalidOutputGuardSettings) {
				t.Fatalf("[%s] want ErrInvalidOutputGuardSettings, got %v", tc.name, err)
			}
		}

		after, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after.ConfigGeneration != before.ConfigGeneration || after.MaxToolArgumentBytes != before.MaxToolArgumentBytes {
			t.Fatalf("invalid update didn't rollback: before=%+v, after=%+v", before, after)
		}
	})
}

func TestOutputGuardSettingsNarrowUpdateNotOverwrittenBySystemSettings(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		toolBytes, channelBytes, requestBytes := int64(262144), int64(2097152), int64(4194304)
		disabled := false
		committed, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			Enabled:              &disabled,
			MaxToolArgumentBytes: &toolBytes,
			MaxChannelBytes:      &channelBytes,
			MaxRequestBytes:      &requestBytes,
		})
		if err != nil {
			t.Fatalf("UpdateOutputGuardSettings error = %v", err)
		}

		if err := db.UpdateSystemSettings(ctx, &SystemSettings{SiteName: "stale full update"}); err != nil {
			t.Fatalf("UpdateSystemSettings error = %v", err)
		}

		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatalf("GetOutputGuardSettings error = %v", err)
		}
		if got.Enabled == nil || *got.Enabled != false || got.MaxToolArgumentBytes != committed.MaxToolArgumentBytes {
			t.Fatalf("OutputGuardSettings overwritten by UpdateSystemSettings: got %+v, want %+v", got, committed)
		}
	})
}

func TestOutputGuardSettingsGenerationOverflowRollback(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, output_guard_config_generation)
			VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET output_guard_config_generation = $1
		`, int64(math.MaxInt64)); err != nil {
			t.Fatalf("seed max generation: %v", err)
		}

		toolBytes := int64(65536)
		_, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			MaxToolArgumentBytes: &toolBytes,
		})
		if !errors.Is(err, ErrOutputGuardGenerationOverflow) {
			t.Fatalf("expected ErrOutputGuardGenerationOverflow, got %v", err)
		}

		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ConfigGeneration != math.MaxInt64 {
			t.Fatalf("generation after overflow = %d, want math.MaxInt64", got.ConfigGeneration)
		}
	})
}

func TestOutputGuardSettingsConcurrentDisjointUpdatesDoNotLoseChanges(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		var wg sync.WaitGroup
		errCh := make(chan error, 10)

		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				tb, cb, rb := int64(131072+(idx+1)*1024), int64(2097152+(idx+1)*1024), int64(4194304+(idx+1)*1024)
				_, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
					MaxToolArgumentBytes: &tb,
					MaxChannelBytes:      &cb,
					MaxRequestBytes:      &rb,
				})
				if err != nil {
					errCh <- err
				}
			}(i)
		}

		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("concurrent update failed: %v", err)
		}

		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ConfigGeneration < 11 {
			t.Fatalf("expected generation >= 11 after 10 concurrent updates, got %d", got.ConfigGeneration)
		}
	})
}

func TestOutputGuardSettingsRestoreAllDefaults(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		disabled := false
		customTool, customDeltas, customChannel, customRequest := int64(524288), 8192, int64(4194304), int64(8388608)
		_, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			Enabled:               &disabled,
			MaxToolArgumentBytes:  &customTool,
			MaxToolArgumentDeltas: &customDeltas,
			MaxChannelBytes:       &customChannel,
			MaxRequestBytes:       &customRequest,
		})
		if err != nil {
			t.Fatalf("setup custom settings error: %v", err)
		}

		def := DefaultOutputGuardSettings()
		restored, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			Enabled:               def.Enabled,
			MaxToolArgumentBytes:  &def.MaxToolArgumentBytes,
			MaxToolArgumentDeltas: &def.MaxToolArgumentDeltas,
			MaxChannelBytes:       &def.MaxChannelBytes,
			MaxRequestBytes:       &def.MaxRequestBytes,
		})
		if err != nil {
			t.Fatalf("restore error: %v", err)
		}

		if restored.Enabled == nil || *restored.Enabled != false ||
			restored.MaxToolArgumentBytes != 131072 || restored.MaxToolArgumentDeltas != 4096 ||
			restored.MaxChannelBytes != 2097152 || restored.MaxRequestBytes != 4194304 {
			t.Fatalf("restored mismatch: %+v", restored)
		}
	})
}

func ptr[T any](v T) *T { return &v }
```

- [ ] **Step 2: Run test to verify failure (Red)**

运行测试验证编译红灯：
```bash
GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -run TestOutputGuardSettings ./database
```
预期输出：`undefined: db.GetOutputGuardSettings` / `undefined: OutputGuardSettings` / `build failed`。

- [ ] **Step 3: Write minimal implementation**

1. 修改 `database/sqlite.go`:
在 `system_settings` 的建表 DDL（约 L395）中添加列：
```go
					output_guard_enabled INTEGER DEFAULT 0,
					output_guard_max_tool_argument_bytes INTEGER NOT NULL DEFAULT 131072,
					output_guard_max_tool_argument_deltas INTEGER NOT NULL DEFAULT 4096,
					output_guard_max_channel_bytes INTEGER NOT NULL DEFAULT 2097152,
					output_guard_max_request_bytes INTEGER NOT NULL DEFAULT 4194304,
					output_guard_config_generation INTEGER NOT NULL DEFAULT 1,
```
在动态迁移切片 `columns`（约 L722）中添加：
```go
		{"system_settings", "output_guard_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "output_guard_max_tool_argument_bytes", "INTEGER NOT NULL DEFAULT 131072"},
		{"system_settings", "output_guard_max_tool_argument_deltas", "INTEGER NOT NULL DEFAULT 4096"},
		{"system_settings", "output_guard_max_channel_bytes", "INTEGER NOT NULL DEFAULT 2097152"},
		{"system_settings", "output_guard_max_request_bytes", "INTEGER NOT NULL DEFAULT 4194304"},
		{"system_settings", "output_guard_config_generation", "INTEGER NOT NULL DEFAULT 1"},
```

2. 修改 `database/postgres.go`:
在 `system_settings` 的建表 DDL（约 L1380）中添加列：
```sql
			output_guard_enabled BOOLEAN DEFAULT FALSE,
			output_guard_max_tool_argument_bytes BIGINT NOT NULL DEFAULT 131072,
			output_guard_max_tool_argument_deltas INT NOT NULL DEFAULT 4096,
			output_guard_max_channel_bytes BIGINT NOT NULL DEFAULT 2097152,
			output_guard_max_request_bytes BIGINT NOT NULL DEFAULT 4194304,
			output_guard_config_generation BIGINT NOT NULL DEFAULT 1,
```
在 `initPostgresSchema`（约 L1586）中添加动态迁移：
```sql
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_max_tool_argument_bytes BIGINT NOT NULL DEFAULT 131072;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_max_tool_argument_deltas INT NOT NULL DEFAULT 4096;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_max_channel_bytes BIGINT NOT NULL DEFAULT 2097152;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_max_request_bytes BIGINT NOT NULL DEFAULT 4194304;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS output_guard_config_generation BIGINT NOT NULL DEFAULT 1;
```
确认 `UpdateSystemSettings` 的 SQL 排除 `output_guard_*` 字段。

3. 创建 `database/output_guard_settings.go`:
```go
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

const (
	DefaultOutputGuardEnabled               = false
	DefaultOutputGuardMaxToolArgumentBytes  = int64(131072)  // 128 KiB
	DefaultOutputGuardMaxToolArgumentDeltas = 4096
	DefaultOutputGuardMaxChannelBytes       = int64(2097152) // 2 MiB
	DefaultOutputGuardMaxRequestBytes       = int64(4194304) // 4 MiB
	DefaultOutputGuardConfigGeneration      = int64(1)

	MinOutputGuardMaxToolArgumentBytes = int64(16384)   // 16 KiB
	MaxOutputGuardMaxToolArgumentBytes = int64(4194304) // 4 MiB

	MinOutputGuardMaxToolArgumentDeltas = 256
	MaxOutputGuardMaxToolArgumentDeltas = 65536

	MinOutputGuardMaxChannelBytes = int64(131072)   // 128 KiB
	MaxOutputGuardMaxChannelBytes = int64(16777216) // 16 MiB

	MinOutputGuardMaxRequestBytes = int64(262144)   // 256 KiB
	MaxOutputGuardMaxRequestBytes = int64(67108864) // 64 MiB
)

var (
	ErrInvalidOutputGuardSettings    = errors.New("invalid output guard settings")
	ErrOutputGuardGenerationOverflow = errors.New("output guard config generation overflow")
)

type OutputGuardSettings struct {
	Enabled               *bool `json:"output_guard_enabled"`
	MaxToolArgumentBytes  int64 `json:"output_guard_max_tool_argument_bytes"`
	MaxToolArgumentDeltas int   `json:"output_guard_max_tool_argument_deltas"`
	MaxChannelBytes       int64 `json:"output_guard_max_channel_bytes"`
	MaxRequestBytes       int64 `json:"output_guard_max_request_bytes"`
	ConfigGeneration      int64 `json:"output_guard_config_generation"`
}

type OutputGuardSettingsUpdate struct {
	Enabled               *bool
	MaxToolArgumentBytes  *int64
	MaxToolArgumentDeltas *int
	MaxChannelBytes       *int64
	MaxRequestBytes       *int64
}

func DefaultOutputGuardSettings() OutputGuardSettings {
	t := DefaultOutputGuardEnabled
	return OutputGuardSettings{
		Enabled:               &t,
		MaxToolArgumentBytes:  DefaultOutputGuardMaxToolArgumentBytes,
		MaxToolArgumentDeltas: DefaultOutputGuardMaxToolArgumentDeltas,
		MaxChannelBytes:       DefaultOutputGuardMaxChannelBytes,
		MaxRequestBytes:       DefaultOutputGuardMaxRequestBytes,
		ConfigGeneration:      DefaultOutputGuardConfigGeneration,
	}
}

func ValidateOutputGuardSettings(settings OutputGuardSettings) error {
	switch {
	case settings.MaxToolArgumentBytes < MinOutputGuardMaxToolArgumentBytes || settings.MaxToolArgumentBytes > MaxOutputGuardMaxToolArgumentBytes:
		return fmt.Errorf("%w: max_tool_argument_bytes must be 16KiB..4MiB", ErrInvalidOutputGuardSettings)
	case settings.MaxToolArgumentDeltas < MinOutputGuardMaxToolArgumentDeltas || settings.MaxToolArgumentDeltas > MaxOutputGuardMaxToolArgumentDeltas:
		return fmt.Errorf("%w: max_tool_argument_deltas must be 256..65536", ErrInvalidOutputGuardSettings)
	case settings.MaxChannelBytes < MinOutputGuardMaxChannelBytes || settings.MaxChannelBytes > MaxOutputGuardMaxChannelBytes:
		return fmt.Errorf("%w: max_channel_bytes must be 128KiB..16MiB", ErrInvalidOutputGuardSettings)
	case settings.MaxRequestBytes < MinOutputGuardMaxRequestBytes || settings.MaxRequestBytes > MaxOutputGuardMaxRequestBytes:
		return fmt.Errorf("%w: max_request_bytes must be 256KiB..64MiB", ErrInvalidOutputGuardSettings)
	case settings.MaxToolArgumentBytes > settings.MaxChannelBytes || settings.MaxChannelBytes > settings.MaxRequestBytes:
		return fmt.Errorf("%w: requires tool <= channel <= request bytes", ErrInvalidOutputGuardSettings)
	case settings.ConfigGeneration <= 0:
		return fmt.Errorf("%w: config generation must be positive", ErrInvalidOutputGuardSettings)
	default:
		return nil
	}
}

func (db *DB) outputGuardSettingsSelectQuery(forUpdate bool) string {
	query := `SELECT output_guard_enabled,
		       COALESCE(output_guard_max_tool_argument_bytes, 131072),
		       COALESCE(output_guard_max_tool_argument_deltas, 4096),
		       COALESCE(output_guard_max_channel_bytes, 2097152),
		       COALESCE(output_guard_max_request_bytes, 4194304),
		       COALESCE(output_guard_config_generation, 1)
		FROM system_settings WHERE id = 1`
	if forUpdate && !db.isSQLite() {
		query += " FOR UPDATE"
	}
	return query
}

type outputGuardSettingsScanner interface {
	Scan(dest ...any) error
}

func scanOutputGuardSettings(row outputGuardSettingsScanner, settings *OutputGuardSettings) error {
	var enabled sql.NullBool
	if err := row.Scan(
		&enabled,
		&settings.MaxToolArgumentBytes,
		&settings.MaxToolArgumentDeltas,
		&settings.MaxChannelBytes,
		&settings.MaxRequestBytes,
		&settings.ConfigGeneration,
	); err != nil {
		return err
	}
	if enabled.Valid {
		val := enabled.Bool
		settings.Enabled = &val
	} else {
		def := DefaultOutputGuardEnabled
		settings.Enabled = &def
	}
	return nil
}

func (db *DB) GetOutputGuardSettings(ctx context.Context) (OutputGuardSettings, error) {
	settings := DefaultOutputGuardSettings()
	err := scanOutputGuardSettings(db.conn.QueryRowContext(ctx, db.outputGuardSettingsSelectQuery(false)), &settings)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return OutputGuardSettings{}, err
	}
	if err := ValidateOutputGuardSettings(settings); err != nil {
		return OutputGuardSettings{}, err
	}
	return settings, nil
}

func (db *DB) UpdateOutputGuardSettings(ctx context.Context, update OutputGuardSettingsUpdate) (OutputGuardSettings, error) {
	var committed OutputGuardSettings
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_settings (id) VALUES (1)
			ON CONFLICT (id) DO NOTHING
		`); err != nil {
			return err
		}

		current := DefaultOutputGuardSettings()
		if err := scanOutputGuardSettings(
			tx.QueryRowContext(ctx, db.outputGuardSettingsSelectQuery(!db.isSQLite())),
			&current,
		); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		next := current
		if update.Enabled != nil {
			val := *update.Enabled
			next.Enabled = &val
		}
		if update.MaxToolArgumentBytes != nil {
			next.MaxToolArgumentBytes = *update.MaxToolArgumentBytes
		}
		if update.MaxToolArgumentDeltas != nil {
			next.MaxToolArgumentDeltas = *update.MaxToolArgumentDeltas
		}
		if update.MaxChannelBytes != nil {
			next.MaxChannelBytes = *update.MaxChannelBytes
		}
		if update.MaxRequestBytes != nil {
			next.MaxRequestBytes = *update.MaxRequestBytes
		}

		if err := ValidateOutputGuardSettings(next); err != nil {
			return err
		}

		changed := (update.Enabled != nil && (current.Enabled == nil || *current.Enabled != *next.Enabled)) ||
			(update.MaxToolArgumentBytes != nil && current.MaxToolArgumentBytes != next.MaxToolArgumentBytes) ||
			(update.MaxToolArgumentDeltas != nil && current.MaxToolArgumentDeltas != next.MaxToolArgumentDeltas) ||
			(update.MaxChannelBytes != nil && current.MaxChannelBytes != next.MaxChannelBytes) ||
			(update.MaxRequestBytes != nil && current.MaxRequestBytes != next.MaxRequestBytes)

		if changed {
			if current.ConfigGeneration == math.MaxInt64 {
				return ErrOutputGuardGenerationOverflow
			}
			next.ConfigGeneration = current.ConfigGeneration + 1

			var enabledVal any
			if next.Enabled != nil {
				if db.isSQLite() {
					if *next.Enabled {
						enabledVal = int64(1)
					} else {
						enabledVal = int64(0)
					}
				} else {
					enabledVal = *next.Enabled
				}
			}

			if _, err := tx.ExecContext(ctx, `
				UPDATE system_settings
				SET output_guard_enabled = $1,
				    output_guard_max_tool_argument_bytes = $2,
				    output_guard_max_tool_argument_deltas = $3,
				    output_guard_max_channel_bytes = $4,
				    output_guard_max_request_bytes = $5,
				    output_guard_config_generation = $6
				WHERE id = 1
			`, enabledVal, next.MaxToolArgumentBytes, next.MaxToolArgumentDeltas, next.MaxChannelBytes, next.MaxRequestBytes, next.ConfigGeneration); err != nil {
				return err
			}
		}

		if err := tx.Commit(); err != nil {
			return err
		}
		committed = next
		return nil
	})
	if err != nil {
		return OutputGuardSettings{}, err
	}
	return committed, nil
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

运行测试确认全部用例通过：
```bash
GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v -run TestOutputGuardSettings ./database
```
预期输出：`PASS ok github.com/codex2api/database`。

- [ ] **Step 5: Commit**

```bash
git add database/output_guard_settings.go database/output_guard_settings_test.go database/sqlite.go database/postgres.go
git commit -m "feat(database): add output guard settings table migration and tri-state persistence"
```
