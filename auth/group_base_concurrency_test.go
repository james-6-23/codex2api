package auth

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
)

func TestGroupBaseConcurrencyInheritanceAndHotUpdates(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 8})
	t.Cleanup(store.Stop)
	account := &Account{DBID: 1, AccessToken: "access-token"}
	store.AddAccount(account)

	groupFive := int64(5)
	groupThree := int64(3)
	store.SetGroupBaseConcurrencyOverride(10, &groupFive)
	store.SetGroupBaseConcurrencyOverride(20, &groupThree)

	store.ApplyAccountGroups(account.DBID, []int64{10})
	assertAccountConcurrency(t, account, 5)

	store.ApplyAccountGroups(account.DBID, []int64{10, 20})
	assertAccountConcurrency(t, account, 3)

	accountOverride := int64(6)
	store.ApplyAccountSchedulerOverridePatch(account.DBID, false, nil, true, &accountOverride, nil)
	assertAccountConcurrency(t, account, 6)

	groupTwo := int64(2)
	store.SetGroupBaseConcurrencyOverride(20, &groupTwo)
	assertAccountConcurrency(t, account, 6)

	store.ApplyAccountSchedulerOverridePatch(account.DBID, false, nil, true, nil, nil)
	assertAccountConcurrency(t, account, 2)

	store.ApplyAccountGroups(account.DBID, []int64{10})
	assertAccountConcurrency(t, account, 5)

	store.DeleteGroupBaseConcurrencyOverride(10)
	assertAccountConcurrency(t, account, 8)
}

func TestApplyAccountGroupMembershipsRecomputesConcurrency(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 9})
	t.Cleanup(store.Stop)
	first := &Account{DBID: 1, AccessToken: "first-token"}
	second := &Account{DBID: 2, AccessToken: "second-token"}
	store.AddAccount(first)
	store.AddAccount(second)

	groupLimit := int64(4)
	store.SetGroupBaseConcurrencyOverride(7, &groupLimit)
	store.ApplyAccountGroupMemberships(map[int64][]int64{1: {7}})

	assertAccountConcurrency(t, first, 4)
	assertAccountConcurrency(t, second, 9)

	groupLimit = 2
	store.SetGroupBaseConcurrencyOverride(7, &groupLimit)
	assertAccountConcurrency(t, first, 2)
	assertAccountConcurrency(t, second, 9)

	store.ApplyAccountGroupMemberships(map[int64][]int64{2: {7}})
	assertAccountConcurrency(t, first, 9)
	assertAccountConcurrency(t, second, 2)
}

func TestGroupBaseConcurrencyHotUpdateRefreshesFastScheduler(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 8, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	account := &Account{DBID: 1, AccessToken: "access-token"}
	store.AddAccount(account)
	store.ApplyAccountGroups(account.DBID, []int64{10})

	limitTwo := int64(2)
	store.SetGroupBaseConcurrencyOverride(10, &limitTwo)
	first := store.Next()
	second := store.Next()
	if first == nil || second == nil {
		t.Fatalf("first=%v second=%v, want two fast-scheduler acquisitions", first, second)
	}
	if third := store.Next(); third != nil {
		store.Release(third)
		t.Fatal("third acquisition should be blocked by group limit 2")
	}

	limitThree := int64(3)
	store.SetGroupBaseConcurrencyOverride(10, &limitThree)
	third := store.Next()
	if third == nil {
		t.Fatal("raising group limit to 3 should immediately allow another acquisition")
	}

	limitOne := int64(1)
	store.SetGroupBaseConcurrencyOverride(10, &limitOne)
	if extra := store.Next(); extra != nil {
		store.Release(extra)
		t.Fatal("lowering group limit below inflight count should block new acquisitions")
	}

	store.Release(first)
	store.Release(second)
	store.Release(third)
	acquired := store.Next()
	if acquired == nil {
		t.Fatal("group limit 1 should allow an acquisition after inflight requests drain")
	}
	store.Release(acquired)
}

func TestStoreInitLoadsGroupBaseConcurrencyOverrides(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	accountID, err := db.InsertAccount(ctx, "grouped", "refresh-token", "")
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	groupSix, err := db.CreateAccountGroup(ctx, "six", "", "", 0, 0, sql.NullInt64{Int64: 6, Valid: true})
	if err != nil {
		t.Fatalf("CreateAccountGroup six: %v", err)
	}
	groupThree, err := db.CreateAccountGroup(ctx, "three", "", "", 0, 0, sql.NullInt64{Int64: 3, Valid: true})
	if err != nil {
		t.Fatalf("CreateAccountGroup three: %v", err)
	}
	if err := db.SetAccountGroups(ctx, accountID, []int64{groupSix, groupThree}); err != nil {
		t.Fatalf("SetAccountGroups: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 8})
	t.Cleanup(store.Stop)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	account := store.FindByID(accountID)
	if account == nil {
		t.Fatalf("runtime account %d not loaded", accountID)
	}
	assertAccountConcurrency(t, account, 3)
}

func assertAccountConcurrency(t *testing.T, account *Account, want int64) {
	t.Helper()
	if got := account.GetBaseConcurrencyEffective(); got != want {
		t.Fatalf("BaseConcurrencyEffective = %d, want %d", got, want)
	}
	if got := account.GetDynamicConcurrencyLimit(); got != want {
		t.Fatalf("DynamicConcurrencyLimit = %d, want %d", got, want)
	}
}
