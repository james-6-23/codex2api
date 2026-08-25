package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

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

func TestPassiveInternalModelRelatedRetryCannotEscapeBusyRoot(t *testing.T) {
	handler, root, other, rootFingerprint := passiveInternalModelTestHandler(true)
	ctx := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("busy-leaf"), rootFingerprint)
	raw, _ := ctx.Get(newAPIPolicyMetaContextKey)
	policy := raw.(verifiedNewAPIPolicyContext)
	policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	policy.Meta.RequestedModel = "gpt-5.6-luna"
	ctx.Set(newAPIPolicyMetaContextKey, policy)
	body := []byte(`{"model":"gpt-5.6-luna","input":"review"}`)
	identity := handler.resolveRequestSessionIdentityForContext(ctx, body)
	key := capacityAwareSessionAffinityKey(identity, 7)
	filter := handler.applyPassiveInternalModelRouting(ctx, "gpt-5.6-luna", "gpt-5.6-luna", identity, key, true, accountFilterForModel("gpt-5.6-luna"))

	root.SetModelCooldownUntil("gpt-5.6-luna", "test", time.Now().Add(time.Minute))
	filter = handler.withModelCooldownFilter("gpt-5.6-luna", filter)
	selected, _ := handler.store.NextForSessionWithFilter(key, 7, nil, filter)
	if selected != nil {
		handler.store.Release(selected)
		t.Fatalf("busy root request escaped to account %d; other account is %d", selected.DBID, other.DBID)
	}
}
