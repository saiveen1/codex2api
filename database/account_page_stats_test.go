package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountUsageWindowsFastBilledMatchesAccountCost(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "window-account", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_generation = 2 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	shortSince, longSince := now.Add(-5*time.Hour), now.AddDate(0, 0, -30)
	for _, row := range []struct {
		name, serviceTier string
		billingTier       any
		createdAt         time.Time
		accountID         int64
		status            int
		retry             any
		internal          string
		generation        int64
		accountBilled     float64
	}{
		{"billing tier wins", "default", "priority", shortSince, id, 200, 0, "", 2, 1.25},
		{"legacy empty billing tier and null retry", "FaSt", "", now, id, 200, nil, "", 0, 2.50},
		{"billing tier suppresses legacy fast", "fast", "default", now, id, 200, 0, "", 0, 4},
		{"no account cost despite user cost", "default", "fast", now, id, 200, 0, "", 0, 0},
		{"legacy null billing tier in long window", "ultrafast", nil, now.AddDate(0, 0, -20), id, 200, 0, "", 0, 8},
		{"retry", "default", "fast", now, id, 502, 1, "", 0, 100},
		{"cancellation", "default", "fast", now, id, 499, 0, "", 0, 100},
		{"probe", "default", "fast", now, id, 200, 0, "grok_capability_probe", 2, 100},
		{"old credential generation", "default", "fast", now, id, 200, 0, "", 1, 100},
		{"outside long window", "default", "fast", longSince.Add(-time.Second), id, 200, 0, "", 0, 100},
		{"another account", "default", "fast", now, id + 1, 200, 0, "", 0, 100},
	} {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
			(account_id, status_code, total_tokens, account_billed, user_billed, service_tier,
			 billing_service_tier, created_at, is_retry_attempt, internal_reason, credential_generation)
			VALUES ($1, $2, 10, $3, 50, $4, $5, $6, $7, $8, $9)`,
			row.accountID, row.status, row.accountBilled, row.serviceTier, row.billingTier, db.timeArg(row.createdAt),
			row.retry, row.internal, row.generation); err != nil {
			t.Fatalf("insert %s: %v", row.name, err)
		}
	}

	wantShort := AccountTimeRangeUsage{AccountID: id, Requests: 4, Tokens: 40, AccountBilled: 7.75, UserBilled: 200, FastBilled: 3.75}
	wantLong := AccountTimeRangeUsage{AccountID: id, Requests: 5, Tokens: 50, AccountBilled: 15.75, UserBilled: 250, FastBilled: 11.75}
	check := func(label string, rows map[int64]*AccountTimeRangeUsage, want AccountTimeRangeUsage, scoped bool) {
		t.Helper()
		if got := rows[id]; got == nil || *got != want {
			t.Errorf("%s = %+v, want %+v", label, got, want)
		}
		if scoped && len(rows) != 1 {
			t.Errorf("%s matched %d accounts, want 1", label, len(rows))
		}
	}

	for _, window := range []struct {
		name  string
		since time.Time
		want  AccountTimeRangeUsage
	}{{"5h", shortSince, wantShort}, {"long", longSince, wantLong}} {
		got, err := db.GetAccountUsageSinceWindows(ctx, map[int64]time.Time{id: window.since})
		if err != nil {
			t.Fatalf("GetAccountUsageSinceWindows: %v", err)
		}
		check(window.name+" per account", got, window.want, true)
	}

	short, long, err := db.GetAccountUsageWindowsByIDs(ctx, []int64{id}, shortSince, longSince)
	if err != nil {
		t.Fatalf("GetAccountUsageWindowsByIDs: %v", err)
	}
	check("5h by IDs", short, wantShort, true)
	check("long by IDs", long, wantLong, true)

	short, long, err = db.GetAccountUsageWindows(ctx, shortSince, longSince)
	if err != nil {
		t.Fatalf("GetAccountUsageWindows: %v", err)
	}
	check("5h global", short, wantShort, false)
	check("long global", long, wantLong, false)

	rangeUsage, err := db.GetAccountTimeRangeUsage(ctx, longSince)
	if err != nil {
		t.Fatalf("GetAccountTimeRangeUsage: %v", err)
	}
	check("single range", rangeUsage, wantLong, false)
}
