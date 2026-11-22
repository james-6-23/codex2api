package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBackgroundQuietHoursPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw := `[{"start":"00:00","end":"07:00"},{"start":"23:30","end":"00:15"}]`
	if err = db.UpdateSystemSettings(context.Background(), &SystemSettings{BackgroundQuietHours: raw, MaxConcurrency: 3}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.BackgroundQuietHours != raw || s.MaxConcurrency != 3 {
		t.Fatalf("settings after restart: quiet=%q concurrency=%d", s.BackgroundQuietHours, s.MaxConcurrency)
	}
	for _, bad := range []string{`null`, `{}`, `[{"start":"+1:00","end":"07:00"}]`, `[{"start":"24:00","end":"07:00"}]`, `[{"start":"00:00","end":"00:00"}]`, `[{"start":"12:60","end":"13:00"}]`} {
		s.BackgroundQuietHours = bad
		if err = db.UpdateSystemSettings(context.Background(), s); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	s, err = db.GetSystemSettings(context.Background())
	if err != nil || s.BackgroundQuietHours != raw {
		t.Fatal("invalid save changed persisted schedule")
	}
}
