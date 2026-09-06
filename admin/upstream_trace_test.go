package admin

import (
	"encoding/json"
	"testing"

	"github.com/codex2api/auth"
)

func TestParseUpstreamRequestIDHeader(t *testing.T) {
	for _, value := range []string{"", "Vendor-Trace", "  X-Request-ID  "} {
		raw, _ := json.Marshal(value)
		update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{UpstreamRequestIDHeader: raw})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := update.CredentialUpdates[auth.UpstreamRequestIDHeaderCredentialKey]; !ok {
			t.Fatal("header update not persisted")
		}
	}
	for _, value := range []string{"Set-Cookie", "Authorization", "bad header", "X-Trace\r\nInjected"} {
		raw, _ := json.Marshal(value)
		if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{UpstreamRequestIDHeader: raw}); err == nil {
			t.Fatalf("accepted invalid trace header %q", value)
		}
	}
}
