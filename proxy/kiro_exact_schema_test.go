package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKiroExactNumericSchema(t *testing.T) {
	for _, test := range []struct{ name, schema, input, want string }{
		{"maximum", `{"type":"array","items":{"maximum":9007199254740992}}`, `first [9007199254740992] then [9007199254740993]`, `[9007199254740992]`},
		{"minimum", `{"type":"array","items":{"minimum":9007199254740993}}`, `first [9007199254740993] then [9007199254740992]`, `[9007199254740993]`},
		{"exclusive", `{"type":"array","items":{"exclusiveMinimum":9007199254740992}}`, `first [9007199254740993] then [9007199254740992]`, `[9007199254740993]`},
		{"unique", `{"type":"array","uniqueItems":true}`, `first [1,2] then [{"id":9007199254740992},{"id":9007199254740993}]`, `[{"id":9007199254740992},{"id":9007199254740993}]`},
		{"nested const", `{"type":"object","const":{"id":9007199254740993}}`, `first {"id":9007199254740993} then {"id":9007199254740992}`, `{"id":9007199254740993}`},
		{"nested enum", `{"enum":[{"id":9007199254740993}]}`, `first {"id":9007199254740993} then {"id":9007199254740992}`, `{"id":9007199254740993}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			inbound := &kiroInboundRequest{OutputConfig: &kiroInboundOutputConfig{Format: json.RawMessage(`{"type":"json_schema","schema":` + test.schema + `}`)}}
			schema := kiroInjectStructuredInstruction(inbound)
			if result, ok := kiroExtractConformingJSON(test.input, schema); !ok || result != test.want {
				t.Fatalf("got %q, %v; want %s", result, ok, test.want)
			}
		})
	}
}

func TestKiroExactNumericEquality(t *testing.T) {
	for _, pair := range [][2]string{{"1", "1.0"}, {"-0", "0.000"}, {"10e-1", "1"}, {"1e1000000000", "10e999999999"}} {
		if !kiroJSONEqual(map[string]any{"nested": []any{json.Number(pair[0])}}, map[string]any{"nested": []any{json.Number(pair[1])}}) {
			t.Errorf("%s and %s must be equal", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]any{{json.Number("9007199254740992"), json.Number("9007199254740993")}, {int64(9007199254740992), int64(9007199254740993)}, {json.Number("1"), "1"}} {
		if kiroJSONEqual(pair[0], pair[1]) {
			t.Errorf("%v and %v must be distinct", pair[0], pair[1])
		}
	}
	if kiroTypeMatches("integer", json.Number("9007199254740992.5")) {
		t.Fatal("a rounded decimal is not an integer")
	}
	if _, ok := kiroExactNumber(json.Number("1e1000000000")); ok {
		t.Fatal("unbounded exponent must not allocate an expanded rational")
	}
	if len(kiroCanonicalJSON(json.Number("1e"+strings.Repeat("9", 100)))) > 200 {
		t.Fatal("canonicalization must not expand exponent-sized memory")
	}
}
