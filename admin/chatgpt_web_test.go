package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
)

func TestChatGPTWebAdminRoutesRequireAuthAndRejectUnknownFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	defer tc.Close()
	h := NewHandler(auth.NewStore(db, tc, nil), db, tc, nil, "admin-secret")
	r := gin.New()
	h.RegisterRoutes(r)
	for i, route := range []struct{ method, path string }{
		{"GET", "/api/admin/chatgpt-web"}, {"GET", "/api/admin/chatgpt-web/accounts"},
		{"POST", "/api/admin/chatgpt-web/accounts/batch"},
		{"PUT", "/api/admin/chatgpt-web/accounts/1"}, {"POST", "/api/admin/chatgpt-web/accounts/1/check"},
		{"GET", "/api/admin/chatgpt-web/executions"}, {"PUT", "/api/admin/chatgpt-web/config"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(route.method, route.path, nil)
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+10)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: got %d", route.method, route.path, w.Code)
		}
	}
	for _, body := range []string{`{"enabled":true,"status":"ready"}`, `{"enabled":null}`, `{"enabled":true} {}`, `{"status":"ready"}`} {
		req := httptest.NewRequest("PUT", "/api/admin/chatgpt-web/accounts/1", strings.NewReader(body))
		req.Header.Set("X-Admin-Key", "admin-secret")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: got %d", body, w.Code)
		}
	}
	states, err := db.ListChatGPTWebAccountStates(t.Context())
	if err != nil || len(states) != 0 {
		t.Fatalf("invalid requests wrote states: %v %v", states, err)
	}
}
