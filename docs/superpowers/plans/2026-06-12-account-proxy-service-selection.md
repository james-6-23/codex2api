# Account Proxy Service Selection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace manual account proxy URL entry with a proxy-service selector that can bind accounts to a managed proxy service or use the full proxy pool/default strategy.

**Architecture:** Add nullable `proxy_id` support to account persistence and runtime account state, keep `proxy_url` for legacy custom proxies, and resolve proxies in the order `proxy_id` binding → legacy `proxy_url` → existing proxy pool/global proxy flow. The frontend loads managed proxies once with account data and reuses a focused proxy strategy selector across add/edit/OAuth/OpenAI Responses forms.

**Tech Stack:** Go + Gin admin API, existing `database.DB` SQL migration layer for SQLite/PostgreSQL, `auth.Store` runtime account manager, React + TypeScript admin frontend, existing custom `Select`, `Input`, `Button`, i18next locale JSON.

---

## File Structure

### Backend files

- Modify `database/postgres.go`
  - Add `ProxyID sql.NullInt64` to `AccountRow`.
  - Add `proxy_id` to SQLite and PostgreSQL schema/migrations.
  - Add proxy lookup helpers: `GetProxyByID`, `GetEnabledProxyByID`, and optionally `ListEnabledProxies` reuse.
  - Update account select/insert/update code paths to read/write `proxy_id`.
- Modify `database/account_groups.go`
  - Extend `UpdateAccountSchedulerMetadata` or related account metadata update path to update `proxy_id` together with `proxy_url`.
- Modify `auth/store.go`
  - Add `ProxyID *int64` to `Account`.
  - Track managed proxy rows by ID in `Store`.
  - Resolve `ProxyID` before legacy `ProxyURL`.
  - Apply proxy binding changes to in-memory account state.
- Modify `auth/proxy_resolution_test.go`
  - Add tests for bound, disabled, missing, legacy, and pool fallback behavior.
- Modify `admin/handler.go`
  - Add `proxy_id` to request structs.
  - Validate proxy service IDs on add/edit.
  - Include `proxy_id` and `proxy_status` in account responses.
  - Add `POST /api/admin/proxies/test-all`.
- Modify `admin/oauth.go`
  - Add `proxy_id` support to OAuth generation/exchange request payloads.
  - Resolve proxy ID to URL before OAuth code exchange.
- Modify `admin/handler_test.go`, `admin/oauth_test.go`, or create targeted tests as needed.

### Frontend files

- Modify `frontend/src/types.ts`
  - Add `proxy_id`, `proxy_status`, and `proxy_id` request fields.
- Modify `frontend/src/api.ts`
  - Add `testAllProxies()` client method.
  - Add proxy ID request typing support.
- Modify `frontend/src/pages/Accounts.tsx`
  - Load proxies alongside accounts/settings.
  - Replace manual proxy URL input with selector component.
  - Preserve legacy custom proxy behavior for old accounts.
  - Keep `测试代理` for bound proxy, legacy proxy, and pool/default strategy.
- Modify `frontend/src/pages/Proxies.tsx`
  - Reuse new `api.testAllProxies()` for one-click testing if feasible.
- Modify `frontend/src/locales/zh.json` and `frontend/src/locales/en.json`
  - Add proxy strategy labels and messages.

---

## Task 1: Persist `proxy_id` in database account rows

**Files:**
- Modify: `database/postgres.go`
- Modify: `database/sqlite.go` if schema helpers are split there only for SQLite migration
- Test: existing Go database tests or add tests to `database/sqlite_test.go`

- [ ] **Step 1: Write failing SQLite migration test**

Add this test to `database/sqlite_test.go` near existing schema/migration tests:

```go
func TestSQLiteAccountsHasProxyIDColumn(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	defer db.Close()

	columns, err := db.sqliteTableColumns(ctx, "accounts")
	if err != nil {
		t.Fatalf("sqliteTableColumns(accounts): %v", err)
	}
	if _, ok := columns["proxy_id"]; !ok {
		t.Fatalf("accounts.proxy_id column missing; columns=%v", columns)
	}
}
```

If `newTestSQLiteDB` is not available in `database/sqlite_test.go`, use the existing helper in that file that opens a temporary SQLite DB and runs migration. Do not create a second migration helper.

- [ ] **Step 2: Run test and verify it fails**

Run:

```bash
go test ./database -run TestSQLiteAccountsHasProxyIDColumn -count=1
```

Expected: FAIL with `accounts.proxy_id column missing`.

- [ ] **Step 3: Add account row field and schema columns**

In `database/postgres.go`, extend `AccountRow`:

```go
type AccountRow struct {
	ID                      int64
	Name                    string
	Platform                string
	Type                    string
	Credentials             map[string]interface{}
	ProxyURL                string
	ProxyID                 sql.NullInt64
	Status                  string
	CooldownReason          string
	CooldownUntil           sql.NullTime
	ErrorMessage            string
	Enabled                 bool
	Locked                  bool
	ScoreBiasOverride       sql.NullInt64
	BaseConcurrencyOverride sql.NullInt64
	SkipWarmTier            bool
	CreatedAt               time.Time
	UpdatedAt               time.Time
	DeletedAt               sql.NullTime
	Tags                    []string
	CreditEnabled           bool
	CreditSkipUsageWindow   bool
	ImageQuotaRemaining     sql.NullInt64
	ImageQuotaTotal         sql.NullInt64
	TodayUsedCount          int64
	ImageQuotaResetAt       sql.NullTime
}
```

In PostgreSQL migration SQL, add:

```sql
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS proxy_id BIGINT NULL;
```

In the SQLite `CREATE TABLE IF NOT EXISTS accounts` statement, add the column immediately after `proxy_url TEXT DEFAULT ''`:

```sql
proxy_id INTEGER NULL,
```

In the SQLite `columns := []struct{...}` list, add:

```go
{"accounts", "proxy_id", "INTEGER NULL"},
```

- [ ] **Step 4: Update account select/scan paths**

Find all account queries that select `proxy_url` from `accounts`, especially `ListAccounts`, `GetAccountByID`, and load-at-startup queries. Add `proxy_id` immediately after `proxy_url` in SELECT lists and scan destinations.

Use this scan pattern wherever a row is scanned into `AccountRow`:

```go
if err := rows.Scan(
	&row.ID,
	&row.Name,
	&row.Platform,
	&row.Type,
	&credentialsRaw,
	&row.ProxyURL,
	&row.ProxyID,
	&row.Status,
	// keep the remaining existing scan destinations in their current order
); err != nil {
	return nil, err
}
```

For `QueryRowContext` use the same placement:

```go
if err := db.conn.QueryRowContext(ctx, query, id).Scan(
	&row.ID,
	&row.Name,
	&row.Platform,
	&row.Type,
	&credentialsRaw,
	&row.ProxyURL,
	&row.ProxyID,
	&row.Status,
	// keep remaining fields unchanged
); err != nil {
	return nil, err
}
```

- [ ] **Step 5: Add insert helpers that accept proxy ID**

Keep existing public methods compatible. Add new variants rather than changing callers all at once:

```go
func (db *DB) InsertAccountWithCredentialsAndProxyID(ctx context.Context, name string, credentials map[string]interface{}, proxyURL string, proxyID *int64) (int64, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	var proxyIDValue interface{}
	if proxyID != nil && *proxyID > 0 {
		proxyIDValue = *proxyID
	} else {
		proxyIDValue = nil
	}
	var id int64
	err := db.conn.QueryRowContext(ctx,
		`INSERT INTO accounts (name, credentials, proxy_url, proxy_id) VALUES ($1, $2, $3, $4) RETURNING id`,
		strings.TrimSpace(name), credentials, proxyURL, proxyIDValue,
	).Scan(&id)
	return id, err
}
```

If the existing implementation must marshal `credentials` differently for SQLite/Postgres, copy that existing credential-marshalling code into the new method; the only semantic change is adding `proxy_id` to the INSERT.

Then make the old method delegate:

```go
func (db *DB) InsertAccountWithCredentials(ctx context.Context, name string, credentials map[string]interface{}, proxyURL string) (int64, error) {
	return db.InsertAccountWithCredentialsAndProxyID(ctx, name, credentials, proxyURL, nil)
}
```

Add the same pattern for `InsertAccount`:

```go
func (db *DB) InsertAccountWithProxyID(ctx context.Context, name, refreshToken, proxyURL string, proxyID *int64) (int64, error) {
	return db.InsertAccountWithCredentialsAndProxyID(ctx, name, map[string]interface{}{"refresh_token": strings.TrimSpace(refreshToken)}, proxyURL, proxyID)
}
```

Keep `InsertAccount` delegating to preserve external callers.

- [ ] **Step 6: Add proxy lookup helpers**

Near `ListProxies` in `database/postgres.go`, add:

```go
func (db *DB) GetProxyByID(ctx context.Context, id int64) (*ProxyRow, error) {
	if id <= 0 {
		return nil, sql.ErrNoRows
	}
	row := db.conn.QueryRowContext(ctx, `SELECT id, url, label, enabled, created_at, COALESCE(test_ip,''), COALESCE(test_location,''), COALESCE(test_latency_ms,0) FROM proxies WHERE id = $1`, id)
	p := &ProxyRow{}
	var createdAtRaw interface{}
	if err := row.Scan(&p.ID, &p.URL, &p.Label, &p.Enabled, &createdAtRaw, &p.TestIP, &p.TestLocation, &p.TestLatencyMs); err != nil {
		return nil, err
	}
	createdAt, err := parseDBTimeValue(createdAtRaw)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = createdAt
	return p, nil
}

func (db *DB) GetEnabledProxyByID(ctx context.Context, id int64) (*ProxyRow, error) {
	proxy, err := db.GetProxyByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !proxy.Enabled {
		return nil, sql.ErrNoRows
	}
	return proxy, nil
}
```

- [ ] **Step 7: Run database tests**

Run:

```bash
go test ./database -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit database persistence changes**

```bash
git add database/postgres.go database/sqlite.go database/sqlite_test.go
git commit -m "feat: persist account proxy service binding"
```

---

## Task 2: Add runtime proxy ID resolution

**Files:**
- Modify: `auth/store.go`
- Modify: `auth/proxy_resolution_test.go`
- Modify: account construction call sites in `admin/handler.go`, `admin/oauth.go`, and any loader that creates `auth.Account` from `database.AccountRow`

- [ ] **Step 1: Write failing proxy ID resolution tests**

Append to `auth/proxy_resolution_test.go`:

```go
func int64Ptr(v int64) *int64 { return &v }

func TestResolveProxyForAccountPrefersEnabledProxyID(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://pool-1:8080"},
		proxyByID: map[int64]managedProxy{
			12: {URL: " http://managed-proxy:8080 ", Enabled: true},
		},
	}
	account := &Account{DBID: 7, ProxyID: int64Ptr(12), ProxyURL: "http://legacy-proxy:8080"}

	got := store.ResolveProxyForAccount(account)
	want := "http://managed-proxy:8080"
	if got != want {
		t.Fatalf("ResolveProxyForAccount() = %q, want %q", got, want)
	}
}

func TestResolveProxyForAccountDoesNotFallbackWhenProxyIDDisabled(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://pool-1:8080"},
		proxyByID: map[int64]managedProxy{
			12: {URL: "http://managed-proxy:8080", Enabled: false},
		},
	}
	account := &Account{DBID: 7, ProxyID: int64Ptr(12), ProxyURL: "http://legacy-proxy:8080"}

	if got := store.ResolveProxyForAccount(account); got != "" {
		t.Fatalf("ResolveProxyForAccount() = %q, want empty for disabled bound proxy", got)
	}
}

func TestResolveProxyForAccountDoesNotFallbackWhenProxyIDMissing(t *testing.T) {
	store := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"http://pool-1:8080"},
		proxyByID:        map[int64]managedProxy{},
	}
	account := &Account{DBID: 7, ProxyID: int64Ptr(12), ProxyURL: "http://legacy-proxy:8080"}

	if got := store.ResolveProxyForAccount(account); got != "" {
		t.Fatalf("ResolveProxyForAccount() = %q, want empty for missing bound proxy", got)
	}
}
```

- [ ] **Step 2: Run tests and verify they fail**

Run:

```bash
go test ./auth -run 'TestResolveProxyForAccount' -count=1
```

Expected: FAIL because `ProxyID`, `managedProxy`, or `proxyByID` do not exist.

- [ ] **Step 3: Add runtime account proxy ID field**

In `auth/store.go`, extend `Account`:

```go
type Account struct {
	mu             sync.RWMutex
	DBID           int64
	RefreshToken   string
	SessionToken   string
	AccessToken    string
	ExpiresAt      time.Time
	AccountID      string
	Email          string
	PlanType       string
	ProxyURL       string
	ProxyID        *int64
	UpstreamType   string
	BaseURL        string
	APIKey         string
	Models         []string
	// keep all existing fields below unchanged
}
```

Add a getter:

```go
func (a *Account) GetProxyID() *int64 {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ProxyID == nil || *a.ProxyID <= 0 {
		return nil
	}
	value := *a.ProxyID
	return &value
}
```

- [ ] **Step 4: Add managed proxy cache to Store**

In `auth/store.go`, near `Store` fields:

```go	type managedProxy struct {
	URL     string
	Enabled bool
}
```

Add Store field:

```go
proxyByID map[int64]managedProxy
```

Initialize it in `NewStore` or equivalent constructor:

```go
proxyByID: make(map[int64]managedProxy),
```

- [ ] **Step 5: Load managed proxies into Store**

In `ReloadProxyPool` or the method that already loads `ListEnabledProxies`, replace/update the proxy map under the store mutex:

```go
func (s *Store) ReloadProxyPool() error {
	if s == nil || s.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	allProxies, err := s.db.ListProxies(ctx)
	if err != nil {
		return err
	}
	proxyByID := make(map[int64]managedProxy, len(allProxies))
	proxyPool := make([]string, 0, len(allProxies))
	for _, proxy := range allProxies {
		if proxy == nil || proxy.ID <= 0 {
			continue
		}
		url := strings.TrimSpace(proxy.URL)
		proxyByID[proxy.ID] = managedProxy{URL: url, Enabled: proxy.Enabled}
		if proxy.Enabled && url != "" {
			proxyPool = append(proxyPool, url)
		}
	}

	s.mu.Lock()
	s.proxyByID = proxyByID
	s.proxyPool = proxyPool
	s.mu.Unlock()
	return nil
}
```

If `ReloadProxyPool` already exists, merge this behavior into the existing implementation instead of duplicating it. Preserve existing proxy pool enabled/global proxy behavior.

- [ ] **Step 6: Resolve proxy ID before legacy URL**

Update `ResolveProxyForAccount` in `auth/store.go`:

```go
func (s *Store) ResolveProxyForAccount(account *Account) string {
	if account == nil {
		return ""
	}
	if proxyID := account.GetProxyID(); proxyID != nil {
		s.mu.RLock()
		proxy, ok := s.proxyByID[*proxyID]
		s.mu.RUnlock()
		if !ok || !proxy.Enabled {
			return ""
		}
		return strings.TrimSpace(proxy.URL)
	}
	if proxyURL := account.GetProxyURL(); proxyURL != "" {
		return proxyURL
	}
	// keep existing proxy pool / global proxy fallback exactly as it is today
}
```

Do not silently fall back when `proxyID` exists but is missing/disabled.

- [ ] **Step 7: Apply proxy binding updates in memory**

Replace or extend `ApplyAccountProxyURL` with a method that updates both fields:

```go
func (s *Store) ApplyAccountProxyBinding(id int64, proxyID *int64, proxyURL string) {
	account := s.FindByID(id)
	if account == nil {
		return
	}
	account.mu.Lock()
	if proxyID != nil && *proxyID > 0 {
		value := *proxyID
		account.ProxyID = &value
		account.ProxyURL = ""
	} else {
		account.ProxyID = nil
		account.ProxyURL = strings.TrimSpace(proxyURL)
	}
	account.mu.Unlock()
}
```

Keep `ApplyAccountProxyURL` as a wrapper for existing callers:

```go
func (s *Store) ApplyAccountProxyURL(id int64, proxyURL string) {
	s.ApplyAccountProxyBinding(id, nil, proxyURL)
}
```

- [ ] **Step 8: Run auth tests**

Run:

```bash
go test ./auth -run 'TestResolveProxyForAccount' -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit runtime proxy resolution**

```bash
git add auth/store.go auth/proxy_resolution_test.go
git commit -m "feat: resolve account proxy service bindings"
```

---

## Task 3: Validate proxy ID in admin account APIs

**Files:**
- Modify: `admin/handler.go`
- Modify: `admin/oauth.go`
- Modify: `admin/handler_test.go`
- Modify: `admin/oauth_test.go`

- [ ] **Step 1: Write failing admin API validation test**

Add to `admin/handler_test.go`:

```go
func TestAddAccountRejectsMissingProxyID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts", strings.NewReader(`{"refresh_token":"rt-test","proxy_id":999}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.AddAccount(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	assertErrorMessage(t, recorder, "代理服务不存在或已禁用")
}
```

Add another test for scheduler update:

```go
func TestUpdateAccountSchedulerAcceptsProxyID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	proxyURL := "http://proxy.example:8080"
	inserted, err := db.InsertProxies(ctx, []string{proxyURL}, "primary")
	if err != nil || inserted != 1 {
		t.Fatalf("InsertProxies inserted=%d err=%v", inserted, err)
	}
	proxies, err := db.ListProxies(ctx)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("ListProxies len=%d err=%v", len(proxies), err)
	}
	accountID, err := db.InsertAccountWithCredentials(ctx, "acct", map[string]interface{}{"access_token":"at"}, "http://legacy:8080")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(accountID, 10)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/1/scheduler", strings.NewReader(fmt.Sprintf(`{"proxy_id":%d,"proxy_url":null}`, proxies[0].ID)))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if !row.ProxyID.Valid || row.ProxyID.Int64 != proxies[0].ID {
		t.Fatalf("ProxyID = %+v, want %d", row.ProxyID, proxies[0].ID)
	}
	if row.ProxyURL != "" {
		t.Fatalf("ProxyURL = %q, want cleared", row.ProxyURL)
	}
}
```

- [ ] **Step 2: Run tests and verify they fail**

Run:

```bash
go test ./admin -run 'Test(AddAccountRejectsMissingProxyID|UpdateAccountSchedulerAcceptsProxyID)' -count=1
```

Expected: FAIL because request structs and persistence do not support `proxy_id` yet.

- [ ] **Step 3: Add optional proxy ID parsing helpers**

In `admin/handler.go`, add an optional int64 helper near other optional field parsers:

```go
type optionalProxyID struct {
	Set   bool
	Valid bool
	Value int64
}

func parseOptionalProxyID(raw json.RawMessage, field string) (optionalProxyID, error) {
	if len(raw) == 0 {
		return optionalProxyID{}, nil
	}
	if string(raw) == "null" {
		return optionalProxyID{Set: true, Valid: false}, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return optionalProxyID{}, fmt.Errorf("%s 必须是正整数或 null", field)
	}
	value, err := n.Int64()
	if err != nil || value <= 0 {
		return optionalProxyID{}, fmt.Errorf("%s 必须是正整数或 null", field)
	}
	return optionalProxyID{Set: true, Valid: true, Value: value}, nil
}
```

Add validator:

```go
func (h *Handler) validateEnabledProxyID(ctx context.Context, proxyID int64) error {
	if proxyID <= 0 {
		return fmt.Errorf("代理服务不存在或已禁用")
	}
	_, err := h.db.GetEnabledProxyByID(ctx, proxyID)
	if err != nil {
		return fmt.Errorf("代理服务不存在或已禁用")
	}
	return nil
}
```

- [ ] **Step 4: Extend request structs**

In `admin/handler.go`:

```go
type addAccountReq struct {
	Name         string          `json:"name"`
	RefreshToken string          `json:"refresh_token"`
	SessionToken string          `json:"session_token"`
	ProxyURL     string          `json:"proxy_url"`
	ProxyID      json.RawMessage `json:"proxy_id"`
}

type addATAccountReq struct {
	Name        string          `json:"name"`
	AccessToken string          `json:"access_token"`
	ProxyURL    string          `json:"proxy_url"`
	ProxyID     json.RawMessage `json:"proxy_id"`
}

type addOpenAIResponsesAccountReq struct {
	Name     string          `json:"name"`
	BaseURL  string          `json:"base_url"`
	APIKey   string          `json:"api_key"`
	Models   []string        `json:"models"`
	ProxyURL string          `json:"proxy_url"`
	ProxyID  json.RawMessage `json:"proxy_id"`
}

type updateAccountSchedulerReq struct {
	ScoreBiasOverride       json.RawMessage `json:"score_bias_override"`
	BaseConcurrencyOverride json.RawMessage `json:"base_concurrency_override"`
	SkipWarmTier            json.RawMessage `json:"skip_warm_tier"`
	AllowedAPIKeyIDs        json.RawMessage `json:"allowed_api_key_ids"`
	Tags                    json.RawMessage `json:"tags"`
	GroupIDs                json.RawMessage `json:"group_ids"`
	AutoPause5hThreshold    json.RawMessage `json:"auto_pause_5h_threshold"`
	AutoPause7dThreshold    json.RawMessage `json:"auto_pause_7d_threshold"`
	AutoPause5hDisabled     json.RawMessage `json:"auto_pause_5h_disabled"`
	AutoPause7dDisabled     json.RawMessage `json:"auto_pause_7d_disabled"`
	ProxyURL                *string         `json:"proxy_url"`
	ProxyID                 json.RawMessage `json:"proxy_id"`
}
```

- [ ] **Step 5: Use proxy ID in add account handlers**

In each add handler, parse and validate after sanitizing `ProxyURL`:

```go
proxyID, err := parseOptionalProxyID(req.ProxyID, "proxy_id")
if err != nil {
	writeError(c, http.StatusBadRequest, err.Error())
	return
}
var proxyIDPtr *int64
if proxyID.Set && proxyID.Valid {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.validateEnabledProxyID(ctx, proxyID.Value); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	value := proxyID.Value
	proxyIDPtr = &value
	req.ProxyURL = ""
}
```

Use new insert methods:

```go
id, err := h.db.InsertAccountWithCredentialsAndProxyID(ctx, name, tokenCredentialMap(seed), req.ProxyURL, proxyIDPtr)
```

When creating runtime account:

```go
newAcc := accountFromCredentialSeed(id, req.ProxyURL, seed)
if proxyIDPtr != nil {
	value := *proxyIDPtr
	newAcc.ProxyID = &value
	newAcc.ProxyURL = ""
}
```

For OpenAI Responses account runtime creation:

```go
account := &auth.Account{
	DBID:         id,
	ProxyURL:     req.ProxyURL,
	HealthTier:   auth.HealthTierHealthy,
	UpstreamType: auth.UpstreamOpenAIResponses,
	BaseURL:      baseURL,
	APIKey:       req.APIKey,
	Models:       models,
	Email:        baseURL,
	PlanType:     "api",
}
if proxyIDPtr != nil {
	value := *proxyIDPtr
	account.ProxyID = &value
	account.ProxyURL = ""
}
h.store.AddAccount(account)
```

- [ ] **Step 6: Use proxy ID in scheduler update**

Inside `UpdateAccountScheduler`, parse before DB update:

```go
proxyID, err := parseOptionalProxyID(req.ProxyID, "proxy_id")
if err != nil {
	writeError(c, http.StatusBadRequest, err.Error())
	return
}
if proxyID.Set && proxyID.Valid {
	if err := h.validateEnabledProxyID(ctx, proxyID.Value); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
}
```

Pass both `proxyURL` and `proxyID` to database update. If `UpdateAccountSchedulerMetadata` grows too many parameters, introduce a struct:

```go
type AccountSchedulerMetadataUpdate struct {
	ScoreBiasOverride       database.OptionalNullInt64
	BaseConcurrencyOverride database.OptionalNullInt64
	SkipWarmTier            database.OptionalBool
	AllowedAPIKeyIDs        database.OptionalInt64Slice
	Tags                    database.OptionalStringSlice
	GroupIDs                database.OptionalInt64Slice
	ProxyURL                database.OptionalString
	ProxyID                 database.OptionalNullInt64
	CredentialUpdates       map[string]interface{}
}
```

For the plan implementation, prefer minimal change: add a `proxyID database.OptionalNullInt64` parameter immediately after `proxyURL`.

Build `proxyIDUpdate`:

```go
proxyIDUpdate := database.OptionalNullInt64{}
if proxyID.Set {
	proxyIDUpdate.Set = true
	if proxyID.Valid {
		proxyIDUpdate.Value = proxyID.Value
	}
}
if proxyID.Valid {
	proxyURL = database.OptionalString{Set: true, Value: ""}
}
```

After DB update:

```go
if h.store != nil && (proxyID.Set || req.ProxyURL != nil) {
	if proxyID.Valid {
		value := proxyID.Value
		h.store.ApplyAccountProxyBinding(id, &value, "")
	} else if proxyID.Set && req.ProxyURL != nil && *req.ProxyURL == "" {
		h.store.ApplyAccountProxyBinding(id, nil, "")
	} else if req.ProxyURL != nil {
		h.store.ApplyAccountProxyBinding(id, nil, *req.ProxyURL)
	}
}
```

- [ ] **Step 7: Add proxy ID support to OAuth requests**

In `admin/oauth.go`, extend generate/exchange request structs with `ProxyID json.RawMessage` and store `ProxyID *int64` in `oauthSession`:

```go
type oauthSession struct {
	State        string
	CodeVerifier string
	RedirectURI  string
	ProxyURL     string
	ProxyID      *int64
	CreatedAt    time.Time
	// keep existing callback fields
}
```

During generate, validate proxy ID and store it:

```go
proxyID, err := parseOptionalProxyID(req.ProxyID, "proxy_id")
if err != nil {
	writeError(c, http.StatusBadRequest, err.Error())
	return
}
var proxyIDPtr *int64
if proxyID.Set && proxyID.Valid {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	proxyRow, err := h.db.GetEnabledProxyByID(ctx, proxyID.Value)
	if err != nil {
		writeError(c, http.StatusBadRequest, "代理服务不存在或已禁用")
		return
	}
	value := proxyRow.ID
	proxyIDPtr = &value
	req.ProxyURL = proxyRow.URL
}
```

During exchange, if a proxy ID exists, resolve current URL before token exchange:

```go
proxyURL := sess.ProxyURL
proxyIDPtr := sess.ProxyID
if proxyIDPtr != nil {
	proxyRow, err := h.db.GetEnabledProxyByID(c.Request.Context(), *proxyIDPtr)
	if err != nil {
		writeError(c, http.StatusBadRequest, "绑定的代理服务不存在或已禁用，请重新选择")
		return
	}
	proxyURL = proxyRow.URL
}
```

When inserting OAuth account, use `InsertAccountWithProxyID` and set runtime `ProxyID`.

- [ ] **Step 8: Run admin tests**

Run:

```bash
go test ./admin -run 'Test(AddAccountRejectsMissingProxyID|UpdateAccountSchedulerAcceptsProxyID)' -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit admin API proxy ID validation**

```bash
git add admin/handler.go admin/oauth.go admin/handler_test.go admin/oauth_test.go database/account_groups.go
git commit -m "feat: validate account proxy service selection"
```

---

## Task 4: Add proxy status to account responses

**Files:**
- Modify: `admin/responses.go` if account response types live there
- Modify: `admin/handler.go`
- Test: `admin/responses_test.go` or `admin/handler_test.go`

- [ ] **Step 1: Write failing response serialization test**

Add to `admin/responses_test.go` near existing account response tests:

```go
func TestAccountResponseIncludesProxyBindingStatus(t *testing.T) {
	row := &database.AccountRow{
		ID:       42,
		Name:     "bound-account",
		ProxyID:  sql.NullInt64{Int64: 12, Valid: true},
		ProxyURL: "",
	}
	proxyStatus := accountProxyStatus(row, map[int64]*database.ProxyRow{
		12: {ID: 12, URL: "http://proxy:8080", Enabled: true},
	})
	if proxyStatus != "bound" {
		t.Fatalf("proxyStatus = %q, want bound", proxyStatus)
	}
}
```

Also add cases for disabled, missing, legacy, and pool:

```go
func TestAccountProxyStatusCases(t *testing.T) {
	proxies := map[int64]*database.ProxyRow{
		1: {ID: 1, URL: "http://enabled", Enabled: true},
		2: {ID: 2, URL: "http://disabled", Enabled: false},
	}
	tests := []struct {
		name string
		row  *database.AccountRow
		want string
	}{
		{name: "pool", row: &database.AccountRow{}, want: "pool"},
		{name: "bound", row: &database.AccountRow{ProxyID: sql.NullInt64{Int64: 1, Valid: true}}, want: "bound"},
		{name: "disabled", row: &database.AccountRow{ProxyID: sql.NullInt64{Int64: 2, Valid: true}}, want: "disabled"},
		{name: "missing", row: &database.AccountRow{ProxyID: sql.NullInt64{Int64: 99, Valid: true}}, want: "missing"},
		{name: "legacy", row: &database.AccountRow{ProxyURL: "http://legacy"}, want: "legacy_custom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountProxyStatus(tc.row, proxies); got != tc.want {
				t.Fatalf("accountProxyStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests and verify they fail**

Run:

```bash
go test ./admin -run 'TestAccount.*Proxy' -count=1
```

Expected: FAIL because `accountProxyStatus` and response fields do not exist.

- [ ] **Step 3: Add response fields**

Where `accountResponse` is defined, add:

```go
ProxyURL    string `json:"proxy_url"`
ProxyID     *int64 `json:"proxy_id,omitempty"`
ProxyStatus string `json:"proxy_status"`
```

- [ ] **Step 4: Add proxy status helper**

In `admin/handler.go` near account response mapping helpers:

```go
func accountProxyStatus(row *database.AccountRow, proxies map[int64]*database.ProxyRow) string {
	if row == nil {
		return "pool"
	}
	if row.ProxyID.Valid && row.ProxyID.Int64 > 0 {
		proxy, ok := proxies[row.ProxyID.Int64]
		if !ok {
			return "missing"
		}
		if !proxy.Enabled {
			return "disabled"
		}
		return "bound"
	}
	if strings.TrimSpace(row.ProxyURL) != "" {
		return "legacy_custom"
	}
	return "pool"
}

func accountProxyID(row *database.AccountRow) *int64 {
	if row == nil || !row.ProxyID.Valid || row.ProxyID.Int64 <= 0 {
		return nil
	}
	value := row.ProxyID.Int64
	return &value
}
```

- [ ] **Step 5: Load proxy rows when listing accounts**

In `ListAccounts`, before mapping account rows to responses:

```go
proxyRows, err := h.db.ListProxies(ctx)
if err != nil {
	log.Printf("加载代理服务列表失败: %v", err)
	proxyRows = nil
}
proxyByID := make(map[int64]*database.ProxyRow, len(proxyRows))
for _, proxy := range proxyRows {
	if proxy != nil && proxy.ID > 0 {
		proxyByID[proxy.ID] = proxy
	}
}
```

In each account response assignment:

```go
ProxyURL:    row.ProxyURL,
ProxyID:     accountProxyID(row),
ProxyStatus: accountProxyStatus(row, proxyByID),
```

- [ ] **Step 6: Run response tests**

Run:

```bash
go test ./admin -run 'TestAccount.*Proxy' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit account response proxy status**

```bash
git add admin/handler.go admin/responses.go admin/responses_test.go admin/handler_test.go
git commit -m "feat: expose account proxy binding status"
```

---

## Task 5: Add reusable proxy pool test-all endpoint

**Files:**
- Modify: `admin/handler.go`
- Modify: `frontend/src/api.ts`
- Modify: `frontend/src/pages/Proxies.tsx`
- Test: `admin/handler_test.go`

- [ ] **Step 1: Write failing route/handler test**

Add to `admin/handler_test.go`:

```go
func TestTestAllProxiesReturnsNoProxyConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{})
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/proxies/test-all", strings.NewReader(`{"lang":"zh-CN"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.TestAllProxies(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	assertErrorMessage(t, recorder, "当前没有可测试的代理配置")
}
```

- [ ] **Step 2: Run test and verify it fails**

Run:

```bash
go test ./admin -run TestTestAllProxiesReturnsNoProxyConfiguration -count=1
```

Expected: FAIL because `TestAllProxies` does not exist.

- [ ] **Step 3: Extract single proxy test helper**

In `admin/handler.go`, factor the core of `TestProxy` into a helper:

```go
type proxyConnectivityResult struct {
	Success   bool   `json:"success"`
	IP        string `json:"ip,omitempty"`
	Country   string `json:"country,omitempty"`
	Region    string `json:"region,omitempty"`
	City      string `json:"city,omitempty"`
	ISP       string `json:"isp,omitempty"`
	LatencyMs int    `json:"latency_ms,omitempty"`
	Location  string `json:"location,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (h *Handler) testProxyConnectivity(ctx context.Context, proxyURL string, lang string) proxyConnectivityResult {
	// Move existing TestProxy network logic here without changing behavior.
	// Return proxyConnectivityResult instead of writing directly to gin.Context.
}
```

Then update `TestProxy` to call the helper and keep response JSON unchanged:

```go
result := h.testProxyConnectivity(c.Request.Context(), req.URL, req.Lang)
if result.Success && req.ID > 0 {
	_ = h.db.UpdateProxyTestResult(c.Request.Context(), req.ID, result.IP, result.Location, result.LatencyMs)
}
c.JSON(http.StatusOK, result)
```

- [ ] **Step 4: Add test-all handler**

In `admin/handler.go`:

```go
func (h *Handler) TestAllProxies(c *gin.Context) {
	var req struct {
		Lang string `json:"lang"`
	}
	_ = c.ShouldBindJSON(&req)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()

	proxies, err := h.db.ListEnabledProxies(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "获取代理列表失败")
		return
	}

	total := 0
	success := 0
	failed := 0
	if len(proxies) > 0 {
		for _, proxy := range proxies {
			if proxy == nil || strings.TrimSpace(proxy.URL) == "" {
				continue
			}
			total++
			result := h.testProxyConnectivity(ctx, proxy.URL, req.Lang)
			if result.Success {
				success++
				_ = h.db.UpdateProxyTestResult(ctx, proxy.ID, result.IP, result.Location, result.LatencyMs)
			} else {
				failed++
				_ = h.db.UpdateProxyTestResult(ctx, proxy.ID, "", "", 0)
			}
		}
	} else if h.store != nil && strings.TrimSpace(h.store.GetProxyURL()) != "" {
		total = 1
		result := h.testProxyConnectivity(ctx, h.store.GetProxyURL(), req.Lang)
		if result.Success {
			success = 1
		} else {
			failed = 1
		}
	}

	if total == 0 {
		writeError(c, http.StatusBadRequest, "当前没有可测试的代理配置")
		return
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "success": success, "failed": failed})
}
```

- [ ] **Step 5: Register route**

In `RegisterRoutes`, after `/proxies/test`:

```go
api.POST("/proxies/test-all", h.TestAllProxies)
```

- [ ] **Step 6: Add frontend API method**

In `frontend/src/api.ts`:

```ts
testAllProxies: (lang?: string) =>
  request<{ total: number; success: number; failed: number }>('/proxies/test-all', {
    method: 'POST',
    body: JSON.stringify({ lang }),
  }),
```

- [ ] **Step 7: Update Proxies page to reuse endpoint**

In `frontend/src/pages/Proxies.tsx`, change `handleTestAll` to call `api.testAllProxies(ipApiLang)` and reload after success:

```tsx
const handleTestAll = async () => {
  setTestAllLoading(true);
  setTestAllDone(0);
  setTestAllFailed(0);
  try {
    const result = await api.testAllProxies(ipApiLang);
    setTestAllDone(result.total);
    setTestAllFailed(result.failed);
    showToast(t("proxies.testAllComplete", { success: result.success, failed: result.failed }));
    await reload();
  } catch (error) {
    showToast(t("proxies.testAllFailed"), "error");
  } finally {
    setTestAllLoading(false);
  }
};
```

Keep existing per-row test behavior unchanged.

- [ ] **Step 8: Run admin and frontend checks**

Run:

```bash
go test ./admin -run 'Test(TestProxy|TestAllProxies)' -count=1
npm --prefix frontend run typecheck
```

Expected: both PASS.

- [ ] **Step 9: Commit proxy test-all endpoint**

```bash
git add admin/handler.go admin/handler_test.go frontend/src/api.ts frontend/src/pages/Proxies.tsx
git commit -m "feat: add reusable proxy pool test endpoint"
```

---

## Task 6: Add frontend types and proxy strategy selector

**Files:**
- Modify: `frontend/src/types.ts`
- Modify: `frontend/src/pages/Accounts.tsx`
- Modify: `frontend/src/locales/zh.json`
- Modify: `frontend/src/locales/en.json`

- [ ] **Step 1: Extend frontend types**

In `frontend/src/types.ts`, add type:

```ts
export type AccountProxyStatus = 'pool' | 'bound' | 'missing' | 'disabled' | 'legacy_custom'
```

Update `AccountRow`:

```ts
proxy_url: string
proxy_id?: number | null
proxy_status?: AccountProxyStatus
```

Update request types:

```ts
export interface AddAccountRequest {
  name?: string
  refresh_token?: string
  session_token?: string
  proxy_url?: string | null
  proxy_id?: number | null
}

export interface AddATAccountRequest {
  name?: string
  access_token: string
  proxy_url?: string | null
  proxy_id?: number | null
}

export interface AddOpenAIResponsesAccountRequest {
  name?: string
  base_url: string
  api_key: string
  models: string[]
  proxy_url?: string | null
  proxy_id?: number | null
}

export interface UpdateOpenAIResponsesAccountRequest {
  name?: string
  base_url: string
  api_key?: string
  models: string[]
  proxy_url?: string | null
  proxy_id?: number | null
}

export interface FetchOpenAIResponsesModelsRequest {
  account_id?: number
  base_url: string
  api_key: string
  proxy_url?: string | null
  proxy_id?: number | null
}

export interface UpdateAccountSchedulerRequest {
  score_bias_override?: number | null
  base_concurrency_override?: number | null
  skip_warm_tier?: boolean
  allowed_api_key_ids?: number[] | null
  proxy_url?: string | null
  proxy_id?: number | null
  tags?: string[] | null
  group_ids?: number[] | null
  auto_pause_5h_threshold?: number | null
  auto_pause_7d_threshold?: number | null
  auto_pause_5h_disabled?: boolean
  auto_pause_7d_disabled?: boolean
}
```

- [ ] **Step 2: Add proxy strategy state type in Accounts page**

At top of `frontend/src/pages/Accounts.tsx`, after constants:

```tsx
type ProxyStrategyValue =
  | { mode: "pool" }
  | { mode: "proxy"; proxyId: number }
  | { mode: "legacy"; proxyUrl: string };

function proxyStrategyToPayload(value: ProxyStrategyValue): { proxy_id?: number | null; proxy_url?: string | null } {
  if (value.mode === "proxy") return { proxy_id: value.proxyId, proxy_url: null };
  if (value.mode === "legacy") return { proxy_url: value.proxyUrl };
  return { proxy_id: null, proxy_url: null };
}

function proxyStrategyFromAccount(account: AccountRow): ProxyStrategyValue {
  if (account.proxy_id && account.proxy_id > 0) return { mode: "proxy", proxyId: account.proxy_id };
  if ((account.proxy_url || "").trim()) return { mode: "legacy", proxyUrl: account.proxy_url };
  return { mode: "pool" };
}
```

- [ ] **Step 3: Import ProxyRow and load proxies with accounts**

Change imports:

```tsx
import { api, getAdminKey, resetAdminAuthState, type ProxyRow } from "../api";
```

Add state:

```tsx
const [proxyServices, setProxyServices] = useState<ProxyRow[]>([]);
```

In `loadAccounts`, include `api.listProxies()` in the `Promise.all` and return it:

```tsx
const [accountsResponse, apiKeysResponse, opsOverview, groupsResponse, proxyResponse, settings] = await Promise.all([
  api.getAccounts(),
  api.getAPIKeys(),
  api.getOpsOverview().catch((): OpsOverviewResponse | null => null),
  api.listAccountGroups().catch(() => ({ groups: [] })),
  api.listProxies().catch(() => ({ proxies: [] as ProxyRow[] })),
  shouldLoadSettings ? api.getSettings().catch((): SystemSettings | null => null) : Promise.resolve<SystemSettings | null>(null),
]);
setProxyServices(proxyResponse.proxies ?? []);
```

- [ ] **Step 4: Replace add/edit proxy string state with strategy state**

Change initial add forms to not require `proxy_url` strings:

```tsx
const [addProxyStrategy, setAddProxyStrategy] = useState<ProxyStrategyValue>({ mode: "pool" });
const [atProxyStrategy, setAtProxyStrategy] = useState<ProxyStrategyValue>({ mode: "pool" });
const [openAIProxyStrategy, setOpenAIProxyStrategy] = useState<ProxyStrategyValue>({ mode: "pool" });
const [oauthProxyStrategy, setOauthProxyStrategy] = useState<ProxyStrategyValue>({ mode: "pool" });
const [editProxyStrategy, setEditProxyStrategy] = useState<ProxyStrategyValue>({ mode: "pool" });
```

Keep `addForm.proxy_url`, `atForm.proxy_url`, and `openAIForm.proxy_url` only until each submit path is updated. Then remove direct writes to those fields from proxy UI.

- [ ] **Step 5: Add selector component**

Replace existing `renderProxyInput` with this focused renderer:

```tsx
const renderProxyStrategySelector = ({
  value,
  onChange,
  testKey,
  label = t("accounts.proxyStrategy"),
  disabled = false,
}: {
  value: ProxyStrategyValue;
  onChange: (value: ProxyStrategyValue) => void;
  testKey: string;
  label?: string;
  disabled?: boolean;
}) => {
  const enabledProxies = proxyServices.filter((proxy) => proxy.enabled);
  const options = [
    { label: t("accounts.proxyStrategyPool"), value: "pool" },
    ...enabledProxies.map((proxy) => ({
      label: `${proxy.label || t("accounts.proxyUnnamedService")} · ${maskProxyUrl(proxy.url)}`,
      value: `proxy:${proxy.id}`,
    })),
    ...(value.mode === "legacy"
      ? [{ label: t("accounts.proxyStrategyLegacy", { url: maskProxyUrl(value.proxyUrl) }), value: "legacy" }]
      : []),
  ];
  const selectedValue = value.mode === "proxy" ? `proxy:${value.proxyId}` : value.mode;
  const isTesting = testingProxyKey === testKey;
  return (
    <div>
      <label className="block mb-2 text-sm font-semibold text-muted-foreground">{label}</label>
      <div className="flex flex-col gap-2 sm:flex-row">
        <Select
          value={selectedValue}
          onValueChange={(next) => {
            if (next === "pool") onChange({ mode: "pool" });
            else if (next === "legacy" && value.mode === "legacy") onChange(value);
            else if (next.startsWith("proxy:")) onChange({ mode: "proxy", proxyId: Number(next.slice(6)) });
          }}
          options={options}
          disabled={disabled}
          compact
        />
        <Button
          type="button"
          variant="outline"
          className="shrink-0 justify-center gap-1.5 sm:min-w-[108px]"
          disabled={disabled || testingProxyKey !== null}
          onClick={() => void handleTestProxyStrategy(value, testKey)}
        >
          <Zap className={`size-3.5 ${isTesting ? "animate-pulse" : ""}`} />
          {isTesting ? t("accounts.testingProxy") : t("accounts.testProxy")}
        </Button>
      </div>
      <p className="mt-1.5 text-xs text-muted-foreground">{t("accounts.proxyStrategyHint")}</p>
      {value.mode === "proxy" && !enabledProxies.some((proxy) => proxy.id === value.proxyId) ? (
        <p className="mt-1.5 text-xs font-medium text-destructive">{t("accounts.proxyStrategyUnavailable")}</p>
      ) : null}
    </div>
  );
};
```

Add helper:

```tsx
function maskProxyUrl(url: string): string {
  try {
    const parsed = new URL(url);
    const host = parsed.hostname.length > 6 ? `${parsed.hostname.slice(0, 3)}***${parsed.hostname.slice(-3)}` : "***";
    return `${parsed.protocol}//${parsed.username ? "***:***@" : ""}${host}${parsed.port ? `:${parsed.port}` : ""}`;
  } catch {
    return url.length > 16 ? `${url.slice(0, 8)}…${url.slice(-6)}` : url;
  }
}
```

- [ ] **Step 6: Add test handler for strategy**

In `Accounts.tsx`:

```tsx
const handleTestProxyStrategy = async (value: ProxyStrategyValue, testKey: string) => {
  if (testingProxyKey !== null) return;
  setTestingProxyKey(testKey);
  try {
    if (value.mode === "pool") {
      const result = await api.testAllProxies(ipApiLang);
      showToast(t("accounts.proxyPoolTestSuccess", { success: result.success, failed: result.failed }));
      return;
    }
    const proxyUrl = value.mode === "legacy"
      ? value.proxyUrl
      : proxyServices.find((proxy) => proxy.id === value.proxyId)?.url || "";
    if (!proxyUrl.trim()) {
      showToast(t("accounts.proxyStrategyUnavailable"), "error");
      return;
    }
    await handleTestProxyUrl(proxyUrl, testKey);
  } catch (error) {
    showToast(t("accounts.proxyTestFailed", { error: getErrorMessage(error) }), "error");
  } finally {
    setTestingProxyKey(null);
  }
};
```

Because `handleTestProxyUrl` currently manages `testingProxyKey`, refactor it into a lower-level `testSingleProxyUrl(url)` function so `handleTestProxyStrategy` does not double-set the loading state:

```tsx
const testSingleProxyUrl = async (rawUrl: string) => {
  const url = rawUrl.trim();
  if (!url) throw new Error(t("accounts.proxyUrlRequired"));
  const result = await api.testProxy(url, undefined, ipApiLang);
  if (!result.success) throw new Error(result.error || t("accounts.proxyTestUnknownError"));
  const location = result.location || [result.country, result.region, result.city].filter(Boolean).join(" ");
  showToast(t("accounts.proxyTestSuccess", { ip: result.ip || "-", location: location || "-", latency: result.latency_ms ?? 0 }));
};
```

- [ ] **Step 7: Add locale strings**

In `frontend/src/locales/zh.json` under `accounts`:

```json
"proxyStrategy": "代理策略",
"proxyStrategyPool": "全部代理池 / 默认代理策略",
"proxyStrategyHint": "不指定单个代理服务时，账号会使用系统代理池或全局代理配置。",
"proxyStrategyService": "指定代理服务",
"proxyStrategyLegacy": "历史自定义代理：{{url}}",
"proxyStrategyUnavailable": "当前绑定的代理服务不可用，请重新选择。",
"proxyUnnamedService": "未命名代理",
"proxyPoolTestSuccess": "代理池测试完成：成功 {{success}} 个，失败 {{failed}} 个",
"proxyNoConfigToTest": "当前没有可测试的代理配置"
```

In `frontend/src/locales/en.json` under `accounts`:

```json
"proxyStrategy": "Proxy strategy",
"proxyStrategyPool": "All proxy pool / default strategy",
"proxyStrategyHint": "When no single proxy service is selected, this account uses the system proxy pool or global proxy configuration.",
"proxyStrategyService": "Specific proxy service",
"proxyStrategyLegacy": "Legacy custom proxy: {{url}}",
"proxyStrategyUnavailable": "The currently bound proxy service is unavailable. Please choose another one.",
"proxyUnnamedService": "Unnamed proxy",
"proxyPoolTestSuccess": "Proxy pool test complete: {{success}} succeeded, {{failed}} failed",
"proxyNoConfigToTest": "No proxy configuration is available to test"
```

- [ ] **Step 8: Run frontend typecheck**

Run:

```bash
npm --prefix frontend run typecheck
```

Expected: likely FAIL until the submit paths are updated in Task 7; continue if errors mention stale `proxy_url` string requirements.

- [ ] **Step 9: Commit selector scaffolding only if typecheck passes**

If typecheck passes:

```bash
git add frontend/src/types.ts frontend/src/pages/Accounts.tsx frontend/src/locales/zh.json frontend/src/locales/en.json
git commit -m "feat: add account proxy strategy selector"
```

If typecheck fails due to submit paths, do not commit yet; finish Task 7 first and commit both frontend tasks together.

---

## Task 7: Wire proxy strategy selector into all account forms

**Files:**
- Modify: `frontend/src/pages/Accounts.tsx`
- Modify: `frontend/src/api.ts`

- [ ] **Step 1: Update add RT/ST submit payload**

In `handleAdd`, merge strategy payload:

```tsx
const payload: AddAccountRequest = {
  ...(credential === "st" ? { ...addForm, refresh_token: "" } : { ...addForm, session_token: "" }),
  ...proxyStrategyToPayload(addProxyStrategy),
};
```

After successful add, reset strategy:

```tsx
setAddForm({ refresh_token: "", session_token: "" });
setAddProxyStrategy({ mode: "pool" });
```

- [ ] **Step 2: Update AT submit payload**

In `handleAddAT`:

```tsx
await api.addATAccount({
  ...atForm,
  ...proxyStrategyToPayload(atProxyStrategy),
});
setAtForm({ access_token: "" });
setAtProxyStrategy({ mode: "pool" });
```

- [ ] **Step 3: Update OpenAI Responses add/fetch payloads**

In `handleFetchOpenAIModels`:

```tsx
const result = await api.fetchOpenAIResponsesModels({
  base_url: openAIForm.base_url,
  api_key: openAIForm.api_key,
  ...proxyStrategyToPayload(openAIProxyStrategy),
});
```

In `handleAddOpenAIResponses`:

```tsx
await api.addOpenAIResponsesAccount({
  ...openAIForm,
  models,
  ...proxyStrategyToPayload(openAIProxyStrategy),
});
setOpenAIProxyStrategy({ mode: "pool" });
```

- [ ] **Step 4: Update OAuth payloads**

In `startOAuthSession`:

```tsx
const result = await api.generateOAuthURL(proxyStrategyToPayload(oauthProxyStrategy));
```

In `handleOAuthComplete`:

```tsx
const result = await api.exchangeOAuthCode({
  session_id: oauthSession.session_id,
  code,
  state,
  name: oauthName.trim() || undefined,
  ...proxyStrategyToPayload(oauthProxyStrategy),
});
```

Reset `oauthProxyStrategy` to pool when closing add modal or after success.

- [ ] **Step 5: Update edit account scheduler payload**

When starting edit:

```tsx
setEditProxyStrategy(proxyStrategyFromAccount(account));
```

In `handleSaveSchedulerSettings`, replace `proxy_url: editProxyUrl.trim() || null` with:

```tsx
...proxyStrategyToPayload(editProxyStrategy),
```

If `editProxyStrategy.mode === "proxy"` and the proxy ID is not in enabled proxies, show error and return:

```tsx
if (editProxyStrategy.mode === "proxy" && !proxyServices.some((proxy) => proxy.enabled && proxy.id === editProxyStrategy.proxyId)) {
  showToast(t("accounts.proxyStrategyUnavailable"), "error");
  return;
}
```

- [ ] **Step 6: Update OpenAI Responses edit/fetch payloads**

When starting edit for OpenAI account:

```tsx
setEditProxyStrategy(proxyStrategyFromAccount(account));
```

In `handleFetchEditOpenAIModels`:

```tsx
const result = await api.fetchOpenAIResponsesModels({
  account_id: editingAccount.id,
  base_url: editOpenAIForm.base_url,
  api_key: editOpenAIForm.api_key ?? "",
  ...proxyStrategyToPayload(editProxyStrategy),
});
```

In `handleSaveOpenAIAccountSettings`:

```tsx
await api.updateOpenAIResponsesAccount(editingAccount.id, {
  ...editOpenAIForm,
  api_key: editOpenAIForm.api_key?.trim() || undefined,
  ...proxyStrategyToPayload(editProxyStrategy),
});
```

- [ ] **Step 7: Replace all renderProxyInput call sites**

RT/ST:

```tsx
{renderProxyStrategySelector({
  value: addProxyStrategy,
  testKey: "add-refresh-token",
  onChange: setAddProxyStrategy,
})}
```

AT:

```tsx
{renderProxyStrategySelector({
  value: atProxyStrategy,
  testKey: "add-access-token",
  onChange: setAtProxyStrategy,
})}
```

OpenAI add:

```tsx
{renderProxyStrategySelector({
  value: openAIProxyStrategy,
  testKey: "add-openai-responses",
  onChange: setOpenAIProxyStrategy,
})}
```

OAuth:

```tsx
{renderProxyStrategySelector({
  value: oauthProxyStrategy,
  testKey: "oauth-generate",
  label: t("accounts.proxyStrategy"),
  onChange: setOauthProxyStrategy,
})}
```

Edit account tab:

```tsx
{renderProxyStrategySelector({
  value: editProxyStrategy,
  testKey: `edit-account-${editingAccount.id}`,
  onChange: setEditProxyStrategy,
})}
```

- [ ] **Step 8: Remove obsolete proxy URL state**

Remove:

```tsx
const [editProxyUrl, setEditProxyUrl] = useState("");
const [oauthProxyUrl, setOauthProxyUrl] = useState("");
```

Remove resets for those states and remove direct `proxy_url` writes from add forms.

- [ ] **Step 9: Run frontend checks**

Run:

```bash
npm --prefix frontend run typecheck
npm --prefix frontend run build
```

Expected: PASS. Build may still print the existing `fatal: No names found, cannot describe anything.` warning but must finish successfully.

- [ ] **Step 10: Commit frontend wiring**

```bash
git add frontend/src/types.ts frontend/src/api.ts frontend/src/pages/Accounts.tsx frontend/src/locales/zh.json frontend/src/locales/en.json
git commit -m "feat: wire account proxy strategy selection"
```

---

## Task 8: End-to-end verification and cleanup

**Files:**
- Verify all touched backend/frontend files
- Optional docs: update changelog only if project convention requires feature entries before release

- [ ] **Step 1: Run backend targeted tests**

Run:

```bash
go test ./database ./auth ./admin -count=1
```

Expected: PASS.

- [ ] **Step 2: Run full Go tests if targeted tests pass**

Run:

```bash
go test ./... -count=1
```

Expected: PASS. If unrelated packages fail, record exact failing package and output before deciding whether to fix or report.

- [ ] **Step 3: Run frontend checks**

Run:

```bash
npm --prefix frontend run typecheck
npm --prefix frontend run build
```

Expected: PASS. Existing `git describe` warning during build is acceptable only if Vite build completes.

- [ ] **Step 4: Manual browser validation**

Start the app according to the repository's normal dev workflow, then validate:

1. Add account modal shows `代理策略`, not manual proxy URL input.
2. Default selection is `全部代理池 / 默认代理策略`.
3. Selecting a specific enabled proxy sends `proxy_id` and no custom `proxy_url`.
4. Editing an account bound to a proxy service shows that service selected.
5. Editing a legacy custom `proxy_url` account shows `历史自定义代理：<masked-url>`.
6. `测试代理` on a specific proxy calls single proxy test.
7. `测试代理` on pool/default calls test-all and shows success/failed summary.
8. Disabling a bound proxy in proxy management makes the account show unavailable status.

- [ ] **Step 5: Inspect diff for accidental unrelated changes**

Run:

```bash
git status --short
git diff --stat main...HEAD
git diff --check
```

Expected: only files from this plan and no whitespace errors. The two existing untracked code review docs may still appear; do not add them.

- [ ] **Step 6: Final commit if verification changes were needed**

If Task 8 required fixes, commit them:

```bash
git add <only-fixed-files>
git commit -m "fix: complete account proxy service selection verification"
```

If no fixes were needed, do not create an empty commit.

---

## Self-Review Checklist

- Spec coverage:
  - Data model: Tasks 1-4.
  - Proxy ID follows proxy URL changes: Tasks 1-3 and Task 2 runtime resolution.
  - Pool/default strategy: Tasks 2, 6, 7.
  - Test proxy retained for pool: Task 5 and Task 6.
  - Legacy custom proxy compatibility: Tasks 2, 4, 6, 7.
  - Disabled/missing proxy does not silently fall back: Tasks 2, 4, 7.
  - Frontend dropdown and copy: Tasks 6-7.
  - Verification: Task 8.
- Placeholder scan: no TBD/TODO/fill-in instructions. Where existing code shape may vary, the plan instructs merging into named existing functions while preserving behavior.
- Type consistency: uses `proxy_id`, `proxy_url`, `proxy_status`, `ProxyID`, `ProxyURL`, `ProxyStatus`, `managedProxy`, `ProxyStrategyValue`, and `testAllProxies` consistently.
