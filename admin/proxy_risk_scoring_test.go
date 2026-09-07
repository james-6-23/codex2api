package admin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestProxyRiskScoringProfileAPIKeepsSecretsMaskedAndSupportsLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "proxy-risk-api.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &Handler{db: db}
	router := gin.New()
	router.GET("/profiles", h.ListProxyRiskScoringProfiles)
	router.POST("/profiles", h.CreateProxyRiskScoringProfile)
	router.PATCH("/profiles/:profile_id", h.UpdateProxyRiskScoringProfile)
	router.DELETE("/profiles/:profile_id", h.DeleteProxyRiskScoringProfile)

	create := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/profiles", strings.NewReader(`{"name":"primary","scamalytics_host":"api11.scamalytics.com","scamalytics_user":"source-user","scamalytics_key":"source-key","daily_check_limit":7,"credit_reserve":3}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(create, req)
	if create.Code != http.StatusCreated || strings.Contains(create.Body.String(), "source-key") || strings.Contains(create.Body.String(), "base_url") || strings.Contains(create.Body.String(), "access_token") {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var created struct {
		ID            int64 `json:"id"`
		KeyConfigured bool  `json:"scamalytics_key_configured"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ID <= 0 || !created.KeyConfigured {
		t.Fatalf("created profile=%s err=%v", create.Body.String(), err)
	}

	patchRecorder := httptest.NewRecorder()
	patchReq := httptest.NewRequest(http.MethodPatch, "/profiles/"+strconv.FormatInt(created.ID, 10), strings.NewReader(`{"daily_check_limit":9}`))
	patchReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(patchRecorder, patchReq)
	if patchRecorder.Code != http.StatusOK || strings.Contains(patchRecorder.Body.String(), "source-key") || strings.Contains(patchRecorder.Body.String(), "base_url") {
		t.Fatalf("patch status=%d body=%s", patchRecorder.Code, patchRecorder.Body.String())
	}
	profile, err := db.GetProxyRiskScoringProfile(context.Background(), created.ID)
	if err != nil || profile.ScamalyticsUser != "source-user" || profile.ScamalyticsKey != "source-key" || profile.DailyCheckLimit != 9 {
		t.Fatalf("patched profile=%+v err=%v", profile, err)
	}

	deleteRecorder := httptest.NewRecorder()
	router.ServeHTTP(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/profiles/"+strconv.FormatInt(created.ID, 10), nil))
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
}

func TestResolveProxyRiskScoringIPRequiresExplicitHostnameResolution(t *testing.T) {
	if got, err := resolveProxyRiskScoringIP(context.Background(), "http://8.8.8.8:8080", false, false, nil); err != nil || got != "8.8.8.8" {
		t.Fatalf("literal IPv4 = %q err=%v", got, err)
	}
	if _, err := resolveProxyRiskScoringIP(context.Background(), "http://proxy.example:8080", false, false, nil); err == nil {
		t.Fatal("hostname should require explicit resolution")
	}
	if _, err := resolveProxyRiskScoringIP(context.Background(), "http://127.0.0.1:8080", false, false, nil); err == nil {
		t.Fatal("loopback target should be rejected")
	}
}

func TestProxyRiskScoringProfileRejectsUnsafeDocumentationLinks(t *testing.T) {
	_, err := mergeProxyRiskScoringProfile(database.ProxyRiskScoringProfile{}, proxyRiskScoringProfileRequest{
		DocsURL: stringPtr("javascript:alert(1)"),
	})
	if err == nil {
		t.Fatal("unsafe documentation link should be rejected")
	}
}

func stringPtr(value string) *string { return &value }

func TestResolveProxyRiskScoringIPRejectsPrivateResolvedAddress(t *testing.T) {
	lookup := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.7")}, nil }
	if _, err := resolveProxyRiskScoringIP(context.Background(), "http://proxy.example:8080", true, false, lookup); err == nil {
		t.Fatal("private resolved address should be rejected")
	}
}

func TestParseProxyRiskScoringResponseExtractsReferenceFields(t *testing.T) {
	body := []byte(`{"scamalytics":{"scamalytics_score":78,"scamalytics_risk":"high","scamalytics_proxy":{"is_vpn":true,"is_datacenter":true},"credits":{"remaining":120,"used":30}},"external_datasources":{"ip2proxy_lite":{"proxy_type":"DCH","ip_country_code":"DE"},"firehol":{"is_proxy":true}}}`)
	result, credits, err := parseProxyRiskScoringResponse(body, 42)
	if err != nil {
		t.Fatal(err)
	}
	if result.Score == nil || *result.Score != 78 || result.RiskLevel != "high" || !result.IsVPN || !result.IsDatacenter || result.ProxyType != "DCH" || result.Country != "DE" || result.LatencyMS != 42 {
		t.Fatalf("parsed result = %+v", result)
	}
	if credits == nil || credits.Remaining == nil || *credits.Remaining != 120 || credits.Used == nil || *credits.Used != 30 {
		t.Fatalf("parsed credits = %+v", credits)
	}
}

func TestParseProxyRiskScoringResponseAcceptsIntegralFloatScore(t *testing.T) {
	result, _, err := parseProxyRiskScoringResponse([]byte(`{"score":78.0,"risk":"medium"}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Score == nil || *result.Score != 78 {
		t.Fatalf("parsed float score = %+v", result.Score)
	}
}

func TestParseProxyRiskScoringResponseRedactsFeatureSecrets(t *testing.T) {
	result, _, err := parseProxyRiskScoringResponse([]byte(`{"score":12,"external_datasources":{"provider_key":"secret-value","name":"safe"}}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.FeaturesJSON, "secret-value") || !strings.Contains(result.FeaturesJSON, "[redacted]") {
		t.Fatalf("feature JSON leaked a secret: %s", result.FeaturesJSON)
	}
}

func TestParseProxyRiskScoringResponseRejectsNestedProviderError(t *testing.T) {
	_, _, err := parseProxyRiskScoringResponse([]byte(`{"scamalytics":{"error":"invalid API key"}}`), 1)
	if err == nil || !strings.Contains(err.Error(), "invalid API key") {
		t.Fatalf("nested provider error = %v", err)
	}
}

func TestProxyRiskScoringClientBuildsDirectScamalyticsV3URL(t *testing.T) {
	var gotURL string
	client := newProxyRiskScoringClient(database.ProxyRiskScoringProfile{ScamalyticsHost: "api11.scamalytics.com", ScamalyticsUser: "source-user", ScamalyticsKey: "source-key"})
	client.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"score":1,"risk":"low"}`)), Header: make(http.Header), Request: req}, nil
	})}
	if _, _, err := client.checkIP(context.Background(), "8.8.8.8"); err != nil {
		t.Fatal(err)
	}
	want := "https://api11.scamalytics.com/v3/source-user/?key=source-key&ip=8.8.8.8"
	if gotURL != want {
		t.Fatalf("direct Scamalytics URL = %q, want %q", gotURL, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }
