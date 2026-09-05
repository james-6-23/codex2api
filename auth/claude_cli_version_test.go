package auth

import "testing"

func TestEffectiveClaudeCLIVersion_NeverBelowBuiltin(t *testing.T) {
	t.Cleanup(func() { SetClaudeSyncedCLIVersion("") })
	cases := map[string]string{
		"":             BuiltinClaudeCLIVersion,
		"garbage":      BuiltinClaudeCLIVersion,
		"2.1.100":      BuiltinClaudeCLIVersion,
		"2.1.258":      BuiltinClaudeCLIVersion,
		"2.1.300":      "2.1.300",
		" v2.1.301 ":   "2.1.301",
		"2.1.300-beta": BuiltinClaudeCLIVersion, // 预发布不高于正式版
	}
	for synced, want := range cases {
		SetClaudeSyncedCLIVersion(synced)
		if got := EffectiveClaudeCLIVersion(); got != want {
			t.Errorf("synced=%q effective=%q want %q", synced, got, want)
		}
	}
}

func TestRewriteClaudeCLIUserAgentVersion(t *testing.T) {
	cases := []struct{ ua, version, want string }{
		{"claude-cli/2.1.219 (external, cli)", "2.1.258", "claude-cli/2.1.258 (external, cli)"},
		{"Claude Code/2.1.1 windows", "2.1.258", "Claude Code/2.1.258 windows"},
		{"curl/8.7.1", "2.1.258", "curl/8.7.1"},
		{"claude-cli/2.1.219 (external, cli)", "bad", ""},
	}
	for _, tc := range cases {
		if got := RewriteClaudeCLIUserAgentVersion(tc.ua, tc.version); got != tc.want {
			t.Errorf("Rewrite(%q,%q)=%q want %q", tc.ua, tc.version, got, tc.want)
		}
	}
}
