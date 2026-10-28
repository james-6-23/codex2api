package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestScheduledConnectionTestsOverlapAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx, cancel := context.WithCancel(context.Background())
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			started := make(chan struct{}, 16)
			var active, peak, calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_scheduled\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			}))
			t.Cleanup(upstream.Close)
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduled.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
			t.Cleanup(store.Stop)
			h := &Handler{store: store, db: db}
			t.Cleanup(func() {
				cancel()
				unblock()
				h.WaitScheduledConnectionTests()
			})
			loc, err := time.LoadLocation(scheduledTestTimezone)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 26, 12, 0, 0, 0, loc)
			plan, _ := json.Marshal(accountScheduledTest{Enabled: true, Model: "gpt-5.5", Content: "hi", WeekdayTimes: []string{"12:00", "12:01"}})
			var ids []int64
			for i := 0; i < 5; i++ {
				id, err := db.InsertOpenAIResponsesAccount(ctx, fmt.Sprintf("scheduled-%d", i), map[string]interface{}{
					"upstream_type": auth.UpstreamOpenAIResponses, "base_url": upstream.URL,
					"api_key": "test-token", "models": []string{"gpt-5.5"}, scheduledTestCredentialKey: string(plan),
				}, "")
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
				store.AddAccount(&auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "test-token", Models: []string{"gpt-5.5"}, PlanType: "api"})
			}
			h.dispatchScheduledConnectionTests(ctx, now)
			for i := 0; i < 4; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("first batch did not start four attempts")
				}
			}
			// A blocked first batch must not prevent claiming the following minute.
			h.dispatchScheduledConnectionTests(ctx, now.Add(time.Minute))
			deadline := time.Now().Add(5 * time.Second)
			for {
				jobs, err := db.ListOneShotMaintenanceJobs(ctx, ids[0], scheduledTestJobPrefix, 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(jobs) == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("next minute was blocked by the running batch")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if shutdown {
				cancel()
			} else {
				unblock()
				// Re-dispatching a slot cannot send another upstream request.
				h.dispatchScheduledConnectionTests(ctx, now)
			}
			done := make(chan struct{})
			go func() { h.WaitScheduledConnectionTests(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("workers did not finish after release/cancellation")
			}
			if peak.Load() > 4 {
				t.Fatalf("global concurrency=%d, want at most 4", peak.Load())
			}
			if !shutdown {
				if calls.Load() != 10 {
					t.Fatalf("upstream attempts=%d, want one per account per minute (10)", calls.Load())
				}
				for _, id := range ids {
					jobs, err := db.ListOneShotMaintenanceJobs(ctx, id, scheduledTestJobPrefix, 10)
					if err != nil || len(jobs) != 2 {
						t.Fatalf("account %d jobs=%v err=%v", id, jobs, err)
					}
					for _, job := range jobs {
						var result scheduledTestRecord
						if json.Unmarshal([]byte(job.LastError), &result) != nil || !result.Success {
							t.Fatalf("unsuccessful slot for %d: %s", id, job.LastError)
						}
					}
				}
			}
		})
	}
}

func TestScheduledConnectionTestCancelledBeforeExecutionRecordsResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduled-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	h := &Handler{store: store, db: db}
	t.Cleanup(func() {
		cancel()
		h.WaitScheduledConnectionTests()
	})
	loc, err := time.LoadLocation(scheduledTestTimezone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, loc)
	plan, err := json.Marshal(accountScheduledTest{Enabled: true, Model: "gpt-5.5", Content: "hi", WeekdayTimes: []string{"12:00"}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertOpenAIResponsesAccount(ctx, "scheduled-cancel", map[string]interface{}{
		"upstream_type": auth.UpstreamOpenAIResponses, scheduledTestCredentialKey: string(plan),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Keep every execution slot occupied until after cancellation has finished.
	gate := h.scheduledConnectionTestGate()
	for i := 0; i < cap(gate); i++ {
		gate <- struct{}{}
	}
	h.dispatchScheduledConnectionTests(ctx, now)
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, err := db.ListOneShotMaintenanceJobs(ctx, id, scheduledTestJobPrefix, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued slot was not claimed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { h.WaitScheduledConnectionTests(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queued batch did not finish after cancellation")
	}
	jobs, err := db.ListOneShotMaintenanceJobs(context.Background(), id, scheduledTestJobPrefix, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	var result scheduledTestRecord
	if err := json.Unmarshal([]byte(jobs[0].LastError), &result); err != nil {
		t.Fatalf("cancelled claim has no result: %q (%v)", jobs[0].LastError, err)
	}
	if result.Status != "failed" || result.Success || result.Error == "" || result.AttemptedAt != "" {
		t.Fatalf("unexpected pre-execution cancellation result: %+v", result)
	}
	if result.AccountID != id || result.Model != "gpt-5.5" || result.ScheduledAt != now.Format(time.RFC3339) {
		t.Fatalf("cancelled result lost slot identity: %+v", result)
	}
	claimed, err := db.TryClaimMaintenanceJob(context.Background(), id, scheduledTestJobPrefix+now.Format("2006-01-02T15:04"), now)
	if err != nil || claimed {
		t.Fatalf("cancelled slot must not be retried: claimed=%v err=%v", claimed, err)
	}
}
