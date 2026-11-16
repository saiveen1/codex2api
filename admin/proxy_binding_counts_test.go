package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestListProxiesTracksBindingChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	urls := []string{"http://first.example:8080", "http://second.example:8080"}
	for _, url := range urls {
		if _, err := db.InsertProxy(ctx, url, "synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	accountID, err := db.InsertAccount(ctx, "synthetic", "binding-test", "")
	if err != nil {
		t.Fatal(err)
	}
	store := newAdminProxyTestStore(t, db)
	if err := store.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, store: store}
	router := gin.New()
	router.GET("/proxies", handler.ListProxies)
	router.PATCH("/accounts/:id/scheduler", handler.UpdateAccountScheduler)
	for _, step := range []struct {
		name string
		url  string
		want [2]int64
	}{
		{"bind", urls[0], [2]int64{1, 0}},
		{"rebind", urls[1], [2]int64{0, 1}},
		{"unbind", "", [2]int64{0, 0}},
	} {
		t.Run(step.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/accounts/%d/scheduler", accountID), strings.NewReader(fmt.Sprintf(`{"proxy_url":%q}`, step.url)))
			request.Header.Set("Content-Type", "application/json")
			saved := httptest.NewRecorder()
			router.ServeHTTP(saved, request)
			if saved.Code != http.StatusOK {
				t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
			}
			listed := httptest.NewRecorder()
			router.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/proxies", nil))
			var response struct {
				Proxies []*database.ProxyRow `json:"proxies"`
			}
			if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &response) != nil || len(response.Proxies) != 2 {
				t.Fatalf("list: %d %s", listed.Code, listed.Body.String())
			}
			for i, proxy := range response.Proxies {
				if proxy.BoundCount != step.want[i] {
					t.Fatalf("proxy %d count = %d, want %d", i, proxy.BoundCount, step.want[i])
				}
			}
		})
	}
}

func TestListProxiesDoesNotReportUnknownOccupancyAsFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "occupancy.db")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.InsertProxy(context.Background(), "http://proxy.example:8080", "synthetic"); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`ALTER TABLE accounts RENAME TO unavailable_accounts`); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.ListProxies(context.Background()); err != nil || len(rows) != 1 {
		t.Fatalf("fixture must allow proxy inventory before failing count: %v", err)
	}
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/proxies", handler.ListProxies)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/proxies", nil))
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), `"bound_count":0`) {
		t.Fatalf("unknown count reported as free: %d %s", recorder.Code, recorder.Body.String())
	}
}
