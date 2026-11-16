package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setWebCheckFixture(t *testing.T, f *webFixture, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f.handler.chatGPTWebRuntime = func() (chatGPTWebConfig, error) {
		return chatGPTWebConfig{enabled: true, endpoint: server.URL, secret: "synthetic-worker-secret", timeout: 10 * time.Second}, nil
	}
}

func TestChatGPTWebLegacyIdentityCheckAndGeneration(t *testing.T) {
	f := newWebFixture(t, nil)
	ctx := context.Background()
	if err := f.db.UpdateCredentials(ctx, f.account.ID(), map[string]any{"account_id": "user-legacy"}); err != nil {
		t.Fatal(err)
	}
	f.account.AccountID = "user-legacy"
	var generated bool
	setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) {
		var request chatGPTWebRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path == "/v1/accounts/check" {
			if request.ChatGPTAccountID != "" {
				t.Error("legacy user ID sent as requested workspace")
			}
			fmt.Fprint(w, `{"status":"ready","capabilities":["generate"],"image_quota_remaining":3,"workspace_id":"resolved-workspace","stage":"complete","upstream_status":200}`)
			return
		}
		generated = true
		if request.ChatGPTAccountID != "resolved-workspace" {
			t.Error("generation did not use verified workspace")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		writeWebFixtureSuccess(w)
	})
	state, err := f.handler.CheckChatGPTWebAccount(ctx, f.account.ID())
	if err != nil {
		t.Fatal(err)
	}
	if state.WorkspaceID != "resolved-workspace" || state.WorkspaceBindingFingerprint != chatGPTWebHash("") || state.LastCheckAt == nil {
		t.Fatalf("missing binding: %+v", state)
	}
	response := f.request(t, "/v1/images/generations", `{"prompt":"synthetic square"}`, "legacy-discovery")
	if response.Code != 200 || !generated {
		t.Fatalf("legacy generation: %d %s", response.Code, response.Body.String())
	}
	f.handler.store.ApplyAccountCustomHeaders(f.account.ID(), map[string]string{"ChatGPT-Account-ID": "team-workspace"})
	generated = false
	response = f.request(t, "/v1/images/generations", `{"prompt":"synthetic square"}`, "switched-route")
	if response.Code != 503 || generated {
		t.Fatal("changed route reused default workspace observation")
	}
}

func TestChatGPTWebDetailedCheckFailureKeepsQuota(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, stage string
		status                  int
	}{
		{"authentication", `{"status":"auth_required","error_code":"auth_required","stage":"identity","upstream_status":401,"message":"synthetic-secret-token"}`, "auth_required", "identity", 200},
		{"worker_http", `{"code":"upstream_error","stage":"quota","upstream_status":503,"message":"synthetic-secret-token"}`, "upstream_error", "quota", 503},
		{"missing_workspace", `{"status":"ready","capabilities":["generate"],"image_quota_remaining":3}`, "workspace_unavailable", "workspace", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWebFixture(t, nil)
			before, err := f.db.GetChatGPTWebAccountState(context.Background(), f.account.ID())
			if err != nil {
				t.Fatal(err)
			}
			setWebCheckFixture(t, f, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) })
			state, err := f.handler.CheckChatGPTWebAccount(context.Background(), f.account.ID())
			var detail *ChatGPTWebCheckError
			if !errors.As(err, &detail) || detail.Code != tc.code || detail.Stage != tc.stage || !detail.Recorded {
				t.Fatalf("diagnostic: %+v %v", detail, err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") || detail.HTTPStatus == 401 {
				t.Fatal("upstream secret/auth leaked into admin session response")
			}
			if state.Status == "ready" || state.LastCheckAt == nil || state.LastCheckErrorCode != tc.code || state.ImageQuotaRemaining == nil || *state.ImageQuotaRemaining != *before.ImageQuotaRemaining || !state.ObservedAt.Equal(*before.ObservedAt) {
				t.Fatalf("snapshot lost: %+v", state)
			}
			if chatGPTWebStateReady(*state, "generate", time.Now()) {
				t.Fatal("failed check remained schedulable")
			}
		})
	}
}

func TestChatGPTWebBusyCheckDoesNotInvalidateActiveObservation(t *testing.T) {
	f := newWebFixture(t, nil)
	ctx := context.Background()
	before, _ := f.db.GetChatGPTWebAccountState(ctx, f.account.ID())
	if ok, err := f.db.AcquireChatGPTWebAccountLease(ctx, f.account.ID(), "existing-operation", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatal("fixture lease failed")
	}
	_, err := f.handler.CheckChatGPTWebAccount(ctx, f.account.ID())
	var detail *ChatGPTWebCheckError
	if !errors.As(err, &detail) || detail.Code != "account_busy" {
		t.Fatalf("busy error: %v", err)
	}
	after, _ := f.db.GetChatGPTWebAccountState(ctx, f.account.ID())
	if after.Status != before.Status || !after.ObservedAt.Equal(*before.ObservedAt) {
		t.Fatal("busy attempt changed active observation")
	}
}
