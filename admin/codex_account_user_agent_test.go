package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestSchedulerCodexUserAgentRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "synthetic", "synthetic-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	store := newAdminProxyTestStore(t, db)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	h := NewHandler(store, db, tokenCache, nil, "")
	router := gin.New()
	router.PATCH("/accounts/:id/scheduler", h.UpdateAccountScheduler)
	patch := func(body string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/accounts/%d/scheduler", id), strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("patch %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	patch(`{"codex_user_agent":"codex-vscode","timezone":"UTC"}`, http.StatusOK)
	patch(`{"force_fast":true}`, http.StatusOK)
	for _, body := range []string{`{"codex_user_agent":"raw: "}`, `{"codex_user_agent":"raw:agent\r\nX: y"}`, `{"codex_user_agent":"raw:agent "}`, `{"codex_user_agent":"CODEX-TUI"}`, `{"codex_user_agent":3}`} {
		patch(body, http.StatusBadRequest)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential(auth.CodexUserAgentCredentialKey) != "codex-vscode" || row.GetCredential("timezone") != "UTC" || row.GetCredential("refresh_token") != "synthetic-refresh" || !row.GetCredentialBool(auth.ForceFastCredentialKey) {
		t.Fatalf("credentials not preserved: %+v", row.ID)
	}
	account := store.FindByID(id)
	if account == nil {
		t.Fatal("account not loaded")
	}
	account.Mu().RLock()
	value := account.CodexUserAgent
	account.Mu().RUnlock()
	if value != "codex-vscode" {
		t.Fatalf("runtime value = %q", value)
	}
	w := invokeListAccounts(t, h, "/api/admin/accounts?view=page&channel=codex&page=1&page_size=20")
	if w.Code != http.StatusOK || gjson.Get(w.Body.String(), "accounts.0.codex_user_agent").String() != "codex-vscode" {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	patch(`{"codex_user_agent":"raw:example/1.2 (test)"}`, http.StatusOK)
	patch(`{"codex_user_agent":""}`, http.StatusOK)
	row, err = db.GetAccountByID(ctx, id)
	if err != nil || row.GetCredential(auth.CodexUserAgentCredentialKey) != "" || row.GetCredential("timezone") != "UTC" {
		t.Fatalf("clear: row=%v err=%v", row, err)
	}
}
