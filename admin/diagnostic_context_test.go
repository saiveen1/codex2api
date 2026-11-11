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
