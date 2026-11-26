package auth

import (
	"context"
	"testing"
	"time"
)

func TestCodexRestartPreservesUnauthorizedUntilVerified(t *testing.T) {
	ctx := context.Background()
	db := newProxyPoolReloadTestDB(t)
	deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	add := func(reason string) int64 {
		t.Helper()
		id, err := db.InsertAccountWithCredentials(ctx, reason, map[string]any{
			"access_token": "test-at", "refresh_token": "test-" + reason,
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetCooldownWithError(ctx, id, reason, deadline, "previous rejection"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	unauthorized, limited := add("unauthorized"), add("rate_limited")
	terminal := add("other")
	if err := db.SetError(ctx, terminal, "deactivated_workspace"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCooldown(ctx, terminal, "rate_limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	a := store.FindByID(unauthorized)
	if a == nil || a.RuntimeStatus() != "unauthorized" || a.IsAvailable() || !a.NeedsRecoveryProbe(0) || a.ErrorMsg != "previous rejection" {
		t.Fatal("expired 401 must stay quarantined and eligible for verification")
	}
	row, err := db.GetAccountByID(ctx, unauthorized)
	if err != nil || row.CooldownReason != "unauthorized" || !row.CooldownUntil.Time.Equal(deadline) {
		t.Fatal("startup erased or extended the persisted 401", err)
	}
	if !store.FindByID(limited).IsAvailable() || store.FindByID(terminal).RuntimeStatus() != "error" {
		t.Fatal("expired quota cooldown should release; terminal error must not")
	}
	calls := 0
	store.SetUsageProbeFunc(func(_ context.Context, account *Account) error {
		calls++
		if calls == 1 {
			store.MarkUpstreamUnauthorized(account, nil, "still unauthorized")
		} else {
			store.ReportRequestSuccess(account, 0)
		}
		return nil // The probe reports handled rejections as well as successes this way.
	})
	store.parallelRecoveryProbe(ctx)
	if calls != 1 || a.IsAvailable() || a.RuntimeStatus() != "unauthorized" {
		t.Fatal("handled 401 incorrectly restored account")
	}
	a.SetCooldownUntil(deadline, "unauthorized")
	a.mu.Lock()
	a.LastRecoveryProbeAt = time.Time{}
	a.mu.Unlock()
	store.parallelRecoveryProbe(ctx)
	if calls != 2 || !a.IsAvailable() || a.RuntimeStatus() != "active" {
		t.Fatal("verified recovery did not restore dispatch")
	}
}
