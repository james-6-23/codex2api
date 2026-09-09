package auth

import (
	"context"
	"testing"

	"github.com/codex2api/database"
)

func TestProxyTimezoneReloadAndOverride(test *testing.T) {
	rows := []*database.ProxyRow{{URL: "http://proxy:8080", TestTimezone: "America/Los_Angeles"}}
	store := &Store{proxyPoolLoader: func(context.Context) ([]*database.ProxyRow, error) { return rows, nil }}
	for _, sample := range []struct{ manual, inferred, expected string }{
		{"", "America/Los_Angeles", "America/Los_Angeles"},
		{"Asia/Tokyo", "Europe/London", "Asia/Tokyo"},
		{"", "Europe/London", "Europe/London"},
		{"", "bad", ""},
	} {
		rows[0].TimezoneOverride, rows[0].TestTimezone = sample.manual, sample.inferred
		if err := store.ReloadProxyPool(); err != nil {
			test.Fatal(err)
		}
		actual := ""
		if location := store.ProxyTimezone(rows[0].URL); location != nil {
			actual = location.String()
		}
		if actual != sample.expected || store.ProxyTimezone("http://other:8080") != nil {
			test.Fatalf("sample=%+v actual=%q", sample, actual)
		}
	}
	store.RemoveProxyURLs([]string{rows[0].URL})
	if store.ProxyTimezone(rows[0].URL) != nil {
		test.Fatal("removed proxy retained timezone")
	}
}
