package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func compactionBoolPtr(value bool) *bool {
	return &value
}

func usageLogModels(logs []*UsageLog) []string {
	models := make([]string, 0, len(logs))
	for _, logRow := range logs {
		models = append(models, logRow.Model)
	}
	sort.Strings(models)
	return models
}

func assertUsageLogModels(t *testing.T, logs []*UsageLog, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := usageLogModels(logs)
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}

func TestUsageLogCompactionStatesRoundTripAndFilter(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "compaction states", "sk-compaction-states-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}

	inputs := []*UsageLogInput{
		{
			APIKeyID: apiKeyID, Endpoint: "/v1/responses", Model: "trigger-only",
			StatusCode: 200, Compact: true,
		},
		{
			APIKeyID: apiKeyID, Endpoint: "/v1/responses", Model: "history-only",
			StatusCode: 200, HasCompactionHistory: true,
		},
		{
			APIKeyID: apiKeyID, Endpoint: "/v1/responses", Model: "both",
			StatusCode: 200, Compact: true, HasCompactionHistory: true,
		},
		{
			APIKeyID: apiKeyID, Endpoint: "/v1/responses", Model: "neither",
			StatusCode: 200,
		},
	}
	for _, input := range inputs {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog(%s): %v", input.Model, err)
		}
	}
	db.FlushUsageLogs()

	recent, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs: %v", err)
	}
	if len(recent) != len(inputs) {
		t.Fatalf("len(recent) = %d, want %d", len(recent), len(inputs))
	}
	states := make(map[string][2]bool, len(recent))
	for _, logRow := range recent {
		states[logRow.Model] = [2]bool{logRow.Compact, logRow.HasCompactionHistory}
	}
	for model, want := range map[string][2]bool{
		"trigger-only": {true, false},
		"history-only": {false, true},
		"both":         {true, true},
		"neither":      {false, false},
	} {
		if got := states[model]; got != want {
			t.Fatalf("%s states = %v, want %v", model, got, want)
		}
	}

	now := time.Now()
	baseFilter := UsageLogFilter{
		Start: now.Add(-time.Hour), End: now.Add(time.Hour), Page: 1, PageSize: 10,
	}

	compactFilter := baseFilter
	compactFilter.CompactOnly = compactionBoolPtr(true)
	compactPage, err := db.ListUsageLogsByTimeRangePaged(ctx, compactFilter)
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged compact: %v", err)
	}
	assertUsageLogModels(t, compactPage.Logs, "both", "trigger-only")

	historyFilter := baseFilter
	historyFilter.CompactionHistoryOnly = compactionBoolPtr(true)
	historyPage, err := db.ListUsageLogsByTimeRangePaged(ctx, historyFilter)
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged history: %v", err)
	}
	assertUsageLogModels(t, historyPage.Logs, "both", "history-only")

	bothFilter := baseFilter
	bothFilter.CompactOnly = compactionBoolPtr(true)
	bothFilter.CompactionHistoryOnly = compactionBoolPtr(true)
	bothLogs, err := db.ListUsageLogsByFilter(ctx, bothFilter)
	if err != nil {
		t.Fatalf("ListUsageLogsByFilter both: %v", err)
	}
	assertUsageLogModels(t, bothLogs, "both")

	plainHistoryFilter := baseFilter
	plainHistoryFilter.CompactOnly = compactionBoolPtr(false)
	plainHistoryFilter.CompactionHistoryOnly = compactionBoolPtr(true)
	plainHistoryLogs, err := db.ListUsageLogsByFilter(ctx, plainHistoryFilter)
	if err != nil {
		t.Fatalf("ListUsageLogsByFilter history without trigger: %v", err)
	}
	assertUsageLogModels(t, plainHistoryLogs, "history-only")

	report, err := db.GetAPIKeySelfUsageReport(ctx, apiKeyID, now.Add(-time.Hour), now.Add(time.Hour), 1, 10)
	if err != nil {
		t.Fatalf("GetAPIKeySelfUsageReport: %v", err)
	}
	if len(report.RecentLogs) != len(inputs) {
		t.Fatalf("len(report.RecentLogs) = %d, want %d", len(report.RecentLogs), len(inputs))
	}
	selfStates := make(map[string][2]bool, len(report.RecentLogs))
	for _, logRow := range report.RecentLogs {
		selfStates[logRow.Model] = [2]bool{logRow.Compact, logRow.HasCompactionHistory}
	}
	for model, want := range states {
		if got := selfStates[model]; got != want {
			t.Fatalf("self usage %s states = %v, want %v", model, got, want)
		}
	}
}

func TestUsageLogInsertColumnCountIncludesCompactionHistory(t *testing.T) {
	// 50 legacy + 2 cache-write + 4 trace + 3 image-token + 3 user image-billing + 1 ultra +
	// 2 manual inject/observe turn-state + 2 auto template audit turn-state fields +
	// 2 upstream response model audit fields (upstream_response_model / upstream_model_mismatch)
	// + 1 Daybreak program field + 1 video seconds field + explicit created_at (issue #857)
	// + 1 bounded diagnostic context field + 4 upstream round timing fields.
	const want = 77
	if usageLogInsertColumnCount != want {
		t.Fatalf("usageLogInsertColumnCount = %d, want %d", usageLogInsertColumnCount, want)
	}
	if maxUsageLogInsertRowsPerSQL*usageLogInsertColumnCount > postgresMaxBindParams {
		t.Fatalf("single INSERT bind params = %d, exceed PostgreSQL limit %d",
			maxUsageLogInsertRowsPerSQL*usageLogInsertColumnCount, postgresMaxBindParams)
	}
}

func TestSQLiteUpstreamRoundTimingRoundTripAndLegacyMigration(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "round-timing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	keyID, err := db.InsertAPIKey(ctx, "round timing", "sk-round-timing-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs (api_key_id, endpoint, model, status_code, duration_ms, first_token_ms) VALUES ($1, '/v1/responses', 'legacy', 200, 999, 111)`, keyID); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"upstream_round_duration_ms", "upstream_first_content_ms", "upstream_round_index", "hidden_continuation_round"} {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE usage_logs DROP COLUMN `+column); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := db.migrateSQLite(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var legacyIsNull bool
	if err := db.conn.QueryRowContext(ctx, `SELECT upstream_round_duration_ms IS NULL AND upstream_first_content_ms IS NULL AND upstream_round_index IS NULL AND hidden_continuation_round IS NULL FROM usage_logs WHERE model = 'legacy'`).Scan(&legacyIsNull); err != nil || !legacyIsNull {
		t.Fatalf("legacy values were backfilled: null=%v err=%v", legacyIsNull, err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{APIKeyID: keyID, Endpoint: "/v1/responses", Model: "measured", StatusCode: 200, DurationMs: 999, FirstTokenMs: 111, UpstreamRoundDurationMs: 800, UpstreamFirstContentMs: 120, UpstreamRoundIndex: 2, HiddenContinuationRound: true}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	now := time.Now()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	filter := UsageLogFilter{Start: start, End: end, Page: 1, PageSize: 10}
	check := func(name string, rows []*UsageLog, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rows) != 2 {
			t.Fatalf("%s: got %d rows", name, len(rows))
		}
		for _, row := range rows {
			if row.DurationMs != 999 || row.FirstTokenMs != 111 {
				t.Fatalf("%s: existing timings changed: %+v", name, row)
			}
			if row.Model == "legacy" && (row.UpstreamRoundDurationMs != 0 || row.UpstreamFirstContentMs != 0 || row.UpstreamRoundIndex != 0 || row.HiddenContinuationRound) {
				t.Fatalf("%s: legacy = %+v", name, row)
			}
			if row.Model == "measured" && (row.UpstreamRoundDurationMs != 800 || row.UpstreamFirstContentMs != 120 || row.UpstreamRoundIndex != 2 || !row.HiddenContinuationRound) {
				t.Fatalf("%s: measured = %+v", name, row)
			}
		}
	}
	recent, err := db.ListRecentUsageLogs(ctx, 10)
	check("recent", recent, err)
	ranged, err := db.ListUsageLogsByTimeRange(ctx, start, end)
	check("range", ranged, err)
	filtered, err := db.ListUsageLogsByFilter(ctx, filter)
	check("filter", filtered, err)
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	check("paged", page.Logs, nil)
	report, err := db.GetAPIKeySelfUsageReport(ctx, keyID, start, end, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RecentLogs) != 2 {
		t.Fatalf("portal rows = %d", len(report.RecentLogs))
	}
	for _, row := range report.RecentLogs {
		if row.Model == "legacy" && (row.UpstreamRoundDurationMs != 0 || row.UpstreamFirstContentMs != 0 || row.UpstreamRoundIndex != 0 || row.HiddenContinuationRound) {
			t.Fatalf("portal legacy = %+v", row)
		}
		if row.Model == "measured" && (row.UpstreamRoundDurationMs != 800 || row.UpstreamFirstContentMs != 120 || row.UpstreamRoundIndex != 2 || !row.HiddenContinuationRound) {
			t.Fatalf("portal measured = %+v", row)
		}
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"upstream_round_duration_ms", "upstream_first_content_ms", "upstream_round_index", "hidden_continuation_round"} {
			if _, ok := fields[field]; !ok {
				t.Fatalf("portal JSON missing %s", field)
			}
		}
	}
}
