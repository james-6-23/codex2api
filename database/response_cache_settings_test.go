package database

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testResponseCacheDefaultTotal       = int64(67_108_864)
	testResponseCacheDefaultEntry       = int64(8_388_608)
	testResponseCacheDefaultReconstruct = int64(67_108_864)
)

func TestResponseCacheSettingsFreshSQLiteDefaults(t *testing.T) {
	db := newResponseCacheSettingsTestDB(t)
	got, err := db.GetResponseCacheSettings(context.Background())
	if err != nil {
		t.Fatalf("GetResponseCacheSettings() error = %v", err)
	}
	assertResponseCacheSettings(t, got, ResponseCacheSettings{
		LocalMaxBytes:       testResponseCacheDefaultTotal,
		LocalMaxEntryBytes:  testResponseCacheDefaultEntry,
		ReconstructMaxBytes: testResponseCacheDefaultReconstruct,
		WritePolicy:         DefaultResponseCacheWritePolicy,
		Generation:          1,
	})
	var rows int
	if err := db.conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM system_settings`).Scan(&rows); err != nil {
		t.Fatalf("count fresh system_settings rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("GetResponseCacheSettings created %d system_settings rows, want 0", rows)
	}

	for name, wantDefault := range map[string]string{
		"response_cache_local_max_bytes":       "67108864",
		"response_cache_local_max_entry_bytes": "8388608",
		"response_cache_reconstruct_max_bytes": "67108864",
		"response_cache_config_generation":     "1",
	} {
		var columnType, defaultValue string
		err := db.conn.QueryRowContext(
			context.Background(),
			`SELECT type, CAST(dflt_value AS TEXT) FROM pragma_table_info('system_settings') WHERE name = $1`,
			name,
		).Scan(&columnType, &defaultValue)
		if err != nil {
			t.Fatalf("read fresh column %s: %v", name, err)
		}
		if strings.ToUpper(columnType) != "INTEGER" || strings.Trim(defaultValue, "'\"()") != wantDefault {
			t.Fatalf("column %s type/default = %s/%s, want INTEGER/%s", name, columnType, defaultValue, wantDefault)
		}
	}
}

func TestResponseCacheSettingsPostgresReadForUpdateLocksSingletonRow(t *testing.T) {
	postgresQuery := responseCacheSettingsSelectQuery(true)
	if !strings.Contains(strings.ToUpper(postgresQuery), "FOR UPDATE") {
		t.Fatalf("PostgreSQL transactional read lacks FOR UPDATE: %s", postgresQuery)
	}
	sqliteQuery := responseCacheSettingsSelectQuery(false)
	if strings.Contains(strings.ToUpper(sqliteQuery), "FOR UPDATE") {
		t.Fatalf("SQLite read must not contain FOR UPDATE: %s", sqliteQuery)
	}
}

func TestSQLiteResponseCacheSettingsMigrationFromLegacySchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-response-cache.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) setup error = %v", err)
	}
	if _, err := db.conn.ExecContext(context.Background(), `INSERT INTO system_settings (id) VALUES (1)`); err != nil {
		_ = db.Close()
		t.Fatalf("insert legacy settings row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close setup database: %v", err)
	}

	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy sqlite: %v", err)
	}
	for _, column := range []string{
		"response_cache_local_max_bytes",
		"response_cache_local_max_entry_bytes",
		"response_cache_reconstruct_max_bytes",
		"response_cache_config_generation",
	} {
		if _, err := legacy.Exec(`ALTER TABLE system_settings DROP COLUMN ` + column); err != nil {
			_ = legacy.Close()
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy sqlite: %v", err)
	}

	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite legacy) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	got, err := db.GetResponseCacheSettings(context.Background())
	if err != nil {
		t.Fatalf("GetResponseCacheSettings() after migration error = %v", err)
	}
	assertResponseCacheSettings(t, got, ResponseCacheSettings{
		LocalMaxBytes:       testResponseCacheDefaultTotal,
		LocalMaxEntryBytes:  testResponseCacheDefaultEntry,
		ReconstructMaxBytes: testResponseCacheDefaultReconstruct,
		WritePolicy:         DefaultResponseCacheWritePolicy,
		Generation:          1,
	})
}

func TestResponseCacheSettingsRoundTripPartialAndGeneration(t *testing.T) {
	db := newResponseCacheSettingsTestDB(t)
	ctx := context.Background()
	total := int64(128 << 20)
	entry := int64(16 << 20)
	reconstruct := int64(96 << 20)

	got, err := db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{
		LocalMaxBytes:       &total,
		LocalMaxEntryBytes:  &entry,
		ReconstructMaxBytes: &reconstruct,
	})
	if err != nil {
		t.Fatalf("UpdateResponseCacheSettings(all) error = %v", err)
	}
	assertResponseCacheSettings(t, got, ResponseCacheSettings{
		LocalMaxBytes:       total,
		LocalMaxEntryBytes:  entry,
		ReconstructMaxBytes: reconstruct,
		WritePolicy:         DefaultResponseCacheWritePolicy,
		Generation:          2,
	})

	newReconstruct := int64(128 << 20)
	got, err = db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{
		ReconstructMaxBytes: &newReconstruct,
	})
	if err != nil {
		t.Fatalf("UpdateResponseCacheSettings(partial) error = %v", err)
	}
	assertResponseCacheSettings(t, got, ResponseCacheSettings{
		LocalMaxBytes:       total,
		LocalMaxEntryBytes:  entry,
		ReconstructMaxBytes: newReconstruct,
		WritePolicy:         DefaultResponseCacheWritePolicy,
		Generation:          3,
	})

	got, err = db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{
		LocalMaxBytes: &total,
	})
	if err != nil {
		t.Fatalf("UpdateResponseCacheSettings(same value) error = %v", err)
	}
	if got.Generation != 3 {
		t.Fatalf("same-value generation = %d, want 3", got.Generation)
	}
	got, err = db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{})
	if err != nil {
		t.Fatalf("UpdateResponseCacheSettings(empty) error = %v", err)
	}
	if got.Generation != 3 {
		t.Fatalf("empty-update generation = %d, want 3", got.Generation)
	}
}

func TestResponseCacheSettingsValidationBoundaries(t *testing.T) {
	valid := []ResponseCacheSettings{
		{LocalMaxBytes: 8 << 20, LocalMaxEntryBytes: 1 << 20, ReconstructMaxBytes: 8 << 20, Generation: 1},
		{LocalMaxBytes: 4 << 30, LocalMaxEntryBytes: 256 << 20, ReconstructMaxBytes: 512 << 20, Generation: 1},
		{LocalMaxBytes: 8 << 20, LocalMaxEntryBytes: 1 << 20, ReconstructMaxBytes: 8 << 20, WritePolicy: ResponseCacheWritePolicyOnDemand, Generation: 1},
	}
	for _, settings := range valid {
		if err := ValidateResponseCacheSettings(settings); err != nil {
			t.Fatalf("ValidateResponseCacheSettings(%+v) error = %v", settings, err)
		}
	}

	invalid := []ResponseCacheSettings{
		{LocalMaxBytes: (8 << 20) - 1, LocalMaxEntryBytes: 1 << 20, ReconstructMaxBytes: 8 << 20, Generation: 1},
		{LocalMaxBytes: (4 << 30) + 1, LocalMaxEntryBytes: 1 << 20, ReconstructMaxBytes: 8 << 20, Generation: 1},
		{LocalMaxBytes: 8 << 20, LocalMaxEntryBytes: (1 << 20) - 1, ReconstructMaxBytes: 8 << 20, Generation: 1},
		{LocalMaxBytes: 4 << 30, LocalMaxEntryBytes: (256 << 20) + 1, ReconstructMaxBytes: 8 << 20, Generation: 1},
		{LocalMaxBytes: 8 << 20, LocalMaxEntryBytes: 1 << 20, ReconstructMaxBytes: (8 << 20) - 1, Generation: 1},
		{LocalMaxBytes: 8 << 20, LocalMaxEntryBytes: 1 << 20, ReconstructMaxBytes: (512 << 20) + 1, Generation: 1},
		{LocalMaxBytes: 8 << 20, LocalMaxEntryBytes: (8 << 20) + 1, ReconstructMaxBytes: 8 << 20, Generation: 1},
		{LocalMaxBytes: 8 << 20, LocalMaxEntryBytes: 1 << 20, ReconstructMaxBytes: 8 << 20, Generation: 0},
		{LocalMaxBytes: 8 << 20, LocalMaxEntryBytes: 1 << 20, ReconstructMaxBytes: 8 << 20, WritePolicy: "banana", Generation: 1},
	}
	for _, settings := range invalid {
		if err := ValidateResponseCacheSettings(settings); !errors.Is(err, ErrInvalidResponseCacheSettings) {
			t.Fatalf("ValidateResponseCacheSettings(%+v) error = %v, want ErrInvalidResponseCacheSettings", settings, err)
		}
	}
}

func TestResponseCacheSettingsInvalidMergedUpdateRollsBack(t *testing.T) {
	db := newResponseCacheSettingsTestDB(t)
	ctx := context.Background()
	before, err := db.GetResponseCacheSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tooSmallTotal := int64(8 << 20)
	tooLargeEntry := int64(9 << 20)
	_, err = db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{
		LocalMaxBytes:      &tooSmallTotal,
		LocalMaxEntryBytes: &tooLargeEntry,
	})
	if !errors.Is(err, ErrInvalidResponseCacheSettings) {
		t.Fatalf("invalid merged update error = %v, want ErrInvalidResponseCacheSettings", err)
	}
	after, err := db.GetResponseCacheSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertResponseCacheSettings(t, after, before)
}

func TestResponseCacheSettingsConcurrentDisjointSQLiteUpdatesDoNotLoseChanges(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent-response-cache.db")
	db1, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite db1) error = %v", err)
	}
	t.Cleanup(func() { _ = db1.Close() })
	db2, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite db2) error = %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	total := int64(128 << 20)
	reconstruct := int64(96 << 20)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := db1.UpdateResponseCacheSettings(context.Background(), ResponseCacheSettingsUpdate{LocalMaxBytes: &total})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := db2.UpdateResponseCacheSettings(context.Background(), ResponseCacheSettingsUpdate{ReconstructMaxBytes: &reconstruct})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent update error = %v", err)
		}
	}

	got, err := db1.GetResponseCacheSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertResponseCacheSettings(t, got, ResponseCacheSettings{
		LocalMaxBytes:       total,
		LocalMaxEntryBytes:  testResponseCacheDefaultEntry,
		ReconstructMaxBytes: reconstruct,
		WritePolicy:         DefaultResponseCacheWritePolicy,
		Generation:          3,
	})
}

func TestResponseCacheSettingsLargeSystemSettingsUpsertCannotOverwriteNarrowValues(t *testing.T) {
	db := newResponseCacheSettingsTestDB(t)
	ctx := context.Background()
	total := int64(128 << 20)
	committed, err := db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{LocalMaxBytes: &total})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSystemSettings(ctx, &SystemSettings{SiteName: "stale full snapshot"}); err != nil {
		t.Fatalf("UpdateSystemSettings() error = %v", err)
	}
	got, err := db.GetResponseCacheSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertResponseCacheSettings(t, got, committed)
}

func TestResponseCacheSettingsGenerationOverflowRollsBack(t *testing.T) {
	db := newResponseCacheSettingsTestDB(t)
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `
		INSERT INTO system_settings (id, response_cache_config_generation)
		VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET response_cache_config_generation = $1
	`, int64(math.MaxInt64)); err != nil {
		t.Fatalf("seed max generation: %v", err)
	}
	total := int64(128 << 20)
	_, err := db.UpdateResponseCacheSettings(ctx, ResponseCacheSettingsUpdate{LocalMaxBytes: &total})
	if !errors.Is(err, ErrResponseCacheGenerationOverflow) {
		t.Fatalf("overflow update error = %v, want ErrResponseCacheGenerationOverflow", err)
	}
	got, err := db.GetResponseCacheSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != math.MaxInt64 || got.LocalMaxBytes != testResponseCacheDefaultTotal {
		t.Fatalf("settings after overflow = %+v, want unchanged defaults with MaxInt64 generation", got)
	}
}

func newResponseCacheSettingsTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "response-cache-settings.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertResponseCacheSettings(t *testing.T, got, want ResponseCacheSettings) {
	t.Helper()
	if got != want {
		t.Fatalf("ResponseCacheSettings = %+v, want %+v", got, want)
	}
}
