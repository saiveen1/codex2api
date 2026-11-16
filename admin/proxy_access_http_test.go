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
