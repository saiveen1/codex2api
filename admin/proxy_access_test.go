package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestProxyAccessEndpointValidationConflictAndReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const url = "http://exclusive.example:8080"
	id, err := db.InsertProxy(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "a", "rt-a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.InsertAccount(ctx, "b", "rt-b", url)
	if err != nil {
		t.Fatal(err)
	}
	store := newAdminProxyTestStore(t, db)
	h := &Handler{db: db, store: store}
	router := gin.New()
	router.PATCH("/proxies/:id/access", h.UpdateProxyAccess)
	patch := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(r, req)
		return r
	}
	path := fmt.Sprintf("/proxies/%d/access", id)
	for _, body := range []string{`{}`, `{"exclusive":true}`, `{"exclusive":true,"allowed_account_ids":null}`, `{"exclusive":true,"allowed_account_ids":[]}`, `{"exclusive":true,"allowed_account_ids":[0]}`, `{"exclusive":true,"allowed_account_ids":[99999999]}`} {
		if r := patch(path, body); r.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, r.Code, r.Body.String())
		}
	}
	body := fmt.Sprintf(`{"exclusive":true,"allowed_account_ids":[%d]}`, a)
	if r := patch(path, body); r.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d response=%s", r.Code, r.Body.String())
	}
	if err := db.SetAccountProxyURLs(ctx, map[int64]string{b: ""}); err != nil {
		t.Fatal(err)
	}
	store.ApplyAccountProxyURL(b, "")
	r := patch(path, body)
	var row database.ProxyRow
	if err := json.Unmarshal(r.Body.Bytes(), &row); err != nil || r.Code != http.StatusOK || !row.Exclusive || len(row.AllowedAccountIDs) != 1 {
		t.Fatalf("success=%d %s err=%v", r.Code, r.Body.String(), err)
	}
	if store.NextProxy() != "" || store.ProxyAllowedForAccount(b, url) || !store.ProxyAllowedForAccount(a, url) {
		t.Fatal("successful endpoint did not update runtime")
	}
	if proxy, usable := store.ResolveUsableProxyForAccount(&auth.Account{DBID: b, ProxyURL: url}); proxy != "" || usable {
		t.Fatalf("denied explicit pin fell through: %q %v", proxy, usable)
	}
	if r := patch("/proxies/999999/access", body); r.Code != http.StatusNotFound {
		t.Fatalf("missing proxy status=%d", r.Code)
	}
	if r := patch(path, `{"exclusive":false,"allowed_account_ids":[]}`); r.Code != http.StatusOK || store.NextProxy() != url {
		t.Fatalf("shared reset status=%d", r.Code)
	}
	h.reloadProxyPoolFn = func() error { return errors.New("synthetic reload failure") }
	if r := patch(path, body); r.Code != http.StatusInternalServerError || !strings.Contains(r.Body.String(), `"saved":true`) {
		t.Fatalf("reload failure status=%d body=%s", r.Code, r.Body.String())
	}
	if store.ProxyAllowedForAccount(b, url) || store.NextProxy() != "" {
		t.Fatal("reload failure reopened node")
	}
}

func TestProxyAccessAutoBalanceExcludesExclusiveAndKeepsManualPins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const private = "http://manual.example:8080"
	const shared = "http://shared.example:8080"
	privateID, err := db.InsertProxy(ctx, private, "")
	if err != nil {
		t.Fatal(err)
	}
	sharedID, err := db.InsertProxy(ctx, shared, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "a", "rt-a", private)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.InsertAccount(ctx, "b", "rt-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateProxyAccess(ctx, privateID, true, []int64{a}); err != nil {
		t.Fatal(err)
	}
	store := newAdminProxyTestStore(t, db)
	h := &Handler{db: db, store: store}
	for _, ids := range [][]int64{nil, {privateID, sharedID}} {
		rows, err := h.candidateProxiesForBalance(ctx, ids)
		if err != nil || len(rows) != 1 || rows[0].ID != sharedID {
			t.Fatalf("candidates=%v err=%v", rows, err)
		}
	}
	router := gin.New()
	router.POST("/balance", h.AutoBalanceProxies)
	r := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/balance", strings.NewReader(`{"mode":"all"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(r, req)
	if r.Code != http.StatusOK {
		t.Fatalf("balance status=%d body=%s", r.Code, r.Body.String())
	}
	rows, err := db.ListActiveByChannel(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]string{}
	for _, row := range rows {
		seen[row.ID] = strings.TrimSpace(row.ProxyURL)
	}
	if seen[a] != private || seen[b] != shared {
		t.Fatalf("balance changed manual pin or assigned exclusive: %v", seen)
	}
}

func TestProxyAccessIntelligenceCheckRejectsDeniedPinBeforeOutbound(t *testing.T) {
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const url = "http://intelligence.example:8080"
	id, err := db.InsertProxy(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.InsertAccount(ctx, "allowed", "rt-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateProxyAccess(ctx, id, true, []int64{a}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: newAdminProxyTestStore(t, db)}
	result := h.executeIntelligenceCheck(ctx, &auth.Account{DBID: a + 1, ProxyURL: url}, "model", "", "prompt")
	if !result.Interrupted || result.Error != "proxy access denied or unavailable" || result.CredentialRejected {
		t.Fatalf("denied policy was not a configuration interruption: %+v", result)
	}
}
