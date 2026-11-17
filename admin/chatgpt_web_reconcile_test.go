package admin

import (
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestChatGPTWebReconcileAdminExact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CHATGPT_WEB_ENABLED", "false")
	path := filepath.Join(t.TempDir(), "test.sqlite")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, id := range []string{"first", "second"} {
		_, err = raw.Exec(`INSERT INTO chatgpt_web_executions (id,api_key_id,account_id,request_hash,credential_revision,environment_revision,proxy_fingerprint,operation,stage,lease_owner,lease_until,created_at,updated_at) VALUES (?,42,7,'hash','revision',2,'proxy','generate','submitting','owner',0,1,1)`, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	tc := cache.NewMemory(4)
	defer tc.Close()
	h := NewHandler(auth.NewStore(db, tc, nil), db, tc, nil, "admin-secret")
	r := gin.New()
	h.RegisterRoutes(r)
	request := func(path string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if authorized {
			req.Header.Set("X-Admin-Key", "admin-secret")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	base := "/api/admin/chatgpt-web/executions"
	if w := request(base+"?execution_id=first", false); w.Code != 401 {
		t.Fatalf("auth %d", w.Code)
	}
	if w := request(base+"?execution_id=first&account_id=8", true); w.Code != 404 {
		t.Fatalf("account %d", w.Code)
	}
	if w := request(base+"?execution_id=missing", true); w.Code != 404 {
		t.Fatalf("missing %d", w.Code)
	}
	if w := request(base+"?execution_id=first&account_id=7", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"first"`) || strings.Contains(w.Body.String(), "second") || !strings.Contains(w.Body.String(), `"verified":false`) || strings.Contains(w.Body.String(), "revision") {
		t.Fatalf("exact %d %s", w.Code, w.Body.String())
	}
	if w := request(base, true); w.Code != 200 || !strings.Contains(w.Body.String(), "first") || !strings.Contains(w.Body.String(), "second") {
		t.Fatalf("list %d %s", w.Code, w.Body.String())
	}
}
