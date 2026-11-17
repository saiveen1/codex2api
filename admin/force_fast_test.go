package admin

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestForceFastAdminPersistence(t *testing.T) {
	ctx := t.Context()
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	newStore := func() *auth.Store {
		t.Helper()
		store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 8})
		t.Cleanup(store.Stop)
		if err := store.Init(ctx); err != nil {
			t.Fatal(err)
		}
		return store
	}
	store := newStore()
	account := store.FindByID(accountID)
	if account == nil {
		t.Fatal("account not loaded")
	}
	h := &Handler{db: db, store: store}
	r := gin.New()
	r.GET("/accounts/:id", h.GetAccount)
	r.PATCH("/accounts/:id/scheduler", h.UpdateAccountScheduler)
	r.GET("/account-groups", h.ListAccountGroups)
	r.POST("/account-groups", h.CreateAccountGroup)
	r.PATCH("/account-groups/:id", h.UpdateAccountGroup)
	r.DELETE("/account-groups/:id", h.DeleteAccountGroup)
	r.GET("/keys", h.ListAPIKeys)
	r.POST("/keys", h.CreateAPIKey)
	r.PATCH("/keys/:id", h.UpdateAPIKey)
	request := func(method, path, body string, want int, result any) {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: status = %d, want %d: %s", method, path, w.Code, want, w.Body)
		}
		if result != nil {
			if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
				t.Fatal(err)
			}
		}
	}
	accountPath := fmt.Sprintf("/accounts/%d", accountID)
	checkAccount := func(want bool) {
		t.Helper()
		var got struct {
			ForceFast *bool `json:"force_fast"`
		}
		request("GET", accountPath, "", 200, &got)
		row, err := db.GetAccountByID(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ForceFast == nil || *got.ForceFast != want || row.GetCredentialBool(auth.ForceFastCredentialKey) != want || row.GetCredential("refresh_token") != "rt_test" || account.IsForceFast() != want {
			t.Fatalf("account response, persistence or runtime does not match force_fast=%t", want)
		}
	}
	checkAccount(false)
	request("PATCH", accountPath+"/scheduler", `{"force_fast":true}`, 200, nil)
	request("PATCH", accountPath+"/scheduler", `{"force_fast":"false"}`, 400, nil)
	request("PATCH", accountPath+"/scheduler", `{"skip_warm_tier":false}`, 200, nil)
	checkAccount(true)

	var fastGroup, defaultGroup struct{ ID int64 }
	request("POST", "/account-groups", `{"name":"fast","force_fast":true}`, 200, &fastGroup)
	request("POST", "/account-groups", `{"name":"default"}`, 200, &defaultGroup)
	groupPath := fmt.Sprintf("/account-groups/%d", fastGroup.ID)
	allGroups := []int64{defaultGroup.ID, fastGroup.ID}
	checkGroup := func(want bool) {
		t.Helper()
		var got struct {
			Groups []struct {
				ID        int64 `json:"id"`
				ForceFast *bool `json:"force_fast"`
			}
		}
		request("GET", "/account-groups", "", 200, &got)
		flags := make(map[int64]*bool)
		for _, group := range got.Groups {
			flags[group.ID] = group.ForceFast
		}
		fast, fallback := flags[fastGroup.ID], flags[defaultGroup.ID]
		if fast == nil || *fast != want || fallback == nil || *fallback || store.GroupsForceFast(allGroups) != want || store.GroupsForceFast([]int64{defaultGroup.ID}) || store.GroupsForceFast(nil) {
			t.Fatalf("group response, persistence or runtime does not match force_fast=%t", want)
		}
	}
	request("PATCH", groupPath, `{"name":"fast renamed"}`, 200, nil)
	checkGroup(true)
	reloaded := newStore()
	if !reloaded.FindByID(accountID).IsForceFast() || !reloaded.GroupsForceFast(allGroups) {
		t.Fatal("persisted account/group flags were not loaded into a fresh store")
	}
	request("PATCH", accountPath+"/scheduler", `{"force_fast":false}`, 200, nil)
	checkAccount(false)
	request("PATCH", groupPath, `{"force_fast":false}`, 200, nil)
	checkGroup(false)
	request("PATCH", groupPath, `{"force_fast":true}`, 200, nil)
	request("DELETE", groupPath, "", 200, nil)
	if store.GroupsForceFast(allGroups) {
		t.Fatal("removed group still enables Force Fast")
	}

	var key createAPIKeyResponse
	request("POST", "/keys", `{"name":"key","limits":{"allow_live":false,"force_fast":true}}`, 200, &key)
	keyPath := fmt.Sprintf("/keys/%d", key.ID)
	checkKey := func(want bool) {
		t.Helper()
		row, err := db.GetAPIKeyByValue(ctx, key.Key)
		if err != nil || row == nil || row.Limits.ForceFast != want {
			t.Fatalf("auth key lookup does not match force_fast=%t: %v", want, err)
		}
		var got apiKeysResponse
		request("GET", "/keys?view=lite", "", 200, &got)
		if len(got.Keys) != 1 || got.Keys[0].Limits.ForceFast != want {
			t.Fatalf("key list does not match force_fast=%t", want)
		}
	}
	checkKey(true)
	request("PATCH", keyPath, `{"name":"key renamed"}`, 200, nil)
	checkKey(true)
	before, err := db.GetAPIKeyAuthRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request("PATCH", keyPath, `{"limits":{"force_fast":false}}`, 200, nil)
	checkKey(false)
	if after, err := db.GetAPIKeyAuthRevision(ctx); err != nil || after == before {
		t.Fatalf("force_fast edit did not advance auth cache revision: %v", err)
	}
}
