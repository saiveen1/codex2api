package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestModelCatalogDiagnosticsStagesAndFreshScope(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	for id := int64(1); id <= 28; id++ {
		a := &auth.Account{DBID: id, AccessToken: "test-token", PlanType: "team", GroupIDs: []int64{10}}
		store.AddAccount(a)
		store.MarkCooldownWithError(a, time.Hour, "unauthorized", "test upstream error")
	}
	healthy := &auth.Account{DBID: 29, AccessToken: "test-token", PlanType: "team", GroupIDs: []int64{10}}
	healthy.SetAllowedAPIKeyIDs([]int64{99})
	store.AddAccount(healthy)
	outside := &auth.Account{DBID: 30, AccessToken: "test-token", PlanType: "team", GroupIDs: []int64{20}}
	store.AddAccount(outside)
	h := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 8751, AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{PlanAllow: []string{"team"}}}
	store.SetAPIKeyAllowedGroups(row.ID, []int64{20})
	d := h.InspectModelCatalog(t.Context(), row)
	if d.Reason != "no_visible_accounts" || d.Scoped == nil || *d.Scoped != 29 || d.Visible == nil || *d.Visible != 0 || d.Counts["unauthorized"] != 28 || d.Counts["account_key_denied"] != 1 || d.Counts["group_mismatch"] != 1 {
		t.Fatalf("unexpected Team diagnosis: %+v", d)
	}
	if !slices.Contains(d.Samples, modelCatalogSample{29, "account_key_denied"}) || len(d.Samples) != 2 {
		t.Fatalf("expected distinct in-scope samples: %+v", d.Samples)
	}
	if !store.APIKeyAllowsAccount(row.ID, outside) {
		t.Fatal("inspection altered runtime scheduler scope")
	}

	healthy.SetAllowedAPIKeyIDs([]int64{row.ID})
	if d := h.InspectModelCatalog(t.Context(), row); d.Reason != "ok" || !slices.Contains(d.Models, "gpt-5.5") {
		t.Fatalf("an official account without stored models must still publish built-ins: %+v", d)
	}
	row.Limits.ModelAllow = []string{"not-in-catalog"}
	if d := h.InspectModelCatalog(t.Context(), row); d.Reason != "models_filtered" {
		t.Fatalf("policy exclusion: %+v", d)
	}
	row.AllowedGroupIDs = []int64{999}
	if d := h.InspectModelCatalog(t.Context(), row); d.Reason != "no_scoped_accounts" || len(d.Samples) != 0 {
		t.Fatalf("unmatched scope: %+v", d)
	}
	store.AddAccount(&auth.Account{DBID: 31, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: "https://relay.example", APIKey: "test-token", GroupIDs: []int64{40}})
	row.AllowedGroupIDs, row.Limits.PlanAllow = []int64{40}, nil
	if d := h.InspectModelCatalog(t.Context(), row); d.Reason != "no_account_models" || d.Visible == nil || *d.Visible != 1 {
		t.Fatalf("visible relay without advertised models: %+v", d)
	}
}

func TestModelCatalogSnapshotThrottleAndExpiry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := cache.NewMemory(1)
	t.Cleanup(func() { _ = backend.Close() })
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	h := NewHandler(store, nil, nil, nil)
	h.SetRuntimeCache(backend)
	row := &database.APIKeyRow{ID: 8752, AllowedGroupIDs: []int64{100}}
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		c.Set(contextAPIKeyRow, row)
		h.ListModels(c)
		return w
	}
	first := request()
	if first.Code != http.StatusOK || first.Header().Get("X-Codex2API-Model-Status") != "no_visible_models" || !strings.Contains(first.Body.String(), `"data":[]`) || strings.Contains(first.Body.String(), "no_scoped_accounts") {
		t.Fatalf("incompatible or leaking list response: %d %v %s", first.Code, first.Header(), first.Body)
	}
	last, status := h.LastEmptyModelCatalog(t.Context(), row.ID)
	if status != "recorded" || last == nil || last.RequestID == "" || last.RequestID != first.Header().Get("X-Codex2API-Request-ID") || last.Reason != "no_scoped_accounts" {
		t.Fatalf("missing compatible snapshot: %+v %s", last, status)
	}
	second := request()
	after, _ := h.LastEmptyModelCatalog(t.Context(), row.ID)
	if after == nil || after.RequestID != last.RequestID || second.Header().Get("X-Codex2API-Request-ID") == last.RequestID {
		t.Fatalf("repeat did not preserve the first sample: %+v", after)
	}
	last.Time = time.Now().Add(-modelCatalogEmptyTTL).UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(last)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.SetRuntime(t.Context(), modelCatalogEmptyNamespace, fmt.Sprint(row.ID), raw, modelCatalogEmptyTTL); err != nil {
		t.Fatal(err)
	}
	if last, status := h.LastEmptyModelCatalog(t.Context(), row.ID); last != nil || status != "uncollected" {
		t.Fatalf("expired snapshot must be explicit: %+v %s", last, status)
	}
}

func TestModelCatalogManifestReportsOnlyTerminalEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const officialModel = "manifest-only-model"
	const upstreamETag = `"catalog-etag"`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", upstreamETag)
		_, _ = fmt.Fprint(w, `{"models":[{"slug":"`+officialModel+`"}]}`)
	}))
	t.Cleanup(upstream.Close)
	oldURL := codexModelsManifestURLForTest
	codexModelsManifestURLForTest = upstream.URL
	t.Cleanup(func() { codexModelsManifestURLForTest = oldURL })
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token"})
	backend := cache.NewMemory(1)
	t.Cleanup(func() { _ = backend.Close() })
	h := NewHandler(store, nil, nil, nil)
	h.SetRuntimeCache(backend)
	row := &database.APIKeyRow{ID: 8753, Limits: database.APIKeyLimits{ModelAllow: []string{officialModel}}}
	if local := h.InspectModelCatalog(context.Background(), row); local.Reason != "models_filtered" {
		t.Fatalf("fixture must start with an empty local catalog: %+v", local)
	}
	request := func(etag string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/backend-api/codex/models", nil)
		c.Request.Header.Set("If-None-Match", etag)
		c.Set(contextAPIKeyRow, row)
		h.CodexModelsManifestHandler(c)
		return w
	}
	w := request("")
	if w.Code != http.StatusOK || w.Header().Get("X-Codex2API-Model-Status") != "" || !strings.Contains(w.Body.String(), officialModel) {
		t.Fatalf("intermediate empty catalog was reported as terminal: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if last, status := h.LastEmptyModelCatalog(t.Context(), row.ID); last != nil || status != "uncollected" {
		t.Fatalf("nonempty official output stored an empty snapshot: %+v %s", last, status)
	}
	row.Limits.ModelAllow = []string{"denied-official-model"}
	w = request("")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"models":[]`) || w.Header().Get("X-Codex2API-Model-Status") != "no_visible_models" || w.Header().Get("ETag") == "" {
		t.Fatalf("empty manifest incompatible: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if last, _ := h.LastEmptyModelCatalog(t.Context(), row.ID); last == nil || last.Reason != "models_filtered" {
		t.Fatalf("terminal manifest did not record key model filter: %+v", last)
	}
	if again := request(w.Header().Get("ETag")); again.Code != http.StatusNotModified || again.Body.Len() != 0 || again.Header().Get("X-Codex2API-Model-Status") != "no_visible_models" {
		t.Fatalf("empty manifest validator changed: %d %v %s", again.Code, again.Header(), again.Body)
	}
}
