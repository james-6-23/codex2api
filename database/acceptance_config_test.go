// database/acceptance_config_test.go
package database

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// 场景 21: 迁移默认关闭（显式开启/关闭持久化），并发无关保存不丢字段，generation 溢出回滚
func TestAcceptanceScenario21_DBCurrencyAndRestoreDefaults(t *testing.T) {
	drivers := []string{"sqlite", "postgres"}
	pgDSN := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")

	for _, driver := range drivers {
		t.Run(driver, func(t *testing.T) {
			var db *DB
			var err error
			if driver == "sqlite" {
				db, err = New("sqlite", filepath.Join(t.TempDir(), "acceptance_outputguard.db"))
			} else {
				// PostgreSQL 真实驱动构造，无 DSN 时跳过，如实报告不宣称已测
				if pgDSN == "" {
					t.Skip("PostgreSQL DSN not configured; skipped")
				}
				db, err = New("postgres", pgDSN)
			}
			if err != nil {
				t.Fatalf("New(%s) error = %v", driver, err)
			}
			t.Cleanup(func() { _ = db.Close() })

			ctx := context.Background()

			// 1. 全新数据库默认值检查（三态指针默认关闭 false，5 项预算与 ConfigGeneration 默认值精确校验）
			settings, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("GetOutputGuardSettings error = %v", err)
			}
			if settings.Enabled == nil || *settings.Enabled != false {
				t.Fatalf("expected Enabled=false by default, got %v", settings.Enabled)
			}
			if settings.MaxToolArgumentBytes != 262144 || settings.MaxToolArgumentDeltas != 4096 ||
				settings.MaxChannelBytes != 2097152 || settings.MaxRequestBytes != 4194304 ||
				settings.ConfigGeneration != 1 {
				t.Fatalf("unexpected defaults: %+v", settings)
			}

			// 2. 显式写入 false，断言持久化且重启后不被零值覆盖；NULL fallback 保持
			falseVal := false
			updated, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled: &falseVal,
			})
			if err != nil {
				t.Fatalf("update to false error = %v", err)
			}
			if updated.Enabled == nil || *updated.Enabled != false {
				t.Fatalf("expected Enabled=false after update, got %v", updated.Enabled)
			}

			// 重启 DB 连接验证显式 false 得到正确持久化恢复
			if driver == "sqlite" {
				dbPath := filepath.Join(t.TempDir(), "acceptance_restart.db")
				initDB, ierr := New("sqlite", dbPath)
				if ierr != nil {
					t.Fatalf("New(sqlite) error = %v", ierr)
				}
				if _, uerr := initDB.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{Enabled: &falseVal}); uerr != nil {
					t.Fatalf("setup explicit false error = %v", uerr)
				}
				_ = initDB.Close()

				restartedDB, rerr := New("sqlite", dbPath)
				if rerr != nil {
					t.Fatalf("reopen error = %v", rerr)
				}
				t.Cleanup(func() { _ = restartedDB.Close() })

				readBack, gerr := restartedDB.GetOutputGuardSettings(ctx)
				if gerr != nil || readBack.Enabled == nil || *readBack.Enabled != false {
					t.Fatalf("expected persistent false after restart, got %v", readBack.Enabled)
				}
			}

			// 验证 NULL fallback: 将 output_guard_enabled 更新为 NULL，Get 必须补齐为默认 false
			if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET output_guard_enabled = NULL WHERE id = 1`); err != nil {
				t.Fatalf("seed NULL enabled failed: %v", err)
			}
			nullFallbackSettings, err := db.GetOutputGuardSettings(ctx)
			if err != nil || nullFallbackSettings.Enabled == nil || *nullFallbackSettings.Enabled != false {
				t.Fatalf("expected NULL fallback to false, got: %+v", nullFallbackSettings)
			}

			// 恢复为显式 true
			trueVal := true
			restoredTrue, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled: &trueVal,
			})
			if err != nil || restoredTrue.Enabled == nil || *restoredTrue.Enabled != true {
				t.Fatalf("restore to true error = %v", err)
			}

			// 3. 场景 21 缺口 1: 非法配置（0/负/超范围/tool>channel/channel>request）全被拒绝，且读取前后完整 settings + generation 严格不变
			beforeInvalid, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings before invalid: %v", err)
			}
			invalidCases := []struct {
				name   string
				update OutputGuardSettingsUpdate
			}{
				{"tool zero", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(0))}},
				{"tool negative", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(-1))}},
				{"tool < 16KiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(16383))}},
				{"tool > 4MiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(4194305))}},
				{"deltas zero", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(0)}},
				{"deltas negative", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(-5)}},
				{"deltas < 256", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(255)}},
				{"deltas > 65536", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(65537)}},
				{"channel zero", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(0))}},
				{"channel < 128KiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(131071))}},
				{"channel > 16MiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(16777217))}},
				{"request zero", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(0))}},
				{"request < 256KiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(262143))}},
				{"request > 64MiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(67108865))}},
				{"tool > channel", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(3000000)), MaxChannelBytes: ptr(int64(2097152))}},
				{"channel > request", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(5000000)), MaxRequestBytes: ptr(int64(4194304))}},
			}
			for _, tc := range invalidCases {
				_, uerr := db.UpdateOutputGuardSettings(ctx, tc.update)
				if !errors.Is(uerr, ErrInvalidOutputGuardSettings) {
					t.Fatalf("[%s] expected ErrInvalidOutputGuardSettings, got %v", tc.name, uerr)
				}
			}
			afterInvalid, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings after invalid: %v", err)
			}
			if !reflect.DeepEqual(beforeInvalid, afterInvalid) {
				t.Fatalf("settings changed after rejected invalid updates! before=%+v, after=%+v", beforeInvalid, afterInvalid)
			}

			// 4. 场景 21 缺口 2: 并发窄更新终值精确核验：构造两个 disjoint guard fields 并发 + response cache settings 独立组并发
			// 收集每 goroutine error 严禁忽略；核验每字段提交值与 revision 真实自增量
			genBeforeConcurrent := afterInvalid.ConfigGeneration
			const guardGoroutines = 4
			const rcGoroutines = 4
			var wg sync.WaitGroup
			guardErrCh := make(chan error, guardGoroutines)
			rcErrCh := make(chan error, rcGoroutines)

			// 分别构建两个不相交字段集合的更新：
			// G1, G2 仅修改 MaxToolArgumentBytes（保证 tool <= 2097152）
			// G3, G4 仅修改 MaxToolArgumentDeltas
			toolVals := []int64{140000, 150000}
			toolCandidates := make(map[int64]bool, len(toolVals))
			for _, v := range toolVals {
				toolCandidates[v] = true
			}

			deltaVals := []int{5000, 6000}
			deltaCandidates := make(map[int]bool, len(deltaVals))
			for _, v := range deltaVals {
				deltaCandidates[v] = true
			}

			// 启动修改 tool bytes 的并发任务
			for i := 0; i < len(toolVals); i++ {
				wg.Add(1)
				val := toolVals[i]
				go func(tb int64) {
					defer wg.Done()
					_, gerr := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
						MaxToolArgumentBytes: &tb,
					})
					if gerr != nil {
						guardErrCh <- gerr
					}
				}(val)
			}

			// 启动修改 tool deltas 的并发任务
			for i := 0; i < len(deltaVals); i++ {
				wg.Add(1)
				val := deltaVals[i]
				go func(d int) {
					defer wg.Done()
					_, gerr := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
						MaxToolArgumentDeltas: &d,
					})
					if gerr != nil {
						guardErrCh <- gerr
					}
				}(val)
			}

			// 并发更新完全独立的 response_cache_settings 组
			rcVals := []int64{10485760, 12582912, 14680064, 16777216}
			rcCandidates := make(map[int64]bool, len(rcVals))
			for _, v := range rcVals {
				rcCandidates[v] = true
			}
			for j := 0; j < len(rcVals); j++ {
				wg.Add(1)
				val := rcVals[j]
				go func(eb int64) {
					defer wg.Done()
					_, rcerr := db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{
						LocalMaxEntryBytes: &eb,
					})
					if rcerr != nil {
						rcErrCh <- rcerr
					}
				}(val)
			}

			wg.Wait()
			close(guardErrCh)
			close(rcErrCh)

			for err := range guardErrCh {
				t.Fatalf("guard concurrent update error: %v", err)
			}
			for err := range rcErrCh {
				t.Fatalf("response cache concurrent update error: %v", err)
			}

			finalSettings, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("GetOutputGuardSettings after concurrent updates: %v", err)
			}

			// 核验 1: Enabled 保持不变 (true)
			if finalSettings.Enabled == nil || *finalSettings.Enabled != true {
				t.Fatalf("expected Enabled=true preserved, got %+v", finalSettings)
			}
			// 核验 2: 并发写同一字段的终值必须落在候选值集合内
			if !toolCandidates[finalSettings.MaxToolArgumentBytes] {
				t.Fatalf("final MaxToolArgumentBytes %d not in candidate set %v", finalSettings.MaxToolArgumentBytes, toolCandidates)
			}
			if !deltaCandidates[finalSettings.MaxToolArgumentDeltas] {
				t.Fatalf("final MaxToolArgumentDeltas %d not in candidate set %v", finalSettings.MaxToolArgumentDeltas, deltaCandidates)
			}
			// 核验 3: 未被并发修改的独立字段保持原有值
			if finalSettings.MaxChannelBytes != afterInvalid.MaxChannelBytes || finalSettings.MaxRequestBytes != afterInvalid.MaxRequestBytes {
				t.Fatalf("independent fields corrupted! channel=%d request=%d", finalSettings.MaxChannelBytes, finalSettings.MaxRequestBytes)
			}
			// 核验 4: revision (ConfigGeneration) 真实自增量：每个 guard update 均导致实际值变化，因此 revision 自增量精确等于 guardGoroutines
			if finalSettings.ConfigGeneration != genBeforeConcurrent+int64(guardGoroutines) {
				t.Fatalf("expected generation increment of exactly %d (from %d to %d), got %d",
					guardGoroutines, genBeforeConcurrent, genBeforeConcurrent+int64(guardGoroutines), finalSettings.ConfigGeneration)
			}

			// 核验 5: 独立 response_cache_settings 并发写终值核验
			finalRCSettings, err := db.GetResponseCacheSettings(ctx)
			if err != nil {
				t.Fatalf("GetResponseCacheSettings after concurrent updates: %v", err)
			}
			if !rcCandidates[finalRCSettings.LocalMaxEntryBytes] {
				t.Fatalf("final ResponseCache LocalMaxEntryBytes %d not in candidate set %v", finalRCSettings.LocalMaxEntryBytes, rcCandidates)
			}

			// 5. 场景 21 缺口 3: ConfigGeneration MaxInt64 overflow 回滚后再次 Get 与 before 严格 DeepEqual，事务正常退出无死锁
			if _, err := db.conn.ExecContext(ctx, `
				UPDATE system_settings SET output_guard_config_generation = $1 WHERE id = 1
			`, int64(math.MaxInt64)); err != nil {
				t.Fatalf("seed max generation: %v", err)
			}
			beforeOverflow, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings before overflow: %v", err)
			}
			attemptToolBytes := int64(262144)
			_, err = db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{MaxToolArgumentBytes: &attemptToolBytes})
			if !errors.Is(err, ErrOutputGuardGenerationOverflow) {
				t.Fatalf("expected ErrOutputGuardGenerationOverflow, got %v", err)
			}
			afterOverflow, err := db.GetOutputGuardSettings(ctx)
			if err != nil {
				t.Fatalf("get settings after overflow (deadlock/tx check): %v", err)
			}
			if !reflect.DeepEqual(beforeOverflow, afterOverflow) {
				t.Fatalf("settings mutated after overflow error! before=%+v, after=%+v", beforeOverflow, afterOverflow)
			}

			// 6. 恢复全部 5 项默认字段并核验
			// 将 generation 重置为有效值 100
			if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET output_guard_config_generation = 100 WHERE id = 1`); err != nil {
				t.Fatalf("reset generation failed: %v", err)
			}
			def := DefaultOutputGuardSettings()
			restoredAll, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
				Enabled:               def.Enabled,
				MaxToolArgumentBytes:  &def.MaxToolArgumentBytes,
				MaxToolArgumentDeltas: &def.MaxToolArgumentDeltas,
				MaxChannelBytes:       &def.MaxChannelBytes,
				MaxRequestBytes:       &def.MaxRequestBytes,
			})
			if err != nil {
				t.Fatalf("restore all defaults failed: %v", err)
			}
			if restoredAll.Enabled == nil || *restoredAll.Enabled != false ||
				restoredAll.MaxToolArgumentBytes != 262144 || restoredAll.MaxToolArgumentDeltas != 4096 ||
				restoredAll.MaxChannelBytes != 2097152 || restoredAll.MaxRequestBytes != 4194304 {
				t.Fatalf("restored defaults mismatch: %+v", restoredAll)
			}
			if restoredAll.ConfigGeneration != 101 {
				t.Fatalf("restored generation want 101, got %d", restoredAll.ConfigGeneration)
			}
		})
	}
}
