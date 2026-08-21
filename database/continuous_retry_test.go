package database

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestContinuousRetryPolicyDefaultsAndNormalization(t *testing.T) {
	defaultPolicy := DefaultContinuousRetryPolicy()
	if defaultPolicy.Enabled {
		t.Fatal("continuous retry must be disabled by default")
	}
	if defaultPolicy.CatchAll {
		t.Fatal("continuous retry catch-all must be disabled by default")
	}
	if defaultPolicy.MaxDurationSeconds != DefaultContinuousRetryMaxDurationSeconds {
		t.Fatalf("default max duration = %d, want %d", defaultPolicy.MaxDurationSeconds, DefaultContinuousRetryMaxDurationSeconds)
	}
	if len(defaultPolicy.Categories) == 0 {
		t.Fatal("default policy should preserve the common transient categories")
	}
	enabledDefault := defaultPolicy
	enabledDefault.Enabled = true
	if enabledDefault.HasCategory(ContinuousRetryCategoryResponseFailed) {
		t.Fatal("default policy must not select every response.failed event")
	}

	got := NormalizeContinuousRetryPolicy(ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{"5xx", "HTTP_429", "5xx", "unknown", "context_length"},
		StatusCodes: []int{504, 404, 504, 99, 600},
		ErrorCodes:  []string{" Context_Length_Exceeded ", "rate_limited", "rate_limited", "bad code!"},
	})
	if strings.Join(got.Categories, ",") != "context_error,http_429,http_5xx" {
		t.Fatalf("normalized categories = %v", got.Categories)
	}
	if strings.Join(intsToStrings(got.StatusCodes), ",") != "404,504" {
		t.Fatalf("normalized status codes = %v", got.StatusCodes)
	}
	if strings.Join(got.ErrorCodes, ",") != "context_length_exceeded,rate_limited" {
		t.Fatalf("normalized error codes = %v", got.ErrorCodes)
	}
	if disabled := NormalizeContinuousRetryPolicy(ContinuousRetryPolicy{CatchAll: true}); disabled.CatchAll {
		t.Fatal("normalization retained catch-all behind a disabled master switch")
	}
	if value := NormalizeContinuousRetryPolicy(ContinuousRetryPolicy{MaxDurationSeconds: -1}).MaxDurationSeconds; value != MinContinuousRetryMaxDurationSeconds {
		t.Fatalf("negative max duration = %d, want minimum", value)
	}
	if value := NormalizeContinuousRetryPolicy(ContinuousRetryPolicy{MaxDurationSeconds: MaxContinuousRetryMaxDurationSeconds + 1}).MaxDurationSeconds; value != MaxContinuousRetryMaxDurationSeconds {
		t.Fatalf("oversized max duration = %d, want %d", value, MaxContinuousRetryMaxDurationSeconds)
	}
}

func TestContinuousRetryPolicyMatchesStatusAndErrorCode(t *testing.T) {
	policy := ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{ContinuousRetryCategoryHTTP429, ContinuousRetryCategoryHTTP5xx},
		StatusCodes: []int{403, 404, 501},
		ErrorCodes:  []string{"context_length_exceeded"},
	}

	for _, status := range []int{403, 404, 429, 500, 501, 503, 504} {
		if !policy.MatchesHTTP(status, nil) {
			t.Errorf("status %d did not match selected policy", status)
		}
	}
	if policy.MatchesHTTP(400, []byte(`{"error":{"code":"invalid_request"}}`)) {
		t.Error("unselected 400 unexpectedly matched")
	}
	if !policy.MatchesHTTP(400, []byte(`{"error":{"code":"context_length_exceeded"}}`)) {
		t.Error("selected error code did not match HTTP body")
	}
	rateCodeOnly := ContinuousRetryPolicy{Enabled: true, ErrorCodes: []string{"rate_limited"}}
	if rateCodeOnly.MatchesErrorCodes([]byte(`{"error":{"code":"rate_limited_model"}}`)) {
		t.Error("an exact error-code selector matched a longer code")
	}

	plain := ContinuousRetryPolicy{Enabled: true, Categories: []string{ContinuousRetryCategoryTransport}}
	if !plain.MatchesTransport("context deadline exceeded") {
		t.Error("transport category did not match a transport error")
	}
	if plain.MatchesHTTP(404, nil) {
		t.Error("transport category unexpectedly matched an HTTP status")
	}
	fourXX := ContinuousRetryPolicy{Enabled: true, Categories: []string{ContinuousRetryCategoryHTTP4xx}}
	if !fourXX.MatchesHTTP(429, nil) {
		t.Error("http_4xx category should include 429")
	}

	catchAll := ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	for _, status := range []int{201, 202, 204, 308, 418, 499, 520, 599, 600, 701} {
		if !catchAll.MatchesHTTP(status, nil) {
			t.Errorf("catch-all policy did not match HTTP status %d", status)
		}
	}
	if catchAll.MatchesHTTP(200, nil) {
		t.Error("catch-all policy selected the protocol's successful HTTP response")
	}
	if !catchAll.MatchesTransport("an unrecognized upstream failure") {
		t.Error("catch-all policy did not match an unrecognized transport failure")
	}
	disabledCatchAll := ContinuousRetryPolicy{CatchAll: true}
	if disabledCatchAll.MatchesHTTP(418, nil) || disabledCatchAll.MatchesTransport("upstream failed") {
		t.Error("catch-all policy bypassed the disabled master switch")
	}
}

func TestContinuousRetryPolicyEncodeParseRoundTrip(t *testing.T) {
	want := ContinuousRetryPolicy{
		Enabled:            true,
		CatchAll:           true,
		Categories:         []string{ContinuousRetryCategoryHTTP4xx},
		StatusCodes:        []int{403, 404},
		ErrorCodes:         []string{"forbidden"},
		MaxDurationSeconds: 75,
	}
	raw := EncodeContinuousRetryPolicy(want)
	got := ParseContinuousRetryPolicy(raw)
	if got.Enabled != want.Enabled || got.CatchAll != want.CatchAll || strings.Join(got.Categories, ",") != strings.Join(want.Categories, ",") {
		t.Fatalf("round-trip policy = %#v, raw=%s", got, raw)
	}
	if strings.Join(intsToStrings(got.StatusCodes), ",") != "403,404" || strings.Join(got.ErrorCodes, ",") != "forbidden" {
		t.Fatalf("round-trip selectors = %#v", got)
	}
	if got.MaxDurationSeconds != want.MaxDurationSeconds {
		t.Fatalf("round-trip max duration = %d, want %d", got.MaxDurationSeconds, want.MaxDurationSeconds)
	}
	legacy := ParseContinuousRetryPolicy(`{"enabled":true,"categories":["transport"]}`)
	if legacy.MaxDurationSeconds != DefaultContinuousRetryMaxDurationSeconds {
		t.Fatalf("legacy max duration = %d, want default", legacy.MaxDurationSeconds)
	}
}

func TestContinuousRetryPolicySelectQueryLocksPostgresRow(t *testing.T) {
	postgresQuery := continuousRetryPolicySelectQuery(true)
	if !strings.Contains(strings.ToUpper(postgresQuery), "FOR UPDATE") {
		t.Fatalf("PostgreSQL policy read lacks FOR UPDATE: %s", postgresQuery)
	}
	sqliteQuery := continuousRetryPolicySelectQuery(false)
	if strings.Contains(strings.ToUpper(sqliteQuery), "FOR UPDATE") {
		t.Fatalf("SQLite policy read must not contain FOR UPDATE: %s", sqliteQuery)
	}
}

func TestSQLiteContinuousRetryPolicyPersistsIndependently(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "continuous-retry.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	want := ContinuousRetryPolicy{
		Enabled:            true,
		CatchAll:           true,
		Categories:         []string{ContinuousRetryCategoryHTTP4xx},
		StatusCodes:        []int{403, 404},
		ErrorCodes:         []string{"forbidden"},
		MaxDurationSeconds: 45,
	}
	committed, err := db.UpdateContinuousRetryPolicy(ctx, ContinuousRetryPolicyUpdate{
		Enabled:            &want.Enabled,
		CatchAll:           &want.CatchAll,
		Categories:         &want.Categories,
		StatusCodes:        &want.StatusCodes,
		ErrorCodes:         &want.ErrorCodes,
		MaxDurationSeconds: &want.MaxDurationSeconds,
	})
	if err != nil {
		t.Fatalf("UpdateContinuousRetryPolicy: %v", err)
	}
	if !committed.Enabled || !committed.CatchAll || len(committed.StatusCodes) != 2 || committed.MaxDurationSeconds != 45 {
		t.Fatalf("committed policy = %#v", committed)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if got := ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy); !got.Enabled || !got.CatchAll || len(got.StatusCodes) != 2 || got.StatusCodes[0] != 403 || len(got.Categories) != 1 || got.Categories[0] != ContinuousRetryCategoryHTTP4xx {
		t.Fatalf("persisted policy = %#v", got)
	}

	// A legacy full settings write must not clear the narrow policy column.
	settings.SiteName = "preserve policy"
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings after full write: %v", err)
	}
	if got := ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy); !got.Enabled || !got.CatchAll || len(got.StatusCodes) != 2 {
		t.Fatalf("full settings write cleared policy = %#v", got)
	}
}

func TestSQLiteContinuousRetryPolicyConcurrentPartialUpdatesDoNotLoseFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "continuous-retry-concurrent.sqlite")
	db1, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New sqlite db1: %v", err)
	}
	t.Cleanup(func() { _ = db1.Close() })
	db2, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New sqlite db2: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	enabled := true
	catchAll := true
	categories := []string{ContinuousRetryCategoryTransport}
	if _, err := db1.UpdateContinuousRetryPolicy(context.Background(), ContinuousRetryPolicyUpdate{
		Enabled:    &enabled,
		CatchAll:   &catchAll,
		Categories: &categories,
	}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	catchAll = false
	statusCodes := []int{403}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := db1.UpdateContinuousRetryPolicy(context.Background(), ContinuousRetryPolicyUpdate{CatchAll: &catchAll})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := db2.UpdateContinuousRetryPolicy(context.Background(), ContinuousRetryPolicyUpdate{StatusCodes: &statusCodes})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent policy update: %v", err)
		}
	}

	settings, err := db1.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	got := ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy)
	if !got.Enabled || got.CatchAll || len(got.StatusCodes) != 1 || got.StatusCodes[0] != 403 || len(got.Categories) != 1 || got.Categories[0] != ContinuousRetryCategoryTransport {
		t.Fatalf("concurrent partial updates lost a field: %#v", got)
	}
}

func intsToStrings(values []int) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strconv.Itoa(value)
	}
	return result
}
