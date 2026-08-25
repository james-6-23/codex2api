package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

const (
	testRootSessionA = "01a031a2-043b-7f42-afa6-ce5491d9be64"
	testRootSessionB = "02b142b3-154c-7043-b0b7-df6502eace75"
	testLeafSessionA = "01a031a2-ca1e-7063-8ba7-f140c182c629"
	testLeafSessionB = "01a031a4-c842-7061-bf7b-4a9aabc6e7a4"
	testIntermediate = "01a031b0-8317-7b21-9297-85bbd886eb9e"
)

func nativeSessionHeaders(root, leaf string, sequence int) http.Header {
	headers := http.Header{}
	headers.Set("Session-Id", root)
	headers.Set("Thread-Id", leaf)
	headers.Set("X-Client-Request-Id", leaf)
	headers.Set("X-Codex-Window-Id", leaf+":"+strconv.Itoa(sequence))
	if root != leaf {
		headers.Set("X-Codex-Parent-Thread-Id", root)
	}
	return headers
}

func TestResolveRequestRootSessionIdentityCollapsesNativeGuardianAndSubagents(t *testing.T) {
	for _, test := range []struct {
		name string
		leaf string
		seq  int
	}{
		{name: "main", leaf: testRootSessionA, seq: 0},
		{name: "guardian", leaf: testLeafSessionA, seq: 4},
		{name: "second subagent", leaf: testLeafSessionB, seq: 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := resolveRequestRootSessionIdentity(nativeSessionHeaders(testRootSessionA, test.leaf, test.seq), nil)
			if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
				t.Fatalf("identity = %+v, want stable root %q", identity, testRootSessionA)
			}
			if identity.related != (test.leaf != testRootSessionA) {
				t.Fatalf("identity related=%t for leaf %q", identity.related, test.leaf)
			}
		})
	}
}

func TestResolveRequestRootSessionIdentityPreservesUnknownRelatedSource(t *testing.T) {
	headers := http.Header{}
	headers.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testLeafSessionA+`","window_id":"`+testLeafSessionA+`:3","forked_from_thread_id":"`+testRootSessionA+`","thread_source":"future_new_source","request_kind":"future_task","subagent_kind":"reviewer"}`)

	identity := resolveRequestRootSessionIdentity(headers, nil)
	if !identity.stable || identity.conflict || !identity.related || identity.sessionID != testRootSessionA {
		t.Fatalf("related identity = %+v", identity)
	}
	if identity.threadSource != "future_new_source" || identity.requestKind != "future_task" || identity.subagentKind != "reviewer" {
		t.Fatalf("source classification was not preserved: %+v", identity)
	}
}

func TestSignedRelatedRequestUsesRootAffinityWithNonCountingMarker(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler()
	rootFingerprint := promptSessionTestFingerprint("shared-root-affinity")
	mainContext := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("main-leaf-affinity"), rootFingerprint)
	mainValue, _ := mainContext.Get(newAPIPolicyMetaContextKey)
	mainPolicy := mainValue.(verifiedNewAPIPolicyContext)
	mainPolicy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRoot
	mainContext.Set(newAPIPolicyMetaContextKey, mainPolicy)

	relatedContext := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("guardian-leaf-affinity"), rootFingerprint)
	relatedValue, _ := relatedContext.Get(newAPIPolicyMetaContextKey)
	relatedPolicy := relatedValue.(verifiedNewAPIPolicyContext)
	relatedPolicy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	relatedPolicy.Meta.ThreadSource = "subagent"
	relatedPolicy.Meta.SubagentKind = "guardian"
	relatedContext.Set(newAPIPolicyMetaContextKey, relatedPolicy)

	body := []byte(`{"model":"gpt-5.6-luna","input":"review"}`)
	mainIdentity := handler.resolveRequestSessionIdentityForContext(mainContext, body)
	relatedIdentity := handler.resolveRequestSessionIdentityForContext(relatedContext, body)
	mainKey := capacityAwareSessionAffinityKey(mainIdentity, 7)
	relatedKey := capacityAwareSessionAffinityKey(relatedIdentity, 7)
	if mainKey != "newapi-root-session:"+rootFingerprint+"::api-key:7" {
		t.Fatalf("main affinity key = %q", mainKey)
	}
	if relatedRoot, ok := auth.RelatedSessionRootKey(relatedKey); !ok || relatedRoot != mainKey {
		t.Fatalf("related affinity key = %q, want authenticated marker for %q", relatedKey, mainKey)
	}
	if !relatedIdentity.relatedToRoot || relatedIdentity.relatedSource.ThreadSource != "subagent" || relatedIdentity.relatedSource.SubagentKind != "guardian" || relatedIdentity.relatedRequestID == "" {
		t.Fatalf("related routing identity = %+v", relatedIdentity)
	}
}

func TestResolveRequestRootSessionIdentityCollapsesNestedGuardianToMainTask(t *testing.T) {
	// This mirrors a real Codex main -> subagent -> Guardian shape: the stable
	// session remains the main task, while parent_thread_id names only the
	// immediate subagent. It must never become the accounting root.
	headers := nativeSessionHeaders(testRootSessionA, testLeafSessionA, 18)
	headers.Set("X-Codex-Parent-Thread-Id", testIntermediate)
	headers.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testLeafSessionA+`","window_id":"`+testLeafSessionA+`:18","parent_thread_id":"`+testIntermediate+`"}`)

	identity := resolveRequestRootSessionIdentity(headers, nil)
	if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
		t.Fatalf("nested Guardian identity = %+v, want main root %q", identity, testRootSessionA)
	}
}

func TestResolveRequestRootSessionIdentityNeverPromotesImmediateParent(t *testing.T) {
	headers := nativeSessionHeaders(testRootSessionA, testLeafSessionA, 19)
	headers.Set("X-Codex-Parent-Thread-Id", testIntermediate)

	identity := resolveRequestRootSessionIdentity(headers, nil)
	if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
		t.Fatalf("parent-only nested identity = %+v, want Session-Id root %q", identity, testRootSessionA)
	}
}

func TestResolveRequestRootSessionIdentityRequiresCoherentRootLeafRelation(t *testing.T) {
	t.Run("session plus parent remains weak", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("Session-Id", testRootSessionA)
		headers.Set("X-Codex-Parent-Thread-Id", testIntermediate)
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if !identity.stable || identity.conflict || identity.nativeRoot || identity.sessionID != testRootSessionA {
			t.Fatalf("Session-Id plus only parent identity = %+v", identity)
		}
	})

	t.Run("unrelated header root and leaf", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("Session-Id", testRootSessionA)
		headers.Set("Thread-Id", testLeafSessionA)
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if identity.stable || !identity.conflict || identity.nativeRoot {
			t.Fatalf("unrelated root and leaf were accepted: %+v", identity)
		}
	})

	t.Run("metadata root without leaf", func(t *testing.T) {
		headers := http.Header{}
		headers.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","parent_thread_id":"`+testIntermediate+`","subagent_kind":"guardian"}`)
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if identity.stable || !identity.conflict || identity.nativeRoot {
			t.Fatalf("metadata root without leaf was accepted: %+v", identity)
		}
	})

	t.Run("subagent marker corroborates child", func(t *testing.T) {
		headers := http.Header{}
		headers.Set("Session-Id", testRootSessionA)
		headers.Set("Thread-Id", testLeafSessionA)
		headers.Set("X-OpenAI-Subagent", "guardian")
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if !identity.stable || identity.conflict || !identity.nativeRoot || identity.sessionID != testRootSessionA {
			t.Fatalf("corroborated child identity = %+v", identity)
		}
	})
}

func TestResolveRequestRootSessionIdentityReadsTurnMetadataOnly(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{
			name: "main task",
			raw:  `{"session_id":"` + testRootSessionA + `","thread_id":"` + testRootSessionA + `","window_id":"` + testRootSessionA + `:0"}`,
		},
		{
			name: "guardian",
			raw:  `{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","window_id":"` + testLeafSessionA + `:18","parent_thread_id":"` + testRootSessionA + `"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set(codexTurnMetadataHeader, test.raw)
			identity := resolveRequestRootSessionIdentity(headers, nil)
			if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
				t.Fatalf("metadata-only identity = %+v, want root %q", identity, testRootSessionA)
			}
		})
	}
}

func TestResolveRequestRootSessionIdentityIgnoresNullTurnMetadataHeader(t *testing.T) {
	headers := nativeSessionHeaders(testRootSessionA, testLeafSessionA, 6)
	headers.Set(codexTurnMetadataHeader, "null")
	identity := resolveRequestRootSessionIdentity(headers, nil)
	if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
		t.Fatalf("null turn metadata changed native identity: %+v", identity)
	}
}

func TestResolveRequestRootSessionIdentityTreatsOptionalJSONNullAsAbsent(t *testing.T) {
	body := []byte(`{"client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x-codex-window-id":"` + testLeafSessionA + `:6","x-codex-parent-thread-id":"` + testRootSessionA + `","client_request_id":null,"subagent_kind":null,"x-codex-turn-metadata":null}}`)
	identity := resolveRequestRootSessionIdentity(nil, body)
	if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
		t.Fatalf("optional JSON null changed native identity: %+v", identity)
	}
}

func TestResolveRequestRootSessionIdentityNullMetadataDoesNotUpgradeOpaqueSession(t *testing.T) {
	headers := http.Header{}
	headers.Set("Session-Id", "opaque-sdk-session")
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":null,"client_metadata":null,"thread_id":null,"subagent_kind":null}}`)
	identity := resolveRequestRootSessionIdentity(headers, body)
	if !identity.stable || identity.conflict || identity.nativeRoot || identity.sessionID != "opaque-sdk-session" {
		t.Fatalf("null metadata upgraded opaque session into a native graph: %+v", identity)
	}
}

func TestResolveRequestRootSessionIdentityCanonicalizesEquivalentUUIDForms(t *testing.T) {
	rootCompact := strings.ReplaceAll(testRootSessionA, "-", "")
	leafCompact := strings.ReplaceAll(testLeafSessionA, "-", "")
	headers := http.Header{}
	headers.Set("Session-Id", "urn:uuid:"+testRootSessionA)
	headers.Set("Thread-Id", testLeafSessionA)
	headers.Set("X-Client-Request-Id", leafCompact)
	headers.Set("X-Codex-Window-Id", leafCompact+":3")
	headers.Set("X-Codex-Parent-Thread-Id", rootCompact)
	headers.Set(codexTurnMetadataHeader, `{"session_id":"`+rootCompact+`","thread_id":"`+testLeafSessionA+`","window_id":"`+leafCompact+`:3","parent_thread_id":"`+testRootSessionA+`"}`)

	identity := resolveRequestRootSessionIdentity(headers, nil)
	if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
		t.Fatalf("equivalent UUID forms were treated as conflicting: %+v", identity)
	}
}

func TestResolveRequestRootSessionIdentityRejectsUncorroboratedMetadataChild(t *testing.T) {
	headers := http.Header{}
	headers.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testLeafSessionA+`","window_id":"`+testLeafSessionA+`:18"}`)
	identity := resolveRequestRootSessionIdentity(headers, nil)
	if identity.stable || !identity.conflict || identity.sessionID != "" {
		t.Fatalf("uncorroborated metadata child was merged: %+v", identity)
	}
}

func TestResolveRequestRootSessionIdentityReadsBodyAndEmbeddedMetadata(t *testing.T) {
	t.Run("client metadata only", func(t *testing.T) {
		body := []byte(`{"client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x-codex-window-id":"` + testLeafSessionA + `:2","x-codex-parent-thread-id":"` + testRootSessionA + `"}}`)
		identity := resolveRequestRootSessionIdentity(nil, body)
		if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
			t.Fatalf("body-only identity = %+v, want root %q", identity, testRootSessionA)
		}
	})

	t.Run("embedded turn metadata", func(t *testing.T) {
		body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"` + testRootSessionA + `\",\"thread_id\":\"` + testLeafSessionA + `\",\"window_id\":\"` + testLeafSessionA + `:5\",\"parent_thread_id\":\"` + testRootSessionA + `\"}"}}`)
		identity := resolveRequestRootSessionIdentity(nil, body)
		if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
			t.Fatalf("embedded identity = %+v, want root %q", identity, testRootSessionA)
		}
	})

	t.Run("string encoded client metadata", func(t *testing.T) {
		metadata := `{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x-codex-window-id":"` + testLeafSessionA + `:6","x-codex-parent-thread-id":"` + testRootSessionA + `"}`
		encoded, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"client_metadata":` + string(encoded) + `}`)
		identity := resolveRequestRootSessionIdentity(nil, body)
		if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
			t.Fatalf("string metadata identity = %+v, want root %q", identity, testRootSessionA)
		}
	})

	t.Run("multiply encoded client metadata", func(t *testing.T) {
		metadata := `{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x-codex-window-id":"` + testLeafSessionA + `:11","x-codex-parent-thread-id":"` + testRootSessionA + `"}`
		once, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		twice, err := json.Marshal(string(once))
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(`{"client_metadata":` + string(twice) + `}`)
		identity := resolveRequestRootSessionIdentity(nil, body)
		if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
			t.Fatalf("multiply encoded identity = %+v, want root %q", identity, testRootSessionA)
		}
	})

	t.Run("nested client metadata", func(t *testing.T) {
		body := []byte(`{"client_metadata":{"client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x-codex-window-id":"` + testLeafSessionA + `:12","x-codex-parent-thread-id":"` + testRootSessionA + `"}}}`)
		identity := resolveRequestRootSessionIdentity(nil, body)
		if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
			t.Fatalf("nested metadata identity = %+v, want root %q", identity, testRootSessionA)
		}
	})

	t.Run("underscore aliases", func(t *testing.T) {
		body := []byte(`{"client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x_client_request_id":"` + testLeafSessionA + `","x_codex_window_id":"` + testLeafSessionA + `:15","x_codex_parent_thread_id":"` + testIntermediate + `","x_openai_subagent":"guardian"}}`)
		identity := resolveRequestRootSessionIdentity(nil, body)
		if !identity.stable || identity.conflict || !identity.nativeRoot || identity.sessionID != testRootSessionA {
			t.Fatalf("underscore aliases identity = %+v, want root %q", identity, testRootSessionA)
		}
	})

	t.Run("underscore embedded turn metadata", func(t *testing.T) {
		body := []byte(`{"client_metadata":{"x_codex_turn_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","window_id":"` + testLeafSessionA + `:16","parent_thread_id":"` + testIntermediate + `","subagent_kind":"guardian"}}}`)
		identity := resolveRequestRootSessionIdentity(nil, body)
		if !identity.stable || identity.conflict || !identity.nativeRoot || identity.sessionID != testRootSessionA {
			t.Fatalf("underscore embedded identity = %+v, want root %q", identity, testRootSessionA)
		}
	})
}

func TestResolveRequestRootSessionIdentityReadsResponseCreateEnvelopes(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "responses websocket v2",
			body: `{"type":"response.create","client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x-codex-window-id":"` + testLeafSessionA + `:7","x-codex-parent-thread-id":"` + testRootSessionA + `"}}`,
		},
		{
			name: "realtime nested response",
			body: `{"type":"response.create","response":{"client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x-codex-window-id":"` + testLeafSessionA + `:9","x-codex-parent-thread-id":"` + testRootSessionA + `"}}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := resolveRequestRootSessionIdentity(nil, []byte(test.body))
			if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
				t.Fatalf("WS identity = %+v, want root %q", identity, testRootSessionA)
			}
		})
	}
}

func TestResolveRequestRootSessionIdentityReadsStringEncodedWebSocketMetadata(t *testing.T) {
	metadata := `{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionA + `","x-codex-window-id":"` + testLeafSessionA + `:14","x-codex-parent-thread-id":"` + testRootSessionA + `"}`
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":"response.create","client_metadata":` + string(encoded) + `}`)
	identity := resolveRequestRootSessionIdentity(nil, body)
	if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
		t.Fatalf("string WS metadata identity = %+v, want root %q", identity, testRootSessionA)
	}
}

func TestResolveRequestRootSessionIdentityCrossValidatesCarriers(t *testing.T) {
	t.Run("matching header and turn metadata", func(t *testing.T) {
		headers := nativeSessionHeaders(testRootSessionA, testLeafSessionA, 3)
		headers.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testLeafSessionA+`","window_id":"`+testLeafSessionA+`:3","parent_thread_id":"`+testRootSessionA+`"}`)
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
			t.Fatalf("matching carriers = %+v", identity)
		}
	})

	t.Run("root conflict", func(t *testing.T) {
		headers := nativeSessionHeaders(testRootSessionA, testLeafSessionA, 3)
		headers.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionB+`","thread_id":"`+testLeafSessionA+`","window_id":"`+testLeafSessionA+`:3"}`)
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if identity.stable || !identity.conflict || identity.sessionID != "" {
			t.Fatalf("conflicting root was accepted: %+v", identity)
		}
	})

	t.Run("header leaf with corroborated metadata root", func(t *testing.T) {
		headers := nativeSessionHeaders(testLeafSessionA, testLeafSessionA, 3)
		headers.Set("X-Codex-Parent-Thread-Id", testRootSessionA)
		headers.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testLeafSessionA+`","window_id":"`+testLeafSessionA+`:3","parent_thread_id":"`+testRootSessionA+`"}`)
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if !identity.stable || identity.conflict || identity.sessionID != testRootSessionA {
			t.Fatalf("corroborated header leaf did not resolve to metadata root: %+v", identity)
		}
	})

	t.Run("header leaf without corroborated parent", func(t *testing.T) {
		headers := nativeSessionHeaders(testLeafSessionA, testLeafSessionA, 3)
		headers.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testLeafSessionA+`","window_id":"`+testLeafSessionA+`:3"}`)
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if identity.stable || !identity.conflict || identity.sessionID != "" {
			t.Fatalf("uncorroborated header leaf was merged: %+v", identity)
		}
	})

	t.Run("leaf conflict", func(t *testing.T) {
		headers := nativeSessionHeaders(testRootSessionA, testLeafSessionA, 3)
		body := []byte(`{"client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testLeafSessionB + `","x-codex-window-id":"` + testLeafSessionB + `:3","x-codex-parent-thread-id":"` + testRootSessionA + `"}}`)
		identity := resolveRequestRootSessionIdentity(headers, body)
		if identity.stable || !identity.conflict || identity.sessionID != "" {
			t.Fatalf("conflicting leaf was accepted: %+v", identity)
		}
	})

	t.Run("duplicate header conflict", func(t *testing.T) {
		headers := nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
		headers.Add("Session-Id", testRootSessionB)
		identity := resolveRequestRootSessionIdentity(headers, nil)
		if identity.stable || !identity.conflict {
			t.Fatalf("conflicting duplicate Session-Id was accepted: %+v", identity)
		}
	})
}

func TestResolveRequestRootSessionIdentityPreservesLegacyFallbacks(t *testing.T) {
	headers := http.Header{"Session_id": []string{"legacy-session"}}
	identity := resolveRequestRootSessionIdentity(headers, nil)
	if !identity.stable || identity.conflict || identity.sessionID != "legacy-session" {
		t.Fatalf("legacy Session_id = %+v", identity)
	}

	identity = resolveRequestRootSessionIdentity(nil, []byte(`{"prompt_cache_key":"legacy-cache"}`))
	if !identity.stable || identity.conflict || identity.sessionID != "legacy-cache" {
		t.Fatalf("legacy prompt cache = %+v", identity)
	}

	for _, test := range []struct {
		name   string
		header string
	}{
		{name: "generic explicit session", header: "X-Session-ID"},
		{name: "OpenAI explicit session", header: "OpenAI-Session-ID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set(test.header, "explicit-session")
			identity := resolveRequestRootSessionIdentity(headers, nil)
			if !identity.stable || identity.conflict || identity.sessionID != "explicit-session" {
				t.Fatalf("%s identity = %+v", test.header, identity)
			}
			if got := ResolveStableExplicitSessionID(headers, nil); got != "explicit-session" {
				t.Fatalf("ResolveStableExplicitSessionID(%s) = %q", test.header, got)
			}
		})
	}
}

func TestNewAPIRootSessionFingerprintProtocolVector(t *testing.T) {
	const want = "a68d950522466e5efa03ef5a2e9b9314"
	if got := newAPIRootSessionFingerprint("newapi", "42", testRootSessionA); got != want {
		t.Fatalf("root fingerprint = %q, want protocol vector %q", got, want)
	}
}
