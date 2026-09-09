package proxy

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func newSessionCooldownFixture(test *testing.T) (*Handler, promptfilter.Config) {
	test.Helper()
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "cooldown.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	handler := promptSessionLimitVerifiedTestHandler(test)
	handler.db = db
	accountID, err := db.InsertOpenAIResponsesAccount(context.Background(), "test", map[string]interface{}{"base_url": "https://example.test", "api_key": "test"}, "")
	if err != nil {
		test.Fatal(err)
	}
	handler.store.AddAccount(&auth.Account{DBID: accountID, SessionCapacityEnabled: true, SessionCapacityMax: 100, SessionCapacityIdleTTLSeconds: 60})
	for index := 0; index < 10; index++ {
		started := time.Now().UTC().Add(-time.Duration(120+index) * time.Minute).Truncate(time.Second)
		if err := db.InsertUsageLog(context.Background(), &database.UsageLogInput{
			AccountID: accountID, NewAPIPlatform: "newapi", NewAPIUserID: "42", SessionHash: fmt.Sprintf("historical-%d", index),
			SessionUsagePeriodID: fmt.Sprintf("period-%d", index), SessionUsageStartedAt: started, ObservedAt: started.Add(5 * time.Minute),
			SessionUsageIdleSeconds: 60, StatusCode: 200,
		}); err != nil {
			test.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	config := promptfilter.Config{Advanced: promptfilter.DefaultAdvancedConfig()}
	config.Advanced.Risk.SessionCreationCooldown.Mode = "enforce"
	return handler, config
}

func completeSessionCooldownFixture(handler *Handler, request *gin.Context, success bool) {
	if receipt := cooldownReceipt(request); receipt != nil {
		receipt.Successful = success
	}
	handler.finishSessionCooldown(request)
}

func TestSessionCooldownThirdRootBoundariesAndRestart(test *testing.T) {
	handler, config := newSessionCooldownFixture(test)
	var subject string
	for _, root := range []string{"first", "second"} {
		request := promptSessionLimitVerifiedUserContext(root)
		status, blocked := handler.checkPromptSessionCreationLimit(request, config, nil)
		if blocked || cooldownReceipt(request) == nil {
			test.Fatalf("initial root: %+v blocked=%v", status, blocked)
		}
		subject = status.Subject
		completeSessionCooldownFixture(handler, request, true)
	}
	third := promptSessionLimitVerifiedUserContext("third")
	status, blocked := handler.checkPromptSessionCreationLimit(third, config, nil)
	if !blocked || !status.Cooldown || status.RetryAfter < 599 || status.RetryAfter > 600 {
		test.Fatalf("third root: %+v blocked=%v", status, blocked)
	}
	if !strings.Contains(promptSessionCreationLimitMessage(status), "已有会话可继续使用") || string(promptSessionCreationLimitAPIError(status).Code) != "session_creation_limit_exceeded" {
		test.Fatal("wrong cooldown error")
	}
	firstRecovery := status.NextRecoveryAt
	repeat, _ := handler.checkPromptSessionCreationLimit(third, config, nil)
	if !repeat.NextRecoveryAt.Equal(firstRecovery) {
		test.Fatal("rejection extended cooldown")
	}
	if _, blocked := handler.checkPromptSessionCreationLimit(promptSessionLimitVerifiedUserContext("first"), config, nil); blocked {
		test.Fatal("existing root blocked")
	}
	restarted := &Handler{store: handler.store, db: handler.db}
	if status, blocked := restarted.checkPromptSessionCreationLimit(promptSessionLimitVerifiedUserContext("after-restart"), config, nil); !blocked || !status.Cooldown {
		test.Fatalf("restart lost state: %+v", status)
	}
	if err := handler.db.UpdateSessionCooldown(context.Background(), subject, func(state *database.SessionCooldownState) error {
		for _, root := range state.Roots {
			root.CreatedAt -= int64(11 * time.Minute / time.Millisecond)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	request := promptSessionLimitVerifiedUserContext("after-spacing")
	defer completeSessionCooldownFixture(handler, request, false)
	if _, blocked := handler.checkPromptSessionCreationLimit(request, config, nil); blocked {
		test.Fatal("wait added despite elapsed spacing")
	}
}

func TestSessionCooldownFailureRollbackAndSameRootRequests(test *testing.T) {
	handler, config := newSessionCooldownFixture(test)
	first := promptSessionLimitVerifiedUserContext("same")
	second := promptSessionLimitVerifiedUserContext("same")
	status, _ := handler.checkPromptSessionCreationLimit(first, config, nil)
	handler.checkPromptSessionCreationLimit(second, config, nil)
	completeSessionCooldownFixture(handler, first, false)
	if len(handler.promptSessionLimits[status.Subject]) != 1 {
		test.Fatal("removed another in-flight lease")
	}
	completeSessionCooldownFixture(handler, second, false)
	if len(handler.promptSessionLimits[status.Subject]) != 0 {
		test.Fatal("failed creation consumed user window")
	}
	for index := 0; index < 4; index++ {
		request := promptSessionLimitVerifiedUserContext(fmt.Sprint(index))
		if _, blocked := handler.checkPromptSessionCreationLimit(request, config, nil); blocked {
			test.Fatal("failed requests accumulated cooldown")
		}
		completeSessionCooldownFixture(handler, request, false)
	}
	request := promptSessionLimitVerifiedUserContext("success-after-retry")
	handler.checkPromptSessionCreationLimit(request, config, nil)
	lease := cooldownReceipt(request).Lease
	handler.checkPromptSessionCreationLimit(request, config, nil)
	if cooldownReceipt(request).Lease != lease {
		test.Fatal("retry created a new reservation")
	}
	completeSessionCooldownFixture(handler, request, true)
}

func TestSessionCooldownConcurrentRootsAndFrameCleanup(test *testing.T) {
	handler, config := newSessionCooldownFixture(test)
	var workers sync.WaitGroup
	requests := make(chan *gin.Context, 10)
	for index := 0; index < 10; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			request := promptSessionLimitVerifiedUserContext(fmt.Sprintf("parallel-%d", index))
			if _, blocked := handler.checkPromptSessionCreationLimit(request, config, nil); !blocked {
				requests <- request
			}
		}()
	}
	workers.Wait()
	close(requests)
	if len(requests) != 2 {
		test.Errorf("concurrent admitted=%d want 2", len(requests))
	}
	for request := range requests {
		completeSessionCooldownFixture(handler, request, true)
		if cooldownReceipt(request) != nil {
			test.Fatal("frame receipt retained")
		}
	}
}

func TestSessionCooldownExemptionsAndObservation(test *testing.T) {
	for _, mode := range []string{"off", "observe", "insufficient", "user-off", "unsigned"} {
		test.Run(mode, func(test *testing.T) {
			handler, config := newSessionCooldownFixture(test)
			switch mode {
			case "off", "observe":
				config.Advanced.Risk.SessionCreationCooldown.Mode = mode
			case "insufficient":
				config.Advanced.Risk.SessionCreationCooldown.MinSamples = 20
			case "user-off":
				handler.store.ApplyPromptSessionLimitOverride(database.PromptSessionLimitOverride{Platform: "newapi", NewAPIUserID: "42", Mode: database.PromptSessionLimitModeOff})
			}
			for index := 0; index < 4; index++ {
				request := promptSessionLimitVerifiedUserContext(fmt.Sprint(index))
				if mode == "unsigned" {
					request = promptSessionLimitTestContext(fmt.Sprint(index))
				}
				if _, blocked := handler.checkPromptSessionCreationLimit(request, config, nil); blocked {
					test.Fatalf("%s incorrectly blocked", mode)
				}
				completeSessionCooldownFixture(handler, request, true)
			}
		})
	}
}

func TestSessionCooldownRecoveryEndsWhenFrequencyDrops(test *testing.T) {
	config := promptfilter.DefaultSessionCreationCooldownConfig()
	now := time.Now().UTC()
	state := &database.SessionCooldownState{Roots: map[string]*database.SessionCooldownRoot{
		"old": {CreatedAt: now.Add(-29 * time.Minute).UnixMilli(), Confirmed: true},
		"new": {CreatedAt: now.UnixMilli(), Confirmed: true},
	}}
	recovery, count := sessionCooldownRecovery(state, config, 900, now)
	if count != 2 || recovery.Sub(now) > time.Minute {
		test.Fatalf("recovery=%v count=%d", recovery, count)
	}
	state.Roots["pending"] = &database.SessionCooldownRoot{CreatedAt: now.UnixMilli(), Leases: map[string]int64{"abandoned": now.Add(-time.Second).UnixMilli()}}
	pruneSessionCooldown(state, now, time.Hour)
	if state.Roots["pending"] != nil {
		test.Fatal("abandoned lease persisted")
	}
}

func TestSessionCooldownUsageOutcomeAndReusedFrameIsolation(test *testing.T) {
	handler, config := newSessionCooldownFixture(test)
	request := promptSessionLimitVerifiedUserContext("frame-one")
	for _, sample := range []struct {
		root    string
		status  int
		success bool
	}{{"frame-one", 500, false}, {"frame-two", 200, true}} {
		request.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
			Identity: newAPIIdentity{UserID: "42"}, APIKeyID: 7, Platform: "newapi", MetaVerified: true,
			Meta: newAPIPolicyMeta{SessionFingerprint: sample.root},
		})
		if _, blocked := handler.checkPromptSessionCreationLimit(request, config, nil); blocked {
			test.Fatal("frame blocked")
		}
		receipt := cooldownReceipt(request)
		if receipt == nil || receipt.Root != hashRiskIdentity(sample.root) || receipt.Successful {
			test.Fatalf("stale frame receipt: %+v", receipt)
		}
		handler.logUsageForRequest(request, &database.UsageLogInput{AccountID: 1, StatusCode: sample.status})
		if receipt.Successful != sample.success {
			test.Fatalf("usage status %d did not update receipt: %+v", sample.status, receipt)
		}
		handler.finishSessionCooldown(request)
		if cooldownReceipt(request) != nil {
			test.Fatal("completed frame was retained")
		}
	}
}

func TestSessionCooldownRelatedAndCompactionDoNotConsumeCreations(test *testing.T) {
	handler, config := newSessionCooldownFixture(test)
	for _, kind := range []string{"guardian", "compact"} {
		request := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint(kind), promptSessionTestFingerprint("missing-root"))
		value, _ := request.Get(newAPIPolicyMetaContextKey)
		identity := value.(verifiedNewAPIPolicyContext)
		identity.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
		identity.Meta.ThreadSource = "guardian_review"
		request.Set(newAPIPolicyMetaContextKey, identity)
		if kind == "compact" {
			request.Request.URL.Path = "/v1/responses/compact"
		}
		status, blocked := handler.checkPromptSessionCreationLimit(request, config, nil)
		if blocked || status.Used != 0 || cooldownReceipt(request) != nil {
			test.Fatalf("related request counted: %+v", status)
		}
	}
}

func TestSessionCooldownCombinesWithQuotaAndDoesNotReserveRejectedRoot(test *testing.T) {
	handler, config := newSessionCooldownFixture(test)
	config.Advanced.Risk.SessionCreationLimitEnabled = true
	config.Advanced.Risk.SessionCreationLimit = 2
	config.Advanced.Risk.SessionCreationLimitWindowSeconds = 60
	for _, root := range []string{"first-quota", "second-quota"} {
		request := promptSessionLimitVerifiedUserContext(root)
		if _, blocked := handler.checkPromptSessionCreationLimit(request, config, nil); blocked {
			test.Fatal("initial creation rejected")
		}
		completeSessionCooldownFixture(handler, request, true)
	}
	request := promptSessionLimitVerifiedUserContext("third-quota")
	status, blocked := handler.checkPromptSessionCreationLimit(request, config, nil)
	if !blocked || !status.Cooldown || status.RetryAfter < 599 || cooldownReceipt(request) != nil {
		test.Fatalf("incorrect combined limit: %+v", status)
	}
	if status.Used != 2 {
		test.Fatal("rejection consumed a window")
	}
	config.Advanced.Risk.SessionCreationLimit = 1
	config.Advanced.Risk.SessionCreationCooldown.Mode = "observe"
	if status, blocked := handler.checkPromptSessionCreationLimit(request, config, nil); !blocked || status.Cooldown {
		test.Fatal("observe mode weakened the original quota")
	}
}

func TestSessionCooldownExistingWindowFailureDoesNotRevokeIt(test *testing.T) {
	handler, config := newSessionCooldownFixture(test)
	config.Advanced.Risk.SessionCreationLimitEnabled = true
	config.Advanced.Risk.SessionCreationCooldown.Mode = "off"
	request := promptSessionLimitVerifiedUserContext("existing-before-feature")
	status, blocked := handler.checkPromptSessionCreationLimit(request, config, nil)
	if blocked {
		test.Fatal("initial window rejected")
	}
	config.Advanced.Risk.SessionCreationCooldown.Mode = "enforce"
	if _, blocked := handler.checkPromptSessionCreationLimit(request, config, nil); blocked {
		test.Fatal("existing window rejected after enabling")
	}
	completeSessionCooldownFixture(handler, request, false)
	if _, exists := handler.promptSessionLimits[status.Subject][status.SessionHash]; !exists {
		test.Fatal("a failed continuation deleted the original window")
	}
}
