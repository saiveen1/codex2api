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
	startStore := func() *Store {
		t.Helper()
		s := NewStore(db, nil, nil)
		t.Cleanup(s.Stop)
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	store := startStore()
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
	probe := func(_ context.Context, account *Account) error {
		calls++
		if calls == 1 {
			store.MarkUpstreamUnauthorized(account, nil, "still unauthorized")
		} else {
			store.ReportRequestSuccess(account, 0)
		}
		return nil // The probe reports handled rejections as well as successes this way.
	}
	store.SetUsageProbeFunc(probe)
	store.parallelRecoveryProbe(ctx)
	if calls != 1 || a.IsAvailable() || a.RuntimeStatus() != "unauthorized" {
		t.Fatal("handled 401 incorrectly restored account")
	}
	store.Stop()
	row, err = db.GetAccountByID(ctx, unauthorized)
	if err != nil || row.CooldownReason != "unauthorized" || !row.CooldownUntil.Time.After(time.Now()) {
		t.Fatal("rejected recovery probe must persist a new 401 deadline", err)
	}
	// Let the persisted cooldown expire between processes, not just in memory.
	if err := db.SetCooldown(ctx, unauthorized, "unauthorized", deadline); err != nil {
		t.Fatal(err)
	}
	store = startStore()
	a = store.FindByID(unauthorized)
	if a == nil || a.IsAvailable() || !a.NeedsRecoveryProbe(0) {
		t.Fatal("restart after another 401 must still require verification")
	}
	store.SetUsageProbeFunc(probe)
	store.parallelRecoveryProbe(ctx)
	if calls != 2 || !a.IsAvailable() || a.RuntimeStatus() != "active" {
		reason, until := a.GetCooldownSnapshot()
		t.Fatalf("verified recovery: calls=%d status=%s available=%v reason=%q until=%v", calls, a.RuntimeStatus(), a.IsAvailable(), reason, until)
	}
	row, err = db.GetAccountByID(ctx, unauthorized)
	if err != nil || row.CooldownReason != "" || row.CooldownUntil.Valid {
		t.Fatal("verified recovery did not clear persisted cooldown", err)
	}
}
