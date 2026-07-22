package proxy

import "testing"

func TestResolveGrokPlan(t *testing.T) {
	cents := func(v float64) *float64 { return &v }

	cases := []struct {
		name  string
		limit *float64
		want  string
	}{
		// 仅在 billing 月度配置成功返回时调用，故 nil/0 额度代表免费档。
		{"nil limit is free", nil, "free"},
		{"zero limit is free", cents(0), "free"},
		{"supergrok", cents(grokSuperGrokCents), "SuperGrok"},
		{"supergrok heavy", cents(grokSuperGrokHeavyCents), "SuperGrok Heavy"},
		// 未知非零额度不臆断，保留占位。
		{"unknown paid tier", cents(42_000), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveGrokPlan(tc.limit); got != tc.want {
				t.Fatalf("resolveGrokPlan(%v) = %q, want %q", tc.limit, got, tc.want)
			}
		})
	}
}
