package proxy

import (
	"net/http"
	"testing"
)

func TestClaudeSessionHintPreservesNewAPIRootRouting(test *testing.T) {
	for _, state := range []string{newAPIPolicyRootSessionResolved, newAPIPolicyRootSessionUnavailable} {
		test.Run(state, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
			fingerprint := promptSessionTestFingerprint("merge-root")
			metadata := newAPIPolicyMeta{
				RootSessionVersion: 1, RootSessionState: state,
				ThreadSource: "user", RequestKind: "turn",
			}
			if state == newAPIPolicyRootSessionResolved {
				metadata.RootSessionRelation = newAPIPolicyRootSessionRelationRoot
				metadata.RootSessionFingerprint = fingerprint
			} else {
				metadata.ThreadSource = "future_internal_kind"
			}
			ctx, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/messages", body, metadata)
			ctx.Request.Header.Set("X-Claude-Code-Session-Id", "11111111-1111-4111-8111-111111111111")
			handler.primeNewAPIPolicyContext(ctx, body)
			if status, verified := handler.cachedNewAPIPolicyAuditState(ctx); status != "verified" || !verified.MetaVerified {
				test.Fatal("test requires verified NewAPI metadata")
			}
			base := resolveClaudeRequestSessionIdentity(ctx.Request.Header, body)
			if base.affinityID == "" {
				test.Fatal("test requires a usable Claude session hint")
			}
			identity := handler.resolveRequestSessionIdentityWithBase(ctx, body, base)
			if state == newAPIPolicyRootSessionResolved {
				if identity.affinityID != "newapi-root-session:"+fingerprint {
					test.Fatalf("Claude hint replaced the verified NewAPI root: %+v", identity)
				}
			} else if identity.affinityID != "" || !identity.unlinkedFallbackOnly {
				test.Fatalf("Claude hint bypassed the rootless fallback boundary: %+v", identity)
			}
		})
	}
}
