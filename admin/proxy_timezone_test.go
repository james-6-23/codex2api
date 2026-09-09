package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProxyTimezoneAdminRoundtrip(test *testing.T) {
	db := newAdminProxyTestDB(test)
	ctx := context.Background()
	proxyURL := "http://timezone.example:8080"
	proxyID, err := db.InsertProxy(ctx, proxyURL, "")
	if err != nil {
		test.Fatal(err)
	}
	store := newAdminProxyTestStore(test, db)
	handler := &Handler{db: db, store: store, proxyProbe: func(context.Context, string, string) proxyProbeResult {
		return parseIPAPIProbeBody([]byte(`{"status":"success","query":"1.2.3.4","timezone":"America/Los_Angeles"}`), 20)
	}}
	router := gin.New()
	router.PATCH("/proxies/:id", handler.UpdateProxy)
	router.POST("/proxies/test", handler.TestProxy)
	for _, sample := range []struct {
		method, path, body, expected string
		status                       int
	}{
		{http.MethodPost, "/proxies/test", fmt.Sprintf(`{"id":%d,"url":%q}`, proxyID, proxyURL), "America/Los_Angeles", 200},
		{http.MethodPatch, fmt.Sprintf("/proxies/%d", proxyID), `{"timezone_override":"Asia/Tokyo"}`, "Asia/Tokyo", 200},
		{http.MethodPost, "/proxies/test", fmt.Sprintf(`{"id":%d,"url":%q}`, proxyID, proxyURL), "Asia/Tokyo", 200},
		{http.MethodPatch, fmt.Sprintf("/proxies/%d", proxyID), `{"timezone_override":"Local"}`, "Asia/Tokyo", 400},
		{http.MethodPatch, fmt.Sprintf("/proxies/%d", proxyID), `{"timezone_override":""}`, "America/Los_Angeles", 200},
	} {
		request := httptest.NewRequest(sample.method, sample.path, strings.NewReader(sample.body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		location := store.ProxyTimezone(proxyURL)
		if recorder.Code != sample.status || location == nil || location.String() != sample.expected {
			test.Fatalf("sample=%+v status=%d location=%v body=%s", sample, recorder.Code, location, recorder.Body.String())
		}
	}
	if result := parseIPAPIProbeBody([]byte(`{"status":"success","query":"1.2.3.4","timezone":"Invalid/Zone"}`), 10); !result.Success || result.Timezone != "" {
		test.Fatalf("invalid timezone failed a working proxy: %+v", result)
	}
	result := proxyProbeResult{Success: true, Conclusive: true, IP: "1.2.3.4", Timezone: "Europe/London"}
	if err := handler.saveProxyTestResult(ctx, proxyID, proxyURL, result); err != nil {
		test.Fatal(err)
	}
	if err := handler.reloadProxyPool(); err != nil {
		test.Fatal(err)
	}
	if location := store.ProxyTimezone(proxyURL); location == nil || location.String() != result.Timezone {
		test.Fatalf("batch save failed: %v", location)
	}
}
