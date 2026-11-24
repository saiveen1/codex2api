package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestIntelligenceCheckOnceAPI(t *testing.T) {
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: store}
	router := gin.New()
	h.registerIntelligenceCheckRoutes(router.Group("/api/admin"))
	request := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return w
	}
	path := fmt.Sprintf("/api/admin/accounts/%d/intelligence-check/once", id)
	for _, body := range []string{`{"model":"a,b"}`, `{"model":123}`, `{`} {
		if w := request(path, body); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s: %d %s", body, w.Code, w.Body)
		}
	}
	w := request(path, `{}`)
	var result struct {
		Run database.IntelligenceCheckRun `json:"run"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusAccepted || result.Run.AccountID != id || result.Run.Model != "gpt-6.2-sol" || !result.Run.RecordOnly || !result.Run.Once {
		t.Fatalf("default: %d %s %v", w.Code, w.Body, err)
	}
	if w = request(path, `{"model":"gpt-5.4"}`); w.Code != http.StatusConflict {
		t.Fatalf("busy: %d %s", w.Code, w.Body)
	}
	other := insertTestAccount(t, db)
	if err := store.LoadAccountByID(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	w = request(fmt.Sprintf("/api/admin/accounts/%d/intelligence-check/once", other), `{"model":"gpt-5.4"}`)
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusAccepted || result.Run.Model != "gpt-5.4" || result.Run.AccountID != other {
		t.Fatalf("override: %d %s %v", w.Code, w.Body, err)
	}
}
