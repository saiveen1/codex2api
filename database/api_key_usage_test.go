package database

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageStatsGroupRenamedAPIKeysByID(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	testUsageStatsGroupRenamedAPIKeysByID(t, db)
}

func testUsageStatsGroupRenamedAPIKeysByID(t *testing.T, db *DB) {
	t.Helper()
	ctx := t.Context()
	keyID, err := db.InsertAPIKey(ctx, "First name", "sk-test-rename-first")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := db.InsertAPIKey(ctx, "Current name", "sk-test-rename-other")
	if err != nil {
		t.Fatal(err)
	}

	const accountID = 7
	start := time.Now().Truncate(time.Hour).Add(-3 * time.Hour)
	end := start.Add(2*time.Hour + time.Minute)
	logs := []UsageLogInput{
		{APIKeyID: keyID, APIKeyName: "First name", TotalTokens: 100, InputTokens: 70, OutputTokens: 30, CachedTokens: 10, StatusCode: 200, CreatedAt: start.Add(time.Minute)},
		{APIKeyID: keyID, APIKeyName: "Second name", TotalTokens: 200, InputTokens: 150, OutputTokens: 50, CachedTokens: 20, StatusCode: 500, CreatedAt: start.Add(2*time.Hour + 30*time.Second)},
		{APIKeyID: otherID, APIKeyName: "Current name", TotalTokens: 50, StatusCode: 200, CreatedAt: start.Add(time.Minute)},
		{APIKeyMasked: "sk-env-a", TotalTokens: 25, StatusCode: 200, CreatedAt: start.Add(time.Minute)},
		{APIKeyMasked: "sk-env-b", TotalTokens: 20, StatusCode: 200, CreatedAt: start.Add(time.Minute)},
		{APIKeyID: keyID, APIKeyName: "Cancelled", TotalTokens: 1000, StatusCode: 499, CreatedAt: start.Add(2*time.Hour + 40*time.Second)},
	}
	for _, entry := range logs {
		entry.AccountID = accountID
		entry.Model = "gpt-5.5"
		if err := db.InsertUsageLog(ctx, &entry); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	if err := db.UpdateAPIKeyName(ctx, keyID, "Current name"); err != nil {
		t.Fatal(err)
	}

	assertStats := func(name string) {
		t.Helper()
		t.Run(name+"/token totals", func(t *testing.T) {
			items, err := db.ListAPIKeyTokenStats(ctx, start, end)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 4 || items[1].APIKeyID != otherID {
				t.Fatalf("token stats should contain both distinct IDs and both untracked keys: %+v", items)
			}
			if got := items[0]; got.APIKeyID != keyID || got.APIKeyName != name || got.Label != name ||
				got.Requests != 2 || got.TotalTokens != 300 || got.InputTokens != 220 || got.OutputTokens != 80 ||
				got.CachedTokens != 30 || got.ErrorCount != 1 {
				t.Fatalf("token stats for renamed key: %+v", got)
			}
		})
		t.Run(name+"/account breakdown", func(t *testing.T) {
			detail, err := db.GetAccountUsageStats(ctx, accountID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(detail.ByAPIKey) != 4 {
				t.Fatalf("account usage should contain four keys: %+v", detail.ByAPIKey)
			}
			if got := detail.ByAPIKey[0]; got.APIKeyID != keyID || got.APIKeyName != name || got.Requests != 2 || got.Tokens != 300 {
				t.Fatalf("account usage for renamed key: %+v", got)
			}
		})
		t.Run(name+"/usage overview", func(t *testing.T) {
			stats, err := db.GetUsageStats(ctx, start, end, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(stats.APIKeyStats) != 4 {
				t.Fatalf("usage overview should contain four keys: %+v", stats.APIKeyStats)
			}
			if got := stats.APIKeyStats[0]; got.APIKeyID != keyID || got.Label != name || got.Requests != 2 || got.Tokens != 300 || got.ErrorCount != 1 {
				t.Fatalf("usage overview for renamed key: %+v", got)
			}
		})
	}

	assertStats("Current name")
	if err := db.DeleteAPIKey(ctx, keyID); err != nil {
		t.Fatal(err)
	}
	assertStats("Second name")
}

func TestAPIKeySelfUsageFeatureStats(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "self-features.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	keyID, err := db.InsertAPIKey(ctx, "self", "sk-self-features")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := db.InsertAPIKey(ctx, "other", "sk-other-features")
	if err != nil {
		t.Fatal(err)
	}
	hour := time.Now().Truncate(time.Hour)
	start, end := hour.Add(-4*time.Hour+10*time.Minute), hour.Add(-time.Hour+10*time.Minute)
	logs := []UsageLogInput{
		{APIKeyID: keyID, CreatedAt: start.Add(time.Minute), StatusCode: 200, Stream: true, ServiceTier: "priority", CachedTokens: 1, ReasoningEffort: "high", Endpoint: "/v1/responses"},
		{APIKeyID: keyID, CreatedAt: hour.Add(-3*time.Hour + 20*time.Minute), StatusCode: 500, Model: "gpt-image-2", AttemptIndex: 2},
		{APIKeyID: keyID, CreatedAt: end.Add(-time.Minute), StatusCode: 200, BillingServiceTier: "standard", ServiceTier: "fast", AttemptIndex: 1},
		{APIKeyID: keyID, CreatedAt: start.Add(-time.Minute), StatusCode: 500, Stream: true},
		{APIKeyID: keyID, CreatedAt: end, StatusCode: 500, Stream: true},
		{APIKeyID: otherID, CreatedAt: start.Add(time.Minute), StatusCode: 500, Stream: true},
		{APIKeyID: keyID, CreatedAt: start.Add(time.Minute), StatusCode: 499, Stream: true},
		{APIKeyID: keyID, CreatedAt: hour.Add(-3*time.Hour + 30*time.Minute), StatusCode: 500, Stream: true, InternalReason: "internal_test"},
	}
	for i := range logs {
		if err := db.InsertUsageLog(ctx, &logs[i]); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	if err := db.RebuildUsageHourlyRollup(ctx); err != nil {
		t.Fatal(err)
	}
	want := UsageFeatureStat{StreamRequests: 1, SyncRequests: 2, FastRequests: 1, CacheHitRequests: 1, ReasoningRequests: 1, ImageRequests: 1, RetryRequests: 1, ErrorRequests: 1}
	for _, ready := range []bool{false, true} {
		setUsageHourlyReady(t, db, ready)
		if ready && !db.planUsageHourlyQuery(ctx, start, end, time.Now()).useRollup {
			t.Fatal("test range should use hourly rollup")
		}
		for _, filter := range []APIKeySelfLogFilter{{}, {Endpoint: "/v1/responses"}, {Status: "error", Stream: "sync"}} {
			report, err := db.GetAPIKeySelfUsageReportFiltered(ctx, keyID, start, end, 1, 10, filter)
			if err != nil {
				t.Fatal(err)
			}
			if report.FeatureStats != want {
				t.Fatalf("rollup=%v filter=%+v: features=%+v, want %+v", ready, filter, report.FeatureStats, want)
			}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]json.RawMessage
			if err := json.Unmarshal(data, &result); err != nil || len(result["feature_stats"]) == 0 {
				t.Fatalf("top-level feature_stats missing: %s (%v)", data, err)
			}
		}
		admin, err := db.GetUsageStatsFiltered(ctx, start, end, "", UsageLogFilter{APIKeyID: &keyID}, true)
		if err != nil {
			t.Fatal(err)
		}
		if admin.FeatureStats != want {
			t.Fatalf("rollup=%v: admin features=%+v, want %+v", ready, admin.FeatureStats, want)
		}
	}
}
