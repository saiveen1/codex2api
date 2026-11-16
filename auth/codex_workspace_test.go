package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type workspaceMetadataTransport func(*http.Request) (*http.Response, error)

func (f workspaceMetadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func workspaceTestToken(t *testing.T, expiry time.Time, workspace string) string {
	t.Helper()
	claims := map[string]any{"exp": expiry.Unix(), "https://api.openai.com/profile": map[string]any{"email": "legacy@example.com"}, "https://api.openai.com/auth": map[string]any{"user_id": "user-legacy"}}
	if workspace != "" {
		claims["https://api.openai.com/auth"].(map[string]any)["chatgpt_account_id"] = workspace
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(body) + ".fixture"
}

func workspaceResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func workspaceMetadataContext(t *testing.T, token, me, accounts string, calls *int) context.Context {
	t.Helper()
	return WithCodexWorkspaceTransport(context.Background(), workspaceMetadataTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.URL.Scheme != "https" || r.URL.Host != "chatgpt.com" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-ID") != "" || r.Header.Get("X-Unsafe-Imported") != "" {
			t.Error("metadata request leaked routing/headers or changed authenticated origin")
		}
		if r.URL.Path == "/backend-api/me" {
			return workspaceResponse(me, 200), nil
		}
		if r.URL.Path != "/backend-api/accounts/check/v4-2023-04-27" || r.URL.RawQuery != "timezone_offset_min=-480" {
			t.Error("unexpected metadata endpoint")
		}
		return workspaceResponse(accounts, 200), nil
	}))
}

func TestCodexWorkspaceLegacyNormalizationAndDiscovery(t *testing.T) {
	token := workspaceTestToken(t, time.Now().Add(time.Hour), "")
	identity := NormalizeCodexWorkspaceIdentity(CodexWorkspaceIdentity{AccountID: " user-legacy ", Email: "LEGACY@example.com"}, "", token)
	if identity.UserID != "user-legacy" || identity.AccountID != "" || identity.WorkspaceID != "" {
		t.Fatalf("legacy normalization: %#v", identity)
	}
	calls := 0
	ctx := workspaceMetadataContext(t, token, `{"id":"user-legacy","email":"legacy@example.com"}`, `{"accounts":{"default":{"account":{"account_id":"workspace-default","is_deactivated":false}}}}`, &calls)
	got, err := HydrateCodexWorkspaceIdentity(ctx, identity, token, "")
	if err != nil || got.WorkspaceID != "workspace-default" || got.AccountID != "workspace-default" || got.UserID != "user-legacy" || calls != 2 {
		t.Fatalf("discovery: identity=%#v err=%v calls=%d", got, err, calls)
	}
}

func TestCodexWorkspaceExplicitTargetNeverFallsBack(t *testing.T) {
	token := workspaceTestToken(t, time.Now().Add(time.Hour), "")
	for _, tc := range []struct {
		name, me, accounts, code string
		rejected                 bool
	}{
		{"not-member", `{"id":"user-legacy","email":"legacy@example.com"}`, `{"accounts":{"default":{"account":{"account_id":"workspace-default"}}}}`, "target_not_member", true},
		{"deactivated", `{"id":"user-legacy","email":"legacy@example.com"}`, `{"accounts":{"target":{"account":{"account_id":"workspace-target","is_deactivated":true}},"default":{"account":{"account_id":"workspace-default"}}}}`, "target_not_member", true},
		{"email-mismatch", `{"id":"user-legacy","email":"another@example.com"}`, `{}`, "login_identity_mismatch", true},
		{"user-mismatch", `{"id":"user-someone-else","email":"legacy@example.com"}`, `{}`, "login_identity_mismatch", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"ChatGPT-Account-ID": "workspace-target", "X-Unsafe-Imported": "keep-local"}
			before := CodexWorkspaceIdentity{Email: "legacy@example.com", UserID: "user-legacy", Headers: headers}
			calls := 0
			ctx := workspaceMetadataContext(t, token, tc.me, tc.accounts, &calls)
			got, err := HydrateCodexWorkspaceIdentity(ctx, before, token, "")
			if err == nil || !strings.Contains(err.Error(), tc.code) || WorkspaceIdentityRejected(err) != tc.rejected || !reflect.DeepEqual(got, before) {
				t.Fatalf("got=%#v err=%v", got, err)
			}
			if headers["ChatGPT-Account-ID"] != "workspace-target" {
				t.Fatal("explicit route mutated")
			}
		})
	}
}

func TestCodexWorkspaceDefaultMustBeAuthenticatedDefault(t *testing.T) {
	token := workspaceTestToken(t, time.Now().Add(time.Hour), "")
	for _, accounts := range []string{
		`{"accounts":{"only-team":{"account":{"account_id":"workspace-team"}}}}`,
		`{"accounts":{"default":{"account":{"account_id":"user-legacy"}}}}`,
		`{"accounts":{"default":{"account":{"account_id":"workspace-default","is_deactivated":true}}}}`,
	} {
		calls := 0
		ctx := workspaceMetadataContext(t, token, `{"id":"user-legacy","email":"legacy@example.com"}`, accounts, &calls)
		got, err := HydrateCodexWorkspaceIdentity(ctx, CodexWorkspaceIdentity{}, token, "")
		if err == nil || got.WorkspaceID != "" || WorkspaceIdentityRejected(err) {
			t.Fatalf("invented default: %#v err=%v", got, err)
		}
	}
}

func TestCodexWorkspaceBoundedBudgetCircuitExpiryAndProxy(t *testing.T) {
	token := workspaceTestToken(t, time.Now().Add(time.Hour), "")
	calls := 0
	ctx := WithCodexWorkspaceHydration(WithCodexWorkspaceTransport(context.Background(), workspaceMetadataTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return workspaceResponse(`{}`, 403), nil
	})))
	for i := 0; i < 100; i++ {
		_, _ = HydrateCodexWorkspaceIdentity(ctx, CodexWorkspaceIdentity{}, token, "")
	}
	if calls != codexWorkspaceBatchAttempts {
		t.Fatalf("calls=%d, budget=%d", calls, codexWorkspaceBatchAttempts)
	}
	calls = 0
	ctx = WithCodexWorkspaceHydration(WithCodexWorkspaceTransport(context.Background(), workspaceMetadataTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("secret proxy credentials")
	})))
	for i := 0; i < 100; i++ {
		_, err := HydrateCodexWorkspaceIdentity(ctx, CodexWorkspaceIdentity{}, token, "")
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("circuit calls=%d", calls)
	}
	calls = 0
	ctx = WithCodexWorkspaceTransport(context.Background(), workspaceMetadataTransport(func(*http.Request) (*http.Response, error) { calls++; return workspaceResponse(`{}`, 200), nil }))
	_, err := HydrateCodexWorkspaceIdentity(ctx, CodexWorkspaceIdentity{}, workspaceTestToken(t, time.Now().Add(-time.Hour), ""), "")
	if err == nil || calls != 0 {
		t.Fatalf("expired token probed: %v calls=%d", err, calls)
	}
	_, err = HydrateCodexWorkspaceIdentity(ctx, CodexWorkspaceIdentity{}, token, "unsupported://proxy.invalid")
	if err == nil || calls != 0 {
		t.Fatalf("invalid proxy fell back: %v calls=%d", err, calls)
	}
	ctx = WithCodexWorkspaceHydration(ctx)
	ctx.Value(codexWorkspaceContextKey{}).(*codexWorkspaceBudget).deadline = time.Now().Add(-time.Second)
	_, err = HydrateCodexWorkspaceIdentity(ctx, CodexWorkspaceIdentity{}, token, "")
	if err == nil || calls != 0 {
		t.Fatalf("expired batch probed: %v calls=%d", err, calls)
	}
}

func TestCodexWorkspaceMetadataRejectsRedirectAndKeepsTLSVerification(t *testing.T) {
	calls := 0
	ctx := WithCodexWorkspaceTransport(context.Background(), workspaceMetadataTransport(func(*http.Request) (*http.Response, error) {
		calls++
		resp := workspaceResponse("", 302)
		resp.Header.Set("Location", "https://untrusted.invalid/steal")
		return resp, nil
	}))
	_, err := HydrateCodexWorkspaceIdentity(ctx, CodexWorkspaceIdentity{}, workspaceTestToken(t, time.Now().Add(time.Hour), ""), "")
	if err == nil || calls != 1 {
		t.Fatalf("redirect followed: %v calls=%d", err, calls)
	}
	client, err := BuildHTTPClientChecked("")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS verification disabled")
	}
}

func TestCodexWorkspaceRefreshDiscoversBeforePersistenceWithoutSecondExchange(t *testing.T) {
	token := workspaceTestToken(t, time.Now().Add(time.Hour), "")
	exchanges := 0
	store, db, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		exchanges++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "refresh_token": "rotated-rt", "expires_in": 3600})
	})
	if err := db.UpdateCredentials(context.Background(), id, map[string]any{"account_id": "user-legacy", "email": "legacy@example.com", "user_id": "user-legacy", "custom_headers": map[string]string{"ChatGPT-Account-ID": "workspace-target"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	defaultRouteID, err := db.InsertAccountWithCredentials(context.Background(), "default-route", map[string]any{"refresh_token": "old-rt", "access_token": "old-at", "account_id": "user-legacy", "email": "legacy@example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ctx := workspaceMetadataContext(t, token, `{"id":"user-legacy","email":"legacy@example.com"}`, `{"accounts":{"default":{"account":{"account_id":"workspace-default"}},"target":{"account":{"account_id":"workspace-target"}}}}`, &calls)
	if err := store.RefreshSingle(ctx, id); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if exchanges != 1 || calls != 2 || row.GetCredential("workspace_id") != "workspace-default" || row.GetCredential("account_id") != "workspace-default" || row.GetCredential("user_id") != "user-legacy" || row.GetCredential("refresh_token") != "rotated-rt" || row.GetCredentialStringMap("custom_headers")["ChatGPT-Account-ID"] != "workspace-target" {
		t.Fatalf("refresh identity not persisted, exchanges=%d metadata=%d", exchanges, calls)
	}
	defaultRow, err := db.GetAccountByID(context.Background(), defaultRouteID)
	if err != nil || defaultRow.GetCredential("workspace_id") != "workspace-default" || len(defaultRow.GetCredentialStringMap("custom_headers")) != 0 {
		t.Fatalf("explicit target leaked into sibling default route: %v", err)
	}
}

func TestCodexWorkspaceRefreshPreservesValidIdentityAndRouting(t *testing.T) {
	token := workspaceTestToken(t, time.Now().Add(time.Hour), "")
	acc := &Account{AccountID: "workspace-existing", Email: "legacy@example.com", CustomHeaders: map[string]string{"ChatGPT-Account-ID": "workspace-existing"}}
	td := &TokenData{AccessToken: token, RefreshToken: "rotated-rt", ExpiresAt: time.Now().Add(time.Hour)}
	updates := codexRefreshedCredentials(acc, td, nil, "")
	calls := 0
	ctx := WithCodexWorkspaceTransport(context.Background(), workspaceMetadataTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not probe known identity")
	}))
	(&Store{}).hydrateCodexRefreshedWorkspace(ctx, acc, td, updates, "")
	if calls != 0 || updates["account_id"] != "workspace-existing" || updates["workspace_id"] != "workspace-existing" || updates["refresh_token"] != "rotated-rt" {
		t.Fatalf("refresh damaged existing identity: %#v calls=%d", updates, calls)
	}
	if _, exists := updates["custom_headers"]; exists {
		t.Fatal("refresh overwrote per-route headers")
	}
}

func TestCodexWorkspaceRefreshMetadataFailureKeepsRotatingTokens(t *testing.T) {
	token := workspaceTestToken(t, time.Now().Add(time.Hour), "")
	acc := &Account{AccountID: "user-legacy", Email: "legacy@example.com", CustomHeaders: map[string]string{"ChatGPT-Account-ID": "workspace-target"}}
	td := &TokenData{AccessToken: token, RefreshToken: "rotated-once", ExpiresAt: time.Now().Add(time.Hour)}
	updates := codexRefreshedCredentials(acc, td, nil, "")
	calls := 0
	ctx := WithCodexWorkspaceTransport(context.Background(), workspaceMetadataTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("secret network error") }))
	(&Store{}).hydrateCodexRefreshedWorkspace(ctx, acc, td, updates, "")
	if calls != 1 || updates["refresh_token"] != "rotated-once" || updates["access_token"] != token || updates["workspace_id"] != "" || updates["user_id"] != "user-legacy" || updates["codex_workspace_error"] != "workspace identity: metadata_transport_failed" {
		t.Fatalf("metadata failure discarded credentials or fabricated workspace: calls=%d", calls)
	}
	if _, exists := updates["custom_headers"]; exists {
		t.Fatal("metadata failure changed explicit routing")
	}
}
