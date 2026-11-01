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
	DefaultOutputGuardMaxToolArgumentBytes  = int64(262144) // 256 KiB
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
	query := fmt.Sprintf(`SELECT output_guard_enabled,
		       COALESCE(output_guard_max_tool_argument_bytes, %d),
		       COALESCE(output_guard_max_tool_argument_deltas, %d),
		       COALESCE(output_guard_max_channel_bytes, %d),
		       COALESCE(output_guard_max_request_bytes, %d),
		       COALESCE(output_guard_config_generation, %d)
		FROM system_settings WHERE id = 1`,
		DefaultOutputGuardMaxToolArgumentBytes,
		DefaultOutputGuardMaxToolArgumentDeltas,
		DefaultOutputGuardMaxChannelBytes,
		DefaultOutputGuardMaxRequestBytes,
		DefaultOutputGuardConfigGeneration,
	)
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
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_settings (id) VALUES (1)
			ON CONFLICT (id) DO NOTHING
		`); err != nil {
			return err
		}
		var err error
		committed, err = db.updateOutputGuardSettingsTx(ctx, tx, update)
		return err
	})
	if err != nil {
		return OutputGuardSettings{}, err
	}
	return committed, nil
}

// UpdateSystemSettingsWithOutputGuard 在同一个事务内保存全局设置和输出防护设置。
// 防护列与 system_settings 的其余列同属一行；任一校验或写入失败时整体回滚，
// 避免“整体保存失败但防护配置已单独提交并热应用”的部分提交。
func (db *DB) UpdateSystemSettingsWithOutputGuard(ctx context.Context, settings *SystemSettings, update OutputGuardSettingsUpdate) (OutputGuardSettings, error) {
	var committed OutputGuardSettings
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := updateSystemSettingsWithExecer(ctx, tx, settings); err != nil {
			return err
		}
		var err error
		committed, err = db.updateOutputGuardSettingsTx(ctx, tx, update)
		return err
	})
	if err != nil {
		return OutputGuardSettings{}, err
	}
	return committed, nil
}

// updateOutputGuardSettingsTx 在调用方提供的事务内读取当前防护配置、合并更新、
// 校验并递增 generation。generation 的读取与写入同处一个事务，因此并发保存
// 不会丢更新或产生重复 generation。
func (db *DB) updateOutputGuardSettingsTx(ctx context.Context, tx *sql.Tx, update OutputGuardSettingsUpdate) (OutputGuardSettings, error) {
	current := DefaultOutputGuardSettings()
	if err := scanOutputGuardSettings(
		tx.QueryRowContext(ctx, db.outputGuardSettingsSelectQuery(!db.isSQLite())),
		&current,
	); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OutputGuardSettings{}, err
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
		return OutputGuardSettings{}, err
	}

	changed := (update.Enabled != nil && (current.Enabled == nil || *current.Enabled != *next.Enabled)) ||
		(update.MaxToolArgumentBytes != nil && current.MaxToolArgumentBytes != next.MaxToolArgumentBytes) ||
		(update.MaxToolArgumentDeltas != nil && current.MaxToolArgumentDeltas != next.MaxToolArgumentDeltas) ||
		(update.MaxChannelBytes != nil && current.MaxChannelBytes != next.MaxChannelBytes) ||
		(update.MaxRequestBytes != nil && current.MaxRequestBytes != next.MaxRequestBytes)

	if changed {
		if current.ConfigGeneration == math.MaxInt64 {
			return OutputGuardSettings{}, ErrOutputGuardGenerationOverflow
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
			return OutputGuardSettings{}, err
		}
	}

	return next, nil
}
