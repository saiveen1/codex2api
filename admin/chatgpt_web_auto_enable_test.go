package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestChatGPTWebAutoEnableAdminList(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := t.Context()
	group, err := db.CreateAccountGroup(ctx, "auto-list", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	id := batchWebFixtureAccount(t, db, "free", nil)
	if err := db.SetAccountGroups(ctx, id, []int64{group}); err != nil {
		t.Fatal(err)
	}
	_, err = db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "web", Key: "auto-list-raw-secret", AllowedGroupIDs: []int64{group}, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelChatGPTWeb}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	router := gin.New()
	router.GET("/states", h.ListChatGPTWebAccounts)
	r := httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/states", nil))
	if r.Code != 200 {
		t.Fatalf("list: %d %s", r.Code, r.Body.String())
	}
	var body struct {
		Accounts []database.ChatGPTWebAccountState `json:"accounts"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].AccountID != id || !body.Accounts[0].Enabled || body.Accounts[0].Status != "unverified" {
		t.Fatalf("missing auto-enabled state: %+v", body)
	}
	for _, secret := range []string{"auto-list-raw-secret", "batch-fixture-access-secret", "batch-fixture-refresh-secret"} {
		if strings.Contains(r.Body.String(), secret) {
			t.Fatal("state listing exposed credential")
		}
	}
}
