package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProxyAccessBindingValidationHTTPStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	owner, err := db.InsertAccount(ctx, "owner", "synthetic-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.InsertAccount(ctx, "other", "synthetic-other", "")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://exclusive.example:8080"
	proxyID, err := db.InsertProxy(ctx, proxyURL, "exclusive")
	if err != nil {
		t.Fatal(err)
	}
	store := newAdminProxyTestStore(t, db)
	if _, err := store.UpdateProxyAccess(ctx, proxyID, true, []int64{owner}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, store: store}
	router := gin.New()
	router.PATCH("/accounts/:id/scheduler", handler.UpdateAccountScheduler)
	router.POST("/accounts/batch-update", handler.BatchUpdateAccounts)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPatch, fmt.Sprintf("/accounts/%d/scheduler", other), fmt.Sprintf(`{"proxy_url":%q}`, proxyURL)},
		{http.MethodPost, "/accounts/batch-update", fmt.Sprintf(`{"ids":[%d],"proxy_url":%q}`, other, proxyURL)},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "指定账号") {
			t.Fatalf("method=%s status=%d body=%s", tc.method, recorder.Code, recorder.Body.String())
		}
	}
	row, err := db.GetAccountByID(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if row.ProxyURL != "" {
		t.Fatal("denied assignment changed the account proxy")
	}
}

func TestManualProxyBindingClaimsAndPromotesHTTP(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		t.Run(fmt.Sprintf("reserved=%t", reserved), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db := newAdminProxyTestDB(t)
			ctx := context.Background()
			owner, err := db.InsertAccount(ctx, "owner", "claim-owner", "")
			if err != nil {
				t.Fatal(err)
			}
			other, err := db.InsertAccount(ctx, "other", "claim-other", "")
			if err != nil {
				t.Fatal(err)
			}
			const url = "http://claim.example:8080"
			proxyID, err := db.InsertProxy(ctx, url, "claim")
			if err != nil {
				t.Fatal(err)
			}
			store := newAdminProxyTestStore(t, db)
			if reserved {
				if _, err := store.UpdateProxyAccess(ctx, proxyID, true, nil); err != nil {
					t.Fatal(err)
				}
			}
			h := &Handler{db: db, store: store}
			router := gin.New()
			router.PATCH("/accounts/:id/scheduler", h.UpdateAccountScheduler)
			bind := func(id int64) *httptest.ResponseRecorder {
				r := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/accounts/%d/scheduler", id), strings.NewReader(fmt.Sprintf(`{"proxy_url":%q}`, url)))
				req.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(r, req)
				return r
			}
			if r := bind(owner); r.Code != http.StatusOK {
				t.Fatalf("claim: %d %s", r.Code, r.Body.String())
			}
			proxy, err := db.GetProxy(ctx, proxyID)
			if err != nil || !proxy.Exclusive || len(proxy.AllowedAccountIDs) != 1 || proxy.AllowedAccountIDs[0] != owner {
				t.Fatalf("proxy=%+v err=%v", proxy, err)
			}
			if !store.ProxyAllowedForAccount(owner, url) || store.ProxyAllowedForAccount(other, url) || store.NextProxy() == url {
				t.Fatal("runtime policy not published after claim")
			}
			if r := bind(other); r.Code != http.StatusBadRequest {
				t.Fatalf("conflict: %d %s", r.Code, r.Body.String())
			}
			row, err := db.GetAccountByID(ctx, other)
			if err != nil || row.ProxyURL != "" {
				t.Fatalf("other=%+v err=%v", row, err)
			}
		})
	}
}
