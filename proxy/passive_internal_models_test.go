package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func standaloneNativeSessionContext(root, leaf string, sequence int, threadSource, requestKind, subagentKind string) *gin.Context {
	c := promptSessionLimitTestContext("")
	c.Request.Header = nativeSessionHeaders(root, leaf, sequence)
	metadata := fmt.Sprintf(`{"session_id":%q,"thread_id":%q,"window_id":%q,"parent_thread_id":%q,"thread_source":%q,"request_kind":%q`, root, leaf, fmt.Sprintf("%s:%d", leaf, sequence), root, threadSource, requestKind)
	if subagentKind != "" {
		metadata += fmt.Sprintf(`,"subagent_kind":%q`, subagentKind)
		c.Request.Header.Set("X-OpenAI-Subagent", subagentKind)
	}
	metadata += "}"
	c.Request.Header.Set(codexTurnMetadataHeader, metadata)
	return c
}

func passiveInternalModelTestHandler(enabled bool) (*Handler, *auth.Account, *auth.Account, string) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:               2,
		TestConcurrency:              1,
		PassiveInternalModelsEnabled: enabled,
	})
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 7, PlatformCode: "newapi", Enabled: true, RequireSignedIdentity: true,
	}})
	root := &auth.Account{DBID: 101, AccessToken: "root-token", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	other := &auth.Account{DBID: 202, AccessToken: "other-token", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	store.AddAccounts([]*auth.Account{root, other})
	rootFingerprint := promptSessionTestFingerprint("passive-internal-root")
	rootKey := "newapi-root-session:" + rootFingerprint + "::api-key:7"
	store.BindSessionAffinity(rootKey, root, "")
	return &Handler{store: store}, root, other, rootFingerprint
}

func TestPassiveInternalModelRoutingDefaultsOff(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	if store.PassiveInternalModelsEnabled() {
		t.Fatal("passive internal model routing must default to disabled")
	}
}

func TestPassiveInternalModelRoutingRequiresTrustedRelatedRequest(t *testing.T) {
	handler, root, other, rootFingerprint := passiveInternalModelTestHandler(true)
	ctx := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("direct-luna"), rootFingerprint)
	raw, _ := ctx.Get(newAPIPolicyMetaContextKey)
	policy := raw.(verifiedNewAPIPolicyContext)
	policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRoot
	policy.Meta.ThreadSource = "subagent" // A source label alone must not authorize bypass.
	policy.Meta.RequestedModel = "gpt-5.6-luna"
	ctx.Set(newAPIPolicyMetaContextKey, policy)

	body := []byte(`{"model":"gpt-5.6-luna","input":"direct request"}`)
	identity := handler.resolveRequestSessionIdentityForContext(ctx, body)
	if identity.relatedToRoot {
		t.Fatal("thread_source alone was trusted as a derived request")
	}
	key := capacityAwareSessionAffinityKey(identity, 7)
	filter := handler.applyPassiveInternalModelRouting(ctx, "gpt-5.6-luna", "gpt-5.6-luna", identity, key, true, accountFilterForModel("gpt-5.6-luna"))
	if filter(root) || filter(other) {
		t.Fatal("direct Luna request bypassed the configured account model list")
	}
}

func TestPassiveInternalModelRoutingPinsTrustedRequestToRootAccount(t *testing.T) {
	for _, tc := range []struct {
		name           string
		requestedModel string
		effectiveModel string
	}{
		{name: "luna", requestedModel: "gpt-5.6-luna", effectiveModel: "gpt-5.6-luna"},
		{name: "auto review mapped to sol", requestedModel: "codex-auto-review", effectiveModel: "gpt-5.6-sol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, root, other, rootFingerprint := passiveInternalModelTestHandler(true)
			ctx := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("passive-leaf-"+tc.name), rootFingerprint)
			raw, _ := ctx.Get(newAPIPolicyMetaContextKey)
			policy := raw.(verifiedNewAPIPolicyContext)
			policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
			policy.Meta.ThreadSource = "subagent"
			policy.Meta.SubagentKind = "guardian"
			policy.Meta.RequestedModel = tc.requestedModel
			ctx.Set(newAPIPolicyMetaContextKey, policy)

			body := []byte(`{"model":"` + tc.effectiveModel + `","input":"review"}`)
			identity := handler.resolveRequestSessionIdentityForContext(ctx, body)
			setPassiveInternalAuthorization(ctx, newAPIPassiveFeatureGuardianApproval, true)
			if !identity.relatedToRoot {
				t.Fatal("signed related request was not recognized")
			}
			key := capacityAwareSessionAffinityKey(identity, 7)
			filter := handler.applyPassiveInternalModelRouting(ctx, tc.effectiveModel, tc.effectiveModel, identity, key, true, accountFilterForModel(tc.effectiveModel))
			if !filter(root) {
				t.Fatal("root account was not authorized for trusted passive model")
			}
			if filter(other) {
				t.Fatal("trusted passive model escaped to another account")
			}
		})
	}
}

func TestStandaloneNativeCompactionRecoversRootAndPinsPassiveModel(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, PassiveInternalModelsEnabled: true,
	})
	first := &auth.Account{
		DBID: 101, AccessToken: "first", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityIdleTTLSeconds: 3600,
	}
	second := &auth.Account{
		DBID: 202, AccessToken: "second", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityIdleTTLSeconds: 3600,
	}
	store.AddAccounts([]*auth.Account{first, second})
	handler := &Handler{store: store}

	mainContext := standaloneNativeSessionContext(testRootSessionA, testLeafSessionA, 21, "user", "compaction", "")
	mainBody := []byte(`{"model":"gpt-5.6-sol","input":"continue"}`)
	mainIdentity := handler.resolveRequestSessionIdentityForContext(mainContext, mainBody)
	if !mainIdentity.relatedToRoot || !mainIdentity.ownsRootBinding {
		t.Fatalf("main compaction identity = %+v", mainIdentity)
	}
	rootKey := capacityAwareSessionAffinityKey(mainIdentity, 7)
	if rootKey != testRootSessionA+"::api-key:7" {
		t.Fatalf("main compaction key = %q", rootKey)
	}

	mainFilter := accountFilterForModel("gpt-5.6-sol")
	selected, proxyURL := store.NextForSessionWithFilter(rootKey, 7, nil, mainFilter)
	if selected == nil {
		t.Fatal("standalone main compaction could not recover a root account")
	}
	handler.bindAccountSession(mainContext, rootKey, selected, proxyURL)
	store.Release(selected)
	if got := store.AccountSessionCount(selected.DBID, time.Now()); got != 1 {
		t.Fatalf("root recovery created %d account windows, want 1", got)
	}

	// Reusing another compaction leaf for the same user-visible task must stay
	// on the recovered account and keep a single root window.
	continuedContext := standaloneNativeSessionContext(testRootSessionA, testLeafSessionB, 22, "user", "compaction", "")
	continuedIdentity := handler.resolveRequestSessionIdentityForContext(continuedContext, mainBody)
	continuedKey := capacityAwareSessionAffinityKey(continuedIdentity, 7)
	continued, continuedProxy := store.NextForSessionWithFilter(continuedKey, 7, nil, mainFilter)
	if continued != selected {
		if continued != nil {
			store.Release(continued)
		}
		t.Fatalf("continued compaction moved from account %d", selected.DBID)
	}
	handler.bindAccountSession(continuedContext, continuedKey, continued, continuedProxy)
	store.Release(continued)
	if got := store.AccountSessionCount(selected.DBID, time.Now()); got != 1 {
		t.Fatalf("continued compaction changed account windows to %d", got)
	}

	// A user-authored Luna request is still an ordinary direct model request;
	// it must not use the passive bypass merely because compaction has a child
	// graph shape.
	directLunaFilter := handler.applyPassiveInternalModelRouting(mainContext, "gpt-5.6-luna", "gpt-5.6-luna", mainIdentity, rootKey, true, accountFilterForModel("gpt-5.6-luna"))
	if directLunaFilter(first) || directLunaFilter(second) {
		t.Fatal("user-authored standalone Luna request bypassed account model settings")
	}

	guardianContext := standaloneNativeSessionContext(testRootSessionA, testIntermediate, 23, "subagent", "turn", "guardian")
	guardianBody := approvalReassessmentWireBody(t, approvalReassessmentWirePrompt(), "gpt-5.6-luna")
	guardianBody = []byte(strings.Replace(string(guardianBody), "00000000-0000-0000-0000-000000000001", testRootSessionA, 1))
	guardianIdentity := handler.resolveRequestSessionIdentityForContext(guardianContext, guardianBody)
	if !guardianIdentity.relatedToRoot || guardianIdentity.ownsRootBinding {
		t.Fatalf("standalone Guardian identity = %+v", guardianIdentity)
	}
	guardianKey := capacityAwareSessionAffinityKey(guardianIdentity, 7)
	if relatedRoot, ok := auth.RelatedSessionRootKey(guardianKey); !ok || relatedRoot != rootKey {
		t.Fatalf("Guardian key = %q, want related root %q", guardianKey, rootKey)
	}
	guardianFilter := handler.applyPassiveInternalModelRouting(guardianContext, "gpt-5.6-luna", "gpt-5.6-luna", guardianIdentity, guardianKey, true, accountFilterForModel("gpt-5.6-luna"))
	guardian, guardianProxy := store.NextForSessionWithFilter(guardianKey, 7, nil, guardianFilter)
	if guardian != selected {
		if guardian != nil {
			store.Release(guardian)
		}
		t.Fatalf("standalone Guardian did not reuse root account %d", selected.DBID)
	}
	handler.bindAccountSession(guardianContext, guardianKey, guardian, guardianProxy)
	store.Release(guardian)
	if got := store.AccountSessionCount(selected.DBID, time.Now()); got != 1 {
		t.Fatalf("Guardian changed account windows to %d", got)
	}
}

func TestStandaloneNativeRelatedRequestUsesFieldsAcrossModelAndPromptDrift(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, PassiveInternalModelsEnabled: true,
	})
	root := &auth.Account{DBID: 101, AccessToken: "root", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	other := &auth.Account{DBID: 202, AccessToken: "other", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	store.AddAccounts([]*auth.Account{root, other})
	handler := &Handler{store: store}
	rootKey := testRootSessionA + "::api-key:7"
	store.BindSessionAffinity(rootKey, root, "")

	c := standaloneNativeSessionContext(testRootSessionA, testIntermediate, 23, "subagent", "turn", "guardian")
	body := []byte(`{
		"model":"future-review-model",
		"instructions":"completely changed internal policy wording",
		"tools":[{"type":"function","name":"read_only_check"}],
		"input":[
			{"role":"developer","content":[{"type":"input_text","text":"read-only sandbox"}]},
			{"role":"user","content":[{"type":"input_text","text":"environment context"}]},
			{"role":"user","content":[{"type":"input_text","text":"assess the planned action"}]}
		]
	}`)
	identity := handler.resolveRequestSessionIdentityForContext(c, body)
	if !identity.relatedToRoot || identity.ownsRootBinding {
		t.Fatalf("native Guardian identity = %+v", identity)
	}
	if !passiveInternalRequestAuthorized(c) {
		t.Fatal("current desktop Guardian graph was not authorized as a passive internal request")
	}
	key := capacityAwareSessionAffinityKey(identity, 7)
	filter := handler.applyPassiveInternalModelRouting(c, "future-review-model", "future-review-model", identity, key, true, accountFilterForModel("future-review-model"))
	selected, _ := store.NextForSessionWithFilter(key, 7, nil, filter)
	if selected != root {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("native Guardian selected %+v, want root account %d", selected, root.DBID)
	}
	store.Release(selected)
}

func TestNativeRelatedFieldsDoNotDependOnEngineFingerprint(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, PassiveInternalModelsEnabled: true,
	})
	root := &auth.Account{DBID: 101, AccessToken: "root", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	store.AddAccount(root)
	handler := &Handler{store: store}
	rootKey := testRootSessionA + "::api-key:7"
	store.BindSessionAffinity(rootKey, root, "")

	c := promptSessionLimitTestContext("")
	c.Request.Header.Set("Session-Id", testRootSessionA)
	c.Request.Header.Set("Thread-Id", testIntermediate)
	c.Request.Header.Set("X-OpenAI-Subagent", "guardian")
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-5.6-luna",
		"client_metadata":{"session_id":%q,"thread_id":%q,"window_id":%q,"parent_thread_id":%q,"thread_source":"subagent","request_kind":"turn","subagent_kind":"guardian"},
		"input":[{"role":"user","content":[{"type":"input_text","text":"not a closed Guardian payload"}]}]
	}`, testRootSessionA, testIntermediate, testIntermediate+":23", testRootSessionA))
	identity := handler.resolveRequestSessionIdentityForContext(c, body)
	if !identity.relatedToRoot {
		t.Fatalf("test graph did not resolve as related: %+v", identity)
	}
	if !passiveInternalRequestAuthorized(c) {
		t.Fatal("a coherent related field graph was rejected because the client fingerprint changed")
	}
}

func TestVerifiedNewAPIRelatedFieldsWorkWithoutPassiveFeatureOrPayloadContract(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, PassiveInternalModelsEnabled: true,
	})
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 7, PlatformCode: "newapi", Enabled: true, RequireSignedIdentity: true,
	}})
	root := &auth.Account{DBID: 101, AccessToken: "root", Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	store.AddAccount(root)
	handler := &Handler{store: store}
	rootFingerprint := newAPIRootSessionFingerprint("newapi", "42", testRootSessionA)
	rootKey := "newapi-root-session:" + rootFingerprint + "::api-key:7"
	store.BindSessionAffinity(rootKey, root, "")

	c := standaloneNativeSessionContext(testRootSessionA, testIntermediate, 23, "subagent", "turn", "guardian")
	c.Set(contextAPIKeyID, int64(7))
	c.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
		Identity: newAPIIdentity{UserID: "42"}, APIKeyID: 7, Platform: "newapi", MetaVerified: true,
		Meta: newAPIPolicyMeta{
			RequestedModel:         "future-review-model",
			SessionFingerprint:     promptSessionTestFingerprint("guardian-leaf"),
			RootSessionVersion:     1,
			RootSessionState:       newAPIPolicyRootSessionResolved,
			RootSessionRelation:    newAPIPolicyRootSessionRelationRelated,
			RootSessionFingerprint: rootFingerprint,
			ThreadSource:           "subagent",
			RequestKind:            "turn",
			SubagentKind:           "future_internal_kind",
		},
	})
	body := []byte(`{"model":"future-review-model","input":"changed internal payload"}`)
	identity := handler.resolveRequestSessionIdentityForContext(c, body)
	if !passiveInternalRequestAuthorized(c) || !identity.relatedToRoot || identity.affinityID != "newapi-root-session:"+rootFingerprint {
		t.Fatalf("verified NewAPI Guardian classification failed: identity=%+v authorized=%v", identity, passiveInternalRequestAuthorized(c))
	}
	key := capacityAwareSessionAffinityKey(identity, 7)
	filter := handler.applyPassiveInternalModelRouting(c, "future-review-model", "future-review-model", identity, key, true, accountFilterForModel("future-review-model"))
	selected, _ := store.NextForSessionWithFilter(key, 7, nil, filter)
	if selected != root {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("verified NewAPI Guardian selected %+v, want root account %d", selected, root.DBID)
	}
	store.Release(selected)
}

func TestStandaloneRootlessGuardianRejectsMarkerOnly(t *testing.T) {
	c := promptSessionLimitTestContext("")
	cacheTrustedRequestedModel(c, "codex-auto-review")
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"Reviewed Codex session id: %s"}`, testRootSessionA))
	identity := (&Handler{}).resolveRequestSessionIdentityForContext(c, body)
	if identity.relatedToRoot || identity.affinityID == testRootSessionA {
		t.Fatalf("marker-only request was trusted as Guardian: %+v", identity)
	}
}

func TestPassiveInternalModelRoutingDoesNotFallbackWhenDisabledOrRootUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		unbind  bool
	}{
		{name: "switch disabled", enabled: false},
		{name: "root unavailable", enabled: true, unbind: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, root, other, rootFingerprint := passiveInternalModelTestHandler(tc.enabled)
			ctx := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("unavailable-leaf"), rootFingerprint)
			raw, _ := ctx.Get(newAPIPolicyMetaContextKey)
			policy := raw.(verifiedNewAPIPolicyContext)
			policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
			policy.Meta.RequestedModel = "gpt-5.6-luna"
			ctx.Set(newAPIPolicyMetaContextKey, policy)
			body := []byte(`{"model":"gpt-5.6-luna","input":"review"}`)
			identity := handler.resolveRequestSessionIdentityForContext(ctx, body)
			setPassiveInternalAuthorization(ctx, newAPIPassiveFeatureGuardianApproval, true)
			key := capacityAwareSessionAffinityKey(identity, 7)
			if tc.unbind {
				rootKey, related := auth.RelatedSessionRootKey(key)
				if !related {
					t.Fatal("expected related affinity key")
				}
				handler.store.UnbindSessionAffinity(rootKey, root.DBID)
				handler.store.RemoveAccountSession(root.DBID, rootKey)
			}
			filter := handler.applyPassiveInternalModelRouting(ctx, "gpt-5.6-luna", "gpt-5.6-luna", identity, key, true, accountFilterForModel("gpt-5.6-luna"))
			if filter(root) || filter(other) {
				t.Fatal("passive request was allowed without both the switch and root binding")
			}
		})
	}
}

func TestFieldClassifiedInternalRequestIgnoresModelCooldownAndNeverEscapesRoot(t *testing.T) {
	handler, root, other, rootFingerprint := passiveInternalModelTestHandler(true)
	ctx := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("busy-leaf"), rootFingerprint)
	raw, _ := ctx.Get(newAPIPolicyMetaContextKey)
	policy := raw.(verifiedNewAPIPolicyContext)
	policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	policy.Meta.RequestedModel = "gpt-5.6-luna"
	ctx.Set(newAPIPolicyMetaContextKey, policy)
	body := []byte(`{"model":"gpt-5.6-luna","input":"review"}`)
	identity := handler.resolveRequestSessionIdentityForContext(ctx, body)
	setPassiveInternalAuthorization(ctx, newAPIPassiveFeatureGuardianApproval, true)
	key := capacityAwareSessionAffinityKey(identity, 7)
	filter := handler.applyPassiveInternalModelRouting(ctx, "gpt-5.6-luna", "gpt-5.6-luna", identity, key, true, accountFilterForModel("gpt-5.6-luna"))

	root.SetModelCooldownUntil("gpt-5.6-luna", "test", time.Now().Add(time.Minute))
	filter = handler.withRequestModelCooldownFilter(ctx, "gpt-5.6-luna", filter)
	selected, _ := handler.store.NextForSessionWithFilter(key, 7, nil, filter)
	if selected != root {
		if selected != nil {
			handler.store.Release(selected)
		}
		t.Fatalf("field-classified request selected %+v, want cooled root %d and never account %d", selected, root.DBID, other.DBID)
	}
	handler.store.Release(selected)
}
