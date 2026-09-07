package admin

import (
	"testing"

	"github.com/codex2api/database"
)

func TestProxyRiskScoringJobItemsIncrementalCursor(t *testing.T) {
	job := &proxyRiskScoringJob{ID: "j1", Status: "running"}
	job.mu.Lock()
	job.appendItem(proxyRiskScoringJobItem{ProxyID: 1, Label: "a:1", Status: "success"})
	job.appendItem(proxyRiskScoringJobItem{ProxyID: 2, Label: "b:2", Status: "error", Error: "boom"})
	job.Current = "c:3"
	job.mu.Unlock()

	all := job.snapshotAfter(0)
	if len(all.Items) != 2 || all.LastSeq != 2 || all.Items[0].Seq != 1 || all.Items[1].Seq != 2 || all.Current != "c:3" {
		t.Fatalf("snapshotAfter(0) = %+v", all)
	}
	inc := job.snapshotAfter(1)
	if len(inc.Items) != 1 || inc.Items[0].ProxyID != 2 || inc.LastSeq != 2 {
		t.Fatalf("snapshotAfter(1) = %+v", inc)
	}
	if none := job.snapshotAfter(2); len(none.Items) != 0 {
		t.Fatalf("snapshotAfter(2) should be empty: %+v", none)
	}
	if none := job.snapshotAfter(0); none.Items == nil {
		t.Fatalf("items must serialize as [] not null")
	}
}

func TestProxyRiskScoringLabelHidesCredentials(t *testing.T) {
	if got := proxyRiskScoringLabel(&database.ProxyRow{ID: 7, URL: "http://user:secret@1.2.3.4:8080"}); got != "1.2.3.4:8080" {
		t.Fatalf("label = %q", got)
	}
	if got := proxyRiskScoringLabel(&database.ProxyRow{ID: 7, URL: "http://1.2.3.4:8080", Label: " 香港-01 "}); got != "香港-01" {
		t.Fatalf("label = %q", got)
	}
	if got := proxyRiskScoringLabel(&database.ProxyRow{ID: 7, URL: "::bad"}); got != "#7" {
		t.Fatalf("label = %q", got)
	}
}
