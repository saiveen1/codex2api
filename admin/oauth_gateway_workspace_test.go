package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/codex2api/internal/oauthgateway/upstream"
)

func TestOAuthGatewayLegacyWorkspaceDiscoveryKeepsDuplicateGuard(t *testing.T) {
	h, db := importWorkspaceFixture(t)
	calls := 0
	ctx := importWorkspaceContext(t, "legacy@example.com", "workspace-default", &calls)
	token := importWorkspaceToken(t, "gateway-legacy", time.Now().Add(time.Hour))
	raw, err := json.Marshal(map[string]any{"platform": "openai", "type": "oauth", "name": "gateway legacy", "credentials": map[string]any{"email": "legacy@example.com", "access_token": token, "refresh_token": "synthetic-gateway-rt", "chatgpt_account_id": "user-legacy", "chatgpt_user_id": "user-legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (oauthGatewayTarget{h}).Import(ctx, raw, nil, "", false)
	if err != nil || result.Success != 1 || len(result.CreatedIDs) != 1 || calls != 2 {
		t.Fatalf("legacy gateway import: %+v %v metadata=%d", result, err, calls)
	}
	row, err := db.GetAccountByID(ctx, result.CreatedIDs[0])
	if err != nil || row.GetCredential("workspace_id") != "workspace-default" || row.GetCredential("account_id") != "workspace-default" || row.GetCredential("user_id") != "user-legacy" {
		t.Fatal("gateway did not preserve separate authenticated identity")
	}
	duplicate, err := (oauthGatewayTarget{h}).Import(ctx, raw, nil, "", false)
	if err == nil || duplicate.Success != 0 || duplicate.Failed != 1 {
		t.Fatal("gateway compatibility bypassed duplicate/no-recovery guard")
	}
	rows, err := db.ListActive(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal("duplicate gateway import mutated account pool")
	}
}

func TestOAuthGatewayLegacyIdentityMismatchStillRejects(t *testing.T) {
	h, db := importWorkspaceFixture(t)
	calls := 0
	ctx := importWorkspaceContext(t, "another@example.com", "workspace-default", &calls)
	token := importWorkspaceToken(t, "gateway-mismatch", time.Now().Add(time.Hour))
	raw, err := json.Marshal(map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{"email": "legacy@example.com", "access_token": token, "refresh_token": "synthetic-gateway-rt", "chatgpt_account_id": "user-legacy", "chatgpt_user_id": "user-legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (oauthGatewayTarget{h}).Import(ctx, raw, nil, "", false)
	if err == nil || result.Success != 0 {
		t.Fatal("gateway accepted mismatched authenticated identity")
	}
	rows, readErr := db.ListActive(ctx)
	if readErr != nil || len(rows) != 0 {
		t.Fatal("rejected identity changed account pool")
	}
}

func TestOAuthGatewayReturnedEnvironmentImportAndRecovery(t *testing.T) {
	h, _, _ := newOAuthGatewayAdminFixture(t)
	ctx := t.Context()
	target := oauthGatewayTarget{h}
	result := upstream.Result{
		Data:     json.RawMessage(`{"type":"sub2api-data","version":1,"accounts":[{"platform":"openai","type":"oauth","workspace_name":"Fixture","oauth_gateway_environment":"chrome_windows","credentials":{"email":"fixture@example.invalid","access_token":"fixture-at","refresh_token":"fixture-rt","chatgpt_user_id":"fixture-user","chatgpt_account_id":"workspace-original","oauth_gateway_environment":"chrome_windows"}}]}`),
		Delivery: upstream.Delivery{IncludedCount: 1, Items: []upstream.DeliveryItem{{ItemID: "fixture", Line: 1, AccountIndex: 0}}},
	}
	deliver := func(profile *upstream.BatchProfile) json.RawMessage {
		t.Helper()
		accounts, err := upstream.ValidateDelivery(result, upstream.Batch{Options: profile, Items: []upstream.BatchItem{{ItemID: "fixture", State: "succeeded", Line: 1}}})
		if err != nil || len(accounts) != 1 {
			t.Fatalf("delivery: %v", err)
		}
		return accounts[0]
	}
	created, err := target.Import(ctx, deliver(nil), nil, "", false)
	if err != nil || len(created.CreatedIDs) != 1 {
		t.Fatalf("legacy import: %v", err)
	}
	id := created.CreatedIDs[0]
	check := func(want string) {
		t.Helper()
		row, err := h.db.GetAccountByID(ctx, id)
		if err != nil || row.GetCredential(upstream.EnvironmentCredential) != want {
			t.Fatalf("persisted environment: want %q, err %v", want, err)
		}
		if acc := h.store.FindByID(id); acc == nil || acc.GetOAuthGatewayEnvironment() != want {
			t.Fatalf("runtime environment: want %q", want)
		}
	}
	check("") // Neither a top-level nor a credential-level remote field is evidence.
	h, _, _ = newOAuthGatewayAdminFixture(t)
	target = oauthGatewayTarget{h}
	created, err = target.Import(ctx, deliver(&upstream.BatchProfile{Environment: "chrome_macos"}), nil, "", false)
	if err != nil || len(created.CreatedIDs) != 1 {
		t.Fatalf("bound import: %v", err)
	}
	id = created.CreatedIDs[0]
	check("chrome_macos")
	for _, env := range []string{"", "future_environment", "chrome_windows"} {
		var profile *upstream.BatchProfile
		if env != "" {
			profile = &upstream.BatchProfile{Environment: env}
		}
		snap, err := target.RecoverySnapshot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if recovered, err := target.Recover(ctx, snap, deliver(profile), "fixture-recovery", false); err != nil || recovered.Updated != 1 {
			t.Fatalf("recover %q: %v", env, err)
		}
		if env == "chrome_windows" {
			check(env)
		} else {
			check("chrome_macos")
		}
	}
}
