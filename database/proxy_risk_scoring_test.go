package database

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestNormalizeProxyRiskScoringProfileKeepsOperatorLimits(t *testing.T) {
	profile := NormalizeProxyRiskScoringProfile(ProxyRiskScoringProfile{
		Name:              "primary",
		TimeoutSeconds:    19,
		Concurrency:       7,
		RequestDelayMS:    350,
		CacheTTLSeconds:   7200,
		MaxChecksPerJob:   1234,
		DailyCheckLimit:   5678,
		CreditReserve:     42,
		ResolveHostnames:  true,
		AllowForceRefresh: true,
	})
	if profile.ScamalyticsHost != "api11.scamalytics.com" {
		t.Fatalf("default Scamalytics host = %q", profile.ScamalyticsHost)
	}
	if profile.Name != "primary" || profile.TimeoutSeconds != 19 || profile.Concurrency != 7 || profile.RequestDelayMS != 350 ||
		profile.CacheTTLSeconds != 7200 || profile.MaxChecksPerJob != 1234 || profile.DailyCheckLimit != 5678 || profile.CreditReserve != 42 ||
		!profile.ResolveHostnames || !profile.AllowForceRefresh {
		t.Fatalf("operator profile values changed: %+v", profile)
	}
}

func TestProxyRiskScoringProfilePersistenceDoesNotExposeSecrets(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "proxy-risk.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	profile := NormalizeProxyRiskScoringProfile(ProxyRiskScoringProfile{
		Name:            "primary",
		ScamalyticsHost: "api11.scamalytics.com",
		ScamalyticsUser: "source-user",
		ScamalyticsKey:  "scam-key",
		DocsURL:         "https://www.scamalytics.com/",
	})
	id, err := db.CreateProxyRiskScoringProfile(context.Background(), &profile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetProxyRiskScoringProfile(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScamalyticsUser != "source-user" || got.ScamalyticsKey != "scam-key" {
		t.Fatalf("stored profile lost credentials: %+v", got)
	}
	if got.Name != "primary" || got.ScamalyticsHost != profile.ScamalyticsHost {
		t.Fatalf("stored profile = %+v", got)
	}
}

func TestProxyRiskScoreSnapshotRoundTripKeepsNullableScore(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "proxy-risk-snapshot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	proxyID, err := db.InsertProxy(context.Background(), "http://198.51.100.20:8080", "test")
	if err != nil {
		t.Fatal(err)
	}
	profile := ProxyRiskScoringProfile{Name: "primary", ScamalyticsHost: "api11.scamalytics.com", ScamalyticsUser: "source-user", ScamalyticsKey: "source-key"}
	profileID, err := db.CreateProxyRiskScoringProfile(context.Background(), &profile)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &ProxyRiskScoreSnapshot{
		ProxyID:        proxyID,
		ProfileID:      profileID,
		ResolvedIP:     "198.51.100.20",
		RiskLevel:      "low",
		Recommendation: "keep",
		Status:         "success",
		Provider:       "scamalytics",
	}
	if err := db.InsertProxyRiskScoreSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	latest, err := db.ListLatestProxyRiskScores(context.Background(), []int64{proxyID})
	if err != nil {
		t.Fatal(err)
	}
	got := latest[proxyID]
	if got == nil || got.Score != nil || got.ResolvedIP != snapshot.ResolvedIP || got.RiskLevel != "low" {
		t.Fatalf("latest snapshot = %+v", got)
	}
}

func TestProxyRiskScoreSnapshotsConcurrentWritesAvoidSchemaLockChurn(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "proxy-risk-concurrent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	proxyID, err := db.InsertProxy(context.Background(), "http://203.0.113.40:8080", "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := db.CreateProxyRiskScoringProfile(context.Background(), &ProxyRiskScoringProfile{Name: "concurrent", ScamalyticsHost: "api11.scamalytics.com", ScamalyticsUser: "source-user", ScamalyticsKey: "source-key"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- db.InsertProxyRiskScoreSnapshot(context.Background(), &ProxyRiskScoreSnapshot{ProxyID: proxyID, ProfileID: profileID, Provider: "scamalytics", Status: "success"})
		}()
	}
	wg.Wait()
	close(errCh)
	for writeErr := range errCh {
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	items, total, err := db.ListProxyRiskScoreHistory(context.Background(), proxyID, profileID, 1, 20)
	if err != nil || total != 8 || len(items) != 8 {
		t.Fatalf("concurrent snapshot history total=%d items=%d err=%v", total, len(items), err)
	}
}
