package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAntigravityCatalogRefreshKeepsAllIDsAndLastSuccess(t *testing.T) {
	var status atomic.Int32
	var empty atomic.Bool
	status.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/quota" {
			t.Errorf("catalog refresh called non-catalog endpoint %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		if empty.Load() {
			_, _ = io.WriteString(w, `{"models":{}}`)
			return
		}
		_, _ = io.WriteString(w, `{"models":{"gemini-3.8-flash-tiered":{"quotaInfo":{"remainingFraction":0.9}},"future-model-v4":{"displayName":"Future model"},"gemini-private-future":{"isInternal":true,"quotaInfo":{"remainingFraction":1}}}}`)
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, db, account, id := newAntigravityRefreshTestAccount(t, nil)
	if err := store.RefreshAntigravityCatalog(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	want := []string{"future-model-v4", "gemini-3.8-flash-tiered"}
	if got := account.AntigravityModels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("full catalog = %v", got)
	}
	before, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if before.GetCredential("antigravity_catalog_updated_at") == "" || !strings.Contains(before.GetCredential("antigravity_quota"), "future-model-v4") {
		t.Fatal("successful catalog metadata was not persisted")
	}
	if !strings.Contains(before.GetCredential("antigravity_quota"), "gemini-private-future") {
		t.Fatal("raw internal catalog evidence was discarded")
	}
	for _, failedStatus := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusOK} {
		status.Store(int32(failedStatus))
		empty.Store(failedStatus == http.StatusOK)
		if err := store.RefreshAntigravityCatalog(context.Background(), account); err == nil {
			t.Fatalf("failed or empty catalog falsely succeeded: %d", failedStatus)
		}
		after, err := db.GetAccountByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after.GetCredentialStringSlice("models"), want) || after.GetCredential("antigravity_quota") != before.GetCredential("antigravity_quota") {
			t.Fatal("failed fetch destroyed last successful catalog")
		}
		if after.GetCredential("access_token") != before.GetCredential("access_token") {
			t.Fatal("catalog poll changed OAuth credentials")
		}
	}
}

func TestAntigravityCatalogRefreshDiscardsOldCredentialGeneration(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":{"gemini-stale":{"quotaInfo":{"remainingFraction":1}}}}`)
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, db, account, id := newAntigravityRefreshTestAccount(t, nil)
	row, _ := db.GetAccountByID(context.Background(), id)
	done := make(chan error, 1)
	go func() { done <- store.RefreshAntigravityCatalog(context.Background(), account) }()
	<-entered
	_, applied, err := db.UpdateAccountCredentialsCAS(context.Background(), id, row.CredentialGeneration, map[string]any{"models": []string{"gemini-new-principal"}})
	close(release)
	if err != nil || !applied {
		t.Fatalf("fixture credential change failed: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("stale generation accepted")
	}
	current, _ := db.GetAccountByID(context.Background(), id)
	if got := current.GetCredentialStringSlice("models"); !reflect.DeepEqual(got, []string{"gemini-new-principal"}) {
		t.Fatalf("stale response overwrote newer catalog: %v", got)
	}
}
