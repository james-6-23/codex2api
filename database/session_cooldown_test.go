package database

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSessionCooldownStateSerializesAcrossConnectionsAndPersists(test *testing.T) {
	path := filepath.Join(test.TempDir(), "cooldown.db")
	first, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = first.Close() })
	second, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = second.Close() })
	var workers sync.WaitGroup
	for index := 0; index < 20; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			database := []*DB{first, second}[index%2]
			err := database.UpdateSessionCooldown(context.Background(), "same-user", func(state *SessionCooldownState) error {
				state.Roots[fmt.Sprint(index)] = &SessionCooldownRoot{CreatedAt: int64(index), Confirmed: true}
				return nil
			})
			if err != nil {
				test.Error(err)
			}
		}()
	}
	workers.Wait()
	if err := second.UpdateSessionCooldown(context.Background(), "same-user", func(state *SessionCooldownState) error {
		if len(state.Roots) != 20 {
			test.Fatalf("lost concurrent writes: %d", len(state.Roots))
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if err := first.UpdateSessionCooldown(context.Background(), "different-user", func(state *SessionCooldownState) error {
		if len(state.Roots) != 0 {
			test.Fatal("cross-user state")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestSessionCooldownSamplesExcludeFailuresActiveAndLegacyPeriods(test *testing.T) {
	database, err := New("sqlite", filepath.Join(test.TempDir(), "samples.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = database.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	entries := []usageLogEntry{
		{AccountID: 1, SessionHash: "root", NewAPIPlatform: "a", NewAPIUserID: "7", SessionUsagePeriodID: "completed", SessionUsageStartedAt: now.Add(-2 * time.Hour), ObservedAt: now.Add(-115 * time.Minute), SessionUsageIdleSeconds: 60, StatusCode: 200},
		{AccountID: 1, SessionHash: "root", NewAPIPlatform: "a", NewAPIUserID: "7", SessionUsagePeriodID: "active", SessionUsageStartedAt: now.Add(-time.Minute), ObservedAt: now, SessionUsageIdleSeconds: 3600, StatusCode: 200},
		{AccountID: 1, SessionHash: "failure", NewAPIPlatform: "a", NewAPIUserID: "7", SessionUsagePeriodID: "failed", SessionUsageStartedAt: now.Add(-2 * time.Hour), ObservedAt: now.Add(-time.Hour), SessionUsageIdleSeconds: 60, StatusCode: 503},
		{AccountID: 1, SessionHash: "legacy", NewAPIPlatform: "a", NewAPIUserID: "7", SessionUsagePeriodID: "legacy", SessionUsageStartedAt: now.Add(-2 * time.Hour), ObservedAt: now.Add(-time.Hour), StatusCode: 200},
		{AccountID: 2, SessionHash: "other", NewAPIPlatform: "b", NewAPIUserID: "7", SessionUsagePeriodID: "other-platform", SessionUsageStartedAt: now.Add(-2 * time.Hour), ObservedAt: now.Add(-time.Hour), SessionUsageIdleSeconds: 60, StatusCode: 200},
	}
	if err := database.applyAccountSessionUsagePeriodsWithExec(context.Background(), database.conn, entries); err != nil {
		test.Fatal(err)
	}
	samples, err := database.SessionCooldownSamples(context.Background(), "a", "7", now.Add(-24*time.Hour), now, 20)
	if err != nil || len(samples) != 1 || samples[0].PeriodID != "completed" {
		test.Fatalf("samples=%+v err=%v", samples, err)
	}
	entries[0].ObservedAt = now
	entries[0].StatusCode = 500
	if err := database.applyAccountSessionUsagePeriodsWithExec(context.Background(), database.conn, entries[:1]); err != nil {
		test.Fatal(err)
	}
	samples, err = database.SessionCooldownSamples(context.Background(), "a", "7", now.Add(-24*time.Hour), now, 20)
	if err != nil || len(samples) != 0 {
		test.Fatalf("reused period eligible prematurely: %+v %v", samples, err)
	}
}
