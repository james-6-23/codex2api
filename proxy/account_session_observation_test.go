package proxy

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPopulateAccountSessionObservationDeduplicatesConversation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("Session-Id", "stable-session")
	handler := &Handler{}
	first := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, first)
	if !first.RecordSessionObservation || first.SessionHash == "" {
		t.Fatalf("first session observation was not recorded: %+v", first)
	}
	second := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, second)
	if second.RecordSessionObservation || second.SessionHash != first.SessionHash {
		t.Fatalf("repeat conversation was not deduplicated: first=%+v second=%+v", first, second)
	}
}

func TestPopulateAccountSessionObservationRefreshesLastSeen(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("Session-Id", "stable-session-refresh")
	handler := &Handler{}
	first := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, first)
	key := "9:" + first.SessionHash
	handler.accountSessionObservations[key] = accountSessionObservationCacheEntry{
		RecordedAt: time.Now().Add(-accountSessionObservationRefreshInterval - time.Second),
	}
	second := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, second)
	if !second.RecordSessionObservation || second.ObservedAt.IsZero() {
		t.Fatalf("stale observation was not refreshed: %+v", second)
	}
}
