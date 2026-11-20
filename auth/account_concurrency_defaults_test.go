package auth

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"testing"

	"github.com/codex2api/database"
)

func TestAccountConcurrencyDefaultsPrecedenceAndChannelIsolation(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 9})
	t.Cleanup(store.Stop)
	store.SetClaudeSessionWindowLimit(4)
	codex := &Account{DBID: 1, AccessToken: "codex", PlanType: " SELF_SERVE_BUSINESS_PROLITE "}
	claude := store.buildAccountFromRow(context.Background(), &database.AccountRow{ID: 2, Enabled: true, Credentials: map[string]any{
		"upstream_type": UpstreamClaude, "access_token": "claude", "plan_type": "pro",
	}}, nil)
	if claude == nil {
		t.Fatal("Claude row did not build")
	}
	store.AddAccounts([]*Account{codex, claude})
	store.SetAccountConcurrencyDefaults([]database.AccountConcurrencyRule{
		{Channel: "codex", Plan: "team5x", Concurrency: 2},
		{Channel: "codex", Plan: "team", Concurrency: 3},
		{Channel: "codex", Plan: "prolite", Concurrency: 6},
		{Channel: "codex", Plan: "pro", Concurrency: 7},
	})
	assertAccountConcurrency(t, codex, 2)
	assertAccountConcurrency(t, claude, 4)

	store.ApplyAccountGroups(codex.DBID, []int64{10, 20})
	store.SetGroupBaseConcurrencyOverride(10, int64Ptr(5))
	store.SetGroupBaseConcurrencyOverride(20, int64Ptr(3))
	assertAccountConcurrency(t, codex, 3)
	store.ApplyAccountSchedulerOverridePatch(codex.DBID, false, nil, true, int64Ptr(8), nil)
	assertAccountConcurrency(t, codex, 8)
	store.ApplyAccountSchedulerOverridePatch(codex.DBID, false, nil, true, nil, nil)
	assertAccountConcurrency(t, codex, 3)
	store.ApplyAccountGroups(codex.DBID, nil)
	assertAccountConcurrency(t, codex, 2)

	store.SetAccountConcurrencyDefaults([]database.AccountConcurrencyRule{
		{Channel: "claude", Plan: "CLAUDE-PRO", Concurrency: 5},
	})
	assertAccountConcurrency(t, codex, 9)
	assertAccountConcurrency(t, claude, 5)
	store.SetAccountConcurrencyDefaults(nil)
	assertAccountConcurrency(t, claude, 4)
}

func TestAccountConcurrencyDefaultsRefreshFastSchedulerOnPlanChangeAndClear(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 3, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	store.SetAccountConcurrencyDefaults([]database.AccountConcurrencyRule{
		{Channel: "codex", Plan: "plus", Concurrency: 1},
		{Channel: "codex", Plan: "pro-lite", Concurrency: 2},
	})
	account := &Account{DBID: 1, AccessToken: "synthetic", PlanType: "plus"}
	store.AddAccount(account)
	acquire := func() {
		t.Helper()
		got := store.Next()
		if got != account {
			t.Fatalf("Next() = %p, want %p", got, account)
		}
		t.Cleanup(func() { store.Release(got) })
	}
	assertFull := func() {
		t.Helper()
		if got := store.Next(); got != nil {
			store.Release(got)
			t.Fatalf("Next() = %p at the concurrency cap", got)
		}
	}
	acquire()
	assertFull()
	generation := store.routingGeneration.Load()
	if !store.ObserveUsageFramePlan(account, "pro_lite") {
		t.Fatal("expected runtime plan change")
	}
	if store.routingGeneration.Load() == generation {
		t.Fatal("plan change did not invalidate routing")
	}
	assertAccountConcurrency(t, account, 2)
	acquire()
	assertFull()
	generation = store.routingGeneration.Load()
	store.SetAccountConcurrencyDefaults(nil)
	if store.routingGeneration.Load() == generation {
		t.Fatal("clearing defaults did not invalidate routing")
	}
	assertAccountConcurrency(t, account, 3)
	acquire()
	assertFull()
}

func TestAccountConcurrencyDefaultsPersistedProjectionAndChoices(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 9})
	t.Cleanup(store.Stop)
	store.SetAccountConcurrencyDefaults([]database.AccountConcurrencyRule{
		{Channel: "codex", Plan: "team", Concurrency: 4},
		{Channel: "codex", Plan: "team5x", Concurrency: 2},
	})
	row := &database.AccountRow{ID: 1, Enabled: true, Credentials: map[string]any{
		"refresh_token": "synthetic", "plan_type": "self_serve_business_prolite",
	}}
	if got := store.AccountRowBaseConcurrency(row); got != 2 {
		t.Fatalf("row fallback = %d, want 2", got)
	}
	row.BaseConcurrencyOverride = sql.NullInt64{Int64: 6, Valid: true}
	if got := store.AccountRowBaseConcurrency(row); got != 6 {
		t.Fatalf("row override = %d, want 6", got)
	}
	row.BaseConcurrencyOverride = sql.NullInt64{}
	account := store.buildAccountFromRow(context.Background(), row, nil)
	if account == nil || account.GetBaseConcurrencyEffective() != 2 {
		t.Fatal("refresh-only account did not inherit its persisted plan's rule")
	}
	store.AddAccounts([]*Account{account,
		{DBID: 2, PlanType: "TEAM5X"},
		{DBID: 3, PlanType: "team"},
		{DBID: 4, UpstreamType: UpstreamClaude, PlanType: "CLAUDE-PRO"},
	})
	want := map[string][]string{"codex": {"team", "team5x"}, "claude": {"pro"}}
	if got := store.AccountConcurrencyPlanChoices(); !maps.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("AccountConcurrencyPlanChoices() = %v, want %v", got, want)
	}
	row.Credentials["plan_type"] = "team"
	fresh := store.buildAccountFromRow(context.Background(), row, nil)
	if fresh == nil {
		t.Fatal("changed row did not build")
	}
	store.applyPersistentAccountSnapshot(account, fresh, row.Enabled)
	if got := account.GetBaseConcurrencyEffective(); got != 4 {
		t.Fatalf("outbox plan change projected concurrency %d, want 4", got)
	}
}
