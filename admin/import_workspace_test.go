package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type importWorkspaceTransport func(*http.Request) (*http.Response, error)

func (f importWorkspaceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func importWorkspaceToken(t *testing.T, suffix string, expires time.Time) string {
	t.Helper()
	return makeAdminTestJWT(t, map[string]interface{}{
		"exp": expires.Unix(), "jti": suffix,
		"https://api.openai.com/profile": map[string]interface{}{"email": "legacy@example.com"},
		"https://api.openai.com/auth":    map[string]interface{}{"user_id": "user-legacy"},
	})
}

func importWorkspaceFixture(t *testing.T) (*Handler, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	return &Handler{db: db, store: store, probeUsage: func(context.Context, *auth.Account) error { return nil }}, db
}

func importWorkspaceContext(t *testing.T, email, target string, calls *int) context.Context {
	t.Helper()
	return auth.WithCodexWorkspaceTransport(context.Background(), importWorkspaceTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.URL.Scheme != "https" || r.URL.Host != "chatgpt.com" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Error("unsafe metadata request")
		}
		body := `{"id":"user-legacy","email":"` + email + `"}`
		if strings.Contains(r.URL.Path, "accounts/check/") {
			body = `{"accounts":{"default":{"account":{"account_id":"` + target + `","is_deactivated":false}}}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
}

func runWorkspaceFileImport(t *testing.T, h *Handler, ctx context.Context, format string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("format", format)
	part, err := writer.CreateFormFile("file", "credentials.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(part).Encode(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", &body).WithContext(ctx)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	h.ImportAccounts(c)
	return recorder
}

func TestWorkspaceImportLifecycleDiscoversLegacyIdentityBeforeDedupeAndReimport(t *testing.T) {
	for _, format := range []string{"json", "json_at"} {
		t.Run(format, func(t *testing.T) {
			h, db := importWorkspaceFixture(t)
			calls := 0
			ctx := importWorkspaceContext(t, "legacy@example.com", "workspace-default", &calls)
			var firstID int64
			for i, suffix := range []string{"first", "rotated"} {
				token := importWorkspaceToken(t, suffix, time.Now().Add(time.Hour))
				payload := map[string]any{"accounts": []any{map[string]any{"name": "legacy", "credentials": map[string]any{
					"access_token": token, "refresh_token": "rt-" + suffix, "account_id": "user-legacy", "email": "LEGACY@example.com",
					"custom_headers": map[string]string{"X-Route-Label": "preserved"},
				}}}}
				recorder := runWorkspaceFileImport(t, h, ctx, format, payload)
				if recorder.Code != http.StatusOK {
					t.Fatalf("import status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				rows, err := db.ListActive(context.Background())
				if err != nil || len(rows) != 1 {
					t.Fatalf("rows=%d err=%v response=%s", len(rows), err, recorder.Body.String())
				}
				row := rows[0]
				if i == 0 {
					firstID = row.ID
				} else if row.ID != firstID {
					t.Fatal("reimport inserted another identity instead of updating")
				}
				if row.GetCredential("account_id") != "workspace-default" || row.GetCredential("workspace_id") != "workspace-default" || row.GetCredential("user_id") != "user-legacy" || row.GetCredential("access_token") != token || row.GetCredentialStringMap("custom_headers")["X-Route-Label"] != "preserved" {
					t.Fatalf("identity/routing missing after lifecycle import id=%d", row.ID)
				}
			}
			if calls != 4 {
				t.Fatalf("metadata calls=%d want 4 (one pair per changed token)", calls)
			}
		})
	}
}

func TestWorkspaceManualATImportUsesSharedDiscoveryInBothResponses(t *testing.T) {
	for _, stream := range []string{"false", "true"} {
		t.Run(stream, func(t *testing.T) {
			h, db := importWorkspaceFixture(t)
			token := importWorkspaceToken(t, "manual", time.Now().Add(time.Hour))
			calls := 0
			ctx := importWorkspaceContext(t, "legacy@example.com", "workspace-default", &calls)
			body, _ := json.Marshal(map[string]any{"access_token": token})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/at?stream="+stream, bytes.NewReader(body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			h.AddATAccount(c)
			rows, err := db.ListActive(context.Background())
			if recorder.Code != 200 || err != nil || len(rows) != 1 || rows[0].GetCredential("workspace_id") != "workspace-default" || rows[0].GetCredential("user_id") != "user-legacy" || calls != 2 {
				t.Fatalf("manual AT import: status=%d rows=%d calls=%d err=%v", recorder.Code, len(rows), calls, err)
			}
		})
	}
}

func TestWorkspaceImportRejectsExplicitTargetAndLoginMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, email, target string
		headers             map[string]string
	}{
		{"target", "legacy@example.com", "workspace-default", map[string]string{"ChatGPT-Account-ID": "workspace-not-member"}},
		{"email", "someone-else@example.com", "workspace-default", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, db := importWorkspaceFixture(t)
			calls := 0
			ctx := importWorkspaceContext(t, tc.email, tc.target, &calls)
			payload := map[string]any{"access_token": importWorkspaceToken(t, "mismatch", time.Now().Add(time.Hour)), "account_id": "user-legacy", "email": "legacy@example.com", "custom_headers": tc.headers}
			recorder := runWorkspaceFileImport(t, h, ctx, "json", payload)
			rows, err := db.ListActive(context.Background())
			if recorder.Code != 400 || err != nil || len(rows) != 0 {
				t.Fatalf("rejected import wrote account: status=%d rows=%d err=%v", recorder.Code, len(rows), err)
			}
		})
	}
}

func TestWorkspaceImportMetadataFailureRemainsNativeAndCircuitBounded(t *testing.T) {
	h, db := importWorkspaceFixture(t)
	calls := 0
	ctx := auth.WithCodexWorkspaceTransport(context.Background(), importWorkspaceTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("do not expose proxy password")
	}))
	payload := make([]map[string]any, 0, 100)
	for i := 0; i < 100; i++ {
		payload = append(payload, map[string]any{"access_token": importWorkspaceToken(t, string(rune('a'+i)), time.Now().Add(time.Hour)), "account_id": "user-legacy", "email": "legacy@example.com"})
	}
	recorder := runWorkspaceFileImport(t, h, ctx, "json_at", payload)
	rows, err := db.ListActive(context.Background())
	if recorder.Code != 200 || err != nil || len(rows) != 100 || calls != 1 {
		t.Fatalf("batch not bounded/native: status=%d rows=%d calls=%d err=%v", recorder.Code, len(rows), calls, err)
	}
	for _, row := range rows {
		if row.GetCredential("workspace_id") != "" || row.GetCredential("account_id") != "" || row.GetCredential("user_id") != "user-legacy" || row.GetCredential("codex_workspace_error") == "" || strings.Contains(row.GetCredential("codex_workspace_error"), "password") || row.Status == "error" {
			t.Fatalf("pending/native import damaged id=%d", row.ID)
		}
	}
}

func TestWorkspaceImportExpiredTokenDoesNotProbeOrFabricateWorkspace(t *testing.T) {
	h, db := importWorkspaceFixture(t)
	calls := 0
	ctx := importWorkspaceContext(t, "legacy@example.com", "workspace-default", &calls)
	payload := map[string]any{"access_token": importWorkspaceToken(t, "expired", time.Now().Add(-time.Hour)), "account_id": "user-legacy", "email": "legacy@example.com"}
	recorder := runWorkspaceFileImport(t, h, ctx, "json_at", payload)
	rows, err := db.ListActive(context.Background())
	if recorder.Code != 200 || err != nil || len(rows) != 1 || calls != 0 || rows[0].GetCredential("workspace_id") != "" || rows[0].GetCredential("user_id") != "user-legacy" || !strings.Contains(rows[0].GetCredential("codex_workspace_error"), "expired_access_token") {
		t.Fatalf("expired import: status=%d rows=%d calls=%d err=%v", recorder.Code, len(rows), calls, err)
	}
}

func TestWorkspaceImportDeniedProxyDoesNotCallMetadata(t *testing.T) {
	h, db := importWorkspaceFixture(t)
	ctx := context.Background()
	owner, err := db.InsertAccountWithCredentials(ctx, "owner", map[string]any{"access_token": "opaque-fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	proxyID, err := db.InsertProxy(ctx, "http://127.0.0.1:19199", "private")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpdateProxyAccess(ctx, proxyID, true, []int64{owner}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ctx = importWorkspaceContext(t, "legacy@example.com", "workspace-default", &calls)
	seed, err := h.prepareTokenCredentialSeed(ctx, tokenCredentialSeed{accessToken: importWorkspaceToken(t, "denied", time.Now().Add(time.Hour)), accountID: "user-legacy"}, "http://127.0.0.1:19199", 0)
	if err != nil || calls != 0 || !strings.Contains(seed.workspaceDiagnostic, "proxy_denied") || seed.workspaceID != "" {
		t.Fatalf("denied egress bypassed: err=%v calls=%d diagnostic=%q", err, calls, seed.workspaceDiagnostic)
	}
}

func TestWorkspaceOAuthAndSelfServiceUseSharedImportPreparation(t *testing.T) {
	for _, source := range []string{"oauth", "self-service"} {
		t.Run(source, func(t *testing.T) {
			h, db := importWorkspaceFixture(t)
			calls := 0
			ctx := importWorkspaceContext(t, "legacy@example.com", "workspace-default", &calls)
			seed := tokenCredentialSeed{refreshToken: "rt-oauth", sessionToken: "st-session", accessToken: importWorkspaceToken(t, source, time.Now().Add(time.Hour)), accountID: "user-legacy", email: "legacy@example.com"}
			var id int64
			var err error
			if source == "oauth" {
				id, _, err = h.upsertOAuthIdentityAccount(ctx, "oauth", "", seed, "import")
			} else {
				id, err = h.upsertSelfServiceAccount(ctx, "portal", "", seed, "contact@example.com")
			}
			if err != nil {
				t.Fatal(err)
			}
			row, err := db.GetAccountByID(context.Background(), id)
			if err != nil || calls != 2 || row.GetCredential("workspace_id") != "workspace-default" || row.GetCredential("user_id") != "user-legacy" || row.GetCredential("session_token") != "st-session" {
				t.Fatalf("shared import preparation missing source=%s calls=%d err=%v", source, calls, err)
			}
		})
	}
}

func TestWorkspaceImportInvalidSuppliedWorkspaceNeverFallsBack(t *testing.T) {
	h, _ := importWorkspaceFixture(t)
	calls := 0
	ctx := importWorkspaceContext(t, "legacy@example.com", "workspace-default", &calls)
	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{accessToken: importWorkspaceToken(t, "invalid", time.Now().Add(time.Hour)), workspaceID: "invalid/workspace"})
	_, err := h.prepareTokenCredentialSeed(ctx, seed, "", 0)
	if err == nil || calls != 0 {
		t.Fatalf("invalid supplied workspace fell back: calls=%d err=%v", calls, err)
	}
}
