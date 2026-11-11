package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeUsageDiagnosticContext(t *testing.T) {
	if MaxUsageDiagnosticContextBytes != 24*1024 {
		t.Fatalf("context byte limit = %d, want 24576", MaxUsageDiagnosticContextBytes)
	}
	for _, tc := range []struct {
		name, raw, status string
	}{
		{"empty", "", ""},
		{"lossless object", " \n{\"schema\":2,\"number\":9007199254740993,\"number\":1e-9,\"escaped\":\"\\u0061\",\"unicode\":\"合成\"}\t", ""},
		{"opaque scalar", "9007199254740993", ""},
		{"opaque array", `[null,true,{"future_field":1}]`, ""},
		{"exact byte limit", `"` + strings.Repeat("x", MaxUsageDiagnosticContextBytes-2) + `"`, ""},
		{"invalid", `{"synthetic_secret":"do-not-store",`, "invalid"},
		{"whitespace", " \n\t", "invalid"},
		{"trailing data", `{} {}`, "invalid"},
		{"invalid utf8", "\"\xff\"", "invalid"},
		{"size limit", `"` + strings.Repeat("x", MaxUsageDiagnosticContextBytes-1) + `"`, "size_limit"},
		{"byte not rune limit", `"` + strings.Repeat("界", MaxUsageDiagnosticContextBytes/3) + `"`, "size_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeUsageDiagnosticContext(tc.raw)
			if tc.status == "" {
				if got != tc.raw {
					t.Fatalf("valid payload changed: got %d bytes, want %d bytes", len(got), len(tc.raw))
				}
				return
			}
			var status map[string]any
			if len(got) > 128 || !json.Valid([]byte(got)) {
				t.Fatalf("rejected payload did not become small valid JSON: %q", got)
			}
			if err := json.Unmarshal([]byte(got), &status); err != nil {
				t.Fatal(err)
			}
			if len(status) != 2 || status["schema"] != float64(2) || status["capture_status"] != tc.status {
				t.Fatalf("unexpected rejection status (or raw data retained): %v", status)
			}
			if NormalizeUsageDiagnosticContext(got) != got {
				t.Fatal("normalization is not idempotent")
			}
		})
	}
}

func TestUsageDiagnosticContextSQLiteRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "diagnostics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	base := time.Now().Add(-48 * time.Hour)
	cases := []struct{ name, raw string }{
		{"lossless", " \n{\"schema\":2,\"event\":{\"synthetic\":true,\"number\":9007199254740993,\"escaped\":\"\\u0061\"}}\t"},
		{"boundary", `"` + strings.Repeat("x", MaxUsageDiagnosticContextBytes-2) + `"`},
		{"empty", ""},
		{"invalid", `{"synthetic_secret":"do-not-store",`},
		{"oversize", `"` + strings.Repeat("x", MaxUsageDiagnosticContextBytes) + `"`},
	}
	want := make(map[string]string, len(cases))
	for i, tc := range cases {
		want[tc.name] = NormalizeUsageDiagnosticContext(tc.raw)
		input := &UsageLogInput{
			Endpoint: "/v1/responses", Model: tc.name, StatusCode: 502,
			RequestID: "synthetic-" + tc.name, DiagnosticContext: tc.raw,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatal(err)
		}
		input.DiagnosticContext = `{"schema":2,"changed_after_enqueue":true}`
	}
	db.FlushUsageLogs()
	filter := UsageLogFilter{Start: base.Add(-time.Hour), End: base.Add(time.Hour), Page: 1, PageSize: 10}
	readers := map[string]func() ([]*UsageLog, error){
		"recent":     func() ([]*UsageLog, error) { return db.ListRecentUsageLogs(ctx, 10) },
		"time range": func() ([]*UsageLog, error) { return db.ListUsageLogsByTimeRange(ctx, filter.Start, filter.End) },
		"paged usage": func() ([]*UsageLog, error) {
			page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				return nil, err
			}
			return page.Logs, nil
		},
		"operations errors": func() ([]*UsageLog, error) {
			f := filter
			f.ErrorOnly = true
			page, err := db.ListUsageLogsByTimeRangePaged(ctx, f)
			if err != nil {
				return nil, err
			}
			return page.Logs, nil
		},
		"filtered export": func() ([]*UsageLog, error) { return db.ListUsageLogsByFilter(ctx, filter) },
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			logs, err := read()
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != len(want) {
				t.Fatalf("logs = %d, want %d", len(logs), len(want))
			}
			for _, row := range logs {
				expected, ok := want[row.Model]
				if !ok || row.DiagnosticContext != expected {
					t.Fatalf("%s context changed: got %d bytes, want %d", row.Model, len(row.DiagnosticContext), len(expected))
				}
				body, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				encoded, exists := fields["diagnostic_context"]
				if expected == "" {
					if exists {
						t.Fatal("empty diagnostic_context was not omitted")
					}
				} else {
					var decoded string
					if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != expected {
						t.Fatalf("diagnostic_context is not the original JSON string: %v", err)
					}
				}
			}
		})
	}
}

func TestUsageDiagnosticContextSQLiteLegacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-diagnostics.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	// Pre-diagnostic schema and data; an existing file bypasses the test schema template.
	if _, err := legacy.Exec(`CREATE TABLE usage_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER DEFAULT 0,
		endpoint TEXT DEFAULT '', model TEXT DEFAULT '', prompt_tokens INTEGER DEFAULT 0,
		completion_tokens INTEGER DEFAULT 0, total_tokens INTEGER DEFAULT 0,
		status_code INTEGER DEFAULT 0, duration_ms INTEGER DEFAULT 0,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	); INSERT INTO usage_logs (model, status_code) VALUES ('synthetic-legacy', 502)`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var notNull int
	var defaultValue, columnType string
	if err := db.conn.QueryRowContext(ctx, `SELECT "notnull", dflt_value, type FROM pragma_table_info('usage_logs') WHERE name = 'diagnostic_context'`).Scan(&notNull, &defaultValue, &columnType); err != nil {
		t.Fatal(err)
	}
	if notNull != 1 || defaultValue != "''" || columnType != "TEXT" {
		t.Fatalf("column definition = %s, notnull=%d, default=%s", columnType, notNull, defaultValue)
	}
	// Old writers can omit the new column, just like existing old rows.
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs (model, status_code) VALUES ('synthetic-old-writer', 502)`); err != nil {
		t.Fatal(err)
	}
	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("legacy logs = %d, want 2", len(logs))
	}
	for _, row := range logs {
		if row.DiagnosticContext != "" {
			t.Fatalf("legacy row %s acquired context: %q", row.Model, row.DiagnosticContext)
		}
	}
}

func TestUsageDiagnosticContextSurvivesBillingCopies(t *testing.T) {
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	SetModelPricingOverrides(map[string]ModelPricingOverride{"gpt-image-2": {UserBillingMode: UserBillingModePerImage, ImageUnitPrice: .05}})
	const captured = ` {"schema":2,"historical_policy":"synthetic-old-policy"} `
	input := &UsageLogInput{Model: "gpt-image-2", StatusCode: 200, ImageCount: 2, DiagnosticContext: captured}
	snapshot := SnapshotUsageLogBilling(input)
	delivered := WithDeliveredImageCount(snapshot, 1)
	if snapshot == input || delivered == snapshot {
		t.Fatal("expected independent billing copies")
	}
	input.DiagnosticContext = `{"schema":2,"policy":"changed-input"}`
	snapshot.DiagnosticContext = `{"schema":2,"policy":"changed-snapshot"}`
	if delivered.DiagnosticContext != captured {
		t.Fatal("billing copies lost the captured historical context")
	}
	db, err := New("sqlite", filepath.Join(t.TempDir(), "historical-diagnostics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, delivered); err != nil {
		t.Fatal(err)
	}
	delivered.DiagnosticContext = ""
	SetModelPricingOverrides(nil)
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].DiagnosticContext != captured {
		t.Fatal("historical context changed after billing copies, enqueue or policy change")
	}
}

func TestUsageDiagnosticContextBatchBindsSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "batch-diagnostics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	batch := []usageLogEntry{
		{StoreUsageLog: true, Model: "synthetic-first", StatusCode: 502, DiagnosticContext: `{"schema":2,"attempt":1}`, CreatedAt: time.Now()},
		{StoreUsageLog: true, Model: "synthetic-second", StatusCode: 502, DiagnosticContext: `{"schema":2,"attempt":2}`, CreatedAt: time.Now()},
	}
	// SQLite supports the PostgreSQL VALUES/RETURNING shape: exercise the actual
	// multi-row placeholder offsets and argument order without a PostgreSQL connection.
	ids, err := db.batchInsertLogsChunk(ctx, tx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(batch) {
		t.Fatalf("inserted ids = %d, want %d", len(ids), len(batch))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		var model, diagnostic string
		if err := db.conn.QueryRowContext(ctx, `SELECT model, diagnostic_context FROM usage_logs WHERE id = $1`, id).Scan(&model, &diagnostic); err != nil {
			t.Fatal(err)
		}
		if model != batch[i].Model || diagnostic != batch[i].DiagnosticContext {
			t.Fatalf("batch row %d metadata mismatch: %s, %s", i, model, diagnostic)
		}
	}
}
