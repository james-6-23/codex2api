package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newGrokStateTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "grok-state.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestGrokStateGenerationFencingAndPresence(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{
		"upstream_type": "grok", "refresh_token": "rt", "account_id": "subject",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 1 || row.CredentialFamilyID == "" {
		t.Fatalf("credential state = generation %d family %q", row.CredentialGeneration, row.CredentialFamilyID)
	}
	now := time.Now().UTC().Truncate(time.Second)
	applied, err := db.UpsertGrokAccountFact(ctx, GrokAccountFact{
		AccountID: id, Kind: GrokFactSettings, CredentialGeneration: 1,
		Status: "ok", HTTPStatus: 200, Source: "settings",
		Payload:       map[string]any{"allow_access": false, "zero": float64(0)},
		FieldPresence: map[string]string{"allow_access": "value", "subscription_tier_display": "null", "missing": "missing"},
		ObservedAt:    now, ExpiresAt: now.Add(5 * time.Minute),
	})
	if err != nil || !applied {
		t.Fatalf("UpsertGrokAccountFact = %v, %v", applied, err)
	}
	newGen, applied, err := db.UpdateAccountCredentialsCAS(ctx, id, 1, map[string]any{"access_token": "at-new", "refresh_token": "rt-new"})
	if err != nil || !applied || newGen != 2 {
		t.Fatalf("CAS = generation %d applied %v err %v", newGen, applied, err)
	}
	applied, err = db.UpsertGrokAccountFact(ctx, GrokAccountFact{
		AccountID: id, Kind: GrokFactSettings, CredentialGeneration: 1,
		Status: "ok", Payload: map[string]any{"allow_access": true}, ObservedAt: now.Add(time.Minute),
	})
	if err != nil || applied {
		t.Fatalf("stale fact = %v, %v; want silently fenced", applied, err)
	}
	fact, err := db.GetGrokAccountFact(ctx, id, GrokFactSettings)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := fact.Payload["allow_access"].(bool); !ok || got {
		t.Fatalf("old fact overwritten: %#v", fact.Payload)
	}
	if fact.FieldPresence["subscription_tier_display"] != "null" || fact.FieldPresence["missing"] != "missing" {
		t.Fatalf("presence lost: %#v", fact.FieldPresence)
	}
}

func TestGrokCatalogReplaceAndNotModifiedKeepsItems(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{
		"upstream_type": "grok", "api_key": "xai-test",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	snapshot := GrokModelCatalogSnapshot{AccountID: id, Origin: "https://api.x.ai/v1", CredentialGeneration: 1, AuthKind: "api_key", Status: "ok", HTTPETag: `"catalog-v1"`, ObservedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	applied, err := db.ReplaceGrokModelCatalog(ctx, snapshot, []GrokModelCatalogItem{{
		ModelID: "grok-4.5", APIBackend: "responses", ContextWindow: 500000,
		SupportedInAPI: false, FieldPresence: map[string]string{"supported_in_api": "value"},
	}})
	if err != nil || !applied {
		t.Fatalf("replace catalog = %v, %v", applied, err)
	}
	touchedAt := now.Add(2 * time.Minute)
	touched, err := db.TouchGrokModelCatalogNotModified(ctx, id, snapshot.Origin, 1, touchedAt, touchedAt.Add(5*time.Minute))
	if err != nil || !touched {
		t.Fatalf("touch catalog = %v, %v", touched, err)
	}
	got, items, err := db.GetGrokModelCatalog(ctx, id, snapshot.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if got.HTTPETag != snapshot.HTTPETag || !got.ObservedAt.Equal(touchedAt) || len(items) != 1 || items[0].ModelID != "grok-4.5" {
		t.Fatalf("catalog after 304 = %#v items %#v", got, items)
	}
	if items[0].SupportedInAPI || items[0].FieldPresence["supported_in_api"] != "value" {
		t.Fatalf("false/presence not preserved: %#v", items[0])
	}
	hintAt := touchedAt.Add(time.Minute)
	applied, err = db.UpdateGrokModelsETagHint(ctx, id, snapshot.Origin, 1, "opaque-refresh-hint", hintAt)
	if err != nil || !applied {
		t.Fatalf("update x-models-etag hint = %v, %v", applied, err)
	}
	got, _, err = db.GetGrokModelCatalog(ctx, id, snapshot.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if got.HTTPETag != snapshot.HTTPETag || got.ETagHint != "opaque-refresh-hint" || !got.ExpiresAt.Equal(hintAt) {
		t.Fatalf("hint must expire catalog without replacing HTTP ETag: %#v", got)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET status='deleted' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.PurgeAccount(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"grok_account_fact_snapshots", "grok_model_catalog_snapshots", "grok_model_catalog_items", "grok_model_capabilities", "grok_credential_identity_claims"} {
		var count int
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE account_id=$1`, id).Scan(&count); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d rows after purge", table, count)
		}
	}
}

func TestPurgedGrokIdentityCanBeImportedAgain(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	credentials := map[string]any{
		"upstream_type": "grok", "refresh_token": "rotating-refresh-token",
		"account_id": "purge-reimport-subject", "credential_family_id": "purge-reimport-family",
	}
	id, duplicate, err := db.InsertGrokAccountIfAbsent(ctx, "before purge", credentials, "", true)
	if err != nil || id <= 0 || duplicate != 0 {
		t.Fatalf("initial import = id %d duplicate %d err %v", id, duplicate, err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET status='deleted' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.PurgeAccount(ctx, id); err != nil {
		t.Fatal(err)
	}

	reimported, duplicate, err := db.InsertGrokAccountIfAbsent(ctx, "after purge", credentials, "", true)
	if err != nil || reimported <= 0 || duplicate != 0 || reimported == id {
		t.Fatalf("reimport = id %d duplicate %d err %v; purged id %d", reimported, duplicate, err, id)
	}
}

func TestUsageLogCredentialGenerationIndexExists(t *testing.T) {
	db := newGrokStateTestDB(t)
	rows, err := db.conn.Query(`PRAGMA index_list('usage_logs')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if name == "idx_usage_logs_account_generation_created_at" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("credential-generation usage index is missing")
	}
}

func TestGrokEmptyCatalogIsPersistedAndReplaceInvalidatesCapabilities(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{
		"upstream_type": "grok", "api_key": "xai-test",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	origin := "https://api.x.ai/v1"
	applied, err := db.UpsertGrokModelCapability(ctx, GrokModelCapability{
		AccountID: id, ModelID: "grok-4.5", Origin: origin,
		Protocol: GrokProtocolResponses, CredentialGeneration: 1,
		Status: "ok", ObservedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil || !applied {
		t.Fatalf("seed capability = %v, %v", applied, err)
	}
	applied, err = db.ReplaceGrokModelCatalog(ctx, GrokModelCatalogSnapshot{
		AccountID: id, Origin: origin, CredentialGeneration: 1,
		AuthKind: "api_key", Status: "ok", ObservedAt: now,
		ExpiresAt: now.Add(5 * time.Minute),
	}, nil)
	if err != nil || !applied {
		t.Fatalf("replace empty catalog = %v, %v", applied, err)
	}
	snapshot, items, err := db.GetGrokModelCatalog(ctx, id, origin)
	if err != nil || snapshot == nil || len(items) != 0 {
		t.Fatalf("empty catalog = snapshot %#v items %#v err %v", snapshot, items, err)
	}
	capabilities, err := db.GetGrokModelCapabilities(ctx, id)
	if err != nil || len(capabilities) != 0 {
		t.Fatalf("stale capabilities retained: %#v err %v", capabilities, err)
	}
}

func TestGrokCatalogFailuresDoNotSlideSuccessfulObservationOrDeleteCapability(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{
		"upstream_type": "grok", "api_key": "xai-test",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://api.x.ai/v1"
	succeededAt := time.Now().UTC().Add(-59 * time.Minute).Truncate(time.Second)
	items := []GrokModelCatalogItem{{ModelID: "grok-4.5", APIBackend: GrokProtocolResponses}}
	if applied, err := db.ReplaceGrokModelCatalog(ctx, GrokModelCatalogSnapshot{
		AccountID: id, Origin: origin, CredentialGeneration: 1, AuthKind: "api_key", Status: "ok",
		HTTPETag: `"catalog-v1"`, ObservedAt: succeededAt, ExpiresAt: succeededAt.Add(5 * time.Minute),
	}, items); err != nil || !applied {
		t.Fatalf("seed catalog = %v, %v", applied, err)
	}
	capability := GrokModelCapability{
		AccountID: id, ModelID: "grok-4.5", Origin: origin, Protocol: GrokProtocolResponses,
		CredentialGeneration: 1, Status: "ok", ObservedAt: succeededAt, ExpiresAt: succeededAt.Add(24 * time.Hour),
	}
	if applied, err := db.UpsertGrokModelCapability(ctx, capability); err != nil || !applied {
		t.Fatalf("seed capability = %v, %v", applied, err)
	}

	for _, attemptedAt := range []time.Time{succeededAt.Add(10 * time.Minute), succeededAt.Add(55 * time.Minute), succeededAt.Add(70 * time.Minute)} {
		if applied, err := db.ReplaceGrokModelCatalog(ctx, GrokModelCatalogSnapshot{
			AccountID: id, Origin: origin, CredentialGeneration: 1, AuthKind: "api_key", Status: "unavailable",
			HTTPETag: `"catalog-v1"`, ObservedAt: attemptedAt, ExpiresAt: attemptedAt,
		}, items); err != nil || !applied {
			t.Fatalf("failed attempt at %v = %v, %v", attemptedAt, applied, err)
		}
	}
	snapshot, gotItems, err := db.GetGrokModelCatalog(ctx, id, origin)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "ok" || !snapshot.ObservedAt.Equal(succeededAt) || !snapshot.ExpiresAt.Equal(succeededAt.Add(5*time.Minute)) {
		t.Fatalf("failed attempts moved successful snapshot: %+v", snapshot)
	}
	if len(gotItems) != 1 || gotItems[0].ModelID != "grok-4.5" {
		t.Fatalf("failed attempts replaced items: %+v", gotItems)
	}
	caps, err := db.GetGrokModelCapabilities(ctx, id)
	if err != nil || len(caps) != 1 || !caps[0].ExpiresAt.Equal(capability.ExpiresAt) {
		t.Fatalf("failed attempts invalidated capability: %+v err=%v", caps, err)
	}
}

func TestGrokCatalogSuccessfulReplacementOnlyInvalidatesCapabilityOnChange(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{
		"upstream_type": "grok", "api_key": "xai-test",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://api.x.ai/v1"
	routeOrigin := "https://models-route.example/v1"
	now := time.Now().UTC().Truncate(time.Second)
	items := []GrokModelCatalogItem{{
		ModelID: "grok-4.5", APIBackend: GrokProtocolResponses, ContextWindow: 500000,
		ExtraHeaders: map[string]string{"x-safe": "one"}, FieldPresence: map[string]string{"api_backend": "value"},
	}}
	replace := func(at time.Time, catalogItems []GrokModelCatalogItem) {
		t.Helper()
		if applied, replaceErr := db.ReplaceGrokModelCatalog(ctx, GrokModelCatalogSnapshot{
			AccountID: id, Origin: origin, CredentialGeneration: 1, AuthKind: "api_key", Status: "ok",
			HTTPETag: `"catalog-v1"`, ObservedAt: at, ExpiresAt: at.Add(5 * time.Minute),
		}, catalogItems); replaceErr != nil || !applied {
			t.Fatalf("replace catalog = %v, %v", applied, replaceErr)
		}
	}
	replace(now, items)
	seedCapability := func() {
		t.Helper()
		if applied, capErr := db.UpsertGrokModelCapability(ctx, GrokModelCapability{
			AccountID: id, ModelID: "grok-4.5", Origin: routeOrigin, Protocol: GrokProtocolResponses,
			CredentialGeneration: 1, Status: "ok", ObservedAt: now, ExpiresAt: now.Add(time.Hour),
		}); capErr != nil || !applied {
			t.Fatalf("seed capability = %v, %v", applied, capErr)
		}
	}
	seedCapability()

	// A second 200 with identical identity/content merely refreshes freshness.
	replace(now.Add(time.Minute), items)
	if caps, capErr := db.GetGrokModelCapabilities(ctx, id); capErr != nil || len(caps) != 1 {
		t.Fatalf("unchanged replacement invalidated capability: %+v err=%v", caps, capErr)
	}

	changed := append([]GrokModelCatalogItem(nil), items...)
	changed[0].APIBackend = GrokProtocolChatCompletions
	replace(now.Add(2*time.Minute), changed)
	if caps, capErr := db.GetGrokModelCapabilities(ctx, id); capErr != nil || len(caps) != 0 {
		t.Fatalf("changed replacement retained cross-origin capability: %+v err=%v", caps, capErr)
	}
}

// 例行 token 刷新的 CAS 必须把上一代目录/能力/事实盖章到新代:上游身份未变,
// 观测事实理应延续,否则每个刷新周期都会触发全模型 × 3 协议的真实推理重探。
func TestUpdateAccountCredentialsCASCarriesForwardObservations(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{"upstream_type": "grok", "refresh_token": "rt"}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	origin := "https://api.x.ai/v1"
	capExpiry := now.Add(time.Hour)
	if applied, err := db.UpsertGrokAccountFact(ctx, GrokAccountFact{
		AccountID: id, Kind: GrokFactBilling, CredentialGeneration: 1, Status: "ok", HTTPStatus: 200,
		Payload: map[string]any{"balance": float64(5)}, ObservedAt: now, ExpiresAt: now.Add(5 * time.Minute),
	}); err != nil || !applied {
		t.Fatalf("seed fact = %v, %v", applied, err)
	}
	if applied, err := db.ReplaceGrokModelCatalog(ctx, GrokModelCatalogSnapshot{
		AccountID: id, Origin: origin, CredentialGeneration: 1, AuthKind: "api_key", Status: "ok",
		ObservedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}, []GrokModelCatalogItem{{ModelID: "grok-4.5", APIBackend: "responses"}}); err != nil || !applied {
		t.Fatalf("seed catalog = %v, %v", applied, err)
	}
	if applied, err := db.UpsertGrokModelCapability(ctx, GrokModelCapability{
		AccountID: id, ModelID: "grok-4.5", Origin: origin, Protocol: GrokProtocolResponses,
		CredentialGeneration: 1, Status: "ok", HTTPStatus: 200, ObservedAt: now, ExpiresAt: capExpiry,
	}); err != nil || !applied {
		t.Fatalf("seed capability = %v, %v", applied, err)
	}

	newGen, applied, err := db.UpdateAccountCredentialsCAS(ctx, id, 1, map[string]any{"access_token": "at-new", "refresh_token": "rt-new"})
	if err != nil || !applied || newGen != 2 {
		t.Fatalf("CAS = generation %d applied %v err %v", newGen, applied, err)
	}

	state, err := db.GetGrokAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if state.CredentialGeneration != 2 {
		t.Fatalf("state generation = %d, want 2", state.CredentialGeneration)
	}
	fact, ok := state.Facts[GrokFactBilling]
	if !ok || fact.CredentialGeneration != 2 {
		t.Fatalf("fact was not carried forward: %+v", state.Facts)
	}
	if len(state.Catalogs) != 1 || state.Catalogs[0].Snapshot.CredentialGeneration != 2 {
		t.Fatalf("catalog snapshot was not carried forward: %+v", state.Catalogs)
	}
	if len(state.Catalogs[0].Items) != 1 || state.Catalogs[0].Items[0].CredentialGeneration != 2 {
		t.Fatalf("catalog items were not carried forward: %+v", state.Catalogs[0].Items)
	}
	if len(state.Capabilities) != 1 || state.Capabilities[0].CredentialGeneration != 2 {
		t.Fatalf("capability was not carried forward: %+v", state.Capabilities)
	}
	if !state.Capabilities[0].ExpiresAt.Equal(capExpiry) {
		t.Fatalf("carry-forward must not extend freshness: %v, want %v", state.Capabilities[0].ExpiresAt, capExpiry)
	}
}

func TestGrokCapabilityGenerationFenced(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{"upstream_type": "grok", "api_key": "key"}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	applied, err := db.UpsertGrokModelCapability(ctx, GrokModelCapability{AccountID: id, ModelID: "grok-4.5", Origin: "https://api.x.ai/v1", Protocol: GrokProtocolResponses, CredentialGeneration: 1, Status: "ok", HTTPStatus: 200, Source: "probe", ObservedAt: now})
	if err != nil || !applied {
		t.Fatalf("upsert capability = %v, %v", applied, err)
	}
	if _, applied, err = db.UpdateAccountCredentialsCAS(ctx, id, 1, map[string]any{"api_key": "rotated"}); err != nil || !applied {
		t.Fatalf("rotate credentials = %v, %v", applied, err)
	}
	applied, err = db.UpsertGrokModelCapability(ctx, GrokModelCapability{AccountID: id, ModelID: "grok-4.5", Origin: "https://api.x.ai/v1", Protocol: GrokProtocolMessages, CredentialGeneration: 1, Status: "ok", ObservedAt: now})
	if err != nil || applied {
		t.Fatalf("stale capability = %v, %v", applied, err)
	}
}

func TestGrokCapabilityExpiryIsScopedAndGenerationFenced(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{"upstream_type": "grok", "api_key": "key"}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	origin := "https://api.x.ai/v1"
	for _, cap := range []GrokModelCapability{
		{AccountID: id, ModelID: "grok-4.5", Origin: origin, Protocol: GrokProtocolResponses, CredentialGeneration: 1, Status: "ok", ObservedAt: now, ExpiresAt: now.Add(time.Hour)},
		{AccountID: id, ModelID: "grok-4", Origin: origin, Protocol: GrokProtocolMessages, CredentialGeneration: 1, Status: "ok", ObservedAt: now, ExpiresAt: now.Add(time.Hour)},
	} {
		if applied, err := db.UpsertGrokModelCapability(ctx, cap); err != nil || !applied {
			t.Fatalf("seed capability = %v, %v", applied, err)
		}
	}
	expiredAt := now.Add(time.Minute)
	if affected, err := db.ExpireGrokModelCapabilities(ctx, id, 1, "grok-4.5", origin+"/", GrokProtocolResponses, expiredAt); err != nil || affected != 1 {
		t.Fatalf("scoped expire = %d, %v", affected, err)
	}
	caps, err := db.GetGrokModelCapabilities(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, cap := range caps {
		if cap.ModelID == "grok-4.5" && !cap.ExpiresAt.Equal(expiredAt) {
			t.Fatalf("target expiry = %v, want %v", cap.ExpiresAt, expiredAt)
		}
		if cap.ModelID == "grok-4" && !cap.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("unrelated capability changed: %+v", cap)
		}
	}
	if _, applied, err := db.UpdateAccountCredentialsCAS(ctx, id, 1, map[string]any{"api_key": "rotated"}); err != nil || !applied {
		t.Fatalf("rotate = %v, %v", applied, err)
	}
	if affected, err := db.ExpireGrokModelCapabilities(ctx, id, 1, "", "", "", now.Add(2*time.Minute)); err != nil || affected != 0 {
		t.Fatalf("stale generation expire = %d, %v", affected, err)
	}
}

func TestGrokSubscriptionFactChangeExpiresCapabilitiesAtomically(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{"upstream_type": "grok", "access_token": "at"}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if applied, err := db.UpsertGrokModelCapability(ctx, GrokModelCapability{AccountID: id, ModelID: "grok-4.5", Origin: "https://example.test/v1", Protocol: GrokProtocolResponses, CredentialGeneration: 1, Status: "ok", ObservedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil || !applied {
		t.Fatalf("seed capability = %v, %v", applied, err)
	}
	applied, err := db.UpsertGrokAccountFactAndExpireCapabilities(ctx, GrokAccountFact{
		AccountID: id, Kind: GrokFactUser, CredentialGeneration: 1, Status: "ok", HTTPStatus: 200,
		Payload: map[string]any{"subscriptionTier": "GrokPro"}, FieldPresence: map[string]string{"subscriptionTier": "value"},
		ObservedAt: now.Add(time.Minute), ExpiresAt: now.Add(2 * time.Minute),
	}, now.Add(time.Minute))
	if err != nil || !applied {
		t.Fatalf("atomic fact/capability update = %v, %v", applied, err)
	}
	caps, err := db.GetGrokModelCapabilities(ctx, id)
	if err != nil || len(caps) != 1 || !caps[0].ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("capabilities after subscription change = %+v, err=%v", caps, err)
	}
}

func TestGrokIdentityUpdateAdvancesGenerationButConfigDoesNot(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "grok", map[string]any{
		"upstream_type": "grok", "refresh_token": "rt-old", "models": []string{"grok-4.5"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]any{"models": []string{"grok-4.5", "grok-4"}, "model_mapping": "{}"}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 1 {
		t.Fatalf("config update generation = %d, want 1", row.CredentialGeneration)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]any{"refresh_token": "rt-new"}); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 2 {
		t.Fatalf("identity update generation = %d, want 2", row.CredentialGeneration)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]any{"refresh_token": "rt-new"}); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 2 {
		t.Fatalf("idempotent identity update generation = %d, want 2", row.CredentialGeneration)
	}
}

func TestGrokStateDoesNotRelabelJWTCompatibilityPlanAsArchive(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok-jwt-plan", "xai", "grok", map[string]any{
		"upstream_type": "grok", "refresh_token": "rt",
		"plan_type": "supergrok_plus", "jwt_plan_type": "supergrok_plus", "jwt_plan_trusted": false,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	state, err := db.GetGrokAccountState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Identity.JWTTier != "supergrok_plus" || state.Identity.JWTTierTrust != "unverified" {
		t.Fatalf("JWT tier projection = %+v", state.Identity)
	}
	if state.Identity.ArchivePlan != "" || state.Identity.ArchivePlanSource != "" {
		t.Fatalf("JWT compatibility plan mislabeled as archive: %+v", state.Identity)
	}
}

func TestGrokSchedulerMetadataCredentialMergeAdvancesGenerationOnlyForIdentity(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "grok-scheduler-merge", "xai", "grok", map[string]any{
		"upstream_type": "grok", "refresh_token": "rt-old", "models": []string{"grok-4.5"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	update := func(credentials map[string]any) {
		t.Helper()
		if err := db.UpdateAccountSchedulerMetadata(ctx, id,
			OptionalNullInt64{}, OptionalNullInt64{}, OptionalBool{}, OptionalInt64Slice{},
			OptionalStringSlice{}, OptionalInt64Slice{}, OptionalString{}, credentials); err != nil {
			t.Fatalf("UpdateAccountSchedulerMetadata: %v", err)
		}
	}
	assertGeneration := func(want int64) {
		t.Helper()
		row, getErr := db.GetAccountByID(ctx, id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if row.CredentialGeneration != want {
			t.Fatalf("credential generation = %d, want %d; credentials=%#v", row.CredentialGeneration, want, row.Credentials)
		}
	}

	update(map[string]any{"models": []string{"grok-4.5", "grok-4"}, "model_mapping": `{}`})
	assertGeneration(1)
	update(map[string]any{"refresh_token": "rt-old"})
	assertGeneration(1)
	update(map[string]any{"refresh_token": "rt-new"})
	assertGeneration(2)
}

func TestGrokBatchCredentialMergeFencesGenerationPerAccount(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	insert := func(name, refreshToken string) int64 {
		t.Helper()
		id, err := db.InsertAccountWithUpstream(ctx, name, "xai", "grok", map[string]any{
			"upstream_type": "grok", "refresh_token": refreshToken,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	alreadyCurrent := insert("grok-batch-current", "rt-target")
	needsRotation := insert("grok-batch-old", "rt-old")

	updated, err := db.BatchUpdateAccountMetadata(ctx, []int64{alreadyCurrent, needsRotation}, BatchAccountMetadataUpdate{
		CredentialUpdates: map[string]any{"refresh_token": "rt-target"},
	})
	if err != nil || len(updated) != 2 {
		t.Fatalf("BatchUpdateAccountMetadata(identity) = %v, %v", updated, err)
	}
	assertGeneration := func(id, want int64) {
		t.Helper()
		row, getErr := db.GetAccountByID(ctx, id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if row.CredentialGeneration != want {
			t.Fatalf("account %d generation = %d, want %d", id, row.CredentialGeneration, want)
		}
	}
	assertGeneration(alreadyCurrent, 1)
	assertGeneration(needsRotation, 2)

	updated, err = db.BatchUpdateAccountMetadata(ctx, []int64{alreadyCurrent, needsRotation}, BatchAccountMetadataUpdate{
		CredentialUpdates: map[string]any{"models": []string{"grok-4.5"}},
	})
	if err != nil || len(updated) != 2 {
		t.Fatalf("BatchUpdateAccountMetadata(config) = %v, %v", updated, err)
	}
	assertGeneration(alreadyCurrent, 1)
	assertGeneration(needsRotation, 2)
}

func TestGrokSchemaMigrationIsIdempotent(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	if err := db.ensureGrokStateSchema(ctx); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if err := db.ensureGrokStateSchema(ctx); err != nil {
		t.Fatalf("third migration: %v", err)
	}
}

func TestGrokStateBackfillMarkerSkipsHistoricalScan(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()

	if _, err := db.conn.ExecContext(ctx, `
		CREATE TRIGGER reject_historical_claim_scan
		BEFORE INSERT ON grok_credential_identity_claims
		BEGIN SELECT RAISE(ABORT, 'historical claims were scanned again'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.ensureGrokStateSchema(ctx); err != nil {
		t.Fatalf("completed migration did not skip historical scan: %v", err)
	}
}

func TestGrokStateBackfillResumesPartialProgress(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	credentials := map[string]any{
		"upstream_type": "grok", "refresh_token": "resume-token", "account_id": "resume-subject",
	}
	first, err := db.InsertAccountWithUpstream(ctx, "resume-first", "xai", "grok", credentials, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.InsertAccountWithUpstream(ctx, "resume-second", "xai", "grok", credentials, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM grok_credential_identity_claims; DELETE FROM data_migrations WHERE version=$1; DELETE FROM grok_state_migration_progress WHERE version=$1`, dataMigrationGrokStateBackfillV1); err != nil {
		t.Fatal(err)
	}
	family, err := db.EnsureAccountCredentialFamilyID(ctx, first, "resume-family")
	if err != nil {
		t.Fatal(err)
	}
	principalKey := grokCredentialIdentityKeys(credentials, family)[0]
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO grok_credential_identity_claims(identity_key,account_id) VALUES($1,$2)`, principalKey, first); err != nil {
		t.Fatal(err)
	}
	if err := db.ensureGrokStateSchema(ctx); err != nil {
		t.Fatalf("resume partial migration: %v", err)
	}
	var owner int64
	if err := db.conn.QueryRowContext(ctx, `SELECT account_id FROM grok_credential_identity_claims WHERE identity_key=$1`, principalKey).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != first {
		t.Fatalf("existing oldest identity owner = %d, want %d (second=%d)", owner, first, second)
	}
	var emptyFamilies, markers int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE COALESCE(credential_family_id,'')=''`).Scan(&emptyFamilies); err != nil {
		t.Fatal(err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_migrations WHERE version=$1`, dataMigrationGrokStateBackfillV1).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if emptyFamilies != 0 || markers != 1 {
		t.Fatalf("partial resume result: empty families=%d markers=%d", emptyFamilies, markers)
	}
}

func TestGrokCredentialFamilyMatchesImporterAlgorithm(t *testing.T) {
	got := credentialFamilyCandidate(map[string]any{
		"upstream_type": "grok", "refresh_token": "rt",
		"account_id": "subject-1", "grok_client_id": "client-1",
	})
	// This fixture is the SHA-256 produced by auth.GrokCredentialFamilyID for
	// the same identity and default xAI issuer/chat-proxy origin.
	const want = "0a28c97a34945ff06221cdf425e7d1065d7e46ea1a9184e05e9e0fb6ff10f6eb"
	if got != want {
		t.Fatalf("family = %q, want %q", got, want)
	}
}

func TestInsertGrokAccountIfAbsentAtomicallyDeduplicatesPrincipalAndFamily(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	base := map[string]any{
		"upstream_type": "grok", "refresh_token": "rt-one", "access_token": "at-one",
		"account_id": "subject-one", "grok_oidc_issuer": "https://auth.x.ai",
		"credential_family_id": "family-one",
	}
	id, duplicateID, err := db.InsertGrokAccountIfAbsent(ctx, "first", base, "", true)
	if err != nil || id <= 0 || duplicateID != 0 {
		t.Fatalf("first insert = id %d duplicate %d err %v", id, duplicateID, err)
	}

	byPrincipal := map[string]any{
		"upstream_type": "grok", "refresh_token": "rt-two", "account_id": "subject-one",
		"grok_oidc_issuer": "https://auth.x.ai", "credential_family_id": "family-two",
	}
	if got, duplicate, err := db.InsertGrokAccountIfAbsent(ctx, "principal duplicate", byPrincipal, "", true); err != nil || got != 0 || duplicate != id {
		t.Fatalf("principal duplicate = id %d duplicate %d err %v", got, duplicate, err)
	}
	byFamily := map[string]any{
		"upstream_type": "grok", "refresh_token": "rt-three", "account_id": "subject-three",
		"grok_oidc_issuer": "https://auth.x.ai", "credential_family_id": "family-one",
	}
	if got, duplicate, err := db.InsertGrokAccountIfAbsent(ctx, "family duplicate", byFamily, "", true); err != nil || got != 0 || duplicate != id {
		t.Fatalf("family duplicate = id %d duplicate %d err %v", got, duplicate, err)
	}

	const workers = 8
	var wg sync.WaitGroup
	ids := make(chan int64, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, insertErr := db.InsertGrokAccountIfAbsent(ctx, "concurrent", map[string]any{
				"upstream_type": "grok", "refresh_token": "rotating", "account_id": "concurrent-subject",
				"credential_family_id": "concurrent-family",
			}, "", true)
			if insertErr != nil {
				errs <- insertErr
				return
			}
			ids <- got
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	created := 0
	for got := range ids {
		if got > 0 {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("concurrent created = %d, want 1", created)
	}
	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE name='concurrent'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent rows = %d err %v", count, err)
	}
}

func TestGrokIdentityClaimBackfillToleratesHistoricalDuplicates(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	credentials := map[string]any{
		"upstream_type": "grok", "refresh_token": "legacy-rt", "account_id": "legacy-duplicate",
		"credential_family_id": "legacy-family",
	}
	first, err := db.InsertAccountWithUpstream(ctx, "legacy-first", "xai", "grok", credentials, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.InsertAccountWithUpstream(ctx, "legacy-second", "xai", "grok", credentials, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM data_migrations WHERE version=$1`, dataMigrationGrokStateBackfillV1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM grok_state_migration_progress WHERE version=$1`, dataMigrationGrokStateBackfillV1); err != nil {
		t.Fatal(err)
	}
	if err := db.ensureGrokStateSchema(ctx); err != nil {
		t.Fatalf("duplicate-tolerant migration: %v", err)
	}
	got, duplicate, err := db.InsertGrokAccountIfAbsent(ctx, "new-duplicate", credentials, "", true)
	if err != nil || got != 0 || duplicate != first {
		t.Fatalf("post-backfill duplicate = id %d duplicate %d err %v; historical ids %d/%d", got, duplicate, err, first, second)
	}
	var historical int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id IN ($1,$2)`, first, second).Scan(&historical); err != nil || historical != 2 {
		t.Fatalf("historical duplicates changed = %d err %v", historical, err)
	}
}

func TestMergeAccountCredentialsForGenerationIsFencedAndSanitized(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "legacy-projection", "xai", "grok", map[string]any{
		"upstream_type": "grok", "api_key": "keep-secret", "models": []string{"old"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := db.MergeAccountCredentialsForGeneration(ctx, id, row.CredentialGeneration, map[string]any{
		"models": []string{"grok-safe"}, "access_token": "must-not-write", "extra_headers": map[string]string{"authorization": "must-not-write"},
	})
	if err != nil || !applied {
		t.Fatalf("legacy merge applied=%v err=%v", applied, err)
	}
	after, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.CredentialGeneration != row.CredentialGeneration {
		t.Fatalf("legacy merge advanced generation from %d to %d", row.CredentialGeneration, after.CredentialGeneration)
	}
	if got := after.GetCredentialStringSlice("models"); len(got) != 1 || got[0] != "grok-safe" {
		t.Fatalf("legacy models = %#v", got)
	}
	if after.GetCredential("api_key") != "keep-secret" || after.GetCredential("access_token") != "" {
		t.Fatalf("legacy merge changed secret identity fields: %#v", after.Credentials)
	}

	if applied, err = db.MergeAccountCredentialsForGeneration(ctx, id, row.CredentialGeneration+1, map[string]any{"models": []string{"stale"}}); err != nil || applied {
		t.Fatalf("stale legacy merge applied=%v err=%v", applied, err)
	}
	final, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := final.GetCredentialStringSlice("models"); len(got) != 1 || got[0] != "grok-safe" {
		t.Fatalf("stale generation changed legacy models: %#v", got)
	}
}
