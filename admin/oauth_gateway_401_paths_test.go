package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestOAuthRecoveryAdminProbePaths(t *testing.T) {
	const revoked = `{"error":{"code":"token_revoked"}}`
	const failed = `{"type":"response.failed","response":{"error":{"code":"token_revoked"}}}`
	for _, tc := range []struct {
		name                string
		path                string
		status              int
		body                string
		optedIn, wantSignal bool
	}{
		{"connection", "connection", 401, revoked, true, true},
		{"batch", "batch", 401, revoked, true, true},
		{"batch-wham", "batch-wham", 401, `{"detail":{"code":"token_revoked"}}`, true, true},
		{"wham", "wham", 401, revoked, true, true},
		{"responses", "responses", 401, revoked, true, true},
		{"connection-stream", "connection", 200, failed, true, true},
		{"batch-stream", "batch", 200, failed, true, true},
		{"responses-stream", "responses", 200, failed, true, true},
		{"ambiguous-401", "connection", 401, `{"error":{"code":"missing_scope"}}`, true, false},
		{"no-opt-in", "connection", 401, revoked, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wham := strings.HasSuffix(r.URL.Path, "/wham/usage")
				if wham && tc.path == "batch" {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{}`)
					return
				}
				if wham != (tc.path == "wham" || tc.path == "batch-wham") {
					t.Errorf("unexpected upstream path %s", r.URL.Path)
				}
				hits.Add(1)
				if tc.status == http.StatusOK {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: "+tc.body+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			previous := proxy.GetResinConfig()
			proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "fixture"})
			t.Cleanup(func() { proxy.SetResinConfig(previous) })
			store := auth.NewStore(nil, nil, nil)
			account := &auth.Account{DBID: 42, AccessToken: "synthetic-at", RefreshToken: "synthetic-rt", CredentialGeneration: 7, Status: auth.StatusReady}
			store.AddAccount(account)
			var signals int
			store.SetOAuthRecoveryHooks(&auth.OAuthRecoveryHooks{
				Enabled: func(id int64) bool { return id == 42 && tc.optedIn },
				Signal: func(id, generation int64, refreshFirst bool) {
					signals++
					if id != 42 || generation != 7 || !refreshFirst || account.GetCooldownReason() != "unauthorized" {
						t.Error("recovery must refresh the same credential after cooldown is written")
					}
				},
			})
			h := &Handler{store: store}
			ctx := context.Background()
			switch tc.path {
			case "connection":
				serveCodexDiagnosticsTest(h)
			case "batch", "batch-wham":
				h.runSingleBatchTest(ctx, account)
			case "wham":
				_ = h.probeUsageViaWham(ctx, account, false)
			case "responses":
				_ = h.probeUsageViaResponses(ctx, account)
			}
			if hits.Load() != 1 || (signals == 1) != tc.wantSignal || signals > 1 {
				t.Fatalf("hits=%d signals=%d want one signal=%v", hits.Load(), signals, tc.wantSignal)
			}
		})
	}
}
