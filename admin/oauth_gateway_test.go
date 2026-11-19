package admin

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/oauthgateway/service"
	"github.com/codex2api/internal/oauthgateway/upstream"
	"github.com/gin-gonic/gin"
)

type oauthGatewayMemoryStore struct {
	mu    sync.Mutex
	state service.State
}

func (s *oauthGatewayMemoryStore) View() (service.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(s.state)
	var out service.State
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}
func (s *oauthGatewayMemoryStore) Update(fn func(*service.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	var next service.State
	if err = json.Unmarshal(raw, &next); err != nil {
		return err
	}
	if err = fn(&next); err != nil {
		return err
	}
	s.state = next
	return nil
}
func (s *oauthGatewayMemoryStore) Close() error { return nil }

func newOAuthGatewayAdminFixture(t *testing.T) (*Handler, *gin.Engine, *oauthGatewayMemoryStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(db, tc, nil)
	h := NewHandler(store, db, tc, nil, "synthetic-admin")
	state := &oauthGatewayMemoryStore{state: service.State{Version: 1, Jobs: []service.Job{}}}
	app, err := service.New(state, oauthGatewayTarget{h}, false)
	if err != nil {
		t.Fatal(err)
	}
	h.oauthGateway = &oauthGatewayRuntime{app: app, slots: make(chan struct{}, 8)}
	r := gin.New()
	h.RegisterRoutes(r)
	return h, r, state
}
func oauthGatewayRequest(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/api/admin/oauth-gateway/"+path, strings.NewReader(body))
	request.Header.Set("X-Admin-Key", "synthetic-admin")
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, request)
	return w
}
func TestOAuthGatewayAdminBoundary(t *testing.T) {
	h, r, _ := newOAuthGatewayAdminFixture(t)
	for i, route := range []struct{ method, path string }{{"GET", "settings"}, {"PUT", "settings"}, {"POST", "jobs"}, {"GET", "jobs"}, {"POST", "proxy-check"}, {"POST", "jobs/x/cancel"}, {"POST", "jobs/x/archive"}} {
		req := httptest.NewRequest(route.method, "/api/admin/oauth-gateway/"+route.path, strings.NewReader("{}"))
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+10)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatalf("unprotected route %s: %d", route.path, w.Code)
		}
	}
	for _, body := range []string{"null", "[]", `{} {}`, `{"oauth_url":"https://gateway.example.invalid","oauth_token":"secret","codex_key":"must-reject"}`, "{\"oauth_token\":\"\xff\"}"} {
		w := oauthGatewayRequest(r, "PUT", "settings", body)
		if w.Code != 400 {
			t.Fatalf("invalid JSON accepted: status=%d", w.Code)
		}
	}
	w := oauthGatewayRequest(r, "PUT", "settings", `{"oauth_token":"`+strings.Repeat("x", 1<<20)+`"}`)
	if w.Code != 413 {
		t.Fatalf("oversize accepted: %d", w.Code)
	}
	req := httptest.NewRequest("GET", "/api/admin/oauth-gateway/settings", nil)
	req.Header.Set("X-Admin-Key", "synthetic-admin")
	req.Header.Set("Origin", "https://attacker.example.invalid")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("cross-site accepted: %d", w.Code)
	}
	w = oauthGatewayRequest(r, "GET", "settings", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), `"oauth_token":`) {
		t.Fatalf("unsafe settings response: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"max_jobs":200`) || !strings.Contains(w.Body.String(), `"max_archived_jobs":10000`) {
		t.Fatal("settings omitted job capacity")
	}
	h.oauthGateway = nil
	h.oauthGatewayInitErr = database.ErrOAuthGatewayEncryption
	w = oauthGatewayRequest(r, "GET", "settings", "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "encryption_required") {
		t.Fatalf("missing key not explained: %d", w.Code)
	}
}

func TestOAuthGatewayNativeImportEndToEnd(t *testing.T) {
	h, r, state := newOAuthGatewayAdminFixture(t)
	ctx := t.Context()
	group, err := h.db.CreateAccountGroup(ctx, "gateway test", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeProxy := "http://runtime.example.invalid:8080"
	proxyID, err := h.db.InsertProxy(ctx, runtimeProxy, "gateway runtime")
	if err != nil {
		t.Fatal(err)
	}
	var submissions atomic.Int32
	var requestID string
	batch := func() upstream.Batch {
		return upstream.Batch{BatchID: "gb_00000000000000000000000000000001", RequestID: requestID, State: "completed", Total: 1, Succeeded: 1, Items: []upstream.BatchItem{{ItemID: "gi_00000000000000000000000000000001", Line: 1, IdentifierMasked: "f***@example.invalid", State: "succeeded"}}}
	}
	account := json.RawMessage(`{"platform":"openai","type":"oauth","name":"fixture","proxy_url":"http://must-not-copy.example.invalid:9090","credentials":{"email":"fixture@example.invalid","access_token":"synthetic-AT","refresh_token":"synthetic-RT","chatgpt_user_id":"user-fixture","chatgpt_account_id":"workspace-fixture","plan_type":"plus"}}`)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Header.Get("Authorization") != "Bearer synthetic-CDK" || req.Header.Get("X-Admin-Key") != "" {
			t.Error("gateway credentials mixed with admin auth")
			w.WriteHeader(401)
			return
		}
		send := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": v}) }
		switch req.URL.Path {
		case "/api/v1/oauth/capabilities":
			var cap upstream.Capabilities
			cap.Protocol.Name = "codex-account-gateway.oauth"
			cap.Protocol.Version = 1
			cap.Protocol.Revision = 3
			cap.OAuthMethods = []string{"password_totp"}
			cap.Features = map[string]bool{"result_item_mapping": true, "request_id_lookup": true, "proxy_check": true}
			cap.Countries = []string{"auto"}
			cap.Environments = []string{"auto"}
			cap.ProxyModes = []string{"system", "custom"}
			cap.WorkspacePolicies = []string{"team_preferred"}
			cap.MaxBatchSize = 100
			cap.MaxUploadBytes = 1 << 20
			send(cap)
		case "/api/v1/oauth/quota":
			send(upstream.Quota{Total: 10, Remaining: 10})
		case "/api/v1/oauth/preview":
			send(upstream.Preview{Valid: true, Count: 1, Rows: []upstream.PreviewRow{{Line: 1}}, Errors: []upstream.PreviewIssue{}})
		case "/api/v1/oauth/batches":
			submissions.Add(1)
			var input upstream.SubmitRequest
			if json.NewDecoder(req.Body).Decode(&input) != nil || len(input.Accounts) != 1 {
				t.Error("bad submit")
				w.WriteHeader(422)
				return
			}
			if input.Accounts[0].Password != "  synthetic-password  " {
				t.Error("password whitespace changed")
			}
			requestID = input.RequestID
			w.WriteHeader(202)
			send(batch())
		case "/api/v1/oauth/batches/gb_00000000000000000000000000000001":
			send(batch())
		case "/api/v1/oauth/batches/gb_00000000000000000000000000000001/result":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{"type": "sub2api-data", "version": 1, "proxies": []any{}, "accounts": []json.RawMessage{account}}, "delivery": upstream.Delivery{IncludedCount: 1, ExpiredItemIDs: []string{}, Items: []upstream.DeliveryItem{{ItemID: batch().Items[0].ItemID, Line: 1, AccountIndex: 0}}}})
		default:
			t.Errorf("unexpected gateway path %s", req.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer gateway.Close()
	settings, _ := json.Marshal(service.Settings{OAuthURL: gateway.URL, OAuthToken: "synthetic-CDK"})
	w := oauthGatewayRequest(r, "PUT", "settings", string(settings))
	if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-CDK") {
		t.Fatalf("save settings failed/leaked: %d", w.Code)
	}
	input := service.Input{RequestID: "native-test-1", SourceText: "fixture@example.invalid----  synthetic-password  ----JBSWY3DPEHPK3PXP", Options: upstream.Options{ProxyMode: "system"}, GroupIDs: []int64{group}}
	input.RuntimeProxy = runtimeProxy
	payload, _ := json.Marshal(input)
	w = oauthGatewayRequest(r, "POST", "preview", string(payload))
	if w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	w = oauthGatewayRequest(r, "POST", "jobs", string(payload))
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	w = oauthGatewayRequest(r, "POST", "jobs", string(payload))
	if w.Code != 200 {
		t.Fatalf("idempotent create: %d", w.Code)
	}
	for i := 0; i < 6; i++ {
		if err := state.Update(func(s *service.State) error {
			for j := range s.Jobs {
				for g := range s.Jobs[j].Groups {
					s.Jobs[j].Groups[g].NextCheck = time.Time{}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.oauthGateway.app.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	view, err := h.oauthGateway.app.Job(input.RequestID)
	if err != nil || view.Imported != 1 || !view.CanArchive {
		t.Fatalf("native job unfinished: %+v err=%v", view, err)
	}
	count, err := h.db.CountAll(ctx)
	if err != nil || count != 1 || submissions.Load() != 1 {
		t.Fatalf("write counts: accounts=%d submits=%d err=%v", count, submissions.Load(), err)
	}
	rows, err := h.db.ListActive(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal("missing imported account")
	}
	row := rows[0]
	if row.ProxyURL != runtimeProxy || row.GetCredential("workspace_id") != "workspace-fixture" || row.GetCredential("user_id") != "user-fixture" || row.GetCredential("access_token") != "synthetic-AT" {
		t.Fatal("account mapping or proxy isolation failed")
	}
	groupIDs, err := h.db.GetAccountGroupIDs(ctx, row.ID)
	if err != nil || len(groupIDs) != 1 || groupIDs[0] != group || h.store.FindByID(row.ID) == nil {
		t.Fatal("group/runtime publication missing")
	}
	if view.JobType != "import" || view.RequestedCountry != "auto" || len(view.AccountResults) != 1 {
		t.Fatalf("missing job summary: %+v", view.JobSummary)
	}
	final := view.AccountResults[0]
	if final.Line != 1 || final.AccountID != row.ID || final.PlanType != "plus" || final.WorkspaceID != "workspace-fixture" || final.GroupIDs == nil || len(*final.GroupIDs) != 1 || (*final.GroupIDs)[0] != group || final.RuntimeProxyID == nil || *final.RuntimeProxyID != proxyID || final.RecoveryState != "not_enrolled" || final.Outcome != "imported" {
		t.Fatalf("wrong committed account result: %+v", final)
	}
	stored, _ := state.View()
	rawState, _ := json.Marshal(stored)
	if bytes.Contains(rawState, []byte("synthetic-password")) {
		t.Fatal("completed job retained password")
	}
	rotated := bytes.ReplaceAll(account, []byte("synthetic-AT"), []byte("new-AT-must-not-overwrite"))
	result, err := (oauthGatewayTarget{h}).Import(ctx, rotated, nil, "")
	if err == nil || result.Failed != 1 {
		t.Fatal("duplicate account overwritten")
	}
	row, err = h.db.GetAccountByID(ctx, row.ID)
	if err != nil || row.GetCredential("access_token") != "synthetic-AT" {
		t.Fatal("duplicate changed stored credential")
	}
	w = oauthGatewayRequest(r, "GET", "jobs", "")
	for _, secret := range []string{"synthetic-CDK", "synthetic-AT", "synthetic-RT", "synthetic-password", "JBSWY3DPEHPK3PXP", runtimeProxy, "observed_country"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("job projection leaked credentials")
		}
	}
	w = oauthGatewayRequest(r, "POST", "jobs/native-test-1/archive", `{"discard_credentials":false}`)
	if w.Code != 200 {
		t.Fatalf("archive: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"account_results":[`) || strings.Contains(w.Body.String(), "fixture@example.invalid") || strings.Contains(w.Body.String(), runtimeProxy) {
		t.Fatal("archive lost committed summary or leaked account material")
	}
	w = oauthGatewayRequest(r, "POST", "jobs", string(payload))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"archived":true`) {
		t.Fatalf("archive dedup lost: %d", w.Code)
	}
	if submissions.Load() != 1 {
		t.Fatal("archive replay submitted OAuth")
	}
}
func TestOAuthGatewayDestinationAndGateway401(t *testing.T) {
	h, r, _ := newOAuthGatewayAdminFixture(t)
	if err := h.validateOAuthGatewayDestination(t.Context(), nil, "http://proxy.example.invalid:8080"); err == nil {
		t.Fatal("unregistered runtime proxy accepted")
	}
	if err := h.validateOAuthGatewayDestination(t.Context(), []int64{999999}, ""); err == nil {
		t.Fatal("missing group accepted")
	}
	if _, err := h.db.InsertProxy(t.Context(), "http://proxy.example.invalid:8080", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := h.validateOAuthGatewayDestination(t.Context(), nil, "http://proxy.example.invalid:8080"); err != nil {
		t.Fatalf("registered runtime proxy rejected: %v", err)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"invalid_activation_code","message":"must-not-echo-secret","source":"activation","action":"authenticate","retryable":false,"retry_after":null}}`))
	}))
	defer gateway.Close()
	if _, err := h.oauthGateway.app.SaveSettings(service.Settings{OAuthURL: gateway.URL, OAuthToken: "fixture"}); err != nil {
		t.Fatal(err)
	}
	w := oauthGatewayRequest(r, "GET", "capabilities", "")
	if w.Code != 502 || strings.Contains(w.Body.String(), "must-not-echo-secret") {
		t.Fatalf("gateway 401 leaked/mapped to admin logout: %d %s", w.Code, w.Body.String())
	}
}

func TestOAuthGatewayRejectsConflictingJWTIdentity(t *testing.T) {
	h, _, _ := newOAuthGatewayAdminFixture(t)
	accessToken := makeAdminTestJWT(t, map[string]interface{}{
		"https://api.openai.com/auth":    map[string]interface{}{"chatgpt_account_id": "workspace-original", "chatgpt_user_id": "user-fixture"},
		"https://api.openai.com/profile": map[string]interface{}{"email": "original@example.invalid"},
	})
	for _, tc := range []struct{ email, workspace string }{
		{"other@example.invalid", "workspace-other"},
		{"other@example.invalid", "workspace-original"},
		{"original@example.invalid", "workspace-other"},
	} {
		raw, err := json.Marshal(map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{
			"email": tc.email, "chatgpt_account_id": tc.workspace, "chatgpt_user_id": "user-fixture", "access_token": accessToken, "refresh_token": "synthetic-RT",
		}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := (oauthGatewayTarget{h}).Import(t.Context(), raw, nil, "")
		problem, ok := err.(*upstream.Error)
		if !ok || result.Failed != 1 || problem.Code != "gateway_identity_mismatch" {
			t.Fatalf("conflicting identity accepted: %+v %v", result, err)
		}
	}
	count, err := h.db.CountAll(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("conflict wrote accounts: %d %v", count, err)
	}
}
