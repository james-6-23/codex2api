package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestBackgroundQuietHoursBoundaries(t *testing.T) {
	s := &Store{}
	if err := s.SetBackgroundQuietHours(`[{"start":"23:00","end":"07:00"},{"start":"06:00","end":"08:00"},{"start":"12:00","end":"13:00"}]`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		utc    string
		paused bool
		next   string
	}{
		{"2026-11-16T14:59:59Z", false, "2026-11-16T14:59:59Z"},
		{"2026-11-16T15:00:00Z", true, "2026-11-17T00:00:00Z"},
		{"2026-11-16T23:00:00Z", true, "2026-11-17T00:00:00Z"},
		{"2026-11-17T00:00:00Z", false, "2026-11-17T00:00:00Z"},
		{"2026-11-17T04:00:00Z", true, "2026-11-17T05:00:00Z"},
		{"2026-11-17T05:00:00Z", false, "2026-11-17T05:00:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.utc)
		if got := s.BackgroundInspectionPaused(now.In(time.FixedZone("host", -7*3600))); got != tc.paused {
			t.Errorf("%s paused=%v", tc.utc, got)
		}
		next, _ := time.Parse(time.RFC3339, tc.next)
		if got := s.NextBackgroundInspectionTime(now); !got.Equal(next) {
			t.Errorf("%s next=%s want=%s", tc.utc, got, next)
		}
	}
	if err := s.SetBackgroundQuietHours(`[{"start":"00:00","end":"24:00"}]`); err != nil {
		t.Fatal(err)
	}
	if !s.NextBackgroundInspectionTime(time.Now()).IsZero() {
		t.Fatal("all-day schedule must not spin")
	}
	if err := s.SetBackgroundQuietHours(`[{"start":"09:00","end":"09:00"}]`); err == nil {
		t.Fatal("invalid schedule accepted")
	}
	if !s.BackgroundInspectionPaused(time.Now()) {
		t.Fatal("invalid update replaced schedule")
	}
	if err := s.SetBackgroundQuietHours(`[]`); err != nil {
		t.Fatal(err)
	}
	if s.BackgroundInspectionPaused(time.Now()) {
		t.Fatal("cleared schedule still blocks")
	}
}

func TestBackgroundQuietHoursBlocksAutomaticButAllowsManual(t *testing.T) {
	s := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, BackgroundQuietHours: `[{"start":"00:00","end":"24:00"}]`})
	t.Cleanup(s.Stop)
	s.AddAccount(&Account{DBID: 1, AccessToken: "fixture", Status: StatusReady})
	var called atomic.Int32
	s.SetUsageProbeFunc(func(context.Context, *Account) error { called.Add(1); return nil })
	s.parallelProbeUsage(context.Background())
	s.parallelRecoveryProbe(context.Background())
	s.TriggerUsageProbeAsync()
	s.TriggerRecoveryProbeAsync()
	if called.Load() != 0 || s.UsageProbeRunning() || s.RecoveryProbeRunning() {
		t.Fatal("automatic probe escaped quiet hours")
	}
	s.parallelProbeUsageWith(context.Background(), 0)
	if called.Load() != 1 {
		t.Fatal("manual probe blocked")
	}
	if err := s.SetBackgroundQuietHours(`[]`); err != nil {
		t.Fatal(err)
	}
	s.parallelProbeUsageWith(context.Background(), time.Nanosecond)
	if called.Load() != 2 {
		t.Fatal("automatic probes did not resume")
	}
}
