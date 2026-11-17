package admin

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestChatGPTWebSchedulingAdminPersistence(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	r := gin.New()
	r.POST("/keys", h.CreateAPIKey)
	r.PATCH("/keys/:id", h.UpdateAPIKey)
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: status %d, want %d", method, path, w.Code, want)
		}
		return w
	}
	request("POST", "/keys", `{"name":"bad","limits":{"upstream_channel":"chatgpt_web","web_scheduling_mode":"other"}}`, 400)
	request("POST", "/keys", `{"name":"bad-type","limits":{"upstream_channel":"chatgpt_web","web_scheduling_mode":42}}`, 400)
	w := request("POST", "/keys", `{"name":"web","limits":{"upstream_channel":"chatgpt_web","web_scheduling_mode":"round_robin"}}`, 200)
	var created struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/keys/%d", created.ID)
	checkMode := func(want string) {
		t.Helper()
		row, err := db.GetAPIKeyByID(t.Context(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Limits.WebSchedulingMode != want {
			t.Fatalf("saved mode %q, want %q", row.Limits.WebSchedulingMode, want)
		}
	}
	checkMode("round_robin")
	request("PATCH", path, `{"limits":{"upstream_channel":"codex","web_scheduling_mode":"round_robin"}}`, 400)
	request("PATCH", path, `{"limits":{"upstream_channel":"chatgpt_web","web_scheduling_mode":"other"}}`, 400)
	checkMode("round_robin")
	request("PATCH", path, `{"limits":{"upstream_channel":"chatgpt_web","web_scheduling_mode":"fill_first"}}`, 200)
	checkMode("fill_first")
	request("PATCH", path, `{"limits":{"upstream_channel":"chatgpt_web"}}`, 200)
	checkMode("")
}
