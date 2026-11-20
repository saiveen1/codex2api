package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestIntelligenceRecordOnlyAgentIdentityDoesNotRegisterMissingTask(t *testing.T) {
	old := EnsureCodexAgentIdentityTaskFunc
	t.Cleanup(func() { EnsureCodexAgentIdentityTaskFunc = old })
	EnsureCodexAgentIdentityTaskFunc = func(context.Context, *auth.Account, bool) error {
		t.Fatal("record-only request must not register a task")
		return nil
	}
	account := &auth.Account{CodexAuthMode: auth.CodexAuthModeAgentIdentity, AgentRuntimeID: "fixture", AgentPrivateKey: "fixture"}
	resp, err := ExecuteRequest(WithRecordOnlyDetector(WithCodexDetectorRequest(context.Background())), account, []byte(`{"model":"gpt-6-astra","input":"hello"}`), "", "", "", nil, nil)
	if resp != nil || err == nil || !strings.Contains(err.Error(), "task_id") {
		t.Fatalf("response=%v error=%v", resp, err)
	}
}
