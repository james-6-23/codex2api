package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	ic "github.com/codex2api/internal/intelligencecheck"
)

type intelligenceHoldFixture struct {
	t        *testing.T
	h        *Handler
	db       *database.DB
	store    *auth.Store
	account  *auth.Account
	id       int64
	requests atomic.Int32
	status   atomic.Int32
}

// newIntelligenceHoldFixture enrolls one account in automatic checks against a
// local upstream whose response status the test controls.
func newIntelligenceHoldFixture(t *testing.T) *intelligenceHoldFixture {
	t.Helper()
	f := &intelligenceHoldFixture{t: t, db: newTestAdminDB(t)}
	f.id = insertTestAccount(t, f.db)
	f.status.Store(http.StatusOK)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if status := int(f.status.Load()); status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", intelligenceSuccess)
	}))
	t.Cleanup(upstream.Close)
	ctx := context.Background()
	if err := f.db.UpdateCredentials(ctx, f.id, map[string]interface{}{"upstream_type": auth.UpstreamOpenAIResponses, "base_url": upstream.URL, "api_key": "fixture-key", "models": []string{"gpt-5.4"}}); err != nil {
		t.Fatal(err)
	}
	f.store = auth.NewStore(f.db, nil, nil)
	if err := f.store.LoadAccountByID(ctx, f.id); err != nil {
		t.Fatal(err)
	}
	f.account = f.store.FindByID(f.id)
	settings, err := f.db.GetIntelligenceCheckSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.Enabled = true
	settings.GPTModel = "gpt-5.4"
	if _, err = f.db.SaveIntelligenceCheckSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	f.h = &Handler{store: f.store, db: f.db}
	return f
}

// tick makes the automatic check due, then runs one scheduler pass to completion.
func (f *intelligenceHoldFixture) tick() {
	f.t.Helper()
	ctx := context.Background()
	if err := f.db.SetIntelligenceCheckEnabled(ctx, []int64{f.id}, true); err != nil {
		f.t.Fatal(err)
	}
	if err := f.h.intelligenceTick(ctx); err != nil {
		f.t.Fatal(err)
	}
	f.h.WaitIntelligenceChecks()
}

func (f *intelligenceHoldFixture) runs() []database.IntelligenceCheckRun {
	f.t.Helper()
	runs, _, err := f.db.ListIntelligenceCheckRuns(context.Background(), database.IntelligenceCheckRunFilter{AccountID: f.id})
	if err != nil {
		f.t.Fatal(err)
	}
	return runs
}

func (f *intelligenceHoldFixture) state() database.IntelligenceCheckAccount {
	f.t.Helper()
	accounts, _, err := f.db.ListIntelligenceCheckAccounts(context.Background(), database.IntelligenceCheckAccountFilter{AccountID: f.id})
	if err != nil || len(accounts) != 1 {
		f.t.Fatal(accounts, err)
	}
	return accounts[0]
}

// Automatic checks leave a rate-limited account alone and resume once it recovers.
func TestIntelligenceRunnerWaitsForRateLimitedAccount(t *testing.T) {
	f := newIntelligenceHoldFixture(t)
	f.store.MarkCooldown(f.account, time.Hour, "usage_limit")
	for i := 0; i < 3; i++ {
		f.tick()
	}
	if got := f.requests.Load(); got != 0 {
		t.Fatalf("rate-limited account was probed %d times", got)
	}
	if runs := f.runs(); len(runs) != 0 {
		t.Fatalf("waiting left records: %+v", runs)
	}
	if state := f.state(); state.ProbePhase != ic.PhaseQuotaWait || state.Status != "unknown" || state.DegradeStreak != 0 {
		t.Fatalf("unexpected waiting state: %+v", state)
	}

	f.store.ClearCooldown(f.account)
	f.tick()
	runs := f.runs()
	if len(runs) != 1 || runs[0].Status != "normal" || runs[0].Source != "automatic" || f.requests.Load() != 1 {
		t.Fatalf("check did not resume after recovery: runs=%+v requests=%d", runs, f.requests.Load())
	}
	if state := f.state(); state.ProbePhase == ic.PhaseQuotaWait {
		t.Fatalf("wait outlived recovery: %+v", state)
	}
}

// A check queued before the limit was known is withdrawn instead of sent.
func TestIntelligenceRunnerDoesNotSendQueuedCheckToRateLimitedAccount(t *testing.T) {
	f := newIntelligenceHoldFixture(t)
	ctx := context.Background()
	if n, err := f.db.EnqueueDueIntelligenceChecks(ctx, time.Now(), 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	f.store.MarkCooldown(f.account, time.Hour, "usage_limit")
	if err := f.h.intelligenceTick(ctx); err != nil {
		t.Fatal(err)
	}
	f.h.WaitIntelligenceChecks()
	runs := f.runs()
	if len(runs) != 1 || runs[0].Status != "interrupted" || !strings.Contains(runs[0].Error, "已跳过") || f.requests.Load() != 0 {
		t.Fatalf("queued check was not withdrawn: runs=%+v requests=%d", runs, f.requests.Load())
	}
	// Later passes wait rather than queue again.
	f.tick()
	if runs = f.runs(); len(runs) != 1 || f.requests.Load() != 0 {
		t.Fatalf("withdrawn check was retried: runs=%+v requests=%d", runs, f.requests.Load())
	}
}

// A probe that is itself rejected for quota is neutral, and is not repeated
// until the quota recovers.
func TestIntelligenceRunnerTreatsQuotaRejectionAsNeutral(t *testing.T) {
	f := newIntelligenceHoldFixture(t)
	f.status.Store(http.StatusTooManyRequests)
	f.tick()
	runs := f.runs()
	if len(runs) != 1 || runs[0].Status != "interrupted" || f.requests.Load() != 1 {
		t.Fatalf("quota rejection misclassified: runs=%+v requests=%d", runs, f.requests.Load())
	}
	if !strings.Contains(runs[0].Error, "不计入结果") || !strings.Contains(runs[0].Error, "429") {
		t.Fatalf("upstream detail lost: %q", runs[0].Error)
	}
	if state := f.state(); state.Status != "interrupted" || state.DegradeStreak != 0 || state.RecoverStreak != 0 {
		t.Fatalf("quota rejection was graded: %+v", state)
	}
	for i := 0; i < 3; i++ {
		f.tick()
	}
	if runs = f.runs(); len(runs) != 1 || f.requests.Load() != 1 {
		t.Fatalf("rejected probe was repeated: runs=%d requests=%d", len(runs), f.requests.Load())
	}
}

// Failures unrelated to quota keep their meaning.
func TestIntelligenceRunnerKeepsOtherUpstreamErrors(t *testing.T) {
	f := newIntelligenceHoldFixture(t)
	f.status.Store(http.StatusInternalServerError)
	f.tick()
	runs := f.runs()
	if len(runs) != 1 || runs[0].Status != "error" || f.requests.Load() != 1 {
		t.Fatalf("upstream error misclassified: runs=%+v requests=%d", runs, f.requests.Load())
	}
}

// An explicit manual check is still sent to a rate-limited account.
func TestIntelligenceRunnerSendsManualCheckToRateLimitedAccount(t *testing.T) {
	f := newIntelligenceHoldFixture(t)
	ctx := context.Background()
	f.store.MarkCooldown(f.account, time.Hour, "usage_limit")
	if _, err := f.db.EnqueueIntelligenceChecks(ctx, []int64{f.id}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.h.intelligenceTick(ctx); err != nil {
		t.Fatal(err)
	}
	f.h.WaitIntelligenceChecks()
	runs := f.runs()
	if len(runs) != 1 || runs[0].Source != "manual" || runs[0].Status != "normal" || f.requests.Load() != 1 {
		t.Fatalf("manual check was held back: runs=%+v requests=%d", runs, f.requests.Load())
	}
}
