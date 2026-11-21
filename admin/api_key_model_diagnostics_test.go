package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestAPIKeyModelDiagnosticsAdminOnlyAndUnmetered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(1)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(db, tc, nil)
	t.Cleanup(store.Stop)
	p := proxy.NewHandler(store, db, nil, nil)
	p.SetRuntimeCache(tc)
	h := NewHandler(store, db, tc, nil, "test-admin-secret")
	h.SetAPIKeyAuthCacheHandler(p)
	r := gin.New()
	r.Use(proxy.RequestDiagnosticMiddleware(store, db))
	p.RegisterRoutes(r)
	h.RegisterRoutes(r)

	const key = "sk-private-model-catalog-key"
	id, err := db.InsertAPIKeyWithOptions(t.Context(), database.APIKeyInput{Name: "test", Key: key, QuotaLimit: 10, QuotaUsed: 3,
		Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelCodex}})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, header, value string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(header, value)
		r.ServeHTTP(w, req)
		return w
	}
	path := fmt.Sprintf("/api/admin/keys/%d/model-diagnostics", id)
	if w := get(path, "Authorization", "Bearer "+key); w.Code < 400 {
		t.Fatalf("downstream key accessed admin diagnostic: %d", w.Code)
	}
	if w := get("/api/admin/keys/0/model-diagnostics", "X-Admin-Key", "test-admin-secret"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid ID returned %d", w.Code)
	}
	if w := get(fmt.Sprintf("/api/admin/keys/%d/model-diagnostics", id+100), "X-Admin-Key", "test-admin-secret"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown ID returned %d", w.Code)
	}
	w := get(path, "X-Admin-Key", "test-admin-secret")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"last_empty_status":"uncollected"`) {
		t.Fatalf("checking admin created false history: %d %s", w.Code, w.Body)
	}
	manifest := get("/backend-api/codex/models", "Authorization", "Bearer "+key)
	if manifest.Code != http.StatusServiceUnavailable || manifest.Header().Get("X-Codex2API-Model-Status") != "no_visible_models" || strings.Contains(manifest.Body.String(), "no_scoped_accounts") {
		t.Fatalf("incompatible or leaking manifest failure: %d %s", manifest.Code, manifest.Body)
	}
	w = get(path, "X-Admin-Key", "test-admin-secret")
	var result struct {
		Current   proxy.ModelCatalogDiagnostic  `json:"current"`
		LastEmpty *proxy.ModelCatalogDiagnostic `json:"last_empty"`
		Status    string                        `json:"last_empty_status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), key) || result.Current.Channel != database.UpstreamChannelCodex || result.Current.Reason != "no_scoped_accounts" {
		t.Fatalf("invalid current diagnostic: %d %s", w.Code, w.Body)
	}
	if result.Status != "recorded" || result.LastEmpty == nil || result.LastEmpty.RequestID == "" || result.LastEmpty.RequestID != manifest.Header().Get("X-Codex2API-Request-ID") {
		t.Fatalf("history did not correlate to discovery: %d %s", w.Code, w.Body)
	}
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(t.Context(), 10)
	if err != nil || len(logs) != 0 {
		t.Fatalf("model discovery affected inference usage: logs=%d err=%v", len(logs), err)
	}
	row, err := db.GetAPIKeyByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.QuotaUsed != 3 || row.TotalUsed != 0 {
		t.Fatalf("model discovery affected quota: quota_used=%v total_used=%v", row.QuotaUsed, row.TotalUsed)
	}
}
