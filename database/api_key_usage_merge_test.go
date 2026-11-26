package database

import (
	"math"
	"testing"
	"time"
)

func TestScopeWindowUsageExcludesDispatchTerminalDiagnostics(t *testing.T) {
	db := newManualFlushTestDB(t, "sqlite")
	ctx := t.Context()
	const keyID, accountID = 925, 11
	const diagnostic = "dispatch_terminal_diagnostic"
	const model = "gpt-5.4"
	now := time.Now()
	for _, event := range []struct {
		mode, reason   string
		status, tokens int
	}{
		{UsageLogModeFull, "", 200, 1000},
		{UsageLogModeFull, diagnostic, 503, 3000},
		{UsageLogModeFull, "", 499, 4000},
		{UsageLogModeOff, "overflow_compact_summary", 200, 2000},
		{UsageLogModeOff, diagnostic, 503, 5000},
	} {
		db.SetUsageLogConfig(event.mode, 100, 5)
		err := db.InsertUsageLog(ctx, &UsageLogInput{
			APIKeyID: keyID, AccountID: accountID, Model: model, InternalReason: event.reason,
			StatusCode: event.status, InputTokens: event.tokens, TotalTokens: event.tokens, CreatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	var diagnostics int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE internal_reason = $1`, diagnostic).Scan(&diagnostics); err != nil || diagnostics != 1 {
		t.Fatalf("retained diagnostic rows = %d, want 1 (err: %v)", diagnostics, err)
	}

	all, err := db.GetAPIKeyAccountWindowsUsage(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	single, err := db.GetAPIKeyAccountWindowUsage(ctx, keyID, 5*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := db.GetAPIKeysAccountWindowUsage(ctx, []int64{keyID}, 5*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wantCost := calculateCost(3000, 0, 0, model, "")
	for name, got := range map[string]APIKeyWindowUsage{
		"single": single[accountID], "all windows": all["5h"][accountID], "batch": batch[keyID][accountID],
	} {
		if got.Requests != 2 || got.Tokens != 3000 || math.Abs(got.UserBilled-wantCost) > 1e-12 {
			t.Errorf("%s: got %+v, want 2 requests, 3000 tokens and %g cost", name, got, wantCost)
		}
	}
}
