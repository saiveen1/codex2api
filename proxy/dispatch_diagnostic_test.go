package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestDispatchDiagnosticTeamPool(t *testing.T) {
	installDiagnosticTestSink(t)
	t.Setenv("LOG_DISABLED", "true")
	oldWait := dispatchAccountWaitTimeout
	dispatchAccountWaitTimeout = 20 * time.Millisecond
	t.Cleanup(func() { dispatchAccountWaitTimeout = oldWait })
	for _, engine := range []string{"legacy", "indexed"} {
		t.Run(engine, func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "dispatch.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			t.Cleanup(store.Stop)
			store.SetSchedulerEngine(engine)
			for id := int64(1); id <= 28; id++ {
				acc := &auth.Account{DBID: id, AccessToken: "PRIVATE_SENTINEL", PlanType: "team", GroupIDs: []int64{2}}
				store.AddAccount(acc)
				store.MarkCooldown(acc, time.Hour, "unauthorized")
			}
			healthy := &auth.Account{DBID: 29, AccessToken: "PRIVATE_SENTINEL", PlanType: "team", GroupIDs: []int64{2}}
			healthy.SetAllowedAPIKeyIDs([]int64{8})
			store.AddAccount(healthy)
			store.AddAccount(&auth.Account{DBID: 30, AccessToken: "fixture", PlanType: "team", GroupIDs: []int64{5}})
			row := &database.APIKeyRow{ID: 3, Name: "team", AllowedGroupIDs: []int64{2}}
			store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
			r := gin.New()
			r.Use(RequestDiagnosticMiddleware(store, db), func(c *gin.Context) {
				c.Set(contextAPIKeyRow, row)
				c.Set(contextAPIKeyID, row.ID)
				c.Set(contextAPIKeyName, row.Name)
			})
			r.POST("/responses", NewHandler(store, db, nil, nil).Responses)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"gpt-5.5","input":"PRIVATE_SENTINEL","stream":false}`))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "account_key_denied") || strings.Contains(w.Body.String(), "PRIVATE_SENTINEL") {
				t.Fatalf("public failure: %d %s", w.Code, w.Body.String())
			}
			assertNoAvailableAccountResponse(t, w.Body.Bytes())
			db.FlushUsageLogs()
			rows, err := db.ListRecentUsageLogs(t.Context(), 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("usage rows=%d err=%v", len(rows), err)
			}
			log := rows[0]
			var event diagnosticEvent
			if err := json.Unmarshal([]byte(log.DiagnosticContext), &event); err != nil {
				t.Fatal(err)
			}
			d := event.Decision
			if w.Header().Get("X-Codex2API-Request-ID") == "" || event.RequestID != w.Header().Get("X-Codex2API-Request-ID") ||
				log.APIKeyID != row.ID || log.TotalTokens != 0 || log.UserBilled != 0 || log.Model != "gpt-5.5" || log.AccountID != 0 ||
				d == nil || d.Reason != "no_available_account" || d.Attempted || d.Scoped != 29 || d.HasCandidate ||
				d.Counts["unauthorized"] != 28 || d.Counts["account_key_denied"] != 1 || len(d.Samples) != 1 || d.Samples[0].AccountID != 29 ||
				strings.Contains(log.DiagnosticContext, "PRIVATE_SENTINEL") {
				t.Fatalf("unexpected diagnostics: %s", log.DiagnosticContext)
			}
		})
	}
}

func TestDispatchDiagnosticCommittedStreamKeepsUpstreamEvidence(t *testing.T) {
	installDiagnosticTestSink(t)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "dispatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	r := gin.New()
	r.Use(RequestDiagnosticMiddleware(store, db))
	r.POST("/v1/responses", func(c *gin.Context) {
		c.Set(contextAPIKeyID, int64(3))
		ctx := c.Request.Context()
		beginUpstreamTrace(ctx, &auth.Account{DBID: 29}, "", false)(&http.Response{StatusCode: 429})
		noteDiagnosticErrorBody(ctx, []byte(`{"error":{"code":"rate_limit_exceeded","message":"PRIVATE_SENTINEL"}}`))
		input := &database.UsageLogInput{AccountID: 29, APIKeyID: 3, StatusCode: 429, Endpoint: "/responses"}
		populateUpstreamTrace(c, input)
		logUsageDiagnostic(c, input)
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Error(err)
		}
		c.Writer.WriteHeaderNow()
		recordDispatchDecision(c, "upstream_pool_exhausted", 429, "gpt-5.5", "gpt-5.5", true, auth.DispatchDiagnostic{})
		fmt.Fprint(c.Writer, "data: {\"type\":\"response.failed\"}\n\n")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	db.FlushUsageLogs()
	rows, err := db.ListRecentUsageLogs(t.Context(), 10)
	if err != nil || len(rows) != 2 || w.Code != http.StatusOK {
		t.Fatalf("usage rows=%d err=%v code=%d", len(rows), err, w.Code)
	}
	var event diagnosticEvent
	if err := json.Unmarshal([]byte(rows[0].DiagnosticContext), &event); err != nil {
		t.Fatal(err)
	}
	if event.Decision == nil || !event.Decision.Attempted || event.HTTPStatus != 200 || event.OutcomeStatus != 429 ||
		event.Attempt.HTTPStatus != 429 || event.Error.Code != "rate_limit_exceeded" || rows[0].InternalReason != "dispatch_terminal_diagnostic" || rows[0].RequestID != rows[1].RequestID {
		t.Fatalf("unexpected terminal event: %s", rows[0].DiagnosticContext)
	}
	usage, err := db.GetAPIKeyUsageSince(t.Context(), 3, time.Now().Add(-time.Minute))
	if err != nil || usage.Requests != 1 {
		t.Fatalf("terminal row changed quota usage: %+v %v", usage, err)
	}
}
