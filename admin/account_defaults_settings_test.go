package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestAccountDefaultsSettingsSaveReadAndValidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, nil, nil, "")
	router := gin.New()
	router.GET("/settings/account-defaults", h.GetAccountDefaults)
	router.PUT("/settings/account-defaults", h.UpdateAccountDefaults)
	request := func(method, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/settings/account-defaults", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s status = %d, want %d: %s", method, rec.Code, want, rec.Body)
		}
		return rec
	}
	request(http.MethodPut, `{"concurrency_rules":[{"channel":"codex","plan":"self_serve_business_prolite","concurrency":50}],"default_timezone":"UTC","use_fixed_proxy_timezone":true}`, http.StatusOK)
	request(http.MethodPut, `{"concurrency_rules":[],"default_timezone":"Local","use_fixed_proxy_timezone":false}`, http.StatusBadRequest)
	request(http.MethodPut, `{"concurrency_rules":[{"channel":"codex","plan":"team5x","concurrency":50},{"channel":"codex","plan":"self_serve_business_prolite","concurrency":10}],"default_timezone":"","use_fixed_proxy_timezone":false}`, http.StatusBadRequest)
	rec := request(http.MethodGet, "", http.StatusOK)
	var cfg accountDefaultsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ConcurrencyRules) != 1 || cfg.ConcurrencyRules[0].Concurrency != 50 || cfg.DefaultTimezone != "UTC" || !cfg.UseFixedProxyTimezone {
		t.Fatalf("unexpected saved defaults: %+v", cfg)
	}
	request(http.MethodPut, `{"concurrency_rules":[],"default_timezone":"","use_fixed_proxy_timezone":false}`, http.StatusOK)
}
