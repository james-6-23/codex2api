package security

import (
	"strings"
	"testing"
)

func TestReviewProxyLabelMasksEmbeddedCredentials(t *testing.T) {
	for _, label := range []string{
		"us-1 http://user:test-password@proxy.example:8080",
		"fallback socks5://user:test-password@proxy.example:1080 enabled",
		"https://user:test-password@proxy.example",
	} {
		if masked := MaskSensitiveData(label); strings.Contains(masked, "test-password") {
			t.Fatalf("proxy label retained credentials: %s", masked)
		}
	}
}
