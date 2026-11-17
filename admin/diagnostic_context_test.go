package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

func TestOpsErrorExportRetainsDiagnosticContext(t *testing.T) {
	context := `{"schema":2,"stage":"upstream_http","error":{"detail":"generic"},"attempt":{"request":{"fingerprint":"sample"}}}`
	entry := newOpsErrorExportEntry(&database.UsageLog{ID: 7, StatusCode: 400, DiagnosticContext: context})
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if entry.DiagnosticContext != context || !strings.Contains(string(data), `"diagnostic_context"`) {
		t.Fatal("export dropped diagnostic evidence")
	}
}

func TestOpsErrorExportRetainsUpstreamRoundTiming(t *testing.T) {
	entry := newOpsErrorExportEntry(&database.UsageLog{UpstreamRoundDurationMs: 800, UpstreamFirstContentMs: 120, UpstreamRoundIndex: 2, HiddenContinuationRound: true})
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"upstream_round_duration_ms": float64(800), "upstream_first_content_ms": float64(120), "upstream_round_index": float64(2), "hidden_continuation_round": true} {
		if got[key] != want {
			t.Fatalf("%s = %v, want %v", key, got[key], want)
		}
	}
}
