package auth

import (
	"context"
	"testing"

	"github.com/codex2api/database"
)

func TestUpstreamTraceConfigAndProxySnapshots(t *testing.T) {
	store := NewStore(nil, nil, nil)
	rows := []*database.ProxyRow{{ID: 7, URL: "http://secret:password@proxy.invalid", Label: "before", Enabled: true}}
	store.proxyPoolLoader = func(context.Context) ([]*database.ProxyRow, error) { return rows, nil }
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	before := store.ProxyAuditForURL(rows[0].URL)
	rows[0].Label = "after"
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	if before.Name != "before" || store.ProxyAuditForURL(rows[0].URL).Name != "after" || before.ID != 7 {
		t.Fatal("proxy attribution was not a value snapshot")
	}
	account := &Account{DBID: 1}
	store.AddAccount(account)
	store.ApplyAccountUpstreamRequestIDHeader(1, "Vendor-ID")
	if account.GetUpstreamRequestIDHeader() != "Vendor-ID" {
		t.Fatal("trace config not published")
	}
	store.applyPersistentAccountSnapshot(account, &Account{DBID: 1, UpstreamRequestIDHeader: "Other-ID"}, true)
	if account.GetUpstreamRequestIDHeader() != "Other-ID" {
		t.Fatal("outbox snapshot dropped trace configuration")
	}
}
