package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/managedweb"
)

func TestChatGPTWebReconcileWorkerReadOnly(t *testing.T) {
	e := &database.ChatGPTWebExecution{ID: "exec-1", AccountID: 7, CredentialRevision: "revision", EnvironmentRevision: 2}
	mismatch := false
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1/executions/exec-1" || r.Header.Get("Authorization") != "Bearer worker-secret-123" || r.ContentLength > 0 {
			t.Error("unexpected request")
		}
		account := 7
		if mismatch {
			account = 8
		}
		fmt.Fprintf(w, `{"execution_id":"exec-1","account_id":%d,"credential_revision":"revision","environment_revision":2,"stage":"completed","asset_ids":["https://secret"],"result_available":true,"result_unknown":false}`, account)
	}))
	cfg := &managedweb.Config{Enabled: true, WorkerURL: server.URL, WorkerSecret: "worker-secret-123"}
	got := ObserveChatGPTWebExecution(context.Background(), e, cfg)
	if !got.Verified || got.Stage != "completed" || got.ResultAvailable == nil || !*got.ResultAvailable || got.AssetCount == nil || *got.AssetCount != 1 {
		t.Fatalf("observation: %+v", got)
	}
	mismatch = true
	got = ObserveChatGPTWebExecution(context.Background(), e, cfg)
	if got.Verified || got.Reason != "binding_mismatch" {
		t.Fatalf("mismatch: %+v", got)
	}
	server.Close()
	got = ObserveChatGPTWebExecution(context.Background(), e, cfg)
	if got.Verified || got.Reason != "unavailable" || calls != 2 {
		t.Fatalf("unavailable: %+v calls=%d", got, calls)
	}
	if ChatGPTWebValidExecutionID("../exec") || ChatGPTWebValidExecutionID(strings.Repeat("a", 161)) {
		t.Fatal("unsafe execution ID")
	}
}
