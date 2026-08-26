package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func projectTitleRoutingTestBody() []byte {
	return []byte(`{
		"model":"gpt-5.6-luna",
		"input":[{"role":"user","content":[{"type":"input_text","text":"You are presented with a user prompt. Provide a short title.\nUser prompt:\nInspect the repository"}]}],
		"text":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}}}}}
	}`)
}

func projectTitleRoutingStringInputTestBody() []byte {
	return []byte(`{
		"model":"gpt-5.6-luna",
		"input":"You are a helpful assistant. You will be presented with a user prompt, and your job is to provide a short title for a task that will be created from that prompt.\nUser prompt:\nInspect the repository",
		"text":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"},"description":{"type":"string"}}}}}
	}`)
}

func projectTitleRoutingTestContext(groupID int64) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contextAPIKeyRow, &database.APIKeyRow{
		ID: 7,
		Limits: database.APIKeyLimits{
			ProjectTitleGroupID: groupID,
		},
	})
	return c
}

func TestProjectTitleRoutingClassifiesConfiguredNativeSystemRequest(t *testing.T) {
	c := projectTitleRoutingTestContext(20)
	identity := requestSessionIdentity{
		relatedSource: auth.AccountSessionRelatedSource{ThreadSource: "system", RequestKind: "turn"},
	}
	if !classifyProjectTitleRequest(c, &identity) {
		t.Fatal("configured native project-title request was not classified")
	}
	if !isProjectTitleRequest(c) || !identity.bypassWindowAccounting {
		t.Fatalf("project-title routing state was incomplete: identity=%+v", identity)
	}
	if got := projectTitleSchedulingAPIKeyID(c, 7); got != -7 {
		t.Fatalf("project-title scheduling key = %d, want -7", got)
	}
}

func TestDirectCodexProjectTitleRequestResolvesAndRoutesWithoutNewAPI(t *testing.T) {
	const titleID = "01a03787-1743-7151-a307-c1c0f1615bb6"
	c := projectTitleRoutingTestContext(20)
	c.Request.Header = nativeSessionHeaders(titleID, titleID, 0)
	c.Request.Header.Set(codexTurnMetadataHeader, `{"session_id":"`+titleID+`","thread_id":"`+titleID+`","window_id":"`+titleID+`:0","thread_source":"system","request_kind":"turn"}`)
	body := projectTitleRoutingStringInputTestBody()

	identity := (&Handler{}).resolveRequestSessionIdentityForContext(c, body)
	if identity.relatedSource.ThreadSource != "system" || !identity.stableIdentity {
		t.Fatalf("direct Codex title metadata was not resolved: %+v", identity)
	}
	if !classifyProjectTitleRequest(c, &identity) {
		t.Fatal("direct Codex2API title request did not enter the configured project-title route")
	}
	if !identity.bypassWindowAccounting || !isProjectTitleRequest(c) {
		t.Fatalf("direct title request did not receive the same accounting behavior: %+v", identity)
	}
}

func TestProjectTitleRoutingDoesNotClassifyDisabledOrOrdinaryUserFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		groupID int64
		source  string
	}{
		{name: "disabled", source: "system"},
		{name: "ordinary user", groupID: 20, source: "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := projectTitleRoutingTestContext(tc.groupID)
			identity := requestSessionIdentity{relatedSource: auth.AccountSessionRelatedSource{ThreadSource: tc.source}}
			if classifyProjectTitleRequest(c, &identity) || isProjectTitleRequest(c) || identity.bypassWindowAccounting {
				t.Fatalf("non-title request entered title route: identity=%+v", identity)
			}
		})
	}
}

func TestProjectTitleRoutingTrustsSignedNewAPIPassiveFeature(t *testing.T) {
	c := projectTitleRoutingTestContext(20)
	c.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
		MetaVerified: true,
		Meta:         newAPIPolicyMeta{PassiveFeature: "system_passive"},
	})
	identity := requestSessionIdentity{}
	if !classifyProjectTitleRequest(c, &identity) {
		t.Fatal("signed project-title feature was rejected after model/prompt drift")
	}
}

func TestProjectTitleRoutingDoesNotSwallowOtherSystemJobs(t *testing.T) {
	direct := projectTitleRoutingTestContext(20)
	directIdentity := requestSessionIdentity{
		bypassWindowAccounting: true,
		relatedSource:          auth.AccountSessionRelatedSource{ThreadSource: "system"},
	}
	if classifyProjectTitleRequest(direct, &directIdentity) {
		t.Fatal("direct ambient system job entered the project-title group")
	}

	signed := projectTitleRoutingTestContext(20)
	signed.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
		MetaVerified: true,
		Meta:         newAPIPolicyMeta{PassiveFeature: newAPIPassiveFeatureRelatedInternal},
	})
	signedIdentity := requestSessionIdentity{relatedSource: auth.AccountSessionRelatedSource{ThreadSource: "system"}}
	if classifyProjectTitleRequest(signed, &signedIdentity) {
		t.Fatal("signed ambient system job entered the project-title group")
	}
}

func TestProjectTitleRoutingUsesSystemFieldIndependentOfPayloadShape(t *testing.T) {
	base := projectTitleRoutingTestBody()
	mutations := map[string][]byte{
		"instructions":          bytes.Replace(base, []byte(`"model":"gpt-5.6-luna",`), []byte(`"model":"gpt-5.6-luna","instructions":"do another task",`), 1),
		"tools":                 bytes.Replace(base, []byte(`"model":"gpt-5.6-luna",`), []byte(`"model":"gpt-5.6-luna","tools":[{"type":"function","name":"shell"}],`), 1),
		"previous response":     bytes.Replace(base, []byte(`"model":"gpt-5.6-luna",`), []byte(`"model":"gpt-5.6-luna","previous_response_id":"resp_other",`), 1),
		"conversation":          bytes.Replace(base, []byte(`"model":"gpt-5.6-luna",`), []byte(`"model":"gpt-5.6-luna","conversation":"conv_other",`), 1),
		"context management":    bytes.Replace(base, []byte(`"model":"gpt-5.6-luna",`), []byte(`"model":"gpt-5.6-luna","context_management":{"type":"compaction"},`), 1),
		"extra schema property": bytes.Replace(base, []byte(`"description":{"type":"string"}`), []byte(`"description":{"type":"string"},"answer":{"type":"string"}`), 1),
		"non-string title":      bytes.Replace(base, []byte(`"title":{"type":"string"}`), []byte(`"title":{"type":"array"}`), 1),
	}
	for name, body := range mutations {
		t.Run(name, func(t *testing.T) {
			c := projectTitleRoutingTestContext(20)
			cacheTrustedRequestedModel(c, "gpt-5.6-luna")
			identity := requestSessionIdentity{relatedSource: auth.AccountSessionRelatedSource{ThreadSource: "system"}}
			if !classifyProjectTitleRequest(c, &identity) || !isProjectTitleRequest(c) || !identity.bypassWindowAccounting {
				t.Fatalf("system field stopped routing after payload drift: identity=%+v", identity)
			}
			handler := newPromptGuardTestHandler(promptGuardTestConfig())
			if decision := handler.evaluatePromptGuard(c, body, body, "/v1/responses", "gpt-5.6-luna", promptfilter.TransportHTTP).Decision; decision.ApplicationPromptKind == "project_title" {
				t.Fatalf("expanded request received project-title Guard downgrade: %+v", decision)
			}
		})
	}
}

func TestProjectTitleRoutingUsesOnlyConfiguredGroupAndBypassesModelVisibility(t *testing.T) {
	c := projectTitleRoutingTestContext(20)
	identity := requestSessionIdentity{relatedSource: auth.AccountSessionRelatedSource{ThreadSource: "system"}}
	if !classifyProjectTitleRequest(c, &identity) {
		t.Fatal("project-title request was not classified")
	}
	titleAccount := &auth.Account{DBID: 1, GroupIDs: []int64{20}, Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	ordinaryAccount := &auth.Account{DBID: 2, GroupIDs: []int64{10}, Models: []string{"gpt-5.6-sol"}, Status: auth.StatusReady}
	filter := applyProjectTitleModelRouting(c, "gpt-5.6-luna", false, accountFilterForModel("gpt-5.6-luna"))
	if !filter(titleAccount) {
		t.Fatal("configured title account was rejected because Luna is hidden from its normal model list")
	}
	if filter(ordinaryAccount) {
		t.Fatal("project-title route escaped into an ordinary account group")
	}
}

func TestResetCodexInternalRequestClassificationClearsWebSocketFrameState(t *testing.T) {
	c := projectTitleRoutingTestContext(20)
	setLocalSessionAccountingBypass(c, true)
	setPassiveInternalAuthorization(c, true)
	c.Set(projectTitleRequestContextKey, projectTitleRequestRoute{GroupID: 20})
	cacheTrustedRequestedModel(c, "codex-auto-review")

	resetCodexInternalRequestClassificationFrame(c)

	if requestSessionAccountingBypass(c) || isProjectTitleRequest(c) {
		t.Fatal("prior WebSocket frame classification survived reset")
	}
	if passiveInternalRequestAuthorized(c) {
		t.Fatal("prior passive internal authorization survived reset")
	}
	if got := trustedRequestedModel(c, "gpt-5.6-sol"); got != "gpt-5.6-sol" {
		t.Fatalf("prior requested model survived reset: %q", got)
	}
}
